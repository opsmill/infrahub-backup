package app

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/sirupsen/logrus"
)

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
		opts = nil
		restoreCmd = []string{"pg_restore", "-d", "postgres", "--clean", "--create", dumpFile}
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
