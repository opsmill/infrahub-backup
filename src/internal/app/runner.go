package app

import (
	"bytes"
	"context"
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

// LaunchComposeBackup runs ONE backup connector op in a one-shot runner container
// co-located with the target Docker Compose database service, and returns the
// created snapshot id.
//
// Validated model (2026-06-30, live Infrahub): the runner uses the DB service's
// OWN image (so neo4j-admin / pg client tools and the matching version are
// present), joins the DB's compose network (reach it by service name), and mounts
// the tool binary + the kloset repo. This needs no separately-built runner image
// and resolves fs:// repo reachability (the host repo dir is bind-mounted in).
func LaunchComposeBackup(project, dbService, repoPath, uri, passphrase string, opts map[string]string, tags []string, mountDBVolumes bool) (string, error) {
	args, err := composeRunnerArgs(project, dbService, repoPath, mountDBVolumes, passphrase != "")
	if err != nil {
		return "", err
	}
	repoArg := repoArgFor(repoPath)
	args = append(args, "__run-connector", "backup", repoArg, uri)
	if passphrase != "" {
		args = append(args, "--passphrase-stdin")
	}
	for k, v := range opts {
		args = append(args, "--opt", k+"="+v)
	}
	for _, t := range tags {
		args = append(args, "--tag", t)
	}
	return runDockerCapture(args, passphrase)
}

// LaunchComposeRestore runs ONE restore connector op in a co-located runner. A
// requested Neo4j format migration runs inside the same runner, in the same offline
// window as the load (see Neo4jMigration).
func LaunchComposeRestore(project, dbService, repoPath, destURI, snapshot, passphrase string, opts map[string]string, mountDBVolumes bool, migrate Neo4jMigration) error {
	args, err := composeRunnerArgs(project, dbService, repoPath, mountDBVolumes, passphrase != "")
	if err != nil {
		return err
	}
	repoArg := repoArgFor(repoPath)
	args = append(args, "__run-connector", "restore", repoArg, destURI, snapshot)
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
	if passphrase != "" {
		args = append(args, "--passphrase-stdin")
	}
	for k, v := range opts {
		args = append(args, "--opt", k+"="+v)
	}
	_, err = runDockerCapture(args, passphrase)
	return err
}

// composeRunnerArgs builds the `docker run …` prefix up to (but not including)
// the in-container command: image, network, mounts, env. When withStdin is set,
// the container keeps stdin open (`-i`) so the passphrase can be piped in.
func composeRunnerArgs(project, dbService, repoPath string, mountDBVolumes, withStdin bool) ([]string, error) {
	cid, err := composeContainerID(project, dbService)
	if err != nil {
		return nil, err
	}
	image, err := dockerInspect(cid, "{{.Config.Image}}")
	if err != nil {
		return nil, fmt.Errorf("inspecting image of %s: %w", dbService, err)
	}
	network, err := firstNetwork(cid)
	if err != nil {
		return nil, err
	}
	bin, err := runnerBinary()
	if err != nil {
		return nil, fmt.Errorf("resolving runner binary: %w", err)
	}

	args := []string{"run", "--rm"}
	if withStdin {
		// Keep stdin open so the orchestrator can pipe the passphrase in. The
		// passphrase is never an arg or -e env var (it would leak via docker
		// inspect / the process list).
		args = append(args, "-i")
	}
	args = append(args,
		"--network", network,
		"--user", "root", // neo4j-admin/pg tools; online backup tolerates root
		"-e", "HOME=/tmp",
		"-w", "/tmp", // kloset writes a relative "<ver>/store" cache under CWD — keep it writable
		"-v", bin+":"+runnerBinaryPath+":ro",
	)
	if local, hostPath := parseRepoLocation(repoPath); local {
		// Local repo, spelled either /path or fs:///path — bind-mount the host
		// directory into the runner, where repoArgFor points the worker at /repo.
		args = append(args, "-v", hostPath+":/repo")
	} else if strings.HasPrefix(repoPath, "s3://") {
		// The runner is on the database's compose network, so it has no route to a
		// service published on the host's loopback. An operator running MinIO or
		// another S3-compatible store on the Docker host is a normal deployment, so
		// map the host in and rewrite loopback endpoints to it rather than failing
		// with a bare "connection refused" from inside the container.
		args = append(args, "--add-host", dockerHostAlias+":host-gateway")
		for _, e := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "INFRAHUB_S3_ENDPOINT"} {
			if v := os.Getenv(e); v != "" {
				args = append(args, "-e", e+"="+containerReachable(v))
			}
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
	return args, nil
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

// repoArgFor maps the configured repo to the location the in-container worker opens:
// a local repo is bind-mounted at /repo; a remote URI is passed through, with a
// loopback host rewritten so it resolves to the Docker host rather than to the runner.
func repoArgFor(repoPath string) string {
	if local, _ := parseRepoLocation(repoPath); local {
		return "/repo"
	}
	return containerReachable(repoPath)
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
// used to pipe the repository passphrase without exposing it on argv/env.
func runDockerCapture(args []string, stdin string) (string, error) {
	return runCapture(runnerTimeout(), "docker", args, stdin)
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
)

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
			return "", fmt.Errorf("runner did not finish within %v and was killed (raise %s if this deployment needs longer): %s",
				timeout, runnerTimeoutEnvVar, runnerOutput(&out, &errb))
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
