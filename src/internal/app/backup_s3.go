package app

import (
	"context"
	"fmt"
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

// s3Downloader is the slice of S3Client a restore's download needs. The download depends on
// it rather than on the concrete client so that where the object lands is exercisable
// without a bucket — against real S3, downloading onto an archive's own name looks like a
// perfectly successful restore right up to the point where the archive is gone.
type s3Downloader interface {
	Download(ctx context.Context, s3Key, localPath string) error
}

var _ s3Downloader = (*S3Client)(nil)

// downloadBackupFromS3 downloads the object a positional `s3://bucket/key` argument names,
// and reports the local path the restore should read.
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

	return downloadRestoreObject(context.Background(), client, iops.config.BackupDir, key)
}

// downloadRestoreObject fetches key into dir under a reserved temporary name and reports
// that path. The caller owns the file and is responsible for removing it.
//
// The temporary name is what keeps the download away from dir/<archive-name>, which
// truncated a local archive of the same name and then let the restore path delete it —
// `create --s3-upload --s3-keep-local` makes exactly that collision the normal state of a
// host, so the local copy an operator reaches for when the bucket is unreachable was being
// consumed by a restore that reported success. See restore_inputs.go for the convention.
func downloadRestoreObject(ctx context.Context, client s3Downloader, dir, key string) (string, error) {
	localPath, err := reserveRestoreTempPath(dir, s3URIRestoreTempPattern)
	if err != nil {
		return "", fmt.Errorf("failed to prepare the download of %s: %w", key, err)
	}

	downloadCtx, cancel := context.WithTimeout(ctx, s3RestoreDownloadTimeout)
	defer cancel()

	if err := client.Download(downloadCtx, key, localPath); err != nil {
		// A failed download leaves nothing occupying the backup directory: the archive's
		// worth of disk would otherwise be held for nobody.
		removeRestoreTempPath(localPath)

		return "", fmt.Errorf("failed to download backup from S3: %w", err)
	}

	return localPath, nil
}
