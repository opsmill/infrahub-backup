package app

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/sirupsen/logrus"
)

// S3Config holds S3-related configuration
type S3Config struct {
	Bucket   string
	Prefix   string
	Endpoint string
	Region   string
}

// S3Client wraps the AWS S3 client
type S3Client struct {
	client *s3.Client
	config *S3Config
}

// NewS3Client creates a new S3 client with the given configuration
func NewS3Client(cfg *S3Config) (*S3Client, error) {
	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}

	opts := []func(*config.LoadOptions) error{
		config.WithRegion(region),
	}

	awsCfg, err := config.LoadDefaultConfig(context.Background(), opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	s3Opts := []func(*s3.Options){}

	// Custom endpoint for S3-compatible storage (MinIO, etc.)
	if cfg.Endpoint != "" {
		s3Opts = append(s3Opts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
			o.UsePathStyle = true // Required for MinIO and most S3-compatible services
			// https://github.com/aws/aws-sdk-go-v2/discussions/2960
			o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
			o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
		})
		logrus.Debugf("Using custom S3 endpoint: %s", cfg.Endpoint)
	}

	client := s3.NewFromConfig(awsCfg, s3Opts...)

	return &S3Client{
		client: client,
		config: cfg,
	}, nil
}

// ValidateConfig validates the S3 configuration for upload/download operations
func (cfg *S3Config) ValidateConfig() error {
	if cfg.Bucket == "" {
		return fmt.Errorf("S3 bucket is required when S3 upload/download is enabled (use --s3-bucket or INFRAHUB_S3_BUCKET)")
	}
	return nil
}

// buildS3Key constructs the full S3 key from prefix and filename
func (c *S3Client) buildS3Key(filename string) string {
	if c.config.Prefix == "" {
		return filename
	}
	// Use forward slashes for S3 keys
	return strings.TrimSuffix(c.config.Prefix, "/") + "/" + filename
}

// Upload uploads a local file to S3 and returns the S3 URI
func (c *S3Client) Upload(ctx context.Context, localPath string) (string, error) {
	file, err := os.Open(localPath)
	if err != nil {
		return "", fmt.Errorf("failed to open file for upload: %w", err)
	}
	defer file.Close()

	filename := filepath.Base(localPath)
	s3Key := c.buildS3Key(filename)

	// Get file size for progress logging
	stat, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("failed to stat file: %w", err)
	}

	logrus.Infof("Uploading %s (%s) to s3://%s/%s",
		filename, formatBytes(stat.Size()), c.config.Bucket, s3Key)

	// Use the S3 manager for multipart uploads of large files
	uploader := manager.NewUploader(c.client, func(u *manager.Uploader) {
		u.PartSize = 64 * 1024 * 1024 // 64MB parts
		u.Concurrency = 4
	})

	_, err = uploader.Upload(ctx, &s3.PutObjectInput{
		Bucket: aws.String(c.config.Bucket),
		Key:    aws.String(s3Key),
		Body:   file,
	})
	if err != nil {
		return "", fmt.Errorf("failed to upload to S3: %w", err)
	}

	s3URI := fmt.Sprintf("s3://%s/%s", c.config.Bucket, s3Key)
	logrus.Infof("Upload complete: %s", s3URI)

	return s3URI, nil
}

// Download downloads a file from S3 to a local path
func (c *S3Client) Download(ctx context.Context, s3Key, localPath string) error {
	logrus.Infof("Downloading s3://%s/%s to %s", c.config.Bucket, s3Key, localPath)

	// Create the local file
	file, err := os.Create(localPath)
	if err != nil {
		return fmt.Errorf("failed to create local file: %w", err)
	}
	defer file.Close()

	// Use the S3 manager for efficient downloads
	downloader := manager.NewDownloader(c.client, func(d *manager.Downloader) {
		d.PartSize = 64 * 1024 * 1024 // 64MB parts
		d.Concurrency = 4
	})

	numBytes, err := downloader.Download(ctx, file, &s3.GetObjectInput{
		Bucket: aws.String(c.config.Bucket),
		Key:    aws.String(s3Key),
	})
	if err != nil {
		os.Remove(localPath) // Clean up partial download
		return fmt.Errorf("failed to download from S3: %w", err)
	}

	logrus.Infof("Download complete: %s (%s)", localPath, formatBytes(numBytes))

	return nil
}

// DownloadToWriter downloads a file from S3 to an io.Writer
func (c *S3Client) DownloadToWriter(ctx context.Context, s3Key string, w io.WriterAt) (int64, error) {
	downloader := manager.NewDownloader(c.client, func(d *manager.Downloader) {
		d.PartSize = 64 * 1024 * 1024 // 64MB parts
		d.Concurrency = 4
	})

	return downloader.Download(ctx, w, &s3.GetObjectInput{
		Bucket: aws.String(c.config.Bucket),
		Key:    aws.String(s3Key),
	})
}

// ParseS3URI parses an s3://bucket/key URI into bucket and key components
// If the URI doesn't have s3:// prefix, it returns empty strings and false
func ParseS3URI(uri string) (bucket, key string, ok bool) {
	if !strings.HasPrefix(uri, "s3://") {
		return "", "", false
	}

	// Remove s3:// prefix
	path := strings.TrimPrefix(uri, "s3://")

	// Split into bucket and key
	parts := strings.SplitN(path, "/", 2)
	if len(parts) == 0 || parts[0] == "" {
		return "", "", false
	}

	bucket = parts[0]
	if len(parts) > 1 {
		key = parts[1]
	}

	return bucket, key, true
}

// IsS3URI returns true if the given string is an S3 URI
func IsS3URI(s string) bool {
	return strings.HasPrefix(s, "s3://")
}
