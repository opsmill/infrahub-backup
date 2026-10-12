package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// Direct mode (FR-032, ADR 0017).
//
// When infrahub-backup itself runs as a pod in the target namespace, and its
// own image carries the database tools, the transient Job is a second pod doing
// what this one could do: the tools are on this process's PATH and the
// database is reachable from this pod's network. So the run executes them as
// child processes of its own and creates no Job and no Secret.
//
// The capture and restore sequences do not know which of the two they are
// using. Both hand around an execution target — the string the Job path calls
// a pod — and every operation they perform on one goes through four seams:
// the bounded exec (execBounded), the streaming exec (ExecStreamPipe), the copy
// across the workload's boundary (copyAcrossTransientWorkload) and the dump
// fit check (refuseUnfittableRestoreDump). Each seam asks localWorkloadFor
// first, and a target this file registered is served here. That is what keeps
// FR-032's "every other requirement applies unchanged" structural: the version
// gate, the edition gate, the scratch sizing, the incomplete-capture removal
// and the restore authorisation are the same lines of code either way.
//
// The sequences address the Job's scratch volume by its mount path,
// transientScratchPath. A local workload maps that path onto a run-specific
// directory, the way the Job's volume mount maps it onto an emptyDir: every
// argument, environment value and copy path is rewritten before it reaches the
// child (see translate). The directory is created under os.TempDir — the same
// place the run stages its own archive, which the operator moves with TMPDIR —
// so the copy out of it is a rename on one filesystem rather than a second
// copy of the database.

const (
	// serviceAccountNamespaceFile is where the kubelet writes the namespace of
	// the pod a container runs in. Its absence is the plain answer to "is this
	// process running as a pod at all".
	serviceAccountNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

	// localTargetPrefix starts every execution target a local workload hands
	// out. A colon cannot occur in a pod name, so no target this file issues can
	// be mistaken for a real pod, and no real pod can be mistaken for one of
	// these.
	localTargetPrefix = "local:"
)

// localToolsFor is every program the commands for one database run, by name.
// Direct mode is taken only when all of them are found on PATH, because a tool
// missing halfway through a capture is a failed capture, not a degraded one.
//
// The database clients are what the image adds; the rest are what the
// sequences' shell commands call (directory preparation, the CA write, the
// tar stream), which a minimal image could lack just as easily.
func localToolsFor(service string) []string {
	switch service {
	case serviceNeo4j:
		return []string{"neo4j-admin", "cypher-shell", "keytool", "sh", "tar", "rm", "mkdir", "ls", "base64"}
	case serviceTaskManagerDB:
		return []string{"pg_dump", "pg_restore", "psql", "sh", "rm", "base64"}
	default:
		return nil
	}
}

// directModeHost is everything direct mode reads from the host it runs on, as
// fields, so detection and the runner can be exercised against a fake host in
// tests without mutating process-wide state.
type directModeHost struct {
	// namespaceFile is serviceAccountNamespaceFile outside tests.
	namespaceFile string
	// lookPath is exec.LookPath outside tests.
	lookPath func(file string) (string, error)
	// tempDir is the parent of every run-specific scratch directory.
	tempDir func() string
	// freeSpace reports the bytes available to an unprivileged writer under
	// path.
	freeSpace func(path string) (uint64, error)
	// command builds the child process. Tests wrap it to record what reached
	// the child; every child is still a real process.
	command func(ctx context.Context, name string, args ...string) *exec.Cmd
}

func defaultDirectModeHost() *directModeHost {
	return &directModeHost{
		namespaceFile: serviceAccountNamespaceFile,
		lookPath:      exec.LookPath,
		tempDir:       os.TempDir,
		freeSpace:     localFreeSpace,
		command:       exec.CommandContext,
	}
}

// directHost is the host direct mode reads, the real one unless a test
// supplied another.
func (iops *InfrahubOps) directHost() *directModeHost {
	if iops.directModeHost == nil {
		iops.directModeHost = defaultDirectModeHost()
	}

	return iops.directModeHost
}

// directModeDecision is the answer to "run the tools here?" and why.
type directModeDecision struct {
	direct bool
	reason string
}

// detectDirectMode applies FR-032's two conditions: this process runs as a pod
// in the namespace the run targets, and every tool the databases need is on
// PATH. A third condition is practical rather than stated: the scratch parent
// must be a path the sequences' unquoted shell commands can carry, which any
// default temporary directory is.
func (h *directModeHost) detectDirectMode(namespace string, services []string) directModeDecision {
	if len(services) == 0 {
		return directModeDecision{reason: "no database was named to run tools for"}
	}

	content, err := os.ReadFile(h.namespaceFile)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return directModeDecision{reason: fmt.Sprintf("this process is not running as a Kubernetes pod (no %s)", h.namespaceFile)}
		}

		return directModeDecision{reason: fmt.Sprintf("the pod's namespace could not be read from %s: %v", h.namespaceFile, err)}
	}

	own := strings.TrimSpace(string(content))
	if own != namespace {
		return directModeDecision{reason: fmt.Sprintf("this pod runs in namespace %q, not in the target namespace %q", own, namespace)}
	}

	missing := []string{}
	for _, service := range services {
		tools := localToolsFor(service)
		if tools == nil {
			return directModeDecision{reason: fmt.Sprintf("%s is not a database this tool runs clients for", service)}
		}

		for _, tool := range tools {
			if _, err := h.lookPath(tool); err != nil {
				missing = append(missing, tool)
			}
		}
	}

	if len(missing) > 0 {
		sort.Strings(missing)

		return directModeDecision{reason: fmt.Sprintf("this pod runs in namespace %q but %s not found on PATH", namespace, describeMissingTools(dedupeSorted(missing)))}
	}

	if parent := h.tempDir(); !isShellSafePath(parent) {
		return directModeDecision{reason: fmt.Sprintf("the temporary directory %q contains characters the database commands cannot carry unquoted; set TMPDIR to a plain path", parent)}
	}

	return directModeDecision{direct: true, reason: fmt.Sprintf("this pod runs in the target namespace %q and finds %s on PATH", namespace, strings.Join(services, " and ")+" tools")}
}

func describeMissingTools(tools []string) string {
	if len(tools) == 1 {
		return tools[0] + " is"
	}

	return strings.Join(tools, ", ") + " are"
}

func dedupeSorted(sorted []string) []string {
	out := sorted[:0]
	for i, value := range sorted {
		if i == 0 || value != sorted[i-1] {
			out = append(out, value)
		}
	}

	return out
}

// isShellSafePath reports whether path can appear unquoted in the sh -c
// commands the sequences build (`rm -rf /scratch/...`), once translated.
func isShellSafePath(path string) bool {
	if path == "" {
		return false
	}

	for i := 0; i < len(path); i++ {
		if !isPathByte(path[i]) {
			return false
		}
	}

	return true
}

// isPathByte is a byte that can continue a path token. '+' is included so that
// base64 text, which the CA write carries in an argument, can never present
// the scratch mount path as a token of its own (see translate).
func isPathByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
		c == '_' || c == '.' || c == '-' || c == '/' || c == '+'
}

// decideDirectMode settles, once per database, whether this run executes the
// database tools in its own process, and logs the answer at info.
//
// It is per database and remembered, so the probe, the capture and the restore
// of one database all run in the same place: a later call for a database
// already decided reads the memo. It reports direct only when every database
// asked about is direct, because the caller builds one set of operations for
// all of them.
func (iops *InfrahubOps) decideDirectMode(namespace string, services []string) bool {
	if iops.directModeDecisions == nil {
		iops.directModeDecisions = map[string]bool{}
	}

	undecided := []string{}
	for _, service := range services {
		if _, decided := iops.directModeDecisions[service]; !decided {
			undecided = append(undecided, service)
		}
	}

	if len(undecided) > 0 {
		decision := iops.directHost().detectDirectMode(namespace, undecided)
		for _, service := range undecided {
			iops.directModeDecisions[service] = decision.direct
		}

		if decision.direct {
			logrus.Infof("Running the external database tools in this process, with no transient Job: %s", decision.reason)
		} else {
			logrus.Infof("Running the external database tools in a transient Job, not in this process: %s", decision.reason)
		}
	}

	if len(services) == 0 {
		return false
	}

	for _, service := range services {
		if !iops.directModeDecisions[service] {
			return false
		}
	}

	return true
}

// ---------------------------------------------------------------------------
// The local workload
// ---------------------------------------------------------------------------

// localWorkload is the stand-in for one transient Job when the tools run in
// this process: a run-specific scratch directory, the credentials the Job
// would have received from a Secret, and the host it runs children on.
type localWorkload struct {
	service string
	role    workloadRole
	scratch string
	// credentials reach the child only through its environment (FR-014): they
	// are appended to cmd.Env and never to an argument.
	credentials map[string]string
	host        *directModeHost
}

// translate maps the Job's scratch mount path onto this workload's directory
// wherever it appears as a path of its own: a whole argument, the start of one
// (`/scratch/infrahubops`), or a token inside one (`--temp-path=/scratch/x`,
// `rm -rf /scratch/a /scratch/b`, `'/scratch/ca.pem'`). An occurrence that is
// part of a longer path (`/data/scratch`, `/scratchpad`) is left alone.
func (w *localWorkload) translate(value string) string {
	if !strings.Contains(value, transientScratchPath) {
		return value
	}

	var out strings.Builder
	for {
		index := strings.Index(value, transientScratchPath)
		if index < 0 {
			out.WriteString(value)

			return out.String()
		}

		end := index + len(transientScratchPath)
		startsToken := index == 0 || !isPathByte(value[index-1])
		endsToken := end == len(value) || value[end] == '/' || !isPathByte(value[end])

		out.WriteString(value[:index])
		if startsToken && endsToken {
			out.WriteString(w.scratch)
		} else {
			out.WriteString(transientScratchPath)
		}

		value = value[end:]
	}
}

// within resolves a path a sequence named in the Job's scratch volume to this
// workload's directory, and refuses any other path: a copy across the
// workload's boundary has no business outside its scratch space, and on this
// host there is nothing else to protect it.
func (w *localWorkload) within(path string) (string, error) {
	translated := filepath.Clean(w.translate(path))
	if translated != w.scratch && !strings.HasPrefix(translated, w.scratch+string(filepath.Separator)) {
		return "", fmt.Errorf("refusing to copy %s: it is not in the scratch space of the %s %s run in this process", path, w.service, w.role)
	}

	return translated, nil
}

// environment is the child's environment: this process's own, then the
// credentials, then what the sequence asked for. Later entries win, which
// os/exec guarantees for duplicate keys.
func (w *localWorkload) environment(extra map[string]string) []string {
	env := os.Environ()

	for _, key := range sortedKeys(w.credentials) {
		env = append(env, key+"="+w.credentials[key])
	}

	for _, key := range sortedKeys(extra) {
		env = append(env, key+"="+w.translate(extra[key]))
	}

	return env
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	return keys
}

// prepare builds the child for one command, with its argument vector
// translated and its environment carrying the credentials.
func (w *localWorkload) prepare(ctx context.Context, command []string, opts *ExecOptions) (*exec.Cmd, error) {
	if len(command) == 0 {
		return nil, fmt.Errorf("no command was given to run for the %s %s", w.service, w.role)
	}

	args := make([]string, len(command))
	for i, arg := range command {
		args[i] = w.translate(arg)
	}

	var extra map[string]string
	if opts != nil {
		extra = opts.Env
	}

	cmd := w.host.command(ctx, args[0], args[1:]...)
	cmd.Env = w.environment(extra)
	cmd.Dir = w.scratch

	return cmd, nil
}

// run is the bounded exec: stdout and stderr apart, and a *timeoutError when
// the bound expired, which is what execBounded's callers read either way.
func (w *localWorkload) run(ctx context.Context, bound time.Duration, command []string, opts *ExecOptions) (string, string, error) {
	cctx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()

	cmd, err := w.prepare(cctx, command, opts)
	if err != nil {
		return "", "", err
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = commandWaitDelay

	err = cmd.Run()
	out, errOut := strings.TrimSpace(stdout.String()), strings.TrimSpace(stderr.String())
	if err != nil && errors.Is(cctx.Err(), context.DeadlineExceeded) {
		return out, errOut, &timeoutError{timeout: bound}
	}

	return out, errOut, err
}

// stream is the streaming exec: the caller reads stdout and then calls wait.
// It carries no bound of its own, like the Job path's stream: its callers hold
// an idle deadline over the reader.
func (w *localWorkload) stream(command []string, opts *ExecOptions) (io.ReadCloser, func() error, error) {
	ctx, cancel := context.WithCancel(context.Background())

	cmd, err := w.prepare(ctx, command, opts)
	if err != nil {
		cancel()

		return nil, nil, err
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()

		return nil, nil, err
	}

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		cancel()

		return nil, nil, err
	}

	wait := func() error {
		defer cancel()

		return withStderr(cmd.Wait(), stderr.String())
	}

	return stdout, wait, nil
}

// copyOut takes an artifact out of the scratch directory to the run's own
// staging, the counterpart of `kubectl cp pod:src dest`: dest becomes the file
// or directory src was. A rename where both are on one filesystem, which they
// are by default; a copy otherwise.
func (w *localWorkload) copyOut(src, dest string) error {
	from, err := w.within(src)
	if err != nil {
		return err
	}

	if _, err := os.Stat(dest); errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
			return err
		}

		if err := os.Rename(from, dest); err == nil {
			return nil
		}
	}

	return copyTree(from, dest)
}

// copyIn places a file from the run's own staging in the scratch directory.
func (w *localWorkload) copyIn(src, dest string) error {
	to, err := w.within(dest)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
		return err
	}

	return copyFile(src, to)
}

// requireFree refuses an operation that needs more scratch space than the
// directory's filesystem has free. what names the content, need is the bytes
// it needs, and remedy is what the operator can change.
func (w *localWorkload) requireFree(what string, need int64, remedy string) error {
	free, err := w.host.freeSpace(w.scratch)
	if err != nil {
		return fmt.Errorf("cannot check whether %s fits the free space under %s: %w", what, w.scratch, err)
	}

	if need > 0 && uint64(need) > free {
		return fmt.Errorf("%s needs %s, and the filesystem holding %s has %s free: %s. Nothing was changed",
			what, formatBytes(need), filepath.Dir(w.scratch), formatBytes(int64(min(free, uint64(1)<<62))), remedy)
	}

	return nil
}

// release removes the scratch directory and everything in it. It is called on
// every exit path: by the sequence when it gives the workload back, and by the
// run's deferred releaseTransientWorkloads for anything still registered.
func (w *localWorkload) release() {
	if err := os.RemoveAll(w.scratch); err != nil {
		logrus.Warnf("Could not remove the scratch directory %s of the %s %s run in this process: %v", w.scratch, w.service, w.role, err)
	}
}

// copyTree copies a file or a directory tree to dest, which becomes src, with
// the copy helpers the run already stages its archive with.
func copyTree(src, dest string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}

	if info.IsDir() {
		return copyDir(src, dest)
	}

	return copyFile(src, dest)
}

// ---------------------------------------------------------------------------
// Binding the sequences to this process
// ---------------------------------------------------------------------------

// localWorkloadFor is the local workload an execution target names, or nil for
// a real pod or no target at all. It is the one question each seam asks.
func (iops *InfrahubOps) localWorkloadFor(target string) *localWorkload {
	if !strings.HasPrefix(target, localTargetPrefix) {
		return nil
	}

	return iops.localWorkloads[target]
}

// localWorkloadForOptions is localWorkloadFor for the exec seams.
func (iops *InfrahubOps) localWorkloadForOptions(opts *ExecOptions) *localWorkload {
	if opts == nil {
		return nil
	}

	return iops.localWorkloadFor(strings.TrimSpace(opts.Pod))
}

// startLocalWorkload creates the scratch directory for one role and registers
// it, returning the execution target the sequences will name and how to give
// it back.
func (iops *InfrahubOps) startLocalWorkload(service string, role workloadRole) (string, *localWorkload, func(), error) {
	credentials, err := transientCredentials(iops.config, service)
	if err != nil {
		return "", nil, nil, err
	}

	host := iops.directHost()

	scratch, err := os.MkdirTemp(host.tempDir(), fmt.Sprintf("%s-%s-%s-", transientObjectPrefix, role, service))
	if err != nil {
		return "", nil, nil, fmt.Errorf("cannot create the scratch directory for the %s %s run in this process: %w", service, role, err)
	}

	workload := &localWorkload{service: service, role: role, scratch: scratch, credentials: credentials, host: host}

	iops.localWorkloadSeq++
	target := fmt.Sprintf("%s%s-%s-%d", localTargetPrefix, role, service, iops.localWorkloadSeq)

	if iops.localWorkloads == nil {
		iops.localWorkloads = map[string]*localWorkload{}
	}
	iops.localWorkloads[target] = workload

	return target, workload, func() { iops.releaseLocalWorkload(target) }, nil
}

// releaseLocalWorkload gives back one local workload. Safe to call twice.
func (iops *InfrahubOps) releaseLocalWorkload(target string) {
	workload, held := iops.localWorkloads[target]
	if !held {
		return
	}

	delete(iops.localWorkloads, target)
	delete(iops.externalCATrust, target)
	workload.release()
}

// releaseLocalWorkloads gives back every local workload this run still holds:
// the backstop on every exit path, as ReleaseTransientWorkloads is for Jobs.
func (iops *InfrahubOps) releaseLocalWorkloads() {
	targets := make([]string, 0, len(iops.localWorkloads))
	for target := range iops.localWorkloads {
		targets = append(targets, target)
	}

	for _, target := range targets {
		iops.releaseLocalWorkload(target)
	}
}

// localCaptureOps are externalCaptureOps served by this process. They create
// nothing in the namespace, so none of the Job path's steps run before them:
// no run identifier is minted, no stray is reaped, no Secret or Job exists.
func (iops *InfrahubOps) localCaptureOps() externalCaptureOps {
	return externalCaptureOps{
		startProbe: func(service string, _ int) (string, func(), error) {
			target, _, release, err := iops.startLocalWorkload(service, workloadRoleProbe)

			return target, release, err
		},
		startCapture: func(service string, storeSizeBytes int64) (string, func(), error) {
			// FR-022 unchanged: the size is derived from the store, or the
			// override, or the run stops naming the override. What changes is
			// what it is compared with: the free space this pod has, where the
			// Job path would size a volume.
			size, err := resolveScratchSize(service, iops.config.ExternalDB.ScratchSize, storeSizeBytes)
			if err != nil {
				return "", nil, err
			}

			need, err := parseStorageQuantity(size)
			if err != nil {
				return "", nil, fmt.Errorf("cannot read the scratch size %s for the %s capture: %w", size, service, err)
			}

			target, workload, release, err := iops.startLocalWorkload(service, workloadRoleCapture)
			if err != nil {
				return "", nil, err
			}

			if err := workload.requireFree("the "+service+" capture's scratch space", need,
				"free space there, point TMPDIR at a larger volume, or supply --external-db-scratch-size if the derived size is wrong"); err != nil {
				release()

				return "", nil, err
			}

			return target, release, nil
		},
		startRestore: func(service string) (string, func(), error) {
			target, _, release, err := iops.startLocalWorkload(service, workloadRoleRestore)

			return target, release, err
		},
		probe: iops.probeExternalDatabase,
	}
}
