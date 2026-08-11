package app

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// repoAccessFor decides what the in-container worker is told to open. For a local
// repo that must be the mount point, never the host path, whichever spelling was
// used. For an s3:// repo the location must reach the store from inside the runner
// AND must not carry the credentials, which would then land on `docker run`'s argv
// where docker inspect and the host process list can both read them.
func TestRepoAccessForLocalRepos(t *testing.T) {
	for _, repo := range []string{"/backups/infra", "fs:///backups/infra"} {
		access := repoAccessFor(repo)
		if access.Location != "/repo" {
			t.Errorf("repoAccessFor(%q).Location = %q, want %q — a host path cannot be opened inside the runner", repo, access.Location, "/repo")
		}
		if access.AccessKey != "" || access.SecretKey != "" || access.Insecure {
			t.Errorf("repoAccessFor(%q) = %+v, want no S3 access for a local repo", repo, access)
		}
	}
}

func TestRepoAccessForS3ReposKeepsCredentialsOffArgv(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")

	t.Run("embedded credentials are lifted out of the URI", func(t *testing.T) {
		access := repoAccessFor("s3://key:secret@minio.example.com:9000/bucket/prefix")
		if want := "s3://minio.example.com:9000/bucket/prefix"; access.Location != want {
			t.Errorf("Location = %q, want %q", access.Location, want)
		}
		if strings.Contains(access.Location, "secret") {
			t.Errorf("Location = %q still carries the secret", access.Location)
		}
		if access.AccessKey != "key" || access.SecretKey != "secret" {
			t.Errorf("credentials = %q/%q, want key/secret carried out of band", access.AccessKey, access.SecretKey)
		}
		// Embedded credentials have always meant a local S3 reached over plain HTTP;
		// stripping them must not silently flip the repository to TLS.
		if !access.Insecure {
			t.Error("Insecure = false; a URI with embedded credentials used to force TLS off")
		}
	})

	t.Run("a loopback host is still rewritten, port and path intact", func(t *testing.T) {
		access := repoAccessFor("s3://key:secret@localhost:9000/bucket/prefix")
		if want := "s3://host.docker.internal:9000/bucket/prefix"; access.Location != want {
			t.Errorf("Location = %q, want %q", access.Location, want)
		}
	})

	t.Run("credentials come from the host environment when the URI carries none", func(t *testing.T) {
		t.Setenv("AWS_ACCESS_KEY_ID", "env-key")
		t.Setenv("AWS_SECRET_ACCESS_KEY", "env-secret")
		access := repoAccessFor("s3://minio.example.com:9000/bucket/prefix")
		if access.AccessKey != "env-key" || access.SecretKey != "env-secret" {
			t.Errorf("credentials = %q/%q, want them read from the host environment", access.AccessKey, access.SecretKey)
		}
		// Read on the host and sent over stdin — never forwarded as -e, which docker
		// inspect would publish, and never through containerReachable, which is a URL
		// rewriter being handed something that is not a URL.
		if access.Insecure {
			t.Error("Insecure = true without embedded credentials; TLS must stay on")
		}
	})

	t.Run("a non-s3 remote scheme is passed through without credential handling", func(t *testing.T) {
		access := repoAccessFor("gs://bucket/prefix")
		if access.Location != "gs://bucket/prefix" {
			t.Errorf("Location = %q, want it passed through unchanged", access.Location)
		}
		if access.AccessKey != "" || access.SecretKey != "" {
			t.Errorf("credentials = %q/%q, want none for a non-s3 scheme", access.AccessKey, access.SecretKey)
		}
	})
}

// The worker is the other end of the credentials channel, so what one encodes the
// other has to decode — exactly, including values a key=value encoding would have
// mangled.
func TestRunnerCredentialsRoundTrip(t *testing.T) {
	creds := runnerCredentials{
		Passphrase:  "correct horse battery staple",
		DBPassword:  "p=ss\nword with = and newline",
		S3AccessKey: "AKIA",
		S3SecretKey: "s3cr3t/with+slashes",
	}
	encoded, err := creds.encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var got runnerCredentials
	if err := json.Unmarshal([]byte(encoded), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got != creds {
		t.Errorf("round trip = %+v, want %+v", got, creds)
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

// The orchestrator stops the `database` service before launching a runner and
// restarts it in a defer, so a launch that never returns leaves Infrahub down.
// These cases pin the timeout that makes that impossible, and the diagnostics a
// failed launch has to surface.
func TestRunCapture(t *testing.T) {
	t.Run("a wedged runner is killed rather than waited on forever", func(t *testing.T) {
		start := time.Now()
		_, err := runCapture(50*time.Millisecond, "sleep", []string{"30"}, "")
		if err == nil {
			t.Fatal("a command exceeding the timeout returned no error")
		}
		if !errors.Is(err, errRunnerTimeout) {
			t.Fatalf("err = %v, want it to wrap errRunnerTimeout — the caller has to tell a timeout from an ordinary failure to know it must also remove the container", err)
		}
		if !strings.Contains(err.Error(), "did not finish within") {
			t.Fatalf("err = %v, want the timeout named in the message", err)
		}
		if !strings.Contains(err.Error(), runnerTimeoutEnvVar) {
			t.Fatalf("err = %v, want the override env var named so an operator can raise it", err)
		}
		if elapsed := time.Since(start); elapsed > 10*time.Second {
			t.Fatalf("waited %v for a 50ms timeout", elapsed)
		}
	})

	t.Run("the snapshot id is the last stdout token", func(t *testing.T) {
		got, err := runCapture(time.Minute, "echo", []string{"noise", "deadbeef"}, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "deadbeef" {
			t.Fatalf("got %q, want %q", got, "deadbeef")
		}
	})

	t.Run("an ordinary failure is not reported as a timeout", func(t *testing.T) {
		_, err := runCapture(time.Minute, "sh", []string{"-c", "exit 1"}, "")
		if err == nil {
			t.Fatal("a non-zero exit returned no error")
		}
		if errors.Is(err, errRunnerTimeout) {
			t.Errorf("err = %v, want it NOT to look like a timeout — the container removal must not fire on ordinary failures", err)
		}
	})

	t.Run("a failed launch surfaces stdout as well as stderr", func(t *testing.T) {
		_, err := runCapture(time.Minute, "sh", []string{"-c", "echo out-detail; echo err-detail >&2; exit 3"}, "")
		if err == nil {
			t.Fatal("a non-zero exit returned no error")
		}
		for _, want := range []string{"out-detail", "err-detail"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("err = %v, want it to contain %q", err, want)
			}
		}
	})

	t.Run("stdin reaches the command", func(t *testing.T) {
		got, err := runCapture(time.Minute, "cat", nil, "a-passphrase")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "a-passphrase" {
			t.Fatalf("got %q, want the piped value back", got)
		}
	})
}

// Each launch needs its own container name: the name is what lets a timed-out
// launch be removed, and a leftover container from an earlier run must not make the
// next launch fail on a clash.
func TestRunnerContainerNameIsUniquePerLaunch(t *testing.T) {
	first := runnerContainerName("database")
	second := runnerContainerName("database")
	if first == second {
		t.Fatalf("two launches got the same container name %q", first)
	}
	for _, name := range []string{first, second} {
		if !strings.HasPrefix(name, "infrahub-backup-runner-database-") {
			t.Errorf("name = %q, want it to identify the tool and the target service", name)
		}
	}
}

// The timeout is configurable because some deployments legitimately take longer
// than 30 minutes to dump or load, but a malformed value must not disable the
// guard — "no timeout" is the failure mode the guard exists for.
func TestRunnerTimeout(t *testing.T) {
	for _, tc := range []struct {
		name, env string
		want      time.Duration
	}{
		{"unset falls back to the default", "", defaultRunnerTimeout},
		{"a valid duration is honoured", "90m", 90 * time.Minute},
		{"an unparseable value falls back", "later", defaultRunnerTimeout},
		{"zero falls back rather than disabling the guard", "0", defaultRunnerTimeout},
		{"a negative value falls back", "-5m", defaultRunnerTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(runnerTimeoutEnvVar, tc.env)
			if got := runnerTimeout(); got != tc.want {
				t.Errorf("runnerTimeout() with %s=%q = %v, want %v", runnerTimeoutEnvVar, tc.env, got, tc.want)
			}
		})
	}
}

// A failed restore needs the ownership fix-up as much as a successful one: by the
// time an export fails, neo4j-admin may already have written into the shared /data
// as real root, and a root-owned store is one Neo4j (uid 7474) will not start on.
// The close has to happen before the chown, because the exporter is what drives
// neo4j-admin. These cases pin both.
func TestExportWithOwnershipRestored(t *testing.T) {
	steps := func() (*[]string, func(string, error) func() error) {
		var calls []string
		return &calls, func(name string, err error) func() error {
			return func() error { calls = append(calls, name); return err }
		}
	}

	t.Run("a failed export still closes and restores ownership", func(t *testing.T) {
		calls, step := steps()
		exportErr := errors.New("neo4j-admin exited 1")
		err := exportWithOwnershipRestored(step("export", exportErr), step("close", nil), step("migrate", nil), step("chown", nil))
		if !errors.Is(err, exportErr) {
			t.Fatalf("err = %v, want the export error", err)
		}
		// The migration is skipped: there is no restored store to migrate.
		if got := strings.Join(*calls, ","); got != "export,close,chown" {
			t.Fatalf("call order = %q, want \"export,close,chown\"", got)
		}
	})

	t.Run("a failing close still restores ownership", func(t *testing.T) {
		calls, step := steps()
		closeErr := errors.New("closing exporter")
		err := exportWithOwnershipRestored(step("export", nil), step("close", closeErr), step("migrate", nil), step("chown", nil))
		if !errors.Is(err, closeErr) {
			t.Fatalf("err = %v, want the close error", err)
		}
		if got := strings.Join(*calls, ","); got != "export,close,chown" {
			t.Fatalf("call order = %q, want \"export,close,chown\"", got)
		}
	})

	t.Run("the restore failure wins over a clean-up failure", func(t *testing.T) {
		_, step := steps()
		exportErr := errors.New("restore failed")
		err := exportWithOwnershipRestored(
			step("export", exportErr),
			step("close", errors.New("close also failed")),
			step("migrate", errors.New("migrate also failed")),
			step("chown", errors.New("chown also failed")),
		)
		if !errors.Is(err, exportErr) {
			t.Fatalf("err = %v, want the export error to win", err)
		}
	})

	t.Run("a requested migration runs after the close and before the chown", func(t *testing.T) {
		calls, step := steps()
		if err := exportWithOwnershipRestored(step("export", nil), step("close", nil), step("migrate", nil), step("chown", nil)); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := strings.Join(*calls, ","); got != "export,close,migrate,chown" {
			t.Fatalf("call order = %q, want \"export,close,migrate,chown\" — a migration must be inside the offline window and behind the chown", got)
		}
	})

	t.Run("a failing migration is reported", func(t *testing.T) {
		_, step := steps()
		migrateErr := errors.New("migrating neo4j to format block")
		err := exportWithOwnershipRestored(step("export", nil), step("close", nil), step("migrate", migrateErr), step("chown", nil))
		if !errors.Is(err, migrateErr) {
			t.Fatalf("err = %v, want the migration error", err)
		}
	})

	t.Run("a chown failure on an otherwise clean restore is reported", func(t *testing.T) {
		_, step := steps()
		chownErr := errors.New("restoring ownership of /data")
		err := exportWithOwnershipRestored(step("export", nil), step("close", nil), step("migrate", nil), step("chown", chownErr))
		if !errors.Is(err, chownErr) {
			t.Fatalf("err = %v, want the chown error", err)
		}
	})
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

	bare := storeConfig(&PlakarConfig{RepoPath: dir})["location"]
	scheme := storeConfig(&PlakarConfig{RepoPath: "fs://" + dir})["location"]

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
		location := storeConfig(&PlakarConfig{RepoPath: repo})["location"]
		path := strings.TrimPrefix(location, "fs://")
		if !filepath.IsAbs(path) {
			t.Errorf("storeConfig(%q) location = %q, want an absolute path", repo, location)
		}
	}
}
