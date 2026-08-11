package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// runnerBinary returns the path to the tool binary to mount into the runner
// container. In production the tool runs on Linux, so its own executable is a
// valid Linux binary; INFRAHUB_RUNNER_BINARY overrides it (e.g. for a cross-built
// binary during development on a non-Linux host).
func runnerBinary() (string, error) {
	if b := os.Getenv("INFRAHUB_RUNNER_BINARY"); b != "" {
		return b, nil
	}
	return os.Executable()
}

// runnerCredentials are the values the in-container worker needs but which must
// not appear on its command line or in its environment.
//
// `docker inspect` reports both Config.Cmd and Config.Env for as long as the
// container exists, and the host process list shows the docker CLI's own argv, so
// either channel publishes a secret to every local user and to anything scraping
// container metadata. The repository passphrase already avoided both by travelling
// over the runner's stdin; the database and object-store credentials did not, and
// went out in the connector URIs (`postgres://user:pass@…`, `s3://key:secret@…`)
// and as `-e AWS_SECRET_ACCESS_KEY=…`.
//
// They are sent as one JSON object rather than key=value lines so that a value
// containing a newline or an `=` cannot be misread.
type runnerCredentials struct {
	// Passphrase opens an encrypted kloset repository.
	Passphrase string `json:"passphrase,omitempty"`
	// DBPassword authenticates the connector to the database it is dumping or
	// loading. The worker applies it as the connector's standalone `password`
	// option, which both pinned connectors document as overriding the location URI.
	DBPassword string `json:"db_password,omitempty"`
	// S3AccessKey / S3SecretKey authenticate an s3:// repository. They are filled in
	// by the launch functions from repoAccessFor, not by callers.
	S3AccessKey string `json:"s3_access_key,omitempty"`
	S3SecretKey string `json:"s3_secret_key,omitempty"`
}

// runnerCredentials assembles what a runner launch has to send over stdin: the
// repository passphrase, and the password for the database this launch talks to
// (empty for the offline Neo4j paths, which authenticate through the filesystem).
func (iops *InfrahubOps) runnerCredentials(dbPassword string) runnerCredentials {
	return runnerCredentials{
		Passphrase: iops.config.Plakar.Passphrase,
		DBPassword: dbPassword,
	}
}

// encode renders the credentials for the runner's stdin.
func (c runnerCredentials) encode() (string, error) {
	data, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("encoding runner credentials: %w", err)
	}
	return string(data), nil
}

// LaunchComposeBackup runs ONE backup connector op in a one-shot runner container
// co-located with the target Docker Compose database service, and returns the
// created snapshot id.
//
// Validated model (2026-06-30, live Infrahub): the runner uses the DB service's
// OWN image (so neo4j-admin / pg client tools and the matching version are
// present), joins the DB's compose network (reach it by service name), and mounts
// the tool binary + the kloset repo. This needs no separately-built runner image
// and resolves fs:// repo reachability (the host repo dir is bind-mounted in).
func LaunchComposeBackup(project, dbService, repoPath, uri string, creds runnerCredentials, opts map[string]string, tags []string, mountDBVolumes bool) (string, error) {
	args, container, err := composeRunnerArgs(project, dbService, repoPath, mountDBVolumes)
	if err != nil {
		return "", err
	}
	access := repoAccessFor(repoPath)
	creds.S3AccessKey, creds.S3SecretKey = access.AccessKey, access.SecretKey
	stdin, err := creds.encode()
	if err != nil {
		return "", err
	}

	args = append(args, "__run-connector", "backup", access.Location, uri, credentialsStdinFlag)
	args = append(args, access.flags()...)
	for k, v := range opts {
		args = append(args, "--opt", k+"="+v)
	}
	for _, t := range tags {
		args = append(args, "--tag", t)
	}
	return runDockerCapture(args, container, stdin)
}

// LaunchComposeRestore runs ONE restore connector op in a co-located runner. A
// requested Neo4j format migration runs inside the same runner, in the same offline
// window as the load (see Neo4jMigration).
func LaunchComposeRestore(project, dbService, repoPath, destURI, snapshot string, creds runnerCredentials, opts map[string]string, mountDBVolumes bool, migrate Neo4jMigration) error {
	args, container, err := composeRunnerArgs(project, dbService, repoPath, mountDBVolumes)
	if err != nil {
		return err
	}
	access := repoAccessFor(repoPath)
	creds.S3AccessKey, creds.S3SecretKey = access.AccessKey, access.SecretKey
	stdin, err := creds.encode()
	if err != nil {
		return err
	}

	args = append(args, "__run-connector", "restore", access.Location, destURI, snapshot, credentialsStdinFlag)
	args = append(args, access.flags()...)
	if migrate.Requested() {
		args = append(args, "--migrate-format", migrate.Format, "--migrate-database", migrate.Database)
	}
	if mountDBVolumes {
		// The runner writes into the database's own data volume, and now does so as
		// real root (see the entrypoint note in composeRunnerArgs). A data directory
		// the database user cannot read is one the server will not start on, so the
		// worker restores the directory's original ownership when it is done. 003
		// recorded this as the tool's job rather than the connector's.
		args = append(args, "--preserve-owner", dbDataDir)
	}
	for k, v := range opts {
		args = append(args, "--opt", k+"="+v)
	}
	_, err = runDockerCapture(args, container, stdin)
	return err
}

// composeRunnerArgs builds the `docker run …` prefix up to (but not including)
// the in-container command: image, network, mounts, env. The container always
// keeps stdin open (`-i`), because that is how the credentials reach the worker.
func composeRunnerArgs(project, dbService, repoPath string, mountDBVolumes bool) ([]string, string, error) {
	cid, err := composeContainerID(project, dbService)
	if err != nil {
		return nil, "", err
	}
	image, err := dockerInspect(cid, "{{.Config.Image}}")
	if err != nil {
		return nil, "", fmt.Errorf("inspecting image of %s: %w", dbService, err)
	}
	network, err := firstNetwork(cid)
	if err != nil {
		return nil, "", err
	}
	bin, err := runnerBinary()
	if err != nil {
		return nil, "", fmt.Errorf("resolving runner binary: %w", err)
	}

	// -i keeps stdin open so the orchestrator can pipe the credentials in. No
	// secret is ever an argument or a -e env var: docker inspect reports both for
	// the life of the container, and argv is visible in the host process list.
	//
	// --name gives the launch a handle: killing the `docker run` client on timeout
	// does NOT stop the container it started, and a runner still writing into the
	// database's shared data volume while the deferred StartServices("database")
	// boots Neo4j on it is worse than the wedge the timeout was added for.
	container := runnerContainerName(dbService)
	args := []string{"run", "--rm", "-i", "--name", container}
	args = append(args,
		"--network", network,
		"--user", "root", // neo4j-admin/pg tools; online backup tolerates root
		"-e", "HOME=/tmp",
		"-w", "/tmp", // kloset writes a relative "<ver>/store" cache under CWD — keep it writable
		"-v", bin+":"+runnerBinaryPath+":ro",
	)
	if local, hostPath := parseRepoLocation(repoPath); local {
		// Local repo, spelled either /path or fs:///path — bind-mount the host
		// directory into the runner, where repoAccessFor points the worker at /repo.
		args = append(args, "-v", hostPath+":/repo")
	} else if strings.HasPrefix(repoPath, "s3://") {
		// The runner is on the database's compose network, so it has no route to a
		// service published on the host's loopback. An operator running MinIO or
		// another S3-compatible store on the Docker host is a normal deployment, so
		// map the host in and rewrite loopback endpoints to it rather than failing
		// with a bare "connection refused" from inside the container.
		args = append(args, "--add-host", dockerHostAlias+":host-gateway")
		// Only the endpoint is forwarded through the environment. It is a URL, which
		// is what containerReachable is for; the access key and secret travel over
		// stdin instead (see runnerCredentials). AWS_SECRET_ACCESS_KEY used to be
		// passed through containerReachable too, which is a URL rewriter being handed
		// something that is not a URL.
		if v := os.Getenv("INFRAHUB_S3_ENDPOINT"); v != "" {
			args = append(args, "-e", "INFRAHUB_S3_ENDPOINT="+containerReachable(v))
		}
	}
	if mountDBVolumes {
		// Neo4j community offline dump / restore: share the DB's data volume.
		args = append(args, "--volumes-from", cid)
	}
	// Bypass the image's own entrypoint. The runner borrows the database image for its
	// tools (neo4j-admin, pg client) and runs our binary in it; it is not starting a
	// database, so a server entrypoint has no work to do here — and running it silently
	// undoes --user root above. neo4j's docker-entrypoint.sh drops to the neo4j user
	// (uid 7474) even when started as root, which left the worker unable to read a
	// repository directory owned by whoever ran the tool:
	//
	//	failed to open plakar repository /repo: open /repo/CONFIG: permission denied
	//
	// postgres' entrypoint honours root, so neo4j was the only image deviating from
	// what this function already asks for. Bypassing the entrypoint keeps the tools
	// usable — NEO4J_HOME and java come from the image's ENV, not its entrypoint.
	//
	// Restores then write as root, so LaunchComposeRestore asks the worker to put the
	// data directory's ownership back; see preserveOwnership in run_connector.go.
	args = append(args, "--entrypoint", runnerBinaryPath, image)
	return args, container, nil
}

// runnerContainerName names one runner launch. It is unique per launch so that a
// container left behind by an earlier run cannot make the next one fail on a name
// clash.
func runnerContainerName(dbService string) string {
	return fmt.Sprintf("infrahub-backup-runner-%s-%d-%d", dbService, os.Getpid(), time.Now().UnixNano())
}

const (
	// runnerBinaryPath is where the tool binary is mounted inside the runner, and the
	// entrypoint the runner is started with.
	runnerBinaryPath = "/usr/local/bin/infrahub-backup"
	// dockerHostAlias resolves to the Docker host from inside the runner, via
	// --add-host …:host-gateway.
	dockerHostAlias = "host.docker.internal"
	// dbDataDir is the database data directory shared into the runner by
	// --volumes-from, and the directory whose ownership a restore must leave intact.
	// Both Neo4j restore paths write here; the Postgres one restores over the wire and
	// does not share volumes at all.
	dbDataDir = "/data"
)

// containerReachable rewrites a loopback host in an endpoint or URI so the runner can
// reach it, because "localhost" inside the container is the container itself.
//
// It only ever substitutes the host: credentials, port, path and query are untouched,
// and anything that is not loopback is returned unchanged.
func containerReachable(s string) string {
	// An endpoint may be given without a scheme ("localhost:9000"), which url.Parse
	// would read as scheme "localhost". Parse those behind a placeholder scheme and
	// hand back the same shape they came in.
	bare := !strings.Contains(s, "://")
	parsed := s
	if bare {
		parsed = "placeholder://" + s
	}

	u, err := url.Parse(parsed)
	if err != nil || u.Host == "" {
		return s
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "::1":
	default:
		return s
	}

	// Read the port before reassigning Host, or it is gone by the time it is asked for.
	if port := u.Port(); port != "" {
		u.Host = dockerHostAlias + ":" + port
	} else {
		u.Host = dockerHostAlias
	}
	if bare {
		return strings.TrimPrefix(u.String(), "placeholder://")
	}
	return u.String()
}

// parseRepoLocation classifies a --repo value and, for a local repository, returns
// its filesystem path with any fs:// scheme stripped.
//
// A local repository has two spellings — a bare path (/backups/infra) and the fs://
// URI the documentation and README use (fs:///backups/infra) — and every decision
// that turns on "is this local" has to treat them as the same thing. Testing only
// for "://" does not: fs:// contains it, so the documented spelling was classified
// as remote. The runner then neither bind-mounted the directory nor rewrote the
// path, and the in-container worker looked for the repository at a host path that
// does not exist inside the container, failing with a missing CONFIG. Only the bare
// spelling worked, which is why the local test recipes never caught it.
func parseRepoLocation(repoPath string) (local bool, path string) {
	if rest, ok := strings.CutPrefix(repoPath, "fs://"); ok {
		return true, rest
	}
	if strings.Contains(repoPath, "://") {
		return false, repoPath
	}
	return true, repoPath
}

// liftS3Credentials resolves the credentials for an s3:// location — any embedded
// in the URI win, then the values passed in, then the host's AWS_* environment —
// and returns the location with the userinfo removed, so that a caller may put it
// somewhere a credential must not appear.
//
// embedded reports that the URI carried userinfo, which both callers read as "a
// local S3-compatible store, reached over plain HTTP": lifting the credentials out
// of the URI must not silently flip such a repository to TLS.
//
// The two callers sit on either side of the runner boundary — storeConfig hands the
// keys to the storage backend as separate config entries, repoAccessFor sends them
// over the runner's stdin — and had grown the same resolution twice.
func liftS3Credentials(location, accessKey, secretKey string) (cleaned, resolvedAccessKey, resolvedSecretKey string, embedded bool) {
	cleaned = location
	if u, err := url.Parse(location); err == nil && u.User != nil {
		accessKey = u.User.Username()
		if secret, ok := u.User.Password(); ok {
			secretKey = secret
		}
		u.User = nil
		cleaned, embedded = u.String(), true
	}
	if accessKey == "" {
		accessKey = os.Getenv("AWS_ACCESS_KEY_ID")
	}
	if secretKey == "" {
		secretKey = os.Getenv("AWS_SECRET_ACCESS_KEY")
	}
	return cleaned, accessKey, secretKey, embedded
}

// runnerRepoAccess is how the runner is told to reach the repository, split into
// the part that is safe to put on a command line and the part that is not.
type runnerRepoAccess struct {
	// Location is the repository as the in-container worker should open it: the
	// bind mount for a local repo, or the URI with any credentials removed.
	Location string
	// AccessKey / SecretKey are the s3:// credentials, to be sent over stdin.
	AccessKey string
	SecretKey string
	// Insecure records that the repository is to be reached over plain HTTP. It is
	// implied by credentials embedded in the URI, which storeConfig has always read
	// as "a local S3-compatible store"; stripping the credentials would otherwise
	// silently flip such a repository to TLS.
	Insecure bool
}

// flags renders the non-secret part of the access for the worker's command line.
func (a runnerRepoAccess) flags() []string {
	if a.Insecure {
		return []string{"--s3-insecure"}
	}
	return nil
}

// repoAccessFor maps the configured repo to what the in-container worker is told:
// a local repo is bind-mounted at /repo; a remote URI is passed through with a
// loopback host rewritten so it resolves to the Docker host rather than to the
// runner, and with any embedded credentials lifted out of the URI so they do not
// land on `docker run`'s argv. When the URI carries none, the host's AWS_*
// environment is used — read here, on the host, rather than forwarded into the
// container's environment.
func repoAccessFor(repoPath string) runnerRepoAccess {
	if local, _ := parseRepoLocation(repoPath); local {
		return runnerRepoAccess{Location: "/repo"}
	}

	access := runnerRepoAccess{Location: containerReachable(repoPath)}
	if !strings.HasPrefix(repoPath, "s3://") {
		return access
	}
	access.Location, access.AccessKey, access.SecretKey, access.Insecure = liftS3Credentials(access.Location, "", "")
	return access
}

func composeContainerID(project, service string) (string, error) {
	// -a so a stopped container is still found (restore stops the writer first).
	out, err := exec.Command("docker", "ps", "-aq",
		"--filter", "label=com.docker.compose.project="+project,
		"--filter", "label=com.docker.compose.service="+service).Output()
	if err != nil {
		return "", fmt.Errorf("locating compose service %q: %w", service, err)
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) == 0 {
		return "", fmt.Errorf("no running container for compose service %q in project %q", service, project)
	}
	return fields[0], nil
}

func dockerInspect(cid, format string) (string, error) {
	out, err := exec.Command("docker", "inspect", "--format", format, cid).Output()
	if err != nil {
		return "", fmt.Errorf("docker inspect %s: %w", cid, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func firstNetwork(cid string) (string, error) {
	s, err := dockerInspect(cid, "{{range $k,$_ := .NetworkSettings.Networks}}{{$k}} {{end}}")
	if err != nil {
		return "", err
	}
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return "", fmt.Errorf("container %s has no network", cid)
	}
	return fields[0], nil
}

// runDockerCapture runs `docker <args>` and returns the last stdout token. When
// stdin is non-empty it is written to the container (one line) and stdin closed,
// used to pipe the credentials without exposing them on argv/env.
//
// container names the launched container so that a timeout can also remove it:
// the timeout kills the `docker run` client, which leaves the container running.
func runDockerCapture(args []string, container, stdin string) (string, error) {
	out, err := runCapture(runnerTimeout(), "docker", args, stdin)
	if errors.Is(err, errRunnerTimeout) && container != "" {
		removeRunnerContainer(container)
	}
	return out, err
}

// removeRunnerContainer force-removes a timed-out runner, best effort. It has its
// own short timeout: this runs on the path where docker has already proved slow,
// and the caller still has a database to restart.
func removeRunnerContainer(container string) {
	if _, err := runCapture(runnerRemoveTimeout, "docker", []string{"rm", "-f", container}, ""); err != nil {
		logrus.Warnf("Could not remove the timed-out runner container %s (it may still be writing to the database volume): %v", container, err)
		return
	}
	logrus.Warnf("Removed the timed-out runner container %s", container)
}

const (
	// defaultRunnerTimeout bounds one runner launch. The orchestrator stops the
	// `database` service before launching the runner and restarts it in a defer, so
	// a call that never returns leaves Infrahub down until a human intervenes — a
	// wedged neo4j-admin on a corrupt store, or a stalled docker daemon, is exactly
	// that. The streaming path this replaced carried the same 30-minute guard
	// (defaultStreamIdleTimeout).
	defaultRunnerTimeout = 30 * time.Minute
	// runnerTimeoutEnvVar raises (or lowers) defaultRunnerTimeout for deployments
	// whose databases legitimately take longer than 30 minutes to dump or load.
	runnerTimeoutEnvVar = "INFRAHUB_RUNNER_TIMEOUT"
	// runnerRemoveTimeout bounds the clean-up removal of a timed-out runner.
	runnerRemoveTimeout = 30 * time.Second
)

// errRunnerTimeout marks a launch that was killed for exceeding its timeout, as
// opposed to one that failed on its own.
var errRunnerTimeout = errors.New("runner timed out")

// runnerTimeout returns the per-launch timeout, honouring INFRAHUB_RUNNER_TIMEOUT
// (any time.ParseDuration value). An unparseable or non-positive value falls back
// to the default with a warning rather than disabling the guard, because "no
// timeout" is the failure mode the guard exists for.
func runnerTimeout() time.Duration {
	raw := os.Getenv(runnerTimeoutEnvVar)
	if raw == "" {
		return defaultRunnerTimeout
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		logrus.Warnf("Ignoring %s=%q (want a positive duration such as 45m); using %v", runnerTimeoutEnvVar, raw, defaultRunnerTimeout)
		return defaultRunnerTimeout
	}
	return d
}

// runCapture runs one command under a timeout and returns the last stdout token.
// Both streams are buffered: stdout because its last token is the snapshot id, and
// stderr because it is the only diagnostic a failed runner leaves. On failure both
// are surfaced — dropping the buffered stdout hid the connector's own error
// message, which is often the one that says what went wrong.
func runCapture(timeout time.Duration, name string, args []string, stdin string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var out, errb bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin + "\n")
	}
	cmd.Stdout = &out
	cmd.Stderr = &errb

	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("%w: did not finish within %v and was killed (raise %s if this deployment needs longer): %s",
				errRunnerTimeout, timeout, runnerTimeoutEnvVar, runnerOutput(&out, &errb))
		}
		return "", fmt.Errorf("runner launch failed: %w: %s", err, runnerOutput(&out, &errb))
	}

	fields := strings.Fields(strings.TrimSpace(out.String()))
	if len(fields) == 0 {
		return "", nil
	}
	return fields[len(fields)-1], nil // snapshot id is the last stdout token
}

// runnerOutput joins whatever the runner said, for a failure message.
func runnerOutput(out, errb *bytes.Buffer) string {
	parts := make([]string, 0, 2)
	if s := strings.TrimSpace(errb.String()); s != "" {
		parts = append(parts, s)
	}
	if s := strings.TrimSpace(out.String()); s != "" {
		parts = append(parts, s)
	}
	if len(parts) == 0 {
		return "(no output)"
	}
	return strings.Join(parts, "\n")
}
