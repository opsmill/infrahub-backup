package app

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeBackupBackend drives CreateBackup's deployment steps without a deployment.
// Exec answers the queries the create path makes — the edition probe decides which
// Neo4j branch runs — and CopyFrom materializes a dump so the archive, its checksums,
// and the tarball are real files on disk.
type fakeBackupBackend struct {
	bareBackend
	copyFromErr error
}

func newFakeBackupBackend() *fakeBackupBackend {
	return &fakeBackupBackend{bareBackend: bareBackend{name: "docker"}}
}

func (f *fakeBackupBackend) Exec(service string, command []string, opts *ExecOptions) (string, error) {
	if len(command) > 0 && command[0] == "cypher-shell" {
		// Enterprise keeps the run on the online-backup branch, which stops no
		// containers and waits for nothing.
		return "enterprise\n", nil
	}

	return "", nil
}

func (f *fakeBackupBackend) CopyFrom(service, src, dest string) error {
	if f.copyFromErr != nil {
		return f.copyFromErr
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}

	return os.WriteFile(filepath.Join(dest, "neo4j.dump"), []byte("neo4j dump"), 0o600)
}

// fakeS3Endpoint serves the two requests a `create --s3-upload` run makes against S3:
// the upload, and the listing retention performs afterwards. Each can be failed on its
// own, which is how the create path's exit semantics are exercised without a bucket.
type fakeS3Endpoint struct {
	server     *httptest.Server
	uploadCode int
	listCode   int
	uploaded   []string
	deleted    []string
	listed     int
}

func newFakeS3Endpoint(t *testing.T) *fakeS3Endpoint {
	t.Helper()

	// The client resolves credentials from the standard AWS sources; the environment
	// provider answers immediately and keeps the test off the instance-metadata path.
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")

	endpoint := &fakeS3Endpoint{uploadCode: http.StatusOK, listCode: http.StatusOK}
	endpoint.server = httptest.NewServer(http.HandlerFunc(endpoint.handle))
	t.Cleanup(endpoint.server.Close)

	return endpoint
}

func (e *fakeS3Endpoint) handle(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPut:
		if e.uploadCode != http.StatusOK {
			writeS3Error(w, e.uploadCode, "AccessDenied", "upload forbidden")
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		e.uploaded = append(e.uploaded, r.URL.Path)
		w.Header().Set("ETag", `"fake-etag"`)
	case http.MethodDelete:
		e.deleted = append(e.deleted, r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	case http.MethodGet:
		e.listed++
		if e.listCode != http.StatusOK {
			writeS3Error(w, e.listCode, "AccessDenied", "list forbidden")
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>infrahub-backups</Name><IsTruncated>false</IsTruncated></ListBucketResult>`)
	default:
		w.WriteHeader(http.StatusOK)
	}
}

func writeS3Error(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message></Error>`, code, message))
}

// createRetentionFixture is a deployment-free `create` run: a backup directory already
// holding one in-policy and two out-of-policy archives, a fake backend, and a policy
// that claims the two old archives.
type createRetentionFixture struct {
	iops        *InfrahubOps
	backend     *fakeBackupBackend
	dir         string
	inPolicy    string
	outOfPolicy []string
}

func newCreateRetentionFixture(t *testing.T) *createRetentionFixture {
	t.Helper()

	inPolicy := backupNameAt(time.Now().Add(-2 * retentionDay))
	outOfPolicy := []string{
		backupNameAt(time.Now().Add(-20 * retentionDay)),
		backupNameAt(time.Now().Add(-30*retentionDay)) + ".enc",
	}
	dir := seedPruneDir(t, append([]string{inPolicy}, outOfPolicy...)...)

	backend := newFakeBackupBackend()
	cfg := createRetentionConfig(dir, RetentionConfig{Days: 7})

	return &createRetentionFixture{
		iops:        &InfrahubOps{config: cfg, backend: backend, executor: NewCommandExecutor()},
		backend:     backend,
		dir:         dir,
		inPolicy:    inPolicy,
		outOfPolicy: outOfPolicy,
	}
}

// run performs the backup the way `infrahub-backup create` does, with the checks that
// need a live deployment waived, and returns everything that was logged plus the error.
func (f *createRetentionFixture) run(t *testing.T, s3Upload bool) (string, error) {
	t.Helper()

	var err error
	output := captureLogrus(t, func() {
		err = f.iops.CreateBackup(true, "all", true, s3Upload, true, 0, false, false, "")
	})

	return output, err
}

// archive returns the base name of the archive this run wrote, failing the test when
// the run produced anything other than exactly one new archive.
func (f *createRetentionFixture) archive(t *testing.T) string {
	t.Helper()

	var found []string
	for _, name := range pruneDirNames(t, f.dir) {
		if _, ok := parseBackupName(name); !ok {
			continue
		}
		if name == f.inPolicy || slices.Contains(f.outOfPolicy, name) {
			continue
		}
		found = append(found, name)
	}
	if len(found) != 1 {
		t.Fatalf("new archives in %s = %v, want exactly one", f.dir, found)
	}

	return found[0]
}

// TestCreateBackupAppliesRetentionAfterSuccess is the first row of the create exit
// semantics (contracts/cli.md): a backup that fully succeeded prunes the out-of-policy
// archives, keeps everything the policy claims, and exits 0. It also proves the fixture
// really reaches retention, which is what makes the two failure rows below meaningful.
func TestCreateBackupAppliesRetentionAfterSuccess(t *testing.T) {
	fixture := newCreateRetentionFixture(t)

	output, err := fixture.run(t, false)
	if err != nil {
		t.Fatalf("CreateBackup() = %v, want nil", err)
	}

	archive := fixture.archive(t)
	want := append([]string{archive, fixture.inPolicy}, pruneDecoys...)
	slices.Sort(want)
	if got := pruneDirNames(t, fixture.dir); !slices.Equal(got, want) {
		t.Errorf("backup directory after create = %v, want %v", got, want)
	}
	for _, name := range fixture.outOfPolicy {
		if !strings.Contains(output, "Pruned backup "+name) {
			t.Errorf("log output = %q, want the deletion of %s reported", output, name)
		}
	}
}

// TestCreateBackupFailurePrunesNothing is the "Backup failed" row: no pruning is
// attempted, so a run that could not produce an archive cannot delete the archives that
// would have been the only remaining recovery points.
func TestCreateBackupFailurePrunesNothing(t *testing.T) {
	t.Run("a failed database backup prunes nothing", func(t *testing.T) {
		fixture := newCreateRetentionFixture(t)
		fixture.backend.copyFromErr = errors.New("no space left on device")
		before := pruneDirNames(t, fixture.dir)

		output, err := fixture.run(t, false)
		if err == nil {
			t.Fatal("CreateBackup() = nil, want the failed database copy reported")
		}
		if strings.Contains(err.Error(), "retention") {
			t.Errorf("error = %q, want a backup failure rather than a retention failure", err)
		}
		if got := pruneDirNames(t, fixture.dir); !slices.Equal(got, before) {
			t.Errorf("backup directory after a failed backup = %v, want it untouched %v", got, before)
		}
		if strings.Contains(output, "Applying retention policy") {
			t.Errorf("log output = %q, want no retention pass after a failed backup", output)
		}
	})

	// The S3 upload is the last step before retention, so failing it pins the ordering
	// that keeps a failed backup from pruning: retention runs after the archive is
	// written, checksummed, and — when asked for — uploaded.
	t.Run("a failed S3 upload prunes nothing", func(t *testing.T) {
		fixture := newCreateRetentionFixture(t)
		endpoint := newFakeS3Endpoint(t)
		endpoint.uploadCode = http.StatusForbidden
		fixture.iops.config.S3.Endpoint = endpoint.server.URL
		before := pruneDirNames(t, fixture.dir)

		output, err := fixture.run(t, true)
		if err == nil {
			t.Fatal("CreateBackup() = nil, want the failed upload reported")
		}
		if !strings.Contains(err.Error(), "S3 upload failed") {
			t.Errorf("error = %q, want it to name the failed upload", err)
		}

		// The archive itself survives a failed upload, so only the new one is extra.
		archive := fixture.archive(t)
		want := append([]string{archive}, before...)
		slices.Sort(want)
		if got := pruneDirNames(t, fixture.dir); !slices.Equal(got, want) {
			t.Errorf("backup directory after a failed upload = %v, want nothing pruned %v", got, want)
		}
		if strings.Contains(output, "Applying retention policy") {
			t.Errorf("log output = %q, want no retention pass after a failed upload", output)
		}
		if endpoint.listed != 0 {
			t.Errorf("S3 listings = %d, want none: retention must not run after a failed upload", endpoint.listed)
		}
	})
}

// TestCreateBackupReportsRetentionFailureAfterSuccess is the "Backup ok, prune leg
// failed" row: the message states the backup succeeded and names the archive before it
// reports the retention failure, and the run exits non-zero.
func TestCreateBackupReportsRetentionFailureAfterSuccess(t *testing.T) {
	fixture := newCreateRetentionFixture(t)
	endpoint := newFakeS3Endpoint(t)
	endpoint.listCode = http.StatusForbidden
	fixture.iops.config.S3.Endpoint = endpoint.server.URL

	_, err := fixture.run(t, true)
	if err == nil {
		t.Fatal("CreateBackup() = nil, want the failing retention leg reported")
	}

	archive := fixture.archive(t)
	wantPrefix := fmt.Sprintf("backup succeeded (%s); retention failed:", archive)
	if !strings.HasPrefix(err.Error(), wantPrefix) {
		t.Errorf("error = %q, want it to begin with %q", err, wantPrefix)
	}
	if !strings.Contains(err.Error(), "retention failed at s3://infrahub-backups/prod") {
		t.Errorf("error = %q, want it to name the failing leg", err)
	}
	if len(endpoint.uploaded) != 1 {
		t.Fatalf("uploads = %v, want exactly the new archive", endpoint.uploaded)
	}

	// The local leg still ran: only the S3 leg failed.
	want := append([]string{archive, fixture.inPolicy}, pruneDecoys...)
	slices.Sort(want)
	if got := pruneDirNames(t, fixture.dir); !slices.Equal(got, want) {
		t.Errorf("backup directory = %v, want the local leg pruned %v", got, want)
	}
}

// ---------------------------------------------------------------------------
// T045: an incomplete capture is not retained anywhere (FR-012)
// ---------------------------------------------------------------------------

// retentionDecision is what the policy would do at one location, as the two name
// sets it splits a listing into. Comparing decisions rather than directories is
// what FR-012's verification asks for: the requirement is that an incomplete run
// leaves retention deciding exactly what it would have decided had the run
// failed before writing anything.
type retentionDecision struct {
	keep  []string
	prune []string
}

func decideRetention(t *testing.T, dir string, policy RetentionPolicy, now time.Time) retentionDecision {
	t.Helper()

	refs, err := newLocalLocation(dir).List(t.Context())
	if err != nil {
		t.Fatalf("listing %s = %v, want nil", dir, err)
	}

	keep, prune := selectPrunable(refs, policy, now)

	decision := retentionDecision{keep: make([]string, 0, len(keep)), prune: make([]string, 0, len(prune))}
	for _, ref := range keep {
		decision.keep = append(decision.keep, ref.Name)
	}
	for _, ref := range prune {
		decision.prune = append(decision.prune, ref.Name)
	}
	slices.Sort(decision.keep)
	slices.Sort(decision.prune)

	return decision
}

// TestIncompleteCaptureIsRetainedNowhere is FR-012's verification: fail one
// component, and the run fails leaving no artefact at any location and leaving
// retention's decisions identical to a run that failed before anything was
// written.
//
// The two failures are deliberately different in kind. The copy failure never
// produces an archive; the incomplete capture is the one that could — it is the
// verdict a backup command reaches after writing something, on the exit status
// it shares with an outright failure — and the point of the requirement is that
// the two are indistinguishable from retention's side afterwards.
func TestIncompleteCaptureIsRetainedNowhere(t *testing.T) {
	now := time.Now()

	// The run that failed before writing anything: the reference decision.
	failedEarly := newCreateRetentionFixture(t)
	failedEarly.backend.copyFromErr = errors.New("no space left on device")
	if _, err := failedEarly.run(t, false); err == nil {
		t.Fatal("CreateBackup() = nil, want the failed database copy reported")
	}
	reference := decideRetention(t, failedEarly.dir, failedEarly.iops.config.Retention.Policy(), now)

	// The run whose capture reported something short of a complete capture.
	// runExternalNeo4jCapture records exactly this before it returns its error;
	// recording it here stands in for a cluster that could not be reached.
	incomplete := newCreateRetentionFixture(t)
	before := pruneDirNames(t, incomplete.dir)
	incomplete.iops.recordIncompleteCapture(serviceNeo4j,
		"the operation produced an artifact and then reported a problem")

	output, err := incomplete.run(t, false)
	if err == nil {
		t.Fatal("CreateBackup() = nil, want an incomplete capture to fail the run")
	}
	if !strings.Contains(err.Error(), "incomplete") {
		t.Errorf("error = %q, want it to say the backup is incomplete", err)
	}

	t.Run("no artefact is left behind", func(t *testing.T) {
		if got := pruneDirNames(t, incomplete.dir); !slices.Equal(got, before) {
			t.Errorf("backup directory after an incomplete capture = %v, want it untouched %v", got, before)
		}
	})

	t.Run("retention never runs", func(t *testing.T) {
		// An incomplete run must not prune. Retention ranks by recency, so an
		// archive written and then removed would still have been the newest
		// while the prune ran and would have evicted the real newest.
		if strings.Contains(output, "Applying retention policy") {
			t.Errorf("log output = %q, want no retention pass after an incomplete capture", output)
		}
	})

	t.Run("retention decides exactly what it would have decided", func(t *testing.T) {
		got := decideRetention(t, incomplete.dir, incomplete.iops.config.Retention.Policy(), now)
		if !slices.Equal(got.keep, reference.keep) || !slices.Equal(got.prune, reference.prune) {
			t.Errorf("retention after an incomplete capture keeps %v and prunes %v;\nafter a run that failed before writing anything it keeps %v and prunes %v",
				got.keep, got.prune, reference.keep, reference.prune)
		}
	})
}

// TestIncompleteCaptureIsRemovedFromEveryLocationItReached covers the half the
// run above cannot reach: an artefact that had already been written locally and
// uploaded before the capture was judged incomplete.
//
// The ordinary path fails at the gate, before an archive exists, so nothing is
// there to remove — which is why the removal is written as an invariant over
// wherever the archive got to rather than as a step of one code path. This is
// what exercises it, and it is the S3 leg that matters: an object left in the
// bucket is one a later `restore --latest --s3` can select, and neither
// retention nor a released reader would know not to.
func TestIncompleteCaptureIsRemovedFromEveryLocationItReached(t *testing.T) {
	endpoint := newFakeS3Endpoint(t)

	archive := backupNameAt(time.Now())
	encrypted := archive + ".enc"
	dir := seedPruneDir(t, archive, encrypted)

	cfg := createRetentionConfig(dir, RetentionConfig{Days: 7})
	cfg.S3.Endpoint = endpoint.server.URL
	iops := &InfrahubOps{config: cfg, executor: NewCommandExecutor()}
	iops.recordIncompleteCapture(serviceNeo4j, "some servers were uncontactable")

	// The archive reached the backup directory under both names — encrypting
	// renames it, and removing the plaintext afterwards is only a warning — and
	// the encrypted one reached the bucket.
	reach := artifactReach{s3Name: encrypted}
	reach.reachedLocally(archive)
	reach.reachedLocally(encrypted)

	err := iops.discardIncompleteCapture(reach)
	if err == nil {
		t.Fatal("discardIncompleteCapture() = nil, want the incomplete backup reported as a failure")
	}
	if !strings.Contains(err.Error(), "some servers were uncontactable") {
		t.Errorf("error = %q, want it to carry the capture's own account", err)
	}
	if strings.Contains(err.Error(), "may still be present") {
		t.Errorf("error = %q, want no leftover reported: every location was cleared", err)
	}

	t.Run("both local names are gone", func(t *testing.T) {
		want := slices.Clone(pruneDecoys)
		slices.Sort(want)
		if got := pruneDirNames(t, dir); !slices.Equal(got, want) {
			t.Errorf("backup directory = %v, want only the non-archive decoys %v", got, want)
		}
	})

	t.Run("the object is deleted under the key the upload built", func(t *testing.T) {
		want := []string{"/infrahub-backups/prod/" + encrypted}
		if !slices.Equal(endpoint.deleted, want) {
			t.Errorf("S3 deletions = %v, want %v: a base name where the full key belongs succeeds silently against real S3",
				endpoint.deleted, want)
		}
	})

	t.Run("a location the archive never reached is left alone", func(t *testing.T) {
		// Configuration alone never authorises a deletion, which is the rule
		// retentionLegsForCreate follows for pruning and the reason an S3
		// bucket that this run never uploaded to is not touched.
		local := newFakeS3Endpoint(t)
		localOnly := createRetentionConfig(seedPruneDir(t, archive), RetentionConfig{Days: 7})
		localOnly.S3.Endpoint = local.server.URL

		iops := &InfrahubOps{config: localOnly, executor: NewCommandExecutor()}
		iops.recordIncompleteCapture(serviceNeo4j, "some servers were uncontactable")

		notUploaded := artifactReach{}
		notUploaded.reachedLocally(archive)
		if err := iops.discardIncompleteCapture(notUploaded); err == nil {
			t.Fatal("discardIncompleteCapture() = nil, want the failure reported")
		}
		if len(local.deleted) != 0 {
			t.Errorf("S3 deletions = %v, want none: this run never uploaded there", local.deleted)
		}
	})
}
