package app

import (
	"context"
	"errors"
	"strings"
	"testing"
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
