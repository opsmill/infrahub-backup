package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/PlakarKorp/kloset/objects"
	"github.com/PlakarKorp/kloset/repository"
	"github.com/PlakarKorp/kloset/snapshot"
	"github.com/sirupsen/logrus"
)

// errRedactRequiresForce is returned when --redact is used without --force.
var errRedactRequiresForce = errors.New(
	"--redact is a destructive operation that replaces all attribute values in the database with random UUIDs; use --force to confirm")

// prepareRepoBeforeRedact runs the repository preparation and, when asked for, the
// redaction — in that order, and only in that order.
//
// Redaction rewrites every attribute value in the LIVE database and is
// irreversible: the pre-redaction data only survives in the backup this run is
// about to take. So the repository has to be proven usable first — for an
// encrypted repository that means the passphrase passing the canary check inside
// newRepository. Redacting before opening the repository is how a mistyped or
// unset INFRAHUB_BACKUP_PASSPHRASE destroyed the data and then failed to write
// any backup of it.
func prepareRepoBeforeRedact(redact, force bool, prepareRepo, redactDatabase func() error) error {
	if !redact {
		return prepareRepo()
	}
	if !force {
		return errRedactRequiresForce
	}
	if err := prepareRepo(); err != nil {
		return err
	}
	return redactDatabase()
}

// CreatePlakarBackup creates an Infrahub backup as multiple Plakar snapshots
// (one per component). Database components are captured by the upstream
// connectors running in a co-located one-shot runner (see runner.go); the
// metadata component is written in-process by the host tool (it can reach the
// kloset repository directly).
func (iops *InfrahubOps) CreatePlakarBackup(force bool, neo4jMetadata string, excludeTaskManager bool, sleepDuration time.Duration, redact bool) (retErr error) {
	if err := iops.checkPrerequisites(); err != nil {
		return err
	}
	if err := iops.DetectEnvironment(); err != nil {
		return err
	}

	project := iops.config.DockerComposeProject
	if project == "" {
		return fmt.Errorf("the plakar runner backend currently supports Docker Compose only; Kubernetes support is pending")
	}

	editionInfo := iops.detectNeo4jEditionInfo("backup")

	// The repository is prepared here rather than just before the first snapshot so
	// that a redaction — which destroys the live data — cannot run against a
	// repository that turns out to be unopenable.
	prepareRepo := func() error {
		if err := iops.ensurePlakarRepo(); err != nil {
			return fmt.Errorf("failed to prepare plakar repository: %w", err)
		}
		return nil
	}
	if err := prepareRepoBeforeRedact(redact, force, prepareRepo, iops.redactDatabase); err != nil {
		return err
	}

	version := iops.getInfrahubVersion()

	if !force {
		logrus.Info("Checking for running tasks before backup...")
		if err := iops.waitForRunningTasks(); err != nil {
			return err
		}
	}

	// Database credentials feed the connector URIs handed to the runner.
	if err := iops.fetchDatabaseCredentials(); err != nil {
		return fmt.Errorf("failed to fetch database credentials: %w", err)
	}

	backupID := time.Now().Format("20060102_150405")
	repoPath, err := iops.runnerRepoPath()
	if err != nil {
		return err
	}

	logrus.WithFields(logrus.Fields{
		"repo":          iops.config.Plakar.RepoPath,
		"neo4j_edition": editionInfo.Edition,
		"backup_id":     backupID,
		"project":       project,
	}).Info("Creating Plakar backup via co-located runner")

	components := []string{ComponentNeo4j}
	if !excludeTaskManager {
		components = append(components, ComponentPostgres)
	}
	components = append(components, ComponentMetadata)

	metadataObj := iops.createBackupMetadata(
		fmt.Sprintf("infrahub_backup_%s", backupID),
		!excludeTaskManager, version, editionInfo.Edition,
	)
	if redact {
		metadataObj.Redacted = true
	}
	metadataObj.Components = components

	var completed []string
	for _, component := range components {
		logrus.Infof("Creating snapshot for component: %s", component)
		tags := buildSnapshotTags(metadataObj, component, backupID, StatusComplete)

		var snapHex string
		var cerr error
		switch component {
		case ComponentNeo4j:
			snapHex, cerr = iops.backupNeo4jComponent(project, repoPath, neo4jMetadata, editionInfo.IsCommunity, tags)

		case ComponentPostgres:
			uri := dbURI("postgres", iops.config.PostgresUsername, "task-manager-db", "5432", iops.config.PostgresDatabase)
			creds := iops.runnerCredentials(iops.config.PostgresPassword)
			snapHex, cerr = LaunchComposeBackup(project, "task-manager-db", repoPath, uri, creds, map[string]string{"compress": "false"}, tags, false)

		case ComponentMetadata:
			snapHex, cerr = iops.writeMetadataSnapshot(metadataObj, tags)
		}

		if cerr != nil {
			logIncompleteBackup(completed, len(components), backupID)
			return fmt.Errorf("backup failed for %s: %w", component, cerr)
		}

		completed = append(completed, component)
		logrus.WithFields(logrus.Fields{
			"component":   component,
			"snapshot_id": snapHex,
		}).Info("Component snapshot created")
	}

	logrus.WithFields(logrus.Fields{
		"backup_id":  backupID,
		"components": len(completed),
		"repo":       iops.config.Plakar.RepoPath,
	}).Info("Plakar backup completed successfully")

	if sleepDuration > 0 {
		logrus.Infof("Sleeping for %v to allow backup file transfer...", sleepDuration)
		time.Sleep(sleepDuration)
	}

	return nil
}

// backupNeo4jComponent captures the Neo4j component. Enterprise uses an online
// backup over the backup port; Community stops the writer, runs an offline dump
// in a runner sharing the (quiesced) data volume, then restarts.
func (iops *InfrahubOps) backupNeo4jComponent(project, repoPath, neo4jMetadata string, community bool, tags []string) (string, error) {
	if community {
		uri := "neo4j+offline:///data?database=" + url.QueryEscape(iops.config.Neo4jDatabase)
		opts := map[string]string{"neo4j_bin_dir": neo4jRunnerBinDir}
		return iops.withDeploymentQuiesced(func() (string, error) {
			return LaunchComposeBackup(project, "database", repoPath, uri, iops.runnerCredentials(""), opts, tags, true)
		})
	}

	uri := dbURI("neo4j", iops.config.Neo4jUsername, "database", "6362", iops.config.Neo4jDatabase)
	return LaunchComposeBackup(project, "database", repoPath, uri, iops.runnerCredentials(iops.config.Neo4jPassword),
		neo4jOnlineBackupOpts(neo4jMetadata), tags, false)
}

// withDeploymentQuiesced stops the application tier and then the database, runs
// dump against the now-idle data volume, and brings both back — the database
// first, then the applications.
//
// The Community offline dump takes the database away, so the applications have to
// go first: main stopped the whole application tier for exactly this path and the
// runner rewrite kept only StopServices("database"). Left running,
// infrahub-server and task-worker spend the dump failing against a stopped
// database, and their in-flight writes are what the dump was meant to be a
// consistent point in time for.
func (iops *InfrahubOps) withDeploymentQuiesced(dump func() (string, error)) (snapHex string, retErr error) {
	stopped, err := iops.stopAppContainers()
	if err != nil {
		if len(stopped) > 0 {
			if startErr := iops.startAppContainers(stopped); startErr != nil {
				logrus.Warnf("Failed to restart services after stop error: %v", startErr)
			}
		}
		return "", fmt.Errorf("failed to stop application services for the Community offline backup: %w", err)
	}
	// Registered before the database restart below so that it runs after it: the
	// application tier must not come back to a database that is still starting.
	defer func() {
		if err := iops.startAppContainers(stopped); err != nil && retErr == nil {
			retErr = fmt.Errorf("failed to restart application services: %w", err)
		}
	}()

	logrus.Info("Stopping Neo4j for offline (Community) backup...")
	if err := iops.StopServices("database"); err != nil {
		return "", fmt.Errorf("failed to stop neo4j: %w", err)
	}
	defer func() {
		logrus.Info("Restarting Neo4j...")
		if err := iops.StartServices("database"); err != nil {
			if retErr == nil {
				retErr = fmt.Errorf("failed to restart neo4j: %w", err)
			}
			return
		}
		// Stopping the container killed every client connection to the database,
		// so returning while it is still starting hands the caller a deployment
		// that looks up but cannot answer queries.
		if err := iops.waitForNeo4jBolt(neo4jBoltReadyTimeout); err != nil {
			logrus.Warnf("Backup completed, but %v", err)
		}
	}()

	return dump()
}

// neo4jOnlineBackupOpts are the connector options for the Enterprise online backup.
//
// --neo4jmetadata=none has to be FORWARDED, not omitted: the integration accepts
// "none" and turns it into --include-metadata=none, whereas omitting the option
// leaves neo4j-admin applying its own default of `all`. Skipping "none" therefore
// wrote users and roles into a backup the operator explicitly asked to exclude
// them from. The deleted streaming path passed the value unconditionally, so
// `none` used to work.
func neo4jOnlineBackupOpts(neo4jMetadata string) map[string]string {
	opts := map[string]string{"neo4j_bin_dir": neo4jRunnerBinDir}
	if neo4jMetadata != "" {
		opts["include_metadata"] = neo4jMetadata
	}
	return opts
}

// neo4jRunnerBinDir is where neo4j-admin lives in the database image the runner
// borrows.
const neo4jRunnerBinDir = "/var/lib/neo4j/bin"

// dbURI builds a connector URI with the username but WITHOUT the password.
//
// The URI is handed to `docker run` as an argument, where the host process list
// and `docker inspect` (Config.Cmd, for as long as the container exists) can both
// read it — the exposure the passphrase's stdin channel exists to avoid. The
// password travels the same way and is reunited with the connection by the worker,
// as the connector's standalone `password` option; see connectorConfig.
func dbURI(scheme, user, host, port, database string) string {
	u := &url.URL{
		Scheme: scheme,
		Host:   host + ":" + port,
		Path:   "/" + database,
	}
	if user != "" {
		u.User = url.User(user)
	}
	return u.String()
}

// runnerRepoPath returns the repository path to hand to the runner: an absolute
// host path for a local repo (bind-mounted into the runner), whether it was spelled
// as a bare path or as fs:///path, or the URI unchanged for s3:// and other schemes.
func (iops *InfrahubOps) runnerRepoPath() (string, error) {
	rp := iops.config.Plakar.RepoPath
	local, path := parseRepoLocation(rp)
	if !local {
		return rp, nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolving repo path %q: %w", rp, err)
	}
	return abs, nil
}

// ensurePlakarRepo opens (creating if necessary) the kloset repository so it
// exists before the runners write into it.
func (iops *InfrahubOps) ensurePlakarRepo() error {
	kctx, err := initPlakarContext(iops.config.Plakar)
	if err != nil {
		return err
	}
	defer closePlakarContext(kctx)
	repo, err := openOrCreateRepo(kctx, iops.config.Plakar)
	if err != nil {
		return err
	}
	closeRepo(repo)
	return nil
}

// writeMetadataSnapshot writes the backup metadata JSON as a snapshot in-process
// (no database connection is needed for it).
func (iops *InfrahubOps) writeMetadataSnapshot(metadataObj *BackupMetadata, tags []string) (string, error) {
	kctx, err := initPlakarContext(iops.config.Plakar)
	if err != nil {
		return "", err
	}
	defer closePlakarContext(kctx)
	repo, err := openOrCreateRepo(kctx, iops.config.Plakar)
	if err != nil {
		return "", err
	}
	defer closeRepo(repo)

	data, err := json.MarshalIndent(metadataObj, "", "    ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal metadata: %w", err)
	}
	imp := NewMemoryImporter(kctx.Hostname, "/backup_information.json", data)

	src, err := snapshot.NewSource(context.Background(), imp)
	if err != nil {
		return "", err
	}
	builder, err := snapshot.Create(repo, repository.DefaultType, os.TempDir(), objects.NilMac, &snapshot.BuilderOptions{
		Name: "metadata",
		Tags: tags,
	})
	if err != nil {
		return "", err
	}
	defer builder.Close()
	if err := builder.Backup(src); err != nil {
		return "", err
	}
	if err := builder.Commit(); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", builder.Header.Identifier), nil
}

// logIncompleteBackup warns about a partial backup failure. kloset doesn't
// support modifying tags after snapshot creation, so a group's incomplete status
// is derived at query time from missing components.
func logIncompleteBackup(completed []string, totalExpected int, backupID string) {
	if len(completed) == 0 {
		return
	}
	logrus.Warnf("Backup group %s is incomplete (%d/%d components created: %s)",
		backupID, len(completed), totalExpected, strings.Join(completed, ", "))
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
