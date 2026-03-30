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

	"github.com/PlakarKorp/kloset/connectors"
	"github.com/PlakarKorp/kloset/connectors/exporter"
	"github.com/PlakarKorp/kloset/kcontext"
	"github.com/PlakarKorp/kloset/objects"
	"github.com/PlakarKorp/kloset/repository"
	"github.com/PlakarKorp/kloset/snapshot"
	"github.com/sirupsen/logrus"
)

// RestorePlakarBackup restores an Infrahub deployment from Plakar snapshots.
// Supports: backup-group restore (--backup-id), single snapshot (--snapshot),
// or latest complete group (default).
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

	repo, err := openRepo(kctx, iops.config.Plakar)
	if err != nil {
		return err
	}
	defer closeRepo(repo)

	cfg := iops.config.Plakar

	// Route based on restore mode
	if cfg.SnapshotID != "" {
		// Single-component restore via --snapshot
		return iops.restoreSingleSnapshot(kctx, repo, cfg.SnapshotID, excludeTaskManager, restoreMigrateFormat)
	}

	// Backup-group restore (--backup-id or latest complete)
	var group *BackupGroupInfo
	if cfg.BackupID != "" {
		group, err = findBackupGroup(repo, cfg.BackupID)
		if err != nil {
			return err
		}
	} else {
		group, err = findLatestCompleteGroup(repo)
		if err != nil {
			return err
		}
	}

	// Check incomplete status
	if group.Status == StatusIncomplete {
		missing := missingComponents(group)
		logrus.Warnf("Backup group %s is incomplete (missing: %s)", group.BackupID, strings.Join(missing, ", "))
		if !force {
			return fmt.Errorf("backup group %s is incomplete (missing: %s); use --force to restore available components",
				group.BackupID, strings.Join(missing, ", "))
		}
	}

	logrus.WithFields(logrus.Fields{
		"backup_id":  group.BackupID,
		"status":     group.Status,
		"components": len(group.Snapshots),
	}).Info("Restoring from backup group")

	return iops.restoreBackupGroup(kctx, repo, group, excludeTaskManager, restoreMigrateFormat)
}

// restoreBackupGroup exports each component snapshot to a temp directory and restores.
// Neo4j community dumps are streamed directly from Plakar into the container.
func (iops *InfrahubOps) restoreBackupGroup(kctx *kcontext.KContext, repo *repository.Repository, group *BackupGroupInfo, excludeTaskManager bool, restoreMigrateFormat bool) error {
	// Create temp directory for extraction
	workDir, err := os.MkdirTemp("", "infrahub_plakar_restore_*")
	if err != nil {
		return fmt.Errorf("failed to create temp directory: %w", err)
	}
	defer os.RemoveAll(workDir)

	backupDir := filepath.Join(workDir, "backup")
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		return fmt.Errorf("failed to create backup directory: %w", err)
	}

	// Export each component snapshot, deferring neo4j for potential streaming
	var neo4jSnapInfo *SnapshotInfo
	for _, snapInfo := range group.Snapshots {
		if snapInfo.Component == ComponentNeo4j {
			si := snapInfo
			neo4jSnapInfo = &si
			continue
		}
		if excludeTaskManager && snapInfo.Component == ComponentPostgres {
			logrus.Info("Skipping postgres component restore as requested")
			continue
		}

		if err := iops.exportSnapshotToDir(kctx, repo, snapInfo, backupDir); err != nil {
			return err
		}
	}

	// Read metadata from the metadata component
	var metadata BackupMetadata
	metadataPath := filepath.Join(backupDir, "metadata", "backup_information.json")
	metadataBytes, err := os.ReadFile(metadataPath)
	if err != nil {
		// Try to construct minimal metadata from tags
		logrus.Warnf("Could not read backup metadata: %v; proceeding with group tags", err)
		metadata = BackupMetadata{
			InfrahubVersion: group.InfrahubVersion,
			Neo4jEdition:    group.Neo4jEdition,
			Components:      group.Components,
		}
	} else {
		if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
			return fmt.Errorf("failed to parse backup metadata: %w", err)
		}
	}

	logrus.WithFields(logrus.Fields{
		"backup_id":        group.BackupID,
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

	// For enterprise, export neo4j snapshot and extract the tar archive for file-based restore
	isCommunity := strings.EqualFold(neo4jEdition, neo4jEditionCommunity)
	if neo4jSnapInfo != nil && !isCommunity {
		if err := iops.exportSnapshotToDir(kctx, repo, *neo4jSnapInfo, backupDir); err != nil {
			return err
		}
		// The exported snapshot contains neo4j-backup.tar; extract it
		// into backupDir/database/ with the infrahubops/ prefix stripped.
		tarPath := filepath.Join(backupDir, "neo4j", "neo4j-backup.tar")
		databaseDir := filepath.Join(backupDir, "database")
		if err := extractNeo4jEnterpriseTar(tarPath, databaseDir); err != nil {
			return fmt.Errorf("failed to extract neo4j enterprise backup: %w", err)
		}
		os.RemoveAll(filepath.Join(backupDir, "neo4j"))
	}

	// PostgreSQL expects workDir/backup/prefect.dump — move from postgres component
	srcDump := filepath.Join(backupDir, "postgres", "prefect.dump")
	dstDump := filepath.Join(backupDir, "prefect.dump")
	if err := os.Rename(srcDump, dstDump); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to restructure postgres export: %w", err)
	}

	// Determine task manager availability
	taskManagerIncluded := false
	for _, snap := range group.Snapshots {
		if snap.Component == ComponentPostgres {
			taskManagerIncluded = true
			break
		}
	}

	shouldRestoreTaskManager := taskManagerIncluded && !excludeTaskManager
	prefectPath := filepath.Join(backupDir, "prefect.dump")
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
	if neo4jSnapInfo != nil && isCommunity {
		// Stream community dump directly from Plakar into the container
		snap, err := snapshot.Load(repo, neo4jSnapInfo.MAC)
		if err != nil {
			return fmt.Errorf("failed to load neo4j snapshot for streaming: %w", err)
		}
		reader, err := snap.NewReader("/neo4j.dump")
		if err != nil {
			snap.Close()
			return fmt.Errorf("failed to open neo4j dump stream from snapshot: %w", err)
		}
		err = iops.restoreNeo4jCommunityStream(reader, restoreMigrateFormat)
		reader.Close()
		snap.Close()
		if err != nil {
			return err
		}
	} else if neo4jSnapInfo != nil {
		if err := iops.restoreNeo4j(workDir, neo4jEdition, restoreMigrateFormat); err != nil {
			return err
		}
	}

	// Restart all services
	logrus.Info("Restarting Infrahub services...")
	if err := iops.StartServices("infrahub-server", "task-worker"); err != nil {
		return fmt.Errorf("failed to restart infrahub services: %w", err)
	}

	logrus.Info("Restore from Plakar backup group completed successfully")
	logrus.Info("Infrahub should be available shortly")

	return nil
}

// extractNeo4jEnterpriseTar extracts the neo4j enterprise backup tar archive
// (created by backupNeo4jEnterpriseStream) into databaseDir.
// The tar contains files under an "infrahubops/" prefix which is stripped.
func extractNeo4jEnterpriseTar(tarPath, databaseDir string) error {
	if err := os.MkdirAll(databaseDir, 0755); err != nil {
		return fmt.Errorf("failed to create database directory: %w", err)
	}
	return extractUncompressedTar(tarPath, databaseDir, 1)
}

// exportSnapshotToDir extracts a single component snapshot to a subdirectory of backupDir.
func (iops *InfrahubOps) exportSnapshotToDir(kctx *kcontext.KContext, repo *repository.Repository, snapInfo SnapshotInfo, backupDir string) error {
	snap, err := snapshot.Load(repo, snapInfo.MAC)
	if err != nil {
		return fmt.Errorf("failed to load %s snapshot: %w", snapInfo.Component, err)
	}
	defer snap.Close()

	componentDir := filepath.Join(backupDir, snapInfo.Component)
	if err := os.MkdirAll(componentDir, 0755); err != nil {
		return fmt.Errorf("failed to create directory for %s: %w", snapInfo.Component, err)
	}

	exp, err := exporter.NewExporter(kctx, &connectors.Options{MaxConcurrency: kctx.MaxConcurrency}, map[string]string{
		"location": "fs://" + componentDir,
	})
	if err != nil {
		return fmt.Errorf("failed to create exporter for %s: %w", snapInfo.Component, err)
	}

	logrus.Infof("Extracting %s snapshot...", snapInfo.Component)
	exportOpts := &snapshot.ExportOptions{
		SkipPermissions: true,
	}
	if err := snap.Export(exp, "/", exportOpts); err != nil {
		exp.Close(kctx.Context)
		return fmt.Errorf("failed to extract %s snapshot: %w", snapInfo.Component, err)
	}

	exp.Close(kctx.Context)
	return nil
}

// restoreSingleSnapshot restores from a single snapshot (--snapshot flag).
func (iops *InfrahubOps) restoreSingleSnapshot(kctx *kcontext.KContext, repo *repository.Repository, snapshotID string, excludeTaskManager bool, restoreMigrateFormat bool) error {
	snapshotMAC, err := resolveSnapshotID(repo, snapshotID)
	if err != nil {
		return err
	}

	snap, err := snapshot.Load(repo, snapshotMAC)
	if err != nil {
		return fmt.Errorf("failed to load plakar snapshot: %w", err)
	}
	defer snap.Close()

	logrus.WithFields(logrus.Fields{
		"snapshot_id": fmt.Sprintf("%x", snap.Header.Identifier[:8]),
		"date":        snap.Header.Timestamp.Format(time.RFC3339),
		"name":        snap.Header.Name,
	}).Info("Restoring from single Plakar snapshot")

	// Determine component type and edition from tags before deciding export strategy
	tags := parseSnapshotTags(snap.Header.Tags)
	component := tags[TagComponent]
	neo4jEdition := tags[TagNeo4jEdition]

	logrus.Infof("Restoring single component: %s", component)

	// Detect Neo4j edition for restore
	detectedEdition, detectionErr := iops.detectNeo4jEdition()
	editionInfo := NewNeo4jEditionInfo(detectedEdition, detectionErr)
	if neo4jEdition != "" {
		resolvedEdition, err := editionInfo.ResolveRestoreEdition(neo4jEdition)
		if err != nil {
			return err
		}
		neo4jEdition = resolvedEdition
	}

	// Neo4j community: stream directly from snapshot without exporting to disk
	if component == ComponentNeo4j && strings.EqualFold(neo4jEdition, neo4jEditionCommunity) {
		reader, err := snap.NewReader("/neo4j.dump")
		if err != nil {
			return fmt.Errorf("failed to open neo4j dump stream from snapshot: %w", err)
		}
		defer reader.Close()

		if _, err := iops.stopAppContainers(); err != nil {
			return err
		}
		if err := iops.restartDependencies(); err != nil {
			return err
		}
		if err := iops.restoreNeo4jCommunityStream(reader, restoreMigrateFormat); err != nil {
			return err
		}

		logrus.Info("Restarting Infrahub services...")
		if err := iops.StartServices("infrahub-server", "task-worker"); err != nil {
			return fmt.Errorf("failed to restart infrahub services: %w", err)
		}

		logrus.Info("Restore from Plakar snapshot completed successfully")
		return nil
	}

	// All other components: export to temp directory first
	workDir, err := os.MkdirTemp("", "infrahub_plakar_restore_*")
	if err != nil {
		return fmt.Errorf("failed to create temp directory: %w", err)
	}
	defer os.RemoveAll(workDir)

	exportDir := filepath.Join(workDir, "backup")
	if err := os.MkdirAll(exportDir, 0755); err != nil {
		return fmt.Errorf("failed to create export directory: %w", err)
	}

	exp, err := exporter.NewExporter(kctx, &connectors.Options{MaxConcurrency: kctx.MaxConcurrency}, map[string]string{
		"location": "fs://" + exportDir,
	})
	if err != nil {
		return fmt.Errorf("failed to create plakar exporter: %w", err)
	}
	defer exp.Close(kctx.Context)

	logrus.Info("Extracting Plakar snapshot...")
	exportOpts := &snapshot.ExportOptions{
		SkipPermissions: true,
	}
	if err := snap.Export(exp, "/", exportOpts); err != nil {
		return fmt.Errorf("failed to extract plakar snapshot: %w", err)
	}

	switch component {
	case ComponentNeo4j:
		// Enterprise: the exported snapshot contains neo4j-backup.tar;
		// extract it into exportDir/database/ with infrahubops/ prefix stripped.
		tarPath := filepath.Join(exportDir, "neo4j-backup.tar")
		databaseDir := filepath.Join(exportDir, "database")
		if err := extractNeo4jEnterpriseTar(tarPath, databaseDir); err != nil {
			return fmt.Errorf("failed to extract neo4j enterprise backup: %w", err)
		}
		os.Remove(tarPath)

		// Stop services and restore
		if _, err := iops.stopAppContainers(); err != nil {
			return err
		}
		if err := iops.restartDependencies(); err != nil {
			return err
		}
		if err := iops.restoreNeo4j(workDir, neo4jEdition, restoreMigrateFormat); err != nil {
			return err
		}

	case ComponentPostgres:
		if excludeTaskManager {
			logrus.Info("Skipping postgres restore as requested")
			return nil
		}
		// Move prefect.dump to expected location
		srcDump := filepath.Join(exportDir, "prefect.dump")
		if _, err := os.Stat(srcDump); os.IsNotExist(err) {
			return fmt.Errorf("postgres snapshot does not contain prefect.dump")
		}
		if _, err := iops.stopAppContainers(); err != nil {
			return err
		}
		if err := iops.restorePostgreSQL(workDir); err != nil {
			return err
		}

	case ComponentMetadata:
		logrus.Info("Metadata-only snapshot — nothing to restore to containers")
		return nil

	default:
		return fmt.Errorf("unknown component type in snapshot: %s", component)
	}

	// Restart services
	logrus.Info("Restarting Infrahub services...")
	if err := iops.StartServices("infrahub-server", "task-worker"); err != nil {
		return fmt.Errorf("failed to restart infrahub services: %w", err)
	}

	logrus.Info("Restore from Plakar snapshot completed successfully")
	return nil
}

// snapshotLister is an interface for listing snapshots in a repository.
type snapshotLister interface {
	ListSnapshots() iter.Seq2[objects.MAC, error]
}

// resolveSnapshotID resolves a snapshot identifier (partial hex or empty for latest).
func resolveSnapshotID(repo snapshotLister, snapshotID string) (objects.MAC, error) {
	if snapshotID == "" {
		// Find the latest snapshot
		var latest objects.MAC
		found := false
		for mac, err := range repo.ListSnapshots() {
			if err != nil {
				return objects.MAC{}, fmt.Errorf("failed to list snapshots: %w", err)
			}
			latest = mac
			found = true
		}
		if !found {
			return objects.MAC{}, fmt.Errorf("no snapshots found in repository")
		}
		return latest, nil
	}

	// Validate hex format
	if _, err := hex.DecodeString(snapshotID); err != nil {
		return objects.MAC{}, fmt.Errorf("invalid snapshot ID %q: not valid hex", snapshotID)
	}

	var matched objects.MAC
	matchCount := 0
	var available []string
	prefix := strings.ToLower(snapshotID)

	for mac, err := range repo.ListSnapshots() {
		if err != nil {
			return objects.MAC{}, fmt.Errorf("failed to list snapshots: %w", err)
		}
		macHex := hex.EncodeToString(mac[:])
		available = append(available, fmt.Sprintf("%x", mac[:8]))
		if strings.HasPrefix(macHex, prefix) {
			matched = mac
			matchCount++
		}
	}

	if matchCount == 0 {
		if len(available) > 0 {
			return objects.MAC{}, fmt.Errorf("snapshot not found: %s\nAvailable snapshots: %s", snapshotID, strings.Join(available, ", "))
		}
		return objects.MAC{}, fmt.Errorf("snapshot not found: %s (repository contains no snapshots)", snapshotID)
	}

	if matchCount > 1 {
		return objects.MAC{}, fmt.Errorf("ambiguous snapshot ID %q: matches %d snapshots; provide a longer prefix", snapshotID, matchCount)
	}

	return matched, nil
}
