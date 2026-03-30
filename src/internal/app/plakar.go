package app

import (
	"bytes"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/PlakarKorp/kloset/caching"
	"github.com/PlakarKorp/kloset/caching/pebble"
	"github.com/PlakarKorp/kloset/connectors/storage"
	"github.com/PlakarKorp/kloset/hashing"
	"github.com/PlakarKorp/kloset/kcontext"
	"github.com/PlakarKorp/kloset/logging"
	"github.com/PlakarKorp/kloset/repository"
	"github.com/PlakarKorp/kloset/resources"
	"github.com/PlakarKorp/kloset/versioning"
	"github.com/sirupsen/logrus"

	// Register filesystem storage backend (handles fs:// URIs)
	_ "github.com/PlakarKorp/integration-fs/exporter"
	_ "github.com/PlakarKorp/integration-fs/importer"
	_ "github.com/PlakarKorp/integration-fs/storage"

	// Register S3 storage backend (handles s3:// URIs)
	_ "github.com/PlakarKorp/integration-s3/storage"
)

// defaultCacheDir returns the default Plakar cache directory.
func defaultCacheDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.TempDir()
	}
	return filepath.Join(home, ".cache", "infrahub-backup", "plakar")
}

// initPlakarContext creates and configures a KContext for Plakar operations.
func initPlakarContext(cfg *PlakarConfig) (*kcontext.KContext, error) {
	kctx := kcontext.NewKContext()

	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}
	kctx.Hostname = hostname

	cwd, err := os.Getwd()
	if err != nil {
		cwd = "/"
	}
	kctx.CWD = cwd
	kctx.MaxConcurrency = 4
	kctx.Client = "infrahub-backup"

	// Set up logging — route kloset logs through logrus
	logger := logging.NewLogger(os.Stdout, os.Stderr)
	kctx.SetLogger(logger)

	// Set up caching with pebble backend
	cacheDir := cfg.CacheDir
	if cacheDir == "" {
		cacheDir = defaultCacheDir()
	}
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create cache directory %s: %w", cacheDir, err)
	}

	cacheMgr := caching.NewManager(pebble.Constructor(cacheDir))
	kctx.SetCache(cacheMgr)

	logrus.Debugf("Initialized Plakar context (cache: %s)", cacheDir)
	return kctx, nil
}

// storeConfig builds the storage configuration map for a given repo path.
// Local paths are prefixed with fs:// for the integration-fs backend.
// For s3:// URIs, credentials are resolved in order:
//  1. URL userinfo (s3://access_key:secret_key@host/...) — also forces TLS off
//  2. AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY environment variables
//
// TLS defaults to true (secure). It is disabled when:
//   - URL contains userinfo (backward compat — typically local MinIO), or
//   - INFRAHUB_S3_ENDPOINT starts with http://
func storeConfig(repoPath string) map[string]string {
	location := repoPath
	if !strings.Contains(repoPath, "://") {
		absPath, err := filepath.Abs(repoPath)
		if err == nil {
			location = absPath
		}
		location = "fs://" + location
	}

	cfg := map[string]string{"location": location}

	if strings.HasPrefix(location, "s3://") {
		// TLS: default true, override from INFRAHUB_S3_ENDPOINT scheme
		useTLS := true
		if ep := os.Getenv("INFRAHUB_S3_ENDPOINT"); strings.HasPrefix(ep, "http://") {
			useTLS = false
		}

		if u, err := url.Parse(location); err == nil && u.User != nil {
			// Credentials from URL userinfo (highest priority)
			cfg["access_key"] = u.User.Username()
			if secret, ok := u.User.Password(); ok {
				cfg["secret_access_key"] = secret
			}
			// Strip userinfo from the location so the S3 backend only sees host/path
			u.User = nil
			cfg["location"] = u.String()
			useTLS = false // embedded creds = local S3, backward compat
		} else {
			// Fallback: AWS environment variables
			if ak := os.Getenv("AWS_ACCESS_KEY_ID"); ak != "" {
				cfg["access_key"] = ak
			}
			if sk := os.Getenv("AWS_SECRET_ACCESS_KEY"); sk != "" {
				cfg["secret_access_key"] = sk
			}
		}

		cfg["use_tls"] = strconv.FormatBool(useTLS)
	}

	return cfg
}

// openRepo opens an existing Plakar repository. Returns an error if the repository does not exist.
func openRepo(kctx *kcontext.KContext, cfg *PlakarConfig) (*repository.Repository, error) {
	sc := storeConfig(cfg.RepoPath)

	store, configBytes, err := storage.Open(kctx, sc)
	if err != nil {
		return nil, fmt.Errorf("failed to open plakar repository %s: %w", cfg.RepoPath, err)
	}

	var secret []byte // plaintext
	repo, err := repository.New(kctx, secret, store, configBytes)
	if err != nil {
		store.Close(kctx.Context)
		return nil, fmt.Errorf("failed to open plakar repository: %w", err)
	}
	logrus.Debugf("Opened existing Plakar repository: %s", cfg.RepoPath)
	return repo, nil
}

// openOrCreateRepo opens an existing Plakar repository, or creates a new one if it doesn't exist.
func openOrCreateRepo(kctx *kcontext.KContext, cfg *PlakarConfig) (*repository.Repository, error) {
	sc := storeConfig(cfg.RepoPath)

	// Try to open existing repository
	store, configBytes, err := storage.Open(kctx, sc)
	if err == nil {
		// Existing repo — open it
		var secret []byte // plaintext
		repo, err := repository.New(kctx, secret, store, configBytes)
		if err != nil {
			store.Close(kctx.Context)
			return nil, fmt.Errorf("failed to open plakar repository: %w", err)
		}
		logrus.Debugf("Opened existing Plakar repository: %s", cfg.RepoPath)
		return repo, nil
	}

	// Repository doesn't exist — create a new one
	logrus.Infof("Creating new Plakar repository: %s", cfg.RepoPath)

	storageConfig := storage.NewConfiguration()
	// Plaintext by default — no encryption
	storageConfig.Encryption = nil

	rawConfigBytes, err := storageConfig.ToBytes()
	if err != nil {
		return nil, fmt.Errorf("failed to serialize storage configuration: %w", err)
	}

	// Wrap config bytes with kloset serialization header (magic + version + HMAC)
	hasher := hashing.GetHasher(hashing.DEFAULT_HASHING_ALGORITHM)
	wrappedConfigRd, err := storage.Serialize(hasher, resources.RT_CONFIG,
		versioning.GetCurrentVersion(resources.RT_CONFIG), bytes.NewReader(rawConfigBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to wrap storage configuration: %w", err)
	}
	wrappedConfig, err := io.ReadAll(wrappedConfigRd)
	if err != nil {
		return nil, fmt.Errorf("failed to read wrapped configuration: %w", err)
	}

	createdStore, err := storage.Create(kctx, sc, wrappedConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create plakar repository: %w", err)
	}
	createdStore.Close(kctx.Context)

	// Re-open to get config bytes for repository.New()
	store, configBytes, err = storage.Open(kctx, sc)
	if err != nil {
		return nil, fmt.Errorf("failed to open newly created plakar repository: %w", err)
	}

	var secret []byte // plaintext
	repo, err := repository.New(kctx, secret, store, configBytes)
	if err != nil {
		store.Close(kctx.Context)
		return nil, fmt.Errorf("failed to initialize plakar repository: %w", err)
	}

	logrus.Infof("Plakar repository created: %s", cfg.RepoPath)
	return repo, nil
}

// closeRepo closes a Plakar repository, logging any errors.
func closeRepo(repo *repository.Repository) {
	if repo == nil {
		return
	}
	if err := repo.Close(); err != nil {
		logrus.Warnf("Failed to close Plakar repository: %v", err)
	}
}

// closePlakarContext cleans up a Plakar context (cache manager, cancel).
func closePlakarContext(kctx *kcontext.KContext) {
	if kctx == nil {
		return
	}
	cache := kctx.GetCache()
	if cache != nil {
		if err := cache.Close(); err != nil {
			logrus.Warnf("Failed to close Plakar cache: %v", err)
		}
	}
	kctx.Close()
}
