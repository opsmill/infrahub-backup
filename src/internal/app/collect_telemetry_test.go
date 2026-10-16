package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// telemetryBackend scripts the probe and the infrahubctl telemetry export
// exec and the copy-out, and records the cleanup rm, so collectTelemetry can be
// exercised without a real container.
type telemetryBackend struct {
	fakeCollectBackend
	probeErr    error
	probeOutput string
	probes      int
	execErr     error
	execOutput  string
	ranCommands [][]string
	copyContent string
	copiedSrc   string
	copiedDest  string
	rmPaths     []string
}

// ExecContext scripts the probe and the export and records the bounded rm
// cleanup. The probe and the cleanup run through cc.execDump → ExecContext (not
// the unbounded Exec), so both are separated out here rather than counted as
// export runs.
func (b *telemetryBackend) ExecContext(ctx context.Context, timeout time.Duration, service string, command []string, opts *ExecOptions) (string, error) {
	if reflect.DeepEqual(command, telemetryProbeCommand) {
		b.probes++
		return b.probeOutput, b.probeErr
	}
	if len(command) > 0 && command[0] == "rm" {
		b.rmPaths = append(b.rmPaths, command[len(command)-1])
		return "", nil
	}
	b.ranCommands = append(b.ranCommands, command)
	return b.execOutput, b.execErr
}

func (b *telemetryBackend) CopyFromContext(ctx context.Context, timeout time.Duration, service, src, dest string) error {
	b.copiedSrc = src
	b.copiedDest = dest
	return os.WriteFile(dest, []byte(b.copyContent), 0644)
}

func newTelemetryBackend() *telemetryBackend {
	b := &telemetryBackend{fakeCollectBackend: *newFakeCollectBackend()}
	b.replicas["task-worker"] = []Replica{{Service: "task-worker", Container: "task-worker-1"}}
	return b
}

var (
	_ contextExecer  = (*telemetryBackend)(nil)
	_ contextCopier  = (*telemetryBackend)(nil)
	_ collectBackend = (*telemetryBackend)(nil)
)

func TestTelemetryStartDate(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 30, 0, 0, time.UTC)
	tests := []struct {
		name string
		days int
		want string
	}{
		{name: "default window", days: 30, want: "2026-06-10"},
		{name: "custom window", days: 7, want: "2026-07-03"},
		{name: "zero falls back to 30", days: 0, want: "2026-06-10"},
		{name: "negative falls back to 30", days: -5, want: "2026-06-10"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := telemetryStartDate(now, tt.days); got != tt.want {
				t.Errorf("telemetryStartDate(%v, %d) = %q, want %q", now, tt.days, got, tt.want)
			}
		})
	}
}

func TestTelemetryExportCommand(t *testing.T) {
	cmd := telemetryExportCommand("2026-06-10")
	want := []string{
		"infrahubctl", "telemetry", "export",
		"--output", telemetryExportContainerPath,
		"--start-date", "2026-06-10",
	}
	if !reflect.DeepEqual(cmd, want) {
		t.Errorf("telemetryExportCommand = %v, want %v", cmd, want)
	}
}

func newTelemetryCC(t *testing.T, backend EnvironmentBackend, opts CollectOptions) *collectContext {
	t.Helper()
	return &collectContext{ctx: context.Background(), backend: backend, bundleDir: t.TempDir(), opts: opts}
}

func TestCollectTelemetry_Success(t *testing.T) {
	backend := newTelemetryBackend()
	backend.copyContent = `{"snapshots":[{"version":"1.5.2"}]}`
	cc := newTelemetryCC(t, backend, CollectOptions{TelemetryDays: 7})

	if err := collectTelemetry(cc); err != nil {
		t.Fatalf("collectTelemetry failed: %v", err)
	}

	// The probe ran once, then the export ran with the fixed structure and an
	// ISO-date --start-date.
	if backend.probes != 1 {
		t.Errorf("probe ran %d times, want 1", backend.probes)
	}
	if len(backend.ranCommands) != 1 {
		t.Fatalf("ran %d commands %v, want exactly the export", len(backend.ranCommands), backend.ranCommands)
	}
	got := backend.ranCommands[0]
	wantPrefix := []string{"infrahubctl", "telemetry", "export", "--output", telemetryExportContainerPath, "--start-date"}
	if len(got) != len(wantPrefix)+1 {
		t.Fatalf("export command = %v, want the fixed prefix plus a start-date value", got)
	}
	for i, arg := range wantPrefix {
		if got[i] != arg {
			t.Errorf("export command[%d] = %q, want %q", i, got[i], arg)
		}
	}
	if _, err := time.Parse(telemetryStartDateLayout, got[len(got)-1]); err != nil {
		t.Errorf("--start-date %q is not an ISO 8601 date: %v", got[len(got)-1], err)
	}

	// The export was copied out of the in-container path into the bundle.
	if backend.copiedSrc != telemetryExportContainerPath {
		t.Errorf("copied from %q, want %q", backend.copiedSrc, telemetryExportContainerPath)
	}
	exportPath := filepath.Join(cc.bundleDir, telemetryBundleDir, telemetryExportFilename)
	data, err := os.ReadFile(exportPath)
	if err != nil {
		t.Fatalf("telemetry export missing from bundle: %v", err)
	}
	if string(data) != backend.copyContent {
		t.Errorf("export content = %q, want %q", data, backend.copyContent)
	}

	// The in-container export file is cleaned up.
	if !reflect.DeepEqual(backend.rmPaths, []string{telemetryExportContainerPath}) {
		t.Errorf("cleanup rm paths = %v, want [%q]", backend.rmPaths, telemetryExportContainerPath)
	}
}

func TestCollectTelemetry_ExportFailureSkips(t *testing.T) {
	tests := []struct {
		name       string
		execOutput string
		execErr    error
		wantReason string
		wantErrTxt string
	}{
		{
			name:       "no snapshots degrades to a friendly skip",
			execOutput: "No telemetry snapshots found.",
			execErr:    fmt.Errorf("exit status 2"),
			wantReason: "no telemetry snapshots found in the requested window",
			wantErrTxt: "No telemetry snapshots found.",
		},
		{
			name:       "other CLI error keeps the reported line",
			execOutput: "Usage: infrahubctl telemetry [OPTIONS]\nError: No such command 'export'.",
			execErr:    fmt.Errorf("exit status 2"),
			wantReason: "infrahubctl telemetry export could not be run: Error: No such command 'export'.",
			wantErrTxt: "No such command",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := newTelemetryBackend()
			backend.execOutput = tt.execOutput
			backend.execErr = tt.execErr
			cc := newTelemetryCC(t, backend, CollectOptions{TelemetryDays: 30})

			err := collectTelemetry(cc)

			// A failing export degrades to skipped, not failed.
			var skip *collectSkipError
			if !errors.As(err, &skip) {
				t.Fatalf("collectTelemetry returned %v, want a *collectSkipError", err)
			}
			if skip.reason != tt.wantReason {
				t.Errorf("skip reason = %q, want %q", skip.reason, tt.wantReason)
			}

			// The CLI's combined output is preserved; no export.json is staged.
			errData, readErr := os.ReadFile(filepath.Join(cc.bundleDir, telemetryBundleDir, telemetryErrorFilename))
			if readErr != nil {
				t.Fatalf("telemetry output missing from bundle: %v", readErr)
			}
			if !strings.Contains(string(errData), tt.wantErrTxt) {
				t.Errorf("preserved output = %q, want it to contain %q", errData, tt.wantErrTxt)
			}
			if _, statErr := os.Stat(filepath.Join(cc.bundleDir, telemetryBundleDir, telemetryExportFilename)); !os.IsNotExist(statErr) {
				t.Errorf("a telemetry-export.json was staged despite the export failing (stat err: %v)", statErr)
			}

			// Cleanup still runs on the failure path, through the bounded path.
			if !reflect.DeepEqual(backend.rmPaths, []string{telemetryExportContainerPath}) {
				t.Errorf("cleanup rm paths = %v, want [%q]", backend.rmPaths, telemetryExportContainerPath)
			}
		})
	}
}

// telemetryTimeoutBackend times out the export exec (the probe succeeds),
// proving an export timeout also degrades to skipped with the timeout surfaced
// in the reason.
type telemetryTimeoutBackend struct {
	telemetryBackend
	timeout time.Duration
}

func (b *telemetryTimeoutBackend) ExecContext(ctx context.Context, timeout time.Duration, service string, command []string, opts *ExecOptions) (string, error) {
	if reflect.DeepEqual(command, telemetryProbeCommand) {
		return "", nil
	}
	return "", &timeoutError{timeout: b.timeout}
}

var _ contextExecer = (*telemetryTimeoutBackend)(nil)

func TestCollectTelemetry_TimeoutSkips(t *testing.T) {
	iops := NewInfrahubOps()
	backend := &telemetryTimeoutBackend{
		telemetryBackend: *newTelemetryBackend(),
		timeout:          collectTransferTimeout,
	}
	outputDir := filepath.Join(t.TempDir(), "bundles")
	opts := CollectOptions{OutputDir: outputDir, LogLines: 100000, TelemetryDays: 30}

	if err := iops.runCollectPlan(backend, opts, []collector{telemetryCollector()}); err != nil {
		t.Fatalf("runCollectPlan failed: %v", err)
	}

	manifest, _ := extractBundleManifest(t, findArchive(t, outputDir))
	if len(manifest.Collectors) != 1 {
		t.Fatalf("manifest has %d entries %+v, want 1", len(manifest.Collectors), manifest.Collectors)
	}
	got := manifest.Collectors[0]
	if got.Name != "telemetry" || got.Status != collectorStatusSkipped {
		t.Fatalf("collector result = %+v, want telemetry skipped", got)
	}
	if !strings.Contains(got.Reason, "timed out after 300s") {
		t.Errorf("skip reason = %q, want it to surface the timeout", got.Reason)
	}
}

// TestCollectTelemetry_ProbeFailureFails covers a container the tool cannot run
// commands in: an exec the API server refuses, or a probe that times out. Unlike
// a failing export, that is recorded as failed, with what kubectl reported in
// the reason, and the export never runs.
func TestCollectTelemetry_ProbeFailureFails(t *testing.T) {
	forbidden := `Error from server (Forbidden): pods "task-worker-0" is forbidden: User "system:serviceaccount:infrahub:infrahub-collect" cannot create resource "pods/exec" in API group "" in the namespace "infrahub"`
	tests := []struct {
		name        string
		probeOutput string
		probeErr    error
		wantReason  string
	}{
		{
			name:        "refused exec keeps the API server's message",
			probeOutput: forbidden,
			probeErr:    fmt.Errorf("exit status 1"),
			wantReason:  "cannot run commands in the task-worker container: exit status 1: " + forbidden,
		},
		{
			name:       "probe timeout collapses to the bare timeout reason",
			probeErr:   &timeoutError{timeout: collectExecTimeout},
			wantReason: "timed out after 60s",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			iops := NewInfrahubOps()
			backend := newTelemetryBackend()
			backend.probeOutput = tt.probeOutput
			backend.probeErr = tt.probeErr
			outputDir := filepath.Join(t.TempDir(), "bundles")
			opts := CollectOptions{OutputDir: outputDir, LogLines: 100000, TelemetryDays: 30}

			if err := iops.runCollectPlan(backend, opts, []collector{telemetryCollector()}); err != nil {
				t.Fatalf("runCollectPlan failed: %v", err)
			}

			manifest, extracted := extractBundleManifest(t, findArchive(t, outputDir))
			if len(manifest.Collectors) != 1 {
				t.Fatalf("manifest has %d entries %+v, want 1", len(manifest.Collectors), manifest.Collectors)
			}
			got := manifest.Collectors[0]
			if got.Name != "telemetry" || got.Status != collectorStatusFailed {
				t.Fatalf("collector result = %+v, want telemetry failed", got)
			}
			if got.Reason != tt.wantReason {
				t.Errorf("failure reason = %q, want %q", got.Reason, tt.wantReason)
			}

			// Nothing ran past the probe: no export, no cleanup, no staged export.
			if len(backend.ranCommands) != 0 {
				t.Errorf("export ran despite the failed probe: %v", backend.ranCommands)
			}
			if len(backend.rmPaths) != 0 {
				t.Errorf("cleanup ran despite nothing being written: %v", backend.rmPaths)
			}
			exportPath := filepath.Join(extracted, "bundle", telemetryBundleDir, telemetryExportFilename)
			if _, statErr := os.Stat(exportPath); !os.IsNotExist(statErr) {
				t.Errorf("bundle contains %s despite the failed probe (stat err: %v)", telemetryExportFilename, statErr)
			}
		})
	}
}
