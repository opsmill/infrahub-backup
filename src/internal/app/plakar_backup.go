package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

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
// (one per component), on Docker Compose or Kubernetes alike.
//
// Each component is captured where its tool has to run. Neo4j runs neo4j-admin
// inside the database container or pod, because it manipulates the data
// directory (see plakar_neo4j.go). The task manager only needs to reach its
// server, so it runs in a co-located runner on Docker Compose (see runner.go)
// and in this process over a port-forward on Kubernetes (see plakar_postgres.go).
// The metadata component is written in-process either way.
//
// Every path emits the upstream connectors' own snapshot layout, so a backup
// taken on one backend restores on the other.
func (iops *InfrahubOps) CreatePlakarBackup(force bool, neo4jMetadata string, excludeTaskManager bool, sleepDuration time.Duration, redact bool) (retErr error) {
	if err := iops.checkPrerequisites(); err != nil {
		return err
	}
	if err := iops.DetectEnvironment(); err != nil {
		return err
	}

	// Empty on Kubernetes. The Neo4j component runs in place on either backend
	// (see plakar_neo4j.go); only the task-manager component still needs a runner,
	// and only on Docker Compose, so the project name is resolved from the detected
	// backend and checked where it is used rather than refused up front.
	project := iops.composeProjectForRunner()

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
		"backend":       iops.backendName(),
	}).Info("Creating Plakar backup")

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
			snapHex, cerr = iops.backupNeo4jComponent(neo4jMetadata, editionInfo.IsCommunity, tags)

		case ComponentPostgres:
			snapHex, cerr = iops.backupTaskManagerComponent(project, repoPath, tags)

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

// backupNeo4jComponent captures the Neo4j component, running neo4j-admin inside
// the database container or pod on either backend.
//
// Enterprise takes an online backup over loopback with the server up. Community
// needs the store idle, so the application tier is stopped and the Neo4j process
// is suspended for the length of the dump — suspended rather than the container
// stopped, because a stopped container is one this tool can no longer exec into,
// and exec is what makes the same code work on Kubernetes.
func (iops *InfrahubOps) backupNeo4jComponent(neo4jMetadata string, community bool, tags []string) (string, error) {
	if !community {
		return iops.backupNeo4jInPlace(neo4jMetadata, false, tags)
	}
	return iops.withDeploymentQuiesced(func() (string, error) {
		return iops.backupNeo4jInPlace(neo4jMetadata, true, tags)
	})
}

// backupTaskManagerComponent captures the task-manager database.
//
// Unlike Neo4j, pg_dump speaks the wire protocol rather than touching the data
// directory, so it needs reachability rather than co-location: a runner beside
// the container on Docker Compose, and a port-forward from this process on
// Kubernetes.
func (iops *InfrahubOps) backupTaskManagerComponent(project, repoPath string, tags []string) (string, error) {
	opts := map[string]string{"compress": "false"}
	if project == "" {
		return iops.backupTaskManagerForwarded(opts, tags)
	}
	uri := dbURI("postgres", iops.config.PostgresUsername, "task-manager-db", "5432", iops.config.PostgresDatabase)
	creds := iops.runnerCredentials(iops.config.PostgresPassword)
	return LaunchComposeBackup(project, "task-manager-db", repoPath, uri, creds, opts, tags, false)
}

// withDeploymentQuiesced stops the application tier, suspends Neo4j, runs the
// dump against the now-idle store, and brings both back — Neo4j first, then the
// applications.
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
	// Registered before the Neo4j resume below so that it runs after it: the
	// application tier must not come back to a database that is still starting.
	defer func() {
		if err := iops.startAppContainers(stopped); err != nil && retErr == nil {
			retErr = fmt.Errorf("failed to restart application services: %w", err)
		}
	}()

	return withNeo4jSuspended(iops, "offline (Community) backup", dump)
}

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
	data, err := json.MarshalIndent(metadataObj, "", "    ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal metadata: %w", err)
	}
	imp := NewMemoryImporter(hostnameForSnapshot(), "/backup_information.json", data)
	return snapshotFromImporter(iops.config.Plakar, imp, "metadata", tags)
}

// hostnameForSnapshot is the origin recorded on an in-process snapshot. It
// resolves the hostname the same way initPlakarContext does — same value, same
// fallback — so moving the metadata snapshot onto the shared builder did not
// change what it records.
func hostnameForSnapshot() string {
	hostname, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return hostname
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
