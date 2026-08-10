package app

import (
	"path/filepath"
	"strings"
	"testing"
)

// The two spellings of a local repository must be indistinguishable to every
// decision the runner makes. They were not: fs:///path contains "://", so the
// documented spelling was classified as remote, and the runner passed a host path
// into a container that has no such path — the repository then failed to open on a
// missing CONFIG. These tests pin both spellings to the same answers.
func TestParseRepoLocation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		repo      string
		wantLocal bool
		wantPath  string
	}{
		{"bare absolute path", "/backups/infra", true, "/backups/infra"},
		{"fs scheme, absolute path", "fs:///backups/infra", true, "/backups/infra"},
		{"bare relative path", "backups/infra", true, "backups/infra"},
		{"fs scheme, relative path", "fs://backups/infra", true, "backups/infra"},
		{"s3 uri", "s3://key:secret@localhost:9000/bucket/prefix", false, "s3://key:secret@localhost:9000/bucket/prefix"},
		{"unknown scheme stays remote", "gs://bucket/prefix", false, "gs://bucket/prefix"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			local, path := parseRepoLocation(tc.repo)
			if local != tc.wantLocal {
				t.Errorf("parseRepoLocation(%q) local = %v, want %v", tc.repo, local, tc.wantLocal)
			}
			if path != tc.wantPath {
				t.Errorf("parseRepoLocation(%q) path = %q, want %q", tc.repo, path, tc.wantPath)
			}
		})
	}
}

// repoArgFor decides what the in-container worker is told to open. For a local repo
// that must be the mount point, never the host path, whichever spelling was used.
func TestRepoArgForUsesMountPointForBothLocalSpellings(t *testing.T) {
	for _, repo := range []string{"/backups/infra", "fs:///backups/infra"} {
		if got := repoArgFor(repo); got != "/repo" {
			t.Errorf("repoArgFor(%q) = %q, want %q — a host path cannot be opened inside the runner", repo, got, "/repo")
		}
	}
	remote := "s3://key:secret@localhost:9000/bucket/prefix"
	if got := repoArgFor(remote); got != remote {
		t.Errorf("repoArgFor(%q) = %q, want it passed through unchanged", remote, got)
	}
}

// storeConfig is what actually opens the repository, so the two spellings must name
// the same absolute location — otherwise a backup and a later restore could disagree
// about where the repository is.
func TestStoreConfigLocationIdenticalForBothLocalSpellings(t *testing.T) {
	dir := t.TempDir()

	bare := storeConfig(dir)["location"]
	scheme := storeConfig("fs://" + dir)["location"]

	if bare != scheme {
		t.Fatalf("location differs by spelling: bare=%q fs=%q", bare, scheme)
	}
	if want := "fs://" + dir; bare != want {
		t.Errorf("location = %q, want %q", bare, want)
	}
}

// A relative local repo must be made absolute in both spellings; the fs:// form used
// to skip the filepath.Abs call that the bare form got.
func TestStoreConfigMakesRelativeLocalPathsAbsolute(t *testing.T) {
	for _, repo := range []string{"relative/repo", "fs://relative/repo"} {
		location := storeConfig(repo)["location"]
		path := strings.TrimPrefix(location, "fs://")
		if !filepath.IsAbs(path) {
			t.Errorf("storeConfig(%q) location = %q, want an absolute path", repo, location)
		}
	}
}
