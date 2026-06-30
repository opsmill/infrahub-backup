package app

import (
	"encoding/hex"
	"fmt"
	"iter"
	"net/url"
	"strings"
	"time"

	"github.com/PlakarKorp/kloset/objects"
	"github.com/PlakarKorp/kloset/snapshot"
	"github.com/sirupsen/logrus"
)

// RestorePlakarBackup restores an Infrahub deployment from Plakar snapshots by
// driving the upstream connectors' exporters in a co-located runner (see
// runner.go). Supports: single snapshot (--snapshot), a specific backup group
// (--backup-id), or the latest complete group (default).
func (iops *InfrahubOps) RestorePlakarBackup(excludeTaskManager bool, restoreMigrateFormat bool, sleepDuration time.Duration, force bool, resetDeploymentID bool) error {
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

	project := iops.config.DockerComposeProject
	if project == "" {
		return fmt.Errorf("the plakar runner restore currently supports Docker Compose only; Kubernetes support is pending")
	}

	editionInfo := iops.detectNeo4jEditionInfo("restore")

	if restoreMigrateFormat {
		logrus.Warn("--migrate-format is not yet supported by the runner restore; ignoring")
	}
	if resetDeploymentID {
		logrus.Warn("--reset-deployment-id is not yet supported by the runner restore; ignoring")
	}

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

	if err := iops.fetchDatabaseCredentials(); err != nil {
		return fmt.Errorf("failed to fetch database credentials: %w", err)
	}
	repoPath, err := iops.runnerRepoPath()
	if err != nil {
		return err
	}

	cfg := iops.config.Plakar

	// Single-snapshot restore (--snapshot)
	if cfg.SnapshotID != "" {
		mac, err := resolveSnapshotID(repo, cfg.SnapshotID)
		if err != nil {
			return err
		}
		snap, err := snapshot.Load(repo, mac)
		if err != nil {
			return fmt.Errorf("failed to load snapshot: %w", err)
		}
		component := parseSnapshotTags(snap.Header.Tags)[TagComponent]
		snap.Close()
		logrus.Infof("Restoring single component: %s", component)
		if err := iops.restoreComponentViaRunner(project, repoPath, component, fmt.Sprintf("%x", mac[:]), editionInfo.IsCommunity, excludeTaskManager); err != nil {
			return err
		}
		logrus.Info("Restore from Plakar snapshot completed successfully")
		return nil
	}

	// Group restore (--backup-id or latest complete)
	var group *BackupGroupInfo
	if cfg.BackupID != "" {
		group, err = findBackupGroup(repo, cfg.BackupID)
	} else {
		group, err = findLatestCompleteGroup(repo)
	}
	if err != nil {
		return err
	}

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

	for _, snapInfo := range group.Snapshots {
		if err := iops.restoreComponentViaRunner(project, repoPath, snapInfo.Component, fmt.Sprintf("%x", snapInfo.MAC[:]), editionInfo.IsCommunity, excludeTaskManager); err != nil {
			return err
		}
	}

	logrus.Info("Restore from Plakar backup group completed successfully")
	logrus.Info("Infrahub should be available shortly")
	return nil
}

// restoreComponentViaRunner restores one component by driving its connector
// exporter in a co-located runner, with the lifecycle each engine needs.
func (iops *InfrahubOps) restoreComponentViaRunner(project, repoPath, component, snapHex string, community, excludeTaskManager bool) (retErr error) {
	switch component {
	case ComponentNeo4j:
		// Stop the writer so neo4j-admin can replace the store, run the exporter in
		// a runner sharing the (now-quiesced) data volume, then restart. The default
		// database already exists in the catalog, so --overwrite-destination replaces
		// its store; no CREATE DATABASE. Enterprise restores from the backup artifact
		// (neo4j://); Community loads the offline dump (neo4j+offline://).
		var uri string
		if community {
			uri = "neo4j+offline:///data?database=" + url.QueryEscape(iops.config.Neo4jDatabase)
		} else {
			uri = dbURI("neo4j", iops.config.Neo4jUsername, iops.config.Neo4jPassword, "database", "6362", iops.config.Neo4jDatabase)
		}
		logrus.Info("Stopping Neo4j for offline restore...")
		if err := iops.StopServices("database"); err != nil {
			return fmt.Errorf("failed to stop neo4j: %w", err)
		}
		defer func() {
			logrus.Info("Restarting Neo4j...")
			if err := iops.StartServices("database"); err != nil && retErr == nil {
				retErr = fmt.Errorf("failed to restart neo4j: %w", err)
			}
		}()
		opts := map[string]string{"neo4j_bin_dir": "/var/lib/neo4j/bin", "overwrite": "true"}
		if err := LaunchComposeRestore(project, "database", repoPath, uri, snapHex, iops.config.Plakar.Passphrase, opts, true); err != nil {
			return fmt.Errorf("neo4j restore failed: %w", err)
		}
		logrus.Info("Neo4j restore completed")
		return nil

	case ComponentPostgres:
		if excludeTaskManager {
			logrus.Info("Skipping postgres restore as requested")
			return nil
		}
		uri := dbURI("postgres", iops.config.PostgresUsername, iops.config.PostgresPassword, "task-manager-db", "5432", iops.config.PostgresDatabase)
		opts := map[string]string{"clean": "true"}
		if err := LaunchComposeRestore(project, "task-manager-db", repoPath, uri, snapHex, iops.config.Plakar.Passphrase, opts, false); err != nil {
			return fmt.Errorf("postgres restore failed: %w", err)
		}
		logrus.Info("Postgres restore completed")
		return nil

	case ComponentMetadata:
		logrus.Info("Metadata component — nothing to restore to containers")
		return nil

	default:
		return fmt.Errorf("unknown component type in snapshot: %s", component)
	}
}

// snapshotLister is an interface for listing snapshots in a repository.
type snapshotLister interface {
	ListSnapshots() iter.Seq2[objects.MAC, error]
}

// resolveSnapshotID resolves a snapshot identifier (partial hex or empty for latest).
func resolveSnapshotID(repo snapshotLister, snapshotID string) (objects.MAC, error) {
	if snapshotID == "" {
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
