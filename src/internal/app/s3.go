package app

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/sirupsen/logrus"
)

// S3Config holds S3-related configuration
type S3Config struct {
	Bucket   string
	Prefix   string
	Endpoint string
	Region   string
}

// S3Client wraps the minio S3 client.
//
// We use minio-go rather than aws-sdk-go-v2 here because the AWS SDK is
// incompatible with Google Cloud Storage's S3-compatible API: it signs the
// Accept-Encoding header (which GCS rewrites in transit, breaking the
// signature -- aws/aws-sdk-go-v2#1816) and forces aws-chunked CRC32 checksum
// trailers on multipart uploads that GCS rejects (aws/aws-sdk-go-v2#3007).
// minio-go avoids both, and with SendContentMd5 uses a portable Content-MD5
// integrity check. This is the same client the Plakar integration-s3 backend
// already uses successfully against GCS-style providers.
type S3Client struct {
	client *minio.Client
	config *S3Config
}

// NewS3Client creates a new S3 client with the given configuration
func NewS3Client(cfg *S3Config) (*S3Client, error) {
	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}

	// Default to AWS S3; a custom endpoint targets MinIO, GCS, or other
	// S3-compatible storage.
	host := "s3.amazonaws.com"
	secure := true
	pathStyle := false

	if cfg.Endpoint != "" {
		u, err := url.Parse(cfg.Endpoint)
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("invalid S3 endpoint %q (expected a URL like https://storage.googleapis.com): %w", cfg.Endpoint, err)
		}
		host = u.Host
		secure = u.Scheme != "http"
		// Path-style addressing is required for MinIO and most S3-compatible services.
		pathStyle = true
		logrus.Debugf("Using custom S3 endpoint: %s", cfg.Endpoint)
	}

	transport, err := minio.DefaultTransport(secure)
	if err != nil {
		return nil, fmt.Errorf("failed to create S3 transport: %w", err)
	}

	bucketLookup := minio.BucketLookupAuto
	if pathStyle {
		bucketLookup = minio.BucketLookupPath
	}

	// Resolve credentials from the standard AWS sources (environment variables,
	// ~/.aws/credentials, and instance/role metadata).
	creds := credentials.NewChainCredentials([]credentials.Provider{
		&credentials.EnvAWS{},
		&credentials.FileAWSCredentials{},
		&credentials.IAM{},
	})

	client, err := minio.New(host, &minio.Options{
		Creds:        creds,
		Secure:       secure,
		Region:       region,
		BucketLookup: bucketLookup,
		Transport:    transport,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create S3 client: %w", err)
	}

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

	// Get file size for progress logging and the multipart uploader.
	stat, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("failed to stat file: %w", err)
	}

	logrus.Infof("Uploading %s (%s) to s3://%s/%s",
		filename, formatBytes(stat.Size()), c.config.Bucket, s3Key)

	// minio handles multipart uploads automatically for large files.
	_, err = c.client.PutObject(ctx, c.config.Bucket, s3Key, file, stat.Size(), minio.PutObjectOptions{
		// GCS/Backblaze reject aws-chunked checksum trailers; Content-MD5 is the
		// portable integrity check. Matches the integration-s3 storage backend.
		SendContentMd5: true,
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

	obj, err := c.client.GetObject(ctx, c.config.Bucket, s3Key, minio.GetObjectOptions{})
	if err != nil {
		os.Remove(localPath)
		return fmt.Errorf("failed to download from S3: %w", err)
	}
	defer obj.Close()

	written, err := io.Copy(file, obj)
	if err != nil {
		os.Remove(localPath) // Clean up partial download
		return fmt.Errorf("failed to download from S3: %w", err)
	}

	logrus.Infof("Download complete: %s (%s)", localPath, formatBytes(written))

	return nil
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
