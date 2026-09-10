package app

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PlakarKorp/kloset/objects"
	"github.com/PlakarKorp/kloset/repository"
	"github.com/PlakarKorp/kloset/snapshot"
	"github.com/PlakarKorp/kloset/snapshot/header"
)

// errDumpToolFailed stands in for what a stream factory returns when the
// vendor tool behind it failed: neo4j-admin refused the credentials, pg_dump
// found no listener, the scratch disk was full.
var errDumpToolFailed = errors.New("failed to backup neo4j: exit status 1")

// failingImporter is a component stream whose reader cannot be opened, which
// is what the streaming importers hand kloset when the dump command fails.
func failingImporter(pathname string, cause error) *StreamingImporter {
	fi := objects.NewFileInfo(pathname, 0, 0644, time.Now(), 0, 0, 0, 0, 0)

	return NewStreamingImporter("test-host", pathname, fi, func() (io.ReadCloser, error) {
		return nil, cause
	})
}

// newPlakarOps is a run with a Plakar repository of its own and nothing else:
// the component commit under test asks nothing of a deployment.
func newPlakarOps(t *testing.T) *InfrahubOps {
	t.Helper()

	iops := NewInfrahubOps()
	iops.config.Plakar = &PlakarConfig{RepoPath: filepath.Join(t.TempDir(), "repo"), CacheDir: t.TempDir()}

	return iops
}

// openPlakarRepo opens the run's repository, creating it on first use, and
// hands back the close the caller runs before reading the repository again —
// a committed snapshot is only listable from a fresh open, which is also how
// production reads one.
func openPlakarRepo(t *testing.T, iops *InfrahubOps) (*repository.Repository, func()) {
	t.Helper()

	kctx, err := initPlakarContext(iops.config.Plakar)
	if err != nil {
		t.Fatalf("initPlakarContext() = %v, want nil", err)
	}
	repo, err := openOrCreateRepo(kctx, iops.config.Plakar)
	if err != nil {
		closePlakarContext(kctx)
		t.Fatalf("openOrCreateRepo() = %v, want nil", err)
	}

	return repo, func() {
		closeRepo(repo)
		closePlakarContext(kctx)
	}
}

// committedSnapshotHeaders is every snapshot the repository holds, read from a
// fresh open so that only what was actually committed is counted.
func committedSnapshotHeaders(t *testing.T, iops *InfrahubOps) []*header.Header {
	t.Helper()

	repo, done := openPlakarRepo(t, iops)
	defer done()

	var headers []*header.Header
	for mac, err := range repo.ListSnapshots() {
		if err != nil {
			t.Fatalf("ListSnapshots() = %v, want nil", err)
		}
		snap, err := snapshot.Load(repo, mac)
		if err != nil {
			t.Fatalf("snapshot.Load(%x) = %v, want nil", mac[:8], err)
		}
		hdr := *snap.Header
		snap.Close()
		headers = append(headers, &hdr)
	}

	return headers
}

// TestKlosetCommitsASourceWhoseReaderFailed pins the kloset contract
// commitComponentSnapshot is written against (T149). Both Backup and Commit
// return nil when a record's reader fails or produces nothing: kloset writes a
// failed read into the snapshot as an error entry, counts it in the source's
// summary, and commits the snapshot with no bytes in it. A tool checking only
// their return values therefore cannot tell a failed dump from a successful
// one, which is what CreatePlakarBackup did.
//
// If this test fails because Backup or Commit now return the error, kloset has
// changed its contract: the check in commitComponentSnapshot becomes
// belt-and-braces rather than the only check, and T149's note is to be
// reworded to say so.
func TestKlosetCommitsASourceWhoseReaderFailed(t *testing.T) {
	cases := []struct {
		name       string
		imp        *StreamingImporter
		wantErrors uint64
	}{
		{"a reader that fails to open", failingImporter("/neo4j-backup.tar", errDumpToolFailed), 1},
		{"a reader that produces nothing", NewMemoryImporter("test-host", "/prefect.dump", nil), 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			iops := newPlakarOps(t)
			repo, done := openPlakarRepo(t, iops)

			builder, err := snapshot.Create(repo, repository.DefaultType, t.TempDir(), objects.NilMac, &snapshot.BuilderOptions{Name: "contract"})
			if err != nil {
				done()
				t.Fatalf("snapshot.Create() = %v, want nil", err)
			}
			source, err := snapshot.NewSource(context.Background(), 0, tc.imp)
			if err != nil {
				builder.Close()
				done()
				t.Fatalf("snapshot.NewSource() = %v, want nil", err)
			}

			backupErr := builder.Backup(source)
			commitErr := builder.Commit()
			builder.Close()
			done()

			if backupErr != nil || commitErr != nil {
				t.Fatalf("Backup() = %v, Commit() = %v: kloset now reports the failed reader itself, so the summary check in commitComponentSnapshot is belt-and-braces and T149's note needs rewording", backupErr, commitErr)
			}

			headers := committedSnapshotHeaders(t, iops)
			if len(headers) != 1 {
				t.Fatalf("committed snapshots = %d, want the one kloset committed over a reader that gave it nothing", len(headers))
			}

			verdict := summarizeSnapshotSources(headers[0].Sources)
			if verdict.Bytes != 0 {
				t.Errorf("committed summary bytes = %d, want 0: the reader stored nothing", verdict.Bytes)
			}
			if verdict.Errors != tc.wantErrors {
				t.Errorf("committed summary errors = %d, want %d", verdict.Errors, tc.wantErrors)
			}
			t.Logf("kloset contract for %s: Backup() = nil, Commit() = nil, committed summary bytes=%d errors=%d over %d source(s)", tc.name, verdict.Bytes, verdict.Errors, len(headers[0].Sources))
		})
	}
}

// TestCommitComponentSnapshotRefusesACaptureThatStoredNothing is T149's fix. A
// component whose stream failed or produced nothing is refused before the
// commit, named in the error, recorded against the service it was a capture of
// so the completeness field cannot read true, and never logged as created.
func TestCommitComponentSnapshotRefusesACaptureThatStoredNothing(t *testing.T) {
	cases := []struct {
		name      string
		component string
		imp       *StreamingImporter
		// service is what the refusal is recorded against; "" for the
		// component that is not a capture.
		service string
	}{
		{"neo4j whose neo4j-admin failed", ComponentNeo4j, failingImporter("/neo4j-backup.tar", errDumpToolFailed), serviceNeo4j},
		{"postgres whose pg_dump produced nothing", ComponentPostgres, NewMemoryImporter("test-host", "/prefect.dump", nil), serviceTaskManagerDB},
		{"metadata that could not be read", ComponentMetadata, failingImporter("/backup_information.json", errors.New("marshal failed")), ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			iops := newPlakarOps(t)
			repo, done := openPlakarRepo(t, iops)

			var (
				mac objects.MAC
				err error
			)
			logged := captureLogrus(t, func() {
				mac, err = iops.commitComponentSnapshot(repo, tc.component, tc.imp, &snapshot.BuilderOptions{Name: "t149_" + tc.component})
			})
			done()

			if err == nil {
				t.Fatalf("commitComponentSnapshot() = nil, want the %s snapshot refused: its source stored no bytes (T149)", tc.component)
			}
			if !strings.Contains(err.Error(), tc.component) || !strings.Contains(err.Error(), "will not be kept") {
				t.Errorf("err = %v, want it to name the component and say the snapshot is not kept", err)
			}
			if mac != objects.NilMac {
				t.Errorf("mac = %x, want none: a refused snapshot is not a committed one", mac[:8])
			}
			if strings.Contains(logged, "Component snapshot created") {
				t.Errorf("the refusal was logged as a created snapshot:\n%s", logged)
			}
			if headers := committedSnapshotHeaders(t, iops); len(headers) != 0 {
				t.Errorf("committed snapshots = %d, want none: the refusal comes before the commit, so there is nothing to discard", len(headers))
			}

			if tc.service == "" {
				if !iops.capturesComplete() {
					t.Errorf("incompleteCaptures = %v, want nothing recorded: the %s component is not a capture of a service", iops.incompleteCaptures, tc.component)
				}
				return
			}
			if iops.capturesComplete() {
				t.Errorf("capturesComplete() = true after the %s snapshot was refused, want the verdict recorded so capture_complete cannot read true", tc.component)
			}
			if detail := iops.incompleteCaptures[tc.service]; detail == "" {
				t.Errorf("incompleteCaptures[%q] = %q, want the refusal recorded against the service the capture was of", tc.service, detail)
			}
		})
	}
}

// TestCommitComponentSnapshotCommitsACaptureThatStoredBytes is the other side
// of the refusal: a stream that moved bytes is committed, listable, reported
// as created, and leaves the run's completeness untouched. Without it, a check
// that refused every snapshot would pass the test above.
func TestCommitComponentSnapshotCommitsACaptureThatStoredBytes(t *testing.T) {
	body := []byte("prefect dump bytes")
	iops := newPlakarOps(t)
	repo, done := openPlakarRepo(t, iops)

	var (
		mac objects.MAC
		err error
	)
	logged := captureLogrus(t, func() {
		mac, err = iops.commitComponentSnapshot(repo, ComponentPostgres, NewMemoryImporter("test-host", "/prefect.dump", body), &snapshot.BuilderOptions{Name: "t149_postgres"})
	})
	done()

	if err != nil {
		t.Fatalf("commitComponentSnapshot() = %v, want nil for a stream that stored bytes", err)
	}
	if mac == objects.NilMac {
		t.Fatal("mac = nil, want the committed snapshot's identifier")
	}
	if !strings.Contains(logged, "Component snapshot created") {
		t.Errorf("the committed snapshot was not reported as created:\n%s", logged)
	}
	if !iops.capturesComplete() {
		t.Errorf("incompleteCaptures = %v, want nothing recorded against a capture that completed", iops.incompleteCaptures)
	}

	headers := committedSnapshotHeaders(t, iops)
	if len(headers) != 1 || headers[0].Identifier != mac {
		t.Fatalf("committed snapshots = %d, want exactly the one returned (%x)", len(headers), mac[:8])
	}
	if verdict := summarizeSnapshotSources(headers[0].Sources); verdict.Bytes != uint64(len(body)) || verdict.Errors != 0 {
		t.Errorf("committed summary = %+v, want bytes=%d errors=0", verdict, len(body))
	}
}

// TestASingleSnapshotRestoreReadsTheEditionForTheComponentItRestores is
// T149's secondary finding. buildSnapshotTags stamps the group's Neo4j
// edition on every component's snapshot, and restoreSingleSnapshot resolved
// it for every component, so a Postgres-only restore of an Enterprise group
// was refused on a Community server — for an edition it never touches.
func TestASingleSnapshotRestoreReadsTheEditionForTheComponentItRestores(t *testing.T) {
	t.Run("a postgres snapshot from an enterprise group restores on a community server", func(t *testing.T) {
		// newRestoringBackend answers the edition probe with community. The
		// status query is armed to fail once the deployment is quiesced, which
		// stops the run at the guard — a point after the edition gate, so
		// reaching it is the evidence the gate let a postgres restore through.
		backend := newRestoringBackend()
		backend.statusErrOnceQuiesced = errDeploymentStoppedAnswering
		iops := newRestoringOps(t, backend)
		kctx, repo := newPlakarSnapshot(t, iops,
			[]string{TagComponent + "=" + ComponentPostgres, TagNeo4jEdition + "=" + neo4jEditionEnterprise},
			"/"+prefectDumpFilename, []byte("prefect dump bytes"))

		err := iops.restoreSingleSnapshot(kctx, repo, "", false, false, false)

		if err != nil && strings.Contains(err.Error(), "Enterprise backup on Community") {
			t.Fatalf("err = %v: a postgres restore was refused on the Neo4j edition, which it never touches", err)
		}
		if len(backend.stopped) == 0 {
			t.Fatalf("the run stopped nothing, so it never got past the edition gate; err = %v", err)
		}
		if !errors.Is(err, errDeploymentStoppedAnswering) {
			t.Fatalf("err = %v, want the run stopped by the quiesce guard, which lies past the edition gate", err)
		}
	})

	t.Run("a neo4j snapshot from an enterprise group is still refused on a community server", func(t *testing.T) {
		backend := newRestoringBackend()
		iops := newRestoringOps(t, backend)
		kctx, repo := newPlakarSnapshot(t, iops,
			[]string{TagComponent + "=" + ComponentNeo4j, TagNeo4jEdition + "=" + neo4jEditionEnterprise},
			"/neo4j-backup.tar", enterpriseNeo4jTar(t))

		err := iops.restoreSingleSnapshot(kctx, repo, "", false, false, false)

		if err == nil || !strings.Contains(err.Error(), "cannot restore Enterprise backup on Community edition Neo4j") {
			t.Fatalf("err = %v, want the edition gate's refusal for the component the edition describes", err)
		}
		if len(backend.stopped) != 0 {
			t.Errorf("stopped = %v, want nothing stopped: the gate refuses before the scale-down", backend.stopped)
		}
	})
}
