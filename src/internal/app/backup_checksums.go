package app

import (
	"fmt"
	"os"
	"path/filepath"
)

const (
	backupMetadataFilename = "backup_information.json"
	prefectDumpFilename    = "prefect.dump"
	neo4jBackupDirName     = "database"
)

// calculateBackupChecksums calculates SHA256 checksums for all backup files
func calculateBackupChecksums(backupDir string, excludeTaskManager bool) (map[string]string, error) {
	checksums := make(map[string]string)

	// Calculate checksums for Neo4j backup files
	neo4jDir := filepath.Join(backupDir, neo4jBackupDirName)
	if err := calculateDirectoryChecksums(backupDir, neo4jDir, checksums); err != nil {
		return nil, fmt.Errorf("failed to calculate Neo4j backup checksums: %w", err)
	}

	// Calculate checksum for Prefect DB dump if included
	if !excludeTaskManager {
		prefectPath := filepath.Join(backupDir, prefectDumpFilename)
		if err := calculateFileChecksum(backupDir, prefectPath, prefectDumpFilename, checksums); err != nil {
			return nil, err
		}
	}

	return checksums, nil
}

// calculateDirectoryChecksums walks a directory and calculates checksums for all files
func calculateDirectoryChecksums(baseDir, targetDir string, checksums map[string]string) error {
	return filepath.Walk(targetDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}

		relPath, err := filepath.Rel(baseDir, path)
		if err != nil {
			return fmt.Errorf("failed to get relative path for %s: %w", path, err)
		}

		sum, err := calculateSHA256(path)
		if err != nil {
			return fmt.Errorf("failed to calculate checksum for %s: %w", relPath, err)
		}

		checksums[relPath] = sum
		return nil
	})
}

// calculateFileChecksum calculates checksum for a single file if it exists
func calculateFileChecksum(baseDir, filePath, relativeName string, checksums map[string]string) error {
	stat, err := os.Stat(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // File doesn't exist, not an error
		}
		return fmt.Errorf("failed to access %s: %w", relativeName, err)
	}

	if !stat.IsDir() {
		sum, err := calculateSHA256(filePath)
		if err != nil {
			return fmt.Errorf("failed to calculate %s checksum: %w", relativeName, err)
		}
		checksums[relativeName] = sum
	}

	return nil
}

// validateBackupChecksums validates all checksums in the backup metadata
func validateBackupChecksums(workDir string, metadata *BackupMetadata, excludeTaskManager bool) error {
	backupDir := filepath.Join(workDir, "backup")

	// Validate Neo4j backup file checksums
	for relPath, expectedSum := range metadata.Checksums {
		if relPath == prefectDumpFilename {
			continue // Handle separately
		}

		filePath := filepath.Join(backupDir, relPath)
		if err := validateFileChecksum(filePath, relPath, expectedSum); err != nil {
			return err
		}
	}

	// Validate Prefect DB dump checksum if applicable
	if !excludeTaskManager {
		prefectPath := filepath.Join(backupDir, prefectDumpFilename)
		if _, err := os.Stat(prefectPath); err == nil {
			expectedSum, ok := metadata.Checksums[prefectDumpFilename]
			if !ok {
				return fmt.Errorf("missing checksum for %s in metadata", prefectDumpFilename)
			}
			if err := validateFileChecksum(prefectPath, prefectDumpFilename, expectedSum); err != nil {
				return err
			}
		}
	}

	return nil
}

// validateFileChecksum validates a single file's checksum
func validateFileChecksum(filePath, name, expectedSum string) error {
	if _, err := os.Stat(filePath); err != nil {
		return fmt.Errorf("missing backup file: %s", name)
	}

	actualSum, err := calculateSHA256(filePath)
	if err != nil {
		return fmt.Errorf("failed to calculate checksum for %s: %w", name, err)
	}

	if actualSum != expectedSum {
		return fmt.Errorf("checksum mismatch for %s: expected %s, got %s", name, expectedSum, actualSum)
	}

	return nil
}
