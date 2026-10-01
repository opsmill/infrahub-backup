package tests

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestGeneralCIRunsForPullRequestsOnly(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not locate the workflow regression test")
	}

	workflowPath := filepath.Join(filepath.Dir(currentFile), "..", ".github", "workflows", "ci.yml")
	workflowBytes, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatalf("read general CI workflow: %v", err)
	}

	workflow := string(workflowBytes)
	triggerStart := strings.Index(workflow, "on:")
	triggerEnd := strings.Index(workflow, "concurrency:")
	if triggerStart < 0 || triggerEnd <= triggerStart {
		t.Fatal("could not locate the general CI trigger block")
	}
	triggers := workflow[triggerStart:triggerEnd]

	if !strings.Contains(triggers, "\n  pull_request:") {
		t.Error("general CI no longer runs for pull requests")
	}
	if strings.Contains(triggers, "\n  push:") {
		t.Error("general CI still reruns after a pull request is merged")
	}
}
