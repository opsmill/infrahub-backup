package app

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// CreateBackup creates a full backup of the Infrahub deployment
func (iops *InfrahubOps) CreateBackup(force bool, neo4jMetadata string, excludeTaskManager bool) (retErr error) {
	if err := iops.checkPrerequisites(); err != nil {
		return err
	}

	if err := iops.DetectEnvironment(); err != nil {
		return err
	}

	// Detect Neo4j edition
	editionInfo := iops.detectNeo4jEditionInfo("backup")
	if editionInfo.IsCommunity {
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
	if editionInfo.IsCommunity {
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

	logrus.WithFields(logrus.Fields{
		"filename":    backupFilename,
		"backup_dir":  iops.config.BackupDir,
		"neo4j_edition": editionInfo.Edition,
	}).Info("Creating backup")

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
	metadata := iops.createBackupMetadata(backupID, !excludeTaskManager, version, editionInfo.Edition)

	// Backup databases
	if err := iops.backupDatabase(backupDir, neo4jMetadata, editionInfo.Edition); err != nil {
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
	checksums, err := calculateBackupChecksums(backupDir, excludeTaskManager)
	if err != nil {
		return err
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

	// Log backup creation with structured fields
	fields := logrus.Fields{
		"path":     backupPath,
		"filename": backupFilename,
	}
	if stat, err := os.Stat(backupPath); err == nil {
		fields["size_bytes"] = stat.Size()
		fields["size_human"] = formatBytes(stat.Size())
	}
	logrus.WithFields(fields).Info("Backup created successfully")

	return retErr
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

	logrus.WithFields(logrus.Fields{
		"backup_file": backupFile,
		"work_dir":    workDir,
	}).Info("Starting backup restore")

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

	// Log backup metadata with structured fields
	logrus.WithFields(logrus.Fields{
		"backup_id":        metadata.BackupID,
		"created_at":       metadata.CreatedAt,
		"tool_version":     metadata.ToolVersion,
		"infrahub_version": metadata.InfrahubVersion,
		"neo4j_edition":    metadata.Neo4jEdition,
		"components":       metadata.Components,
	}).Info("Backup metadata loaded")

	// Detect Neo4j edition for restore
	detectedEdition, detectionErr := iops.detectNeo4jEdition()
	editionInfo := NewNeo4jEditionInfo(detectedEdition, detectionErr)

	neo4jEdition, err := editionInfo.ResolveRestoreEdition(metadata.Neo4jEdition)
	if err != nil {
		return err
	}
	editionInfo.LogDetection("restore")

	// Determine task manager database availability
	taskManagerIncluded := slices.Contains(metadata.Components, "task-manager-db")
	if !taskManagerIncluded {
		if _, ok := metadata.Checksums["prefect.dump"]; ok {
			taskManagerIncluded = true
		}
	}

	// Validate checksums for all backup files
	if err := validateBackupChecksums(workDir, &metadata, excludeTaskManager); err != nil {
		return err
	}

	// Determine if we should restore task manager database
	shouldRestoreTaskManager := taskManagerIncluded && !excludeTaskManager
	prefectPath := filepath.Join(workDir, "backup", prefectDumpFilename)
	prefectExists := fileExists(prefectPath)
	validatePrefect := shouldRestoreTaskManager && prefectExists

	// Validate task manager restore requirements
	if taskManagerIncluded && !prefectExists && !excludeTaskManager {
		return fmt.Errorf("backup metadata includes task manager database but %s is missing", prefectDumpFilename)
	}

	// Log task manager restore status
	if taskManagerIncluded && excludeTaskManager {
		logrus.Info("Skipping task manager database restore as requested")
	} else if !taskManagerIncluded {
		logrus.Info("Backup does not include task manager database; skipping restore")
	} else if prefectExists {
		logrus.Info("Task manager database dump detected; will restore")
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
