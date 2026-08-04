package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// Bundle directory names for the parity diagnostics collectors
// (specs/003-collect-tool/contracts/bundle-layout.md).
const (
	databaseBundleDir     = "database"
	messageQueueBundleDir = "message-queue"
	cacheBundleDir        = "cache"
	taskWorkerBundleDir   = "task-worker"
	taskManagerBundleDir  = "task-manager"
	serverBundleDir       = "server"
)

// neo4jLogPath is where the official Neo4j image writes its server logs.
const neo4jLogPath = "/logs"

// prefectServerAPIFallback is used when the task-manager container does not
// define PREFECT_API_URL: the Prefect server serves its API on port 4200 by
// default, and the command runs inside that container.
const prefectServerAPIFallback = "http://localhost:4200/api"

// infrahubServerAPIFallback is used when INFRAHUB_INTERNAL_ADDRESS is not
// discoverable: the fetch runs inside the infrahub-server container, where
// the API listens on its default port.
const infrahubServerAPIFallback = "http://localhost:8000"

// prefectEventsScript is the embedded script dumping recent Prefect events
// (the Prefect CLI has no non-streaming events command; research R9).
const (
	prefectEventsScript       = "collect_prefect_events.py"
	prefectEventsScriptTarget = "/tmp/infrahubops_collect_prefect_events.py"
)

// contextExecer is an optional backend capability: a timeout-bounded variant
// of Exec (research R2: 60s per status dump). Both concrete backends
// implement it; collectors fall back to plain Exec on backends (or test
// fakes) that do not.
type contextExecer interface {
	ExecContext(ctx context.Context, timeout time.Duration, service string, command []string, opts *ExecOptions) (string, error)
}

// replicaExecer is an optional backend capability: execute a command in one
// specific replica instead of the backend's default pod/container for the
// service. KubernetesBackend implements it; on backends without it the
// collectors fall back to service-level Exec (single-replica behavior).
type replicaExecer interface {
	ExecReplica(ctx context.Context, timeout time.Duration, replica Replica, command []string) (string, error)
}

// separateExecer is an optional backend capability: a timeout-bounded exec that
// returns the command's stdout and stderr separately instead of merged. Dump
// collectors need it because a dump's file must hold the command's payload
// alone: merged output lets kubectl's `Defaulted container "x" out of: …`
// notice, or any warning the command writes to stderr, land inside the file and
// corrupt it (a .json dump then parses as nothing at all). Both concrete
// backends implement it; collectors fall back to merged output on backends (or
// test fakes) that do not.
type separateExecer interface {
	ExecSeparateContext(ctx context.Context, timeout time.Duration, service string, command []string, opts *ExecOptions) (string, string, error)
}

// separateReplicaExecer is separateExecer for a specific replica: the
// per-replica variant used by the task-worker collector.
type separateReplicaExecer interface {
	ExecReplicaSeparate(ctx context.Context, timeout time.Duration, replica Replica, command []string) (string, string, error)
}

// contextCopier is an optional backend capability: a timeout-bounded variant of
// CopyFrom (research R2, FIX-1). Both concrete backends implement it; the
// collect path must use it so a wedged container/daemon cannot hang the run on
// an unbounded file copy. There is no fallback to the unbounded CopyFrom.
type contextCopier interface {
	CopyFromContext(ctx context.Context, timeout time.Duration, service, src, dest string) error
}

// copyFrom copies a file out of a service container bounded by the collect
// transfer timeout. A backend without the bounded-copy capability is a
// per-collector failure (recorded in the manifest) rather than an unbounded
// hang.
func (cc *collectContext) copyFrom(service, src, dest string) error {
	copier, ok := cc.backend.(contextCopier)
	if !ok {
		return fmt.Errorf("%s environment does not support bounded file copy for bundle collection", cc.backend.Name())
	}
	return copier.CopyFromContext(cc.ctx, collectTransferTimeout, service, src, dest)
}

// captureTimeout returns the first *timeoutError seen: the already-captured one
// if present, otherwise the one unwrapped from err (or nil). Aggregating
// collectors use it to preserve a command timeout through string aggregation so
// the orchestrator can normalize the manifest reason to the bare
// "timed out after <duration>" contract string (research R2, FIX-4).
func captureTimeout(existing *timeoutError, err error) *timeoutError {
	if existing != nil {
		return existing
	}
	var t *timeoutError
	if errors.As(err, &t) {
		return t
	}
	return nil
}

// partialError builds an aggregated partial-failure error from per-item
// reasons. When any sub-failure was a command timeout, the *timeoutError is
// wrapped with %w so the orchestrator's errors.As normalization reports the
// bare "timed out after <duration>" reason (research R2, FIX-4). Non-timeout
// partial failures keep their per-item reasons verbatim.
func partialError(prefix string, failures []string, timeout *timeoutError) error {
	if len(failures) == 0 {
		return nil
	}
	joined := strings.Join(failures, "; ")
	if timeout != nil {
		return fmt.Errorf("%s: %s: %w", prefix, joined, timeout)
	}
	return fmt.Errorf("%s: %s", prefix, joined)
}

// execDumpTimeout runs a command inside a service container bounded by the
// given timeout when the backend supports it, falling back to the shared
// unbounded Exec on backends (or test fakes) that do not implement the bounded
// variant. Collectors whose command may run longer than a status dump (e.g. the
// telemetry export, which pages the API) pass collectTransferTimeout.
func (cc *collectContext) execDumpTimeout(timeout time.Duration, service string, command []string) (string, error) {
	if execer, ok := cc.backend.(contextExecer); ok {
		return execer.ExecContext(cc.ctx, timeout, service, command, nil)
	}
	return cc.backend.Exec(service, command, nil)
}

// execDump runs a command inside a service container, bounded by the collect
// exec timeout when the backend supports it.
func (cc *collectContext) execDump(service string, command []string) (string, error) {
	return cc.execDumpTimeout(collectExecTimeout, service, command)
}

// execReplicaDump runs a command inside one specific replica, falling back to
// service-level exec when the backend cannot target replicas.
func (cc *collectContext) execReplicaDump(replica Replica, command []string) (string, error) {
	if execer, ok := cc.backend.(replicaExecer); ok {
		return execer.ExecReplica(cc.ctx, collectExecTimeout, replica, command)
	}
	return cc.execDump(replica.Service, command)
}

// execDumpSeparate runs a command inside a service container and returns its
// stdout and stderr separately, so the caller can write the payload to the
// bundle without runtime notices or command diagnostics mixed in. Backends
// without the capability (test fakes) fall back to merged output returned as
// stdout.
func (cc *collectContext) execDumpSeparate(service string, command []string) (string, string, error) {
	if execer, ok := cc.backend.(separateExecer); ok {
		return execer.ExecSeparateContext(cc.ctx, collectExecTimeout, service, command, nil)
	}
	output, err := cc.execDump(service, command)
	return output, "", err
}

// execReplicaDumpSeparate is execDumpSeparate targeted at one replica, falling
// back to service-level exec when the backend cannot target replicas.
func (cc *collectContext) execReplicaDumpSeparate(replica Replica, command []string) (string, string, error) {
	if execer, ok := cc.backend.(separateReplicaExecer); ok {
		return execer.ExecReplicaSeparate(cc.ctx, collectExecTimeout, replica, command)
	}
	output, err := cc.execReplicaDump(replica, command)
	return output, "", err
}

// skipWhenServiceAbsent mirrors serviceLogCollector's precondition: a service
// with zero replicas is not deployed in this environment, so the collector is
// skipped. Enumeration errors (and backends without the collect primitives)
// leave the decision to run(), which surfaces real failures as failed.
func skipWhenServiceAbsent(service string) func(cc *collectContext) (bool, string) {
	return func(cc *collectContext) (bool, string) {
		cb, err := cc.collect()
		if err != nil {
			return false, ""
		}
		replicas, err := cb.ServiceReplicas(service)
		if err != nil {
			return false, ""
		}
		if len(replicas) == 0 {
			return true, "service not deployed"
		}
		return false, ""
	}
}

// execDumpSpec describes one diagnostic dump: the command executed inside the
// service container and the bundle file its output is written to. mask, when
// set, is applied to the output before it is written (research R5).
type execDumpSpec struct {
	filename string
	command  []string
	mask     func(string) string
}

// writeDumpFile writes one dump to the bundle, masking it first when the spec
// requires it.
func writeDumpFile(path, content string, mask func(string) string) error {
	if mask != nil {
		content = mask(content)
	}
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return fmt.Errorf("failed to write %s: %w", path, err)
	}
	return nil
}

// dumpFailurePath is the sibling path a failed dump's diagnostics are written
// to: flow-runs.json → flow-runs.err.txt. The failure never goes into the
// dump's own path, so a present dump file always holds the payload the command
// produced and nothing else — a support engineer reading flow-runs.json must
// never find `error: command terminated with exit code 137` where the JSON
// document belongs (FIX-8).
func dumpFailurePath(path string) string {
	return strings.TrimSuffix(path, filepath.Ext(path)) + ".err.txt"
}

// renderDumpFailure describes a failed dump for the bundle: what ran, how it
// failed, and whatever the command managed to write to either stream. It is
// never empty, so the bundle always records the attempt even when the command
// produced no output at all (the "container not found" case, where the previous
// file was simply missing with no explanation).
func renderDumpFailure(command []string, err error, stdout, stderr string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "command: %s\n", strings.Join(command, " "))
	fmt.Fprintf(&b, "error: %v\n", err)
	if stderr != "" {
		fmt.Fprintf(&b, "\n--- stderr ---\n%s\n", stderr)
	}
	if stdout != "" {
		fmt.Fprintf(&b, "\n--- stdout ---\n%s\n", stdout)
	}
	return b.String()
}

// writeDumpFailure records a failed dump next to where its payload would have
// gone. The dump's mask still applies: an error path can echo the environment
// or configuration it choked on.
func writeDumpFailure(path string, command []string, err error, stdout, stderr string, mask func(string) string) error {
	return writeDumpFile(dumpFailurePath(path), renderDumpFailure(command, err, stdout, stderr), mask)
}

// execDumpsInto runs each dump inside service and writes the outputs into
// dir, returning one failure string per dump that could not be fully
// collected plus the first command timeout observed (FIX-4). Only the command's
// stdout reaches the dump file; a failed dump's diagnostics — masked — go to a
// sibling .err.txt so the bundle shows what the service reported (FR-009)
// without corrupting the payload file.
func (cc *collectContext) execDumpsInto(service, dir string, dumps []execDumpSpec) ([]string, *timeoutError) {
	failures := []string{}
	var timeout *timeoutError
	for _, dump := range dumps {
		path := filepath.Join(dir, dump.filename)
		stdout, stderr, err := cc.execDumpSeparate(service, dump.command)
		if err != nil {
			logrus.Warnf("Failed to run %q in %s: %v", strings.Join(dump.command, " "), service, err)
			failures = append(failures, fmt.Sprintf("%s: %v", dump.filename, err))
			timeout = captureTimeout(timeout, err)
			if writeErr := writeDumpFailure(path, dump.command, err, stdout, stderr, dump.mask); writeErr != nil {
				failures = append(failures, writeErr.Error())
			}
			continue
		}
		// A successful command's stderr is runtime noise (kubectl container
		// notices, deprecation warnings), not bundle content: keep it out of
		// the payload and out of the bundle, but log it for a verbose run.
		if stderr != "" {
			logrus.Debugf("stderr from %q in %s: %s", strings.Join(dump.command, " "), service, stderr)
		}
		if writeErr := writeDumpFile(path, stdout, dump.mask); writeErr != nil {
			failures = append(failures, writeErr.Error())
		}
	}
	return failures, timeout
}

// runExecDumps creates bundle/<dirName>/ and collects the dumps into it,
// returning the aggregated failures and the first command timeout observed.
func (cc *collectContext) runExecDumps(service, dirName string, dumps []execDumpSpec) ([]string, *timeoutError) {
	dir := filepath.Join(cc.bundleDir, dirName)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return []string{fmt.Sprintf("failed to create %s directory: %v", dirName, err)}, nil
	}
	return cc.execDumpsInto(service, dir, dumps)
}

// execDumpCollector builds a collector that runs a fixed set of exec dumps
// inside one service. Partial failures keep the successful dumps in the
// bundle and mark the collector failed with the aggregated reasons; a command
// timeout is wrapped so the manifest reason collapses to the bare timeout
// string (FIX-4).
func execDumpCollector(name, service, dirName string, dumps func() []execDumpSpec) collector {
	return collector{
		name: name,
		skip: skipWhenServiceAbsent(service),
		run: func(cc *collectContext) error {
			failures, timeout := cc.runExecDumps(service, dirName, dumps())
			return partialError(fmt.Sprintf("partial %s collection", dirName), failures, timeout)
		},
	}
}

// --- database (T018) ---

// defaultDatabaseLogFiles are the Neo4j server logs always collected; query
// logs join them only with --include-queries (FR-004).
var defaultDatabaseLogFiles = []string{"neo4j.log", "debug.log"}

// databaseLogsCollector copies the Neo4j server logs into bundle/database/.
func databaseLogsCollector() collector {
	return collector{
		name: "database-logs",
		skip: skipWhenServiceAbsent("database"),
		run:  collectDatabaseLogs,
	}
}

func collectDatabaseLogs(cc *collectContext) error {
	dir := filepath.Join(cc.bundleDir, databaseBundleDir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create %s directory: %w", databaseBundleDir, err)
	}

	failures := []string{}
	var timeout *timeoutError
	files := defaultDatabaseLogFiles
	if cc.opts.IncludeQueries {
		// The full log directory includes rotated query logs
		// (query.log, query.log.1, ...), so it is enumerated live.
		names, err := cc.listDatabaseLogFiles()
		switch {
		case err != nil:
			failures = append(failures, fmt.Sprintf("failed to list %s: %v", neo4jLogPath, err))
			timeout = captureTimeout(timeout, err)
		case len(names) == 0:
			// Enumeration returned nothing (empty ls output): fall back to the
			// default server logs so the collector never records an empty
			// database/ directory as success (FIX-7).
			logrus.Warnf("Neo4j log directory %s enumerated empty; falling back to the default log set", neo4jLogPath)
		default:
			files = names
		}
	}

	for _, name := range files {
		src := neo4jLogPath + "/" + name
		if err := cc.copyFrom("database", src, filepath.Join(dir, name)); err != nil {
			logrus.Warnf("Failed to copy %s from database: %v", src, err)
			failures = append(failures, fmt.Sprintf("%s: %v", name, err))
			timeout = captureTimeout(timeout, err)
		}
	}

	return partialError("partial database log collection", failures, timeout)
}

// listDatabaseLogFiles enumerates the files in the Neo4j log directory. Every
// line of stdout becomes a filename to copy, so the listing must not carry
// stderr: on a multi-container database pod kubectl's "Defaulted container"
// notice would otherwise become a bogus log file and fail its copy.
func (cc *collectContext) listDatabaseLogFiles() ([]string, error) {
	output, _, err := cc.execDumpSeparate("database", []string{"sh", "-c", "cd " + neo4jLogPath + " && ls -1"})
	if err != nil {
		return nil, err
	}
	return nonEmptyLines(output), nil
}

// --- message-queue (T019) ---

// messageQueueDumps lists the RabbitMQ parity dumps (research R9), one file
// per dump; environment and status pass through Erlang-config masking
// (research R5).
func messageQueueDumps() []execDumpSpec {
	return []execDumpSpec{
		{filename: "queues.txt", command: []string{"rabbitmqctl", "list_queues"}},
		{filename: "exchanges.txt", command: []string{"rabbitmqctl", "list_exchanges"}},
		{filename: "bindings.txt", command: []string{"rabbitmqctl", "list_bindings"}},
		{filename: "connections.txt", command: []string{"rabbitmqctl", "list_connections"}},
		{filename: "channels.txt", command: []string{"rabbitmqctl", "list_channels"}},
		{filename: "status.txt", command: []string{"rabbitmqctl", "status"}, mask: maskErlangConfig},
		{filename: "environment.txt", command: []string{"rabbitmqctl", "environment"}, mask: maskErlangConfig},
	}
}

func messageQueueCollector() collector {
	return execDumpCollector("message-queue-status", "message-queue", messageQueueBundleDir, messageQueueDumps)
}

// --- cache (T020) ---

// cacheDumps lists the Redis parity dumps (research R9); the configuration
// dump passes through key/value-pair masking (research R5). redis-cli runs
// unauthenticated, matching the default deployment; a password-protected
// Redis surfaces its NOAUTH error in the dump files.
func cacheDumps() []execDumpSpec {
	return []execDumpSpec{
		{filename: "info.txt", command: []string{"redis-cli", "info"}},
		{filename: "clients.txt", command: []string{"redis-cli", "client", "list"}},
		{filename: "config.txt", command: []string{"redis-cli", "config", "get", "*"}, mask: maskConfigPairs},
		{filename: "slowlog.txt", command: []string{"redis-cli", "slowlog", "get"}},
		{filename: "dbsize.txt", command: []string{"redis-cli", "dbsize"}},
	}
}

func cacheCollector() collector {
	return execDumpCollector("cache-status", "cache", cacheBundleDir, cacheDumps)
}

// --- task-worker (T021) ---

// taskWorkerDumps lists the per-replica Prefect worker dumps. The Prefect
// worker exposes no dedicated status subcommand, so parity is the CLI
// version, the effective configuration (masked — it can carry API keys), and
// the work pools visible to the worker.
func taskWorkerDumps() []execDumpSpec {
	return []execDumpSpec{
		{filename: "version.txt", command: []string{"prefect", "version"}},
		{filename: "config.txt", command: []string{"prefect", "config", "view"}, mask: maskEnvOutput},
		{filename: "work-pools.txt", command: []string{"prefect", "work-pool", "ls"}},
	}
}

// taskWorkerCollector captures Prefect worker CLI output per replica into
// bundle/task-worker/<replica>/.
func taskWorkerCollector() collector {
	return collector{
		name: "task-worker-state",
		skip: skipWhenServiceAbsent("task-worker"),
		run:  collectTaskWorkerState,
	}
}

func collectTaskWorkerState(cc *collectContext) error {
	cb, err := cc.collect()
	if err != nil {
		return err
	}
	replicas, err := cb.ServiceReplicas("task-worker")
	if err != nil {
		return fmt.Errorf("failed to enumerate task-worker replicas: %w", err)
	}
	if len(replicas) == 0 {
		return fmt.Errorf("no replicas found for service task-worker")
	}

	counts := podContainerCounts(replicas)
	failures := []string{}
	var timeout *timeoutError
	collected := 0
	for _, replica := range replicas {
		replicaName := replicaBaseName(replica, counts[replica.Pod] > 1)
		replicaDir := filepath.Join(cc.bundleDir, taskWorkerBundleDir, replicaName)
		if err := os.MkdirAll(replicaDir, 0755); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", replicaName, err))
			continue
		}
		for _, dump := range taskWorkerDumps() {
			path := filepath.Join(replicaDir, dump.filename)
			stdout, stderr, err := cc.execReplicaDumpSeparate(replica, dump.command)
			if err != nil {
				logrus.Warnf("Failed to run %q in %s: %v", strings.Join(dump.command, " "), replicaName, err)
				failures = append(failures, fmt.Sprintf("%s/%s: %v", replicaName, dump.filename, err))
				timeout = captureTimeout(timeout, err)
				// A failing CLI often prints the interesting error itself;
				// keep it beside the (absent) dump rather than inside it.
				if writeErr := writeDumpFailure(path, dump.command, err, stdout, stderr, dump.mask); writeErr != nil {
					failures = append(failures, writeErr.Error())
				}
				continue
			}
			collected++
			if stderr != "" {
				logrus.Debugf("stderr from %q in %s: %s", strings.Join(dump.command, " "), replicaName, stderr)
			}
			if writeErr := writeDumpFile(path, stdout, dump.mask); writeErr != nil {
				failures = append(failures, writeErr.Error())
			}
		}
	}

	// Per research R9 the collector is failed only when nothing at all could
	// be collected; partial output is a success with warnings (the files show
	// which commands failed).
	if collected == 0 {
		return partialError("task-worker state collection produced no output", failures, timeout)
	}
	if len(failures) > 0 {
		logrus.Warnf("Partial task-worker state collection: %s", strings.Join(failures, "; "))
	}
	return nil
}

// --- task-manager (T022) ---

// prefectServerCommand wraps a command so it runs with PREFECT_API_URL
// defaulting to the local Prefect server; a value already present in the
// container environment wins over the fallback. The fallback therefore only
// applies when the query runs inside the Prefect server itself — a task-worker
// already has the variable pointing at the task manager.
func prefectServerCommand(command ...string) []string {
	script := fmt.Sprintf(`export PREFECT_API_URL="${PREFECT_API_URL:-%s}"; exec "$@"`, prefectServerAPIFallback)
	return append([]string{"sh", "-c", script, "sh"}, command...)
}

// taskManagerQueryService picks which container the Prefect state queries run
// in. A task-worker is preferred: it is already an API client of the task
// manager (its PREFECT_API_URL points there, and taskWorkerDumps' unwrapped
// `prefect work-pool ls` only works because of it), so a listing that
// materializes every recent flow run spends a worker's memory instead of the
// Prefect server's. That matters because the server is the single point of
// failure for the whole task pipeline: a field bundle showed
// `prefect flow-run ls --limit 200 --output json` killed inside the server
// container (exit code 137), after which the next two dumps reported a stream
// error and then `container not found` — the collector had taken the task
// manager down with it. The task-manager container stays the fallback for
// deployments that run no worker.
func (cc *collectContext) taskManagerQueryService() string {
	cb, err := cc.collect()
	if err != nil {
		return "task-manager"
	}
	replicas, err := cb.ServiceReplicas("task-worker")
	if err != nil || len(replicas) == 0 {
		return "task-manager"
	}
	return "task-worker"
}

// taskManagerActiveStates are the flow/task-run state types treated as
// "in flight" for troubleshooting: work accepted and awaiting execution
// (PENDING) or actively running (RUNNING). Passed to the CLI as repeated
// --state-type filters (stable across Prefect 2.x and 3.x).
var taskManagerActiveStates = []string{"PENDING", "RUNNING"}

// taskManagerDumps lists the Prefect server state dumps collected via the
// Prefect CLI, run in whichever container taskManagerQueryService picks
// (research R9). Alongside the
// recent flow runs, the PENDING and RUNNING flow runs and task runs are
// captured explicitly: `prefect flow-run ls` returns only the most recent runs
// regardless of state, so in-flight work can be buried beyond its limit on a
// busy instance, and there is no unfiltered task-run listing at all. Recent
// events have no CLI equivalent and are collected separately (see
// collectTaskManagerState).
//
// Only `prefect flow-run ls` accepts --output json, so the flow-run dumps are
// captured as JSON (.json); the other list commands (work-pool, work-queue,
// task-run, automation) have no JSON option and stay plain text (.txt).
func taskManagerDumps() []execDumpSpec {
	return []execDumpSpec{
		{filename: "work-pools.txt", command: prefectServerCommand("prefect", "work-pool", "ls")},
		{filename: "work-queues.txt", command: prefectServerCommand("prefect", "work-queue", "ls")},
		{filename: "flow-runs.json", command: prefectServerCommand(runListCommand("flow-run", nil, true)...)},
		{filename: "flow-runs-pending-running.json", command: prefectServerCommand(runListCommand("flow-run", taskManagerActiveStates, true)...)},
		{filename: "task-runs-pending-running.txt", command: prefectServerCommand(runListCommand("task-run", taskManagerActiveStates, false)...)},
		{filename: "automations.txt", command: prefectServerCommand("prefect", "automation", "ls")},
	}
}

// runListCommand builds a `prefect <resource> ls` invocation. states, when
// non-empty, adds a repeated --state-type filter per state; jsonOutput adds
// --output json (supported only by `flow-run ls`, so callers pass false for
// task-run and the other list commands). The limit matches the recent
// flow-runs dump so a busy instance never silently truncates the in-flight set
// below what the recent listing shows.
func runListCommand(resource string, states []string, jsonOutput bool) []string {
	command := []string{"prefect", resource, "ls"}
	for _, state := range states {
		command = append(command, "--state-type", state)
	}
	command = append(command, "--limit", "200")
	if jsonOutput {
		command = append(command, "--output", "json")
	}
	return command
}

// taskManagerCollector captures work pools, work queues, recent flow runs,
// the pending/running flow and task runs, automations, and recent events into
// bundle/task-manager/. The precondition is the task manager itself — it owns
// the state being captured — even though the queries are executed from a
// task-worker where one is deployed (taskManagerQueryService).
func taskManagerCollector() collector {
	return collector{
		name: "task-manager-state",
		skip: skipWhenServiceAbsent("task-manager"),
		run:  collectTaskManagerState,
	}
}

func collectTaskManagerState(cc *collectContext) error {
	service := cc.taskManagerQueryService()
	logrus.Debugf("Querying task-manager state from the %s container", service)
	failures, timeout := cc.runExecDumps(service, taskManagerBundleDir, taskManagerDumps())

	dir := filepath.Join(cc.bundleDir, taskManagerBundleDir)
	eventsPath := filepath.Join(dir, "events.json")
	command, stdout, stderr, err := cc.runEmbeddedScriptDump(service, prefectEventsScript, prefectEventsScriptTarget)
	if err != nil {
		logrus.Warnf("Failed to collect Prefect events: %v", err)
		failures = append(failures, fmt.Sprintf("events.json: %v", err))
		timeout = captureTimeout(timeout, err)
		// Writing a Python traceback to events.json would masquerade as a valid
		// events dump (FIX-8); preserve it in a sibling .err.txt for support
		// instead. The failed reason is already recorded above.
		if writeErr := writeDumpFailure(eventsPath, command, err, stdout, stderr, nil); writeErr != nil {
			failures = append(failures, writeErr.Error())
		}
	} else if writeErr := writeDumpFile(eventsPath, stdout, nil); writeErr != nil {
		failures = append(failures, writeErr.Error())
	}

	return partialError("partial task-manager state collection", failures, timeout)
}

// runEmbeddedScriptDump copies an embedded Python script into a service
// container, runs it under the collect exec timeout, and removes it again.
// Unlike executeScriptWithOpts it captures the output quietly instead of
// streaming it to the console — dump payloads belong in the bundle, not the
// progress log. It returns the command it ran plus the script's stdout and
// stderr separately: only stdout is the dump payload, and a Python traceback on
// stderr must never end up inside a .json file.
func (cc *collectContext) runEmbeddedScriptDump(service, scriptName, targetPath string) ([]string, string, string, error) {
	command := []string{"python", "-u", targetPath}

	scriptContent, err := readEmbeddedScript(scriptName)
	if err != nil {
		return command, "", "", fmt.Errorf("could not retrieve %s: %w", scriptName, err)
	}

	tmpFile, err := os.CreateTemp("", "infrahubops_collect_*.py")
	if err != nil {
		return command, "", "", fmt.Errorf("failed to create temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name())
	if _, err := tmpFile.Write(scriptContent); err != nil {
		tmpFile.Close()
		return command, "", "", fmt.Errorf("failed to write script: %w", err)
	}
	tmpFile.Close()

	if err := cc.backend.CopyTo(service, tmpFile.Name(), targetPath); err != nil {
		return command, "", "", fmt.Errorf("failed to copy %s to %s: %w", scriptName, service, err)
	}
	defer func() {
		if _, err := cc.backend.Exec(service, []string{"rm", "-f", targetPath}, nil); err != nil {
			logrus.Warnf("Failed to clean up script %s on %s: %v", targetPath, service, err)
		}
	}()

	stdout, stderr, err := cc.execDumpSeparate(service, command)
	return command, stdout, stderr, err
}

// --- server (T023) ---

// serverAPITarget maps one Infrahub API document to its bundle file.
type serverAPITarget struct {
	filename string
	path     string
	mask     func(string) string
}

// serverAPITargets lists the API documents captured into bundle/server/. The
// configuration dump is masked (research R5); info and schema carry no
// credentials. Paths that a given Infrahub version does not serve produce a
// failed reason in the manifest and an observable HTTP error in the sibling
// <name>.err.txt, leaving the .json file absent rather than holding a body that
// is not the document it names.
func serverAPITargets() []serverAPITarget {
	return []serverAPITarget{
		{filename: "info.json", path: "/api/info"},
		{filename: "config.json", path: "/api/config", mask: maskJSON},
		{filename: "schema.json", path: "/api/schema"},
	}
}

// serverAPIFetchCommand builds the Python command fetching one API document
// from inside the infrahub-server container (the image ships Python and
// httpx, not necessarily curl; research R8). The body is printed as-is; a
// non-2xx status is appended on stderr and reported via the exit code so the
// collector records the failure while the response stays observable.
func serverAPIFetchCommand(url string) []string {
	script := strings.Join([]string{
		"import sys",
		"import httpx",
		"resp = httpx.get(sys.argv[1], timeout=30)",
		"sys.stdout.write(resp.text)",
		"if resp.status_code >= 400:",
		"    sys.stderr.write('\\nHTTP %d\\n' % resp.status_code)",
		"    sys.exit(1)",
	}, "\n")
	return []string{"python", "-c", script, url}
}

// infrahubStatusFilename is the bundle file holding the InfrahubStatus query
// result.
const infrahubStatusFilename = "infrahub_status.json"

// infrahubStatusQuery reports whether every Infrahub worker agrees on the
// active schema (summary.schema_hash_synced) together with each worker's
// individual hash and active state. The REST /api/schema dump shows what the
// schema is; only this query shows whether the workers are actually in sync,
// which is the fastest signal for a schema-out-of-sync incident.
const infrahubStatusQuery = `query {
  InfrahubStatus {
    summary {
      schema_hash_synced
    }
    workers {
      edges {
        node {
          id
          active
          schema_hash
        }
      }
    }
  }
}`

// serverGraphQLFetchCommand builds the Python command that runs a GraphQL query
// against the Infrahub server from inside the infrahub-server container using
// the bundled Infrahub SDK (infrahub_sdk), which the server already depends on.
// The SDK resolves the branch-qualified /graphql endpoint and, when a token is
// present, sends it with the correct auth header — so this collector hardcodes
// neither. The token (INFRAHUB_API_TOKEN, falling back to
// INFRAHUB_INITIAL_ADMIN_TOKEN) is read from the container environment and used
// only for the request; it is never written to the bundle. On success the
// result is re-wrapped in a {"data": ...} GraphQL envelope; any failure (import,
// auth, transport, or a GraphQL error) is captured as {"errors": [...]} and
// reported via the exit code, so the collector records the failure while the
// payload stays observable in the sibling <name>.err.txt. retry_on_failure
// defaults to false, so a wedged
// server fails fast within the collector's exec budget rather than looping.
func serverGraphQLFetchCommand(address, query string) []string {
	script := strings.Join([]string{
		"import json",
		"import os",
		"import sys",
		"try:",
		"    from infrahub_sdk import Config, InfrahubClientSync",
		`    token = os.environ.get("INFRAHUB_API_TOKEN") or os.environ.get("INFRAHUB_INITIAL_ADMIN_TOKEN")`,
		"    config = Config(api_token=token) if token else Config()",
		"    client = InfrahubClientSync(address=sys.argv[1], config=config)",
		"    data = client.execute_graphql(query=sys.argv[2], timeout=30)",
		"except Exception as exc:",
		`    json.dump({"errors": [{"message": str(exc)}]}, sys.stdout, indent=2)`,
		`    sys.stdout.write("\n")`,
		`    sys.stderr.write("InfrahubStatus query failed: %s\n" % exc)`,
		"    sys.exit(1)",
		`json.dump({"data": data}, sys.stdout, indent=2)`,
		`sys.stdout.write("\n")`,
	}, "\n")
	return []string{"python", "-c", script, address, query}
}

// serverPackagesCommand lists the packages installed in the same interpreter
// that imports infrahub, using the stdlib importlib.metadata. Infrahub runs
// from a uv-managed virtualenv that does not install pip, so `pip list`
// resolves to a base interpreter and reports only that environment's handful
// of packages (packaging/pip/wheel). Going through `python` — which correctly
// resolves to the venv, as version detection already relies on — lists the
// real dependency set. Output is `name==version` (pip-freeze style), sorted
// case-insensitively and de-duplicated across metadata directories.
func serverPackagesCommand() []string {
	script := strings.Join([]string{
		"import importlib.metadata as md",
		"seen = {}",
		"for dist in md.distributions():",
		"    try:",
		"        name = dist.metadata['Name']",
		"        version = dist.version",
		"    except Exception:",
		"        continue",
		"    if not name:",
		"        continue",
		"    seen[name.lower()] = (name, version)",
		"for key in sorted(seen):",
		"    name, version = seen[key]",
		"    print('%s==%s' % (name, version))",
	}, "\n")
	return []string{"python", "-c", script}
}

// serverInfoCollector captures the Infrahub version, installed packages,
// masked environment, and API info/config/schema into bundle/server/.
func serverInfoCollector() collector {
	return collector{
		name: "server-info",
		skip: skipWhenServiceAbsent("infrahub-server"),
		run:  collectServerInfo,
	}
}

// kubectlDefaultedContainerNotice is the prefix of the message kubectl writes
// to stderr when it execs into a multi-container pod without an explicit
// container: `Defaulted container "x" out of: x, y`. The dump path keeps stderr
// out of captured output (separateExecer), so this is defense in depth for
// single-value reads (an env var, a version string) on backends that still
// return merged output: a contaminated INFRAHUB_INTERNAL_ADDRESS carries an
// embedded newline and yields a URL httpx rejects.
const kubectlDefaultedContainerNotice = `Defaulted container "`

// stripKubectlExecNotices removes kubectl's "Defaulted container" notice lines
// from merged exec output and returns the remainder trimmed. Output from
// backends that emit no such notice (Docker exec) passes through as a plain
// trim.
func stripKubectlExecNotices(output string) string {
	if !strings.Contains(output, kubectlDefaultedContainerNotice) {
		return strings.TrimSpace(output)
	}
	lines := strings.Split(output, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), kubectlDefaultedContainerNotice) {
			continue
		}
		kept = append(kept, line)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

// collectInfrahubVersion detects the Infrahub version bounded by the collect
// exec timeout (FIX-5). It mirrors iops.getInfrahubVersion but goes through the
// bounded execDump so a wedged infrahub-server cannot hang the run; it does not
// touch the shared unbounded path the backup tool uses.
func (cc *collectContext) collectInfrahubVersion() string {
	output, _, err := cc.execDumpSeparate("infrahub-server", []string{"python", "-c", "import infrahub; print(infrahub.__version__)"})
	if err != nil {
		logrus.Warnf("Could not detect Infrahub version: %v", err)
		return "unknown"
	}
	return stripKubectlExecNotices(output)
}

// collectInfrahubInternalAddress fetches INFRAHUB_INTERNAL_ADDRESS from the
// task-worker container bounded by the collect exec timeout (FIX-5). Empty
// string when unset or unreachable. Unlike iops.getInfrahubInternalAddress it
// never runs an unbounded exec and does not populate the shared cache.
func (cc *collectContext) collectInfrahubInternalAddress() string {
	output, _, err := cc.execDumpSeparate("task-worker", []string{"printenv", "INFRAHUB_INTERNAL_ADDRESS"})
	if err != nil {
		logrus.Debugf("INFRAHUB_INTERNAL_ADDRESS not set in task-worker container: %v", err)
		return ""
	}
	return stripKubectlExecNotices(output)
}

func collectServerInfo(cc *collectContext) error {
	dir := filepath.Join(cc.bundleDir, serverBundleDir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create %s directory: %w", serverBundleDir, err)
	}

	failures := []string{}

	version := cc.collectInfrahubVersion()
	if version != "" && version != "unknown" && cc.manifest != nil {
		cc.manifest.InfrahubVersion = version
	}
	if err := writeDumpFile(filepath.Join(dir, "version.txt"), version, nil); err != nil {
		failures = append(failures, err.Error())
	}

	dumps := []execDumpSpec{
		{filename: "packages.txt", command: serverPackagesCommand()},
		{filename: "environment.txt", command: []string{"env"}, mask: maskEnvOutput},
	}

	baseURL := cc.collectInfrahubInternalAddress()
	if baseURL == "" {
		baseURL = infrahubServerAPIFallback
	}
	baseURL = strings.TrimRight(baseURL, "/")
	for _, target := range serverAPITargets() {
		dumps = append(dumps, execDumpSpec{
			filename: target.filename,
			command:  serverAPIFetchCommand(baseURL + target.path),
			mask:     target.mask,
		})
	}

	// The InfrahubStatus GraphQL query reports schema-hash sync across workers,
	// which the REST endpoints above cannot. The SDK derives the /graphql
	// endpoint from this base address. Its result carries no credentials, so it
	// is written unmasked.
	dumps = append(dumps, execDumpSpec{
		filename: infrahubStatusFilename,
		command:  serverGraphQLFetchCommand(baseURL, infrahubStatusQuery),
	})

	dumpFailures, timeout := cc.execDumpsInto("infrahub-server", dir, dumps)
	failures = append(failures, dumpFailures...)

	return partialError("partial server info collection", failures, timeout)
}
