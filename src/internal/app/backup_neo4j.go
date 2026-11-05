package app

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

const (
	neo4jTempBackupDir       = "/tmp/infrahubops"
	neo4jWatchdogInitTimeout = 5 * time.Second
	neo4jProcessStopTimeout  = 120 * time.Second
	neo4jMetadataScriptPath  = "/data/scripts/neo4j/restore_metadata.cypher"

	// DefaultNeo4jClusterOnlineTimeout leaves time for the other cluster servers to copy a
	// store of several gigabytes from the seed server.
	DefaultNeo4jClusterOnlineTimeout = 10 * time.Minute
	neo4jClusterOnlinePollInterval   = 5 * time.Second
	neo4jDatabaseStatusOnline        = "online"

	neo4jLocalBoltAddress       = "bolt://localhost:7687"
	neo4jSeedServerOption       = "existingDataSeedServer"
	neo4jLegacySeedServerOption = "existingDataSeedInstance"
)

// backupNeo4jEnterpriseStream returns a data factory that streams a tar archive of the Neo4j
// Enterprise backup directory from the container via exec stdout.
// The backup is created with --compress=false for better Plakar deduplication.
func (iops *InfrahubOps) backupNeo4jEnterpriseStream(backupMetadata string) (func() (io.ReadCloser, error), error) {
	return func() (io.ReadCloser, error) {
		cleanupBackupDir := func() {
			if _, err := iops.Exec("database", []string{"rm", "-rf", neo4jTempBackupDir}, nil); err != nil {
				logrus.Warnf("Failed to remove temporary Neo4j backup directory: %v", err)
			}
		}

		// Prepare backup directory
		if _, err := iops.Exec("database", []string{"sh", "-c",
			fmt.Sprintf("rm -rf %s && mkdir -p %s", neo4jTempBackupDir, neo4jTempBackupDir),
		}, nil); err != nil {
			return nil, fmt.Errorf("failed to prepare neo4j backup directory: %w", err)
		}

		// Run backup command separately so its stdout logs don't contaminate the data stream
		if output, err := iops.Exec("database", []string{
			"neo4j-admin", "database", "backup",
			"--expand-commands",
			"--include-metadata=" + backupMetadata,
			"--compress=false",
			"--to-path=" + neo4jTempBackupDir,
			iops.config.Neo4jDatabase,
		}, nil); err != nil {
			cleanupBackupDir()
			return nil, fmt.Errorf("failed to backup neo4j: %w\nOutput: %v", err, output)
		}

		// Stream only the tar archive — no other command output in the pipe
		stdout, wait, err := iops.ExecStreamPipe("database", []string{"tar", "cf", "-", "-C", "/tmp", "infrahubops"}, nil)
		if err != nil {
			cleanupBackupDir()
			return nil, fmt.Errorf("failed to start neo4j enterprise stream: %w", err)
		}

		return &execReadCloser{reader: stdout, wait: wait, idleTimeout: defaultStreamIdleTimeout, cleanup: cleanupBackupDir}, nil
	}, nil
}

// backupNeo4jCommunityStream returns a data factory that streams the Neo4j Community dump
// directly from the container via exec stdout using --to-stdout.
// Community edition requires stopping neo4j before dumping; the process is resumed
// when the returned ReadCloser is closed.
func (iops *InfrahubOps) backupNeo4jCommunityStream() (func() (io.ReadCloser, error), error) {
	return func() (io.ReadCloser, error) {
		restoreNeo4j := func(pidStr string) {
			if _, err := iops.Exec("database", []string{"rm", "-f", neo4jRemoteWatchdogBinary, neo4jRemoteWatchdogReady, neo4jRemoteWatchdogLog}, nil); err != nil {
				logrus.Debugf("Failed to remove watchdog artifacts: %v", err)
			}
			if _, err := iops.Exec("database", []string{"kill", "-CONT", pidStr}, nil); err != nil {
				logrus.Errorf("Failed to send SIGCONT to neo4j (pid %s): %v", pidStr, err)
			}
		}

		pidStr, err := iops.readNeo4jPID()
		if err != nil {
			return nil, err
		}

		if err := iops.stopNeo4jCommunity(pidStr); err != nil {
			return nil, err
		}

		// Stream the dump directly to stdout — no temp files needed
		stdout, wait, err := iops.ExecStreamPipe("database", []string{
			"neo4j-admin", "database", "dump",
			"--to-stdout",
			iops.config.Neo4jDatabase,
		}, nil)
		if err != nil {
			restoreNeo4j(pidStr)
			return nil, fmt.Errorf("failed to start neo4j community stream: %w", err)
		}

		// Neo4j is resumed when the stream is closed (after reading completes)
		return &execReadCloser{reader: stdout, wait: wait, idleTimeout: defaultStreamIdleTimeout, cleanup: func() {
			restoreNeo4j(pidStr)
		}}, nil
	}, nil
}

// defaultStreamIdleTimeout is the default duration after which a streaming backup
// is considered stalled if no data has been read.
const defaultStreamIdleTimeout = 30 * time.Minute

// execReadCloser wraps an exec stdout pipe with cleanup logic and idle timeout.
type execReadCloser struct {
	reader      io.ReadCloser
	wait        func() error
	cleanup     func()
	closed      bool
	timedOut    bool
	idleTimeout time.Duration // 0 = no timeout
	timer       *time.Timer   // reusable timer for idle timeout
}

var errStreamIdleTimeout = fmt.Errorf("stream idle timeout")

func (e *execReadCloser) Read(p []byte) (int, error) {
	if e.timedOut {
		return 0, errStreamIdleTimeout
	}
	if e.idleTimeout <= 0 {
		return e.reader.Read(p)
	}

	type readResult struct {
		n   int
		err error
	}
	ch := make(chan readResult, 1)
	go func() {
		n, err := e.reader.Read(p)
		ch <- readResult{n, err}
	}()

	// Lazily create the timer on first use, reset on subsequent calls
	if e.timer == nil {
		e.timer = time.NewTimer(e.idleTimeout)
	} else {
		if !e.timer.Stop() {
			select {
			case <-e.timer.C:
			default:
			}
		}
		e.timer.Reset(e.idleTimeout)
	}

	select {
	case res := <-ch:
		return res.n, res.err
	case <-e.timer.C:
		e.timedOut = true
		// Close the underlying reader to unblock the goroutine
		e.reader.Close()
		return 0, fmt.Errorf("stream idle timeout after %v with no data", e.idleTimeout)
	}
}

func (e *execReadCloser) Close() error {
	if e.closed {
		return nil
	}
	e.closed = true

	// Stop the idle timer if active
	if e.timer != nil {
		e.timer.Stop()
	}

	// Close the reader first (may signal EOF to the process)
	readErr := e.reader.Close()

	// Wait for the process to finish
	var waitErr error
	if e.wait != nil {
		waitErr = e.wait()
	}

	// Run cleanup
	if e.cleanup != nil {
		e.cleanup()
	}

	if waitErr != nil {
		return waitErr
	}
	return readErr
}

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

	if _, err := iops.Exec("database", []string{"mkdir", "-p", neo4jTempBackupDir}, nil); err != nil {
		return fmt.Errorf("failed to create backup directory: %w", err)
	}
	defer func() {
		if _, err := iops.Exec("database", []string{"rm", "-rf", neo4jTempBackupDir}, nil); err != nil {
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

	if err := iops.CopyFrom("database", neo4jTempBackupDir, filepath.Join(backupDir, "database")); err != nil {
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

	if err := iops.waitForRemoteFile(neo4jRemoteWatchdogReady, neo4jWatchdogInitTimeout); err != nil {
		return fmt.Errorf("watchdog failed to initialize: %w", err)
	}

	if _, err := iops.Exec("database", []string{"kill", pidStr}, nil); err != nil {
		return fmt.Errorf("failed to stop neo4j: %w", err)
	}

	logrus.Info("Waiting for Neo4j process to stop...")
	if err := iops.waitForProcessStopped(pidStr, neo4jProcessStopTimeout); err != nil {
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

	dumpFilename := fmt.Sprintf("%s.dump", iops.config.Neo4jDatabase)
	remoteDumpPath := neo4jRemoteWorkDir + "/" + dumpFilename

	// The dump is a byproduct of this backup, and it lands in the very directory a later
	// restore copies an archive's dump into. Left behind, it is a complete, loadable
	// archive of *this* deployment sitting where the loader looks — so the archive an
	// operator names for a restore is not necessarily what gets loaded. Removing it is
	// half of that fix; restoreNeo4j clearing the directory before it copies is the other.
	//
	// Deferred, and registered before the dump runs, so a dump that was written but could
	// not be copied out is cleaned up as well. The deferred watchdog cleanup below it
	// removes the artifacts that share this directory, so a plain `rm -f` of the dump is
	// what keeps the two from stepping on each other.
	defer func() {
		if _, err := iops.Exec("database", []string{"rm", "-f", remoteDumpPath}, nil); err != nil {
			logrus.Warnf("Failed to remove the temporary Neo4j dump %s: %v", remoteDumpPath, err)
		}
	}()

	dumpCmd := []string{
		"neo4j-admin", "database", "dump",
		"--overwrite-destination=true",
		"--to-path=" + neo4jRemoteWorkDir,
		iops.config.Neo4jDatabase,
	}
	if output, dumpErr := iops.Exec("database", dumpCmd, nil); dumpErr != nil {
		return fmt.Errorf("failed to dump neo4j database: %w\nOutput: %v", dumpErr, output)
	}

	if err := iops.CopyFrom("database", remoteDumpPath, filepath.Join(databaseDir, dumpFilename)); err != nil {
		return fmt.Errorf("failed to copy neo4j dump: %w", err)
	}

	logrus.Info("Neo4j dump completed")
	return nil
}

func (iops *InfrahubOps) restoreNeo4j(workDir, neo4jEdition string, restoreMigrateFormat bool) error {
	backupPath := filepath.Join(workDir, "backup", "database")

	// Clear the destination before copying into it, and abort if it cannot be cleared.
	// Neither backend replaces a directory that already exists: `docker cp` copies the
	// source *inside* it, `kubectl cp` merges *into* it. So whatever an earlier run left
	// there decides where the archive's dump ends up and which dump the loader then reads
	// — the archive's, or the leftover one. Removing the directory first makes the copy's
	// shape the same on both backends and on every run, which is what makes the dump that
	// is loaded necessarily the one from the archive that was asked for.
	//
	// It also means the cleanup below is best-effort in fact as well as in name: a failed
	// removal can no longer decide a later restore.
	if _, err := iops.Exec("database", []string{"rm", "-rf", neo4jTempBackupDir}, nil); err != nil {
		return fmt.Errorf("failed to clear the temporary Neo4j restore directory %s: %w", neo4jTempBackupDir, err)
	}

	if err := iops.CopyTo("database", backupPath, neo4jTempBackupDir); err != nil {
		return fmt.Errorf("failed to copy backup to container: %w", err)
	}
	defer func() {
		// The community path removes this directory itself, and a container that is
		// restarting after the restore refuses the exec outright, so a failure here is
		// ordinary rather than alarming — and harmless, because the next restore clears the
		// directory before it copies.
		if _, err := iops.Exec("database", []string{"rm", "-rf", neo4jTempBackupDir}, nil); err != nil {
			logrus.Warnf("Failed to remove the temporary Neo4j restore directory %s; it is cleared again before the next restore copies into it: %v", neo4jTempBackupDir, err)
		}
	}()

	if _, err := iops.Exec("database", []string{"chown", "-R", "neo4j:neo4j", neo4jTempBackupDir}, nil); err != nil {
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
		[]string{"neo4j-admin", "database", "restore", "--expand-commands", "--overwrite-destination=true", "--from-path=" + neo4jTempBackupDir, iops.config.Neo4jDatabase},
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
		[]string{"sh", "-c", "cat " + neo4jMetadataScriptPath + " | cypher-shell -u " + iops.config.Neo4jUsername + " -p" + iops.config.Neo4jPassword + " -d system --param \"database => '" + iops.config.Neo4jDatabase + "'\""},
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
		"--from-path=" + neo4jTempBackupDir,
		iops.config.Neo4jDatabase,
	}, opts); err != nil {
		return fmt.Errorf("failed to restore neo4j: %w\nOutput: %v", err, output)
	}

	// 3. Get current node's serverId using dbms.cluster.statusCheck()
	logrus.Info("Getting current server ID...")
	serverIdOutput, err := iops.Exec("database", []string{
		"cypher-shell", "-u", iops.config.Neo4jUsername, "-p" + iops.config.Neo4jPassword,
		// A routed connection runs the procedure on the system database leader, which then
		// reports itself as the requester even though the restored store is on this server.
		"-a", neo4jLocalBoltAddress,
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
	if err := iops.createSeededNeo4jDatabase(serverId); err != nil {
		return err
	}

	// 5. Wait for database to come online
	timeout := iops.config.Neo4jClusterOnlineTimeout
	logrus.Infof("Waiting up to %s for database %s to come online on every server...", timeout, iops.config.Neo4jDatabase)
	if err := iops.waitForNeo4jClusterDatabaseOnline(timeout, neo4jClusterOnlinePollInterval); err != nil {
		return err
	}

	logrus.Info("Neo4j cluster restore completed successfully")
	return nil
}

// createSeededNeo4jDatabase creates the database on every server from the store on the seed
// server. Cypher 25 refuses existingDataSeedInstance and requires existingDataSeedServer; the
// older name is tried first because Neo4j releases that predate the newer name accept only it.
func (iops *InfrahubOps) createSeededNeo4jDatabase(seedServerID string) error {
	output, err := iops.Exec("database", iops.createSeededNeo4jDatabaseCommand(neo4jLegacySeedServerOption, seedServerID), nil)
	refused := "invalid input '" + strings.ToLower(neo4jLegacySeedServerOption) + "'"
	if err != nil && strings.Contains(strings.ToLower(output), refused) {
		logrus.Infof("Neo4j does not accept the %s option; creating the database with %s instead",
			neo4jLegacySeedServerOption, neo4jSeedServerOption)
		output, err = iops.Exec("database", iops.createSeededNeo4jDatabaseCommand(neo4jSeedServerOption, seedServerID), nil)
	}
	if err != nil {
		return fmt.Errorf("failed to create database with seeder: %w\nOutput: %s", err, strings.TrimSpace(output))
	}
	return nil
}

func (iops *InfrahubOps) createSeededNeo4jDatabaseCommand(seedOption, seedServerID string) []string {
	return []string{
		"cypher-shell", "-u", iops.config.Neo4jUsername, "-p" + iops.config.Neo4jPassword,
		"-d", "system",
		fmt.Sprintf(`CREATE DATABASE %s
TOPOLOGY 3 PRIMARIES
OPTIONS {
  existingData: 'use',
  %s: '%s'
}`, iops.config.Neo4jDatabase, seedOption, seedServerID),
	}
}

// neo4jServerDatabaseStatus is the state of the restored database on one cluster server.
type neo4jServerDatabaseStatus struct {
	Address string
	Status  string
	Message string
}

func (s neo4jServerDatabaseStatus) String() string {
	if s.Message == "" {
		return s.Address + ": " + s.Status
	}
	return s.Address + ": " + s.Status + " (" + s.Message + ")"
}

// waitForNeo4jClusterDatabaseOnline returns nil once every server that hosts the database
// reports it online, and an error naming each server's status once the timeout has passed.
func (iops *InfrahubOps) waitForNeo4jClusterDatabaseOnline(timeout, interval time.Duration) error {
	database := iops.config.Neo4jDatabase
	deadline := time.Now().Add(timeout)

	var lastServers []neo4jServerDatabaseStatus
	var lastSummary string
	var lastErr error
	var lastErrText string
	for {
		servers, err := iops.neo4jClusterDatabaseStatus()
		lastErr = err
		// Both kinds of line are logged only when they change, so a long store copy shows
		// progress without repeating the same line every poll.
		if err != nil {
			if err.Error() != lastErrText {
				logrus.Warnf("Failed to read the status of database %s: %v", database, err)
				lastErrText = err.Error()
			}
		} else {
			lastErrText = ""
			if neo4jDatabaseOnlineOnEveryServer(servers) {
				logrus.Infof("Database %s is online on %d servers", database, len(servers))
				return nil
			}
			lastServers = servers
			if summary := describeNeo4jServers(servers); summary != lastSummary {
				logNeo4jDatabaseProgress(database, servers, summary)
				lastSummary = summary
			}
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		time.Sleep(min(interval, remaining))
	}

	logNeo4jDatabaseNotOnline(database, lastServers)
	if lastErr != nil {
		if lastSummary != "" {
			return fmt.Errorf("database %s did not come online on every Neo4j server within %s (last status read: %s); reading its status failed: %w",
				database, timeout, lastSummary, lastErr)
		}
		return fmt.Errorf("database %s did not come online on every Neo4j server within %s; reading its status failed: %w",
			database, timeout, lastErr)
	}
	return fmt.Errorf("database %s did not come online on every Neo4j server within %s: %s; if a server is still copying the store, raise --neo4j-cluster-online-timeout",
		database, timeout, lastSummary)
}

// logNeo4jDatabaseProgress logs at warning level once a server reports a status message,
// because Neo4j sets one when the database fails to start there.
func logNeo4jDatabaseProgress(database string, servers []neo4jServerDatabaseStatus, summary string) {
	for _, server := range servers {
		if server.Message != "" {
			logrus.Warnf("Database %s is not online on every server yet: %s", database, summary)
			return
		}
	}
	logrus.Infof("Database %s is not online on every server yet: %s", database, summary)
}

// logNeo4jDatabaseNotOnline logs one line for each server where the database is not online,
// with the reason Neo4j gives for it.
func logNeo4jDatabaseNotOnline(database string, servers []neo4jServerDatabaseStatus) {
	for _, server := range servers {
		if strings.EqualFold(server.Status, neo4jDatabaseStatusOnline) {
			continue
		}
		reason := server.Message
		if reason == "" {
			reason = "Neo4j gave no status message"
		}
		logrus.WithFields(logrus.Fields{
			"server": server.Address,
			"status": server.Status,
			"reason": reason,
		}).Errorf("Database %s is not online on this server", database)
	}
}

func (iops *InfrahubOps) neo4jClusterDatabaseStatus() ([]neo4jServerDatabaseStatus, error) {
	output, err := iops.Exec("database", []string{
		"cypher-shell", "-u", iops.config.Neo4jUsername, "-p" + iops.config.Neo4jPassword,
		"-d", "system", "--format", "plain",
		"SHOW DATABASE " + iops.config.Neo4jDatabase +
			" YIELD currentStatus, address, statusMessage" +
			" RETURN currentStatus, coalesce(address, '') AS address, coalesce(statusMessage, '') AS statusMessage",
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("%w\nOutput: %s", err, strings.TrimSpace(output))
	}
	return parseNeo4jDatabaseStatus(output), nil
}

// parseNeo4jDatabaseStatus reads the plain cypher-shell output of the status query: a header
// line, then one row per server of quoted strings, with the message last because it can
// contain commas.
func parseNeo4jDatabaseStatus(output string) []neo4jServerDatabaseStatus {
	var servers []neo4jServerDatabaseStatus
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		// Only rows start with a quote; the header and any notice printed with them do not.
		if !strings.HasPrefix(line, `"`) {
			continue
		}
		fields := strings.SplitN(line, ", ", 3)
		if len(fields) != 3 {
			continue
		}
		servers = append(servers, neo4jServerDatabaseStatus{
			Status:  unquoteCypherString(fields[0]),
			Address: unquoteCypherString(fields[1]),
			Message: unquoteCypherString(fields[2]),
		})
	}
	return servers
}

func unquoteCypherString(value string) string {
	if unquoted, err := strconv.Unquote(value); err == nil {
		return unquoted
	}
	return strings.Trim(value, `"`)
}

func neo4jDatabaseOnlineOnEveryServer(servers []neo4jServerDatabaseStatus) bool {
	if len(servers) == 0 {
		return false
	}
	for _, server := range servers {
		if !strings.EqualFold(server.Status, neo4jDatabaseStatusOnline) {
			return false
		}
	}
	return true
}

func describeNeo4jServers(servers []neo4jServerDatabaseStatus) string {
	if len(servers) == 0 {
		return "no server hosts the database"
	}
	descriptions := make([]string, 0, len(servers))
	for _, server := range servers {
		descriptions = append(descriptions, server.String())
	}
	return strings.Join(descriptions, "; ")
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
		if _, err := iops.Exec("database", []string{"rm", "-rf", neo4jTempBackupDir}, nil); err != nil {
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
		[]string{"neo4j-admin", "database", "load", "--overwrite-destination=true", "--from-path=" + neo4jTempBackupDir, iops.config.Neo4jDatabase},
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

// restoreNeo4jCommunityStream restores a Neo4j Community dump by streaming the data
// directly from the provided reader into `neo4j-admin database load --from-stdin`.
// This avoids copying dump files to a temporary directory on the container.
func (iops *InfrahubOps) restoreNeo4jCommunityStream(reader io.ReadCloser, restoreMigrateFormat bool) (retErr error) {
	logrus.Info("Restoring Neo4j database (Community Edition streamed load)...")

	pidStr, err := iops.readNeo4jPID()
	if err != nil {
		return err
	}

	if err := iops.stopNeo4jCommunity(pidStr); err != nil {
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

	opts := iops.getNeo4jExecOptions()

	wait, err := iops.ExecWritePipe(
		"database",
		[]string{"neo4j-admin", "database", "load", "--from-stdin", "--overwrite-destination=true", iops.config.Neo4jDatabase},
		opts,
		reader,
	)
	if err != nil {
		return fmt.Errorf("failed to start neo4j streamed load: %w", err)
	}

	if err := wait(); err != nil {
		return fmt.Errorf("failed to load neo4j dump from stream: %w", err)
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

	logrus.Info("Neo4j streamed restore completed successfully")
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
func (iops *InfrahubOps) redactDatabase() error {
	logrus.Warn("Redacting attribute values in the database. This operation is destructive and irreversible!")

	query := `MATCH (av:AttributeValue) WITH av.value AS av_value, collect(av) AS av_verts WITH av_value, av_verts, randomUUID() as new_value CALL (av_value, new_value, av_verts) { UNWIND av_verts AS av SET av.value = new_value } IN TRANSACTIONS`

	if _, err := iops.Exec("database", []string{
		"cypher-shell",
		"-u", iops.config.Neo4jUsername,
		"-p" + iops.config.Neo4jPassword,
		"-d", iops.config.Neo4jDatabase,
		"--format", "plain",
		query,
	}, nil); err != nil {
		return fmt.Errorf("failed to redact database: %w", err)
	}

	logrus.Info("Database redaction completed")
	return nil
}

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
