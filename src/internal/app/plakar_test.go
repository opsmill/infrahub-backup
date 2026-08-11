package app

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/minio/minio-go/v7"
)

// Only a genuinely absent repository may be created. Every other open failure —
// an unmounted volume whose CONFIG cannot be read, a permission problem, an S3
// error — must be reported, because creating instead would produce a new (and
// without --encrypt, plaintext) repository while the operator believes the backup
// landed in the real one.
func TestRepoMissingClassification(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil is not a missing repo", nil, false},
		{"fs CONFIG absent", &fs.PathError{Op: "open", Path: "/repo/CONFIG", Err: fs.ErrNotExist}, true},
		{"fs CONFIG absent, wrapped", fmt.Errorf("opening: %w", &fs.PathError{Err: fs.ErrNotExist}), true},
		{"fs permission denied", &fs.PathError{Op: "open", Path: "/repo/CONFIG", Err: fs.ErrPermission}, false},
		{"path component is not a directory", &fs.PathError{Op: "open", Path: "/file/repo/CONFIG", Err: syscall.ENOTDIR}, false},
		{"s3 CONFIG object absent", fmt.Errorf("error reading object: %w", minio.ErrorResponse{Code: "NoSuchKey"}), true},
		{"s3 bucket absent", fmt.Errorf("error getting object: %w", minio.ErrorResponse{Code: "NoSuchBucket"}), true},
		{"s3 access denied", fmt.Errorf("error getting object: %w", minio.ErrorResponse{Code: "AccessDenied"}), false},
		{"s3 server error", fmt.Errorf("error getting object: %w", minio.ErrorResponse{Code: "InternalError"}), false},
		{"s3 untyped absent bucket", errors.New("error checking if bucket exists: bucket does not exist"), true},
		{"unrelated failure", errors.New("connection reset by peer"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := repoMissing(tc.err); got != tc.want {
				t.Errorf("repoMissing(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// kloset resolves its per-snapshot scratch state under kctx.CacheDir. Left unset,
// the join is relative and every backup litters the working directory with a
// "<cache-version>/store" tree — 28 MB across 132 directories had accumulated under
// src/internal/app/ from this branch's tests. The state must land under the
// configured cache directory instead.
func TestPlakarContextUsesTheConfiguredCacheDir(t *testing.T) {
	cacheDir := t.TempDir()
	kctx, err := initPlakarContext(&PlakarConfig{RepoPath: filepath.Join(t.TempDir(), "repo"), CacheDir: cacheDir})
	if err != nil {
		t.Fatalf("initPlakarContext: %v", err)
	}
	defer closePlakarContext(kctx)

	if kctx.CacheDir != cacheDir {
		t.Errorf("kctx.CacheDir = %q, want %q — an empty value makes kloset's scratch path relative to the CWD", kctx.CacheDir, cacheDir)
	}
	if !filepath.IsAbs(kctx.CacheDir) {
		t.Errorf("kctx.CacheDir = %q, want an absolute path", kctx.CacheDir)
	}
}

// An unreadable repository location must fail on the OPEN, not silently fall
// through to creating a fresh repository there. The path here has a regular file
// where a directory is needed, so opening CONFIG fails with ENOTDIR rather than
// ErrNotExist — the same shape as an unmounted backup volume or an unreadable
// CONFIG, and the case that used to be read as "no repository yet".
func TestOpenOrCreateRepoFailsLoudlyWhenTheRepoCannotBeRead(t *testing.T) {
	base := t.TempDir()
	blocker := filepath.Join(base, "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &PlakarConfig{RepoPath: filepath.Join(blocker, "repo"), CacheDir: t.TempDir()}
	kctx, err := initPlakarContext(cfg)
	if err != nil {
		t.Fatalf("initPlakarContext: %v", err)
	}
	defer closePlakarContext(kctx)

	_, err = openOrCreateRepo(kctx, cfg)
	if err == nil {
		t.Fatal("openOrCreateRepo succeeded on an unreadable repository location")
	}
	if !strings.Contains(err.Error(), "failed to open plakar repository") {
		t.Fatalf("error = %v, want the open failure reported rather than a creation attempt", err)
	}
}

// The ordinary case still has to work: a path with nothing at it is a new
// repository, and openOrCreateRepo creates it.
func TestOpenOrCreateRepoStillCreatesAnAbsentRepo(t *testing.T) {
	cfg := &PlakarConfig{RepoPath: filepath.Join(t.TempDir(), "fresh"), CacheDir: t.TempDir()}
	kctx, err := initPlakarContext(cfg)
	if err != nil {
		t.Fatalf("initPlakarContext: %v", err)
	}
	defer closePlakarContext(kctx)

	repo, err := openOrCreateRepo(kctx, cfg)
	if err != nil {
		t.Fatalf("openOrCreateRepo on an absent repo: %v", err)
	}
	closeRepo(repo)
	if _, err := os.Stat(filepath.Join(cfg.RepoPath, "CONFIG")); err != nil {
		t.Fatalf("repository was not created: %v", err)
	}
}
