package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/PlakarKorp/kloset/caching"
)

// TestPlakarCacheLandsUnderTheConfiguredCacheDir pins where a Plakar run keeps
// its state.
//
// kloset reads the cache location from two places for two caches. The scan and
// packing caches come from the manager the context is given; the repository's
// own state cache is derived from KContext.CacheDir, which initPlakarContext
// left at the zero value. path.Join of an empty first element yields a relative
// path, so every plakar create, restore and snapshot listing wrote a
// `<cache version>/store/<repository uuid>` tree into whatever directory the
// tool was run from — and found the previous run's state only if it was run
// from the same one. Neither --plakar-cache-dir nor the default under ~/.cache
// reached that cache.
//
// The assertion is deliberately about the absence in the working directory as
// well as the presence under the configured directory: a cache that is written
// to both would satisfy the second half alone.
func TestPlakarCacheLandsUnderTheConfiguredCacheDir(t *testing.T) {
	cacheDir := t.TempDir()
	cfg := &PlakarConfig{RepoPath: filepath.Join(t.TempDir(), "repo"), CacheDir: cacheDir}

	kctx, err := initPlakarContext(cfg)
	if err != nil {
		t.Fatalf("initPlakarContext() = %v, want nil", err)
	}
	defer closePlakarContext(kctx)

	if kctx.CacheDir != cacheDir {
		t.Errorf("kctx.CacheDir = %q, want the configured %q: the repository state cache is derived from this field, not from the cache manager", kctx.CacheDir, cacheDir)
	}

	repo, err := openOrCreateRepo(kctx, cfg)
	if err != nil {
		t.Fatalf("openOrCreateRepo() = %v, want nil", err)
	}
	defer closeRepo(repo)

	// Opening the repository is what creates the state cache, so this is the
	// first moment the question can be asked.
	stateCache := filepath.Join(cacheDir, caching.CACHE_VERSION, "store")
	if _, err := os.Stat(stateCache); err != nil {
		t.Errorf("stat(%s) = %v, want the repository state cache under the configured cache directory", stateCache, err)
	}

	// The working directory is the package directory under `go test`, which is
	// inside the repository — so a regression here is visible as an untracked
	// directory appearing in the source tree.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd() = %v, want nil", err)
	}
	strayCache := filepath.Join(cwd, caching.CACHE_VERSION)
	if _, err := os.Stat(strayCache); err == nil {
		t.Errorf("%s exists, want the state cache only under the configured cache directory: an empty KContext.CacheDir makes kloset write it relative to wherever the tool was run from", strayCache)
	}
}
