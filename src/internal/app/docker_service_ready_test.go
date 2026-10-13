package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func runningWithHealth(status string, streak int) dockerContainerState {
	st := dockerContainerState{Status: "running", Running: true}
	st.Health = &struct {
		Status        string `json:"Status"`
		FailingStreak int    `json:"FailingStreak"`
	}{Status: status, FailingStreak: streak}
	return st
}

func TestDockerReadiness(t *testing.T) {
	cases := []struct {
		name      string
		states    []dockerContainerState
		ready     bool
		needStart bool
	}{
		{"no container", nil, false, false},
		{"healthy", []dockerContainerState{runningWithHealth("healthy", 0)}, true, false},
		{"stale healthy after an outage", []dockerContainerState{runningWithHealth("healthy", 4)}, false, false},
		{"starting", []dockerContainerState{runningWithHealth("starting", 0)}, false, false},
		{"unhealthy", []dockerContainerState{runningWithHealth("unhealthy", 20)}, false, false},
		{"restarting", []dockerContainerState{{Status: "restarting"}}, false, false},
		{"exited", []dockerContainerState{{Status: "exited"}}, false, true},
		{"running without healthcheck", []dockerContainerState{{Status: "running", Running: true}}, true, false},
		{"one replica not ready", []dockerContainerState{runningWithHealth("healthy", 0), {Status: "restarting"}}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ready, needStart, _ := dockerReadiness(tc.states)
			if ready != tc.ready || needStart != tc.needStart {
				t.Fatalf("dockerReadiness = (%v, %v), want (%v, %v)", ready, needStart, tc.ready, tc.needStart)
			}
		})
	}
}

// The sequence the Community streamed load leaves behind on Docker: the
// resumed Neo4j finishes its shutdown, the container exits, its restart policy
// (or the wait) starts it again, and its healthcheck passes some time later.
// Starting infrahub-server at any point before the end of the sequence is
// what compose refuses.
func TestWaitForDockerServiceReadyWaitsThroughARestart(t *testing.T) {
	readings := [][]dockerContainerState{
		{runningWithHealth("healthy", 5)}, // stale: Neo4j was down for the load
		{{Status: "exited"}},
		{{Status: "restarting"}},
		{runningWithHealth("starting", 0)},
		{runningWithHealth("healthy", 0)},
		{runningWithHealth("healthy", 0)},
	}
	calls, starts := 0, 0
	observe := func(time.Duration) ([]dockerContainerState, error) {
		r := readings[calls]
		if calls < len(readings)-1 {
			calls++
		}
		return r, nil
	}
	start := func(time.Duration) error { starts++; return nil }

	if err := waitForDockerServiceReady("database", observe, start, time.Minute, time.Millisecond); err != nil {
		t.Fatalf("waitForDockerServiceReady = %v, want nil", err)
	}
	if calls != len(readings)-1 {
		t.Fatalf("returned after %d readings, want all %d", calls+1, len(readings))
	}
	if starts != 1 {
		t.Fatalf("start called %d times, want 1 (for the exited reading)", starts)
	}
}

func TestWaitForDockerServiceReadyNeedsConsecutiveReadings(t *testing.T) {
	readings := [][]dockerContainerState{
		{runningWithHealth("healthy", 0)},
		{{Status: "restarting"}},
		{runningWithHealth("healthy", 0)},
		{runningWithHealth("healthy", 0)},
	}
	calls := 0
	observe := func(time.Duration) ([]dockerContainerState, error) {
		r := readings[calls]
		calls++
		return r, nil
	}
	if err := waitForDockerServiceReady("database", observe, func(time.Duration) error { return nil }, time.Minute, time.Millisecond); err != nil {
		t.Fatalf("waitForDockerServiceReady = %v, want nil", err)
	}
	if calls != 4 {
		t.Fatalf("observed %d times, want 4", calls)
	}
}

func TestWaitForDockerServiceReadyTimesOutWithLastState(t *testing.T) {
	observe := func(time.Duration) ([]dockerContainerState, error) {
		return []dockerContainerState{runningWithHealth("unhealthy", 20)}, nil
	}
	err := waitForDockerServiceReady("database", observe, func(time.Duration) error { return nil }, 20*time.Millisecond, time.Millisecond)
	if err == nil {
		t.Fatal("waitForDockerServiceReady = nil, want a timeout error")
	}
	if !strings.Contains(err.Error(), "database did not become healthy") || !strings.Contains(err.Error(), "unhealthy (failing streak 20)") {
		t.Fatalf("error = %q, want the service and its last state", err)
	}
}

func TestWaitForDockerServiceReadyRetriesObserveErrors(t *testing.T) {
	calls := 0
	observe := func(time.Duration) ([]dockerContainerState, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("no such container")
		}
		return []dockerContainerState{runningWithHealth("healthy", 0)}, nil
	}
	if err := waitForDockerServiceReady("database", observe, func(time.Duration) error { return nil }, time.Minute, time.Millisecond); err != nil {
		t.Fatalf("waitForDockerServiceReady = %v, want nil", err)
	}
}

func TestComposeLifecycleError(t *testing.T) {
	if err := composeLifecycleError("whatever", nil); err != nil {
		t.Fatalf("composeLifecycleError(_, nil) = %v, want nil", err)
	}
	base := errors.New("exit status 1")
	if err := composeLifecycleError("  ", base); err != base {
		t.Fatalf("composeLifecycleError(blank, err) = %v, want err unchanged", err)
	}
	err := composeLifecycleError("dependency failed to start: container db exited (0)\n", base)
	if !errors.Is(err, base) || err.Error() != "exit status 1: dependency failed to start: container db exited (0)" {
		t.Fatalf("composeLifecycleError = %q, want the output appended to the wrapped error", err)
	}
}

// A `docker compose start` that fails must say why: the output compose
// printed is part of the error, not discarded.
func TestDockerBackendStartIncludesComposeOutput(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\necho 'dependency failed to start: container infrahub-database-1 exited (0)'\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake docker = %v", err)
	}
	t.Setenv("PATH", bin)

	d := NewDockerBackend(&Configuration{}, NewCommandExecutor())
	for name, op := range map[string]func(...string) error{"Start": d.Start, "Stop": d.Stop} {
		err := op("infrahub-server")
		if err == nil || !strings.Contains(err.Error(), "container infrahub-database-1 exited (0)") {
			t.Fatalf("%s error = %v, want it to carry the compose output", name, err)
		}
	}
}

// A docker call that hangs must not hold the wait past its bound: each call
// gets only the time left, and the error names the call that ran out.
func TestDockerBackendWaitServiceReadyBoundsHungDockerCalls(t *testing.T) {
	bin := t.TempDir()
	// Not `exec sleep`: the sleep is a child that keeps the output pipe open
	// after the killed shell, as the compose plugin does under the docker CLI.
	script := "#!/bin/sh\nsleep 30\n"
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake docker = %v", err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	d := NewDockerBackend(&Configuration{}, NewCommandExecutor())
	began := time.Now()
	err := d.WaitServiceReady("database", 300*time.Millisecond)
	if elapsed := time.Since(began); elapsed > 10*time.Second {
		t.Fatalf("WaitServiceReady returned after %s, want it bounded by its timeout", elapsed)
	}
	if err == nil || !strings.Contains(err.Error(), "docker compose ps database") || !strings.Contains(err.Error(), "timed out after") {
		t.Fatalf("error = %v, want a timeout naming docker compose ps", err)
	}
}

func TestDockerBackendStartWithinBoundsHungStart(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatalf("writing fake docker = %v", err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	d := NewDockerBackend(&Configuration{}, NewCommandExecutor())
	began := time.Now()
	err := d.startWithin(300*time.Millisecond, "database")
	if elapsed := time.Since(began); elapsed > 10*time.Second {
		t.Fatalf("startWithin returned after %s, want it bounded by its limit", elapsed)
	}
	if err == nil || !strings.Contains(err.Error(), "docker compose start database") || !strings.Contains(err.Error(), "timed out after") {
		t.Fatalf("error = %v, want a timeout naming docker compose start", err)
	}
}
