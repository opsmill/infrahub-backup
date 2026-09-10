package app

import (
	"errors"
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
)

// backupNeo4jEnterpriseStream returns a data factory that streams a tar archive of the Neo4j
// Enterprise backup directory from the container via exec stdout.
// The backup is created with --compress=false for better Plakar deduplication.
func (iops *InfrahubOps) backupNeo4jEnterpriseStream(backupMetadata string) (func() (io.ReadCloser, error), error) {
	return func() (io.ReadCloser, error) {
		// The branch is here rather than where the factory is built, so that the
		// transient workload's life covers the stream rather than starting when
		// the factory was merely constructed. A nil source is an internal
		// database, and everything below is the path it has always taken.
		source, err := iops.externalDatabaseFor(serviceNeo4j)
		if err != nil {
			return nil, err
		}
		if source != nil {
			return iops.streamNeo4jEnterpriseExternal(backupMetadata)
		}

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
		if output, err := iops.Exec("database", neo4jCaptureCommand(neo4jCaptureRequest{
			Database:       iops.config.Neo4jDatabase,
			BackupMetadata: backupMetadata,
			ToPath:         neo4jTempBackupDir,
			Uncompressed:   true,
		}), nil); err != nil {
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
	source, err := iops.externalDatabaseFor(serviceNeo4j)
	if err != nil {
		return err
	}
	if source != nil {
		return iops.backupNeo4jEnterpriseExternal(backupDir, backupMetadata)
	}

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
		neo4jCaptureCommand(neo4jCaptureRequest{
			Database:       iops.config.Neo4jDatabase,
			BackupMetadata: backupMetadata,
			ToPath:         neo4jTempBackupDir,
		}),
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
	// Before the copy into the container, because for a database outside the
	// deployment there is no container to copy into and nothing to copy: the
	// server fetches the artifact itself from where the pre-flight staged it.
	// A nil restore is an in-deployment database and takes the path below
	// unchanged (FR-015).
	restore, err := iops.externalRestoreFor(serviceNeo4j)
	if err != nil {
		return err
	}
	if restore != nil {
		return iops.restoreNeo4jExternal(restore)
	}

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
	clustered, err := iops.isNeo4jCluster()
	if err != nil {
		return err
	}
	if clustered {
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
	// The Plakar paths reach this without going through restoreNeo4j, so the
	// dispatch that sends a database outside the deployment to the seed path is
	// not on this route. Everything below suspends the database's own process
	// and writes into its store, which needs a container this database does not
	// have.
	//
	// Both Plakar entry points now refuse this at their gate, before anything
	// is stopped, so this is the backstop rather than the refusal — it is the
	// last place the guarantee can be held, and a path added later could reach
	// it without passing a gate. See refuseExternalCommunityDumpRestore.
	restore, err := iops.externalRestoreFor(serviceNeo4j)
	if err != nil {
		return err
	}
	if restore != nil {
		return refuseExternalDumpRestoreAfterQuiescing(restore.Endpoint)
	}

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

// redactDatabase replaces every attribute value in the database with a random
// UUID. It runs `cypher-shell` in the `database` container, which is what makes
// it an in-deployment operation: it modifies the source database in place,
// before the capture reads it.
//
// A database that lives outside the deployment has no such container, and the
// exec used to be attempted anyway — producing "no pods found for service
// database in namespace …", which is the exact misdiagnosis FR-008's refusal
// exists to remove. So the refusal is stated here too, in the terms an operator
// can act on: nothing is missing from the deployment, and this build has no way
// to redact a database it does not host.
func (iops *InfrahubOps) redactDatabase() error {
	source, err := iops.externalDatabaseFor(serviceNeo4j)
	if err != nil {
		return err
	}
	if source != nil {
		return fmt.Errorf(
			"cannot redact %s: redaction rewrites the database in place through the %s container, and there is none here because the database is managed outside this deployment. "+
				"Nothing is missing from the deployment. Take the backup without --redact, or redact the data at its source. "+
				"No Infrahub service was stopped and no data was changed",
			source.Endpoint.endpointTarget(), serviceNeo4j)
	}

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

// isNeo4jCluster reports whether Neo4j runs in cluster mode.
//
// For a database inside the deployment it counts the servers the system
// database reports, and a query that fails is read as "not clustered" — the
// answer only selects between two restore procedures, and degrading is what
// this has always done.
//
// For one outside it, the count was already taken: the probe enumerated the
// members before anything was stopped, and asking again would mean exec'ing
// `cypher-shell` in a `database` container that is not there — the same
// misdiagnosis redactDatabase carried. Only the location failing is fatal here,
// because that is the one thing this cannot degrade around: it decides which of
// the two answers is even being computed.
func (iops *InfrahubOps) isNeo4jCluster() (bool, error) {
	source, err := iops.externalDatabaseFor(serviceNeo4j)
	if err != nil {
		return false, err
	}
	if source != nil {
		return len(source.Facts.Members) > 1, nil
	}

	output, err := iops.Exec("database", []string{
		"cypher-shell",
		"-u", iops.config.Neo4jUsername,
		"-p" + iops.config.Neo4jPassword,
		"-d", "system",
		"--format", "plain",
		"SHOW SERVERS YIELD * RETURN count(*) as serverCount",
	}, nil)
	if err != nil {
		return false, nil // Assume not clustered if query fails
	}
	// Parse server count - if > 1, it's a cluster
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) >= 2 {
		count, _ := strconv.Atoi(strings.TrimSpace(lines[len(lines)-1]))

		return count > 1, nil
	}

	return false, nil
}

// ---------------------------------------------------------------------------
// Capturing a Neo4j database that lives outside the deployment
// ---------------------------------------------------------------------------

// The online backup command is a client: the vendor's own documented topology
// is to run it "from a server on the same network as the database, but that is
// not part of the cluster" (research R1). The transient workload is that
// machine, so the capture below is the same command the in-container path runs,
// with the one addition that turns it from a local operation into a remote one —
// the ordered --from list of members to try.
//
// Everything else about the two paths is deliberately identical, including the
// order the flags are written in and the name of the directory the artifact
// lands in, because an artifact taken from an external database has to be
// restorable into an internal deployment and vice versa (FR-015, and the
// artifact contract). neo4jCaptureCommand is what makes that a shared fact
// rather than two implementations that agree today.

const (
	// externalNeo4jCaptureName is the directory the artifact lands in, and it
	// is the internal path's basename rather than a name of its own: the
	// archive assembled from it is a tar of this directory, so a different name
	// here would produce an archive whose entries a restore does not recognise.
	externalNeo4jCaptureName = "infrahubops"

	// externalNeo4jCaptureParent is the scratch volume the capture directory
	// sits in — the volume FR-022 sizes — rather than the node's own disk,
	// which a large database filling would take neighbouring workloads with it.
	externalNeo4jCaptureParent = transientScratchPath

	externalNeo4jCaptureDir = externalNeo4jCaptureParent + "/" + externalNeo4jCaptureName

	// externalNeo4jStagingDir is the --temp-path the command stages the store
	// and transaction logs into before producing the artifact beside them. It
	// is named explicitly because the vendor's default is the working
	// directory, and "the temporary directory must therefore have enough free
	// space to hold the entire backup" — so leaving it implicit puts the
	// staging copy somewhere this run has not sized (research R5).
	externalNeo4jStagingDir = transientScratchPath + "/staging"
)

// neo4jCaptureRequest is everything the online backup command needs. It exists
// so the argv is built once for both the internal and the external path.
type neo4jCaptureRequest struct {
	// Database is the database to capture, which names the artifact.
	Database string

	// BackupMetadata is the --include-metadata value.
	BackupMetadata string

	// ToPath is the directory the artifact is placed in.
	ToPath string

	// TempPath is the staging directory. Empty leaves the flag off, which is
	// what the in-container path does — it stages inside the database's own
	// container, where the vendor's default is already the right place.
	TempPath string

	// From is the ordered member list. Empty leaves the flag off entirely,
	// which is what makes the command an in-container capture of the local
	// store; a member list makes the same command a remote one (FR-005).
	From []HostPort

	// Uncompressed emits --compress=false, which the streaming path asks for
	// so the artifact deduplicates.
	Uncompressed bool
}

// neo4jCaptureCommand builds the online backup argv.
//
// The flag order is the order the in-container path has always written, and each
// addition is appended only when its field is set, so a request with no member
// list, no staging path and no compression override produces the exact argv that
// path produced before this feature existed. That equality is asserted by test
// rather than left as an intention (FR-015).
//
// `--expand-commands` is emitted only for the in-container capture, and the
// reason is a property of the vendor's own tooling rather than a preference.
// Given that flag, `neo4j-admin` refuses to read any configuration file whose
// permissions are looser than 0640 — and it reads both `neo4j.conf` and
// `neo4j-admin.conf`, so tightening one is not enough. The images this tool
// pins for the transient workload ship them world-writable: `neo4j:5-enterprise`
// has `/var/lib/neo4j/conf/neo4j.conf` at 0777. Passing the flag there aborts
// the capture *before the database is contacted*, reporting a file permission
// the operator never set, in an image the operator did not build.
//
// Nothing is lost by omitting it. The flag exists so that a deployment's own
// configuration may hold command-valued settings for `neo4j-admin` to expand,
// and the transient workload has no such configuration — it is a stock image
// holding only the credentials it was given and the endpoint it was told to
// read. The in-container path does run against the deployment's config, so it
// keeps the flag, which is also what keeps its argv byte-identical (FR-015).
//
// Verified against real servers on 2026-09-03: with the flag, `neo4j:5-enterprise`
// fails on config permissions; without it, the same argv completes a full remote
// capture from a live backup listener and writes
// `neo4j-<timestamp>.backup`.
func neo4jCaptureCommand(req neo4jCaptureRequest) []string {
	command := []string{
		"neo4j-admin", "database", "backup",
	}

	if len(req.From) == 0 {
		command = append(command, "--expand-commands")
	}

	command = append(command, "--include-metadata="+req.BackupMetadata)

	if req.Uncompressed {
		command = append(command, "--compress=false")
	}
	if len(req.From) > 0 {
		command = append(command, "--from="+renderHostPorts(req.From))
	}
	if req.TempPath != "" {
		command = append(command, "--temp-path="+req.TempPath)
	}

	return append(command, "--to-path="+req.ToPath, req.Database)
}

// externalNeo4jCaptureRequest is the request for a capture from outside the
// deployment.
//
// It carries no credential, and there is nothing missing: the backup service is
// reached over its own protocol on its own port, and where the probe statements
// need to authenticate they read NEO4J_USERNAME and NEO4J_PASSWORD from the
// environment the transient workload's secret provides. So no path here puts a
// password on a command line (FR-014).
func externalNeo4jCaptureRequest(capture *externalCapture, backupMetadata string, uncompressed bool) neo4jCaptureRequest {
	return neo4jCaptureRequest{
		Database:       capture.Endpoint.Database,
		BackupMetadata: backupMetadata,
		ToPath:         externalNeo4jCaptureDir,
		TempPath:       externalNeo4jStagingDir,
		From:           capture.captureTargets(),
		Uncompressed:   uncompressed,
	}
}

// errExternalCommunityCapture is the FR-008 refusal, as a value callers and
// tests can identify rather than a message they have to match.
var errExternalCommunityCapture = errors.New("the Community Edition backup mechanism requires direct access to the database's own storage")

// externalCommunityCaptureRefusal refuses to capture an external Community
// database, saying why the mechanism is unavailable.
//
// The wording matters more than usual here. Before this feature the same
// situation produced "service not found", and a customer reasonably concluded
// their deployment was broken and went looking for a missing container — so the
// message states that the mechanism needs the database's own storage, that the
// remote alternative is an Enterprise feature, and explicitly that nothing is
// missing from the deployment (FR-008, and the failure-message contract).
func externalCommunityCaptureRefusal(endpoint *DatabaseEndpoint) error {
	return fmt.Errorf(
		"cannot back up %s: %w, because it is an offline dump of the store files, and this run has no access to the storage of a server it does not host. "+
			"The online backup that does work remotely is a Neo4j Enterprise Edition feature. "+
			"Nothing is missing from the deployment — the %s service has no container here because the database is managed outside it. "+
			"No Infrahub service was stopped",
		endpoint.endpointTarget(), errExternalCommunityCapture, serviceNeo4j,
	)
}

// refuseExternalCommunityCapture refuses an external capture whose server
// reported an edition the remote mechanism cannot serve (FR-008).
//
// It is called from the gate, on the edition the probe read over Bolt, which is
// the only place the answer is both known and still free: one exec into a probe
// pod has been spent, and no Infrahub service has been stopped, no abort window
// announced, and no capture workload built. Deciding later would mean deciding
// after detectNeo4jEdition had already turned Community into
// RequiresOfflineCapture and scaled six services to zero for a dump this run was
// never going to be able to take.
//
// An edition the server did not report is not read as Community: the same
// asymmetry Neo4jEditionInfo documents applies here, and guessing the
// destructive branch from a probe that did not answer is what Principle II
// forbids. requireDeterminedEdition is what stops such a run, one step later.
func refuseExternalCommunityCapture(endpoint *DatabaseEndpoint, edition string) error {
	if !isCommunityEdition(edition) {
		return nil
	}

	return externalCommunityCaptureRefusal(endpoint)
}

// ---------------------------------------------------------------------------
// Deciding whether a capture captured everything (FR-012, research R6)
// ---------------------------------------------------------------------------

// The backup command's exit codes conflate two outcomes. Code 1 is "Backup
// failed, or succeeded but encountered problems such as some servers being
// uncontactable", and for several databases "One or several backups failed, or
// succeeded with problems". There is no code that separates them, and the
// vendor documents no machine-readable marker in the output either — the exit
// code table says only "See logs for more details".
//
// So completeness is established from two things the run can observe for itself:
// whether the command reported success, and whether the artifact it was asked
// for is actually there. Both are required. A non-zero exit is never complete,
// and a zero exit that produced no artifact is not complete either, which is the
// case a signal read from exit status alone would have called a success.
//
// What the output is used for is telling the operator which of the two outcomes
// they hit. Where the output names a server it could not reach, that line is
// quoted in the failure; where it does not, the failure says so rather than
// implying an outright failure it did not establish. The markers below therefore
// never decide anything — inventing a completeness signal the command does not
// emit is the failure mode this whole section exists to avoid.

// neo4jCaptureVerdict is what a capture established about itself.
type neo4jCaptureVerdict struct {
	// Complete is true only for a capture that reported success and left the
	// artifact it was asked for.
	Complete bool

	// ArtifactProduced records whether the artifact is there, which is what
	// separates "succeeded with problems" from "failed outright" on the
	// identical exit status they share.
	ArtifactProduced bool

	// Detail is the operator-facing account of an incomplete capture.
	Detail string
}

// neo4jUncontactableMarkers are phrasings that indicate the command could not
// reach a member. They corroborate a verdict already reached from the exit
// status and the artifact; none of them is the verdict, because the vendor
// documents no output contract for this and a phrasing that changes between
// releases must not be able to turn an incomplete capture into a complete one.
//
// None of them is attribution either. Every one of these phrasings is also how
// a cluster's own control plane reports that *it* is unreachable, so a marker
// counts only on a line that names an endpoint the run supplied — see
// uncontactableEvidence.
var neo4jUncontactableMarkers = []string{
	"uncontactable",
	"could not be contacted",
	"unable to connect",
	"connection refused",
	"did not respond",
	"not responding",
	"unreachable",
	"failed to connect",
}

// uncontactableEvidence returns the first line that names one of the endpoints
// the capture was asked to try and says it could not be reached, or the empty
// string when nothing does.
//
// It reads the command's stdout *and* the error, because the two carry
// different halves of what the command said: bounded execution returns stdout
// alone, and a failed command's stderr is folded into its error by withStderr.
// Reading only stdout — which is what an earlier version of this did, back when
// the two streams were merged — would look for the evidence in the one place a
// failing neo4j-admin does not write it.
//
// The endpoints are what make reading the error safe, and this is why they are
// a parameter. kubectl writes its *own* stderr into the same stream the remote
// command's goes to, and its transport failures are worded exactly like a
// database that will not answer: an API server that has stopped answering
// produces "Unable to connect to the server: dial tcp …: connection refused",
// which matches two of the markers below. So the whole of an API-server outage
// was reported to the operator as a named Neo4j endpoint the capture could not
// reach — a diagnosis pointing at the wrong system entirely, on a run that
// failed for a reason the cluster could have told them.
//
// Attribution is therefore positive rather than a list of transport phrasings
// to ignore: a line is evidence about this database only if it names a host the
// run supplied. On the host, not the whole address, because the supplied
// endpoint carries the backup listener's port and the server names its own. A
// genuine failure whose wording omits the host is not read as evidence, and
// that is the safe direction: the markers only ever corroborate a verdict
// already reached from the exit status and the artifact, so losing one costs
// the operator the endpoint's name in the message, never a capture reported
// complete when it was not.
func uncontactableEvidence(output string, runErr error, targets []HostPort) string {
	for _, line := range nonEmptyLines(output) {
		if namesAnUncontactableEndpoint(line, targets) {
			return strings.TrimSpace(line)
		}
	}

	if runErr == nil {
		return ""
	}

	for _, line := range nonEmptyLines(runErr.Error()) {
		if namesAnUncontactableEndpoint(line, targets) {
			return strings.TrimSpace(line)
		}
	}

	return ""
}

// namesAnUncontactableEndpoint reports whether one line says a supplied
// endpoint could not be reached.
func namesAnUncontactableEndpoint(line string, targets []HostPort) bool {
	lowered := strings.ToLower(strings.TrimSpace(line))

	named := false
	for _, target := range targets {
		host := strings.ToLower(strings.TrimSpace(target.Host))
		if host != "" && strings.Contains(lowered, host) {
			named = true

			break
		}
	}
	if !named {
		return false
	}

	for _, marker := range neo4jUncontactableMarkers {
		if strings.Contains(lowered, marker) {
			return true
		}
	}

	return false
}

// neo4jArtifactProduced reports whether a listing of the capture directory shows
// an artifact for the database that was asked for.
//
// The artifact is named `<database>-<timestamp>.backup`, and this reads all
// three parts. Matching the bare name as a prefix — which is what this did —
// lets a sibling database's `<database>2-….backup` satisfy the check, so a run
// that captured nothing of the database it was asked for records a complete
// capture. Requiring the separator fixes that case but not `<database>-staging`,
// whose own artifact also begins `<database>-`; so what follows the separator
// must additionally begin a timestamp rather than more name.
//
// Both of the assumptions here — the `.backup` suffix and a timestamp starting
// with its year — fail in the same direction if the vendor ever changes the
// name: the artifact is not recognised, the capture is reported incomplete, and
// the run fails. That is the direction Principle II asks for. The opposite
// reading is the one this whole section exists to prevent: a complete, loadable
// artifact of something else, sitting where the reader looks, reported as the
// backup that was asked for.
func neo4jArtifactProduced(database, listing string) bool {
	prefix := database + "-"

	for _, entry := range nonEmptyLines(listing) {
		if !strings.HasPrefix(entry, prefix) || !strings.HasSuffix(entry, ".backup") {
			continue
		}

		if remainder := entry[len(prefix):]; remainder != "" && remainder[0] >= '0' && remainder[0] <= '9' {
			return true
		}
	}

	return false
}

// classifyNeo4jCapture decides whether a capture captured everything it was
// asked for, from what the operation reported rather than from its exit status
// alone (FR-012).
//
// targets are the endpoints the capture was asked to try, and they are what
// make the "could not reach an endpoint" wording attributable to this database
// rather than to the cluster the command ran through (uncontactableEvidence).
func classifyNeo4jCapture(runErr error, output string, artifactProduced bool, targets []HostPort) neo4jCaptureVerdict {
	verdict := neo4jCaptureVerdict{ArtifactProduced: artifactProduced}
	evidence := uncontactableEvidence(output, runErr, targets)

	switch {
	case runErr == nil && artifactProduced:
		verdict.Complete = true

		// Reported, not acted on: a successful capture that also mentions an
		// unreachable member is a capture the vendor called successful, and
		// overriding that on a phrase match would refuse work the server did.
		if evidence != "" {
			logrus.Warnf("The capture reported success but its output mentions an endpoint it could not reach: %s", evidence)
		}

	case runErr == nil && !artifactProduced:
		verdict.Detail = "the operation reported success but left no artifact in the capture directory, so there is nothing to record as a backup"
		if line := firstOutputLine(output); line != "" {
			verdict.Detail += " (the command said: " + line + ")"
		}

	case artifactProduced && evidence != "":
		verdict.Detail = "the operation produced an artifact and then reported a problem, and its output names an endpoint it could not reach: " + evidence + ". A capture that reached only some of the endpoints supplied is not a complete capture, and its exit status is the same as an outright failure's, so it is treated as incomplete"

	case artifactProduced:
		verdict.Detail = "the operation produced an artifact and then reported a problem. Its exit status means either that the capture failed or that it succeeded against only some of the endpoints supplied, and no exit status distinguishes the two, so it is treated as incomplete. The server's own logs say which"

	case evidence != "":
		verdict.Detail = "the operation produced no artifact and its output names an endpoint it could not reach: " + evidence

	default:
		verdict.Detail = "the operation produced no artifact"
	}

	return verdict
}

// firstOutputLine is what the command said, for the one verdict that has no
// endpoint to name: a success that produced nothing should not be reported as a
// bare assertion when the command's own first line is there to quote.
func firstOutputLine(output string) string {
	lines := nonEmptyLines(output)
	if len(lines) == 0 {
		return ""
	}

	return lines[0]
}

// ---------------------------------------------------------------------------
// The external capture itself
// ---------------------------------------------------------------------------

// prepareExternalCaptureDirs clears and creates the capture and staging
// directories in the transient workload's scratch volume.
func (iops *InfrahubOps) prepareExternalCaptureDirs(capture *externalCapture) error {
	command := []string{"sh", "-c", fmt.Sprintf("rm -rf %s %s && mkdir -p %s %s",
		externalNeo4jCaptureDir, externalNeo4jStagingDir, externalNeo4jCaptureDir, externalNeo4jStagingDir)}

	if _, err := iops.execBoundedAgainst(
		externalDBControlBound(iops.config), "preparing the capture directory",
		transientWorkloadTarget(serviceNeo4j), serviceNeo4j, command, capture.execOptions(nil),
	); err != nil {
		return fmt.Errorf("failed to prepare the external Neo4j capture directory for %s: %w", capture.Endpoint.endpointTarget(), err)
	}

	return nil
}

// cleanupExternalCaptureDirs removes what the capture staged. It is best-effort:
// the workload is about to be given back and its scratch volume goes with it, so
// a failure here costs nothing (FR-011, FR-023).
func (iops *InfrahubOps) cleanupExternalCaptureDirs(capture *externalCapture) {
	iops.removeTransientScratch(serviceNeo4j, "the capture directory", capture.execOptions(nil), externalNeo4jCaptureDir, externalNeo4jStagingDir)
}

// externalCaptureProducedArtifact asks the capture directory whether the
// artifact is there, which is the observation that separates a partial capture
// from a failed one.
//
// A listing that cannot be read reports "no artifact", which is the safe
// direction: it makes the run fail as an incomplete capture rather than record a
// completeness it did not observe.
func (iops *InfrahubOps) externalCaptureProducedArtifact(capture *externalCapture) bool {
	command := []string{"sh", "-c", "ls -1 " + externalNeo4jCaptureDir + " 2>/dev/null"}

	output, err := iops.execBoundedAgainst(
		externalDBControlBound(iops.config), "listing the capture directory",
		transientWorkloadTarget(serviceNeo4j), serviceNeo4j, command, capture.execOptions(nil),
	)
	if err != nil {
		logrus.Warnf("Could not list the external Neo4j capture directory, so the capture is treated as having produced nothing: %v", err)

		return false
	}

	return neo4jArtifactProduced(capture.Endpoint.Database, output)
}

// runExternalNeo4jCapture runs the online backup against the resolved members
// and fails the run for anything short of a complete capture (FR-012).
//
// It is bounded by the operator's --external-db-timeout rather than run through
// the unbounded Exec (FR-025). The internal path is deliberately left unbounded:
// it is the path every existing deployment takes, and a bound that has never
// been there is a behaviour change to a working flow (FR-015).
func (iops *InfrahubOps) runExternalNeo4jCapture(capture *externalCapture, backupMetadata string, uncompressed bool) error {
	command := neo4jCaptureCommand(externalNeo4jCaptureRequest(capture, backupMetadata, uncompressed))

	output, runErr := iops.execBoundedAgainst(
		capture.Bound, "the online backup", capture.Endpoint.endpointTarget(), serviceNeo4j, command, capture.execOptions(nil),
	)

	verdict := classifyNeo4jCapture(runErr, output, iops.externalCaptureProducedArtifact(capture), capture.captureTargets())
	if verdict.Complete {
		return nil
	}

	// Recorded before the error is returned, so that FR-012's other half — no
	// artefact survives this run — is decided from the verdict itself rather
	// than from an error value a caller between here and the archive could
	// replace. See InfrahubOps.discardIncompleteCapture.
	iops.recordIncompleteCapture(serviceNeo4j, verdict.Detail)

	if runErr != nil {
		return fmt.Errorf("the Neo4j capture from %s is incomplete and will not be kept: %s: %w", capture.Endpoint.endpointTarget(), verdict.Detail, runErr)
	}

	return fmt.Errorf("the Neo4j capture from %s is incomplete and will not be kept: %s", capture.Endpoint.endpointTarget(), verdict.Detail)
}

// reportExternalCapture announces the capture about to run: the members it will
// try, in order, and the role each was observed in at this moment (FR-005).
//
// It states no member as the one that will serve the capture, because --from
// tries its list in order and reports no such thing. What the operator gets
// instead is enough to see that a follower could serve it, which is the fact
// that bears on the recovery point.
func (capture *externalCapture) reportExternalCapture(action string) {
	targets := capture.captureTargets()

	logrus.Infof("%s the external Neo4j database (Enterprise Edition online backup, endpoints tried in order: %s; roles observed now: %s; the member that serves the capture is not reported by the backup command)",
		action, renderHostPorts(targets), describeCaptureRoles(targets, capture.observedRoles()))
}

// backupNeo4jEnterpriseExternal is the directory-copy capture from a database
// outside the deployment. It is the internal path's shape with the container
// replaced by the transient workload, which is why the destination and the
// artifact layout are unchanged.
func (iops *InfrahubOps) backupNeo4jEnterpriseExternal(backupDir, backupMetadata string) error {
	capture, err := iops.openExternalCapture(serviceNeo4j)
	if err != nil {
		return err
	}
	defer capture.Release()

	capture.reportExternalCapture("Backing up")

	if err := iops.prepareExternalCaptureDirs(capture); err != nil {
		return err
	}
	defer iops.cleanupExternalCaptureDirs(capture)

	if err := iops.runExternalNeo4jCapture(capture, backupMetadata, false); err != nil {
		return err
	}

	if err := iops.copyFromTransientWorkload(capture.Pod, externalNeo4jCaptureDir, filepath.Join(backupDir, "database")); err != nil {
		return fmt.Errorf("failed to copy the external database backup: %w", err)
	}

	logrus.Info("Neo4j backup completed")

	return nil
}

// streamNeo4jEnterpriseExternal is the streaming capture from a database outside
// the deployment.
//
// The tar is taken with the same -C and the same member name the internal path
// uses, so the archive is byte-compatible with an internal one and restores by
// the same code (FR-015). The transient workload is given back when the stream
// is closed, and on every failure path before that (FR-011).
func (iops *InfrahubOps) streamNeo4jEnterpriseExternal(backupMetadata string) (io.ReadCloser, error) {
	capture, err := iops.openExternalCapture(serviceNeo4j)
	if err != nil {
		return nil, err
	}

	capture.reportExternalCapture("Streaming")

	if err := iops.prepareExternalCaptureDirs(capture); err != nil {
		capture.Release()

		return nil, err
	}

	// Once the directories exist, giving the workload back means clearing them
	// first, on every path out of here — including the stream's own close.
	teardown := func() {
		iops.cleanupExternalCaptureDirs(capture)
		capture.Release()
	}

	if err := iops.runExternalNeo4jCapture(capture, backupMetadata, true); err != nil {
		teardown()

		return nil, err
	}

	stdout, wait, err := iops.ExecStreamPipe(serviceNeo4j,
		[]string{"tar", "cf", "-", "-C", externalNeo4jCaptureParent, externalNeo4jCaptureName}, capture.execOptions(nil))
	if err != nil {
		teardown()

		return nil, fmt.Errorf("failed to start the external neo4j enterprise stream: %w", err)
	}

	return &execReadCloser{reader: stdout, wait: wait, idleTimeout: defaultStreamIdleTimeout, cleanup: teardown}, nil
}
