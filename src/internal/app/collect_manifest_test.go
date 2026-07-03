package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

var collectIDPattern = regexp.MustCompile(`^[0-9]{8}_[0-9]{6}$`)

func TestGenerateCollectID_Pattern(t *testing.T) {
	id := generateCollectID()
	if !collectIDPattern.MatchString(id) {
		t.Errorf("generateCollectID() = %q, want match for %s", id, collectIDPattern)
	}
}

func TestNewBundleManifest_Fields(t *testing.T) {
	before := time.Now().UTC()
	manifest := newBundleManifest("20260703_101530", "kubernetes", 100000)

	if manifest.ManifestVersion != 2026070200 {
		t.Errorf("ManifestVersion = %d, want 2026070200", manifest.ManifestVersion)
	}
	if manifest.CollectID != "20260703_101530" {
		t.Errorf("CollectID = %q, want %q", manifest.CollectID, "20260703_101530")
	}
	if manifest.Environment != "kubernetes" {
		t.Errorf("Environment = %q, want %q", manifest.Environment, "kubernetes")
	}
	if manifest.LogLines != 100000 {
		t.Errorf("LogLines = %d, want 100000", manifest.LogLines)
	}
	if manifest.ToolVersion == "" {
		t.Error("ToolVersion is empty, want BuildRevision output")
	}
	if manifest.Collectors == nil {
		t.Error("Collectors is nil, want empty slice so JSON serializes as []")
	}

	createdAt, err := time.Parse(time.RFC3339, manifest.CreatedAt)
	if err != nil {
		t.Fatalf("CreatedAt %q is not RFC3339: %v", manifest.CreatedAt, err)
	}
	if createdAt.Before(before.Truncate(time.Second)) || createdAt.After(time.Now().UTC().Add(time.Second)) {
		t.Errorf("CreatedAt %q is not a current UTC timestamp", manifest.CreatedAt)
	}
}

// TestBundleManifest_JSONShape validates the serialized manifest against the
// semantics of specs/003-collect-tool/contracts/manifest.schema.json: exact
// field names, collect_id pattern, environment/status enums, and reason
// present exactly when status is failed or skipped.
func TestBundleManifest_JSONShape(t *testing.T) {
	manifest := newBundleManifest(generateCollectID(), "docker", 500)
	manifest.recordSuccess("logs/infrahub-server")
	manifest.recordFailed("cache-status", "container not running")
	manifest.recordSkipped("benchmark", "not requested")
	manifest.recordFailed("message-queue-status", "timed out after 60s")

	data, err := json.MarshalIndent(manifest, "", "    ")
	if err != nil {
		t.Fatalf("MarshalIndent failed: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	// All schema-required fields must be present with the exact JSON names.
	// The schema allows additionalProperties, so the exact field count is not
	// asserted — that would break the next time a legal optional field is added
	// (FIX-T1).
	for _, field := range []string{
		"manifest_version", "collect_id", "created_at", "tool_version",
		"infrahub_version", "environment", "log_lines", "collectors",
	} {
		if _, ok := decoded[field]; !ok {
			t.Errorf("serialized manifest is missing required field %q", field)
		}
	}

	if version, ok := decoded["manifest_version"].(float64); !ok || int(version) != 2026070200 {
		t.Errorf("manifest_version = %v, want 2026070200", decoded["manifest_version"])
	}
	if id, ok := decoded["collect_id"].(string); !ok || !collectIDPattern.MatchString(id) {
		t.Errorf("collect_id = %v, want match for %s", decoded["collect_id"], collectIDPattern)
	}
	if env, ok := decoded["environment"].(string); !ok || (env != "docker" && env != "kubernetes") {
		t.Errorf("environment = %v, want docker or kubernetes", decoded["environment"])
	}
	if lines, ok := decoded["log_lines"].(float64); !ok || int(lines) != 500 {
		t.Errorf("log_lines = %v, want 500", decoded["log_lines"])
	}
	if infrahubVersion, ok := decoded["infrahub_version"].(string); !ok {
		t.Errorf("infrahub_version = %v, want a string (empty allowed)", decoded["infrahub_version"])
	} else if infrahubVersion != "" {
		t.Errorf("infrahub_version = %q, want empty before server collection populates it", infrahubVersion)
	}

	collectors, ok := decoded["collectors"].([]any)
	if !ok {
		t.Fatalf("collectors = %T, want array", decoded["collectors"])
	}
	if len(collectors) != 4 {
		t.Fatalf("collectors has %d entries, want 4", len(collectors))
	}

	wantEntries := []struct {
		name       string
		status     string
		wantReason string
		hasReason  bool
	}{
		{"logs/infrahub-server", "success", "", false},
		{"cache-status", "failed", "container not running", true},
		{"benchmark", "skipped", "not requested", true},
		{"message-queue-status", "failed", "timed out after 60s", true},
	}

	validStatuses := map[string]bool{"success": true, "failed": true, "skipped": true}

	for i, want := range wantEntries {
		entry, ok := collectors[i].(map[string]any)
		if !ok {
			t.Fatalf("collectors[%d] = %T, want object", i, collectors[i])
		}
		if entry["name"] != want.name {
			t.Errorf("collectors[%d].name = %v, want %q", i, entry["name"], want.name)
		}
		status, _ := entry["status"].(string)
		if !validStatuses[status] {
			t.Errorf("collectors[%d].status = %q, not in schema enum success|failed|skipped", i, status)
		}
		if status != want.status {
			t.Errorf("collectors[%d].status = %q, want %q", i, status, want.status)
		}
		reason, hasReason := entry["reason"]
		if hasReason != want.hasReason {
			t.Errorf("collectors[%d] reason present = %v, want %v (schema: reason required iff status != success)", i, hasReason, want.hasReason)
		}
		if want.hasReason && reason != want.wantReason {
			t.Errorf("collectors[%d].reason = %v, want %q", i, reason, want.wantReason)
		}
	}
}

func TestBundleManifest_EmptyCollectorsSerializesAsArray(t *testing.T) {
	manifest := newBundleManifest(generateCollectID(), "docker", 100000)

	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if _, ok := decoded["collectors"].([]any); !ok {
		t.Errorf("collectors = %v (%T), want JSON array (not null)", decoded["collectors"], decoded["collectors"])
	}
}

func TestBundleManifest_Write(t *testing.T) {
	bundleDir := t.TempDir()
	manifest := newBundleManifest("20260703_101530", "docker", 100000)
	manifest.recordSuccess("metrics")

	if err := manifest.write(bundleDir); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(bundleDir, "bundle_information.json"))
	if err != nil {
		t.Fatalf("bundle_information.json not written: %v", err)
	}

	var decoded BundleManifest
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("written manifest is not valid JSON: %v", err)
	}
	if decoded.CollectID != "20260703_101530" {
		t.Errorf("round-tripped CollectID = %q, want %q", decoded.CollectID, "20260703_101530")
	}
	if len(decoded.Collectors) != 1 || decoded.Collectors[0].Status != collectorStatusSuccess {
		t.Errorf("round-tripped Collectors = %+v, want one success entry", decoded.Collectors)
	}
}

func TestBundleManifest_WriteFailsOnMissingDir(t *testing.T) {
	manifest := newBundleManifest(generateCollectID(), "docker", 100000)
	err := manifest.write(filepath.Join(t.TempDir(), "does", "not", "exist"))
	if err == nil {
		t.Fatal("write to missing directory succeeded, want error")
	}
}
