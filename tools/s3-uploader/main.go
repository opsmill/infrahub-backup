package main

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
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
// incompatible with Google Cloud Storage's S3-compatible API (signed
// Accept-Encoding header -- aws/aws-sdk-go-v2#1816 -- and forced aws-chunked
// CRC32 checksum trailers on multipart uploads -- aws/aws-sdk-go-v2#3007).
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
		pathStyle = true // Required for MinIO and most S3-compatible services
		fmt.Printf("Using custom S3 endpoint: %s\n", cfg.Endpoint)
	}

	transport, err := minio.DefaultTransport(secure)
	if err != nil {
		return nil, fmt.Errorf("failed to create S3 transport: %w", err)
	}

	bucketLookup := minio.BucketLookupAuto
	if pathStyle {
		bucketLookup = minio.BucketLookupPath
	}

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

	fmt.Printf("Uploading %s (%s) to s3://%s/%s\n",
		filename, formatBytes(stat.Size()), c.config.Bucket, s3Key)

	// minio handles multipart uploads automatically for large files.
	_, err = c.client.PutObject(ctx, c.config.Bucket, s3Key, file, stat.Size(), minio.PutObjectOptions{
		SendContentMd5: true,
	})
	if err != nil {
		return "", fmt.Errorf("failed to upload to S3: %w", err)
	}

	s3URI := fmt.Sprintf("s3://%s/%s", c.config.Bucket, s3Key)
	fmt.Printf("Upload complete: %s\n", s3URI)

	return s3URI, nil
}

func formatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

func main() {
	bucket := flag.String("bucket", "", "S3 bucket name (required)")
	prefix := flag.String("prefix", "", "S3 key prefix (optional)")
	endpoint := flag.String("endpoint", "", "Custom S3 endpoint for S3-compatible storage (e.g., http://localhost:9000 for MinIO)")
	region := flag.String("region", "us-east-1", "AWS region")
	filePath := flag.String("file", "", "Path to the file to upload (required)")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "S3 Uploader Test Tool\n\n")
		fmt.Fprintf(os.Stderr, "Usage: %s [options]\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Options:\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nEnvironment variables:\n")
		fmt.Fprintf(os.Stderr, "  AWS_ACCESS_KEY_ID      AWS access key\n")
		fmt.Fprintf(os.Stderr, "  AWS_SECRET_ACCESS_KEY  AWS secret key\n")
		fmt.Fprintf(os.Stderr, "  AWS_REGION             AWS region (can also use -region flag)\n")
		fmt.Fprintf(os.Stderr, "\nExamples:\n")
		fmt.Fprintf(os.Stderr, "  # Upload to AWS S3\n")
		fmt.Fprintf(os.Stderr, "  %s -bucket my-bucket -file /path/to/file.tar.gz\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  # Upload to MinIO\n")
		fmt.Fprintf(os.Stderr, "  %s -bucket my-bucket -endpoint http://localhost:9000 -file /path/to/file.tar.gz\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  # Upload to Google Cloud Storage\n")
		fmt.Fprintf(os.Stderr, "  %s -bucket my-bucket -endpoint https://storage.googleapis.com -region auto -file /path/to/file.tar.gz\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  # Upload with prefix\n")
		fmt.Fprintf(os.Stderr, "  %s -bucket my-bucket -prefix backups/daily -file /path/to/file.tar.gz\n", os.Args[0])
	}

	flag.Parse()

	if *bucket == "" {
		fmt.Fprintln(os.Stderr, "Error: -bucket is required")
		flag.Usage()
		os.Exit(1)
	}

	if *filePath == "" {
		fmt.Fprintln(os.Stderr, "Error: -file is required")
		flag.Usage()
		os.Exit(1)
	}

	// Check if file exists
	if _, err := os.Stat(*filePath); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Error: file does not exist: %s\n", *filePath)
		os.Exit(1)
	}

	cfg := &S3Config{
		Bucket:   *bucket,
		Prefix:   *prefix,
		Endpoint: *endpoint,
		Region:   *region,
	}

	client, err := NewS3Client(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating S3 client: %v\n", err)
		os.Exit(1)
	}

	ctx := context.Background()
	s3URI, err := client.Upload(ctx, *filePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error uploading file: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("\nSuccessfully uploaded to: %s\n", s3URI)
}
