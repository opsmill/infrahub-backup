package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// manifestVersion is the date-serial version of the bundle manifest schema
// (bump on breaking manifest changes), mirroring metadataVersion for backups.
const manifestVersion = 2026070200

// bundleManifestFilename is the manifest file written at the bundle root.
const bundleManifestFilename = "bundle_information.json"

// collectIDLayout formats collect identifiers as YYYYMMDD_HHMMSS (UTC).
const collectIDLayout = "20060102_150405"

// collectorStatus is the outcome of a single collector run.
type collectorStatus string

const (
	collectorStatusSuccess collectorStatus = "success"
	collectorStatusFailed  collectorStatus = "failed"
	collectorStatusSkipped collectorStatus = "skipped"
)

// CollectorResult records the outcome of one collector in the bundle
// manifest. Reason is required whenever Status is not success.
type CollectorResult struct {
	Name   string          `json:"name"`
	Status collectorStatus `json:"status"`
	Reason string          `json:"reason,omitempty"`
}

// BundleManifest is the collect-side sibling of BackupMetadata, serialized as
// bundle_information.json at the bundle root. JSON field names are normative
// (specs/003-collect-tool/contracts/manifest.schema.json).
type BundleManifest struct {
	ManifestVersion int               `json:"manifest_version"`
	CollectID       string            `json:"collect_id"`
	CreatedAt       string            `json:"created_at"`
	ToolVersion     string            `json:"tool_version"`
	InfrahubVersion string            `json:"infrahub_version"`
	Environment     string            `json:"environment"`
	LogLines        int               `json:"log_lines"`
	Collectors      []CollectorResult `json:"collectors"`
}

// generateCollectID returns a UTC timestamp identifier shared by the bundle
// manifest and the archive filename.
func generateCollectID() string {
	return time.Now().UTC().Format(collectIDLayout)
}

// newBundleManifest builds a manifest for a collect run. InfrahubVersion is
// populated later by the server-info collection step; an empty string is
// valid when the server is unreachable.
func newBundleManifest(collectID, environment string, logLines int) *BundleManifest {
	return &BundleManifest{
		ManifestVersion: manifestVersion,
		CollectID:       collectID,
		CreatedAt:       time.Now().UTC().Format(time.RFC3339),
		ToolVersion:     BuildRevision(),
		Environment:     environment,
		LogLines:        logLines,
		Collectors:      []CollectorResult{},
	}
}

// recordSuccess appends a success outcome for a collector.
func (m *BundleManifest) recordSuccess(name string) {
	m.Collectors = append(m.Collectors, CollectorResult{Name: name, Status: collectorStatusSuccess})
}

// recordFailed appends a failed outcome with a human-readable reason.
func (m *BundleManifest) recordFailed(name, reason string) {
	m.Collectors = append(m.Collectors, CollectorResult{Name: name, Status: collectorStatusFailed, Reason: reason})
}

// recordSkipped appends a skipped outcome with a human-readable reason.
func (m *BundleManifest) recordSkipped(name, reason string) {
	m.Collectors = append(m.Collectors, CollectorResult{Name: name, Status: collectorStatusSkipped, Reason: reason})
}

// write serializes the manifest as bundle_information.json in bundleDir.
func (m *BundleManifest) write(bundleDir string) error {
	data, err := json.MarshalIndent(m, "", "    ")
	if err != nil {
		return fmt.Errorf("failed to marshal bundle manifest: %w", err)
	}

	path := filepath.Join(bundleDir, bundleManifestFilename)
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("failed to write bundle manifest %s: %w", path, err)
	}

	return nil
}
