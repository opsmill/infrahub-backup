package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// uploadBackupToS3 uploads the backup file to S3
func (iops *InfrahubOps) uploadBackupToS3(backupPath string) (string, error) {
	if err := iops.config.S3.ValidateConfig(); err != nil {
		return "", err
	}

	client, err := NewS3Client(iops.config.S3)
	if err != nil {
		return "", fmt.Errorf("failed to create S3 client: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	return client.Upload(ctx, backupPath)
}

// downloadBackupFromS3 downloads a backup from S3
func (iops *InfrahubOps) downloadBackupFromS3(s3URI string) (string, error) {
	bucket, key, ok := ParseS3URI(s3URI)
	if !ok {
		return "", fmt.Errorf("invalid S3 URI: %s", s3URI)
	}

	// Create S3 config from URI, using CLI flags for endpoint/region
	s3Config := &S3Config{
		Bucket:   bucket,
		Endpoint: iops.config.S3.Endpoint,
		Region:   iops.config.S3.Region,
	}

	client, err := NewS3Client(s3Config)
	if err != nil {
		return "", fmt.Errorf("failed to create S3 client: %w", err)
	}

	// Ensure backup directory exists
	if err := os.MkdirAll(iops.config.BackupDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create backup directory: %w", err)
	}

	// Download to local backup directory
	filename := filepath.Base(key)
	localPath := filepath.Join(iops.config.BackupDir, filename)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	if err := client.Download(ctx, key, localPath); err != nil {
		return "", fmt.Errorf("failed to download backup from S3: %w", err)
	}

	return localPath, nil
}
