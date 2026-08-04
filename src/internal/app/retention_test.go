package app

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// retentionNow is the fixed evaluation instant used by the selection tests, in
// host-local time so it matches the timestamps parsed from backup names.
var retentionNow = time.Date(2026, 8, 4, 12, 0, 0, 0, time.Local)

// backupNameAt renders the archive name a backup created at ts would carry.
func backupNameAt(ts time.Time) string {
	return "infrahub_backup_" + ts.Format(backupNameTimestampLayout) + ".tar.gz"
}

// refAgo builds a backupRef for a backup created age before retentionNow.
func refAgo(age time.Duration) backupRef {
	created := retentionNow.Add(-age)
	return backupRef{Name: backupNameAt(created), CreatedAt: created}
}

func refNames(refs []backupRef) []string {
	names := make([]string, 0, len(refs))
	for _, ref := range refs {
		names = append(names, ref.Name)
	}
	return names
}

func TestRetentionPolicyActive(t *testing.T) {
	tests := []struct {
		name   string
		policy RetentionPolicy
		want   bool
	}{
		{name: "zero value is inactive", policy: RetentionPolicy{}, want: false},
		{name: "days only", policy: RetentionPolicy{Days: 7}, want: true},
		{name: "count only", policy: RetentionPolicy{Count: 14}, want: true},
		{name: "both rules", policy: RetentionPolicy{Days: 7, Count: 14}, want: true},
		{name: "negative values do not activate", policy: RetentionPolicy{Days: -1, Count: -1}, want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.policy.Active(); got != tc.want {
				t.Errorf("Active() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRetentionPolicyValidate(t *testing.T) {
	tests := []struct {
		name        string
		policy      RetentionPolicy
		wantErr     bool
		errContains string
	}{
		{name: "inactive policy is valid", policy: RetentionPolicy{}},
		{name: "days only", policy: RetentionPolicy{Days: 1}},
		{name: "count only", policy: RetentionPolicy{Count: 1}},
		{name: "both rules", policy: RetentionPolicy{Days: 7, Count: 14}},
		{
			name:        "negative days rejected",
			policy:      RetentionPolicy{Days: -1},
			wantErr:     true,
			errContains: "retention-days must be at least 1",
		},
		{
			name:        "negative count rejected",
			policy:      RetentionPolicy{Count: -5},
			wantErr:     true,
			errContains: "retention-count must be at least 1",
		},
		{
			name:        "days reported before count",
			policy:      RetentionPolicy{Days: -1, Count: -1},
			wantErr:     true,
			errContains: "retention-days must be at least 1",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.policy.Validate()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Validate() = nil, want error containing %q", tc.errContains)
				}
				if !strings.Contains(err.Error(), tc.errContains) {
					t.Errorf("Validate() error = %q, want it to contain %q", err, tc.errContains)
				}
				return
			}
			if err != nil {
				t.Errorf("Validate() = %v, want nil", err)
			}
		})
	}
}

func TestRetentionConfigPolicy(t *testing.T) {
	cfg := RetentionConfig{Days: 7, Count: 14}
	if got := cfg.Policy(); got != (RetentionPolicy{Days: 7, Count: 14}) {
		t.Errorf("Policy() = %+v, want Days 7 / Count 14", got)
	}
	if (RetentionConfig{}).Policy().Active() {
		t.Error("zero RetentionConfig produced an active policy; retention must be opt-in")
	}
}

func TestParseBackupName(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    bool
		wantUTC string // expected CreatedAt rendered with the embedded layout
	}{
		// Recognized backups.
		{name: "plain archive", input: "infrahub_backup_20260804_120000.tar.gz", want: true, wantUTC: "20260804_120000"},
		{name: "encrypted archive", input: "infrahub_backup_20260804_120000.tar.gz.enc", want: true, wantUTC: "20260804_120000"},
		{name: "midnight timestamp", input: "infrahub_backup_20260101_000000.tar.gz", want: true, wantUTC: "20260101_000000"},
		{name: "leap day", input: "infrahub_backup_20240229_235959.tar.gz", want: true, wantUTC: "20240229_235959"},

		// Decoys that share the directory with real backups.
		{name: "unrelated file", input: "notes.txt"},
		{name: "backup-like without timestamp", input: "infrahub_backup_garbage.tar.gz"},
		{name: "other tarball", input: "somebackup.tar.gz"},
		{name: "checksum sidecar", input: "infrahub_backup_20260804_120000.tar.gz.sha256"},
		{name: "double encryption suffix", input: "infrahub_backup_20260804_120000.tar.gz.enc.enc"},
		{name: "wrong extension", input: "infrahub_backup_20260804_120000.tar"},
		{name: "temp suffix", input: "infrahub_backup_20260804_120000.tar.gz.part"},
		{name: "wrong prefix", input: "old_infrahub_backup_20260804_120000.tar.gz"},
		{name: "empty name", input: ""},

		// Superficial matches whose timestamp is not a real point in time.
		{name: "month 13", input: "infrahub_backup_20261301_120000.tar.gz"},
		{name: "february 30", input: "infrahub_backup_20260230_120000.tar.gz"},
		{name: "hour 25", input: "infrahub_backup_20260804_250000.tar.gz"},
		{name: "minute 60", input: "infrahub_backup_20260804_126000.tar.gz"},
		{name: "day zero", input: "infrahub_backup_20260800_120000.tar.gz"},
		{name: "too few digits", input: "infrahub_backup_2026080_120000.tar.gz"},
		{name: "too many digits", input: "infrahub_backup_202608041_120000.tar.gz"},

		// Path separators can never match: retention only ever deletes base names.
		{name: "relative parent path", input: "../infrahub_backup_20260804_120000.tar.gz"},
		{name: "absolute path", input: "/etc/infrahub_backup_20260804_120000.tar.gz"},
		{name: "s3 style prefixed key", input: "backups/prod/infrahub_backup_20260804_120000.tar.gz"},
		{name: "windows separator", input: `dir\infrahub_backup_20260804_120000.tar.gz`},
		{name: "traversal suffix", input: "infrahub_backup_20260804_120000.tar.gz/../../etc/passwd"},
		{name: "embedded newline", input: "infrahub_backup_20260804_120000.tar.gz\nnotes.txt"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ref, ok := parseBackupName(tc.input)
			if ok != tc.want {
				t.Fatalf("parseBackupName(%q) ok = %v, want %v", tc.input, ok, tc.want)
			}
			if !tc.want {
				if ref != (backupRef{}) {
					t.Errorf("parseBackupName(%q) returned %+v alongside ok=false, want the zero ref", tc.input, ref)
				}
				return
			}
			if ref.Name != tc.input {
				t.Errorf("ref.Name = %q, want %q", ref.Name, tc.input)
			}
			if got := ref.CreatedAt.Format(backupNameTimestampLayout); got != tc.wantUTC {
				t.Errorf("ref.CreatedAt = %s, want embedded timestamp %s", got, tc.wantUTC)
			}
			if loc := ref.CreatedAt.Location(); loc != time.Local {
				t.Errorf("ref.CreatedAt location = %v, want host-local time", loc)
			}
		})
	}
}

// TestParseBackupNameRoundTripsGeneratedFilename binds the writer
// (generateBackupFilename) to the reader (parseBackupName): if the archive naming
// format ever changes, new backups must not silently become invisible to
// retention.
func TestParseBackupNameRoundTripsGeneratedFilename(t *testing.T) {
	before := time.Now().Truncate(time.Second)
	generated := (&InfrahubOps{}).generateBackupFilename()
	after := time.Now()

	ref, ok := parseBackupName(generated)
	if !ok {
		t.Fatalf("parseBackupName(%q) did not recognize a name produced by generateBackupFilename", generated)
	}
	if ref.Name != generated {
		t.Errorf("ref.Name = %q, want %q", ref.Name, generated)
	}
	if ref.CreatedAt.Before(before) || ref.CreatedAt.After(after) {
		t.Errorf("ref.CreatedAt = %v, want within [%v, %v]", ref.CreatedAt, before, after)
	}

	encrypted := generated + ".enc"
	encRef, ok := parseBackupName(encrypted)
	if !ok {
		t.Fatalf("parseBackupName(%q) did not recognize the encrypted variant", encrypted)
	}
	if !encRef.CreatedAt.Equal(ref.CreatedAt) {
		t.Errorf("encrypted variant CreatedAt = %v, want %v", encRef.CreatedAt, ref.CreatedAt)
	}
}

func TestSortBackupRefsNewestFirst(t *testing.T) {
	sameTimestamp := retentionNow.Add(-2 * retentionDay)
	plain := backupRef{Name: backupNameAt(sameTimestamp), CreatedAt: sameTimestamp}
	encrypted := backupRef{Name: backupNameAt(sameTimestamp) + ".enc", CreatedAt: sameTimestamp}
	newest := refAgo(1 * retentionDay)
	oldest := refAgo(30 * retentionDay)

	// Ties break by name descending, so the encrypted variant precedes the plain one.
	want := refNames([]backupRef{newest, encrypted, plain, oldest})

	inputs := [][]backupRef{
		{newest, encrypted, plain, oldest},
		{oldest, plain, encrypted, newest},
		{plain, oldest, newest, encrypted},
	}
	for _, input := range inputs {
		refs := slices.Clone(input)
		sortBackupRefsNewestFirst(refs)
		if got := refNames(refs); !slices.Equal(got, want) {
			t.Errorf("sortBackupRefsNewestFirst(%v) = %v, want %v", refNames(input), got, want)
		}
	}
}

func TestSelectPrunable(t *testing.T) {
	// Age ladder relative to retentionNow.
	day1 := refAgo(1 * retentionDay)
	day3 := refAgo(3 * retentionDay)
	day6 := refAgo(6 * retentionDay)
	day8 := refAgo(8 * retentionDay)
	day20 := refAgo(20 * retentionDay)
	day30 := refAgo(30 * retentionDay)
	exactly7 := refAgo(7 * retentionDay)
	future := refAgo(-2 * retentionDay)

	tests := []struct {
		name      string
		refs      []backupRef
		policy    RetentionPolicy
		wantKeep  []string
		wantPrune []string
	}{
		{
			name:     "empty location is a no-op",
			refs:     nil,
			policy:   RetentionPolicy{Days: 7},
			wantKeep: nil,
		},
		{
			name:     "single backup is never pruned",
			refs:     []backupRef{day30},
			policy:   RetentionPolicy{Days: 7, Count: 1},
			wantKeep: []string{day30.Name},
		},
		{
			name:      "days rule keeps recent backups",
			refs:      []backupRef{day1, day6, day8, day30},
			policy:    RetentionPolicy{Days: 7},
			wantKeep:  []string{day1.Name, day6.Name},
			wantPrune: []string{day8.Name, day30.Name},
		},
		{
			name:      "age exactly at the boundary is out of policy",
			refs:      []backupRef{day1, exactly7, day30},
			policy:    RetentionPolicy{Days: 7},
			wantKeep:  []string{day1.Name},
			wantPrune: []string{exactly7.Name, day30.Name},
		},
		{
			name:      "count rule keeps the newest N",
			refs:      []backupRef{day1, day3, day20, day30},
			policy:    RetentionPolicy{Count: 2},
			wantKeep:  []string{day1.Name, day3.Name},
			wantPrune: []string{day20.Name, day30.Name},
		},
		{
			name:      "union keeps what either rule claims",
			refs:      []backupRef{day1, day6, day8, day20, day30},
			policy:    RetentionPolicy{Days: 7, Count: 4},
			wantKeep:  []string{day1.Name, day6.Name, day8.Name, day20.Name},
			wantPrune: []string{day30.Name},
		},
		{
			name:      "union: age rule keeps more than the count rule alone",
			refs:      []backupRef{day1, day3, day6, day30},
			policy:    RetentionPolicy{Days: 7, Count: 1},
			wantKeep:  []string{day1.Name, day3.Name, day6.Name},
			wantPrune: []string{day30.Name},
		},
		{
			name:      "all expired keeps the newest as the floor",
			refs:      []backupRef{day20, day30, refAgo(40 * retentionDay)},
			policy:    RetentionPolicy{Days: 7},
			wantKeep:  []string{day20.Name},
			wantPrune: []string{day30.Name, refAgo(40 * retentionDay).Name},
		},
		{
			name:      "count zero-equivalent still respects the floor",
			refs:      []backupRef{day1, day3, day20},
			policy:    RetentionPolicy{Count: 1},
			wantKeep:  []string{day1.Name},
			wantPrune: []string{day3.Name, day20.Name},
		},
		{
			name:     "inactive policy prunes nothing",
			refs:     []backupRef{day1, day20, day30},
			policy:   RetentionPolicy{},
			wantKeep: []string{day1.Name, day20.Name, day30.Name},
		},
		{
			name:      "future timestamps are kept and anchor the floor",
			refs:      []backupRef{future, day1, day30},
			policy:    RetentionPolicy{Days: 7},
			wantKeep:  []string{future.Name, day1.Name},
			wantPrune: []string{day30.Name},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			input := slices.Clone(tc.refs)
			keep, prune := selectPrunable(tc.refs, tc.policy, retentionNow)

			if got := refNames(keep); !slices.Equal(got, tc.wantKeep) {
				t.Errorf("keep = %v, want %v", got, tc.wantKeep)
			}
			if got := refNames(prune); !slices.Equal(got, tc.wantPrune) {
				t.Errorf("prune = %v, want %v", got, tc.wantPrune)
			}
			if len(keep)+len(prune) != len(tc.refs) {
				t.Errorf("keep+prune covered %d refs, want all %d", len(keep)+len(prune), len(tc.refs))
			}
			// SC-003: a location that had a backup never ends up with none.
			if len(tc.refs) > 0 && len(keep) == 0 {
				t.Error("keep is empty for a non-empty location; the keep-newest floor was violated")
			}
			// The function is pure: the caller's slice must be untouched.
			if !slices.Equal(refNames(tc.refs), refNames(input)) {
				t.Errorf("input slice was reordered to %v, want %v", refNames(tc.refs), refNames(input))
			}
		})
	}
}

// TestSelectPrunableFloorAlwaysKeepsNewest asserts the keep-newest invariant
// (FR-003, SC-003) for every location size under a policy that would otherwise
// claim nothing at all.
func TestSelectPrunableFloorAlwaysKeepsNewest(t *testing.T) {
	policies := []RetentionPolicy{{Days: 1}, {Count: 1}, {Days: 1, Count: 1}}

	for _, policy := range policies {
		refs := []backupRef{}
		for size := 1; size <= 20; size++ {
			// Every backup is older than any rule can claim.
			refs = append(refs, refAgo(time.Duration(size+10)*retentionDay))

			keep, prune := selectPrunable(refs, policy, retentionNow)
			if len(keep) != 1 {
				t.Fatalf("policy %+v, %d refs: kept %v, want exactly the newest", policy, size, refNames(keep))
			}
			if keep[0].Name != refs[0].Name {
				t.Errorf("policy %+v, %d refs: kept %q, want newest %q", policy, size, keep[0].Name, refs[0].Name)
			}
			if len(prune) != size-1 {
				t.Errorf("policy %+v, %d refs: pruned %d, want %d", policy, size, len(prune), size-1)
			}
		}
	}
}

func TestLocalLocationListAndDelete(t *testing.T) {
	dir := t.TempDir()

	backups := []string{
		"infrahub_backup_20260804_120000.tar.gz",
		"infrahub_backup_20260801_090000.tar.gz.enc",
	}
	decoys := []string{
		"notes.txt",
		"infrahub_backup_garbage.tar.gz",
		"somebackup.tar.gz",
		"infrahub_backup_20261301_120000.tar.gz", // unparseable timestamp
		"infrahub_backup_20260804_120000.tar.gz.sha256",
	}
	for _, name := range append(slices.Clone(backups), decoys...) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A directory whose name matches the backup pattern is not a backup.
	if err := os.Mkdir(filepath.Join(dir, "infrahub_backup_20260101_000000.tar.gz"), 0o755); err != nil {
		t.Fatal(err)
	}

	location := newLocalLocation(dir)
	if got, want := location.Name(), "local:"+dir; got != want {
		t.Errorf("Name() = %q, want %q", got, want)
	}

	refs, err := location.List(context.Background())
	if err != nil {
		t.Fatalf("List() = %v, want nil", err)
	}
	sortBackupRefsNewestFirst(refs)
	want := []string{backups[0], backups[1]}
	if got := refNames(refs); !slices.Equal(got, want) {
		t.Fatalf("List() = %v, want %v", got, want)
	}

	for _, ref := range refs {
		if err := location.Delete(context.Background(), ref); err != nil {
			t.Fatalf("Delete(%q) = %v, want nil", ref.Name, err)
		}
	}

	// Deleting a name that is not a backup is refused, even if the file exists.
	err = location.Delete(context.Background(), backupRef{Name: "notes.txt"})
	if err == nil {
		t.Fatal("Delete(\"notes.txt\") = nil, want a refusal error")
	}
	if !strings.Contains(err.Error(), "not a backup archive name") {
		t.Errorf("Delete error = %q, want it to mention a non-backup name", err)
	}
	err = location.Delete(context.Background(), backupRef{Name: "../" + backups[0]})
	if err == nil {
		t.Fatal("Delete of a path-traversal name = nil, want a refusal error")
	}

	remaining, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	gotRemaining := make([]string, 0, len(remaining))
	for _, entry := range remaining {
		gotRemaining = append(gotRemaining, entry.Name())
	}
	slices.Sort(gotRemaining)
	wantRemaining := append(slices.Clone(decoys), "infrahub_backup_20260101_000000.tar.gz")
	slices.Sort(wantRemaining)
	if !slices.Equal(gotRemaining, wantRemaining) {
		t.Errorf("directory after pruning = %v, want the decoys untouched %v", gotRemaining, wantRemaining)
	}
}

func TestLocalLocationListEmptyAndMissingDir(t *testing.T) {
	empty := newLocalLocation(t.TempDir())
	refs, err := empty.List(context.Background())
	if err != nil {
		t.Fatalf("List() on an empty directory = %v, want nil", err)
	}
	if len(refs) != 0 {
		t.Errorf("List() on an empty directory = %v, want no refs", refNames(refs))
	}

	missing := newLocalLocation(filepath.Join(t.TempDir(), "does-not-exist"))
	if _, err := missing.List(context.Background()); err == nil {
		t.Error("List() on a missing directory = nil error, want a failure so a mistyped --backup-dir is not silently empty")
	}
}
