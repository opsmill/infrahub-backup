package app

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/sirupsen/logrus"
)

// backupTaskManagerDBStream returns a data factory that streams the PostgreSQL dump
// in custom format without compression (-Fc -Z0) from the container via exec stdout.
func (iops *InfrahubOps) backupTaskManagerDBStream() (func() (io.ReadCloser, error), error) {
	return func() (io.ReadCloser, error) {
		opts := &ExecOptions{Env: map[string]string{
			"PGPASSWORD": iops.config.PostgresPassword,
		}}

		stdout, wait, err := iops.ExecStreamPipe(
			"task-manager-db",
			[]string{"pg_dump", "-Fc", "-Z0", "-h", "localhost", "-U", iops.config.PostgresUsername, "-d", iops.config.PostgresDatabase},
			opts,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to start postgres stream: %w", err)
		}

		return &execReadCloser{reader: stdout, wait: wait, idleTimeout: defaultStreamIdleTimeout}, nil
	}, nil
}

func (iops *InfrahubOps) backupTaskManagerDB(backupDir string) error {
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
		[]string{"pg_dump", "-Fc", "-h", "localhost", "-U", iops.config.PostgresUsername, "-d", iops.config.PostgresDatabase, "-f", dumpFile},
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

func (iops *InfrahubOps) restorePostgreSQL(workDir string) error {
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
		restoreCmd = []string{"pg_restore", "-d", "postgres", "--clean", "--create", dumpFile}
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
		restoreCmd = []string{"pg_restore", "-h", "localhost", "-d", "postgres", "-U", iops.config.PostgresUsername, "--clean", "--create", dumpFile}
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
