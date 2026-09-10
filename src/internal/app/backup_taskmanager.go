package app

import (
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/sirupsen/logrus"
)

// A PostgreSQL dump is a client operation either way, so the external path
// differs from the internal one in exactly three places: the host it connects
// to, where the credentials come from, and whether it verifies the server's
// certificate.
//
// The host is the reason the internal command cannot simply be reused —
// `-h localhost` inside the transient workload names the workload, not the
// database. The credentials move because ExecOptions.Env becomes an
// `env PGPASSWORD=…` prefix on the exec command line, which is readable by
// anyone who can watch the deployment's processes or its API-server audit log
// (FR-014, research R7); the workload's own secret supplies PGUSER and PGPASSWORD
// as real environment instead, so the external command carries neither -U nor a
// password. And the certificate is verified unless the operator opted out
// (FR-029).

// postgresDumpRequest is everything the dump command needs.
type postgresDumpRequest struct {
	// Host is the server to connect to. A nil host means the internal path,
	// which connects to localhost inside the database's own container.
	Host *HostPort

	// User is the role to connect as. It is empty on the external path, where
	// PGUSER comes from the workload's secret; naming it on the command line
	// there would be redundant, and the password that goes with it must not be
	// there at all.
	User string

	// Database is the database to dump.
	Database string

	// File is where the dump is written. Empty streams it to stdout.
	File string

	// Uncompressed emits -Z0, which the streaming path asks for so the dump
	// deduplicates.
	Uncompressed bool
}

// postgresDumpCommand builds the pg_dump argv.
//
// The flag order is the order the in-container path has always written, and every
// difference is driven by a field rather than by a branch, so an internal
// request produces the exact argv that path produced before this feature existed
// (FR-015). It never carries a password: the only credential pg_dump needs
// beyond the role name is PGPASSWORD, which is environment, and on the path where
// that matters it is environment the workload's secret provides.
func postgresDumpCommand(req postgresDumpRequest) []string {
	command := []string{"pg_dump", "-Fc"}
	if req.Uncompressed {
		command = append(command, "-Z0")
	}

	host := "localhost"
	if req.Host != nil {
		host = req.Host.Host
	}
	command = append(command, "-h", host)

	if req.Host != nil && req.Host.Port > 0 {
		command = append(command, "-p", strconv.Itoa(req.Host.Port))
	}
	if req.User != "" {
		command = append(command, "-U", req.User)
	}

	command = append(command, "-d", req.Database)

	if req.File != "" {
		command = append(command, "-f", req.File)
	}

	return command
}

// externalPostgresDumpRequest is the request for a dump from a server outside
// the deployment: the member the probe reached, no role on the command line, and
// no password anywhere.
//
// The host is the one the probe got an answer from, not the first in the list.
// The two differ exactly when they matter — the probe walks the members until
// one responds, so taking Hosts[0] here, as an earlier version of this did,
// throws away the only reachability evidence the run has and points the dump at
// a member it has just watched fail.
func externalPostgresDumpRequest(capture *externalCapture, file string, uncompressed bool) postgresDumpRequest {
	host := capture.Facts.Answered

	return postgresDumpRequest{
		Host:         &host,
		Database:     capture.Endpoint.Database,
		File:         file,
		Uncompressed: uncompressed,
	}
}

// internalPostgresDumpRequest is the request the in-container path has always
// made.
func internalPostgresDumpRequest(cfg *Configuration, file string, uncompressed bool) postgresDumpRequest {
	return postgresDumpRequest{
		User:         cfg.PostgresUsername,
		Database:     cfg.PostgresDatabase,
		File:         file,
		Uncompressed: uncompressed,
	}
}

// backupTaskManagerDBStream returns a data factory that streams the PostgreSQL dump
// in custom format without compression (-Fc -Z0) from the container via exec stdout.
func (iops *InfrahubOps) backupTaskManagerDBStream() (func() (io.ReadCloser, error), error) {
	return func() (io.ReadCloser, error) {
		// Resolved when the factory runs rather than when it was built, so the
		// transient workload's life covers the stream. A nil source is an
		// internal database and takes the path below unchanged.
		source, err := iops.externalDatabaseFor(serviceTaskManagerDB)
		if err != nil {
			return nil, err
		}
		if source != nil {
			return iops.streamTaskManagerDBExternal()
		}

		opts := &ExecOptions{Env: map[string]string{
			"PGPASSWORD": iops.config.PostgresPassword,
		}}

		stdout, wait, err := iops.ExecStreamPipe(
			"task-manager-db",
			postgresDumpCommand(internalPostgresDumpRequest(iops.config, "", true)),
			opts,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to start postgres stream: %w", err)
		}

		return &execReadCloser{reader: stdout, wait: wait, idleTimeout: defaultStreamIdleTimeout}, nil
	}, nil
}

// externalPostgresDumpFile is where an external dump is written: the transient
// workload's scratch volume, which is the volume FR-022 sized, rather than a
// temp directory on the node.
const externalPostgresDumpFile = transientScratchPath + "/infrahubops_prefect.dump"

// streamTaskManagerDBExternal streams the dump from a server outside the
// deployment. The transient workload is given back when the stream closes, and
// on every failure path before that (FR-011).
func (iops *InfrahubOps) streamTaskManagerDBExternal() (io.ReadCloser, error) {
	capture, err := iops.openExternalCapture(serviceTaskManagerDB)
	if err != nil {
		return nil, err
	}

	logrus.Infof("Streaming the external PostgreSQL dump from %s (of the endpoints supplied, %s answered the probe)...",
		capture.Endpoint.endpointTarget(), capture.Facts.Answered)

	stdout, wait, err := iops.ExecStreamPipe(
		serviceTaskManagerDB,
		postgresDumpCommand(externalPostgresDumpRequest(capture, "", true)),
		capture.execOptions(capture.ClientEnv),
	)
	if err != nil {
		capture.Release()

		return nil, fmt.Errorf("failed to start the external postgres stream from %s: %w", capture.Endpoint.endpointTarget(), err)
	}

	return &execReadCloser{reader: stdout, wait: wait, idleTimeout: defaultStreamIdleTimeout, cleanup: capture.Release}, nil
}

// backupTaskManagerDBExternal takes the dump from a server outside the
// deployment. The destination and the entry name are the internal path's, so the
// artifact is byte-compatible either way (FR-015).
func (iops *InfrahubOps) backupTaskManagerDBExternal(backupDir string) error {
	capture, err := iops.openExternalCapture(serviceTaskManagerDB)
	if err != nil {
		return err
	}
	defer capture.Release()

	logrus.Infof("Backing up the external PostgreSQL database from %s (of the endpoints supplied, %s answered the probe)...",
		capture.Endpoint.endpointTarget(), capture.Facts.Answered)

	if output, err := iops.execBoundedAgainst(
		capture.Bound, "the database dump", capture.Endpoint.endpointTarget(), serviceTaskManagerDB,
		postgresDumpCommand(externalPostgresDumpRequest(capture, externalPostgresDumpFile, false)),
		capture.execOptions(capture.ClientEnv),
	); err != nil {
		return fmt.Errorf("failed to dump the external PostgreSQL database at %s: %w\nOutput: %v", capture.Endpoint.endpointTarget(), err, output)
	}
	defer iops.removeTransientScratch(serviceTaskManagerDB, "the dump", capture.execOptions(nil), externalPostgresDumpFile)

	if err := iops.copyFromTransientWorkload(capture.Pod, externalPostgresDumpFile, filepath.Join(backupDir, "prefect.dump")); err != nil {
		return fmt.Errorf("failed to copy the external postgresql dump: %w", err)
	}

	logrus.Info("PostgreSQL backup completed")

	return nil
}

func (iops *InfrahubOps) backupTaskManagerDB(backupDir string) error {
	source, err := iops.externalDatabaseFor(serviceTaskManagerDB)
	if err != nil {
		return err
	}
	if source != nil {
		return iops.backupTaskManagerDBExternal(backupDir)
	}

	logrus.Info("Backing up PostgreSQL database...")

	// Determine writable temp directory
	tempDir := iops.getWritableTempDir("task-manager-db")
	dumpFile := tempDir + "/infrahubops_prefect.dump"

	// Create dump
	opts := &ExecOptions{Env: map[string]string{
		"PGPASSWORD": iops.config.PostgresPassword,
	}}
	if output, err := iops.Exec(
		"task-manager-db",
		postgresDumpCommand(internalPostgresDumpRequest(iops.config, dumpFile, false)),
		opts,
	); err != nil {
		return fmt.Errorf("failed to create postgresql dump: %w\nOutput: %v", err, output)
	}
	defer func() {
		if _, err := iops.Exec("task-manager-db", []string{"rm", dumpFile}, nil); err != nil {
			logrus.Warnf("Failed to remove temporary postgres dump: %v", err)
		}
	}()

	// Copy dump
	if err := iops.CopyFrom("task-manager-db", dumpFile, filepath.Join(backupDir, "prefect.dump")); err != nil {
		return fmt.Errorf("failed to copy postgresql dump: %w", err)
	}

	logrus.Info("PostgreSQL backup completed")
	return nil
}

// postgresRestoreRequest is everything the restore command needs, and it is
// the dump request's mirror: the same three fields differ between a client
// running inside the database's own container and one running in a transient
// workload beside an external server.
type postgresRestoreRequest struct {
	// Host is the server to connect to. A nil host is the internal path's Unix
	// socket branch, which names no host at all; a host with no port is its TCP
	// branch, which has always connected to localhost without naming one.
	Host *HostPort

	// User is the role to connect as. It is empty on the external path, where
	// PGUSER comes from the workload's secret, and empty on the socket branch,
	// where the container's own user is the role.
	User string

	// File is the dump to read.
	File string

	// Socket asks for the connection the internal path uses where the
	// container's user matches the database role: no host, no user, no
	// password.
	Socket bool
}

// postgresRestoreCommand builds the pg_restore argv.
//
// The flag order is the order the in-container path has always written, and
// every difference is driven by a field rather than by a branch, so an internal
// request produces the exact argv that path produced before this feature
// existed (FR-015). It never carries a password: the only credential
// pg_restore needs beyond the role name is PGPASSWORD, which is environment —
// and on the path where that matters it is environment the workload's secret
// provides (FR-014).
func postgresRestoreCommand(req postgresRestoreRequest) []string {
	command := []string{"pg_restore"}

	if !req.Socket {
		host := "localhost"
		if req.Host != nil {
			host = req.Host.Host
		}
		command = append(command, "-h", host)

		if req.Host != nil && req.Host.Port > 0 {
			command = append(command, "-p", strconv.Itoa(req.Host.Port))
		}
	}

	command = append(command, "-d", "postgres")

	if req.User != "" {
		command = append(command, "-U", req.User)
	}

	return append(command, "--clean", "--create", req.File)
}

func (iops *InfrahubOps) restorePostgreSQL(workDir string) error {
	// Resolved before anything is started or copied: a nil restore is an
	// in-deployment database and takes the path below unchanged, and a database
	// the gate established is external with nothing prepared for it is an error
	// rather than a silent fall-through into a container it does not live in.
	restore, err := iops.externalRestoreFor(serviceTaskManagerDB)
	if err != nil {
		return err
	}
	if restore != nil {
		return iops.restorePostgreSQLExternal(workDir, restore)
	}

	logrus.Info("Restoring PostgreSQL database...")

	// Start task-manager-db
	if err := iops.StartServices("task-manager-db"); err != nil {
		backend, backendErr := iops.ensureBackend()
		if backendErr == nil && backend.Name() == "kubernetes" {
			logrus.Infof("Skipping task-manager-db start on Kubernetes (may be externally managed): %v", err)
		} else {
			return fmt.Errorf("failed to start task-manager-db: %w", err)
		}
	}

	// Determine writable temp directory
	tempDir := iops.getWritableTempDir("task-manager-db")
	dumpFile := tempDir + "/infrahubops_prefect.dump"

	// Copy dump to container
	dumpPath := filepath.Join(workDir, "backup", "prefect.dump")
	if err := iops.CopyTo("task-manager-db", dumpPath, dumpFile); err != nil {
		return fmt.Errorf("failed to copy dump to container: %w", err)
	}
	defer func() {
		if _, err := iops.Exec("task-manager-db", []string{"rm", dumpFile}, nil); err != nil {
			logrus.Warnf("Failed to remove temporary postgres dump: %v", err)
		}
	}()

	// Restore database
	// Check if we can use Unix socket (container user matches postgres username)
	var restoreCmd []string
	var opts *ExecOptions
	containerUser, err := iops.Exec("task-manager-db", []string{"whoami"}, nil)
	useUnixSocket := err == nil && !strings.Contains(strings.TrimSpace(containerUser), "cannot find name")
	if useUnixSocket {
		// Use Unix socket connection (no host, user, or password)
		restoreCmd = postgresRestoreCommand(postgresRestoreRequest{Socket: true, File: dumpFile})
		// On Docker, run as the postgres user so Unix socket auth succeeds
		backend, backendErr := iops.ensureBackend()
		if backendErr == nil && backend.Name() == "docker" {
			opts = &ExecOptions{User: iops.config.PostgresUsername}
		} else {
			opts = nil
		}
	} else {
		// Use TCP connection with credentials
		opts = &ExecOptions{Env: map[string]string{
			"PGPASSWORD": iops.config.PostgresPassword,
		}}
		restoreCmd = postgresRestoreCommand(postgresRestoreRequest{User: iops.config.PostgresUsername, File: dumpFile})
	}
	if output, err := iops.Exec(
		"task-manager-db",
		restoreCmd,
		opts,
	); err != nil {
		return fmt.Errorf("failed to restore postgresql: %w\nOutput: %v", err, output)
	}

	return nil
}
