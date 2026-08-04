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
