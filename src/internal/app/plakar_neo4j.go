package app

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	kimporter "github.com/PlakarKorp/kloset/connectors/importer"
	"github.com/PlakarKorp/kloset/kcontext"
	"github.com/PlakarKorp/kloset/objects"
	"github.com/PlakarKorp/kloset/repository"
	"github.com/PlakarKorp/kloset/snapshot"
	"github.com/opsmill/plakar-integration-neo4j/exporter"
	"github.com/opsmill/plakar-integration-neo4j/importer"
	"github.com/sirupsen/logrus"
)

// The Neo4j component runs neo4j-admin INSIDE the database container or pod,
// and builds the kloset snapshot in this process from what it produced.
//
// It does not use the co-located runner the other database component uses, and
// that is deliberate. neo4j-admin manipulates files in the data directory, so it
// has to run where that directory is. A sibling container sharing the volume
// satisfies that on Docker Compose and nowhere else — a Kubernetes pod's volume
// cannot be mounted into a container this tool starts, which is why the runner
// rework moved plakar on Kubernetes from working (on main) to refused. The same
// sibling model is also what made an Enterprise online backup reach the database
// over the network, so neo4j-admin acquired --from=database:6362 and every stock
// deployment then had to publish server.backup.listen_address for a backup to
// work at all.
//
// Running in place removes both: Exec and CopyFrom/CopyTo are implemented by the
// Docker and Kubernetes backends alike, and a command running inside the
// container reaches the backup service over loopback with no --from at all.
//
// What must NOT differ between this path and the runner's is the snapshot: the
// artifact's name and shape, the manifest, the neo4j-admin flags. So none of it
// is reimplemented here. The integration exposes a staged importer and a staged
// exporter that own the layout and report the argv; this file owns only WHERE
// the argv runs and how the bytes get there.

// neo4jStageDir is the directory inside the database container or pod that
// neo4j-admin writes its artifact into, and that a restore's staged artifact is
// copied to. It sits under the same work directory the tarball backend uses.
const neo4jStageDir = neo4jRemoteWorkDir + "/plakar-stage"

// neo4jAdminBin is the neo4j-admin executable as the database container sees it.
// Resolved through PATH rather than an absolute path: the runner needed
// /var/lib/neo4j/bin because it borrowed the image without its entrypoint, while
// an exec into the running container inherits the image's own PATH — which is
// what the tarball backend has always relied on.
const neo4jAdminBin = "neo4j-admin"

// neo4jDumpArgv is the staged importer's ability to report the neo4j-admin
// command that produces the artifact. It is an interface rather than the
// concrete type because NewStagedImporter returns kloset's importer interface;
// asserting on the behaviour keeps the dependency to the one method used.
type neo4jDumpArgv interface {
	DumpArgs(toPath string) ([]string, error)
}

// neo4jConnectorConfig builds the connector protocol and configuration for
// running neo4j-admin inside the database container.
//
// The online location carries NO host. The integration adds --from only when one
// is set, and without it neo4j-admin talks to the backup service over loopback —
// which is the whole point of running inside the container, and what makes
// server.backup.listen_address stop being a prerequisite.
func (iops *InfrahubOps) neo4jConnectorConfig(community bool, neo4jMetadata string) (string, map[string]string) {
	db := iops.config.Neo4jDatabase
	if community {
		location := (&url.URL{
			Scheme:   "neo4j+offline",
			Path:     "/data",
			RawQuery: url.Values{"database": []string{db}}.Encode(),
		}).String()
		return "neo4j+offline", map[string]string{"location": location}
	}

	location := (&url.URL{Scheme: "neo4j", Path: "/" + db}).String()
	config := map[string]string{"location": location}
	if neo4jMetadata != "" {
		config["include_metadata"] = neo4jMetadata
	}
	return "neo4j", config
}

// backupNeo4jInPlace captures the Neo4j component: run neo4j-admin in the
// database container, copy the artifact out, and emit the snapshot through the
// integration's staged importer so its layout is the connector's by construction.
func (iops *InfrahubOps) backupNeo4jInPlace(neo4jMetadata string, community bool, tags []string) (string, error) {
	proto, config := iops.neo4jConnectorConfig(community, neo4jMetadata)

	// The staged importer consumes the directory it is handed — it streams the
	// files lazily and removes the directory once the last reader is closed — so
	// the local stage is created BY the copy, inside a base this function owns.
	localBase, err := os.MkdirTemp("", "infrahub-neo4j-backup-*")
	if err != nil {
		return "", fmt.Errorf("creating the local staging directory: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(localBase); err != nil {
			logrus.Warnf("Failed to remove the local Neo4j staging directory %s: %v", localBase, err)
		}
	}()
	localStage := filepath.Join(localBase, "stage")

	imp, err := importer.NewStagedImporter(proto, config, localStage)
	if err != nil {
		return "", fmt.Errorf("creating the staged Neo4j importer: %w", err)
	}
	dumper, ok := imp.(neo4jDumpArgv)
	if !ok {
		return "", fmt.Errorf("the Neo4j integration does not report its neo4j-admin arguments; a release providing DumpArgs is required")
	}
	args, err := dumper.DumpArgs(neo4jStageDir)
	if err != nil {
		return "", err
	}

	if err := iops.prepareNeo4jStage(); err != nil {
		return "", err
	}
	defer iops.clearNeo4jStage()

	if err := iops.runNeo4jAdmin(args); err != nil {
		return "", err
	}

	// Copied to a path that does not exist, so the destination BECOMES the stage.
	// Both backends copy a directory INTO an existing destination instead of
	// replacing it, which would have nested the artifact one level too deep and
	// left the importer emitting nothing.
	if err := iops.CopyFrom("database", neo4jStageDir, localStage); err != nil {
		return "", fmt.Errorf("copying the Neo4j artifact out of the database container: %w", err)
	}

	return snapshotFromImporter(iops.config.Plakar, imp, proto, tags)
}

// restoreNeo4jInPlace replaces the Neo4j store from a snapshot: stage the
// snapshot's records locally through the integration's staged exporter, copy the
// stage into the database container, and run the neo4j-admin command the exporter
// reports there.
//
// The exporter is what decides which artifact is restorable, what it must be
// named, and whether --from-path takes the file or the directory. Those rules
// were established against a real Neo4j and they stay in one place: this function
// transfers bytes and runs an argv it is given.
func (iops *InfrahubOps) restoreNeo4jInPlace(snapHex string, community bool, migrate Neo4jMigration) error {
	proto, config := iops.neo4jConnectorConfig(community, "")
	config["overwrite"] = "true"

	localBase, err := os.MkdirTemp("", "infrahub-neo4j-restore-*")
	if err != nil {
		return fmt.Errorf("creating the local staging directory: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(localBase); err != nil {
			logrus.Warnf("Failed to remove the local Neo4j staging directory %s: %v", localBase, err)
		}
	}()
	// Must not exist when CopyTo runs, for the same reason the backup's must not:
	// a copy onto an existing directory nests instead of replacing.
	localStage := filepath.Join(localBase, "stage")

	exp, err := exporter.NewStagedExporter(proto, config, localStage)
	if err != nil {
		return fmt.Errorf("creating the staged Neo4j exporter: %w", err)
	}
	if err := iops.stageSnapshot(snapHex, exp); err != nil {
		return err
	}

	args, err := exp.RestoreArgs(neo4jStageDir)
	if err != nil {
		return err
	}

	if err := iops.clearNeo4jStageStrict(); err != nil {
		return err
	}
	defer iops.clearNeo4jStage()

	if err := iops.CopyTo("database", localStage, neo4jStageDir); err != nil {
		return fmt.Errorf("copying the staged Neo4j artifact into the database container: %w", err)
	}
	iops.ownNeo4jStage()

	if err := iops.runNeo4jAdmin(args); err != nil {
		return err
	}
	if migrate.Requested() {
		if err := iops.runNeo4jAdmin(migrate.args()); err != nil {
			return err
		}
		logrus.Infof("Migrated %s to format %s", migrate.Database, migrate.Format)
	}
	return nil
}

// withNeo4jSuspended runs body with the Neo4j process suspended, then resumes it
// and waits for Bolt to answer again.
//
// Suspension rather than stopping the container is what lets neo4j-admin run in
// place: a stopped container or a scaled-down pod cannot be exec'd into, and the
// data directory it holds is then reachable only by a sibling that shares the
// volume — which is exactly the Docker-only arrangement this path replaces. The
// watchdog is what makes suspension survivable: it freezes the JVM the moment
// Neo4j deletes its pid file, so the container's PID 1 never exits and the
// container stays up with an idle store.
//
// Generic in body's result so the backup (which yields a snapshot id) and the
// restore (which yields nothing) share one description of the offline window.
func withNeo4jSuspended[T any](iops *InfrahubOps, purpose string, body func() (T, error)) (result T, retErr error) {
	pidStr, err := iops.readNeo4jPID()
	if err != nil {
		return result, err
	}

	logrus.Infof("Suspending Neo4j for the %s...", purpose)
	if err := iops.stopNeo4jCommunity(pidStr); err != nil {
		return result, fmt.Errorf("failed to suspend neo4j for the %s: %w", purpose, err)
	}

	defer func() {
		if _, err := iops.Exec("database", []string{"rm", "-f", neo4jRemoteWatchdogBinary, neo4jRemoteWatchdogReady, neo4jRemoteWatchdogLog}, nil); err != nil {
			logrus.Debugf("Failed to remove watchdog artifacts: %v", err)
		}
		logrus.Info("Resuming Neo4j...")
		if _, err := iops.Exec("database", []string{"kill", "-CONT", pidStr}, nil); err != nil {
			logrus.Errorf("Failed to send SIGCONT to neo4j (pid %s): %v", pidStr, err)
			if retErr == nil {
				retErr = fmt.Errorf("failed to resume neo4j process: %w", err)
			}
			return
		}
		// Resuming lets the interrupted shutdown finish, so the container goes down
		// and every client connection with it. Returning before Bolt answers hands
		// the caller a deployment that looks up but cannot serve a query — and a
		// deployment with no restart policy would not come back at all, which is
		// why this waits on the database being usable rather than on a restart
		// happening.
		if err := iops.waitForNeo4jBack(neo4jBoltReadyTimeout); err != nil {
			logrus.Warnf("The %s completed, but %v", purpose, err)
		}
	}()

	return body()
}

// backendName reports the detected environment's name for logging, without
// forcing a detection that has not happened yet.
func (iops *InfrahubOps) backendName() string {
	if iops.backend == nil {
		return "unknown"
	}
	return iops.backend.Name()
}

// dockerBackendName is what DockerBackend.Name reports.
const dockerBackendName = "docker"

// composeProjectForRunner reports the Docker Compose project the task-manager
// runner should use, or "" when the deployment is not on Docker Compose.
//
// Read from the DETECTED backend rather than from --project alone. The flag can
// be given alongside --k8s-namespace, and detection can land on Kubernetes with
// a project name still set — handing that to the runner would look for compose
// containers that do not exist and fail with a confusing error instead of taking
// the port-forwarded route that does work.
func (iops *InfrahubOps) composeProjectForRunner() string {
	if iops.backendName() != dockerBackendName {
		return ""
	}
	return iops.config.DockerComposeProject
}

// stageSnapshot exports one snapshot through exp, which stages its records and
// stops short of running neo4j-admin.
func (iops *InfrahubOps) stageSnapshot(snapHex string, exp *exporter.Exporter) error {
	kctx, err := initPlakarContext(iops.config.Plakar)
	if err != nil {
		return err
	}
	defer closePlakarContext(kctx)
	repo, err := openRepo(kctx, iops.config.Plakar)
	if err != nil {
		return err
	}
	defer closeRepo(repo)

	mac, err := parseMAC(snapHex)
	if err != nil {
		return err
	}
	snap, err := snapshot.Load(repo, mac)
	if err != nil {
		return fmt.Errorf("loading snapshot %s: %w", snapHex, err)
	}
	defer snap.Close()

	if err := snap.Export(exp, "/", &snapshot.ExportOptions{SkipPermissions: true}); err != nil {
		return fmt.Errorf("staging snapshot %s: %w", snapHex, err)
	}
	// Close is what settles the staged artifact's name; a staged exporter leaves
	// the directory itself alone, which is why the caller still owns it.
	if err := exp.Close(kctx.Context); err != nil {
		return fmt.Errorf("closing the staged Neo4j exporter: %w", err)
	}
	return nil
}

// runNeo4jAdmin runs one neo4j-admin command inside the database container.
func (iops *InfrahubOps) runNeo4jAdmin(args []string) error {
	output, err := iops.Exec("database", append([]string{neo4jAdminBin}, args...), iops.getNeo4jExecOptions())
	if err != nil {
		return fmt.Errorf("neo4j-admin %v failed: %w\nOutput: %v", args, err, output)
	}
	return nil
}

// prepareNeo4jStage creates an empty stage directory in the database container.
//
// Cleared rather than merely created: the exporter requires exactly one data
// artifact, so an artifact left behind by an interrupted run would make the next
// backup's snapshot unrestorable — and a dump left in the directory a restore
// loads from is a complete archive of the wrong deployment sitting where the
// loader looks.
func (iops *InfrahubOps) prepareNeo4jStage() error {
	if err := iops.clearNeo4jStageStrict(); err != nil {
		return err
	}
	if _, err := iops.Exec("database", []string{"mkdir", "-p", neo4jStageDir}, nil); err != nil {
		return fmt.Errorf("creating %s in the database container: %w", neo4jStageDir, err)
	}
	iops.ownNeo4jStage()
	return nil
}

// clearNeo4jStageStrict removes the stage directory and fails if it cannot.
func (iops *InfrahubOps) clearNeo4jStageStrict() error {
	if _, err := iops.Exec("database", []string{"rm", "-rf", neo4jStageDir}, nil); err != nil {
		return fmt.Errorf("clearing %s in the database container: %w", neo4jStageDir, err)
	}
	return nil
}

// clearNeo4jStage removes the stage directory, best effort. Used on the way out,
// where the container may already be restarting and refusing execs, and where the
// next run clears the directory before it writes anyway.
func (iops *InfrahubOps) clearNeo4jStage() {
	if _, err := iops.Exec("database", []string{"rm", "-rf", neo4jStageDir}, nil); err != nil {
		logrus.Debugf("Could not remove %s in the database container; it is cleared again before the next run writes into it: %v", neo4jStageDir, err)
	}
}

// ownNeo4jStage gives the stage to the neo4j user, best effort.
//
// neo4j-admin runs as neo4j and must be able to write the artifact (backup) and
// read it (restore). When the exec already runs as neo4j the directory is created
// correctly owned and the chown is redundant; when it runs as root it is
// required. It fails harmlessly in the first case, so it is not an error.
func (iops *InfrahubOps) ownNeo4jStage() {
	if _, err := iops.Exec("database", []string{"chown", "-R", "neo4j:neo4j", neo4jStageDir}, nil); err != nil {
		logrus.Debugf("Could not chown %s to neo4j (it is usually already owned by it): %v", neo4jStageDir, err)
	}
}

// snapshotFromImporter writes one snapshot from imp into the kloset repository,
// in this process.
//
// Shared by the metadata component and the in-place Neo4j one so that a snapshot
// built here is built the same way whichever component asked for it.
func snapshotFromImporter(cfg *PlakarConfig, imp kimporter.Importer, name string, tags []string) (string, error) {
	return snapshotFromImporterFunc(cfg, name, tags, func(*kcontext.KContext) (kimporter.Importer, error) {
		return imp, nil
	})
}

// snapshotFromImporterFunc is snapshotFromImporter for an importer that needs
// the kloset context to be built — which the registry-backed connectors do, and
// the staged and in-memory importers do not.
func snapshotFromImporterFunc(cfg *PlakarConfig, name string, tags []string, build func(*kcontext.KContext) (kimporter.Importer, error)) (string, error) {
	kctx, err := initPlakarContext(cfg)
	if err != nil {
		return "", err
	}
	defer closePlakarContext(kctx)
	repo, err := openOrCreateRepo(kctx, cfg)
	if err != nil {
		return "", err
	}
	defer closeRepo(repo)

	imp, err := build(kctx)
	if err != nil {
		return "", err
	}
	defer func() {
		if err := imp.Close(kctx.Context); err != nil {
			logrus.Warnf("Failed to close the importer for %s: %v", name, err)
		}
	}()

	src, err := snapshot.NewSource(context.Background(), imp)
	if err != nil {
		return "", fmt.Errorf("creating snapshot source: %w", err)
	}
	builder, err := snapshot.Create(repo, repository.DefaultType, os.TempDir(), objects.NilMac, &snapshot.BuilderOptions{
		Name: name,
		Tags: tags,
	})
	if err != nil {
		return "", fmt.Errorf("creating snapshot: %w", err)
	}
	defer builder.Close()

	if err := builder.Backup(src); err != nil {
		return "", fmt.Errorf("backup failed: %w", err)
	}
	if err := builder.Commit(); err != nil {
		return "", fmt.Errorf("commit failed: %w", err)
	}
	return fmt.Sprintf("%x", builder.Header.Identifier), nil
}
