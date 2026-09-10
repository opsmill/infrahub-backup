package app

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/PlakarKorp/kloset/kcontext"
	"github.com/PlakarKorp/kloset/objects"
	"github.com/PlakarKorp/kloset/repository"
	"github.com/PlakarKorp/kloset/snapshot"
)

// restoringBackend is a deployment a restore can be driven all the way through,
// rather than one that answers a single helper's question.
//
// That distinction is the whole point of the tests below. FR-013's restart and
// FR-026's quiesce guard both had a good unit test of the helper — one that
// called returnAppContainersToScale or confirmAppContainersQuiesced with a
// hand-built list of stopped services — and both were invisible to mutation:
// every `defer` calling the restart, and every call to the guard, could be
// deleted from all four restore paths with the suite still green, because no
// test ever drove a restore far enough to reach them. A test that is handed the
// `stopped` slice cannot observe whether the production path builds one.
//
// So this backend answers what a whole restore asks of a deployment: the
// edition probe, the transient-data wipe, the scale-down, the status polls, the
// copies into containers, the scale-up. What it records is what a restore did
// to the deployment, which is what the guarantees are about.
type restoringBackend struct {
	*quiescingBackend

	// execOut answers a command by the service it runs in and its first word,
	// keyed "service/argv0". Anything unlisted answers empty, which is what an
	// exec that succeeded and printed nothing looks like.
	execOut map[string]string

	// execErr fails a command by the same key. It is how a restore is made to
	// fail at a chosen step *after* the deployment has been quiesced, which is
	// the only state from which FR-013's restart is observable.
	execErr map[string]error

	// statusErrOnceQuiesced makes the status query fail across exactly the
	// window the quiesce confirmation runs in: from the moment every service
	// has been asked to stop until the first one is started again. Before that
	// window it answers, so the scale-down itself succeeds and the run reaches
	// the guard with a real `stopped` list; after it, FR-013's restart can
	// still report whether the deployment came back.
	//
	// It is this arm of FR-026 rather than the "pod will not terminate" arm
	// because only this one refuses at once: the other is bounded by
	// appQuiesceTimeout, five minutes, and RestoreBackup does not take a window
	// (confirmAppContainersQuiescedWithin does, and its own test drives that
	// arm). Both are the same guard's refusal.
	statusErrOnceQuiesced error

	// execs, copies and writes are what the restore did to the deployment, in
	// order. copies and writes are the destructive steps: nothing on a restore
	// copies into a database container or streams a dump into one until the
	// deployment is meant to be quiesced.
	execs  []string
	copies []string
	writes []string
}

func newRestoringBackend() *restoringBackend {
	return &restoringBackend{
		quiescingBackend: newQuiescingBackend(),
		execOut: map[string]string{
			// The edition probe, which runs before anything is stopped. An
			// unanswered probe stops the run in requireDeterminedEdition, so
			// the deployment has to have an edition for the restore to get as
			// far as the sequence under test.
			"database/cypher-shell": "community\n",
		},
		execErr: map[string]error{},
	}
}

func (b *restoringBackend) key(service string, command []string) string {
	if len(command) == 0 {
		return service + "/"
	}

	return service + "/" + command[0]
}

func (b *restoringBackend) Exec(service string, command []string, opts *ExecOptions) (string, error) {
	key := b.key(service, command)
	b.execs = append(b.execs, key)

	if err, ok := b.execErr[key]; ok {
		return "", err
	}

	return b.execOut[key], nil
}

func (b *restoringBackend) ExecStream(service string, command []string, opts *ExecOptions) (string, error) {
	return b.Exec(service, command, opts)
}

func (b *restoringBackend) ExecWritePipe(service string, command []string, opts *ExecOptions, stdin io.Reader) (func() error, error) {
	b.writes = append(b.writes, b.key(service, command))

	return func() error { return nil }, nil
}

func (b *restoringBackend) CopyTo(service, src, dest string) error {
	b.copies = append(b.copies, service+":"+dest)

	return nil
}

func (b *restoringBackend) IsRunning(service string) (bool, error) {
	// Every service asked to stop is the point at which the scale-down is over
	// and the confirmation begins; the first service started again is the point
	// at which the confirmation is over either way. So a deployment that stops
	// answering in between answered every question the scale-down needed and
	// none of the ones the guard needs — an API server that was briefly
	// unreachable, which is the uncertainty FR-026 is written against.
	if b.statusErrOnceQuiesced != nil && len(b.stopped) == len(appServicesStoppedForBackup) && len(b.started) == 0 {
		return false, b.statusErrOnceQuiesced
	}

	return b.quiescingBackend.IsRunning(service)
}

var _ EnvironmentBackend = (*restoringBackend)(nil)

// newRestoringOps is a restore against the supplied deployment, with a backup
// directory and a Plakar repository of its own.
func newRestoringOps(t *testing.T, backend *restoringBackend) *InfrahubOps {
	t.Helper()

	iops := NewInfrahubOps()
	iops.backend = backend
	iops.config.BackupDir = t.TempDir()
	iops.config.Plakar = &PlakarConfig{RepoPath: filepath.Join(t.TempDir(), "repo"), CacheDir: t.TempDir()}

	return iops
}

// writeRestoreArchive writes a real backup archive into the run's pool: a
// gzipped tarball holding backup_information.json and a Neo4j dump, with the
// checksums the restore validates computed from the files actually written. A
// restore of it gets past extraction, metadata parsing, the completeness gate,
// the edition resolution and the checksums — which is what it takes to reach
// the scale-down.
func writeRestoreArchive(t *testing.T, iops *InfrahubOps) string {
	t.Helper()

	staging := t.TempDir()
	backupDir := filepath.Join(staging, "backup")
	databaseDir := filepath.Join(backupDir, neo4jBackupDirName)
	if err := os.MkdirAll(databaseDir, 0o755); err != nil {
		t.Fatalf("preparing the archive contents = %v, want nil", err)
	}
	if err := os.WriteFile(filepath.Join(databaseDir, "neo4j.dump"), []byte("neo4j dump bytes"), 0o600); err != nil {
		t.Fatalf("writing the dump = %v, want nil", err)
	}

	// Computed from the files on disk, so the archive is one the restore's own
	// validation accepts rather than one it is told to accept.
	checksums, err := calculateBackupChecksums(backupDir, true)
	if err != nil {
		t.Fatalf("calculateBackupChecksums() = %v, want nil", err)
	}

	metadata := BackupMetadata{
		MetadataVersion: 1,
		BackupID:        "restore-wiring",
		CreatedAt:       retentionNow.Format(backupNameTimestampLayout),
		ToolVersion:     "test",
		InfrahubVersion: "1.0.0",
		Components:      []string{"database"},
		Checksums:       checksums,
		Neo4jEdition:    neo4jEditionCommunity,
	}
	metadataBytes, err := json.MarshalIndent(metadata, "", "    ")
	if err != nil {
		t.Fatalf("marshalling the metadata = %v, want nil", err)
	}
	if err := os.WriteFile(filepath.Join(backupDir, backupMetadataFilename), metadataBytes, 0o600); err != nil {
		t.Fatalf("writing the metadata = %v, want nil", err)
	}

	path := filepath.Join(iops.config.BackupDir, backupNameAt(retentionNow))
	if err := createTarball(path, staging, "backup/"); err != nil {
		t.Fatalf("createTarball() = %v, want nil", err)
	}

	return path
}

// newPlakarSnapshot creates a real single-snapshot Plakar repository in the
// run's own repository path, carrying the supplied tags and one file. It is
// what lets restoreSingleSnapshot be driven: that path reads the component and
// the edition off the snapshot's own tags and streams the dump out of it, so
// there is no seam short of a repository.
func newPlakarSnapshot(t *testing.T, iops *InfrahubOps, tags []string, pathname string, body []byte) (*kcontext.KContext, *repository.Repository) {
	t.Helper()

	writeCtx, err := initPlakarContext(iops.config.Plakar)
	if err != nil {
		t.Fatalf("initPlakarContext() = %v, want nil", err)
	}
	writeRepo, err := openOrCreateRepo(writeCtx, iops.config.Plakar)
	if err != nil {
		closePlakarContext(writeCtx)
		t.Fatalf("openOrCreateRepo() = %v, want nil", err)
	}

	builder, err := snapshot.Create(writeRepo, repository.DefaultType, t.TempDir(), objects.NilMac, &snapshot.BuilderOptions{
		Name: "restore-wiring",
		Tags: tags,
	})
	if err != nil {
		t.Fatalf("snapshot.Create() = %v, want nil", err)
	}
	source, err := snapshot.NewSource(context.Background(), 0, NewMemoryImporter("test-host", pathname, body))
	if err != nil {
		builder.Close()
		t.Fatalf("snapshot.NewSource() = %v, want nil", err)
	}
	if err := builder.Backup(source); err != nil {
		builder.Close()
		t.Fatalf("builder.Backup() = %v, want nil", err)
	}
	if err := builder.Commit(); err != nil {
		builder.Close()
		t.Fatalf("builder.Commit() = %v, want nil", err)
	}
	builder.Close()
	closeRepo(writeRepo)
	closePlakarContext(writeCtx)

	// Re-opened rather than handed back, because that is the only way the
	// committed snapshot is listable — which is also what production does: a
	// `create` run commits and exits, and a `restore` run opens the repository
	// afresh.
	kctx, err := initPlakarContext(iops.config.Plakar)
	if err != nil {
		t.Fatalf("initPlakarContext() = %v, want nil", err)
	}
	t.Cleanup(func() { closePlakarContext(kctx) })

	repo, err := openRepo(kctx, iops.config.Plakar)
	if err != nil {
		t.Fatalf("openRepo() = %v, want nil", err)
	}
	t.Cleanup(func() { closeRepo(repo) })

	return kctx, repo
}

// enterpriseNeo4jTar is the payload an enterprise Neo4j snapshot carries: an
// uncompressed tar whose entries sit under an `infrahubops/` prefix, which is
// what extractNeo4jEnterpriseTar strips. Without a tar this shape the
// enterprise branch of restoreSingleSnapshot fails at the extraction, upstream
// of the sequence under test.
func enterpriseNeo4jTar(t *testing.T) []byte {
	t.Helper()

	var buf bytes.Buffer
	writer := tar.NewWriter(&buf)
	body := []byte("neo4j enterprise backup bytes")
	if err := writer.WriteHeader(&tar.Header{
		Name: "infrahubops/neo4j.backup",
		Mode: 0o644,
		Size: int64(len(body)),
	}); err != nil {
		t.Fatalf("writing the tar header = %v, want nil", err)
	}
	if _, err := writer.Write(body); err != nil {
		t.Fatalf("writing the tar body = %v, want nil", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("closing the tar = %v, want nil", err)
	}

	return buf.Bytes()
}

// emptyBackupGroup is a group whose components are all absent. It is the one
// shape of group that reaches the scale-down without a snapshot having to be
// exported, which is what makes restoreBackupGroup's own quiesce sequence
// drivable: every step between the group being read and the deployment being
// stopped is reached, and nothing between them needs a component.
func emptyBackupGroup() *BackupGroupInfo {
	return &BackupGroupInfo{
		BackupID:        "restore-wiring",
		Status:          StatusComplete,
		InfrahubVersion: "1.0.0",
		Neo4jEdition:    neo4jEditionCommunity,
		Components:      []string{},
	}
}

// assertReturnedToScale is FR-013: whatever the run took down is back up, and
// every service it stopped reports itself running again.
func assertReturnedToScale(t *testing.T, backend *restoringBackend) {
	t.Helper()

	if len(backend.stopped) == 0 {
		t.Fatal("the run stopped nothing, so it never reached the region the restart guards; the test is not exercising FR-013")
	}

	stopped := slices.Clone(backend.stopped)
	started := slices.Clone(backend.started)
	slices.Sort(stopped)
	slices.Sort(started)
	if !slices.Equal(stopped, started) {
		t.Errorf("started = %v, want every service the run stopped (%v): a restore that failed after quiescing leaves Infrahub scaled to zero, which is the outage FR-013 exists to prevent", started, stopped)
	}

	for _, service := range backend.stopped {
		running, err := backend.IsRunning(service)
		if err != nil {
			t.Errorf("IsRunning(%q) = %v, want the service back up", service, err)

			continue
		}
		if !running {
			t.Errorf("%s is still stopped, want it returned to the scale the run found it at", service)
		}
	}
}

// assertNothingWritten is FR-026: the destructive steps did not run. Nothing on
// a restore copies into a database container or streams a dump into one until
// the deployment is meant to be quiesced, so either of those having happened is
// a restore that overwrote a database with Infrahub still up.
func assertNothingWritten(t *testing.T, backend *restoringBackend) {
	t.Helper()

	if len(backend.copies) > 0 {
		t.Errorf("copies into containers = %v, want none: the restore wrote to a database while the deployment could not be confirmed quiesced (FR-026)", backend.copies)
	}
	if len(backend.writes) > 0 {
		t.Errorf("dump streams into containers = %v, want none: the restore wrote to a database while the deployment could not be confirmed quiesced (FR-026)", backend.writes)
	}
}

// errDeploymentStoppedAnswering is the status query failing mid-run: the
// deployment answered while the run scaled it down and stopped answering when
// the run asked whether the scale-down had taken effect.
var errDeploymentStoppedAnswering = errors.New(`pods "infrahub-server-0" not found: the API server did not answer`)

// TestARestoreThatFailsAfterQuiescingReturnsTheDeploymentToScale is FR-013
// driven through the restore paths themselves rather than through
// returnAppContainersToScale.
//
// Every one of these runs quiesces the deployment and then fails at its next
// step, which is the only state the deferred restart is reachable from. Before
// this test all three `defer` blocks could be deleted with the suite green: the
// helper's own test hands it a `stopped` slice, and every restore-level test
// asserted an early refusal, so no test ever reached the deferred region.
func TestARestoreThatFailsAfterQuiescingReturnsTheDeploymentToScale(t *testing.T) {
	t.Run("restore", func(t *testing.T) {
		backend := newRestoringBackend()
		iops := newRestoringOps(t, backend)
		archive := writeRestoreArchive(t, iops)

		// The first step of restoreNeo4j, and the first thing that happens
		// after the deployment is confirmed quiesced: it clears the temporary
		// directory it is about to copy the dump into.
		backend.execErr["database/rm"] = errors.New("device or resource busy")

		err := iops.RestoreBackup(archive, false, false, 0, "", false, false)
		if err == nil {
			t.Fatal("RestoreBackup() = nil, want the run to have failed after quiescing")
		}
		if !strings.Contains(err.Error(), "temporary Neo4j restore directory") {
			t.Fatalf("RestoreBackup() = %v, want the failure to be the one after the quiesce; the run did not get that far", err)
		}

		assertReturnedToScale(t, backend)
	})

	t.Run("restore --latest", func(t *testing.T) {
		backend := newRestoringBackend()
		iops := newRestoringOps(t, backend)
		writeRestoreArchive(t, iops)

		backend.execErr["database/rm"] = errors.New("device or resource busy")

		err := iops.RestoreLatestBackup(false, false, false, 0, "", false, false)
		if err == nil {
			t.Fatal("RestoreLatestBackup() = nil, want the run to have failed after quiescing")
		}
		if !strings.Contains(err.Error(), "temporary Neo4j restore directory") {
			t.Fatalf("RestoreLatestBackup() = %v, want the failure to be the one after the quiesce", err)
		}

		assertReturnedToScale(t, backend)
	})

	t.Run("plakar backup group", func(t *testing.T) {
		backend := newRestoringBackend()
		iops := newRestoringOps(t, backend)

		// cache is restarted immediately after the quiesce, before anything is
		// restored, and its failure is the group path's first failure past the
		// guard.
		backend.startErr["cache"] = errors.New("admission webhook denied the request")

		err := iops.restoreBackupGroup(nil, nil, emptyBackupGroup(), false, false, false)
		if err == nil {
			t.Fatal("restoreBackupGroup() = nil, want the run to have failed after quiescing")
		}
		if !strings.Contains(err.Error(), "failed to restart cache and message-queue") {
			t.Fatalf("restoreBackupGroup() = %v, want the failure to be the one after the quiesce", err)
		}

		// The restart is still attempted for every service, and the one that
		// would not start is the one the deployment refused — so what is
		// asserted is the attempt, not the outcome, which is all FR-013 can
		// promise when a workload will not come up.
		stopped := slices.Clone(backend.stopped)
		started := slices.Clone(backend.started)
		slices.Sort(stopped)
		slices.Sort(started)
		if !slices.Equal(stopped, started) {
			t.Errorf("started = %v, want every service the run stopped (%v)", started, stopped)
		}
	})

	t.Run("plakar single snapshot", func(t *testing.T) {
		backend := newRestoringBackend()
		iops := newRestoringOps(t, backend)
		kctx, repo := newPlakarSnapshot(t, iops,
			[]string{TagComponent + "=" + ComponentNeo4j, TagNeo4jEdition + "=" + neo4jEditionCommunity},
			"/neo4j.dump", []byte("neo4j dump bytes"))

		// The streamed community load's first step after the quiesce: it reads
		// the running server's PID out of the container before suspending it.
		backend.execErr["database/cat"] = errors.New("no such file or directory")

		err := iops.restoreSingleSnapshot(kctx, repo, "", false, false, false)
		if err == nil {
			t.Fatal("restoreSingleSnapshot() = nil, want the run to have failed after quiescing")
		}
		if !strings.Contains(err.Error(), "failed to read neo4j pid file") {
			t.Fatalf("restoreSingleSnapshot() = %v, want the failure to be the one after the quiesce", err)
		}

		assertReturnedToScale(t, backend)
	})
}

// TestARestoreWillNotWriteToADatabaseItCannotConfirmIsQuiesced is FR-026 driven
// through the restore paths themselves rather than through
// confirmAppContainersQuiesced.
//
// The deployment below answers every status query the scale-down makes and then
// stops answering, which is the uncertainty the guard exists for: asked for is
// not stopped, and the next step overwrites a database. Before this test the
// guard could be removed from RestoreBackup and from all four plakar_restore.go
// sites with the suite green.
func TestARestoreWillNotWriteToADatabaseItCannotConfirmIsQuiesced(t *testing.T) {
	assertRefusedAfterStopping := func(t *testing.T, backend *restoringBackend, err error) {
		t.Helper()

		if len(backend.stopped) == 0 {
			t.Fatal("the run stopped nothing, so it never reached the guard; the test is not exercising FR-026")
		}

		// Asserted before the error, so that a build with the guard removed
		// reports what it actually did to the databases rather than only that
		// it failed somewhere else afterwards.
		assertNothingWritten(t, backend)

		if err == nil {
			t.Fatal("the restore = nil, want it refused: the deployment could not be confirmed quiesced")
		}
		if !errors.Is(err, errDeploymentStoppedAnswering) {
			t.Fatalf("err = %v, want the run stopped by the status query it could not get an answer to (FR-026)", err)
		}
		if !strings.Contains(err.Error(), "will not write data as if it were") {
			t.Errorf("err = %v, want the guard's own refusal", err)
		}

		// FR-013 holds on this exit path too: the guard's refusal is a failure
		// after the quiesce, so the deployment goes back up.
		assertReturnedToScale(t, backend)
	}

	t.Run("restore", func(t *testing.T) {
		backend := newRestoringBackend()
		backend.statusErrOnceQuiesced = errDeploymentStoppedAnswering
		iops := newRestoringOps(t, backend)
		archive := writeRestoreArchive(t, iops)

		assertRefusedAfterStopping(t, backend, iops.RestoreBackup(archive, false, false, 0, "", false, false))
	})

	t.Run("restore --latest", func(t *testing.T) {
		backend := newRestoringBackend()
		backend.statusErrOnceQuiesced = errDeploymentStoppedAnswering
		iops := newRestoringOps(t, backend)
		writeRestoreArchive(t, iops)

		assertRefusedAfterStopping(t, backend, iops.RestoreLatestBackup(false, false, false, 0, "", false, false))
	})

	t.Run("plakar backup group", func(t *testing.T) {
		backend := newRestoringBackend()
		backend.statusErrOnceQuiesced = errDeploymentStoppedAnswering
		iops := newRestoringOps(t, backend)

		assertRefusedAfterStopping(t, backend, iops.restoreBackupGroup(nil, nil, emptyBackupGroup(), false, false, false))
	})

	t.Run("plakar single snapshot streaming a community dump", func(t *testing.T) {
		backend := newRestoringBackend()
		backend.statusErrOnceQuiesced = errDeploymentStoppedAnswering
		iops := newRestoringOps(t, backend)
		kctx, repo := newPlakarSnapshot(t, iops,
			[]string{TagComponent + "=" + ComponentNeo4j, TagNeo4jEdition + "=" + neo4jEditionCommunity},
			"/neo4j.dump", []byte("neo4j dump bytes"))

		assertRefusedAfterStopping(t, backend, iops.restoreSingleSnapshot(kctx, repo, "", false, false, false))
	})

	t.Run("plakar single snapshot loading an enterprise backup", func(t *testing.T) {
		backend := newRestoringBackend()
		backend.statusErrOnceQuiesced = errDeploymentStoppedAnswering
		// The enterprise branch is only reachable on a server that reports
		// itself enterprise: ResolveRestoreEdition refuses an enterprise
		// artefact against a Community server before anything is stopped.
		backend.execOut["database/cypher-shell"] = "enterprise\n"
		iops := newRestoringOps(t, backend)
		kctx, repo := newPlakarSnapshot(t, iops,
			[]string{TagComponent + "=" + ComponentNeo4j, TagNeo4jEdition + "=" + neo4jEditionEnterprise},
			"/neo4j-backup.tar", enterpriseNeo4jTar(t))

		assertRefusedAfterStopping(t, backend, iops.restoreSingleSnapshot(kctx, repo, "", false, false, false))
	})

	t.Run("plakar single snapshot restoring postgres", func(t *testing.T) {
		backend := newRestoringBackend()
		backend.statusErrOnceQuiesced = errDeploymentStoppedAnswering
		// The write this branch would perform, armed to fail. The guard means it
		// is never attempted, so this changes nothing about the run under test —
		// what it changes is a build whose guard has been removed: that run
		// reports the write it tried rather than carrying on to a restart that
		// waits out its whole window for services it never brought back.
		backend.execErr["task-manager-db/pg_restore"] = errors.New("could not connect to server")
		iops := newRestoringOps(t, backend)
		kctx, repo := newPlakarSnapshot(t, iops,
			[]string{TagComponent + "=" + ComponentPostgres},
			"/"+prefectDumpFilename, []byte("prefect dump bytes"))

		assertRefusedAfterStopping(t, backend, iops.restoreSingleSnapshot(kctx, repo, "", false, false, false))
	})
}
