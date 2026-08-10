package app

import (
	"os"
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
	// A remote repo on a real host is passed through untouched.
	remote := "s3://key:secret@minio.example.com:9000/bucket/prefix"
	if got := repoArgFor(remote); got != remote {
		t.Errorf("repoArgFor(%q) = %q, want it passed through unchanged", remote, got)
	}

	// A loopback host is rewritten, because inside the runner "localhost" is the
	// runner. Everything else about the URI has to survive — dropping the port is a
	// mistake this asserts against, having made it once.
	loopback := "s3://key:secret@localhost:9000/bucket/prefix"
	want := "s3://key:secret@host.docker.internal:9000/bucket/prefix"
	if got := repoArgFor(loopback); got != want {
		t.Errorf("repoArgFor(%q) = %q, want %q", loopback, got, want)
	}
}

// preserveOwnership must be inert where there is nothing to preserve: the Postgres
// restore shares no volumes, so the directory it is told about may not exist.
func TestPreserveOwnershipIsInertWithoutADirectory(t *testing.T) {
	for _, dir := range []string{"", filepath.Join(t.TempDir(), "absent")} {
		apply, err := preserveOwnership(dir)
		if err != nil {
			t.Fatalf("preserveOwnership(%q) errored: %v", dir, err)
		}
		if err := apply(); err != nil {
			t.Errorf("applying preserved ownership for %q errored: %v", dir, err)
		}
	}
}

// The walk has to cope with a tree that grew during the restore, including
// subdirectories and symlinks, without erroring. The chown itself only does work where
// the uid actually differs, which needs two uids and therefore the runner in CI; this
// covers the traversal.
func TestPreserveOwnershipWalksATreeGrownDuringRestore(t *testing.T) {
	dir := t.TempDir()
	apply, err := preserveOwnership(dir)
	if err != nil {
		t.Fatalf("preserveOwnership errored: %v", err)
	}

	nested := filepath.Join(dir, "databases", "neo4j")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "store.db"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(nested, "store.db"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}

	if err := apply(); err != nil {
		t.Errorf("applying preserved ownership over a grown tree errored: %v", err)
	}
}

// containerReachable is what lets the runner reach an S3 endpoint published on the
// Docker host. It must only ever touch the host, and only for loopback.
func TestContainerReachable(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"s3 uri with credentials and port", "s3://k:s@localhost:9000/b/p", "s3://k:s@host.docker.internal:9000/b/p"},
		{"loopback ipv4", "http://127.0.0.1:9000", "http://host.docker.internal:9000"},
		{"loopback ipv6", "http://[::1]:9000", "http://host.docker.internal:9000"},
		{"bare host:port keeps its shape", "localhost:9000", "host.docker.internal:9000"},
		{"no port", "http://localhost", "http://host.docker.internal"},
		{"real host untouched", "https://s3.eu-west-1.amazonaws.com/bucket", "https://s3.eu-west-1.amazonaws.com/bucket"},
		{"real host with port untouched", "http://minio.internal:9000/b", "http://minio.internal:9000/b"},
		{"host merely containing localhost untouched", "http://localhost.example.com:9000", "http://localhost.example.com:9000"},
		{"empty stays empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := containerReachable(tc.in); got != tc.want {
				t.Errorf("containerReachable(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
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
