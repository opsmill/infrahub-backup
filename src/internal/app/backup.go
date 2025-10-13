package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

const metadataVersion = 2025092500

// BackupMetadata represents the backup metadata structure
type BackupMetadata struct {
	MetadataVersion int               `json:"metadata_version"`
	BackupID        string            `json:"backup_id"`
	CreatedAt       string            `json:"created_at"`
	ToolVersion     string            `json:"tool_version"`
	InfrahubVersion string            `json:"infrahub_version"`
	Components      []string          `json:"components"`
	Checksums       map[string]string `json:"checksums,omitempty"`
}

// CreateBackup creates a full backup of the Infrahub deployment
func (iops *InfrahubOps) CreateBackup(force bool, neo4jMetadata string) error {
	if err := iops.checkPrerequisites(); err != nil {
		return err
	}

	if err := iops.DetectEnvironment(); err != nil {
		return err
	}

	// Check for running tasks unless --force is set
	if !force {
		logrus.Info("Checking for running tasks before backup...")
		scriptBytes, err := readEmbeddedScript("get_running_tasks.py")
		if err != nil {
			return fmt.Errorf("could not retrieve get_running_tasks.py: %w", err)
		}
		scriptContent := string(scriptBytes)
		for {
			var output string
			output, err = iops.executeScript("task-worker", scriptContent, "/tmp/get_running_tasks.py", "python", "-u", "/tmp/get_running_tasks.py")
			if err != nil {
				return fmt.Errorf("failed to check running tasks: %w", err)
			}
			var tasks []tasksOutput
			if err = json.Unmarshal([]byte(output), &tasks); err != nil {
				return fmt.Errorf("could not parse json: %w\n%v", err, output)
			}
			if len(tasks) == 0 {
				logrus.Info("No running tasks detected. Proceeding with backup.")
				break
			} else {
				logrus.Warnf("There are running %v tasks: %v", len(tasks), tasks)
				logrus.Warnf("Waiting for them to complete... (use --force to override)")
				time.Sleep(5 * time.Second)
			}
		}
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
	metadata := iops.createBackupMetadata(backupID)

	// Backup databases
	if err := iops.backupDatabase(backupDir, neo4jMetadata); err != nil {
		return err
	}

	if err := iops.backupTaskManagerDB(backupDir); err != nil {
		return err
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
	if sum, err := calculateSHA256(prefectPath); err == nil {
		checksums["prefect.dump"] = sum
	}

	metadata.Checksums = checksums

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

	return nil
}

// RestoreBackup restores an Infrahub deployment from a backup archive
func (iops *InfrahubOps) RestoreBackup(backupFile string) error {
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

	// Validate checksum for Prefect DB dump
	prefectPath := filepath.Join(workDir, "backup", "prefect.dump")
	if expectedSum, ok := metadata.Checksums["prefect.dump"]; ok {
		if _, err := os.Stat(prefectPath); err != nil {
			return fmt.Errorf("missing backup file: prefect.dump")
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
	if err := iops.stopAppContainers(); err != nil {
		return err
	}

	// Restore PostgreSQL
	if err := iops.restorePostgreSQL(workDir); err != nil {
		return err
	}

	// Restart dependencies
	if err := iops.restartDependencies(); err != nil {
		return err
	}

	// Restore Neo4j
	if err := iops.restoreNeo4j(workDir); err != nil {
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

func (iops *InfrahubOps) createBackupMetadata(backupID string) *BackupMetadata {
	return &BackupMetadata{
		MetadataVersion: metadataVersion,
		BackupID:        backupID,
		CreatedAt:       time.Now().UTC().Format(time.RFC3339),
		ToolVersion:     BuildRevision(),
		InfrahubVersion: iops.getInfrahubVersion(),
		Components:      []string{"database", "task-manager-db", "artifacts"},
	}
}

func (iops *InfrahubOps) stopAppContainers() error {
	logrus.Info("Stopping Infrahub application services...")

	services := []string{
		"infrahub-server", "task-worker", "task-manager",
		"task-manager-background-svc", "cache", "message-queue",
	}

	for _, service := range services {
		running, err := iops.IsServiceRunning(service)
		if err != nil {
			logrus.Debugf("Could not determine status of %s: %v", service, err)
			continue
		}

		if running {
			logrus.Infof("Stopping %s...", service)
			if err := iops.StopServices(service); err != nil {
				return fmt.Errorf("failed to stop %s: %w", service, err)
			}
		}
	}

	logrus.Info("Application services stopped")
	return nil
}

func (iops *InfrahubOps) backupDatabase(backupDir string, backupMetadata string) error {
	logrus.Info("Backing up Neo4j database...")

	// Create backup directory in container
	if _, err := iops.Exec("database", []string{"mkdir", "-p", "/tmp/infrahubops"}, nil); err != nil {
		return fmt.Errorf("failed to create backup directory: %w", err)
	}
	defer func() {
		if _, err := iops.Exec("database", []string{"rm", "-rf", "/tmp/infrahubops"}, nil); err != nil {
			logrus.Warnf("Failed to remove temporary Neo4j backup directory: %v", err)
		}
	}()

	// Create backup using neo4j-admin
	if output, err := iops.Exec(
		"database",
		[]string{"neo4j-admin", "database", "backup", "--include-metadata=" + backupMetadata, "--to-path=/tmp/infrahubops", iops.config.Neo4jDatabase},
		nil,
	); err != nil {
		return fmt.Errorf("failed to backup neo4j: %w\nOutput: %v", err, output)
	}

	// Copy backup
	if err := iops.CopyFrom("database", "/tmp/infrahubops", filepath.Join(backupDir, "database")); err != nil {
		return fmt.Errorf("failed to copy database backup: %w", err)
	}

	logrus.Info("Neo4j backup completed")
	return nil
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

func (iops *InfrahubOps) restoreNeo4j(workDir string) error {
	logrus.Info("Restoring Neo4j database...")

	// Copy backup to container
	backupPath := filepath.Join(workDir, "backup", "database")
	if err := iops.CopyTo("database", backupPath, "/tmp/infrahubops"); err != nil {
		return fmt.Errorf("failed to copy backup to container: %w", err)
	}
	defer func() {
		if _, err := iops.Exec("database", []string{"rm", "-rf", "/tmp/infrahubops"}, nil); err != nil {
			logrus.Warnf("Failed to cleanup temporary Neo4j backup data: %v", err)
		}
	}()

	// Change ownership
	if _, err := iops.Exec("database", []string{"chown", "-R", "neo4j:neo4j", "/tmp/infrahubops"}, nil); err != nil {
		return fmt.Errorf("failed to change backup ownership: %w", err)
	}

	// Stop neo4j database
	if _, err := iops.Exec(
		"database",
		[]string{"cypher-shell", "-u", iops.config.Neo4jUsername, "-p" + iops.config.Neo4jPassword, "-d", "system", "stop database " + iops.config.Neo4jDatabase},
		nil,
	); err != nil {
		return fmt.Errorf("failed to stop neo4j database: %w", err)
	}

	// Restore database
	opts := &ExecOptions{User: "neo4j"}
	if output, err := iops.Exec(
		"database",
		[]string{"neo4j-admin", "database", "restore", "--overwrite-destination=true", "--from-path=/tmp/infrahubops", iops.config.Neo4jDatabase},
		opts,
	); err != nil {
		return fmt.Errorf("failed to restore neo4j: %w\nOutput: %v", err, output)
	}

	// Restore metadata
	if output, err := iops.Exec(
		"database",
		[]string{"sh", "-c", "cat /data/scripts/neo4j/restore_metadata.cypher | cypher-shell -u " + iops.config.Neo4jUsername + " -p" + iops.config.Neo4jPassword + " -d system --param \"database => '" + iops.config.Neo4jDatabase + "'\""},
		opts,
	); err != nil {
		return fmt.Errorf("failed to restore neo4j metadata: %w\nOutput: %v", err, output)
	}

	// Start neo4j database
	if _, err := iops.Exec(
		"database",
		[]string{"cypher-shell", "-u", iops.config.Neo4jUsername, "-p" + iops.config.Neo4jPassword, "-d", "system", "start database " + iops.config.Neo4jDatabase},
		nil,
	); err != nil {
		return fmt.Errorf("failed to start neo4j database: %w", err)
	}

	return nil
}
