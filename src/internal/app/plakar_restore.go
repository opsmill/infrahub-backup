package app

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/PlakarKorp/kloset/objects"
	"github.com/PlakarKorp/kloset/snapshot"
	"github.com/PlakarKorp/kloset/snapshot/exporter"
	"github.com/sirupsen/logrus"
)

// RestorePlakarBackup restores an Infrahub deployment from a Plakar snapshot.
func (iops *InfrahubOps) RestorePlakarBackup(excludeTaskManager bool, restoreMigrateFormat bool, sleepDuration time.Duration, force bool) error {
	// Sleep if requested (for K8s users to transfer backup file into pod)
	if sleepDuration > 0 {
		logrus.Infof("Sleeping for %v to allow backup file transfer...", sleepDuration)
		time.Sleep(sleepDuration)
	}

	if err := iops.checkPrerequisites(); err != nil {
		return err
	}

	if err := iops.DetectEnvironment(); err != nil {
		return err
	}

	// Initialize Plakar context and repository
	kctx, err := initPlakarContext(iops.config.Plakar)
	if err != nil {
		return fmt.Errorf("failed to initialize plakar context: %w", err)
	}
	defer closePlakarContext(kctx)

	repo, err := openOrCreateRepo(kctx, iops.config.Plakar)
	if err != nil {
		return err
	}
	defer closeRepo(repo)

	// Resolve snapshot ID
	snapshotMAC, err := resolveSnapshotID(repo, iops.config.Plakar.SnapshotID)
	if err != nil {
		return err
	}

	// Load snapshot
	snap, err := snapshot.Load(repo, snapshotMAC)
	if err != nil {
		return fmt.Errorf("failed to load plakar snapshot: %w", err)
	}
	defer snap.Close()

	logrus.WithFields(logrus.Fields{
		"snapshot_id": fmt.Sprintf("%x", snap.Header.Identifier[:8]),
		"date":        snap.Header.Timestamp.Format(time.RFC3339),
		"name":        snap.Header.Name,
	}).Info("Restoring from Plakar snapshot")

	// Create temp directory for extraction
	workDir, err := os.MkdirTemp("", "infrahub_plakar_restore_*")
	if err != nil {
		return fmt.Errorf("failed to create temp directory: %w", err)
	}
	defer os.RemoveAll(workDir)

	// Export snapshot to temp directory using fs exporter
	exportDir := filepath.Join(workDir, "backup")
	if err := os.MkdirAll(exportDir, 0755); err != nil {
		return fmt.Errorf("failed to create export directory: %w", err)
	}

	exp, err := exporter.NewExporter(kctx, map[string]string{
		"location": "fs://" + exportDir,
	})
	if err != nil {
		return fmt.Errorf("failed to create plakar exporter: %w", err)
	}
	defer exp.Close(kctx.Context)

	logrus.Info("Extracting Plakar snapshot...")
	restoreOpts := &snapshot.RestoreOptions{
		SkipPermissions: true,
	}
	if err := snap.Restore(exp, exportDir, "/", restoreOpts); err != nil {
		return fmt.Errorf("failed to extract plakar snapshot: %w", err)
	}

	// Read and parse backup metadata
	metadataPath := filepath.Join(exportDir, "metadata", "backup_information.json")
	metadataBytes, err := os.ReadFile(metadataPath)
	if err != nil {
		return fmt.Errorf("failed to read backup metadata from snapshot: %w", err)
	}
	var metadata BackupMetadata
	if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
		return fmt.Errorf("failed to parse backup metadata: %w", err)
	}

	logrus.WithFields(logrus.Fields{
		"backup_id":        metadata.BackupID,
		"created_at":       metadata.CreatedAt,
		"tool_version":     metadata.ToolVersion,
		"infrahub_version": metadata.InfrahubVersion,
		"neo4j_edition":    metadata.Neo4jEdition,
		"components":       metadata.Components,
	}).Info("Backup metadata loaded")

	// Detect Neo4j edition for restore
	detectedEdition, detectionErr := iops.detectNeo4jEdition()
	editionInfo := NewNeo4jEditionInfo(detectedEdition, detectionErr)

	neo4jEdition, err := editionInfo.ResolveRestoreEdition(metadata.Neo4jEdition)
	if err != nil {
		return err
	}
	editionInfo.LogDetection("restore")

	// The snapshot layout has database/ and metadata/ at the top level.
	// We need to restructure for the existing restore functions which expect
	// workDir/backup/database/ and workDir/backup/prefect.dump.
	// Since we exported into workDir/backup/, the database dir is at
	// workDir/backup/database/ which matches what restoreNeo4j expects (it uses workDir).

	// Determine task manager database availability
	taskManagerIncluded := false
	for _, comp := range metadata.Components {
		if comp == "task-manager-db" {
			taskManagerIncluded = true
			break
		}
	}

	shouldRestoreTaskManager := taskManagerIncluded && !excludeTaskManager
	prefectPath := filepath.Join(exportDir, "prefect.dump")
	prefectExists := fileExists(prefectPath)

	if taskManagerIncluded && excludeTaskManager {
		logrus.Info("Skipping task manager database restore as requested")
	} else if !taskManagerIncluded {
		logrus.Info("Backup does not include task manager database; skipping restore")
	} else if prefectExists {
		logrus.Info("Task manager database dump detected; will restore")
	}

	// Wipe transient data
	iops.wipeTransientData()

	// Stop application containers
	if _, err := iops.stopAppContainers(); err != nil {
		return err
	}

	// Restore PostgreSQL when available
	if shouldRestoreTaskManager && prefectExists {
		if err := iops.restorePostgreSQL(workDir); err != nil {
			return err
		}
	} else {
		logrus.Info("Skipping task manager database restore step")
	}

	// Restart dependencies
	if err := iops.restartDependencies(); err != nil {
		return err
	}

	// Restore Neo4j
	if err := iops.restoreNeo4j(workDir, neo4jEdition, restoreMigrateFormat); err != nil {
		return err
	}

	// Restart all services
	logrus.Info("Restarting Infrahub services...")
	if err := iops.StartServices("infrahub-server", "task-worker"); err != nil {
		return fmt.Errorf("failed to restart infrahub services: %w", err)
	}

	logrus.Info("Restore from Plakar snapshot completed successfully")
	logrus.Info("Infrahub should be available shortly")

	return nil
}

// snapshotLister is an interface for listing snapshots in a repository.
type snapshotLister interface {
	ListSnapshots() iter.Seq[objects.MAC]
}

// resolveSnapshotID resolves a snapshot identifier (partial hex or empty for latest).
func resolveSnapshotID(repo snapshotLister, snapshotID string) (objects.MAC, error) {
	if snapshotID == "" {
		// Find the latest snapshot
		var latest objects.MAC
		found := false
		for mac := range repo.ListSnapshots() {
			latest = mac
			found = true
		}
		if !found {
			return objects.MAC{}, fmt.Errorf("no snapshots found in repository")
		}
		return latest, nil
	}

	// Try to match a partial snapshot ID
	idBytes, err := hex.DecodeString(snapshotID)
	if err != nil {
		return objects.MAC{}, fmt.Errorf("invalid snapshot ID %q: not valid hex", snapshotID)
	}

	var matched objects.MAC
	matchCount := 0
	prefix := strings.ToLower(snapshotID)

	for mac := range repo.ListSnapshots() {
		macHex := hex.EncodeToString(mac[:])
		if strings.HasPrefix(macHex, prefix) {
			matched = mac
			matchCount++
		}
	}

	if matchCount == 0 {
		// List available snapshots in error
		var available []string
		for mac := range repo.ListSnapshots() {
			available = append(available, fmt.Sprintf("%x", mac[:8]))
		}
		if len(available) > 0 {
			return objects.MAC{}, fmt.Errorf("snapshot not found: %s\nAvailable snapshots: %s", snapshotID, strings.Join(available, ", "))
		}
		return objects.MAC{}, fmt.Errorf("snapshot not found: %s (repository contains no snapshots)", snapshotID)
	}

	if matchCount > 1 {
		return objects.MAC{}, fmt.Errorf("ambiguous snapshot ID %q: matches %d snapshots; provide a longer prefix", snapshotID, matchCount)
	}

	_ = idBytes // used for validation
	return matched, nil
}
