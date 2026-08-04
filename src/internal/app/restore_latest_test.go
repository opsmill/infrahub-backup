package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestResolveLatestBackup covers FR-005 and FR-008: the archive `--latest` picks is the
// one retention ranks first at that location, and a pool with nothing in it is a
// failure rather than a quiet no-op.
func TestResolveLatestBackup(t *testing.T) {
	sameTimestamp := retentionNow.Add(-2 * retentionDay)
	plain := backupRef{Name: backupNameAt(sameTimestamp)}
	encrypted := backupRef{Name: backupNameAt(sameTimestamp) + ".enc"}
	newest := refAgo(1 * retentionDay)
	oldest := refAgo(30 * retentionDay)

	listFailure := errors.New("bucket unreachable")

	tests := []struct {
		name string
		// refs is the location's listing, deliberately in an order the ranking has to
		// correct rather than one it can pass through.
		refs        []backupRef
		listErr     error
		want        backupRef
		errContains []string
		wantWrapped error
	}{
		{
			name: "newest timestamp wins",
			refs: []backupRef{oldest, newest, plain},
			want: newest,
		},
		{
			name: "newest timestamp wins whatever the listing order",
			refs: []backupRef{newest, plain, oldest},
			want: newest,
		},
		{
			name: "a single backup is the latest",
			refs: []backupRef{oldest},
			want: oldest,
		},
		{
			// Retention's tiebreak, inherited unchanged: with two archives sharing a
			// timestamp the name-descending order puts the encrypted variant first, so
			// both features agree on which of the two is "the newest".
			name: "timestamp tie breaks by name descending",
			refs: []backupRef{plain, encrypted},
			want: encrypted,
		},
		{
			name:        "empty pool errors and names the location",
			refs:        nil,
			errContains: []string{"no backups found", "local:/backups", "--latest"},
		},
		{
			name:        "listing failure propagates wrapped",
			listErr:     listFailure,
			errContains: []string{"failed to resolve the latest backup at", "local:/backups"},
			wantWrapped: listFailure,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			location := &fakeLocation{name: "local:/backups", refs: tc.refs, listErr: tc.listErr}

			got, err := resolveLatestBackup(context.Background(), location)

			if len(tc.errContains) > 0 {
				if err == nil {
					t.Fatalf("resolveLatestBackup() = %+v, nil, want an error", got)
				}
				for _, want := range tc.errContains {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error = %q, want it to contain %q", err, want)
					}
				}
				if tc.wantWrapped != nil && !errors.Is(err, tc.wantWrapped) {
					t.Errorf("error = %q, want it to wrap %v", err, tc.wantWrapped)
				}
				if got != (backupRef{}) {
					t.Errorf("ref = %+v, want the zero ref alongside an error", got)
				}
				return
			}

			if err != nil {
				t.Fatalf("resolveLatestBackup() = %v, want nil", err)
			}
			if got != tc.want {
				t.Errorf("resolveLatestBackup() = %+v, want %+v", got, tc.want)
			}
			// Nothing is deleted while resolving: --latest only ever reads its pool.
			if len(location.attempted) != 0 {
				t.Errorf("attempted deletions = %v, want none", location.attempted)
			}
		})
	}
}

// TestResolveLatestBackupMatchesRetentionRanking pins FR-005 structurally: whatever the
// pool, the archive `--latest` restores is the one retention reports as its newest —
// the ref that always survives a prune.
func TestResolveLatestBackupMatchesRetentionRanking(t *testing.T) {
	sameTimestamp := retentionNow.Add(-2 * retentionDay)
	refs := []backupRef{
		refAgo(30 * retentionDay),
		{Name: backupNameAt(sameTimestamp)},
		refAgo(1 * retentionDay),
		{Name: backupNameAt(sameTimestamp) + ".enc"},
		refAgo(8 * retentionDay),
	}

	latest, err := resolveLatestBackup(context.Background(), &fakeLocation{name: "local:/backups", refs: refs})
	if err != nil {
		t.Fatalf("resolveLatestBackup() = %v, want nil", err)
	}

	// A policy that would prune everything it is allowed to still keeps rank 0.
	keep, _ := selectPrunable(refs, RetentionPolicy{Count: 1}, retentionNow)
	if len(keep) != 1 {
		t.Fatalf("selectPrunable kept %d backup(s), want only the newest", len(keep))
	}
	if latest != keep[0] {
		t.Errorf("resolveLatestBackup() = %+v, want retention's newest %+v", latest, keep[0])
	}
}

// TestRestoreLatestFromFailFast covers FR-007 and FR-009: an encrypted newest archive
// with no key to decrypt it is refused before anything is delivered — before any
// download, before the --sleep wait, before any container is touched — and every run
// that does proceed says which archive it chose and where from, first.
func TestRestoreLatestFromFailFast(t *testing.T) {
	plain := refAgo(1 * retentionDay)
	encrypted := backupRef{Name: backupNameAt(retentionNow) + ".enc"}
	olderEncrypted := backupRef{Name: backupNameAt(retentionNow.Add(-10*retentionDay)) + ".enc"}
	older := refAgo(30 * retentionDay)

	tests := []struct {
		name        string
		refs        []backupRef
		decryptKey  string
		wantDeliver backupRef
		errContains []string
	}{
		{
			name:        "plain newest is delivered",
			refs:        []backupRef{older, plain},
			wantDeliver: plain,
		},
		{
			// The gate asks about the archive that was selected, not about the pool: an
			// older encrypted archive sitting behind a plain newest one demands no key.
			name:        "an older encrypted archive does not demand a key",
			refs:        []backupRef{olderEncrypted, plain, older},
			wantDeliver: plain,
		},
		{
			name:        "encrypted newest with a key is delivered",
			refs:        []backupRef{plain, encrypted, older},
			decryptKey:  "/etc/infrahub/backup.key",
			wantDeliver: encrypted,
		},
		{
			name:        "an encrypted newest never falls back to the older plain archive",
			refs:        []backupRef{encrypted, plain, older},
			errContains: []string{"is encrypted", "--decrypt-key", encrypted.Name, "never falls back"},
		},
		{
			name:        "an empty pool is refused before delivery",
			refs:        nil,
			errContains: []string{"no backups found", "local:/backups"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			location := &fakeLocation{name: "local:/backups", refs: tc.refs}

			var delivered []backupRef
			var err error
			output := captureLogrus(t, func() {
				err = restoreLatestFrom(context.Background(), location, tc.decryptKey, func(ref backupRef) error {
					delivered = append(delivered, ref)
					return nil
				})
			})

			if len(tc.errContains) > 0 {
				if err == nil {
					t.Fatalf("restoreLatestFrom() = nil, want an error")
				}
				for _, want := range tc.errContains {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error = %q, want it to contain %q", err, want)
					}
				}
				// The refusal is the whole guarantee: nothing downstream of the gate ran,
				// so no archive was downloaded and no container was touched.
				if len(delivered) != 0 {
					t.Errorf("delivered = %v, want no delegation after a refusal", refNames(delivered))
				}
				// A refused run must not claim it is restoring anything.
				if strings.Contains(output, "Restoring latest backup") {
					t.Errorf("log output = %q, want no restore announcement on a refused run", output)
				}
				return
			}

			if err != nil {
				t.Fatalf("restoreLatestFrom() = %v, want nil", err)
			}
			if got, want := refNames(delivered), []string{tc.wantDeliver.Name}; !slices.Equal(got, want) {
				t.Fatalf("delivered = %v, want %v", got, want)
			}
			// FR-009: exactly the archive and the pool, at info level, before the restore.
			wantLine := fmt.Sprintf("Restoring latest backup %s from local:/backups", tc.wantDeliver.Name)
			if !strings.Contains(output, wantLine) {
				t.Errorf("log output = %q, want it to contain %q", output, wantLine)
			}
			if !strings.Contains(output, "level=info") {
				t.Errorf("log output = %q, want the selection reported at info level", output)
			}
		})
	}
}

// TestRestoreLatestFromPropagatesDeliveryFailure keeps a failing restore a failing
// command: the selection layer adds no context it cannot improve on, and swallows
// nothing (constitution V).
func TestRestoreLatestFromPropagatesDeliveryFailure(t *testing.T) {
	restoreFailure := errors.New("neo4j-admin load failed")
	location := &fakeLocation{name: "local:/backups", refs: []backupRef{refAgo(1 * retentionDay)}}

	err := restoreLatestFrom(context.Background(), location, "", func(backupRef) error { return restoreFailure })
	if !errors.Is(err, restoreFailure) {
		t.Errorf("restoreLatestFrom() = %v, want it to wrap %v", err, restoreFailure)
	}
}

// TestRestoreLatestBackupLocalPool drives the exported entry point against a real backup
// directory. It covers the two refusals that must happen before the deployment is
// touched at all (FR-007, FR-008) — the paths a scheduled restore has to fail on rather
// than proceed through — and that the archive it selects is the newest name in the
// directory, from the directory the configuration points at.
func TestRestoreLatestBackupLocalPool(t *testing.T) {
	writeArchives := func(t *testing.T, dir string, names ...string) {
		t.Helper()

		for _, name := range names {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("archive"), 0o600); err != nil {
				t.Fatalf("writing %s = %v, want nil", name, err)
			}
		}
	}

	t.Run("an empty directory is refused", func(t *testing.T) {
		dir := t.TempDir()
		iops := &InfrahubOps{config: &Configuration{BackupDir: dir, Backend: BackendTarball}}

		err := iops.RestoreLatestBackup(false, false, false, 0, "", false, false)
		if err == nil {
			t.Fatal("RestoreLatestBackup() = nil, want an error for an empty pool")
		}
		for _, want := range []string{"no backups found", "local:" + dir} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %q, want it to contain %q", err, want)
			}
		}
	})

	t.Run("a missing directory is refused rather than read as an empty pool", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "does-not-exist")
		iops := &InfrahubOps{config: &Configuration{BackupDir: dir, Backend: BackendTarball}}

		err := iops.RestoreLatestBackup(false, false, false, 0, "", false, false)
		if err == nil {
			t.Fatal("RestoreLatestBackup() = nil, want an error for a missing backup directory")
		}
		if !strings.Contains(err.Error(), dir) {
			t.Errorf("error = %q, want it to name the directory", err)
		}
	})

	t.Run("the S3 pool is refused before anything is listed when it is not configured", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, backupNameAt(retentionNow)), []byte("archive"), 0o600); err != nil {
			t.Fatalf("writing the local archive = %v, want nil", err)
		}
		iops := &InfrahubOps{config: &Configuration{BackupDir: dir, Backend: BackendTarball, S3: &S3Config{}}}

		// A sleep a run reaching it would hang the test on: an unusable pool has to be
		// refused before any of the restore path runs.
		err := iops.RestoreLatestBackup(true, false, false, time.Hour, "", false, false)
		if err == nil {
			t.Fatal("RestoreLatestBackup() = nil, want the unconfigured S3 pool refused")
		}
		if !strings.Contains(err.Error(), "S3 bucket is required") {
			t.Errorf("error = %q, want it to report the missing bucket", err)
		}
		// --s3 never falls back to the local pool, however restorable it looks (FR-006).
		if strings.Contains(err.Error(), dir) {
			t.Errorf("error = %q, want no mention of the local pool", err)
		}
	})

	t.Run("an encrypted newest archive without a key is refused before the sleep", func(t *testing.T) {
		dir := t.TempDir()
		newest := backupNameAt(retentionNow) + ".enc"
		writeArchives(t, dir, backupNameAt(retentionNow.Add(-retentionDay)), newest, "notes.txt")
		iops := &InfrahubOps{config: &Configuration{BackupDir: dir, Backend: BackendTarball}}

		// A sleep long enough that a run reaching it would hang the test rather than
		// pass: the refusal has to come first (FR-007).
		err := iops.RestoreLatestBackup(false, false, false, time.Hour, "", false, false)
		if err == nil {
			t.Fatal("RestoreLatestBackup() = nil, want the encrypted newest archive refused")
		}
		for _, want := range []string{newest, "--decrypt-key"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %q, want it to contain %q", err, want)
			}
		}
		// The archive is still there: a refused run reads its pool and changes nothing.
		if _, statErr := os.Stat(filepath.Join(dir, newest)); statErr != nil {
			t.Errorf("stat(%s) = %v, want the archive untouched", newest, statErr)
		}
	})
}

// remoteArchiveBody is what the fake bucket's object contains, distinct from anything a
// local copy holds so the two can never be confused for one another in an assertion.
const remoteArchiveBody = "the archive that lives in the bucket"

// fakeS3RestoreClient is an s3RestoreClient whose download is scripted. It extends the
// retention fake — so the listing, the key construction, and the location label are the
// real client's — with the one call the restore leg adds, recording every key and local
// path it was asked for.
type fakeS3RestoreClient struct {
	*fakeS3Backend
	// body is what the object contains; the leg must hand exactly these bytes to the
	// restore.
	body string
	// err makes the download fail the way an unreachable endpoint or an object pruned
	// between the listing and the download does.
	err error

	keys       []string
	localPaths []string
}

func newFakeS3RestoreClient(prefix string, refs ...backupRef) *fakeS3RestoreClient {
	return &fakeS3RestoreClient{
		fakeS3Backend: &fakeS3Backend{S3Client: s3KeyClient("infrahub-backups", prefix), refs: refs},
		body:          remoteArchiveBody,
	}
}

func (f *fakeS3RestoreClient) Download(_ context.Context, s3Key, localPath string) error {
	f.keys = append(f.keys, s3Key)
	f.localPaths = append(f.localPaths, localPath)

	if f.err != nil {
		// S3Client.Download removes the file it created before reporting a failure, so
		// the fake leaves the same state behind for the leg's own cleanup to be judged
		// against.
		os.Remove(localPath)

		return f.err
	}

	return os.WriteFile(localPath, []byte(f.body), 0o600)
}

var (
	_ s3RestoreClient = (*fakeS3RestoreClient)(nil)
	_ s3Backend       = (*fakeS3RestoreClient)(nil)
)

// dirNames is everything a directory holds, which is how both "nothing was left behind"
// and "nothing was removed" are asserted.
func dirNames(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s) = %v, want nil", dir, err)
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	slices.Sort(names)

	return names
}

// TestDownloadLatestS3BackupUsesACollisionProofTempPath is critiques E1/X1 and E3 as a
// test: the download lands on a name that is not a backup archive name, so the operator's
// own local copy of the same archive — what `create --s3-upload --s3-keep-local` leaves
// behind, and therefore the expected state of this flow — is neither overwritten nor
// deleted, and a temp file no cleanup reached could never be selected by a later --latest
// run.
func TestDownloadLatestS3BackupUsesACollisionProofTempPath(t *testing.T) {
	const localCopy = "the operator's own local copy"

	dir := t.TempDir()
	ref := backupRef{Name: backupNameAt(retentionNow)}
	localPath := filepath.Join(dir, ref.Name)
	if err := os.WriteFile(localPath, []byte(localCopy), 0o600); err != nil {
		t.Fatalf("writing the local copy = %v, want nil", err)
	}

	client := newFakeS3RestoreClient("prod")

	var restored []string
	var restoredBody string
	err := downloadLatestS3Backup(context.Background(), client, dir, ref, func(path string) error {
		restored = append(restored, path)

		body, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Errorf("reading the downloaded archive = %v, want nil", readErr)
		}
		restoredBody = string(body)

		return nil
	})
	if err != nil {
		t.Fatalf("downloadLatestS3Backup() = %v, want nil", err)
	}

	if len(restored) != 1 {
		t.Fatalf("restored = %v, want exactly one restore", restored)
	}
	name := filepath.Base(restored[0])

	// The property the whole leg rests on: the download is invisible to the pool.
	if backupNamePattern.MatchString(name) {
		t.Errorf("temp download %q matches backupNamePattern: a failed cleanup would pollute the pool", name)
	}
	if !strings.HasPrefix(name, "restore-latest-") || !strings.HasSuffix(name, ".download") {
		t.Errorf("temp download = %q, want the %q shape", name, s3RestoreTempPattern)
	}
	if got := filepath.Dir(restored[0]); got != dir {
		t.Errorf("temp download directory = %q, want the backup directory %q", got, dir)
	}

	// The object is addressed by the key an upload wrote it to, and the bytes that arrive
	// are the ones handed to the restore.
	if want := []string{"prod/" + ref.Name}; !slices.Equal(client.keys, want) {
		t.Errorf("downloaded keys = %v, want %v", client.keys, want)
	}
	if !slices.Equal(client.localPaths, restored) {
		t.Errorf("download paths = %v, want the restored path %v", client.localPaths, restored)
	}
	if restoredBody != remoteArchiveBody {
		t.Errorf("restored archive = %q, want the downloaded object %q", restoredBody, remoteArchiveBody)
	}

	// Neither overwritten nor deleted (critique E1/X1).
	survivor, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatalf("reading the local copy back = %v, want it untouched", err)
	}
	if string(survivor) != localCopy {
		t.Errorf("local copy = %q, want it byte-identical %q", survivor, localCopy)
	}
	if got, want := dirNames(t, dir), []string{ref.Name}; !slices.Equal(got, want) {
		t.Errorf("backup directory = %v, want only the local copy %v", got, want)
	}
}

// TestDownloadLatestS3BackupCleansUpTheTempDownload covers the failure exits: a download
// or a restore that fails reports why and leaves no half-downloaded archive occupying the
// backup directory — and, either way, the operator's same-named local copy is still there.
func TestDownloadLatestS3BackupCleansUpTheTempDownload(t *testing.T) {
	const localCopy = "the operator's own local copy"

	ref := backupRef{Name: backupNameAt(retentionNow)}
	downloadFailure := errors.New("connection reset by peer")
	restoreFailure := errors.New("neo4j-admin load failed")

	tests := []struct {
		name         string
		downloadErr  error
		restoreErr   error
		wantWrapped  error
		wantRestores int
		errContains  []string
	}{
		{
			name:        "a failed download is reported and leaves nothing behind",
			downloadErr: downloadFailure,
			wantWrapped: downloadFailure,
			errContains: []string{"failed to download the latest backup", ref.Name},
		},
		{
			name:         "a failed restore still removes the download",
			restoreErr:   restoreFailure,
			wantWrapped:  restoreFailure,
			wantRestores: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			localPath := filepath.Join(dir, ref.Name)
			if err := os.WriteFile(localPath, []byte(localCopy), 0o600); err != nil {
				t.Fatalf("writing the local copy = %v, want nil", err)
			}

			client := newFakeS3RestoreClient("prod")
			client.err = tc.downloadErr

			restores := 0
			err := downloadLatestS3Backup(context.Background(), client, dir, ref, func(string) error {
				restores++
				return tc.restoreErr
			})

			if err == nil {
				t.Fatalf("downloadLatestS3Backup() = nil, want an error")
			}
			if !errors.Is(err, tc.wantWrapped) {
				t.Errorf("error = %q, want it to wrap %v", err, tc.wantWrapped)
			}
			for _, want := range tc.errContains {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to contain %q", err, want)
				}
			}
			if restores != tc.wantRestores {
				t.Errorf("restores = %d, want %d", restores, tc.wantRestores)
			}
			// The failure path is where a leaked temp file would go unnoticed, so it is
			// exactly where the directory has to be back to what it was.
			if got, want := dirNames(t, dir), []string{ref.Name}; !slices.Equal(got, want) {
				t.Errorf("backup directory = %v, want only the local copy %v", got, want)
			}
			survivor, readErr := os.ReadFile(localPath)
			if readErr != nil {
				t.Fatalf("reading the local copy back = %v, want it untouched", readErr)
			}
			if string(survivor) != localCopy {
				t.Errorf("local copy = %q, want it byte-identical %q", survivor, localCopy)
			}
		})
	}
}

// TestRestoreLatestFromS3Pool wires the S3 leg the way RestoreLatestBackup wires it — one
// client behind both the listing and the download — and pins the two properties that
// wiring exists for: the object downloaded is the one the selection chose, named with the
// bucket and prefix it came from (FR-006, FR-009), and an encrypted newest object with no
// key downloads nothing at all (FR-007).
func TestRestoreLatestFromS3Pool(t *testing.T) {
	newest := backupRef{Name: backupNameAt(retentionNow)}
	older := refAgo(10 * retentionDay)
	encrypted := backupRef{Name: backupNameAt(retentionNow.Add(retentionDay)) + encryptedArchiveSuffix}

	// run performs the whole S3 leg against a fake bucket and reports everything a
	// scheduled run would be judged on: what was downloaded, what was restored, what the
	// backup directory holds afterwards, and what the operator saw.
	run := func(t *testing.T, decryptKey string, refs ...backupRef) (*fakeS3RestoreClient, []string, []string, string, error) {
		t.Helper()

		dir := t.TempDir()
		client := newFakeS3RestoreClient("prod", refs...)

		var restored []string
		var err error
		output := captureLogrus(t, func() {
			err = restoreLatestFrom(context.Background(), newS3Location(client), decryptKey, func(ref backupRef) error {
				return downloadLatestS3Backup(context.Background(), client, dir, ref, func(path string) error {
					restored = append(restored, path)
					return nil
				})
			})
		})

		return client, restored, dirNames(t, dir), output, err
	}

	t.Run("the newest object is downloaded and restored", func(t *testing.T) {
		client, restored, remaining, output, err := run(t, "", older, newest)
		if err != nil {
			t.Fatalf("restoreLatestFrom() = %v, want nil", err)
		}

		if want := []string{"prod/" + newest.Name}; !slices.Equal(client.keys, want) {
			t.Errorf("downloaded keys = %v, want only the newest object %v", client.keys, want)
		}
		if len(restored) != 1 {
			t.Errorf("restored = %v, want exactly one restore", restored)
		}
		if len(remaining) != 0 {
			t.Errorf("backup directory = %v, want the temporary download removed", remaining)
		}
		wantLine := fmt.Sprintf("Restoring latest backup %s from s3://infrahub-backups/prod", newest.Name)
		if !strings.Contains(output, wantLine) {
			t.Errorf("log output = %q, want it to contain %q", output, wantLine)
		}
	})

	t.Run("an encrypted newest object is refused before it is downloaded", func(t *testing.T) {
		client, restored, remaining, _, err := run(t, "", newest, encrypted, older)
		if err == nil {
			t.Fatal("restoreLatestFrom() = nil, want the encrypted newest object refused")
		}
		for _, want := range []string{encrypted.Name, "--decrypt-key", "never falls back"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %q, want it to contain %q", err, want)
			}
		}

		// FR-007 in its strongest form: no request was made and no file was created, so
		// nothing was transferred and no space was consumed before the refusal.
		if len(client.keys) != 0 {
			t.Errorf("downloaded keys = %v, want no download after a refusal", client.keys)
		}
		if len(restored) != 0 {
			t.Errorf("restored = %v, want no restore after a refusal", restored)
		}
		if len(remaining) != 0 {
			t.Errorf("backup directory = %v, want it untouched", remaining)
		}
	})
}
