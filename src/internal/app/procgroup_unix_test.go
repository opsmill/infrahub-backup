//go:build !windows

package app

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A timed-out `docker compose start` must not leave the compose plugin
// running: the child the docker CLI spawned is killed with it.
func TestDockerBackendStartWithinKillsChildrenOnTimeout(t *testing.T) {
	bin := t.TempDir()
	marker := filepath.Join(bin, "child.pid")
	script := "#!/bin/sh\nsleep 30 &\necho $! > " + marker + "\nwait\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake docker = %v", err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	d := NewDockerBackend(&Configuration{}, NewCommandExecutor())
	err := d.startWithin(500*time.Millisecond, "database")
	if err == nil || !strings.Contains(err.Error(), "timed out after") {
		t.Fatalf("startWithin error = %v, want a timeout", err)
	}

	raw, readErr := os.ReadFile(marker)
	if readErr != nil {
		t.Fatalf("reading child pid = %v", readErr)
	}
	pid, convErr := strconv.Atoi(strings.TrimSpace(string(raw)))
	if convErr != nil {
		t.Fatalf("parsing child pid %q = %v", raw, convErr)
	}
	// The killed child is reaped by whichever process adopts it, and until then it
	// is a zombie that kill(pid, 0) still finds; what matters is that it no longer runs.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if !processExecuting(pid) {
			return
		}
		if time.Now().After(deadline) {
			syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("child %d still running after startWithin timed out", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// processExecuting reports whether pid is a process that is still running: gone
// (ESRCH) and zombie (killed, not yet reaped) both count as not running.
func processExecuting(pid int) bool {
	if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		return false
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false // ps exits non-zero once the pid is gone
	}
	state := strings.TrimSpace(string(out))

	return state != "" && !strings.HasPrefix(state, "Z")
}
