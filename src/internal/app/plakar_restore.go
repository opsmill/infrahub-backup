package app

import (
	"encoding/hex"
	"fmt"
	"iter"
	"net/url"
	"strings"
	"time"

	"github.com/PlakarKorp/kloset/objects"
	"github.com/PlakarKorp/kloset/repository"
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

	var plan restorePlan
	if cfg.SnapshotID != "" {
		plan, err = singleSnapshotPlan(repo, cfg.SnapshotID)
	} else {
		plan, err = backupGroupPlan(repo, cfg.BackupID, force)
	}
	if err != nil {
		return err
	}
	if err := plan.validate(); err != nil {
		return err
	}

	// The edition the backup was taken with decides which Neo4j artifact shape and
	// connector this restore has to use, so it is reconciled with the target rather
	// than assumed from live detection alone.
	community, err := iops.resolveRestoreCommunity(plan.backupEdition)
	if err != nil {
		return err
	}

	// A format migration has to run in the same offline window as the load, so it
	// travels to the runner rather than being run from here. With no Neo4j component
	// to migrate it is said to be ignored rather than quietly dropped.
	var migrate Neo4jMigration
	if restoreMigrateFormat {
		if !plan.hasComponent(ComponentNeo4j) {
			logrus.Warn("--migrate-format ignored: no Neo4j component in this restore")
		} else {
			migrate = Neo4jMigration{Format: neo4jBlockFormat, Database: iops.config.Neo4jDatabase}
		}
	}

	restoreComponent := func(snapInfo SnapshotInfo) error {
		return iops.restoreComponentViaRunner(project, repoPath, snapInfo.Component,
			fmt.Sprintf("%x", snapInfo.MAC[:]), community, excludeTaskManager, migrate)
	}
	if err := iops.restoreComponents(plan, restoreComponent, resetDeploymentID); err != nil {
		return err
	}

	logrus.Infof("Restore from Plakar %s completed successfully", plan.describe())
	logrus.Info("Infrahub should be available shortly")
	return nil
}

// restorePlan is what one restore invocation resolved to: the component snapshots
// to apply and the Neo4j edition recorded in the backup.
type restorePlan struct {
	// backupID is set for a group restore, snapshotID for a single-snapshot one.
	backupID   string
	snapshotID string
	// backupEdition is the Neo4j edition tag recorded when the backup was taken.
	// Empty when the snapshot predates the tag.
	backupEdition string
	snapshots     []SnapshotInfo
}

// neo4jBlockFormat is the store format --migrate-format migrates to, matching what
// main passed to `neo4j-admin database migrate --to-format=`.
const neo4jBlockFormat = "block"

// hasComponent reports whether the plan includes a snapshot of the named component.
func (p restorePlan) hasComponent(component string) bool {
	for _, snapInfo := range p.snapshots {
		if snapInfo.Component == component {
			return true
		}
	}
	return false
}

func (p restorePlan) describe() string {
	if p.backupID != "" {
		return "backup group " + p.backupID
	}
	return "snapshot " + p.snapshotID
}

// validate refuses a plan naming a component this tool cannot restore. It matters
// because restoreComponents dispatches by component in a fixed order: an unknown
// name would otherwise be silently skipped rather than reported.
func (p restorePlan) validate() error {
	for _, snapInfo := range p.snapshots {
		switch snapInfo.Component {
		case ComponentNeo4j, ComponentPostgres, ComponentMetadata:
		default:
			return fmt.Errorf("unknown component type in snapshot: %s", snapInfo.Component)
		}
	}
	if len(p.snapshots) == 0 {
		return fmt.Errorf("nothing to restore: %s contains no component snapshots", p.describe())
	}
	return nil
}

// singleSnapshotPlan resolves --snapshot to a one-component plan.
func singleSnapshotPlan(repo *repository.Repository, snapshotID string) (restorePlan, error) {
	mac, err := resolveSnapshotID(repo, snapshotID)
	if err != nil {
		return restorePlan{}, err
	}
	snap, err := snapshot.Load(repo, mac)
	if err != nil {
		return restorePlan{}, fmt.Errorf("failed to load snapshot: %w", err)
	}
	tags := parseSnapshotTags(snap.Header.Tags)
	snap.Close()

	shortID := fmt.Sprintf("%x", mac[:8])
	plan := restorePlan{
		snapshotID:    shortID,
		backupEdition: tags[TagNeo4jEdition],
		snapshots: []SnapshotInfo{{
			SnapshotID: shortID,
			Component:  tags[TagComponent],
			MAC:        mac,
		}},
	}
	logrus.Infof("Restoring single component: %s", plan.snapshots[0].Component)
	return plan, nil
}

// backupGroupPlan resolves --backup-id (or the latest complete group) to a plan.
func backupGroupPlan(repo *repository.Repository, backupID string, force bool) (restorePlan, error) {
	var group *BackupGroupInfo
	var err error
	if backupID != "" {
		group, err = findBackupGroup(repo, backupID)
	} else {
		group, err = findLatestCompleteGroup(repo)
	}
	if err != nil {
		return restorePlan{}, err
	}

	if group.Status == StatusIncomplete {
		missing := missingComponents(group)
		logrus.Warnf("Backup group %s is incomplete (missing: %s)", group.BackupID, strings.Join(missing, ", "))
		if !force {
			return restorePlan{}, fmt.Errorf("backup group %s is incomplete (missing: %s); use --force to restore available components",
				group.BackupID, strings.Join(missing, ", "))
		}
	}

	logrus.WithFields(logrus.Fields{
		"backup_id":  group.BackupID,
		"status":     group.Status,
		"components": len(group.Snapshots),
	}).Info("Restoring from backup group")

	return restorePlan{
		backupID:      group.BackupID,
		backupEdition: group.Neo4jEdition,
		snapshots:     group.Snapshots,
	}, nil
}

// resolveRestoreCommunity decides which Neo4j restore path to take, by reconciling
// the edition recorded in the backup with the edition detected on the target.
//
// Live detection alone is not safe to route on: when detectNeo4jEdition fails for
// any reason — cypher-shell missing, an auth hiccup, the database mid-restart —
// NewNeo4jEditionInfo defaults to Community after only an Infof. An Enterprise
// restore would then drive neo4j+offline:///data against an Enterprise `.backup`
// artifact: wrong connector, wrong artifact shape. ResolveRestoreEdition (which
// main used here) instead turns that combination into a refusal, and keeps the one
// cross-edition restore that does work — a Community backup onto Enterprise.
func (iops *InfrahubOps) resolveRestoreCommunity(backupEdition string) (bool, error) {
	info := iops.detectNeo4jEditionInfo("restore")
	if backupEdition == "" {
		// Snapshots predating the edition tag carry nothing to reconcile against;
		// detection is all there is.
		logrus.Warn("Backup records no Neo4j edition; using the detected edition")
		return info.IsCommunity, nil
	}
	edition, err := info.ResolveRestoreEdition(backupEdition)
	if err != nil {
		return false, err
	}
	return strings.EqualFold(edition, neo4jEditionCommunity), nil
}

// appServices are the Infrahub services stopped for the duration of a restore, in
// the wording stopAppContainers uses.
var appServices = []string{
	"infrahub-server", "task-worker", "task-manager",
	"task-manager-background-svc", "cache", "message-queue",
}

// restoreComponents applies a plan with the deployment lifecycle a restore needs:
// quiesce the application tier, replace the databases, then bring it back.
//
// Without this the plakar restore replaced the databases underneath a running
// deployment: pg_restore's DROPs contended with the task manager's open sessions
// on `prefect`, Redis and RabbitMQ kept state describing the database that had
// just been replaced, and infrahub-server / task-worker spent the restore talking
// to a stopped Neo4j and were never restarted. main did all four steps; only
// StopServices("database") survived the move to the runner.
//
// The order is main's: transient state is wiped through the containers that hold
// it (so before they are stopped), the task-manager database is restored while
// nothing is connected to it, its dependencies come back, and Neo4j is replaced
// last. restoreComponent applies one component snapshot; it is a parameter so the
// lifecycle can be exercised without launching runners.
func (iops *InfrahubOps) restoreComponents(plan restorePlan, restoreComponent func(SnapshotInfo) error, resetDeploymentID bool) error {
	if err := iops.wipeTransientData(); err != nil {
		return err
	}
	if _, err := iops.stopAppContainers(); err != nil {
		return err
	}

	appsRestarted := false
	defer func() {
		if !appsRestarted {
			logrus.Warnf("Infrahub application services were left stopped by the failed restore (%s); "+
				"start them once the state of the deployment is understood", strings.Join(appServices, ", "))
		}
	}()

	// ComponentMetadata restores nothing to the containers, but it is dispatched
	// like the others so an unexpected component cannot be silently skipped.
	restoredNeo4j := false
	for _, phase := range []string{ComponentPostgres, ComponentNeo4j, ComponentMetadata} {
		for _, snapInfo := range plan.snapshots {
			if snapInfo.Component != phase {
				continue
			}
			if err := restoreComponent(snapInfo); err != nil {
				return err
			}
			if phase == ComponentNeo4j {
				restoredNeo4j = true
			}
		}
		if phase == ComponentPostgres {
			// cache, message-queue and the task manager come back on the wiped state
			// before Neo4j is replaced, as they did on main.
			if err := iops.restartDependencies(); err != nil {
				return err
			}
		}
	}

	// The new Root UUID is written before the application containers restart,
	// because they read and cache the old one on startup. The Neo4j restore has
	// already restarted the database and waited for Bolt by this point, which is
	// what resetDeploymentID needs.
	if resetDeploymentID {
		if !restoredNeo4j {
			logrus.Warn("--reset-deployment-id ignored: no Neo4j component was restored")
		} else if err := iops.resetDeploymentID(); err != nil {
			return err
		}
	}

	logrus.Info("Restarting Infrahub services...")
	if err := iops.StartServices("infrahub-server", "task-worker"); err != nil {
		return fmt.Errorf("failed to restart infrahub services: %w", err)
	}
	appsRestarted = true
	return nil
}

// restoreComponentViaRunner restores one component by driving its connector
// exporter in a co-located runner, with the lifecycle each engine needs.
func (iops *InfrahubOps) restoreComponentViaRunner(project, repoPath, component, snapHex string, community, excludeTaskManager bool, migrate Neo4jMigration) (retErr error) {
	switch component {
	case ComponentNeo4j:
		// Stop the writer so neo4j-admin can replace the store, run the exporter in
		// a runner sharing the (now-quiesced) data volume, then restart. The default
		// database already exists in the catalog, so --overwrite-destination replaces
		// its store; no CREATE DATABASE. Enterprise restores from the backup artifact
		// (neo4j://); Community loads the offline dump (neo4j+offline://).
		var uri string
		var dbPassword string
		if community {
			uri = "neo4j+offline:///data?database=" + url.QueryEscape(iops.config.Neo4jDatabase)
		} else {
			uri = dbURI("neo4j", iops.config.Neo4jUsername, "database", "6362", iops.config.Neo4jDatabase)
			dbPassword = iops.config.Neo4jPassword
		}
		logrus.Info("Stopping Neo4j for offline restore...")
		if err := iops.StopServices("database"); err != nil {
			return fmt.Errorf("failed to stop neo4j: %w", err)
		}
		defer func() {
			logrus.Info("Restarting Neo4j...")
			if err := iops.StartServices("database"); err != nil {
				if retErr == nil {
					retErr = fmt.Errorf("failed to restart neo4j: %w", err)
				}
				return
			}
			// "Infrahub should be available shortly" is only true once the database is
			// answering again; wait for that rather than asserting it.
			if err := iops.waitForNeo4jBolt(neo4jBoltReadyTimeout); err != nil {
				logrus.Warnf("Restore completed, but %v", err)
			}
		}()
		opts := map[string]string{"neo4j_bin_dir": neo4jRunnerBinDir, "overwrite": "true"}
		if err := LaunchComposeRestore(project, "database", repoPath, uri, snapHex, iops.runnerCredentials(dbPassword), opts, true, migrate); err != nil {
			return fmt.Errorf("neo4j restore failed: %w", err)
		}
		logrus.Info("Neo4j restore completed")
		return nil

	case ComponentPostgres:
		if excludeTaskManager {
			logrus.Info("Skipping postgres restore as requested")
			return nil
		}
		uri := dbURI("postgres", iops.config.PostgresUsername, "task-manager-db", "5432", iops.config.PostgresDatabase)
		creds := iops.runnerCredentials(iops.config.PostgresPassword)
		if err := LaunchComposeRestore(project, "task-manager-db", repoPath, uri, snapHex, creds, postgresRestoreOpts(), false, Neo4jMigration{}); err != nil {
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

// postgresRestoreOpts are the connector options for restoring the task-manager
// database, chosen to reproduce what main ran: `pg_restore --clean --create`.
//
//   - recreate maps to `-C --clean --if-exists`, i.e. drop and recreate the
//     database from the archive's own metadata. The weaker `clean` (which is only
//     `--clean --if-exists`) drops just the objects the dump contains, so rolling
//     back to an older Prefect schema left every table, column and enum value
//     added since the backup in place — nothing in the dump names them — and
//     Prefect then ran against a hybrid schema. It also never reapplied the
//     database-level properties (owner, encoding, collation, per-database SET
//     options) that `--create` carries.
//   - no_globals skips feeding the archive's 00000-globals.sql to psql. Replaying
//     it would run CREATE/ALTER ROLE against the whole cluster — a side effect
//     main never had, and one that reaches beyond the database being restored.
//
// clean and recreate are mutually exclusive in the connector, so only recreate is
// set.
func postgresRestoreOpts() map[string]string {
	return map[string]string{"recreate": "true", "no_globals": "true"}
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
