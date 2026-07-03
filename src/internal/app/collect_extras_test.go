package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestIncludeBackupCollector_OutcomeRecording drives the include-backup
// collector through runCollectPlan with fake backup runners (T034): success
// records the artifact path, failure records a failed entry while the bundle
// is still produced (US3 scenario 2), and an unset flag records skipped.
func TestIncludeBackupCollector_OutcomeRecording(t *testing.T) {
	t.Run("success records the backup artifact", func(t *testing.T) {
		iops := NewInfrahubOps()
		backend := newFakeCollectBackend()
		outputDir := filepath.Join(t.TempDir(), "bundles")
		opts := CollectOptions{OutputDir: outputDir, LogLines: 100000, IncludeBackup: true}

		const artifact = "/backups/infrahub_backup_20260703_101530.tar.gz"
		ran := false
		plan := []collector{includeBackupCollector(func(cc *collectContext) (string, error) {
			ran = true
			return artifact, nil
		})}

		if err := iops.runCollectPlan(backend, opts, plan); err != nil {
			t.Fatalf("runCollectPlan failed: %v", err)
		}
		if !ran {
			t.Fatal("backup runner was not invoked despite --include-backup")
		}

		manifest, _ := extractBundleManifest(t, findArchive(t, outputDir))
		want := CollectorResult{Name: "backup", Status: collectorStatusSuccess, Artifact: artifact}
		if len(manifest.Collectors) != 1 || manifest.Collectors[0] != want {
			t.Errorf("collectors = %+v, want [%+v]", manifest.Collectors, want)
		}
	})

	t.Run("backup failure records failed and still produces the bundle", func(t *testing.T) {
		iops := NewInfrahubOps()
		backend := newFakeCollectBackend()
		outputDir := filepath.Join(t.TempDir(), "bundles")
		opts := CollectOptions{OutputDir: outputDir, LogLines: 100000, IncludeBackup: true}

		plan := []collector{
			{
				name: "metrics",
				run:  func(cc *collectContext) error { return nil },
			},
			includeBackupCollector(func(cc *collectContext) (string, error) {
				return "", fmt.Errorf("database container unreachable")
			}),
		}

		// US3 scenario 2: a backup failure never aborts the run.
		if err := iops.runCollectPlan(backend, opts, plan); err != nil {
			t.Fatalf("runCollectPlan failed after backup error, want bundle still produced: %v", err)
		}

		manifest, _ := extractBundleManifest(t, findArchive(t, outputDir))
		wantResults := []CollectorResult{
			{Name: "metrics", Status: collectorStatusSuccess},
			{Name: "backup", Status: collectorStatusFailed, Reason: "backup failed: database container unreachable"},
		}
		if len(manifest.Collectors) != len(wantResults) {
			t.Fatalf("manifest has %d collector entries %+v, want %d", len(manifest.Collectors), manifest.Collectors, len(wantResults))
		}
		for i, want := range wantResults {
			if manifest.Collectors[i] != want {
				t.Errorf("collectors[%d] = %+v, want %+v", i, manifest.Collectors[i], want)
			}
		}
	})

	t.Run("skipped as not requested when the flag is unset", func(t *testing.T) {
		iops := NewInfrahubOps()
		backend := newFakeCollectBackend()
		outputDir := filepath.Join(t.TempDir(), "bundles")
		opts := CollectOptions{OutputDir: outputDir, LogLines: 100000, IncludeBackup: false}

		plan := []collector{includeBackupCollector(func(cc *collectContext) (string, error) {
			t.Error("backup runner invoked without --include-backup")
			return "", nil
		})}

		if err := iops.runCollectPlan(backend, opts, plan); err != nil {
			t.Fatalf("runCollectPlan failed: %v", err)
		}

		manifest, _ := extractBundleManifest(t, findArchive(t, outputDir))
		want := CollectorResult{Name: "backup", Status: collectorStatusSkipped, Reason: "not requested"}
		if len(manifest.Collectors) != 1 || manifest.Collectors[0] != want {
			t.Errorf("collectors = %+v, want [%+v]", manifest.Collectors, want)
		}
	})
}

// TestCollectorResult_ArtifactJSONShape pins the manifest contract for the
// artifact reference (contracts/manifest.schema.json): the field serializes
// as "artifact" and is omitted from entries that did not produce one.
func TestCollectorResult_ArtifactJSONShape(t *testing.T) {
	manifest := newBundleManifest(generateCollectID(), "docker", 100000)
	manifest.recordSuccessArtifact("backup", "/backups/infrahub_backup_20260703_101530.tar.gz")
	manifest.recordSuccess("metrics")

	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	collectors, ok := decoded["collectors"].([]any)
	if !ok || len(collectors) != 2 {
		t.Fatalf("collectors = %v, want array of 2 entries", decoded["collectors"])
	}

	backupEntry, ok := collectors[0].(map[string]any)
	if !ok {
		t.Fatalf("collectors[0] = %T, want object", collectors[0])
	}
	if backupEntry["artifact"] != "/backups/infrahub_backup_20260703_101530.tar.gz" {
		t.Errorf("backup entry artifact = %v, want the archive path", backupEntry["artifact"])
	}

	metricsEntry, ok := collectors[1].(map[string]any)
	if !ok {
		t.Fatalf("collectors[1] = %T, want object", collectors[1])
	}
	if _, present := metricsEntry["artifact"]; present {
		t.Errorf("metrics entry = %v, want no artifact field (omitempty)", metricsEntry)
	}
}

// sampleBenchmarkOutput mimics the OpsMill bench image's entrypoint output,
// including the ANSI color codes it wraps result lines in.
const sampleBenchmarkOutput = "Running Disk IOPS benchmark... hold on\n" +
	"Benchmark results:\n" +
	"\x1b[0;32mMemory: 15895 MB - Required: 7000 MB : OK\n" +
	"\x1b[0;32mCPU Single Core Perf: 3500 - Required: 2000 : OK\n" +
	"\x1b[0;31mDisk Read IOPS: 4231 - Required: 5000 : KO\n" +
	"\x1b[0;32mDisk Write IOPS: 5120 - Required: 5000 : OK\n" +
	"\x1b[0m\n" +
	"Benchmark completed...\n"

// TestBenchmarkCollector_OutcomeRecording drives the benchmark collector
// through runCollectPlan with fake runners (T036): an unset flag records
// skipped/not requested, success stages bundle/benchmark/ files, and an image
// pull/run failure records skipped with a warning reason — never failed
// (FR-013, research R11, spec air-gapped edge case).
func TestBenchmarkCollector_OutcomeRecording(t *testing.T) {
	t.Run("skipped as not requested when the flag is unset", func(t *testing.T) {
		iops := NewInfrahubOps()
		backend := newFakeCollectBackend()
		outputDir := filepath.Join(t.TempDir(), "bundles")
		opts := CollectOptions{OutputDir: outputDir, LogLines: 100000, Benchmark: false}

		plan := []collector{benchmarkCollector(func(cc *collectContext, image string) (string, error) {
			t.Error("benchmark runner invoked without --benchmark")
			return "", nil
		})}

		if err := iops.runCollectPlan(backend, opts, plan); err != nil {
			t.Fatalf("runCollectPlan failed: %v", err)
		}

		manifest, _ := extractBundleManifest(t, findArchive(t, outputDir))
		want := CollectorResult{Name: "benchmark", Status: collectorStatusSkipped, Reason: "not requested"}
		if len(manifest.Collectors) != 1 || manifest.Collectors[0] != want {
			t.Errorf("collectors = %+v, want [%+v]", manifest.Collectors, want)
		}
	})

	t.Run("success stages benchmark.log and benchmark.json", func(t *testing.T) {
		iops := NewInfrahubOps()
		backend := newFakeCollectBackend()
		outputDir := filepath.Join(t.TempDir(), "bundles")
		opts := CollectOptions{OutputDir: outputDir, LogLines: 100000, Benchmark: true}

		// The override must reach the runner (INFRAHUB_BENCHMARK_IMAGE seam).
		t.Setenv(benchmarkImageEnvVar, "registry.example.com/opsmill/bench:pinned")
		gotImage := ""
		plan := []collector{benchmarkCollector(func(cc *collectContext, image string) (string, error) {
			gotImage = image
			return sampleBenchmarkOutput, nil
		})}

		if err := iops.runCollectPlan(backend, opts, plan); err != nil {
			t.Fatalf("runCollectPlan failed: %v", err)
		}
		if gotImage != "registry.example.com/opsmill/bench:pinned" {
			t.Errorf("runner image = %q, want the INFRAHUB_BENCHMARK_IMAGE override", gotImage)
		}

		manifest, destDir := extractBundleManifest(t, findArchive(t, outputDir))
		want := CollectorResult{Name: "benchmark", Status: collectorStatusSuccess}
		if len(manifest.Collectors) != 1 || manifest.Collectors[0] != want {
			t.Errorf("collectors = %+v, want [%+v]", manifest.Collectors, want)
		}

		logData, err := os.ReadFile(filepath.Join(destDir, "bundle", "benchmark", "benchmark.log"))
		if err != nil {
			t.Fatalf("benchmark/benchmark.log missing from archive: %v", err)
		}
		if string(logData) != sampleBenchmarkOutput {
			t.Errorf("benchmark.log = %q, want the raw runner output", logData)
		}

		jsonData, err := os.ReadFile(filepath.Join(destDir, "bundle", "benchmark", "benchmark.json"))
		if err != nil {
			t.Fatalf("benchmark/benchmark.json missing from archive: %v", err)
		}
		var report benchmarkReport
		if err := json.Unmarshal(jsonData, &report); err != nil {
			t.Fatalf("benchmark.json is not valid JSON: %v", err)
		}
		if want := (benchmarkMeasure{Value: 4231, Required: 5000, Status: "KO"}); report.Results["disk_read_iops"] != want {
			t.Errorf("disk_read_iops = %+v, want %+v", report.Results["disk_read_iops"], want)
		}
		if len(report.Results) != 4 {
			t.Errorf("parsed %d results %+v, want 4", len(report.Results), report.Results)
		}
	})

	t.Run("image pull or run failure records skipped with a warning, not failed", func(t *testing.T) {
		iops := NewInfrahubOps()
		backend := newFakeCollectBackend()
		outputDir := filepath.Join(t.TempDir(), "bundles")
		opts := CollectOptions{OutputDir: outputDir, LogLines: 100000, Benchmark: true}
		t.Setenv(benchmarkImageEnvVar, "") // pin the default image for the reason assertion

		plan := []collector{
			benchmarkCollector(func(cc *collectContext, image string) (string, error) {
				return "", fmt.Errorf("docker run failed: manifest unknown")
			}),
			{
				name: "metrics",
				run:  func(cc *collectContext) error { return nil },
			},
		}

		// Air-gapped edge case: the run continues and still exits 0.
		if err := iops.runCollectPlan(backend, opts, plan); err != nil {
			t.Fatalf("runCollectPlan failed after benchmark error, want bundle still produced: %v", err)
		}

		manifest, _ := extractBundleManifest(t, findArchive(t, outputDir))
		wantResults := []CollectorResult{
			{
				Name:   "benchmark",
				Status: collectorStatusSkipped,
				Reason: "benchmark image " + defaultBenchmarkImage + " could not be pulled or run: docker run failed: manifest unknown",
			},
			{Name: "metrics", Status: collectorStatusSuccess},
		}
		if len(manifest.Collectors) != len(wantResults) {
			t.Fatalf("manifest has %d collector entries %+v, want %d", len(manifest.Collectors), manifest.Collectors, len(wantResults))
		}
		for i, want := range wantResults {
			if manifest.Collectors[i] != want {
				t.Errorf("collectors[%d] = %+v, want %+v", i, manifest.Collectors[i], want)
			}
		}
	})
}

func TestResolveBenchmarkImage(t *testing.T) {
	env := map[string]string{}
	getenv := func(key string) string { return env[key] }

	if got := resolveBenchmarkImage(getenv); got != defaultBenchmarkImage {
		t.Errorf("resolveBenchmarkImage() = %q, want the default %q", got, defaultBenchmarkImage)
	}

	env[benchmarkImageEnvVar] = "registry.example.com/opsmill/bench:pinned"
	if got := resolveBenchmarkImage(getenv); got != "registry.example.com/opsmill/bench:pinned" {
		t.Errorf("resolveBenchmarkImage() = %q, want the override", got)
	}
}

func TestBenchmarkResourceName(t *testing.T) {
	got := benchmarkResourceName("20260703_141530")
	want := "infrahub-collect-benchmark-20260703-141530"
	if got != want {
		t.Errorf("benchmarkResourceName() = %q, want %q (RFC 1123 pod-name safe)", got, want)
	}
}

func TestDockerBenchmarkRunArgs(t *testing.T) {
	name := "infrahub-collect-benchmark-20260703-141530"

	withNetwork := dockerBenchmarkRunArgs(name, "infrahub_default", defaultBenchmarkImage)
	want := []string{
		"run", "--pull", "always", "--rm", "--name", name,
		"--network", "infrahub_default", defaultBenchmarkImage,
	}
	if !reflect.DeepEqual(withNetwork, want) {
		t.Errorf("dockerBenchmarkRunArgs(network) = %v, want %v", withNetwork, want)
	}

	withoutNetwork := dockerBenchmarkRunArgs(name, "", defaultBenchmarkImage)
	want = []string{"run", "--pull", "always", "--rm", "--name", name, defaultBenchmarkImage}
	if !reflect.DeepEqual(withoutNetwork, want) {
		t.Errorf("dockerBenchmarkRunArgs(no network) = %v, want %v", withoutNetwork, want)
	}
}

func TestKubectlBenchmarkRunArgs(t *testing.T) {
	got := kubectlBenchmarkRunArgs("infrahub", "infrahub-collect-benchmark-20260703-141530", defaultBenchmarkImage)
	want := []string{
		"run", "infrahub-collect-benchmark-20260703-141530",
		"-n", "infrahub",
		"--image=" + defaultBenchmarkImage,
		"--image-pull-policy=Always",
		"--restart=Never",
		"--attach",
		"--rm",
		"--quiet",
		"--pod-running-timeout=5m",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("kubectlBenchmarkRunArgs() = %v, want %v", got, want)
	}
}

func TestPickComposeNetwork(t *testing.T) {
	tests := []struct {
		name     string
		project  string
		networks []string
		want     string
	}{
		{
			name:     "prefers the compose default network",
			project:  "infrahub",
			networks: []string{"infrahub_backendnet", "infrahub_default"},
			want:     "infrahub_default",
		},
		{
			name:     "falls back to the first project network in lexical order",
			project:  "infrahub",
			networks: []string{"infrahub_frontnet", "infrahub_backendnet"},
			want:     "infrahub_backendnet",
		},
		{
			name:     "no networks means no attachment",
			project:  "infrahub",
			networks: nil,
			want:     "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pickComposeNetwork(tt.project, tt.networks); got != tt.want {
				t.Errorf("pickComposeNetwork(%q, %v) = %q, want %q", tt.project, tt.networks, got, tt.want)
			}
		})
	}
}

func TestCommandErrorLine(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   string
	}{
		{
			name: "picks the error line over docker's trailing help hint",
			output: "Unable to find image 'x:latest' locally\n" +
				"docker: Error response from daemon: manifest unknown\n" +
				"Run 'docker run --help' for more information\n",
			want: "docker: Error response from daemon: manifest unknown",
		},
		{
			name:   "falls back to the last non-empty line",
			output: "pulling layer 1/3\npulling layer 2/3\n\n",
			want:   "pulling layer 2/3",
		},
		{
			name:   "empty output yields empty string",
			output: "  \n\n",
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := commandErrorLine(tt.output); got != tt.want {
				t.Errorf("commandErrorLine(%q) = %q, want %q", tt.output, got, tt.want)
			}
		})
	}
}

func TestParseBenchmarkResults(t *testing.T) {
	clean, results := parseBenchmarkResults(sampleBenchmarkOutput)

	if strings.Contains(clean, "\x1b[") {
		t.Errorf("cleaned output still contains ANSI escapes: %q", clean)
	}

	want := map[string]benchmarkMeasure{
		"memory":               {Value: 15895, Required: 7000, Status: "OK"},
		"cpu_single_core_perf": {Value: 3500, Required: 2000, Status: "OK"},
		"disk_read_iops":       {Value: 4231, Required: 5000, Status: "KO"},
		"disk_write_iops":      {Value: 5120, Required: 5000, Status: "OK"},
	}
	if !reflect.DeepEqual(results, want) {
		t.Errorf("parseBenchmarkResults() = %+v, want %+v", results, want)
	}

	if _, empty := parseBenchmarkResults("no results here"); len(empty) != 0 {
		t.Errorf("parseBenchmarkResults(no results) = %+v, want empty", empty)
	}
}

func TestNewestNewArchive(t *testing.T) {
	tests := []struct {
		name   string
		before []string
		after  []string
		want   string
	}{
		{
			name:   "single new archive",
			before: []string{"/b/infrahub_backup_20260101_000000.tar.gz"},
			after: []string{
				"/b/infrahub_backup_20260101_000000.tar.gz",
				"/b/infrahub_backup_20260703_101530.tar.gz",
			},
			want: "/b/infrahub_backup_20260703_101530.tar.gz",
		},
		{
			name:   "no new archive",
			before: []string{"/b/infrahub_backup_20260101_000000.tar.gz"},
			after:  []string{"/b/infrahub_backup_20260101_000000.tar.gz"},
			want:   "",
		},
		{
			name:   "empty directory before and after",
			before: nil,
			after:  nil,
			want:   "",
		},
		{
			name:   "several new archives pick the newest",
			before: nil,
			after: []string{
				"/b/infrahub_backup_20260703_101530.tar.gz",
				"/b/infrahub_backup_20260703_101531.tar.gz",
			},
			want: "/b/infrahub_backup_20260703_101531.tar.gz",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := newestNewArchive(tt.before, tt.after); got != tt.want {
				t.Errorf("newestNewArchive(%v, %v) = %q, want %q", tt.before, tt.after, got, tt.want)
			}
		})
	}
}
