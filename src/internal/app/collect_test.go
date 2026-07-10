package app

import (
	"encoding/json"
	"errors"
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
// execFn, when set, overrides Exec so tests can script per-command outputs.
type fakeCollectBackend struct {
	bareBackend
	replicas map[string][]Replica
	logs     map[string]string
	metrics  string
	execFn   func(service string, command []string, opts *ExecOptions) (string, error)
}

func (f *fakeCollectBackend) Exec(service string, command []string, opts *ExecOptions) (string, error) {
	if f.execFn != nil {
		return f.execFn(service, command, opts)
	}
	return f.bareBackend.Exec(service, command, opts)
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

// helmDescriberBackend is a backend that reports Helm chart provenance,
// standing in for the Kubernetes backend in populateHelmRelease tests.
type helmDescriberBackend struct {
	bareBackend
	release *HelmRelease
	err     error
}

func (h *helmDescriberBackend) HelmRelease() (*HelmRelease, error) {
	return h.release, h.err
}

var _ deploymentDescriber = (*helmDescriberBackend)(nil)

func TestPopulateHelmRelease(t *testing.T) {
	t.Run("backend without the capability leaves helm unset", func(t *testing.T) {
		manifest := newBundleManifest("20260703_101530", "docker", 100)
		populateHelmRelease(&bareBackend{name: "docker"}, manifest)
		if manifest.Helm != nil {
			t.Errorf("Helm = %+v, want nil on a non-Helm backend", manifest.Helm)
		}
	})

	t.Run("a detected release is recorded on the manifest", func(t *testing.T) {
		manifest := newBundleManifest("20260703_101530", "kubernetes", 100)
		want := &HelmRelease{ReleaseName: "infrahub", Chart: "infrahub", ChartVersion: "1.2.3"}
		populateHelmRelease(&helmDescriberBackend{
			bareBackend: bareBackend{name: "kubernetes"},
			release:     want,
		}, manifest)
		if manifest.Helm == nil || *manifest.Helm != *want {
			t.Errorf("Helm = %+v, want %+v", manifest.Helm, want)
		}
	})

	t.Run("a detection error leaves helm unset without failing the run", func(t *testing.T) {
		manifest := newBundleManifest("20260703_101530", "kubernetes", 100)
		populateHelmRelease(&helmDescriberBackend{
			bareBackend: bareBackend{name: "kubernetes"},
			err:         errors.New("forbidden"),
		}, manifest)
		if manifest.Helm != nil {
			t.Errorf("Helm = %+v, want nil when detection errored", manifest.Helm)
		}
	})

	t.Run("no Helm metadata leaves helm unset", func(t *testing.T) {
		manifest := newBundleManifest("20260703_101530", "kubernetes", 100)
		populateHelmRelease(&helmDescriberBackend{
			bareBackend: bareBackend{name: "kubernetes"},
			release:     nil,
		}, manifest)
		if manifest.Helm != nil {
			t.Errorf("Helm = %+v, want nil when the install is not Helm-managed", manifest.Helm)
		}
	})
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

// TestCollectBundle_RegisteredPlan runs the full registered plan (T025)
// against the fake backend: only infrahub-server has replicas, so its logs,
// server info, and metrics are collected while every other service-bound
// collector is skipped as not deployed.
func TestCollectBundle_RegisteredPlan(t *testing.T) {
	iops := NewInfrahubOps()
	backend := newFakeCollectBackend()
	backend.execFn = func(service string, command []string, opts *ExecOptions) (string, error) {
		joined := strings.Join(command, " ")
		switch {
		case strings.Contains(joined, "infrahub.__version__"):
			return "1.5.2", nil
		case strings.Contains(joined, "INFRAHUB_INTERNAL_ADDRESS"):
			return "http://infrahub-server:8000", nil
		case joined == "pip list":
			return "infrahub 1.5.2", nil
		case joined == "env":
			return "INFRAHUB_API_TOKEN=secret123\nINFRAHUB_HOST=localhost", nil
		case strings.HasSuffix(joined, "/api/config"):
			return `{"security":{"secret_key":"abc123"}}`, nil
		case strings.HasSuffix(joined, "/api/info"):
			return `{"version":"1.5.2"}`, nil
		case strings.HasSuffix(joined, "/api/schema"):
			return `{"nodes":[]}`, nil
		default:
			return "", nil
		}
	}
	iops.backend = backend
	outputDir := filepath.Join(t.TempDir(), "bundles")
	opts := CollectOptions{OutputDir: outputDir, LogLines: 100000}

	if err := iops.CollectBundle(opts); err != nil {
		t.Fatalf("CollectBundle failed: %v", err)
	}

	archivePath := findArchive(t, outputDir)
	manifest, destDir := extractBundleManifest(t, archivePath)

	if manifest.ManifestVersion != manifestVersion {
		t.Errorf("manifest_version = %d, want %d", manifest.ManifestVersion, manifestVersion)
	}
	if manifest.Environment != "docker" {
		t.Errorf("environment = %q, want %q", manifest.Environment, "docker")
	}
	if manifest.InfrahubVersion != "1.5.2" {
		t.Errorf("infrahub_version = %q, want %q (populated by server-info)", manifest.InfrahubVersion, "1.5.2")
	}
	if manifest.LogLines != 100000 {
		t.Errorf("log_lines = %d, want 100000", manifest.LogLines)
	}

	notDeployed := "service not deployed"
	wantResults := []CollectorResult{
		{Name: "logs/infrahub-server", Status: collectorStatusSuccess},
		{Name: "logs/task-worker", Status: collectorStatusSkipped, Reason: notDeployed},
		{Name: "logs/database", Status: collectorStatusSkipped, Reason: notDeployed},
		{Name: "logs/message-queue", Status: collectorStatusSkipped, Reason: notDeployed},
		{Name: "logs/cache", Status: collectorStatusSkipped, Reason: notDeployed},
		{Name: "logs/task-manager", Status: collectorStatusSkipped, Reason: notDeployed},
		{Name: "logs/task-manager-db", Status: collectorStatusSkipped, Reason: notDeployed},
		{Name: "logs/task-manager-background-svc", Status: collectorStatusSkipped, Reason: notDeployed},
		{Name: "database-logs", Status: collectorStatusSkipped, Reason: notDeployed},
		{Name: "message-queue-status", Status: collectorStatusSkipped, Reason: notDeployed},
		{Name: "cache-status", Status: collectorStatusSkipped, Reason: notDeployed},
		{Name: "task-worker-state", Status: collectorStatusSkipped, Reason: notDeployed},
		{Name: "task-manager-state", Status: collectorStatusSkipped, Reason: notDeployed},
		{Name: "server-info", Status: collectorStatusSuccess},
		{Name: "metrics", Status: collectorStatusSuccess},
		{Name: "benchmark", Status: collectorStatusSkipped, Reason: "not requested"},
		{Name: "backup", Status: collectorStatusSkipped, Reason: "not requested"},
	}
	if len(manifest.Collectors) != len(wantResults) {
		t.Fatalf("manifest has %d collector entries %+v, want %d", len(manifest.Collectors), manifest.Collectors, len(wantResults))
	}
	for i, want := range wantResults {
		if manifest.Collectors[i] != want {
			t.Errorf("collectors[%d] = %+v, want %+v", i, manifest.Collectors[i], want)
		}
	}

	// Server dumps are masked before they reach the bundle (research R5).
	envDump, err := os.ReadFile(filepath.Join(destDir, "bundle", "server", "environment.txt"))
	if err != nil {
		t.Errorf("server/environment.txt missing from archive: %v", err)
	} else {
		if strings.Contains(string(envDump), "secret123") {
			t.Errorf("environment.txt leaks the API token: %q", envDump)
		}
		if !strings.Contains(string(envDump), "INFRAHUB_API_TOKEN="+maskedValue) {
			t.Errorf("environment.txt = %q, want the token masked", envDump)
		}
	}
	configDump, err := os.ReadFile(filepath.Join(destDir, "bundle", "server", "config.json"))
	if err != nil {
		t.Errorf("server/config.json missing from archive: %v", err)
	} else if strings.Contains(string(configDump), "abc123") || !strings.Contains(string(configDump), maskedValue) {
		t.Errorf("config.json = %q, want the secret masked", configDump)
	}
	versionDump, err := os.ReadFile(filepath.Join(destDir, "bundle", "server", "version.txt"))
	if err != nil {
		t.Errorf("server/version.txt missing from archive: %v", err)
	} else if strings.TrimSpace(string(versionDump)) != "1.5.2" {
		t.Errorf("version.txt = %q, want %q", versionDump, "1.5.2")
	}

	logDump, err := os.ReadFile(filepath.Join(destDir, "bundle", "logs", "infrahub-server", "infrahub-server-1.log"))
	if err != nil {
		t.Errorf("logs/infrahub-server/infrahub-server-1.log missing from archive: %v", err)
	} else if string(logDump) != "log line\n" {
		t.Errorf("log content = %q, want %q", logDump, "log line\n")
	}

	metricsDump, err := os.ReadFile(filepath.Join(destDir, "bundle", "metrics", "metrics.txt"))
	if err != nil {
		t.Errorf("metrics/metrics.txt missing from archive: %v", err)
	} else if !strings.HasPrefix(string(metricsDump), backend.metrics) {
		t.Errorf("metrics.txt content = %q, want prefix %q", metricsDump, backend.metrics)
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

// TestRunCollectPlan_CollectorPanicRecovered proves a panicking collector (run
// or skip) is recorded as failed and the run continues, so one bad collector
// never defeats FR-009 (FIX-3).
func TestRunCollectPlan_CollectorPanicRecovered(t *testing.T) {
	t.Run("a panic in run is recorded and the run continues", func(t *testing.T) {
		iops := NewInfrahubOps()
		backend := newFakeCollectBackend()
		outputDir := filepath.Join(t.TempDir(), "bundles")
		opts := CollectOptions{OutputDir: outputDir, LogLines: 100000}

		afterRan := false
		plan := []collector{
			{name: "panicker", run: func(cc *collectContext) error { panic("boom") }},
			{name: "after", run: func(cc *collectContext) error { afterRan = true; return nil }},
		}

		if err := iops.runCollectPlan(backend, opts, plan); err != nil {
			t.Fatalf("runCollectPlan aborted on a collector panic, want the run to complete: %v", err)
		}
		if !afterRan {
			t.Error("collector after the panicking one did not run (FR-009)")
		}

		manifest, _ := extractBundleManifest(t, findArchive(t, outputDir))
		wantResults := []CollectorResult{
			{Name: "panicker", Status: collectorStatusFailed, Reason: "panic: boom"},
			{Name: "after", Status: collectorStatusSuccess},
		}
		if len(manifest.Collectors) != len(wantResults) {
			t.Fatalf("manifest has %d entries %+v, want %d", len(manifest.Collectors), manifest.Collectors, len(wantResults))
		}
		for i, want := range wantResults {
			if manifest.Collectors[i] != want {
				t.Errorf("collectors[%d] = %+v, want %+v", i, manifest.Collectors[i], want)
			}
		}
		assertStagingRemoved(t, outputDir)
	})

	t.Run("a panic in the skip precondition is recorded as failed", func(t *testing.T) {
		iops := NewInfrahubOps()
		backend := newFakeCollectBackend()
		outputDir := filepath.Join(t.TempDir(), "bundles")
		opts := CollectOptions{OutputDir: outputDir, LogLines: 100000}

		plan := []collector{
			{
				name: "panic-skip",
				skip: func(cc *collectContext) (bool, string) { panic("skip boom") },
				run:  func(cc *collectContext) error { t.Error("run reached despite panicking skip"); return nil },
			},
		}

		if err := iops.runCollectPlan(backend, opts, plan); err != nil {
			t.Fatalf("runCollectPlan aborted on a panicking skip, want the run to complete: %v", err)
		}

		manifest, _ := extractBundleManifest(t, findArchive(t, outputDir))
		want := CollectorResult{Name: "panic-skip", Status: collectorStatusFailed, Reason: "panic: skip boom"}
		if len(manifest.Collectors) != 1 || manifest.Collectors[0] != want {
			t.Errorf("collectors = %+v, want [%+v]", manifest.Collectors, want)
		}
	})
}

// readOnlyGuardBackend fails the test if any workload-lifecycle method is
// called, enforcing FR-010 / SC-003: collect must never stop, start, or scale
// a deployment's workloads (FIX-T3).
type readOnlyGuardBackend struct {
	fakeCollectBackend
	t *testing.T
}

func (b *readOnlyGuardBackend) Start(services ...string) error {
	b.t.Errorf("Start(%v) called during collect: FR-010 requires a read-only run", services)
	return nil
}

func (b *readOnlyGuardBackend) Stop(services ...string) error {
	b.t.Errorf("Stop(%v) called during collect: FR-010 requires a read-only run", services)
	return nil
}

// TestRunCollectPlan_ReadOnlyGuard drives the full registered plan through
// runCollectPlan with a backend that fails the test on any lifecycle call, and
// asserts the bundle is still produced with zero Start/Stop calls (FR-010,
// SC-003 — previously only covered by the environment-gated k8s e2e; FIX-T3).
func TestRunCollectPlan_ReadOnlyGuard(t *testing.T) {
	iops := NewInfrahubOps()
	guard := &readOnlyGuardBackend{fakeCollectBackend: *newFakeCollectBackend(), t: t}
	iops.backend = guard
	outputDir := filepath.Join(t.TempDir(), "bundles")
	opts := CollectOptions{OutputDir: outputDir, LogLines: 100000}

	if err := iops.runCollectPlan(guard, opts, iops.collectPlan(opts)); err != nil {
		t.Fatalf("runCollectPlan failed: %v", err)
	}

	// The guard asserts zero lifecycle calls; the archive must still exist.
	findArchive(t, outputDir)
	assertStagingRemoved(t, outputDir)
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
