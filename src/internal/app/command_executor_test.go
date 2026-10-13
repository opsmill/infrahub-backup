package app

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFormatCommandTimeout(t *testing.T) {
	tests := []struct {
		name string
		d    time.Duration
		want string
	}{
		{"exec dump default", collectExecTimeout, "60s"},
		{"transfer default", collectTransferTimeout, "300s"},
		{"sub-second", 100 * time.Millisecond, "100ms"},
		{"fractional seconds", 1500 * time.Millisecond, "1.5s"},
		{"one second", time.Second, "1s"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatCommandTimeout(tt.d); got != tt.want {
				t.Errorf("formatCommandTimeout(%v) = %q, want %q", tt.d, got, tt.want)
			}
		})
	}
}

func TestTimeoutError_Message(t *testing.T) {
	err := &timeoutError{timeout: collectExecTimeout}
	if err.Error() != "timed out after 60s" {
		t.Errorf("Error() = %q, want %q", err.Error(), "timed out after 60s")
	}
}

func TestRunCommandContext_Success(t *testing.T) {
	ce := NewCommandExecutor()
	output, err := ce.runCommandContext(context.Background(), 10*time.Second, "echo", "hello")
	if err != nil {
		t.Fatalf("runCommandContext failed: %v", err)
	}
	if output != "hello" {
		t.Errorf("output = %q, want %q", output, "hello")
	}
}

func TestRunCommandContext_Timeout(t *testing.T) {
	ce := NewCommandExecutor()
	start := time.Now()
	_, err := ce.runCommandContext(context.Background(), 100*time.Millisecond, "sleep", "5")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("runCommandContext did not fail, want timeout error")
	}
	var timeout *timeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("error = %v (%T), want *timeoutError", err, err)
	}
	if err.Error() != "timed out after 100ms" {
		t.Errorf("Error() = %q, want %q", err.Error(), "timed out after 100ms")
	}
	if elapsed > 3*time.Second {
		t.Errorf("command was not killed at the timeout: took %v", elapsed)
	}
}

func TestRunCommandContext_NonTimeoutFailure(t *testing.T) {
	ce := NewCommandExecutor()
	_, err := ce.runCommandContext(context.Background(), 10*time.Second, "sh", "-c", "exit 3")
	if err == nil {
		t.Fatal("runCommandContext succeeded, want exit error")
	}
	var timeout *timeoutError
	if errors.As(err, &timeout) {
		t.Errorf("plain command failure was reported as a timeout: %v", err)
	}
}

func TestRunCommandContext_ParentCancellationIsNotTimeout(t *testing.T) {
	ce := NewCommandExecutor()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := ce.runCommandContext(ctx, 10*time.Second, "echo", "hello")
	if err == nil {
		t.Fatal("runCommandContext with cancelled parent succeeded, want error")
	}
	var timeout *timeoutError
	if errors.As(err, &timeout) {
		t.Errorf("parent cancellation was reported as a timeout: %v", err)
	}
}

func TestRunCommandPipeContext_Success(t *testing.T) {
	ce := NewCommandExecutor()
	stdout, wait, err := ce.runCommandPipeContext(context.Background(), 10*time.Second, "sh", "-c", "printf 'a\\nb\\n'")
	if err != nil {
		t.Fatalf("runCommandPipeContext failed to start: %v", err)
	}

	content, err := io.ReadAll(stdout)
	if err != nil {
		t.Fatalf("reading stdout failed: %v", err)
	}
	if err := wait(); err != nil {
		t.Fatalf("wait failed: %v", err)
	}
	if string(content) != "a\nb\n" {
		t.Errorf("output = %q, want %q", content, "a\nb\n")
	}
}

// TestRunCommandPipeContext_Timeout is the stdout-only sibling of
// TestRunCommandCombinedPipeContext_Timeout, and carried the identical race:
// the same 100ms shared by process spawn, `exec` and a pipe flush, with the
// same assertion that the output had arrived. It fails under -count=50 for the
// same reason and is made deterministic the same way.
func TestRunCommandPipeContext_Timeout(t *testing.T) {
	const bound = time.Hour

	sentinel := filepath.Join(t.TempDir(), "written")
	parent := &deadlineOnDemand{done: make(chan struct{})}

	ce := NewCommandExecutor()
	stdout, wait, err := ce.runCommandPipeContext(parent, bound,
		"sh", "-c", `echo started; : > "$0"; exec sleep 300`, sentinel)
	if err != nil {
		t.Fatalf("runCommandPipeContext failed to start: %v", err)
	}

	waitForFile(t, sentinel)
	parent.expire()

	content, err := io.ReadAll(stdout)
	if err != nil {
		t.Fatalf("reading stdout failed: %v", err)
	}
	if !strings.Contains(string(content), "started") {
		t.Errorf("output before timeout = %q, want to contain %q", content, "started")
	}

	err = wait()
	if err == nil {
		t.Fatal("wait succeeded, want timeout error")
	}
	var timeout *timeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("error = %v (%T), want *timeoutError", err, err)
	}
	// The reported duration is the bound that was configured, not the time the
	// command happened to run for.
	if want := "timed out after " + formatCommandTimeout(bound); err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}
}

func TestRunCommandCombinedPipeContext_MergesStderr(t *testing.T) {
	ce := NewCommandExecutor()
	reader, wait, err := ce.runCommandCombinedPipeContext(context.Background(), 10*time.Second, "sh", "-c", "printf 'out\\n'; printf 'err\\n' >&2")
	if err != nil {
		t.Fatalf("runCommandCombinedPipeContext failed to start: %v", err)
	}

	content, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("reading combined output failed: %v", err)
	}
	if err := wait(); err != nil {
		t.Fatalf("wait failed: %v", err)
	}
	for _, want := range []string{"out\n", "err\n"} {
		if !strings.Contains(string(content), want) {
			t.Errorf("combined output = %q, want it to contain %q", content, want)
		}
	}
}

func TestRunCommandCombinedPipeContext_ReaderSeesEOFWhenCommandExits(t *testing.T) {
	ce := NewCommandExecutor()
	reader, wait, err := ce.runCommandCombinedPipeContext(context.Background(), 10*time.Second, "echo", "done")
	if err != nil {
		t.Fatalf("runCommandCombinedPipeContext failed to start: %v", err)
	}

	// io.ReadAll only returns if the parent's write end was closed correctly;
	// a leaked descriptor would block this read forever (test timeout).
	if _, err := io.ReadAll(reader); err != nil {
		t.Fatalf("reading combined output failed: %v", err)
	}
	if err := wait(); err != nil {
		t.Fatalf("wait failed: %v", err)
	}
}

// deadlineOnDemand is a context whose deadline the test fires by hand.
//
// It takes the wall clock out of a test whose subject is an ordering rather
// than a duration. Err reports context.DeadlineExceeded because that is what
// the production path reads to tell a bound expiring apart from a command
// failing, and context.WithTimeout over a parent like this one propagates that
// error to the context the command is actually started with.
type deadlineOnDemand struct {
	done chan struct{}
}

func (d *deadlineOnDemand) Deadline() (time.Time, bool) { return time.Time{}, false }
func (d *deadlineOnDemand) Done() <-chan struct{}       { return d.done }
func (*deadlineOnDemand) Value(any) any                 { return nil }

func (d *deadlineOnDemand) Err() error {
	select {
	case <-d.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func (d *deadlineOnDemand) expire() { close(d.done) }

// TestRunCommandCombinedPipeContext_Timeout asserts that output the command
// wrote before its bound expired is still readable afterwards: a killed
// command's diagnostics are most of what makes a timeout reportable.
//
// The ordering is established rather than assumed. The original test gave
// process spawn, `exec` and a pipe flush a shared 100ms budget and then
// asserted the output had arrived, so a loaded machine failed it for losing a
// race rather than for the pipe having lost anything — invisible to a clean run
// and reproducible under -count=25. Here the child announces, through a file,
// that it has finished writing; only then is the deadline fired. Nothing is
// consumed from the pipe until after the kill, so what the assertion reads is
// still what survived it.
func TestRunCommandCombinedPipeContext_Timeout(t *testing.T) {
	sentinel := filepath.Join(t.TempDir(), "written")
	parent := &deadlineOnDemand{done: make(chan struct{})}

	ce := NewCommandExecutor()
	// The bound is long enough that it cannot be what fires. The deadline under
	// test is the parent's, and expire() below is what fires it.
	reader, wait, err := ce.runCommandCombinedPipeContext(parent, time.Hour,
		"sh", "-c", `echo started; : > "$0"; exec sleep 300`, sentinel)
	if err != nil {
		t.Fatalf("runCommandCombinedPipeContext failed to start: %v", err)
	}

	waitForFile(t, sentinel)
	parent.expire()

	content, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("reading combined output failed: %v", err)
	}
	if !strings.Contains(string(content), "started") {
		t.Errorf("output before timeout = %q, want to contain %q", content, "started")
	}

	err = wait()
	if err == nil {
		t.Fatal("wait succeeded, want timeout error")
	}
	var timeout *timeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("error = %v (%T), want *timeoutError", err, err)
	}
}

// waitForFile blocks until a path exists. It converges rather than budgeting: a
// path that never appears means the child never ran, which is a failure worth
// the test binary's own timeout reporting, not one worth guessing a duration
// for.
func waitForFile(t *testing.T, path string) {
	t.Helper()

	for {
		if _, err := os.Stat(path); err == nil {
			return
		}

		select {
		case <-t.Context().Done():
			t.Fatalf("the command never created %s, so it never wrote its output", path)
		case <-time.After(time.Millisecond):
		}
	}
}

func TestRunCommandPipeContext_CommandFailureKeepsStderr(t *testing.T) {
	ce := NewCommandExecutor()
	stdout, wait, err := ce.runCommandPipeContext(context.Background(), 10*time.Second, "sh", "-c", "echo oops >&2; exit 2")
	if err != nil {
		t.Fatalf("runCommandPipeContext failed to start: %v", err)
	}
	if _, err := io.ReadAll(stdout); err != nil {
		t.Fatalf("reading stdout failed: %v", err)
	}

	err = wait()
	if err == nil {
		t.Fatal("wait succeeded, want exit error")
	}
	var timeout *timeoutError
	if errors.As(err, &timeout) {
		t.Errorf("plain command failure was reported as a timeout: %v", err)
	}
	if !strings.Contains(err.Error(), "oops") {
		t.Errorf("error = %q, want stderr content included", err)
	}
}

func TestRunCommandSeparateContext_KeepsStreamsApart(t *testing.T) {
	ce := NewCommandExecutor()
	stdout, stderr, err := ce.runCommandSeparateContext(context.Background(), 10*time.Second,
		"sh", "-c", `printf '{"ok": true}'; printf 'notice\n' >&2`)
	if err != nil {
		t.Fatalf("runCommandSeparateContext failed: %v", err)
	}
	if stdout != `{"ok": true}` {
		t.Errorf("stdout = %q, want the payload alone", stdout)
	}
	if stderr != "notice" {
		t.Errorf("stderr = %q, want %q", stderr, "notice")
	}
}

func TestRunCommandSeparateContext_FailureKeepsBothStreams(t *testing.T) {
	ce := NewCommandExecutor()
	stdout, stderr, err := ce.runCommandSeparateContext(context.Background(), 10*time.Second,
		"sh", "-c", `printf 'partial'; printf 'boom\n' >&2; exit 3`)
	if err == nil {
		t.Fatal("runCommandSeparateContext succeeded, want exit error")
	}
	var timeout *timeoutError
	if errors.As(err, &timeout) {
		t.Errorf("plain command failure was reported as a timeout: %v", err)
	}
	if stdout != "partial" || stderr != "boom" {
		t.Errorf("stdout = %q, stderr = %q, want %q and %q", stdout, stderr, "partial", "boom")
	}
}

func TestRunCommandSeparateContext_Timeout(t *testing.T) {
	ce := NewCommandExecutor()
	_, _, err := ce.runCommandSeparateContext(context.Background(), 100*time.Millisecond, "sleep", "5")
	var timeout *timeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("error = %v (%T), want *timeoutError", err, err)
	}
}
