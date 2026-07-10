package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// telemetryBackend scripts the infrahubctl telemetry export exec and the
// copy-out, and records the cleanup rm, so collectTelemetry can be exercised
// without a real container.
type telemetryBackend struct {
	fakeCollectBackend
	execErr     error
	execOutput  string
	ranCommands [][]string
	copyContent string
	copiedSrc   string
	copiedDest  string
	rmPaths     []string
}

func (b *telemetryBackend) ExecContext(ctx context.Context, timeout time.Duration, service string, command []string, opts *ExecOptions) (string, error) {
	b.ranCommands = append(b.ranCommands, command)
	return b.execOutput, b.execErr
}

func (b *telemetryBackend) CopyFromContext(ctx context.Context, timeout time.Duration, service, src, dest string) error {
	b.copiedSrc = src
	b.copiedDest = dest
	return os.WriteFile(dest, []byte(b.copyContent), 0644)
}

func (b *telemetryBackend) Exec(service string, command []string, opts *ExecOptions) (string, error) {
	if len(command) > 0 && command[0] == "rm" {
		b.rmPaths = append(b.rmPaths, command[len(command)-1])
	}
	return "", nil
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

	// The export ran with the fixed structure and an ISO-date --start-date.
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

func TestCollectTelemetry_ExportFailure(t *testing.T) {
	backend := newTelemetryBackend()
	backend.execErr = fmt.Errorf("exit status 2")
	backend.execOutput = "Usage: infrahubctl telemetry [OPTIONS]\nError: No such command 'export'."
	cc := newTelemetryCC(t, backend, CollectOptions{TelemetryDays: 30})

	err := collectTelemetry(cc)
	if err == nil {
		t.Fatal("collectTelemetry succeeded, want a failure when the export command errors")
	}
	if !strings.Contains(err.Error(), "infrahubctl telemetry export failed") {
		t.Errorf("error = %q, want it to name the failed export", err)
	}

	// The CLI's combined output is preserved for support; no export.json is
	// staged when the export fails.
	errPath := filepath.Join(cc.bundleDir, telemetryBundleDir, telemetryErrorFilename)
	errData, readErr := os.ReadFile(errPath)
	if readErr != nil {
		t.Fatalf("telemetry error output missing from bundle: %v", readErr)
	}
	if !strings.Contains(string(errData), "No such command") {
		t.Errorf("error output = %q, want it to preserve the CLI message", errData)
	}
	if _, statErr := os.Stat(filepath.Join(cc.bundleDir, telemetryBundleDir, telemetryExportFilename)); !os.IsNotExist(statErr) {
		t.Errorf("a telemetry-export.json was staged despite the export failing (stat err: %v)", statErr)
	}

	// Cleanup still runs on the failure path.
	if !reflect.DeepEqual(backend.rmPaths, []string{telemetryExportContainerPath}) {
		t.Errorf("cleanup rm paths = %v, want [%q]", backend.rmPaths, telemetryExportContainerPath)
	}
}

// telemetryTimeoutBackend times out the export exec, proving the command
// timeout survives collectTelemetry's %w-wrap and reaches the manifest as the
// bare contract reason (FIX-4).
type telemetryTimeoutBackend struct {
	telemetryBackend
	timeout time.Duration
}

func (b *telemetryTimeoutBackend) ExecContext(ctx context.Context, timeout time.Duration, service string, command []string, opts *ExecOptions) (string, error) {
	return "", &timeoutError{timeout: b.timeout}
}

var _ contextExecer = (*telemetryTimeoutBackend)(nil)

func TestCollectTelemetry_TimeoutReasonSurvivesAggregation(t *testing.T) {
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
	want := CollectorResult{Name: "telemetry", Status: collectorStatusFailed, Reason: "timed out after 300s"}
	if manifest.Collectors[0] != want {
		t.Errorf("collector result = %+v, want %+v (bare timeout reason, FIX-4)", manifest.Collectors[0], want)
	}
}
