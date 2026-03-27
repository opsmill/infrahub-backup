package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/PlakarKorp/kloset/objects"
	"github.com/PlakarKorp/kloset/repository"
	"github.com/PlakarKorp/kloset/snapshot"
	"github.com/sirupsen/logrus"
)

// componentBackup holds the result of a single component snapshot creation.
type componentBackup struct {
	component string
	mac       objects.MAC
}

// CreatePlakarBackup creates an Infrahub backup as multiple Plakar snapshots (one per component),
// streaming database dumps directly from container exec stdout into kloset.
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

	// Generate backup-id timestamp
	backupID := time.Now().Format("20060102_150405")

	logrus.WithFields(logrus.Fields{
		"repo":          iops.config.Plakar.RepoPath,
		"neo4j_edition": editionInfo.Edition,
		"backup_id":     backupID,
	}).Info("Creating Plakar streaming backup")

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

	hostname := kctx.Hostname

	// Build component list
	components := []string{ComponentNeo4j}
	if !excludeTaskManager {
		components = append(components, ComponentPostgres)
	}
	components = append(components, ComponentMetadata)

	// Track completed snapshots for partial failure handling
	var completed []componentBackup

	// Generate backup metadata for the metadata component
	metadataObj := iops.createBackupMetadata(
		fmt.Sprintf("infrahub_backup_%s", backupID),
		!excludeTaskManager, version, editionInfo.Edition,
	)
	if redact {
		metadataObj.Redacted = true
	}
	// Override components to use Plakar naming (neo4j, postgres, metadata)
	// instead of the tarball naming (database, task-manager-db) from createBackupMetadata
	metadataObj.Components = components

	// Create one snapshot per component
	for _, component := range components {
		logrus.Infof("Creating snapshot for component: %s", component)

		var imp *StreamingImporter
		switch component {
		case ComponentNeo4j:
			dataFunc, err := iops.neo4jStreamFactory(editionInfo.Edition, neo4jMetadata)
			if err != nil {
				logIncompleteBackup(completed, len(components), backupID)
				return fmt.Errorf("failed to prepare neo4j stream: %w", err)
			}
			now := time.Now()
			fi := objects.NewFileInfo("/neo4j-backup.tar", 0, 0644, now, 0, 0, 0, 0, 0)
			if editionInfo.IsCommunity {
				fi = objects.NewFileInfo("/neo4j.dump", 0, 0644, now, 0, 0, 0, 0, 0)
			}
			imp = NewStreamingImporter(hostname, fi.Name(), fi, dataFunc)

		case ComponentPostgres:
			dataFunc, err := iops.postgresStreamFactory()
			if err != nil {
				logIncompleteBackup(completed, len(components), backupID)
				return fmt.Errorf("failed to prepare postgres stream: %w", err)
			}
			now := time.Now()
			fi := objects.NewFileInfo("/prefect.dump", 0, 0644, now, 0, 0, 0, 0, 0)
			imp = NewStreamingImporter(hostname, "/prefect.dump", fi, dataFunc)

		case ComponentMetadata:
			metadataBytes, err := json.MarshalIndent(metadataObj, "", "    ")
			if err != nil {
				logIncompleteBackup(completed, len(components), backupID)
				return fmt.Errorf("failed to marshal metadata: %w", err)
			}
			imp = NewMemoryImporter(hostname, "/backup_information.json", metadataBytes)
		}

		// Create snapshot
		tags := buildSnapshotTags(metadataObj, component, backupID, StatusComplete)
		builderOpts := &snapshot.BuilderOptions{
			Name: fmt.Sprintf("%s_%s", backupID, component),
			Tags: tags,
		}
		builder, err := snapshot.Create(repo, repository.DefaultType, os.TempDir(), objects.NilMac, builderOpts)
		if err != nil {
			logIncompleteBackup(completed, len(components), backupID)
			return fmt.Errorf("failed to create snapshot for %s: %w", component, err)
		}

		source, err := snapshot.NewSource(context.Background(), 0, imp)
		if err != nil {
			builder.Close()
			logIncompleteBackup(completed, len(components), backupID)
			return fmt.Errorf("failed to create source for %s: %w", component, err)
		}

		if err := builder.Backup(source); err != nil {
			builder.Close()
			logIncompleteBackup(completed, len(components), backupID)
			return fmt.Errorf("streaming backup failed for %s: %w", component, err)
		}

		if err := builder.Commit(); err != nil {
			builder.Close()
			logIncompleteBackup(completed, len(components), backupID)
			return fmt.Errorf("failed to commit snapshot for %s: %w", component, err)
		}

		mac := builder.Header.Identifier
		builder.Close()

		completed = append(completed, componentBackup{component: component, mac: mac})
		logrus.WithFields(logrus.Fields{
			"component":   component,
			"snapshot_id": fmt.Sprintf("%x", mac[:8]),
		}).Info("Component snapshot created")
	}

	logrus.WithFields(logrus.Fields{
		"backup_id":  backupID,
		"components": len(completed),
		"repo":       iops.config.Plakar.RepoPath,
	}).Info("Plakar streaming backup completed successfully")

	// Sleep if requested
	if sleepDuration > 0 {
		logrus.Infof("Sleeping for %v to allow backup file transfer...", sleepDuration)
		time.Sleep(sleepDuration)
	}

	return nil
}

// neo4jStreamFactory returns a data factory for streaming Neo4j backup data.
func (iops *InfrahubOps) neo4jStreamFactory(edition string, backupMetadata string) (func() (io.ReadCloser, error), error) {
	switch strings.ToLower(edition) {
	case neo4jEditionCommunity:
		return iops.backupNeo4jCommunityStream()
	default:
		return iops.backupNeo4jEnterpriseStream(backupMetadata)
	}
}

// postgresStreamFactory returns a data factory for streaming PostgreSQL backup data.
func (iops *InfrahubOps) postgresStreamFactory() (func() (io.ReadCloser, error), error) {
	return iops.backupTaskManagerDBStream()
}

// logIncompleteBackup warns about a partial backup failure.
// Note: kloset doesn't support modifying tags after snapshot creation,
// so the group's incomplete status is derived at query time from missing components.
func logIncompleteBackup(completed []componentBackup, totalExpected int, backupID string) {
	if len(completed) == 0 {
		return
	}
	names := make([]string, len(completed))
	for i, c := range completed {
		names[i] = c.component
	}
	logrus.Warnf("Backup group %s is incomplete (%d/%d components created: %s)",
		backupID, len(completed), totalExpected, strings.Join(names, ", "))
}

// buildSnapshotTags creates Plakar snapshot tags for a component snapshot.
func buildSnapshotTags(metadata *BackupMetadata, component, backupID, status string) []string {
	tags := []string{
		TagBackupID + "=" + backupID,
		TagComponent + "=" + component,
		TagBackupStatus + "=" + status,
		TagVersion + "=" + metadata.InfrahubVersion,
		TagToolVersion + "=" + metadata.ToolVersion,
		TagNeo4jEdition + "=" + metadata.Neo4jEdition,
		TagComponents + "=" + strings.Join(metadata.Components, ","),
	}
	if metadata.Redacted {
		tags = append(tags, TagRedacted+"=true")
	}
	return tags
}
