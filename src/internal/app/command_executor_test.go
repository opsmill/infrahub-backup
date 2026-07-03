package app

import (
	"context"
	"errors"
	"io"
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

func TestRunCommandPipeContext_Timeout(t *testing.T) {
	ce := NewCommandExecutor()
	stdout, wait, err := ce.runCommandPipeContext(context.Background(), 100*time.Millisecond, "sh", "-c", "echo started; exec sleep 5")
	if err != nil {
		t.Fatalf("runCommandPipeContext failed to start: %v", err)
	}

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
	if err.Error() != "timed out after 100ms" {
		t.Errorf("Error() = %q, want %q", err.Error(), "timed out after 100ms")
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

func TestRunCommandCombinedPipeContext_Timeout(t *testing.T) {
	ce := NewCommandExecutor()
	reader, wait, err := ce.runCommandCombinedPipeContext(context.Background(), 100*time.Millisecond, "sh", "-c", "echo started; exec sleep 5")
	if err != nil {
		t.Fatalf("runCommandCombinedPipeContext failed to start: %v", err)
	}

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
