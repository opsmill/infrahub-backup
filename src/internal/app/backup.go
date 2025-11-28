package app

import (
	"encoding/json"
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

const metadataVersion = 2025111200

const (
	neo4jEditionEnterprise = "enterprise"
	neo4jEditionCommunity  = "community"

	neo4jPIDFile              = "/var/lib/neo4j/run/neo4j.pid"
	neo4jRemoteWorkDir        = "/tmp/infrahubops"
	neo4jRemoteWatchdogBinary = neo4jRemoteWorkDir + "/neo4j_watchdog"
	neo4jRemoteWatchdogReady  = neo4jRemoteWorkDir + "/neo4j_watchdog.ready"
	neo4jRemoteWatchdogLog    = neo4jRemoteWorkDir + "/neo4j_watchdog.log"
)

func (iops *InfrahubOps) detectNeo4jEdition() (string, error) {
	output, err := iops.Exec("database", []string{
		"cypher-shell",
		"-u", iops.config.Neo4jUsername,
		"-p" + iops.config.Neo4jPassword,
		"-d", "system",
		"--format", "plain",
		"CALL dbms.components() YIELD edition",
	}, nil)
	if err != nil {
		return "", fmt.Errorf("failed to query neo4j edition: %w", err)
	}

	edition := extractNeo4jEdition(output)
	if edition == "" {
		return "", fmt.Errorf("unable to parse neo4j edition from output: %s", strings.TrimSpace(output))
	}

	return edition, nil
}

func extractNeo4jEdition(output string) string {
	lines := strings.Split(output, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(strings.Trim(lines[i], "\""))
		if trimmed != "" {
			return strings.ToLower(trimmed)
		}
	}
	return ""
}

// BackupMetadata represents the backup metadata structure
type BackupMetadata struct {
	MetadataVersion int               `json:"metadata_version"`
	BackupID        string            `json:"backup_id"`
	CreatedAt       string            `json:"created_at"`
	ToolVersion     string            `json:"tool_version"`
	InfrahubVersion string            `json:"infrahub_version"`
	Components      []string          `json:"components"`
	Checksums       map[string]string `json:"checksums,omitempty"`
	Neo4jEdition    string            `json:"neo4j_edition,omitempty"`
}

// CreateBackup creates a full backup of the Infrahub deployment
func (iops *InfrahubOps) CreateBackup(force bool, neo4jMetadata string, excludeTaskManager bool) (retErr error) {
	if err := iops.checkPrerequisites(); err != nil {
		return err
	}

	if err := iops.DetectEnvironment(); err != nil {
		return err
	}

	edition, editionErr := iops.detectNeo4jEdition()
	if editionErr != nil {
		logrus.Warnf("Could not determine Neo4j edition: %v", editionErr)
	} else {
		logrus.Infof("Detected Neo4j %s edition", edition)
	}

	isCommunityEdition := strings.EqualFold(edition, neo4jEditionCommunity)
	if isCommunityEdition {
		logrus.Warn("Neo4j Community Edition detected; Infrahub services will be stopped and restarted before the backup begins.")
		logrus.Warn("Waiting 10 seconds to allow the user to abort... CTRL+C to cancel.")
		time.Sleep(10 * time.Second)
	}

	version := iops.getInfrahubVersion()

	// Check for running tasks unless --force is set
	if !force {
		logrus.Info("Checking for running tasks before backup...")
		if err := iops.waitForRunningTasks(); err != nil {
			return err
		}
	}

	var servicesToRestart []string
	if isCommunityEdition {
		stoppedServices, stopErr := iops.stopAppContainers()
		if stopErr != nil {
			if len(stoppedServices) > 0 {
				if startErr := iops.startAppContainers(stoppedServices); startErr != nil {
					logrus.Warnf("Failed to restart services after stop error: %v", startErr)
				}
			}
			return fmt.Errorf("failed to stop services for Neo4j Community backup: %w", stopErr)
		}
		servicesToRestart = append([]string(nil), stoppedServices...)
		defer func() {
			if len(servicesToRestart) == 0 {
				return
			}
			if startErr := iops.startAppContainers(servicesToRestart); startErr != nil {
				logrus.Errorf("Failed to restart services after backup: %v", startErr)
				if retErr == nil {
					retErr = fmt.Errorf("failed to restart services after backup: %w", startErr)
				}
			}
		}()
	}

	backupFilename := iops.generateBackupFilename()
	backupPath := filepath.Join(iops.config.BackupDir, backupFilename)
	workDir, err := os.MkdirTemp("", "infrahub_backup_*")
	if err != nil {
		return fmt.Errorf("failed to create temp directory: %w", err)
	}
	defer os.RemoveAll(workDir)

	logrus.Infof("Creating backup: %s", backupFilename)

	// Create backup directory structure
	backupDir := filepath.Join(workDir, "backup")
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		return fmt.Errorf("failed to create backup directory: %w", err)
	}

	if err := os.MkdirAll(iops.config.BackupDir, 0755); err != nil {
		return fmt.Errorf("failed to create backup parent directory: %w", err)
	}

	// Create metadata
	backupID := strings.TrimSuffix(backupFilename, ".tar.gz")
	metadata := iops.createBackupMetadata(backupID, !excludeTaskManager, version, edition)

	// Backup databases
	if err := iops.backupDatabase(backupDir, neo4jMetadata, edition); err != nil {
		return err
	}

	if !excludeTaskManager {
		if err := iops.backupTaskManagerDB(backupDir); err != nil {
			return err
		}
	} else {
		logrus.Info("Skipping task manager database backup as requested")
	}

	// Calculate checksums for backup files
	checksums := make(map[string]string)
	neo4jDir := filepath.Join(backupDir, "database")
	prefectPath := filepath.Join(backupDir, "prefect.dump")

	// Calculate checksum for each file in Neo4j backup directory
	err = filepath.Walk(neo4jDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			rel, _ := filepath.Rel(backupDir, path)
			if sum, err := calculateSHA256(path); err == nil {
				checksums[rel] = sum
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to calculate Neo4j backup checksums: %w", err)
	}

	// Calculate checksum for Prefect DB dump
	if !excludeTaskManager {
		if _, err := os.Stat(prefectPath); err == nil {
			if sum, err := calculateSHA256(prefectPath); err == nil {
				checksums["prefect.dump"] = sum
			} else {
				return fmt.Errorf("failed to calculate Prefect DB checksum: %w", err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("could not access Prefect DB dump: %w", err)
		}
	}

	if len(checksums) > 0 {
		metadata.Checksums = checksums
	}

	metadataBytes, err := json.MarshalIndent(metadata, "", "    ")
	if err != nil {
		return fmt.Errorf("failed to marshal metadata: %w", err)
	}

	if err := os.WriteFile(filepath.Join(backupDir, "backup_information.json"), metadataBytes, 0644); err != nil {
		return fmt.Errorf("failed to write metadata: %w", err)
	}

	// TODO: Backup artifact store
	logrus.Info("Artifact store backup will be added in future versions")

	// Create tarball
	logrus.Info("Creating backup archive...")
	if err := createTarball(backupPath, workDir, "backup/"); err != nil {
		return fmt.Errorf("failed to create archive: %w", err)
	}

	logrus.Infof("Backup created: %s", backupPath)

	// Show backup size
	if stat, err := os.Stat(backupPath); err == nil {
		logrus.Infof("Backup size: %s", formatBytes(stat.Size()))
	}

	return retErr
}

func (iops *InfrahubOps) waitForRunningTasks() error {
	useInfrahubctl := true
	var scriptContent string

	loadScriptContent := func() error {
		if scriptContent != "" {
			return nil
		}
		scriptBytes, err := readEmbeddedScript("get_running_tasks.py")
		if err != nil {
			return fmt.Errorf("could not retrieve get_running_tasks.py: %w", err)
		}
		scriptContent = string(scriptBytes)
		return nil
	}

	isCommandNotFound := func(err error, output string) bool {
		if err == nil {
			return false
		}
		errMsg := strings.ToLower(err.Error())
		if strings.Contains(errMsg, "no such command") {
			return true
		}
		outputLower := strings.ToLower(output)
		return strings.Contains(outputLower, "no such command")
	}

	for {
		var (
			output string
			err    error
		)

		if useInfrahubctl {
			output, err = iops.Exec("task-worker", []string{"infrahubctl", "task", "list", "--json", "--state", "running", "--state", "pending"}, nil)
			if err != nil {
				if isCommandNotFound(err, output) {
					logrus.Infof("infrahubctl task list command not available in task-worker, falling back to embedded script")
					useInfrahubctl = false
					if loadErr := loadScriptContent(); loadErr != nil {
						return loadErr
					}
					continue
				}
				return fmt.Errorf("failed to check running tasks: %w\n%s", err, output)
			}
		} else {
			if err := loadScriptContent(); err != nil {
				return err
			}
			output, err = iops.executeScript("task-worker", scriptContent, "/tmp/get_running_tasks.py", "python", "-u", "/tmp/get_running_tasks.py")
			if err != nil {
				return fmt.Errorf("failed to check running tasks: %w", err)
			}
		}

		output = strings.TrimSpace(output)
		var tasks []tasksOutput
		if output != "" {
			if err := json.Unmarshal([]byte(output), &tasks); err != nil {
				return fmt.Errorf("could not parse json: %w\n%v", err, output)
			}
		}
		if len(tasks) == 0 {
			logrus.Info("No running tasks detected. Proceeding with backup.")
			return nil
		}

		logrus.Warnf("There are running %v tasks: %v", len(tasks), tasks)
		logrus.Warnf("Waiting for them to complete... (use --force to override)")
		time.Sleep(5 * time.Second)
	}
}

// RestoreBackup restores an Infrahub deployment from a backup archive
func (iops *InfrahubOps) RestoreBackup(backupFile string, excludeTaskManager bool, restoreMigrateFormat bool) error {
	if _, err := os.Stat(backupFile); os.IsNotExist(err) {
		return fmt.Errorf("backup file not found: %s", backupFile)
	}

	if err := iops.checkPrerequisites(); err != nil {
		return err
	}

	if err := iops.DetectEnvironment(); err != nil {
		return err
	}

	workDir, err := os.MkdirTemp("", "infrahub_restore_*")
	if err != nil {
		return fmt.Errorf("failed to create temp directory: %w", err)
	}
	defer os.RemoveAll(workDir)

	logrus.Infof("Restoring from backup: %s", backupFile)

	// Extract backup
	logrus.Info("Extracting backup archive...")
	if err := extractTarball(backupFile, workDir); err != nil {
		return fmt.Errorf("failed to extract backup: %w", err)
	}

	// Validate backup
	metadataPath := filepath.Join(workDir, "backup", "backup_information.json")
	if _, err := os.Stat(metadataPath); os.IsNotExist(err) {
		return fmt.Errorf("invalid backup file: missing metadata")
	}

	// Read and parse backup info
	metadataBytes, err := os.ReadFile(metadataPath)
	if err != nil {
		return fmt.Errorf("failed to read metadata: %w", err)
	}
	var metadata BackupMetadata
	if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
		return fmt.Errorf("failed to parse metadata: %w", err)
	}

	logrus.Info("Backup metadata:")
	fmt.Println(string(metadataBytes))

	neo4jEdition := strings.ToLower(metadata.Neo4jEdition)
	if detectedEdition, err := iops.detectNeo4jEdition(); err != nil {
		logrus.Warnf("Could not detect Neo4j edition during restore; defaulting to community workflow: %v", err)
		neo4jEdition = neo4jEditionCommunity
	} else {
		if neo4jEdition == neo4jEditionCommunity && strings.ToLower(detectedEdition) == neo4jEditionEnterprise {
			// if the backup artifact is a community one, always use the community method to restore
			neo4jEdition = neo4jEditionCommunity
		} else if neo4jEdition == neo4jEditionEnterprise && strings.ToLower(detectedEdition) == neo4jEditionCommunity {
			return fmt.Errorf("cannot restore Enterprise backup on Community edition Neo4j")
		} else {
			neo4jEdition = strings.ToLower(detectedEdition)
		}
		logrus.Infof("Detected Neo4j %s edition for restore", neo4jEdition)
	}

	// Determine task manager database availability
	taskManagerIncluded := false
	for _, component := range metadata.Components {
		if component == "task-manager-db" {
			taskManagerIncluded = true
			break
		}
	}
	if !taskManagerIncluded {
		if _, ok := metadata.Checksums["prefect.dump"]; ok {
			taskManagerIncluded = true
		}
	}

	// Validate checksums for Neo4j backup files
	for relPath, expectedSum := range metadata.Checksums {
		if relPath == "prefect.dump" {
			continue
		}
		filePath := filepath.Join(workDir, "backup", relPath)
		if _, err := os.Stat(filePath); err != nil {
			return fmt.Errorf("missing backup file: %s", relPath)
		}
		sum, err := calculateSHA256(filePath)
		if err != nil {
			return fmt.Errorf("failed to calculate checksum for %s: %w", relPath, err)
		}
		if sum != expectedSum {
			return fmt.Errorf("checksum mismatch for %s: expected %s, got %s", relPath, expectedSum, sum)
		}
	}

	// Validate checksum for Prefect DB dump when applicable
	prefectPath := filepath.Join(workDir, "backup", "prefect.dump")
	prefectExists := false
	if _, err := os.Stat(prefectPath); err == nil {
		prefectExists = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("failed to access prefect.dump: %w", err)
	}

	shouldRestoreTaskManager := taskManagerIncluded && !excludeTaskManager
	validatePrefect := shouldRestoreTaskManager && prefectExists

	if taskManagerIncluded && !prefectExists && !excludeTaskManager {
		return fmt.Errorf("backup metadata includes task manager database but prefect.dump is missing")
	}

	if taskManagerIncluded && excludeTaskManager {
		logrus.Info("Skipping task manager database restore as requested")
	} else if !taskManagerIncluded {
		logrus.Info("Backup does not include task manager database; skipping restore")
	} else if prefectExists {
		logrus.Info("Task manager database dump detected; will restore")
	}

	if validatePrefect {
		expectedSum, ok := metadata.Checksums["prefect.dump"]
		if !ok {
			return fmt.Errorf("missing checksum for prefect.dump in metadata")
		}
		sum, err := calculateSHA256(prefectPath)
		if err != nil {
			return fmt.Errorf("failed to calculate checksum for prefect.dump: %w", err)
		}
		if sum != expectedSum {
			return fmt.Errorf("checksum mismatch for prefect.dump: expected %s, got %s", expectedSum, sum)
		}
	}

	// Wipe transient data
	iops.wipeTransientData()

	// Stop application containers
	if _, err := iops.stopAppContainers(); err != nil {
		return err
	}

	// Restore PostgreSQL when available
	if validatePrefect {
		if err := iops.restorePostgreSQL(workDir); err != nil {
			return err
		}
	} else {
		logrus.Info("Skipping task manager database restore step")
	}

	// Restart dependencies
	if err := iops.restartDependencies(); err != nil {
		return err
	}

	// Restore Neo4j
	if err := iops.restoreNeo4j(workDir, neo4jEdition, restoreMigrateFormat); err != nil {
		return err
	}

	// Restart all services
	logrus.Info("Restarting Infrahub services...")
	if err := iops.StartServices("infrahub-server", "task-worker"); err != nil {
		return fmt.Errorf("failed to restart infrahub services: %w", err)
	}

	logrus.Info("Restore completed successfully")
	logrus.Info("Infrahub should be available shortly")

	return nil
}

func (iops *InfrahubOps) generateBackupFilename() string {
	timestamp := time.Now().Format("20060102_150405")
	return fmt.Sprintf("infrahub_backup_%s.tar.gz", timestamp)
}

func (iops *InfrahubOps) createBackupMetadata(backupID string, includeTaskManager bool, infrahubVersion string, neo4jEdition string) *BackupMetadata {
	components := []string{"database"}
	if includeTaskManager {
		components = append(components, "task-manager-db")
	}

	return &BackupMetadata{
		MetadataVersion: metadataVersion,
		BackupID:        backupID,
		CreatedAt:       time.Now().UTC().Format(time.RFC3339),
		ToolVersion:     BuildRevision(),
		InfrahubVersion: infrahubVersion,
		Components:      components,
		Neo4jEdition:    strings.ToLower(neo4jEdition),
	}
}

func (iops *InfrahubOps) stopAppContainers() ([]string, error) {
	logrus.Info("Stopping Infrahub application services...")

	services := []string{
		"infrahub-server", "task-worker", "task-manager",
		"task-manager-background-svc", "cache", "message-queue",
	}

	stopped := []string{}

	for _, service := range services {
		running, err := iops.IsServiceRunning(service)
		if err != nil {
			logrus.Debugf("Could not determine status of %s: %v", service, err)
			continue
		}

		if running {
			logrus.Infof("Stopping %s...", service)
			if err := iops.StopServices(service); err != nil {
				return stopped, fmt.Errorf("failed to stop %s: %w", service, err)
			}
			stopped = append(stopped, service)
		}
	}

	if len(stopped) == 0 {
		logrus.Info("No application services were running")
	} else {
		logrus.Info("Application services stopped")
	}

	return stopped, nil
}

func (iops *InfrahubOps) startAppContainers(services []string) error {
	if len(services) == 0 {
		return nil
	}

	logrus.Info("Starting Infrahub application services...")

	preferredOrder := []string{
		"cache",
		"message-queue",
		"task-manager",
		"task-manager-background-svc",
		"infrahub-server",
		"task-worker",
	}

	serviceSet := make(map[string]struct{}, len(services))
	for _, svc := range services {
		serviceSet[svc] = struct{}{}
	}

	ordered := make([]string, 0, len(serviceSet))
	for _, svc := range preferredOrder {
		if _, ok := serviceSet[svc]; ok {
			ordered = append(ordered, svc)
			delete(serviceSet, svc)
		}
	}
	for svc := range serviceSet {
		ordered = append(ordered, svc)
	}

	for _, svc := range ordered {
		logrus.Infof("Starting %s...", svc)
		if err := iops.StartServices(svc); err != nil {
			return fmt.Errorf("failed to start %s: %w", svc, err)
		}
	}

	logrus.Info("Application services started")
	return nil
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
		[]string{"neo4j-admin", "database", "backup", "--include-metadata=" + backupMetadata, "--to-path=/tmp/infrahubops", iops.config.Neo4jDatabase},
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

func selectWatchdogBinary(arch string) ([]byte, error) {
	switch strings.ToLower(arch) {
	case "x86_64", "amd64":
		return neo4jWatchdogLinuxAMD64, nil
	case "aarch64", "arm64":
		return neo4jWatchdogLinuxARM64, nil
	default:
		return nil, fmt.Errorf("unsupported architecture for watchdog: %s", arch)
	}
}

func writeEmbeddedWatchdog(content []byte) (string, func(), error) {
	file, err := os.CreateTemp("", "neo4j_watchdog_*")
	if err != nil {
		return "", nil, fmt.Errorf("failed to create temp watchdog binary: %w", err)
	}

	if _, err := file.Write(content); err != nil {
		file.Close()
		os.Remove(file.Name())
		return "", nil, fmt.Errorf("failed to write watchdog binary: %w", err)
	}
	if err := file.Close(); err != nil {
		os.Remove(file.Name())
		return "", nil, fmt.Errorf("failed to close watchdog binary: %w", err)
	}

	if err := os.Chmod(file.Name(), 0755); err != nil {
		os.Remove(file.Name())
		return "", nil, fmt.Errorf("failed to set watchdog permissions: %w", err)
	}

	cleanup := func() {
		_ = os.Remove(file.Name())
	}
	return file.Name(), cleanup, nil
}

func (iops *InfrahubOps) waitForRemoteFile(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := iops.Exec("database", []string{"test", "-f", path}, nil); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for remote file %s", path)
}

func (iops *InfrahubOps) waitForProcessStopped(pid string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		stateCmd := fmt.Sprintf("sed -n 's/^State:\t//p' /proc/%s/status", pid)
		state, err := iops.Exec("database", []string{"sh", "-c", stateCmd}, nil)
		if err == nil {
			trimmed := strings.TrimSpace(state)
			if strings.HasPrefix(trimmed, "T") {
				return nil
			}
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(1 * time.Second)
	}
	return fmt.Errorf("timed out waiting for neo4j process %s to stop", pid)
}

func (iops *InfrahubOps) backupTaskManagerDB(backupDir string) error {
	logrus.Info("Backing up PostgreSQL database...")

	// Create dump
	opts := &ExecOptions{Env: map[string]string{
		"PGPASSWORD": iops.config.PostgresPassword,
	}}
	if output, err := iops.Exec(
		"task-manager-db",
		[]string{"pg_dump", "-Fc", "-U", iops.config.PostgresUsername, "-d", iops.config.PostgresDatabase, "-f", "/tmp/infrahubops_prefect.dump"},
		opts,
	); err != nil {
		return fmt.Errorf("failed to create postgresql dump: %w\nOutput: %v", err, output)
	}
	defer func() {
		if _, err := iops.Exec("task-manager-db", []string{"rm", "/tmp/infrahubops_prefect.dump"}, nil); err != nil {
			logrus.Warnf("Failed to remove temporary postgres dump: %v", err)
		}
	}()

	// Copy dump
	if err := iops.CopyFrom("task-manager-db", "/tmp/infrahubops_prefect.dump", filepath.Join(backupDir, "prefect.dump")); err != nil {
		return fmt.Errorf("failed to copy postgresql dump: %w", err)
	}

	logrus.Info("PostgreSQL backup completed")
	return nil
}

type tasksOutput struct {
	Id   string `json:"id"`
	Name string `json:"title"`
}

func (iops *InfrahubOps) wipeTransientData() error {
	logrus.Info("Wiping cache and message queue data...")

	if _, err := iops.Exec("message-queue", []string{"find", "/var/lib/rabbitmq", "-mindepth", "1", "-delete"}, nil); err != nil {
		logrus.Warnf("Failed to wipe message queue data: %v", err)
	}
	if _, err := iops.Exec("cache", []string{"find", "/data", "-mindepth", "1", "-delete"}, nil); err != nil {
		logrus.Warnf("Failed to wipe cache data: %v", err)
	}
	logrus.Info("Transient data wiped")
	return nil
}

func (iops *InfrahubOps) restorePostgreSQL(workDir string) error {
	logrus.Info("Restoring PostgreSQL database...")

	// Start task-manager-db
	if err := iops.StartServices("task-manager-db"); err != nil {
		return fmt.Errorf("failed to start task-manager-db: %w", err)
	}

	// Copy dump to container
	dumpPath := filepath.Join(workDir, "backup", "prefect.dump")
	if err := iops.CopyTo("task-manager-db", dumpPath, "/tmp/infrahubops_prefect.dump"); err != nil {
		return fmt.Errorf("failed to copy dump to container: %w", err)
	}
	defer func() {
		if _, err := iops.Exec("task-manager-db", []string{"rm", "/tmp/infrahubops_prefect.dump"}, nil); err != nil {
			logrus.Warnf("Failed to remove temporary postgres dump: %v", err)
		}
	}()

	// Restore database
	opts := &ExecOptions{Env: map[string]string{
		"PGPASSWORD": iops.config.PostgresPassword,
	}}
	if output, err := iops.Exec(
		"task-manager-db",
		// "-x", "--no-owner" for role does not exist
		[]string{"pg_restore", "-d", "postgres", "-U", iops.config.PostgresUsername, "--clean", "--create", "/tmp/infrahubops_prefect.dump"},
		opts,
	); err != nil {
		return fmt.Errorf("failed to restore postgresql: %w\nOutput: %v", err, output)
	}

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

	opts := &ExecOptions{User: "neo4j"}

	if _, err := iops.Exec(
		"database",
		[]string{"cypher-shell", "-u", iops.config.Neo4jUsername, "-p" + iops.config.Neo4jPassword, "-d", "system", "stop database " + iops.config.Neo4jDatabase},
		nil,
	); err != nil {
		return fmt.Errorf("failed to stop neo4j database: %w", err)
	}

	if output, err := iops.Exec(
		"database",
		[]string{"neo4j-admin", "database", "restore", "--overwrite-destination=true", "--from-path=/tmp/infrahubops", iops.config.Neo4jDatabase},
		opts,
	); err != nil {
		return fmt.Errorf("failed to restore neo4j: %w\nOutput: %v", err, output)
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

// CreateBackupFromFiles creates a backup archive from local Neo4j backup files and PostgreSQL dump.
// This is useful when you already have database dumps on the local filesystem and want to
// create a compatible backup archive without connecting to a running Infrahub instance.
func (iops *InfrahubOps) CreateBackupFromFiles(neo4jPath string, postgresPath string, neo4jEdition string, infrahubVersion string) error {
	// Validate input paths
	if neo4jPath == "" {
		return fmt.Errorf("neo4j backup path is required")
	}

	neo4jInfo, err := os.Stat(neo4jPath)
	if err != nil {
		return fmt.Errorf("neo4j backup path not accessible: %w", err)
	}

	var postgresIncluded bool
	if postgresPath != "" {
		if _, err := os.Stat(postgresPath); err != nil {
			return fmt.Errorf("postgres dump file not accessible: %w", err)
		}
		postgresIncluded = true
	}

	// Create work directory
	workDir, err := os.MkdirTemp("", "infrahub_backup_*")
	if err != nil {
		return fmt.Errorf("failed to create temp directory: %w", err)
	}
	defer os.RemoveAll(workDir)

	// Create backup directory structure
	backupDir := filepath.Join(workDir, "backup")
	databaseDir := filepath.Join(backupDir, "database")
	if err := os.MkdirAll(databaseDir, 0755); err != nil {
		return fmt.Errorf("failed to create backup directory: %w", err)
	}

	// Ensure output directory exists
	if err := os.MkdirAll(iops.config.BackupDir, 0755); err != nil {
		return fmt.Errorf("failed to create backup parent directory: %w", err)
	}

	logrus.Info("Copying Neo4j backup files...")

	// Copy Neo4j backup files
	if neo4jInfo.IsDir() {
		// Copy directory contents
		if err := copyDir(neo4jPath, databaseDir); err != nil {
			return fmt.Errorf("failed to copy neo4j backup directory: %w", err)
		}
	} else {
		// Copy single file (e.g., .dump file for community edition)
		destPath := filepath.Join(databaseDir, filepath.Base(neo4jPath))
		if err := copyFile(neo4jPath, destPath); err != nil {
			return fmt.Errorf("failed to copy neo4j backup file: %w", err)
		}
	}

	// Copy PostgreSQL dump if provided
	if postgresIncluded {
		logrus.Info("Copying PostgreSQL dump file...")
		destPath := filepath.Join(backupDir, "prefect.dump")
		if err := copyFile(postgresPath, destPath); err != nil {
			return fmt.Errorf("failed to copy postgres dump: %w", err)
		}
	}

	// Calculate checksums
	checksums := make(map[string]string)

	err = filepath.Walk(databaseDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			rel, _ := filepath.Rel(backupDir, path)
			if sum, err := calculateSHA256(path); err == nil {
				checksums[rel] = sum
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to calculate Neo4j backup checksums: %w", err)
	}

	if postgresIncluded {
		prefectPath := filepath.Join(backupDir, "prefect.dump")
		if sum, err := calculateSHA256(prefectPath); err == nil {
			checksums["prefect.dump"] = sum
		} else {
			return fmt.Errorf("failed to calculate Prefect DB checksum: %w", err)
		}
	}

	// Generate backup filename and ID
	backupFilename := iops.generateBackupFilename()
	backupPath := filepath.Join(iops.config.BackupDir, backupFilename)
	backupID := strings.TrimSuffix(backupFilename, ".tar.gz")

	// Normalize neo4j edition
	edition := strings.ToLower(neo4jEdition)
	if edition == "" {
		// Try to detect from file structure
		// If it's a .dump file, likely community edition
		if !neo4jInfo.IsDir() && strings.HasSuffix(neo4jPath, ".dump") {
			edition = neo4jEditionCommunity
		} else {
			edition = neo4jEditionEnterprise
		}
		logrus.Infof("Auto-detected Neo4j edition: %s", edition)
	}

	// Create metadata
	metadata := iops.createBackupMetadata(backupID, postgresIncluded, infrahubVersion, edition)
	metadata.Checksums = checksums

	metadataBytes, err := json.MarshalIndent(metadata, "", "    ")
	if err != nil {
		return fmt.Errorf("failed to marshal metadata: %w", err)
	}

	if err := os.WriteFile(filepath.Join(backupDir, "backup_information.json"), metadataBytes, 0644); err != nil {
		return fmt.Errorf("failed to write metadata: %w", err)
	}

	// Create tarball
	logrus.Info("Creating backup archive...")
	if err := createTarball(backupPath, workDir, "backup/"); err != nil {
		return fmt.Errorf("failed to create archive: %w", err)
	}

	logrus.Infof("Backup created: %s", backupPath)

	// Show backup size
	if stat, err := os.Stat(backupPath); err == nil {
		logrus.Infof("Backup size: %s", formatBytes(stat.Size()))
	}

	return nil
}

// copyFile copies a single file from src to dst
func copyFile(src, dst string) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	srcInfo, err := srcFile.Stat()
	if err != nil {
		return err
	}

	dstFile, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, srcInfo.Mode())
	if err != nil {
		return err
	}
	defer dstFile.Close()

	_, err = io.Copy(dstFile, srcFile)
	return err
}

// copyDir recursively copies a directory from src to dst
func copyDir(src, dst string) error {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(dst, srcInfo.Mode()); err != nil {
		return err
	}

	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())

		if entry.IsDir() {
			if err := copyDir(srcPath, dstPath); err != nil {
				return err
			}
		} else {
			if err := copyFile(srcPath, dstPath); err != nil {
				return err
			}
		}
	}

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

	opts := &ExecOptions{User: "neo4j"}
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
