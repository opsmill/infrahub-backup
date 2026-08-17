package app

import (
	"strings"
	"testing"
)

// taskFlushBackend records what a flush command runs against the deployment and
// answers the task-manager pagination probe with paginationLimit when set.
type taskFlushBackend struct {
	bareBackend
	paginationLimit string
	commands        [][]string
	copies          []string
}

func (f *taskFlushBackend) Exec(service string, command []string, opts *ExecOptions) (string, error) {
	f.commands = append(f.commands, append([]string{service}, command...))
	if service == "task-manager" && strings.Contains(strings.Join(command, " "), "PREFECT_API_DEFAULT_LIMIT") {
		return f.paginationLimit, nil
	}
	return "", nil
}

func (f *taskFlushBackend) ExecStream(service string, command []string, opts *ExecOptions) (string, error) {
	return f.Exec(service, command, opts)
}

func (f *taskFlushBackend) CopyTo(service, src, dest string) error {
	f.copies = append(f.copies, service+":"+dest)
	return nil
}

func (f *taskFlushBackend) ran(command string) bool {
	for _, recorded := range f.commands {
		if strings.Join(recorded, " ") == command {
			return true
		}
	}
	return false
}

func (f *taskFlushBackend) recorded() string {
	lines := make([]string, 0, len(f.commands))
	for _, command := range f.commands {
		lines = append(lines, strings.Join(command, " "))
	}
	return strings.Join(lines, "\n")
}

func newFlushOps(backend *taskFlushBackend) *InfrahubOps {
	iops := NewInfrahubOps()
	iops.backend = backend
	return iops
}

func TestFlushStaleRunsRunsTheScriptInsteadOfTheInfrahubCLI(t *testing.T) {
	backend := &taskFlushBackend{}

	if err := newFlushOps(backend).FlushStaleRuns(-1, 0); err != nil {
		t.Fatalf("FlushStaleRuns() error = %v", err)
	}

	// `infrahub tasks flush stale-runs` hardcodes RUNNING, so covering PENDING runs
	// means the script has to drive the cleanup itself.
	for _, command := range backend.commands {
		if strings.Contains(strings.Join(command, " "), "tasks flush") {
			t.Fatalf("stale runs went through the infrahub CLI:\n%s", backend.recorded())
		}
	}

	if len(backend.copies) != 1 || backend.copies[0] != "task-worker:/tmp/infrahubops_clean_stale_tasks.py" {
		t.Fatalf("script copies = %v, want the stale-runs script on task-worker", backend.copies)
	}

	want := "task-worker python -u /tmp/infrahubops_clean_stale_tasks.py 2 200"
	if !backend.ran(want) {
		t.Fatalf("did not run %q, got:\n%s", want, backend.recorded())
	}
}

func TestFlushFlowRunsStillUsesTheInfrahubCLI(t *testing.T) {
	backend := &taskFlushBackend{}

	if err := newFlushOps(backend).FlushFlowRuns(-1, 0); err != nil {
		t.Fatalf("FlushFlowRuns() error = %v", err)
	}

	want := "task-worker infrahub tasks flush flow-runs --days-to-keep 30 --batch-size 200"
	if !backend.ran(want) {
		t.Fatalf("did not run %q, got:\n%s", want, backend.recorded())
	}
}

func TestFlushStaleRunsClampsTheBatchSizeToTheTaskManagerCap(t *testing.T) {
	backend := &taskFlushBackend{paginationLimit: "100"}

	if err := newFlushOps(backend).FlushStaleRuns(5, 500); err != nil {
		t.Fatalf("FlushStaleRuns() error = %v", err)
	}

	want := "task-worker python -u /tmp/infrahubops_clean_stale_tasks.py 5 100"
	if !backend.ran(want) {
		t.Fatalf("did not run %q, got:\n%s", want, backend.recorded())
	}
}
