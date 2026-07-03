package app

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// bareBackend implements EnvironmentBackend with inert stubs. It deliberately
// does NOT implement the collect primitives, standing in for a backend that
// has not been extended yet.
type bareBackend struct{ name string }

func (b *bareBackend) Name() string  { return b.name }
func (b *bareBackend) Detect() error { return nil }
func (b *bareBackend) Info() string  { return "fake environment" }
func (b *bareBackend) Exec(service string, command []string, opts *ExecOptions) (string, error) {
	return "", nil
}
func (b *bareBackend) ExecStream(service string, command []string, opts *ExecOptions) (string, error) {
	return "", nil
}
func (b *bareBackend) ExecStreamPipe(service string, command []string, opts *ExecOptions) (io.ReadCloser, func() error, error) {
	return io.NopCloser(strings.NewReader("")), func() error { return nil }, nil
}
func (b *bareBackend) ExecWritePipe(service string, command []string, opts *ExecOptions, stdin io.Reader) (func() error, error) {
	return func() error { return nil }, nil
}
func (b *bareBackend) CopyTo(service, src, dest string) error   { return nil }
func (b *bareBackend) CopyFrom(service, src, dest string) error { return nil }
func (b *bareBackend) Start(services ...string) error           { return nil }
func (b *bareBackend) Stop(services ...string) error            { return nil }
func (b *bareBackend) IsRunning(service string) (bool, error)   { return true, nil }

// fakeCollectBackend satisfies the full collectBackend seam for unit tests.
type fakeCollectBackend struct {
	bareBackend
	replicas map[string][]Replica
	logs     map[string]string
	metrics  string
}

func (f *fakeCollectBackend) ServiceReplicas(service string) ([]Replica, error) {
	return f.replicas[service], nil
}

func (f *fakeCollectBackend) ReplicaLogs(replica Replica, tailLines int, previous bool) (io.ReadCloser, func() error, error) {
	return io.NopCloser(strings.NewReader(f.logs[replica.Container])), func() error { return nil }, nil
}

func (f *fakeCollectBackend) Metrics() (string, error) {
	return f.metrics, nil
}

var (
	_ EnvironmentBackend = (*bareBackend)(nil)
	_ collectBackend     = (*fakeCollectBackend)(nil)
)

func newFakeCollectBackend() *fakeCollectBackend {
	return &fakeCollectBackend{
		bareBackend: bareBackend{name: "docker"},
		replicas: map[string][]Replica{
			"infrahub-server": {
				{Service: "infrahub-server", Container: "infrahub-server-1"},
			},
		},
		logs:    map[string]string{"infrahub-server-1": "log line\n"},
		metrics: "CONTAINER CPU% MEM%\ninfrahub-server-1 1% 2%\n",
	}
}

// extractBundleManifest extracts the archive and parses bundle/bundle_information.json.
func extractBundleManifest(t *testing.T, archivePath string) (*BundleManifest, string) {
	t.Helper()

	destDir := t.TempDir()
	if err := extractTarball(archivePath, destDir); err != nil {
		t.Fatalf("failed to extract archive %s: %v", archivePath, err)
	}

	data, err := os.ReadFile(filepath.Join(destDir, "bundle", "bundle_information.json"))
	if err != nil {
		t.Fatalf("bundle_information.json missing from archive: %v", err)
	}

	var manifest BundleManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("manifest is not valid JSON: %v", err)
	}
	return &manifest, destDir
}

// assertStagingRemoved verifies no infrahub_collect_* staging dir remains.
func assertStagingRemoved(t *testing.T, outputDir string) {
	t.Helper()
	leftovers, err := filepath.Glob(filepath.Join(outputDir, "infrahub_collect_*"))
	if err != nil {
		t.Fatalf("glob failed: %v", err)
	}
	if len(leftovers) != 0 {
		t.Errorf("staging directories left behind: %v", leftovers)
	}
}

func findArchive(t *testing.T, outputDir string) string {
	t.Helper()
	archives, err := filepath.Glob(filepath.Join(outputDir, "support_bundle_*.tar.gz"))
	if err != nil {
		t.Fatalf("glob failed: %v", err)
	}
	if len(archives) != 1 {
		t.Fatalf("found %d archives in %s, want exactly 1: %v", len(archives), outputDir, archives)
	}
	return archives[0]
}

func TestRunCollectPlan_OutcomeRecording(t *testing.T) {
	iops := NewInfrahubOps()
	backend := newFakeCollectBackend()
	outputDir := filepath.Join(t.TempDir(), "bundles")
	opts := CollectOptions{OutputDir: outputDir, LogLines: 500}

	epsilonRan := false
	plan := []collector{
		{
			name: "metrics",
			run: func(cc *collectContext) error {
				cb, err := cc.collect()
				if err != nil {
					return err
				}
				metrics, err := cb.Metrics()
				if err != nil {
					return err
				}
				return os.WriteFile(filepath.Join(cc.bundleDir, "metrics.txt"), []byte(metrics), 0644)
			},
		},
		{
			name: "cache-status",
			run: func(cc *collectContext) error {
				return fmt.Errorf("cache status dump: %w", fmt.Errorf("container not running"))
			},
		},
		{
			name: "benchmark",
			skip: func(cc *collectContext) (bool, string) {
				return !cc.opts.Benchmark, "not requested"
			},
			run: func(cc *collectContext) error { return nil },
		},
		{
			name: "message-queue-status",
			run: func(cc *collectContext) error {
				_, err := cc.iops.executor.runCommandContext(cc.ctx, 100*time.Millisecond, "sleep", "5")
				if err != nil {
					return fmt.Errorf("rabbitmqctl status: %w", err)
				}
				return nil
			},
		},
		{
			name: "logs/infrahub-server",
			run: func(cc *collectContext) error {
				epsilonRan = true
				cb, err := cc.collect()
				if err != nil {
					return err
				}
				replicas, err := cb.ServiceReplicas("infrahub-server")
				if err != nil {
					return err
				}
				for _, replica := range replicas {
					reader, wait, err := cb.ReplicaLogs(replica, cc.opts.LogLines, false)
					if err != nil {
						return err
					}
					content, err := io.ReadAll(reader)
					if err != nil {
						return err
					}
					if err := wait(); err != nil {
						return err
					}
					logPath := filepath.Join(cc.bundleDir, replica.Container+".log")
					if err := os.WriteFile(logPath, content, 0644); err != nil {
						return err
					}
				}
				return nil
			},
		},
	}

	if err := iops.runCollectPlan(backend, opts, plan); err != nil {
		t.Fatalf("runCollectPlan failed: %v", err)
	}

	if !epsilonRan {
		t.Error("collector after failures did not run; run must continue past failures (FR-009)")
	}

	archivePath := findArchive(t, outputDir)
	manifest, destDir := extractBundleManifest(t, archivePath)

	// The manifest accounts for every planned collector, in order (SC-005).
	if len(manifest.Collectors) != len(plan) {
		t.Fatalf("manifest has %d collector entries, want %d (one per planned collector)", len(manifest.Collectors), len(plan))
	}

	wantResults := []CollectorResult{
		{Name: "metrics", Status: collectorStatusSuccess},
		{Name: "cache-status", Status: collectorStatusFailed, Reason: "cache status dump: container not running"},
		{Name: "benchmark", Status: collectorStatusSkipped, Reason: "not requested"},
		{Name: "message-queue-status", Status: collectorStatusFailed, Reason: "timed out after 100ms"},
		{Name: "logs/infrahub-server", Status: collectorStatusSuccess},
	}
	for i, want := range wantResults {
		if manifest.Collectors[i] != want {
			t.Errorf("collectors[%d] = %+v, want %+v", i, manifest.Collectors[i], want)
		}
	}

	if manifest.Environment != "docker" {
		t.Errorf("manifest environment = %q, want %q", manifest.Environment, "docker")
	}
	if manifest.LogLines != 500 {
		t.Errorf("manifest log_lines = %d, want 500", manifest.LogLines)
	}
	if !strings.Contains(archivePath, manifest.CollectID) {
		t.Errorf("archive filename %q does not embed collect_id %q", archivePath, manifest.CollectID)
	}

	// Files written by successful collectors are inside the archive under bundle/.
	metricsData, err := os.ReadFile(filepath.Join(destDir, "bundle", "metrics.txt"))
	if err != nil {
		t.Errorf("metrics.txt missing from archive: %v", err)
	} else if string(metricsData) != backend.metrics {
		t.Errorf("metrics.txt content = %q, want %q", metricsData, backend.metrics)
	}
	logData, err := os.ReadFile(filepath.Join(destDir, "bundle", "infrahub-server-1.log"))
	if err != nil {
		t.Errorf("infrahub-server-1.log missing from archive: %v", err)
	} else if string(logData) != "log line\n" {
		t.Errorf("log content = %q, want %q", logData, "log line\n")
	}

	assertStagingRemoved(t, outputDir)
}

func TestCollectBundle_EmptyPlanProducesValidBundle(t *testing.T) {
	iops := NewInfrahubOps()
	iops.backend = newFakeCollectBackend()
	outputDir := filepath.Join(t.TempDir(), "bundles")
	opts := CollectOptions{OutputDir: outputDir, LogLines: 100000}

	if err := iops.CollectBundle(opts); err != nil {
		t.Fatalf("CollectBundle failed: %v", err)
	}

	archivePath := findArchive(t, outputDir)
	manifest, _ := extractBundleManifest(t, archivePath)

	if manifest.ManifestVersion != manifestVersion {
		t.Errorf("manifest_version = %d, want %d", manifest.ManifestVersion, manifestVersion)
	}
	if manifest.Collectors == nil {
		t.Error("collectors is null, want an empty JSON array")
	}
	if len(manifest.Collectors) != 0 {
		t.Errorf("collectors = %+v, want empty until the run plan is registered (T025)", manifest.Collectors)
	}
	if manifest.Environment != "docker" {
		t.Errorf("environment = %q, want %q", manifest.Environment, "docker")
	}

	assertStagingRemoved(t, outputDir)
}

func TestRunCollectPlan_UnwritableOutputDir(t *testing.T) {
	iops := NewInfrahubOps()
	backend := newFakeCollectBackend()

	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("file, not dir"), 0644); err != nil {
		t.Fatal(err)
	}

	opts := CollectOptions{OutputDir: filepath.Join(blocker, "bundles"), LogLines: 100000}
	err := iops.runCollectPlan(backend, opts, nil)
	if err == nil {
		t.Fatal("runCollectPlan with unwritable output dir succeeded, want hard error")
	}
	if !strings.Contains(err.Error(), "output directory") {
		t.Errorf("error %q does not name the output directory", err)
	}
}

func TestRunCollectPlan_StagingRemovedOnFailure(t *testing.T) {
	iops := NewInfrahubOps()
	backend := newFakeCollectBackend()
	outputDir := filepath.Join(t.TempDir(), "bundles")
	opts := CollectOptions{OutputDir: outputDir, LogLines: 100000}

	// Sabotage the staging bundle dir so manifest finalization fails,
	// exercising the failure-path cleanup.
	plan := []collector{
		{
			name: "saboteur",
			run: func(cc *collectContext) error {
				return os.RemoveAll(cc.bundleDir)
			},
		},
	}

	err := iops.runCollectPlan(backend, opts, plan)
	if err == nil {
		t.Fatal("runCollectPlan succeeded, want manifest write failure")
	}

	assertStagingRemoved(t, outputDir)

	archives, globErr := filepath.Glob(filepath.Join(outputDir, "support_bundle_*.tar.gz"))
	if globErr != nil {
		t.Fatal(globErr)
	}
	if len(archives) != 0 {
		t.Errorf("failed run left archives behind: %v", archives)
	}
}

func TestRunCollectPlan_InterruptCleansStaging(t *testing.T) {
	iops := NewInfrahubOps()
	backend := newFakeCollectBackend()
	outputDir := filepath.Join(t.TempDir(), "bundles")
	opts := CollectOptions{OutputDir: outputDir, LogLines: 100000}

	plan := []collector{
		{
			name: "interrupter",
			run: func(cc *collectContext) error {
				process, err := os.FindProcess(os.Getpid())
				if err != nil {
					return err
				}
				if err := process.Signal(os.Interrupt); err != nil {
					return err
				}
				select {
				case <-cc.ctx.Done():
					return fmt.Errorf("stopped by interrupt")
				case <-time.After(5 * time.Second):
					return fmt.Errorf("interrupt signal was never delivered")
				}
			},
		},
		{
			name: "never-reached",
			run: func(cc *collectContext) error {
				t.Error("collector ran after interrupt, want run aborted")
				return nil
			},
		},
	}

	err := iops.runCollectPlan(backend, opts, plan)
	if err == nil {
		t.Fatal("runCollectPlan succeeded, want interrupt error")
	}
	if !strings.Contains(err.Error(), "interrupted") {
		t.Errorf("error = %q, want mention of interruption", err)
	}

	assertStagingRemoved(t, outputDir)

	archives, globErr := filepath.Glob(filepath.Join(outputDir, "support_bundle_*.tar.gz"))
	if globErr != nil {
		t.Fatal(globErr)
	}
	if len(archives) != 0 {
		t.Errorf("interrupted run left an archive with a final name: %v", archives)
	}
}

func TestCollectContext_BackendSeam(t *testing.T) {
	t.Run("bare backend is rejected with a clear error", func(t *testing.T) {
		cc := &collectContext{backend: &bareBackend{name: "docker"}}
		_, err := cc.collect()
		if err == nil {
			t.Fatal("collect() on a backend without collect primitives succeeded, want error")
		}
		if !strings.Contains(err.Error(), "does not support bundle collection") {
			t.Errorf("error = %q, want a clear unsupported-backend message", err)
		}
	})

	t.Run("collect-capable backend resolves", func(t *testing.T) {
		cc := &collectContext{backend: newFakeCollectBackend()}
		cb, err := cc.collect()
		if err != nil {
			t.Fatalf("collect() failed: %v", err)
		}
		replicas, err := cb.ServiceReplicas("infrahub-server")
		if err != nil {
			t.Fatalf("ServiceReplicas failed: %v", err)
		}
		if len(replicas) != 1 || replicas[0].Container != "infrahub-server-1" {
			t.Errorf("replicas = %+v, want the fake's single infrahub-server replica", replicas)
		}
	})
}
