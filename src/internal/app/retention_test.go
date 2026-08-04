package app

import (
	"context"
	"errors"
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

func TestS3LocationName(t *testing.T) {
	tests := []struct {
		name   string
		bucket string
		prefix string
		want   string
	}{
		{name: "bucket only", bucket: "infrahub-backups", want: "s3://infrahub-backups"},
		{name: "bucket and prefix", bucket: "infrahub-backups", prefix: "prod", want: "s3://infrahub-backups/prod"},
		{name: "nested prefix", bucket: "b", prefix: "backups/prod", want: "s3://b/backups/prod"},
		{name: "trailing slash trimmed", bucket: "b", prefix: "backups/prod/", want: "s3://b/backups/prod"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			location := newS3Location(s3KeyClient(tc.bucket, tc.prefix))
			if got := location.Name(); got != tc.want {
				t.Errorf("Name() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestS3LocationDeleteRefusesNonBackupNames mirrors the localLocation guard: a ref
// that did not come from List can never reach an unrelated object. The refusal
// happens before the client is touched, so a client with no endpoint is enough.
func TestS3LocationDeleteRefusesNonBackupNames(t *testing.T) {
	location := newS3Location(s3KeyClient("bucket", "backups/prod"))

	for _, name := range []string{
		"notes.txt",
		"",
		"../infrahub_backup_20260804_120000.tar.gz",
		"other/infrahub_backup_20260804_120000.tar.gz",
		"infrahub_backup_20261301_120000.tar.gz",
	} {
		err := location.Delete(context.Background(), backupRef{Name: name})
		if err == nil {
			t.Fatalf("Delete(%q) = nil, want a refusal error", name)
		}
		if !strings.Contains(err.Error(), "not a backup archive name") {
			t.Errorf("Delete(%q) error = %q, want it to mention a non-backup name", name, err)
		}
		if !strings.Contains(err.Error(), "s3://bucket/backups/prod") {
			t.Errorf("Delete(%q) error = %q, want it to name the location", name, err)
		}
	}
}

// fakeLocation is a storageLocation whose contents and failures are scripted, so
// the orchestrator's per-location evaluation, best-effort deletion, and error
// aggregation can be observed without a filesystem or a bucket.
type fakeLocation struct {
	name       string
	refs       []backupRef
	listErr    error
	deleteErrs map[string]error

	listCalls int
	attempted []string
	deleted   []string
}

func (f *fakeLocation) Name() string { return f.name }

func (f *fakeLocation) List(_ context.Context) ([]backupRef, error) {
	f.listCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	return slices.Clone(f.refs), nil
}

func (f *fakeLocation) Delete(_ context.Context, ref backupRef) error {
	f.attempted = append(f.attempted, ref.Name)
	if err := f.deleteErrs[ref.Name]; err != nil {
		return err
	}
	f.deleted = append(f.deleted, ref.Name)
	return nil
}

var _ storageLocation = (*fakeLocation)(nil)

// refDaysAgo builds a ref for a backup created whole days before now. applyRetention
// evaluates against the wall clock, so orchestrator fixtures are anchored to
// time.Now() rather than to the fixed instant the selection tests use.
func refDaysAgo(days int) backupRef {
	created := time.Now().Add(-time.Duration(days) * retentionDay)
	return backupRef{Name: backupNameAt(created), CreatedAt: created}
}

// TestApplyRetentionBestEffortWithinLeg covers FR-008: one failing deletion must
// not strand the candidates behind it.
func TestApplyRetentionBestEffortWithinLeg(t *testing.T) {
	newest, day10, day20, day30 := refDaysAgo(1), refDaysAgo(10), refDaysAgo(20), refDaysAgo(30)
	stubborn := errors.New("permission denied")

	location := &fakeLocation{
		name:       "local:/backups",
		refs:       []backupRef{day30, newest, day10, day20},
		deleteErrs: map[string]error{day20.Name: stubborn},
	}

	outcomes, err := applyRetention(context.Background(), []storageLocation{location}, RetentionPolicy{Days: 7}, false)
	if err == nil {
		t.Fatal("applyRetention() = nil error, want the failed deletion reported")
	}
	if !errors.Is(err, stubborn) {
		t.Errorf("applyRetention() error = %q, want it to wrap the deletion failure", err)
	}
	if !strings.Contains(err.Error(), "retention failed at local:/backups") {
		t.Errorf("applyRetention() error = %q, want it to name the failing leg", err)
	}

	// Every candidate was attempted, oldest-last, including the ones after the failure.
	wantAttempted := []string{day10.Name, day20.Name, day30.Name}
	if !slices.Equal(location.attempted, wantAttempted) {
		t.Errorf("attempted deletions = %v, want every candidate %v", location.attempted, wantAttempted)
	}
	if want := []string{day10.Name, day30.Name}; !slices.Equal(location.deleted, want) {
		t.Errorf("deleted = %v, want %v", location.deleted, want)
	}

	if len(outcomes) != 1 {
		t.Fatalf("outcomes = %d, want 1 per location", len(outcomes))
	}
	outcome := outcomes[0]
	if outcome.Location != "local:/backups" {
		t.Errorf("outcome.Location = %q, want the location name", outcome.Location)
	}
	if outcome.Err == nil {
		t.Error("outcome.Err = nil, want the leg marked as failed")
	}
	if got, want := refNames(outcome.Kept), []string{newest.Name}; !slices.Equal(got, want) {
		t.Errorf("outcome.Kept = %v, want %v", got, want)
	}
	// Pruned reports what actually went away, not what was attempted.
	if got, want := refNames(outcome.Pruned), []string{day10.Name, day30.Name}; !slices.Equal(got, want) {
		t.Errorf("outcome.Pruned = %v, want %v", got, want)
	}
}

// TestApplyRetentionAttemptsEveryLeg covers FR-008's cross-leg half: a failing leg
// must not prevent the others from running.
func TestApplyRetentionAttemptsEveryLeg(t *testing.T) {
	newest, day20 := refDaysAgo(1), refDaysAgo(20)
	listFailure := errors.New("no such directory")

	broken := &fakeLocation{name: "local:/gone", listErr: listFailure}
	healthy := &fakeLocation{name: "s3://bucket/prod", refs: []backupRef{newest, day20}}

	outcomes, err := applyRetention(context.Background(), []storageLocation{broken, healthy}, RetentionPolicy{Days: 7}, false)
	if err == nil {
		t.Fatal("applyRetention() = nil error, want the broken leg reported")
	}
	if !errors.Is(err, listFailure) {
		t.Errorf("applyRetention() error = %q, want it to wrap the listing failure", err)
	}

	if healthy.listCalls != 1 {
		t.Errorf("healthy leg List calls = %d, want 1 despite the earlier leg failing", healthy.listCalls)
	}
	if want := []string{day20.Name}; !slices.Equal(healthy.deleted, want) {
		t.Errorf("healthy leg deleted = %v, want %v", healthy.deleted, want)
	}

	if len(outcomes) != 2 {
		t.Fatalf("outcomes = %d, want one per location", len(outcomes))
	}
	if outcomes[0].Err == nil {
		t.Error("outcomes[0].Err = nil, want the broken leg marked as failed")
	}
	if outcomes[0].Pruned != nil {
		t.Errorf("outcomes[0].Pruned = %v, want nothing pruned at a leg that could not be listed", refNames(outcomes[0].Pruned))
	}
	if outcomes[1].Err != nil {
		t.Errorf("outcomes[1].Err = %v, want nil for the healthy leg", outcomes[1].Err)
	}
}

// TestApplyRetentionErrorAggregation checks the reported text when several legs
// fail: every leg is named and every underlying failure is present (FR-009).
func TestApplyRetentionErrorAggregation(t *testing.T) {
	newest, day20, day30 := refDaysAgo(1), refDaysAgo(20), refDaysAgo(30)
	listFailure := errors.New("no such directory")
	denied := errors.New("AccessDenied: delete forbidden")
	transient := errors.New("500 internal error")

	local := &fakeLocation{name: "local:/gone", listErr: listFailure}
	s3 := &fakeLocation{
		name:       "s3://bucket/prod",
		refs:       []backupRef{newest, day20, day30},
		deleteErrs: map[string]error{day20.Name: denied, day30.Name: transient},
	}

	_, err := applyRetention(context.Background(), []storageLocation{local, s3}, RetentionPolicy{Days: 7}, false)
	if err == nil {
		t.Fatal("applyRetention() = nil error, want both legs reported")
	}

	for _, want := range []string{
		"retention failed at local:/gone: no such directory",
		"retention failed at s3://bucket/prod:",
		denied.Error(),
		transient.Error(),
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("applyRetention() error = %q, want it to contain %q", err, want)
		}
	}
	for _, target := range []error{listFailure, denied, transient} {
		if !errors.Is(err, target) {
			t.Errorf("applyRetention() error does not wrap %v", target)
		}
	}
}

// TestApplyRetentionDryRunMatchesRealRun is SC-004: the preview is exactly the set
// a real run deletes, and a dry run touches nothing.
func TestApplyRetentionDryRunMatchesRealRun(t *testing.T) {
	refs := []backupRef{refDaysAgo(1), refDaysAgo(3), refDaysAgo(10), refDaysAgo(20), refDaysAgo(40)}
	policy := RetentionPolicy{Days: 7, Count: 3}

	newFixture := func() []storageLocation {
		return []storageLocation{
			&fakeLocation{name: "local:/backups", refs: slices.Clone(refs)},
			&fakeLocation{name: "s3://bucket/prod", refs: slices.Clone(refs[:3])},
		}
	}

	dryLocations := newFixture()
	dryOutcomes, err := applyRetention(context.Background(), dryLocations, policy, true)
	if err != nil {
		t.Fatalf("dry-run applyRetention() = %v, want nil", err)
	}
	for _, location := range dryLocations {
		fake := location.(*fakeLocation)
		if len(fake.attempted) != 0 {
			t.Errorf("dry run attempted deletions at %s: %v, want none", fake.name, fake.attempted)
		}
	}

	realLocations := newFixture()
	realOutcomes, err := applyRetention(context.Background(), realLocations, policy, false)
	if err != nil {
		t.Fatalf("applyRetention() = %v, want nil", err)
	}

	if len(dryOutcomes) != len(realOutcomes) {
		t.Fatalf("dry run produced %d outcomes, real run %d", len(dryOutcomes), len(realOutcomes))
	}
	for i := range dryOutcomes {
		if dryOutcomes[i].Location != realOutcomes[i].Location {
			t.Fatalf("outcome %d: dry-run location %q, real-run location %q", i, dryOutcomes[i].Location, realOutcomes[i].Location)
		}
		if got, want := refNames(dryOutcomes[i].Pruned), refNames(realOutcomes[i].Pruned); !slices.Equal(got, want) {
			t.Errorf("%s: dry-run candidates %v, want the real-run set %v", dryOutcomes[i].Location, got, want)
		}
		if got, want := refNames(dryOutcomes[i].Kept), refNames(realOutcomes[i].Kept); !slices.Equal(got, want) {
			t.Errorf("%s: dry-run kept %v, want the real-run set %v", dryOutcomes[i].Location, got, want)
		}
		if got, want := realLocations[i].(*fakeLocation).deleted, refNames(realOutcomes[i].Pruned); !slices.Equal(got, want) {
			t.Errorf("%s: deleted %v, want exactly the reported set %v", dryOutcomes[i].Location, got, want)
		}
	}

	// The preview is not trivially empty: the fixture really has out-of-policy backups.
	if len(dryOutcomes[0].Pruned) == 0 {
		t.Error("dry run previewed no candidates; the fixture no longer exercises pruning")
	}
}

func TestApplyRetentionNoOpCases(t *testing.T) {
	tests := []struct {
		name     string
		refs     []backupRef
		policy   RetentionPolicy
		wantKept int
	}{
		{name: "empty location", refs: nil, policy: RetentionPolicy{Days: 7}},
		{name: "single backup is the floor", refs: []backupRef{refDaysAgo(40)}, policy: RetentionPolicy{Days: 7, Count: 1}, wantKept: 1},
		{name: "everything within policy", refs: []backupRef{refDaysAgo(1), refDaysAgo(3)}, policy: RetentionPolicy{Days: 7}, wantKept: 2},
		{name: "inactive policy prunes nothing", refs: []backupRef{refDaysAgo(1), refDaysAgo(40)}, policy: RetentionPolicy{}, wantKept: 2},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, dryRun := range []bool{false, true} {
				location := &fakeLocation{name: "local:/backups", refs: slices.Clone(tc.refs)}
				outcomes, err := applyRetention(context.Background(), []storageLocation{location}, tc.policy, dryRun)
				if err != nil {
					t.Fatalf("applyRetention(dryRun=%v) = %v, want nil", dryRun, err)
				}
				if len(location.attempted) != 0 {
					t.Errorf("dryRun=%v: attempted deletions %v, want none", dryRun, location.attempted)
				}
				if len(outcomes) != 1 {
					t.Fatalf("outcomes = %d, want 1", len(outcomes))
				}
				if outcomes[0].Pruned != nil {
					t.Errorf("dryRun=%v: Pruned = %v, want nothing", dryRun, refNames(outcomes[0].Pruned))
				}
				if len(outcomes[0].Kept) != tc.wantKept {
					t.Errorf("dryRun=%v: Kept = %v, want %d ref(s)", dryRun, refNames(outcomes[0].Kept), tc.wantKept)
				}
			}
		})
	}
}

func TestApplyRetentionNoLocations(t *testing.T) {
	outcomes, err := applyRetention(context.Background(), nil, RetentionPolicy{Days: 7}, false)
	if err != nil {
		t.Fatalf("applyRetention() = %v, want nil", err)
	}
	if len(outcomes) != 0 {
		t.Errorf("outcomes = %v, want none", outcomes)
	}
}
