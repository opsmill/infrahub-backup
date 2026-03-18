package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/PlakarKorp/kloset/objects"
	"github.com/PlakarKorp/kloset/repository"
	"github.com/PlakarKorp/kloset/snapshot"
	"github.com/sirupsen/logrus"
)

// CreatePlakarBackup creates an Infrahub backup as a Plakar snapshot.
func (iops *InfrahubOps) CreatePlakarBackup(force bool, neo4jMetadata string, excludeTaskManager bool, sleepDuration time.Duration, redact bool) error {
	if err := iops.checkPrerequisites(); err != nil {
		return err
	}

	if err := iops.DetectEnvironment(); err != nil {
		return err
	}

	// Detect Neo4j edition
	editionInfo := iops.detectNeo4jEditionInfo("backup")
	if editionInfo.IsCommunity {
		logrus.Warn("Neo4j Community Edition detected; Infrahub services will be stopped and restarted before the backup begins.")
		logrus.Warn("Waiting 10 seconds to allow the user to abort... CTRL+C to cancel.")
		time.Sleep(10 * time.Second)
	}

	// Redact attribute values if requested
	if redact {
		if !force {
			return fmt.Errorf("--redact is a destructive operation that replaces all attribute values in the database with random UUIDs; use --force to confirm")
		}
		if err := iops.redactDatabase(); err != nil {
			return err
		}
	}

	version := iops.getInfrahubVersion()

	// Check for running tasks unless --force is set
	if !force {
		logrus.Info("Checking for running tasks before backup...")
		if err := iops.waitForRunningTasks(); err != nil {
			return err
		}
	}

	// Stop app containers for community edition
	var servicesToRestart []string
	if editionInfo.IsCommunity {
		stoppedServices, stopErr := iops.stopAppContainers()
		if stopErr != nil {
			if len(stoppedServices) > 0 {
				if startErr := iops.startAppContainers(stoppedServices); startErr != nil {
					logrus.Warnf("Failed to restart services after stop error: %v", startErr)
				}
			}
			return fmt.Errorf("failed to stop services for Neo4j Community backup: %w", stopErr)
		}
		servicesToRestart = append([]string(nil), stoppedServices...)
		defer func() {
			if len(servicesToRestart) == 0 {
				return
			}
			if startErr := iops.startAppContainers(servicesToRestart); startErr != nil {
				logrus.Errorf("Failed to restart services after backup: %v", startErr)
			}
		}()
	}

	// Create temp directory for database dumps
	workDir, err := os.MkdirTemp("", "infrahub_plakar_backup_*")
	if err != nil {
		return fmt.Errorf("failed to create temp directory: %w", err)
	}
	defer os.RemoveAll(workDir)

	backupDir := filepath.Join(workDir, "backup")
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		return fmt.Errorf("failed to create backup directory: %w", err)
	}

	logrus.WithFields(logrus.Fields{
		"repo":          iops.config.Plakar.RepoPath,
		"neo4j_edition": editionInfo.Edition,
	}).Info("Creating Plakar backup")

	// Dump databases to temp directory
	if err := iops.backupDatabase(backupDir, neo4jMetadata, editionInfo.Edition); err != nil {
		return err
	}

	if !excludeTaskManager {
		if err := iops.backupTaskManagerDB(backupDir); err != nil {
			return err
		}
	} else {
		logrus.Info("Skipping task manager database backup as requested")
	}

	// Generate backup metadata
	backupID := fmt.Sprintf("infrahub_backup_%s", time.Now().Format("20060102_150405"))
	metadata := iops.createBackupMetadata(backupID, !excludeTaskManager, version, editionInfo.Edition)
	if redact {
		metadata.Redacted = true
	}

	// Write metadata JSON into the dump directory
	metadataBytes, err := json.MarshalIndent(metadata, "", "    ")
	if err != nil {
		return fmt.Errorf("failed to marshal metadata: %w", err)
	}
	metadataDir := filepath.Join(backupDir, "metadata")
	if err := os.MkdirAll(metadataDir, 0755); err != nil {
		return fmt.Errorf("failed to create metadata directory: %w", err)
	}
	if err := os.WriteFile(filepath.Join(metadataDir, "backup_information.json"), metadataBytes, 0644); err != nil {
		return fmt.Errorf("failed to write metadata: %w", err)
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

	// Create snapshot builder
	builder, err := snapshot.Create(repo, repository.DefaultType, os.TempDir(), objects.NilMac)
	if err != nil {
		return fmt.Errorf("failed to create plakar snapshot: %w", err)
	}
	defer builder.Close()

	// Build snapshot tags from metadata
	tags := buildSnapshotTags(metadata)

	// Create importer from the dump directory
	hostname, _ := os.Hostname()
	imp := NewInfrahubImporter(hostname, backupDir)

	// Run the backup
	opts := &snapshot.BackupOptions{
		Name: backupID,
		Tags: tags,
	}

	logrus.Info("Creating Plakar snapshot...")
	if err := builder.Backup(imp, opts); err != nil {
		return fmt.Errorf("failed to create plakar snapshot: %w", err)
	}

	logrus.WithFields(logrus.Fields{
		"snapshot_id": fmt.Sprintf("%x", builder.Header.Identifier[:8]),
		"repo":        iops.config.Plakar.RepoPath,
	}).Info("Plakar backup created successfully")

	// Sleep if requested
	if sleepDuration > 0 {
		logrus.Infof("Sleeping for %v to allow backup file transfer...", sleepDuration)
		time.Sleep(sleepDuration)
	}

	return nil
}

// buildSnapshotTags creates Plakar snapshot tags from backup metadata.
func buildSnapshotTags(metadata *BackupMetadata) []string {
	tags := []string{
		"infrahub.version=" + metadata.InfrahubVersion,
		"infrahub.backup-tool-version=" + metadata.ToolVersion,
		"infrahub.neo4j-edition=" + metadata.Neo4jEdition,
		"infrahub.components=" + strings.Join(metadata.Components, ","),
	}
	if metadata.Redacted {
		tags = append(tags, "infrahub.redacted=true")
	}
	return tags
}
