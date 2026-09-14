package app

import (
	"embed"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// maxPrefectPaginationSize is the largest pagination size the task-manager accepts.
const maxPrefectPaginationSize = 200

// scriptsFS holds the embedded maintenance scripts.
//
//go:embed scripts/*
var scriptsFS embed.FS

// Embeddable scripts require exposing a helper to other packages.
func ReadScript(name string) ([]byte, error) {
	return scriptsFS.ReadFile("scripts/" + name)
}

// BackendType selects the backup storage engine.
type BackendType string

const (
	BackendTarball BackendType = "tarball"
	BackendPlakar  BackendType = "plakar"
)

// PlakarConfig holds Plakar-specific configuration.
type PlakarConfig struct {
	RepoPath   string // Repository location (local path or URI like s3://bucket/prefix)
	CacheDir   string // Local cache directory for dedup state
	SnapshotID string // Specific snapshot ID for restore (empty = latest)
	BackupID   string // Specific backup-id tag for restore (empty = latest complete group)
}

// Defaults for reaching a database that lives outside the deployment.
const (
	// defaultNeo4jBackupPort is the port the Neo4j backup service listens on. It
	// is the one endpoint detail discovery cannot supply: Infrahub never connects
	// to the backup listener, so no setting in the deployment records it
	// (research R4). Overridable with --neo4j-backup-port.
	defaultNeo4jBackupPort = 6362

	// defaultExternalDBTimeout bounds a single operation against an external
	// database (FR-025). Generous enough for a capture of a large store, finite
	// so that an endpoint which accepts a connection and then stops responding
	// cannot hang a restore while Infrahub is scaled down.
	defaultExternalDBTimeout = 2 * time.Hour

	// defaultExternalDBImageNeo4j and defaultExternalDBImagePostgres are the
	// pinned images supplying the database tooling for a transient workload.
	// They track the images Infrahub itself deploys, with the Neo4j one on the
	// enterprise tag because the backup mechanism is an Enterprise feature.
	// Pinned rather than floating, and settable by flag only (see
	// ConfigureRootCommand), because the tool holds workload-creation rights
	// (FR-027).
	defaultExternalDBImageNeo4j    = "neo4j:2025.10.1-enterprise"
	defaultExternalDBImagePostgres = "postgres:18-alpine"
)

// ExternalRestoreAuthSource names the channel that supplied an external-restore
// authorisation. An unattended restore may only use the configuration channel
// (FR-010), so which channel supplied it is part of the answer, not just whether
// one did.
type ExternalRestoreAuthSource string

const (
	ExternalRestoreAuthAbsent ExternalRestoreAuthSource = ""
	ExternalRestoreAuthFlag   ExternalRestoreAuthSource = "flag"
	ExternalRestoreAuthConfig ExternalRestoreAuthSource = "config"
)

// ExternalRestoreAuth is the resolved authorisation to restore into a database
// the deployment does not manage (FR-009). Its zero value is "not authorised",
// so a restore path that never consults it cannot proceed by default.
//
// The channel is the whole of it. It used to carry an `Allowed bool` beside the
// source, and the two answered one question between them: reachable read only
// the bool, describe read only the source. `{Allowed: true}` was therefore a
// representable value that authorised a destructive write into infrastructure
// the deployment does not manage and then logged "no channel this run can
// name" — losing precisely the FR-010 fact the record exists for, which is
// whether an operator was present. Every constructor in cli.go set both
// consistently, so it never happened; it was true by convention, and one
// derivable field is how it stops being a convention.
type ExternalRestoreAuth struct {
	Source ExternalRestoreAuthSource
}

// allowed reports whether a restore into an unmanaged database is authorised,
// which is exactly "some channel said so". It is unexported because
// databaseTarget.reachable is the only reader — FR-009's refusal lives there
// and nowhere else (see databaseTarget.reachable).
func (a ExternalRestoreAuth) allowed() bool {
	return a.Source != ExternalRestoreAuthAbsent
}

// describe names the channel that authorised the restore, for the line that
// records a destructive write to infrastructure the deployment does not manage.
//
// The channel is the part worth naming. An unattended restore can only be
// authorised through configuration, because nothing on this path prompts
// (FR-010), so "--allow-external-restore" and "INFRAHUB_ALLOW_EXTERNAL_RESTORE"
// answer different questions after the fact: whether an operator was present.
func (a ExternalRestoreAuth) describe() string {
	switch a.Source {
	case ExternalRestoreAuthFlag:
		return "--" + AllowExternalRestoreFlag
	case ExternalRestoreAuthConfig:
		return AllowExternalRestoreEnvVar
	default:
		return "no channel this run can name"
	}
}

// ExternalDBTLS holds the TLS settings of a client connection to a database, as
// the deployment's own INFRAHUB_DB_TLS_* settings describe it.
type ExternalDBTLS struct {
	Enabled bool   // INFRAHUB_DB_TLS_ENABLED
	CAFile  string // INFRAHUB_DB_TLS_CA_FILE
	// Insecure records INFRAHUB_DB_TLS_INSECURE as the deployment set it. It
	// describes what Infrahub does; it is not on its own an instruction to skip
	// certificate verification for the connections this tool opens (FR-029).
	Insecure bool
}

// ExternalDBConfig holds the settings for reaching a database outside the
// deployment. Its zero value engages nothing, and a deployment whose databases
// are all internal never consults it (FR-015). Each field is either an operator
// override of a value discovery supplies, a setting discovery reads from the
// deployment's running components, or -- for the backup port and the tooling
// images -- a value discovery cannot supply at all.
type ExternalDBConfig struct {
	// Neo4jAddress overrides the discovered Neo4j host list. It takes the same
	// host[:port][,host[:port]...] shape as Infrahub's own INFRAHUB_DB_ADDRESS,
	// and a member without an explicit port inherits the client port.
	Neo4jAddress string
	// Neo4jBackupPort is the port the backup service listens on, not the Bolt
	// port Infrahub itself uses.
	Neo4jBackupPort int
	// Neo4jProtocol is bolt or neo4j (INFRAHUB_DB_PROTOCOL). The routed form is
	// required to reach a multi-member deployment.
	Neo4jProtocol string
	// Neo4jTLS applies to the client connection used to probe the server version
	// and to restore, and it is what the deployment configured for Infrahub's
	// own connections. It is read, never obeyed: whether *this* tool verifies a
	// certificate is InsecureTLS's decision alone (FR-029).
	Neo4jTLS ExternalDBTLS
	// PostgresAddress overrides the discovered PostgreSQL endpoint as host[:port].
	PostgresAddress string
	// PostgresTLS is what the deployment's own task-manager connection URL says
	// about TLS — today only its `sslrootcert`, which is the certificate
	// authority the deployment's own client verifies that server against and
	// therefore the one this tool's client needs. It is read, never obeyed, for
	// the same FR-029 reason Neo4jTLS is.
	PostgresTLS ExternalDBTLS
	// ImageNeo4j and ImagePostgres are the images supplying the database tooling
	// for the transient workload.
	ImageNeo4j    string
	ImagePostgres string
	// ScratchSize is the scratch space granted to the transient workload, e.g.
	// 50Gi. Empty means derive it from the store size queried before the capture
	// (FR-022).
	ScratchSize string
	// InsecureTLS is this tool's own explicit opt-out from protecting the
	// connections it opens to an external server: it skips certificate
	// verification, and it is also the only thing that lets the channel be
	// unencrypted where the deployment records TLS as disabled. Without it the
	// channel is encrypted and verified whatever the deployment configures for
	// Infrahub's own connections.
	//
	// It is deliberately separate from Neo4jTLS.Insecure, which records what the
	// deployment configured for Infrahub's own connections. FR-029 turns on that
	// separation: a discovered insecure-TLS setting must not silently disable
	// verification for the credentials this tool sends, so only this field can.
	InsecureTLS bool
	// Scheduling carries what the operator's cluster requires of a pod it did
	// not author: where the tooling image may be pulled from, and where the pod
	// is allowed to run. Every field is empty by default and every empty field
	// is absent from the manifest, so a cluster that requires none of it gets
	// exactly the pod it got before.
	Scheduling ExternalDBScheduling

	// Timeout bounds any single operation against an external database (FR-025).
	Timeout time.Duration
	// Restore carries the authorisation to restore into a database the
	// deployment does not manage, and the channel that supplied it.
	Restore ExternalRestoreAuth
}

// ExternalDBScheduling is the placement and image-access surface for the
// transient workload, held as the operator wrote it and parsed once per run by
// resolveTransientScheduling.
//
// It exists because the image flags FR-027 provides are not usable on their
// own. An operator who must mirror the tooling image into their own registry —
// which is the whole reason those flags exist — has a registry that needs
// credentials, and a pod that can name no pull secret sits in ImagePullBackOff
// until the readiness wait gives up, with nothing the operator can pass to fix
// it. The same is true of a namespace whose only capable nodes are tainted, or
// which admits a pod only under a named service account.
//
// It is held as text rather than as parsed values so that one place — and one
// error message per input, naming the flag — turns the operator's words into
// manifest fields.
type ExternalDBScheduling struct {
	// ImagePullSecrets is a comma-separated list of secret names in the
	// deployment's namespace, each holding registry credentials for the
	// tooling image.
	ImagePullSecrets string

	// ServiceAccount is the service account the pod runs under. The pod mounts
	// no token whatever this says (automountServiceAccountToken is false), so
	// what it carries is the account's own image-pull secrets and whatever the
	// namespace's admission requires of one.
	ServiceAccount string

	// NodeSelector is a comma-separated list of key=value labels a node must
	// carry to run the workload.
	NodeSelector string

	// Tolerations is a comma-separated list of taints the workload tolerates,
	// each written as kubectl writes one: key=value:Effect, key:Effect, or a
	// bare key to tolerate every effect for it.
	Tolerations string

	// PriorityClass is the PriorityClass the pod is admitted under, for a
	// namespace whose quota or preemption rules require one.
	PriorityClass string
}

// Configuration holds the application configuration
type Configuration struct {
	BackupDir            string
	DockerComposeProject string
	K8sNamespace         string
	Neo4jUsername        string
	Neo4jPassword        string
	Neo4jDatabase        string
	PostgresUsername     string
	PostgresPassword     string
	PostgresDatabase     string
	S3                   *S3Config
	// Retention holds the backup retention rules. Its zero value activates no
	// rule, so retention is opt-in and never applied by default.
	Retention RetentionConfig
	Backend   BackendType
	Plakar    *PlakarConfig
	// ExternalDB holds the settings for databases that live outside the
	// deployment. It is inert for a deployment whose databases are all internal.
	ExternalDB ExternalDBConfig
}

// InfrahubOps is the main application struct
type InfrahubOps struct {
	config                  *Configuration
	backend                 EnvironmentBackend
	executor                *CommandExecutor
	dockerBackend           *DockerBackend
	kubernetesBackend       *KubernetesBackend
	infrahubInternalAddress string // cached INFRAHUB_INTERNAL_ADDRESS from task-worker
	// discoveredNeo4j and discoveredPostgres hold where the deployment's own
	// components say each database lives, read alongside the credentials. They
	// are kept off Configuration because that carries the operator's overrides,
	// which discovery must not overwrite; endpoint resolution reads both.
	discoveredNeo4j    discoveredEndpoint
	discoveredPostgres discoveredEndpoint
	// externalSources holds what the run's probe established about each database
	// that lives outside the deployment, keyed by the deployment service name.
	// It is written once at the gate and read, through externalDatabaseFor, by
	// the capture paths and by every decision that asks what the probe read, so
	// a database with no container is described by one answer rather than by
	// whichever caller asked last. A service absent from both this and
	// externalRestores is internal, which is what every capture path branches
	// on.
	externalSources map[string]*externalSource
	// externalRestores holds the workload and the facts this run prepared for
	// each database that lives outside the deployment and is about to be
	// written to, keyed by the deployment service name.
	//
	// It is the restore path's counterpart of externalSources, and it is a
	// second map rather than a flag on the first because the two hold different
	// things: a capture's source is probed and then given a workload sized from
	// what the probe read, while a restore's workload is created at the gate and
	// held for the whole run — the Bolt seed and the online confirmation both
	// run in it, long after the deployment's own components have been scaled to
	// zero. A service absent from it is internal, which is what every restore
	// path branches on.
	externalRestores map[string]*externalRestore
	// externalCATrust remembers the trust environment installed in each
	// transient workload, keyed by pod, so a workload's keystore is built once
	// however many clients ask for it (see installExternalCATrust).
	externalCATrust map[string]map[string]string
	// deploymentCAFiles remembers the certificate authorities read out of the
	// deployment's own components, keyed by "<container>:<path>".
	//
	// It is what makes the read survive the scale-down. The trust material is
	// read out of a running component — infrahub-server for Neo4j, task-manager
	// for PostgreSQL — but a transient workload is created per role, and the
	// PostgreSQL capture workload is created *after* an offline Neo4j capture
	// has scaled task-manager to zero. Read again at that point the `cat`
	// fails, the install degrades to the system trust store, and pg_dump then
	// fails verify-full against a privately signed server while blaming the
	// certificate rather than the container that is no longer there.
	//
	// The gate probes every external database while the deployment is still up,
	// so the entry every later install needs is already here. Only a successful
	// read is remembered: a failure says the component could not answer, not
	// that there is no authority.
	deploymentCAFiles map[string]string
	// incompleteCaptures records, per deployment service name, why a capture of
	// that database was not a complete one. It is the run's memory of FR-012:
	// the capture path fails as soon as it reaches this verdict, and this is
	// what lets the artefact's own removal be decided from the verdict rather
	// than from an error value that any layer between could have replaced.
	// Empty is the normal state, including for a run that never went external.
	incompleteCaptures map[string]string
	// externalRun is the identifier every transient object this run creates is
	// labelled with, minted once per run rather than once per database — see
	// externalRunID.
	externalRun string
	// transientStraysReaped records that this run has already swept the objects
	// earlier runs left behind, so a second external database does not sweep
	// again.
	transientStraysReaped bool
	// externalCaptureForbidden records that this run must not capture a
	// database that lives outside the deployment, because doing so means
	// creating and then deleting a Pod and a Secret in the operator's
	// namespace. It is set by CollectBundle, and read at the one place a
	// transient workload becomes possible (externalCaptureOps). Its zero value
	// is the backup tool's: capture permitted.
	externalCaptureForbidden bool
	// resolveConfiguration runs, at most once, the resolution of Configuration
	// from the root command ConfigureRootCommand was given. It is held here so
	// ResolveConfiguration can be asked for it by name from a command whose
	// argument validation runs before cobra reaches the persistent pre-run.
	// Nil until a root command is configured, which is every binary's second
	// statement and no test's obligation.
	resolveConfiguration func() error
}

// NewInfrahubOps creates a new InfrahubOps instance
func NewInfrahubOps() *InfrahubOps {
	executor := NewCommandExecutor()
	config := &Configuration{
		BackupDir:    getEnvOrDefault("BACKUP_DIR", filepath.Join(getCurrentDir(), "infrahub_backups")),
		K8sNamespace: os.Getenv("INFRAHUB_K8S_NAMESPACE"),
		S3: &S3Config{
			Region: "us-east-1",
		},
		Backend: BackendTarball,
		Plakar:  &PlakarConfig{},
		ExternalDB: ExternalDBConfig{
			Neo4jBackupPort: defaultNeo4jBackupPort,
			ImageNeo4j:      defaultExternalDBImageNeo4j,
			ImagePostgres:   defaultExternalDBImagePostgres,
			Timeout:         defaultExternalDBTimeout,
		},
	}
	return &InfrahubOps{
		config:   config,
		executor: executor,
	}
}

func (iops *InfrahubOps) Config() *Configuration {
	return iops.config
}

func (iops *InfrahubOps) getDockerBackend() *DockerBackend {
	if iops.dockerBackend == nil {
		iops.dockerBackend = NewDockerBackend(iops.config, iops.executor)
	}
	return iops.dockerBackend
}

func (iops *InfrahubOps) getKubernetesBackend() *KubernetesBackend {
	if iops.kubernetesBackend == nil {
		iops.kubernetesBackend = NewKubernetesBackend(iops.config, iops.executor)
	}
	return iops.kubernetesBackend
}

func (iops *InfrahubOps) backendOrder() []EnvironmentBackend {
	order := []EnvironmentBackend{}
	add := func(backend EnvironmentBackend) {
		if backend == nil {
			return
		}
		for _, existing := range order {
			if existing.Name() == backend.Name() {
				return
			}
		}
		order = append(order, backend)
	}

	if iops.config.K8sNamespace != "" {
		add(iops.getKubernetesBackend())
	}
	if iops.config.DockerComposeProject != "" {
		add(iops.getDockerBackend())
	}

	add(iops.getDockerBackend())
	add(iops.getKubernetesBackend())

	return order
}

func (iops *InfrahubOps) ensureBackend() (EnvironmentBackend, error) {
	if iops.backend != nil {
		return iops.backend, nil
	}

	detectionErrors := []string{}
	// missingRuntimes are the backends skipped because the CLI they shell out to
	// is not there. They are collected rather than only logged, because when
	// nothing is detected they are the answer: FR-018 requires the failure to
	// name what is missing, and "no Infrahub environment detected" names a
	// deployment when the actual problem is an absent tool on the host.
	missingRuntimes := []string{}
	for _, backend := range iops.backendOrder() {
		if backend == nil {
			continue
		}
		if err := backend.Detect(); err != nil {
			// Soft failures: CLI not available OR environment not found during auto-detect
			if errors.Is(err, ErrEnvironmentNotFound) || errors.Is(err, ErrCLIUnavailable) {
				logrus.Debugf("Skipping %s backend: %v", backend.Name(), err)
				if errors.Is(err, ErrCLIUnavailable) {
					missingRuntimes = append(missingRuntimes, runtimeCommandOf(backend))
				}

				continue
			}
			// Hard failures: something went wrong that should be reported
			detectionErrors = append(detectionErrors, fmt.Sprintf("%s: %v", backend.Name(), err))
			continue
		}
		iops.backend = backend
		logrus.Infof("Detected %s environment (%s)", backend.Name(), backend.Info())
		return backend, nil
	}

	if len(detectionErrors) > 0 {
		return nil, fmt.Errorf("environment detection errors: %s", strings.Join(detectionErrors, "; "))
	}

	// Every backend was skipped for want of its own CLI, so nothing was ever
	// asked about a deployment. Saying none was detected would send the operator
	// to look at their cluster for a tool that is missing from this host
	// (FR-018, constitution Principle III).
	if len(missingRuntimes) == len(iops.backendOrder()) && len(missingRuntimes) > 0 {
		return nil, fmt.Errorf("cannot detect an Infrahub environment: none of the required command-line tools (%s) was found on PATH, so neither deployment type could be queried", strings.Join(missingRuntimes, ", "))
	}

	return nil, fmt.Errorf("no Infrahub environment detected")
}

func (iops *InfrahubOps) Exec(service string, command []string, opts *ExecOptions) (string, error) {
	backend, err := iops.ensureBackend()
	if err != nil {
		return "", err
	}
	return backend.Exec(service, command, opts)
}

// getInfrahubInternalAddress fetches and caches INFRAHUB_INTERNAL_ADDRESS from task-worker.
// Returns empty string if the env var is not set (with a warning logged).
func (iops *InfrahubOps) getInfrahubInternalAddress() string {
	if iops.infrahubInternalAddress != "" {
		return iops.infrahubInternalAddress
	}

	backend, err := iops.ensureBackend()
	if err != nil {
		logrus.Warnf("Could not get backend to fetch INFRAHUB_INTERNAL_ADDRESS: %v", err)
		return ""
	}

	output, err := backend.Exec("task-worker", []string{"printenv", "INFRAHUB_INTERNAL_ADDRESS"}, nil)
	if err != nil {
		logrus.Debugf("INFRAHUB_INTERNAL_ADDRESS not set in task-worker container: %v", err)
		return ""
	}

	iops.infrahubInternalAddress = strings.TrimSpace(output)
	if iops.infrahubInternalAddress != "" {
		logrus.Debugf("Cached INFRAHUB_INTERNAL_ADDRESS: %s", iops.infrahubInternalAddress)
	}
	return iops.infrahubInternalAddress
}

// buildTaskWorkerExecOpts creates ExecOptions for task-worker commands with INFRAHUB_ADDRESS
// set to INFRAHUB_INTERNAL_ADDRESS and the SDK pagination size capped to the task-manager
// maximum. Existing options are merged with precedence to user values, then the pagination
// cap is enforced last so neither a caller nor the inherited deployment value can exceed it.
func (iops *InfrahubOps) buildTaskWorkerExecOpts(existingOpts *ExecOptions) *ExecOptions {
	internalAddr := iops.getInfrahubInternalAddress()

	opts := &ExecOptions{Env: make(map[string]string)}
	if existingOpts != nil {
		opts.User = existingOpts.User
	}

	if internalAddr != "" {
		opts.Env["INFRAHUB_ADDRESS"] = internalAddr
	}
	if existingOpts != nil {
		maps.Copy(opts.Env, existingOpts.Env)
	}

	if size, err := strconv.Atoi(opts.Env["INFRAHUB_PAGINATION_SIZE"]); err != nil || size <= 0 || size > maxPrefectPaginationSize {
		opts.Env["INFRAHUB_PAGINATION_SIZE"] = strconv.Itoa(maxPrefectPaginationSize)
	}

	return opts
}

func (iops *InfrahubOps) ExecStreamPipe(service string, command []string, opts *ExecOptions) (io.ReadCloser, func() error, error) {
	backend, err := iops.ensureBackend()
	if err != nil {
		return nil, nil, err
	}
	return backend.ExecStreamPipe(service, command, opts)
}

func (iops *InfrahubOps) ExecWritePipe(service string, command []string, opts *ExecOptions, stdin io.Reader) (func() error, error) {
	backend, err := iops.ensureBackend()
	if err != nil {
		return nil, err
	}
	return backend.ExecWritePipe(service, command, opts, stdin)
}

func (iops *InfrahubOps) ExecStream(service string, command []string, opts *ExecOptions) (string, error) {
	backend, err := iops.ensureBackend()
	if err != nil {
		return "", err
	}
	return backend.ExecStream(service, command, opts)
}

func (iops *InfrahubOps) CopyTo(service, src, dest string) error {
	backend, err := iops.ensureBackend()
	if err != nil {
		return err
	}
	return backend.CopyTo(service, src, dest)
}

func (iops *InfrahubOps) CopyFrom(service, src, dest string) error {
	backend, err := iops.ensureBackend()
	if err != nil {
		return err
	}
	return backend.CopyFrom(service, src, dest)
}

func (iops *InfrahubOps) StartServices(services ...string) error {
	backend, err := iops.ensureBackend()
	if err != nil {
		return err
	}
	return backend.Start(services...)
}

func (iops *InfrahubOps) StopServices(services ...string) error {
	backend, err := iops.ensureBackend()
	if err != nil {
		return err
	}
	return backend.Stop(services...)
}

func (iops *InfrahubOps) IsServiceRunning(service string) (bool, error) {
	backend, err := iops.ensureBackend()
	if err != nil {
		return false, err
	}
	return backend.IsRunning(service)
}

// GetAllPods returns all pod names for a service (Kubernetes only, returns nil for Docker)
func (iops *InfrahubOps) GetAllPods(service string) ([]string, error) {
	backend, err := iops.ensureBackend()
	if err != nil {
		return nil, err
	}
	if k8s, ok := backend.(*KubernetesBackend); ok {
		return k8s.GetAllPods(service)
	}
	// For Docker, return nil (single instance)
	return nil, nil
}

// Prerequisites checker
func (iops *InfrahubOps) checkPrerequisites() error {
	// Docker and kubectl are now optional. This function always succeeds.
	return nil
}

// Environment detection
func (iops *InfrahubOps) DetectEnvironment() error {
	logrus.Info("Detecting Infrahub deployment environment...")
	backend, err := iops.ensureBackend()
	if err != nil {
		return err
	}

	if backend.Info() != "" {
		logrus.Infof("Target: %s", backend.Info())
	}

	if err := iops.fetchDatabaseCredentials(); err != nil {
		return fmt.Errorf("could not fetch database credentials: %w", err)
	}

	return nil
}

// ReportEnvironment is what `environment detect` runs: the detection above, and
// then where each database lives (US2 scenario 5).
//
// It is a separate method rather than an addition to DetectEnvironment because
// that one is also the first step of `create`, `restore` and the task-manager
// flushes, and none of those wants the report — the backup paths establish the
// same locations a moment later through their own gate, and the flushes touch
// no database location at all. Adding it there would have put two pod listings
// on every flush to serve a report nobody asked for.
//
// Shared by all three binaries through AttachEnvironmentCommands, and it must
// stay correct for the read-only one: it resolves locations and reads the
// deployment's own configuration, and creates nothing (ADR-0003).
func (iops *InfrahubOps) ReportEnvironment() error {
	if err := iops.DetectEnvironment(); err != nil {
		return err
	}

	// After the credentials, because an external endpoint is resolved out of
	// the same component environment the credential fetch has just read: in
	// this order the deployment is asked once rather than twice.
	return iops.ReportDatabaseLocations()
}

func (iops *InfrahubOps) getInfrahubVersion() string {
	output, err := iops.Exec("infrahub-server", []string{"python", "-c", "import infrahub; print(infrahub.__version__)"}, nil)
	if err != nil {
		logrus.Warnf("Could not detect Infrahub version: %v", err)
		return "unknown"
	}

	return strings.TrimSpace(output)
}

func (iops *InfrahubOps) restartDependencies() error {
	logrus.Info("Restarting cache and message-queue")
	if err := iops.StopServices("cache", "message-queue"); err != nil {
		logrus.Debugf("Failed to stop cache/message-queue: %v", err)
	}
	if err := iops.StartServices("cache", "message-queue"); err != nil {
		return fmt.Errorf("failed to restart cache and message-queue: %w", err)
	}

	logrus.Info("Restarting task manager...")
	if err := iops.StopServices("task-manager"); err != nil {
		logrus.Debugf("Failed to stop task-manager: %v", err)
	}
	if err := iops.StopServices("task-manager-background-svc"); err != nil {
		logrus.Debugf("Failed to stop optional task-manager-background-svc: %v", err)
	}
	if err := iops.StartServices("task-manager"); err != nil {
		return fmt.Errorf("failed to restart task-manager: %w", err)
	}
	if err := iops.StartServices("task-manager-background-svc"); err != nil {
		logrus.Infof("Skipping optional task-manager-background-svc restart: %v", err)
	}

	return nil
}
