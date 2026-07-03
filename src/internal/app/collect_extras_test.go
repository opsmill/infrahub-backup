package app

import (
	"encoding/json"
	"fmt"
	"path/filepath"
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
