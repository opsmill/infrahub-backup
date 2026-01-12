package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

func (iops *InfrahubOps) backupDatabase(backupDir string, backupMetadata string, neo4jEdition string) error {
	edition := strings.ToLower(neo4jEdition)
	switch edition {
	case neo4jEditionCommunity:
		return iops.backupNeo4jCommunity(backupDir)
	default:
		return iops.backupNeo4jEnterprise(backupDir, backupMetadata)
	}
}

func (iops *InfrahubOps) backupNeo4jEnterprise(backupDir string, backupMetadata string) error {
	logrus.Info("Backing up Neo4j database (Enterprise Edition online backup)...")

	if _, err := iops.Exec("database", []string{"mkdir", "-p", "/tmp/infrahubops"}, nil); err != nil {
		return fmt.Errorf("failed to create backup directory: %w", err)
	}
	defer func() {
		if _, err := iops.Exec("database", []string{"rm", "-rf", "/tmp/infrahubops"}, nil); err != nil {
			logrus.Warnf("Failed to remove temporary Neo4j backup directory: %v", err)
		}
	}()

	if output, err := iops.Exec(
		"database",
		[]string{"neo4j-admin", "database", "backup", "--expand-commands", "--include-metadata=" + backupMetadata, "--to-path=/tmp/infrahubops", iops.config.Neo4jDatabase},
		nil,
	); err != nil {
		return fmt.Errorf("failed to backup neo4j: %w\nOutput: %v", err, output)
	}

	if err := iops.CopyFrom("database", "/tmp/infrahubops", filepath.Join(backupDir, "database")); err != nil {
		return fmt.Errorf("failed to copy database backup: %w", err)
	}

	logrus.Info("Neo4j backup completed")
	return nil
}

func (iops *InfrahubOps) stopNeo4jCommunity(pidStr string) error {
	if _, err := iops.Exec("database", []string{"mkdir", "-p", neo4jRemoteWorkDir}, nil); err != nil {
		return fmt.Errorf("failed to prepare remote work directory: %w", err)
	}

	arch, err := iops.detectNeo4jArchitecture()
	if err != nil {
		return err
	}

	watchdogBytes, err := selectWatchdogBinary(arch)
	if err != nil {
		return err
	}

	localWatchdog, cleanup, err := writeEmbeddedWatchdog(watchdogBytes)
	if err != nil {
		return err
	}
	defer cleanup()

	if err := iops.CopyTo("database", localWatchdog, neo4jRemoteWatchdogBinary); err != nil {
		return fmt.Errorf("failed to deploy watchdog binary: %w", err)
	}

	if _, err := iops.Exec("database", []string{"chmod", "+x", neo4jRemoteWatchdogBinary}, nil); err != nil {
		return fmt.Errorf("failed to mark watchdog executable: %w", err)
	}

	if _, err := iops.Exec("database", []string{"rm", "-f", neo4jRemoteWatchdogReady, neo4jRemoteWatchdogLog}, nil); err != nil {
		logrus.Debugf("Could not clear watchdog markers: %v", err)
	}

	watchdogCmd := fmt.Sprintf("nohup %s --ready-file %s >%s 2>&1 &", neo4jRemoteWatchdogBinary, neo4jRemoteWatchdogReady, neo4jRemoteWatchdogLog)
	if _, err := iops.Exec("database", []string{"sh", "-c", watchdogCmd}, nil); err != nil {
		return fmt.Errorf("failed to start watchdog: %w", err)
	}

	if err := iops.waitForRemoteFile(neo4jRemoteWatchdogReady, 5*time.Second); err != nil {
		return fmt.Errorf("watchdog failed to initialize: %w", err)
	}

	if _, err := iops.Exec("database", []string{"kill", pidStr}, nil); err != nil {
		return fmt.Errorf("failed to stop neo4j: %w", err)
	}

	logrus.Info("Waiting for Neo4j process to stop...")
	if err := iops.waitForProcessStopped(pidStr, 120*time.Second); err != nil {
		return err
	}

	return nil
}

func (iops *InfrahubOps) backupNeo4jCommunity(backupDir string) (retErr error) {
	logrus.Info("Backing up Neo4j database (Community Edition offline dump)...")

	pidStr, err := iops.readNeo4jPID()
	if err != nil {
		return err
	}

	err = iops.stopNeo4jCommunity(pidStr)
	if err != nil {
		return err
	}

	defer func() {
		if _, err := iops.Exec("database", []string{"rm", "-f", neo4jRemoteWatchdogBinary, neo4jRemoteWatchdogReady, neo4jRemoteWatchdogLog}, nil); err != nil {
			logrus.Debugf("Failed to remove watchdog artifacts: %v", err)
		}
		if _, err := iops.Exec("database", []string{"kill", "-CONT", pidStr}, nil); err != nil {
			logrus.Errorf("Failed to send SIGCONT to neo4j (pid %s): %v", pidStr, err)
			if retErr == nil {
				retErr = fmt.Errorf("failed to resume neo4j process: %w", err)
			}
		}
	}()

	if _, err := iops.Exec("database", []string{"mkdir", "-p", neo4jRemoteWorkDir}, nil); err != nil {
		return fmt.Errorf("failed to prepare remote dump directory: %w", err)
	}

	databaseDir := filepath.Join(backupDir, "database")
	if err := os.MkdirAll(databaseDir, 0755); err != nil {
		return fmt.Errorf("failed to prepare local dump directory: %w", err)
	}

	dumpCmd := []string{
		"neo4j-admin", "database", "dump",
		"--overwrite-destination=true",
		"--to-path=" + neo4jRemoteWorkDir,
		iops.config.Neo4jDatabase,
	}
	if output, dumpErr := iops.Exec("database", dumpCmd, nil); dumpErr != nil {
		return fmt.Errorf("failed to dump neo4j database: %w\nOutput: %v", dumpErr, output)
	}

	dumpFilename := fmt.Sprintf("%s.dump", iops.config.Neo4jDatabase)
	if err := iops.CopyFrom("database", neo4jRemoteWorkDir+"/"+dumpFilename, filepath.Join(databaseDir, dumpFilename)); err != nil {
		return fmt.Errorf("failed to copy neo4j dump: %w", err)
	}

	logrus.Info("Neo4j dump completed")
	return nil
}

func (iops *InfrahubOps) restoreNeo4j(workDir, neo4jEdition string, restoreMigrateFormat bool) error {
	backupPath := filepath.Join(workDir, "backup", "database")

	if err := iops.CopyTo("database", backupPath, "/tmp/infrahubops"); err != nil {
		return fmt.Errorf("failed to copy backup to container: %w", err)
	}
	defer func() {
		if _, err := iops.Exec("database", []string{"rm", "-rf", "/tmp/infrahubops"}, nil); err != nil {
			logrus.Warnf("Failed to cleanup temporary Neo4j backup data (this is expected for community restore method): %v", err)
		}
	}()

	if _, err := iops.Exec("database", []string{"chown", "-R", "neo4j:neo4j", "/tmp/infrahubops"}, nil); err != nil {
		return fmt.Errorf("failed to change backup ownership: %w", err)
	}

	edition := strings.ToLower(neo4jEdition)
	switch edition {
	case neo4jEditionCommunity:
		return iops.restoreNeo4jCommunity(restoreMigrateFormat)
	default:
		return iops.restoreNeo4jEnterprise(restoreMigrateFormat)
	}
}

func (iops *InfrahubOps) restoreNeo4jEnterprise(restoreMigrateFormat bool) error {
	logrus.Info("Restoring Neo4j database (Enterprise Edition)...")

	opts := iops.getNeo4jExecOptions()

	// Check if Neo4j is running in cluster mode
	if iops.isNeo4jCluster() {
		return iops.restoreNeo4jCluster(opts)
	}

	if _, err := iops.Exec(
		"database",
		[]string{"cypher-shell", "-u", iops.config.Neo4jUsername, "-p" + iops.config.Neo4jPassword, "-d", "system", "stop database " + iops.config.Neo4jDatabase},
		nil,
	); err != nil {
		return fmt.Errorf("failed to stop neo4j database: %w", err)
	}

	if output, err := iops.Exec(
		"database",
		[]string{"neo4j-admin", "database", "restore", "--expand-commands", "--overwrite-destination=true", "--from-path=/tmp/infrahubops", iops.config.Neo4jDatabase},
		opts,
	); err != nil {
		return fmt.Errorf("failed to restore neo4j: %w\nOutput: %v", err, output)
	}

	if restoreMigrateFormat {
		if output, err := iops.Exec(
			"database",
			[]string{"neo4j-admin", "database", "migrate", "--expand-commands", "--to-format=block", iops.config.Neo4jDatabase},
			opts,
		); err != nil {
			return fmt.Errorf("failed to migrate neo4j to block format: %w\nOutput: %v", err, output)
		}
	}

	if output, err := iops.Exec(
		"database",
		[]string{"sh", "-c", "cat /data/scripts/neo4j/restore_metadata.cypher | cypher-shell -u " + iops.config.Neo4jUsername + " -p" + iops.config.Neo4jPassword + " -d system --param \"database => '" + iops.config.Neo4jDatabase + "'\""},
		opts,
	); err != nil {
		return fmt.Errorf("failed to restore neo4j metadata: %w\nOutput: %v", err, output)
	}

	if _, err := iops.Exec(
		"database",
		[]string{"cypher-shell", "-u", iops.config.Neo4jUsername, "-p" + iops.config.Neo4jPassword, "-d", "system", "start database " + iops.config.Neo4jDatabase},
		nil,
	); err != nil {
		return fmt.Errorf("failed to start neo4j database: %w", err)
	}

	return nil
}

func (iops *InfrahubOps) restoreNeo4jCluster(opts *ExecOptions) error {
	logrus.Info("Using Neo4j cluster restore flow (designated seeder method)...")

	// 1. Stop and drop database
	logrus.Info("Stopping database...")
	if _, err := iops.Exec("database", []string{
		"cypher-shell", "-u", iops.config.Neo4jUsername, "-p" + iops.config.Neo4jPassword,
		"-d", "system",
		"STOP DATABASE " + iops.config.Neo4jDatabase,
	}, nil); err != nil {
		logrus.Warnf("Failed to stop database (may not exist): %v", err)
	}

	logrus.Info("Dropping database...")
	if _, err := iops.Exec("database", []string{
		"cypher-shell", "-u", iops.config.Neo4jUsername, "-p" + iops.config.Neo4jPassword,
		"-d", "system",
		"DROP DATABASE " + iops.config.Neo4jDatabase + " IF EXISTS",
	}, nil); err != nil {
		return fmt.Errorf("failed to drop database: %w", err)
	}

	// 2. Restore backup using neo4j-admin (on current node only)
	logrus.Info("Restoring backup with neo4j-admin...")
	if output, err := iops.Exec("database", []string{
		"neo4j-admin", "database", "restore",
		"--expand-commands", "--overwrite-destination=true",
		"--from-path=/tmp/infrahubops",
		iops.config.Neo4jDatabase,
	}, opts); err != nil {
		return fmt.Errorf("failed to restore neo4j: %w\nOutput: %v", err, output)
	}

	// 3. Get current node's serverId using dbms.cluster.statusCheck()
	logrus.Info("Getting current server ID...")
	serverIdOutput, err := iops.Exec("database", []string{
		"cypher-shell", "-u", iops.config.Neo4jUsername, "-p" + iops.config.Neo4jPassword,
		"-d", "system",
		"--format", "plain",
		"CALL dbms.cluster.statusCheck([]) YIELD requester, serverId RETURN requester, serverId",
	}, nil)
	if err != nil {
		return fmt.Errorf("failed to get server ID: %w", err)
	}
	// Parse output to find the row where requester = true
	// Output format: "requester, serverId\ntrue, \"abc-123\"\nfalse, \"def-456\"\n"
	var serverId string
	lines := strings.Split(strings.TrimSpace(serverIdOutput), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		// Skip header line
		if strings.HasPrefix(line, "requester") {
			continue
		}
		// Check if this row has requester = true
		if strings.HasPrefix(line, "true") || strings.HasPrefix(line, "TRUE") {
			// Extract serverId from "true, \"abc-123\"" or "true, abc-123"
			parts := strings.SplitN(line, ",", 2)
			if len(parts) == 2 {
				serverId = strings.TrimSpace(parts[1])
				serverId = strings.Trim(serverId, "\"")
				break
			}
		}
	}
	if serverId == "" {
		return fmt.Errorf("failed to find current server ID (no requester=true found in output)")
	}
	logrus.Infof("Current server ID: %s", serverId)

	// 4. Create database with designated seeder
	logrus.Info("Creating database with designated seeder...")
	createCmd := fmt.Sprintf(`CREATE DATABASE %s
TOPOLOGY 3 PRIMARIES
OPTIONS {
  existingData: 'use',
  existingDataSeedInstance: '%s'
}`, iops.config.Neo4jDatabase, serverId)

	if _, err := iops.Exec("database", []string{
		"cypher-shell", "-u", iops.config.Neo4jUsername, "-p" + iops.config.Neo4jPassword,
		"-d", "system",
		createCmd,
	}, nil); err != nil {
		return fmt.Errorf("failed to create database with seeder: %w", err)
	}

	// 5. Wait for database to come online
	logrus.Info("Waiting for database to come online...")
	for i := 0; i < 100; i++ {
		output, err := iops.Exec("database", []string{
			"cypher-shell", "-u", iops.config.Neo4jUsername, "-p" + iops.config.Neo4jPassword,
			"-d", "system", "--format", "plain",
			"SHOW DATABASE " + iops.config.Neo4jDatabase + " YIELD currentStatus RETURN currentStatus",
		}, nil)
		if err == nil && strings.Contains(strings.ToLower(output), "online") {
			logrus.Info("Database is online")
			break
		}
		time.Sleep(2 * time.Second)
	}

	logrus.Info("Neo4j cluster restore completed successfully")
	return nil
}

func (iops *InfrahubOps) restoreNeo4jCommunity(restoreMigrateFormat bool) (retErr error) {
	logrus.Info("Restoring Neo4j database (Community Edition dump)...")

	pidStr, err := iops.readNeo4jPID()
	if err != nil {
		return err
	}

	err = iops.stopNeo4jCommunity(pidStr)
	if err != nil {
		return err
	}

	defer func() {
		if _, err := iops.Exec("database", []string{"rm", "-rf", "/tmp/infrahubops"}, nil); err != nil {
			logrus.Warnf("Failed to cleanup temporary Neo4j backup data: %v", err)
		}
		if _, err := iops.Exec("database", []string{"rm", "-f", neo4jRemoteWatchdogBinary, neo4jRemoteWatchdogReady, neo4jRemoteWatchdogLog}, nil); err != nil {
			logrus.Debugf("Failed to remove watchdog artifacts: %v", err)
		}
		if _, err := iops.Exec("database", []string{"kill", "-CONT", pidStr}, nil); err != nil {
			logrus.Errorf("Failed to send SIGCONT to neo4j (pid %s): %v", pidStr, err)
			if retErr == nil {
				retErr = fmt.Errorf("failed to resume neo4j process: %w", err)
			}
		}
	}()

	opts := iops.getNeo4jExecOptions()
	if output, err := iops.Exec(
		"database",
		[]string{"neo4j-admin", "database", "load", "--overwrite-destination=true", "--from-path=/tmp/infrahubops", iops.config.Neo4jDatabase},
		opts,
	); err != nil {
		return fmt.Errorf("failed to load neo4j dump: %w\nOutput: %v", err, output)
	}

	if restoreMigrateFormat {
		if output, err := iops.Exec(
			"database",
			[]string{"neo4j-admin", "database", "migrate", "--to-format=block", iops.config.Neo4jDatabase},
			opts,
		); err != nil {
			return fmt.Errorf("failed to migrate neo4j to block format: %w\nOutput: %v", err, output)
		}
	}

	logrus.Info("Neo4j dump restored successfully")
	return nil
}

func (iops *InfrahubOps) readNeo4jPID() (string, error) {
	output, err := iops.Exec("database", []string{"cat", neo4jPIDFile}, nil)
	if err != nil {
		return "", fmt.Errorf("failed to read neo4j pid file: %w", err)
	}
	pid := strings.TrimSpace(output)
	if pid == "" {
		return "", fmt.Errorf("neo4j pid file is empty")
	}
	if _, err := strconv.Atoi(pid); err != nil {
		return "", fmt.Errorf("invalid pid %q: %w", pid, err)
	}
	return pid, nil
}

func (iops *InfrahubOps) detectNeo4jArchitecture() (string, error) {
	output, err := iops.Exec("database", []string{"uname", "-m"}, nil)
	if err != nil {
		return "", fmt.Errorf("failed to detect neo4j architecture: %w", err)
	}
	arch := strings.TrimSpace(output)
	if arch == "" {
		return "", fmt.Errorf("empty architecture string")
	}
	return arch, nil
}

// getNeo4jExecOptions returns ExecOptions with User set to "neo4j" only if not already running as neo4j
func (iops *InfrahubOps) getNeo4jExecOptions() *ExecOptions {
	output, err := iops.Exec("database", []string{"whoami"}, nil)
	if err == nil && strings.TrimSpace(output) == "neo4j" {
		return nil
	}
	return &ExecOptions{User: "neo4j"}
}

// isNeo4jCluster checks if Neo4j is running in cluster mode by counting servers
func (iops *InfrahubOps) isNeo4jCluster() bool {
	output, err := iops.Exec("database", []string{
		"cypher-shell",
		"-u", iops.config.Neo4jUsername,
		"-p" + iops.config.Neo4jPassword,
		"-d", "system",
		"--format", "plain",
		"SHOW SERVERS YIELD * RETURN count(*) as serverCount",
	}, nil)
	if err != nil {
		return false // Assume not clustered if query fails
	}
	// Parse server count - if > 1, it's a cluster
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) >= 2 {
		count, _ := strconv.Atoi(strings.TrimSpace(lines[len(lines)-1]))
		return count > 1
	}
	return false
}
