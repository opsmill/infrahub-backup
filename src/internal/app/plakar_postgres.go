package app

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	kexporter "github.com/PlakarKorp/kloset/connectors/exporter"
	kimporter "github.com/PlakarKorp/kloset/connectors/importer"
	"github.com/PlakarKorp/kloset/kcontext"
	"github.com/PlakarKorp/kloset/snapshot"
	"github.com/sirupsen/logrus"
)

// The task-manager (PostgreSQL) component is the one that still needs something
// beside the database rather than inside it.
//
// pg_dump and pg_restore speak the wire protocol, so — unlike neo4j-admin — they
// do not have to sit with the data directory. What they need is a route to the
// server and the client binaries. On Docker Compose the runner supplies both by
// borrowing the postgres image. On Kubernetes there is no such sibling, so the
// upstream connector runs in this process and reaches the pod through a
// port-forward. That keeps the snapshot the real connector's output on both
// backends, which is what makes a backup taken on one restorable on the other.
//
// The cost is a host dependency: the PostgreSQL client binaries must be present
// and at least as new as the server. It is checked up front rather than
// discovered inside the connector as an exec failure.

// portForwarder is the backend capability the forwarded path needs. Only the
// Kubernetes backend has it; asserting on the behaviour keeps this file from
// depending on the concrete backend type.
type portForwarder interface {
	PortForward(service string, remotePort int) (*portForward, error)
}

// postgresPort is the port the task-manager database listens on in the pod.
const postgresPort = 5432

// postgresClientBinaries are the executables the upstream connector shells out
// to. The importer uses pg_dump and pg_dumpall; the exporter uses pg_restore and
// psql. All four are checked for either direction so a restore cannot fail
// halfway through for want of a binary a backup did not need.
var postgresClientBinaries = []string{"pg_dump", "pg_dumpall", "pg_restore", "psql"}

// requirePostgresClient reports a missing PostgreSQL client installation as one
// actionable error, before anything is stopped or written.
func requirePostgresClient() error {
	var missing []string
	for _, bin := range postgresClientBinaries {
		if _, err := exec.LookPath(bin); err != nil {
			missing = append(missing, bin)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf(
		"the task-manager component on Kubernetes runs the PostgreSQL client on this machine, and %s %s not on PATH; "+
			"install the postgresql-client package (at a version no older than the server), "+
			"or pass --exclude-task-manager to skip the component",
		strings.Join(missing, ", "), pluralIsAre(len(missing)))
}

func pluralIsAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

// withTaskManagerForwarded runs body against a loopback address that reaches the
// task-manager database in the cluster.
func withTaskManagerForwarded[T any](iops *InfrahubOps, body func(location string) (T, error)) (result T, retErr error) {
	if err := requirePostgresClient(); err != nil {
		return result, err
	}
	backend, err := iops.ensureBackend()
	if err != nil {
		return result, err
	}
	forwarder, ok := backend.(portForwarder)
	if !ok {
		return result, fmt.Errorf("the %s backend cannot reach the task-manager database from this process", backend.Name())
	}

	pf, err := forwarder.PortForward("task-manager-db", postgresPort)
	if err != nil {
		return result, fmt.Errorf("reaching the task-manager database: %w", err)
	}
	defer pf.Close()

	location := dbURI("postgres", iops.config.PostgresUsername, "127.0.0.1",
		strconv.Itoa(pf.LocalPort), iops.config.PostgresDatabase)
	return body(location)
}

// postgresConnectorConfig builds the connector configuration, reuniting the
// password with the connection the way the runner's worker does.
func (iops *InfrahubOps) postgresConnectorConfig(location string, opts map[string]string) map[string]string {
	config := map[string]string{"location": location}
	if iops.config.PostgresPassword != "" {
		config["password"] = iops.config.PostgresPassword
	}
	for k, v := range opts {
		config[k] = v
	}
	return config
}

// backupTaskManagerForwarded captures the task-manager component in this
// process, through a port-forward.
func (iops *InfrahubOps) backupTaskManagerForwarded(opts map[string]string, tags []string) (string, error) {
	return withTaskManagerForwarded(iops, func(location string) (string, error) {
		config := iops.postgresConnectorConfig(location, opts)
		return snapshotFromImporterFunc(iops.config.Plakar, location, tags, func(kctx *kcontext.KContext) (kimporter.Importer, error) {
			imp, err := kimporter.NewImporter(kctx, connectorOptions(kctx), config)
			if err != nil {
				return nil, fmt.Errorf("creating the PostgreSQL importer: %w", err)
			}
			return imp, nil
		})
	})
}

// restoreTaskManagerForwarded restores the task-manager component in this
// process, through a port-forward.
func (iops *InfrahubOps) restoreTaskManagerForwarded(snapHex string, opts map[string]string) error {
	_, err := withTaskManagerForwarded(iops, func(location string) (struct{}, error) {
		return struct{}{}, iops.exportSnapshotToPostgres(snapHex, location, opts)
	})
	return err
}

func (iops *InfrahubOps) exportSnapshotToPostgres(snapHex, location string, opts map[string]string) error {
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

	config := iops.postgresConnectorConfig(location, opts)
	exp, err := kexporter.NewExporter(kctx, connectorOptions(kctx), config)
	if err != nil {
		return fmt.Errorf("creating the PostgreSQL exporter: %w", err)
	}

	exportErr := snap.Export(exp, "/", &snapshot.ExportOptions{SkipPermissions: true})
	// Closed on every exit path: pg_restore runs as the exporter drains, and a
	// failed export leaves it as much to finish as a successful one.
	if err := exp.Close(context.Background()); err != nil && exportErr == nil {
		exportErr = fmt.Errorf("closing the PostgreSQL exporter: %w", err)
	}
	if exportErr != nil {
		return fmt.Errorf("restore failed: %w", exportErr)
	}
	logrus.Info("Postgres restore completed")
	return nil
}
