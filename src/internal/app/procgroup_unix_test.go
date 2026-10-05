//go:build !windows

package app

import (
	"errors"
	"os"
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
	// The killed child is reaped by init once its shell is gone; allow for that.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("child %d still running after startWithin timed out", pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
