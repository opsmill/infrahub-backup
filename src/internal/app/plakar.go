package app

import (
	"bytes"
	"errors"
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
	"github.com/PlakarKorp/kloset/encryption"
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

// Encryption-related errors. None of these embed the passphrase (FR-007).
var (
	// errEncryptedRepoNeedsPassphrase is returned when opening an encrypted
	// repository without any passphrase available.
	errEncryptedRepoNeedsPassphrase = errors.New(
		"repository is encrypted; a passphrase is required (set INFRAHUB_BACKUP_PASSPHRASE or --passphrase-file)")
	// errWrongPassphrase is returned when the supplied passphrase fails the
	// repository's canary check (FR-012).
	errWrongPassphrase = errors.New(
		"cannot open encrypted repository: incorrect passphrase")
	// errEncryptWithoutPassphrase is returned when --encrypt is requested but no
	// passphrase is available (FR-006); refused before any repository is created.
	errEncryptWithoutPassphrase = errors.New(
		"--encrypt requires a passphrase (set INFRAHUB_BACKUP_PASSPHRASE or --passphrase-file)")
	// errEncryptKeyOnPlakar is returned when the legacy tarball --encrypt-key flag
	// is used with the plakar backend, which uses passphrase-derived symmetric keys.
	errEncryptKeyOnPlakar = errors.New(
		"--encrypt-key is only valid for the tarball backend; for --backend plakar use --encrypt with a passphrase (INFRAHUB_BACKUP_PASSPHRASE or --passphrase-file)")
	// errEncryptExistingPlaintextRepo is returned when --encrypt targets a
	// repository that already exists as plaintext. Encryption is fixed at
	// creation (FR-008); refusing loudly avoids silently appending plaintext to a
	// repo the operator believes is being encrypted.
	errEncryptExistingPlaintextRepo = errors.New(
		"cannot enable encryption on an existing plaintext repository; encryption is fixed at repository creation — create a new repository (a different --repo) with --encrypt")
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
	// Both spellings of a local repo — /path and fs:///path — must resolve to the
	// same storage location, absolute in each case, so that the two cannot disagree
	// about which directory the repository lives in.
	if local, path := parseRepoLocation(repoPath); local {
		if absPath, err := filepath.Abs(path); err == nil {
			path = absPath
		}
		location = "fs://" + path
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

// inspectEncryption reads the storage CONFIG to detect whether the repository is
// encrypted and, if so, returns its encryption parameters (KDF + canary). The
// CONFIG payload (salt, KDF params, canary) is NOT itself encrypted — only the
// CONFIG wrapper's trailing HMAC is keyed by the repository secret — so we can
// read it with the plaintext hasher. For an encrypted repo that HMAC won't match
// the plaintext hasher, so Deserialize's reader reports "hmac mismatch" at EOF;
// the payload bytes are fully delivered before that check, so we intentionally
// ignore the read error here and rely on NewConfigurationFromBytes to catch a
// genuinely corrupt config. The real authentication happens later in
// repository.New, which re-reads the CONFIG with the secret-keyed MAC hasher.
func inspectEncryption(configBytes []byte) (*encryption.Configuration, error) {
	hasher := hashing.GetHasher(hashing.DEFAULT_HASHING_ALGORITHM)
	version, rd, err := storage.Deserialize(hasher, resources.RT_CONFIG, io.NopCloser(bytes.NewReader(configBytes)))
	if err != nil {
		return nil, fmt.Errorf("reading repository configuration: %w", err)
	}
	// The reader reports an HMAC mismatch at EOF for an encrypted repo (we used
	// the plaintext hasher), but the payload is fully delivered before that
	// check — so a non-nil readErr is expected and the config still parses. If
	// the config ALSO fails to parse, the read error is the more informative one:
	// it points at a truncated/corrupt CONFIG (e.g. a short read) rather than a
	// benign HMAC mismatch.
	raw, readErr := io.ReadAll(rd)
	cfg, err := storage.NewConfigurationFromBytes(version, raw)
	if err != nil {
		if readErr != nil {
			return nil, fmt.Errorf("reading repository configuration (possibly truncated or corrupt): %w", readErr)
		}
		return nil, fmt.Errorf("parsing repository configuration: %w", err)
	}
	return cfg.Encryption, nil
}

// newRepository wraps repository.New, deriving and verifying the symmetric secret
// when the repository is encrypted. The cases follow the open contract:
//   - requireEncrypted (--encrypt) but repo is plaintext → errEncryptExistingPlaintextRepo
//   - plaintext repo, no passphrase           → open plaintext
//   - plaintext repo, passphrase supplied      → warn + ignore the passphrase (VR-3)
//   - encrypted repo, no passphrase            → errEncryptedRepoNeedsPassphrase
//   - encrypted repo, passphrase supplied      → derive key, VerifyCanary, then open
//
// requireEncrypted is set only when the caller explicitly requested encryption
// at create time (--encrypt); it turns the otherwise-benign "passphrase supplied
// for a plaintext repo" warning into a hard error so encryption can never be
// silently downgraded on an existing plaintext repo (FR-008).
//
// No read or write of repository contents happens before the canary check (FR-012).
func newRepository(kctx *kcontext.KContext, store storage.Store, configBytes []byte, passphrase string, requireEncrypted bool) (*repository.Repository, error) {
	enc, err := inspectEncryption(configBytes)
	if err != nil {
		store.Close(kctx.Context)
		return nil, err
	}

	var secret []byte
	switch {
	case enc == nil && requireEncrypted:
		store.Close(kctx.Context)
		return nil, errEncryptExistingPlaintextRepo
	case enc == nil && passphrase != "":
		logrus.Warn("repository is not encrypted; the supplied passphrase is ignored")
	case enc != nil && passphrase == "":
		store.Close(kctx.Context)
		return nil, errEncryptedRepoNeedsPassphrase
	case enc != nil:
		secret, err = encryption.DeriveKey(enc.KDFParams, []byte(passphrase))
		if err != nil {
			store.Close(kctx.Context)
			return nil, fmt.Errorf("deriving repository key: %w", err)
		}
		if !encryption.VerifyCanary(enc, secret) {
			store.Close(kctx.Context)
			return nil, errWrongPassphrase
		}
	}

	repo, err := repository.New(kctx, secret, store, configBytes)
	if err != nil {
		store.Close(kctx.Context)
		return nil, fmt.Errorf("failed to open plakar repository: %w", err)
	}
	return repo, nil
}

// openRepo opens an existing Plakar repository. Returns an error if the repository does not exist.
func openRepo(kctx *kcontext.KContext, cfg *PlakarConfig) (*repository.Repository, error) {
	sc := storeConfig(cfg.RepoPath)

	store, configBytes, err := storage.Open(kctx, sc)
	if err != nil {
		return nil, fmt.Errorf("failed to open plakar repository %s: %w", cfg.RepoPath, err)
	}

	// openRepo never requires encryption — it opens whatever the repo is (restore/list).
	repo, err := newRepository(kctx, store, configBytes, cfg.Passphrase, false)
	if err != nil {
		return nil, err
	}
	logrus.Debugf("Opened existing Plakar repository: %s", cfg.RepoPath)
	return repo, nil
}

// openOrCreateRepo opens an existing Plakar repository, or creates a new one if it doesn't exist.
func openOrCreateRepo(kctx *kcontext.KContext, cfg *PlakarConfig) (*repository.Repository, error) {
	sc := storeConfig(cfg.RepoPath)

	// Try to open existing repository. When --encrypt was requested, refuse to
	// open a pre-existing plaintext repo rather than silently appending plaintext.
	store, configBytes, err := storage.Open(kctx, sc)
	if err == nil {
		repo, oerr := newRepository(kctx, store, configBytes, cfg.Passphrase, cfg.Encrypt)
		if oerr != nil {
			return nil, oerr
		}
		logrus.Debugf("Opened existing Plakar repository: %s", cfg.RepoPath)
		return repo, nil
	}

	// Repository doesn't exist — create a new one.
	return createRepo(kctx, cfg, sc)
}

// createRepo creates a new Plakar repository, encrypted when cfg.Encrypt is set.
// For an encrypted repo the storage CONFIG carries the symmetric encryption
// parameters (Argon2id KDF + AES256-GCM-SIV) and a canary derived from the
// passphrase, and the CONFIG wrapper is authenticated with the secret-keyed MAC
// hasher — matching what repository.New expects on open.
func createRepo(kctx *kcontext.KContext, cfg *PlakarConfig, sc map[string]string) (*repository.Repository, error) {
	storageConfig := storage.NewConfiguration()

	// hasher authenticates the CONFIG wrapper: plaintext hasher for an
	// unencrypted repo, secret-keyed MAC hasher for an encrypted one.
	hasher := hashing.GetHasher(hashing.DEFAULT_HASHING_ALGORITHM)
	var secret []byte

	if cfg.Encrypt {
		if cfg.Passphrase == "" {
			return nil, errEncryptWithoutPassphrase
		}
		// storage.NewConfiguration() pre-populates a default symmetric encryption
		// configuration; guard the invariant so a future kloset default of nil
		// fails clearly instead of panicking on the derefs below.
		if storageConfig.Encryption == nil {
			return nil, fmt.Errorf("encryption requested but the storage engine did not provide an encryption configuration")
		}
		// Derive the key from its KDF params and stamp the canary.
		key, err := encryption.DeriveKey(storageConfig.Encryption.KDFParams, []byte(cfg.Passphrase))
		if err != nil {
			return nil, fmt.Errorf("deriving repository key: %w", err)
		}
		canary, err := encryption.DeriveCanary(storageConfig.Encryption, key)
		if err != nil {
			return nil, fmt.Errorf("deriving repository canary: %w", err)
		}
		storageConfig.Encryption.Canary = canary
		secret = key
		hasher = hashing.GetMACHasher(storage.DEFAULT_HASHING_ALGORITHM, key)
		logrus.Infof("Creating new ENCRYPTED Plakar repository: %s", cfg.RepoPath)
	} else {
		// Plaintext repository — no encryption (unchanged 003 behavior).
		storageConfig.Encryption = nil
		logrus.Infof("Creating new Plakar repository: %s", cfg.RepoPath)
	}

	rawConfigBytes, err := storageConfig.ToBytes()
	if err != nil {
		return nil, fmt.Errorf("failed to serialize storage configuration: %w", err)
	}

	// Wrap config bytes with kloset serialization header (magic + version + HMAC).
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

	// Re-open to get config bytes for repository.New().
	store, configBytes, err := storage.Open(kctx, sc)
	if err != nil {
		return nil, fmt.Errorf("failed to open newly created plakar repository: %w", err)
	}

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

// PreparePlakarEncryption resolves and validates the encryption inputs for a
// plakar create, storing them on the Plakar config. It rejects the legacy
// tarball --encrypt-key flag, resolves the passphrase (env/file), and — when
// --encrypt is set — refuses an absent or too-short passphrase BEFORE any
// repository is created (FR-006, FR-013).
func (iops *InfrahubOps) PreparePlakarEncryption(encrypt bool, encryptKey, passphraseFile string) error {
	if encryptKey != "" {
		return errEncryptKeyOnPlakar
	}
	if err := iops.LoadPlakarPassphrase(passphraseFile); err != nil {
		return err
	}
	iops.config.Plakar.Encrypt = encrypt
	if encrypt {
		pass := iops.config.Plakar.Passphrase
		if pass == "" {
			return errEncryptWithoutPassphrase
		}
		if err := validatePassphrase(pass); err != nil {
			return err
		}
	}
	return nil
}

// LoadPlakarPassphrase resolves the passphrase (env/file) for opening an
// encrypted repository on restore/list and stores it on the Plakar config.
// A plaintext repo simply ignores an empty (or supplied) passphrase.
func (iops *InfrahubOps) LoadPlakarPassphrase(passphraseFile string) error {
	pass, err := resolvePassphrase(passphraseFile)
	if err != nil {
		return err
	}
	iops.config.Plakar.Passphrase = pass
	return nil
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
