package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// retentionDay is the duration of one day for the age rule. Day-granularity
// retention deliberately tolerates DST shifts and timezone skew between hosts.
const retentionDay = 24 * time.Hour

// backupNameTimestampLayout is the layout of the timestamp that
// generateBackupFilename embeds in every backup archive name, in host-local time.
const backupNameTimestampLayout = "20060102_150405"

// backupNamePattern matches the base name of a backup archive, plain or
// encrypted. It is anchored at both ends and only ever applied to base names, so
// a name carrying a path separator can never match, and files that merely look
// like backups stay invisible to retention.
var backupNamePattern = regexp.MustCompile(`^infrahub_backup_(\d{8}_\d{6})\.tar\.gz(\.enc)?$`)

// RetentionConfig carries the retention rules supplied through command-line
// flags, environment variables, or the configuration file. Its zero value
// activates nothing.
type RetentionConfig struct {
	Days  int
	Count int
}

// Policy converts configured rules into the policy evaluated at a storage location.
func (c RetentionConfig) Policy() RetentionPolicy {
	return RetentionPolicy(c)
}

// RetentionPolicy is the pair of optional retention rules for one invocation.
// It is ephemeral: no retention state is persisted between runs. A field left at
// 0 means that rule is inactive.
type RetentionPolicy struct {
	// Days keeps backups whose age is strictly less than Days days.
	Days int
	// Count keeps the Count most recent backups at a storage location.
	Count int
}

// Active reports whether at least one rule is set. `create` skips retention
// entirely when the policy is inactive; `prune` refuses to run.
func (p RetentionPolicy) Active() bool {
	return p.Days >= 1 || p.Count >= 1
}

// Validate rejects rule values that cannot express a retention rule. A rule left
// at 0 is inactive and therefore valid; a negative value is a configuration
// error. Explicitly passing 0 on the command line is rejected where the flag is
// parsed, which is the only layer that can tell an explicit 0 from an unset flag.
func (p RetentionPolicy) Validate() error {
	if p.Days < 0 {
		return fmt.Errorf("retention-days must be at least 1 when set (got %d); omit it to disable the age rule", p.Days)
	}
	if p.Count < 0 {
		return fmt.Errorf("retention-count must be at least 1 when set (got %d); omit it to disable the count rule", p.Count)
	}
	return nil
}

// backupRef is one backup discovered at a storage location. Refs are only ever
// created by parseBackupName, so anything that is not a backup archive cannot
// become a deletion candidate.
type backupRef struct {
	// Name is the archive's base name; it doubles as the deletion key.
	Name string
	// CreatedAt is the timestamp embedded in Name, in host-local time.
	CreatedAt time.Time
}

// parseBackupName recognizes a backup archive by its base name and derives its
// creation time from the embedded timestamp. It reports false for any name that
// does not match the backup pattern and for names whose timestamp is not a real
// point in time (for example month 13), keeping both invisible to retention.
func parseBackupName(name string) (backupRef, bool) {
	match := backupNamePattern.FindStringSubmatch(name)
	if match == nil {
		return backupRef{}, false
	}

	// generateBackupFilename formats the timestamp in host-local time, so age
	// must be measured against the same location.
	createdAt, err := time.ParseInLocation(backupNameTimestampLayout, match[1], time.Local)
	if err != nil {
		return backupRef{}, false
	}

	return backupRef{Name: name, CreatedAt: createdAt}, true
}

// sortBackupRefsNewestFirst orders refs by creation time descending, breaking
// ties by name descending. The tiebreak keeps the order — and therefore the
// count rule — deterministic when several archives share a timestamp.
func sortBackupRefsNewestFirst(refs []backupRef) {
	slices.SortFunc(refs, func(a, b backupRef) int {
		if byTime := b.CreatedAt.Compare(a.CreatedAt); byTime != 0 {
			return byTime
		}
		return strings.Compare(b.Name, a.Name)
	})
}

// retentionKeeps reports whether the backup at the given recency rank (0 being
// the most recent at the location) survives the policy.
func retentionKeeps(ref backupRef, rank int, policy RetentionPolicy, now time.Time) bool {
	// The floor: the most recent backup at a location always survives, whatever
	// the rules say. There is no override.
	if rank == 0 {
		return true
	}
	if policy.Days >= 1 && now.Sub(ref.CreatedAt) < time.Duration(policy.Days)*retentionDay {
		return true
	}
	// rank is 0-based, so rank < Count means the ref is among the Count newest.
	return policy.Count >= 1 && rank < policy.Count
}

// selectPrunable splits the backups found at one storage location into those
// retention keeps and those it may delete. It is pure: the input slice is left
// untouched and "now" is supplied by the caller.
//
// A backup is kept when either active rule claims it (union semantics), and the
// most recent backup is kept even when every rule would prune it. The floor
// lives here rather than in callers so every location, command, and future
// backend inherits it.
func selectPrunable(refs []backupRef, policy RetentionPolicy, now time.Time) (keep, prune []backupRef) {
	ordered := slices.Clone(refs)
	sortBackupRefsNewestFirst(ordered)

	// An inactive policy claims nothing, so evaluating it would prune every
	// backup but the newest. Callers gate on Active() first; returning early
	// keeps that mistake harmless instead of destructive.
	if !policy.Active() {
		return ordered, nil
	}

	for rank, ref := range ordered {
		if retentionKeeps(ref, rank, policy, now) {
			keep = append(keep, ref)
			continue
		}
		prune = append(prune, ref)
	}

	return keep, prune
}

// storageLocation is a place where backups accumulate. The policy is evaluated
// independently per location, with the keep-newest floor applied per location.
type storageLocation interface {
	// Name returns a human-readable label used in logs and error messages.
	Name() string
	// List returns every backup currently present at the location. An empty
	// location returns an empty slice and no error.
	List(ctx context.Context) ([]backupRef, error)
	// Delete removes exactly the given backup, wrapping failures with the
	// operation context.
	Delete(ctx context.Context, ref backupRef) error
}

// localLocation is the local backup directory.
type localLocation struct {
	dir string
}

func newLocalLocation(dir string) *localLocation {
	return &localLocation{dir: dir}
}

func (l *localLocation) Name() string {
	return "local:" + l.dir
}

// List returns the backup archives in the directory. Directories and names that
// are not backups are skipped. A missing directory is reported as an error
// rather than as an empty location, so a mistyped backup directory cannot be
// mistaken for "nothing to prune".
func (l *localLocation) List(_ context.Context) ([]backupRef, error) {
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return nil, fmt.Errorf("failed to list backups in %s: %w", l.dir, err)
	}

	refs := make([]backupRef, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if ref, ok := parseBackupName(entry.Name()); ok {
			refs = append(refs, ref)
		}
	}

	return refs, nil
}

// Delete removes one backup archive from the directory. The name is re-checked
// against the backup pattern so that a ref which did not come from List can
// never reach an unrelated file — matching names cannot contain a path separator.
func (l *localLocation) Delete(_ context.Context, ref backupRef) error {
	if _, ok := parseBackupName(ref.Name); !ok {
		return fmt.Errorf("refusing to delete %q from %s: not a backup archive name", ref.Name, l.dir)
	}

	path := filepath.Join(l.dir, ref.Name)
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("failed to delete backup %s: %w", path, err)
	}

	return nil
}

// s3Location is the configured S3 bucket and prefix, reached through the shared
// S3Client. It is a thin adapter: key filtering and the context bounds live with
// the client, so both locations agree on what counts as a backup.
type s3Location struct {
	client *S3Client
}

func newS3Location(client *S3Client) *s3Location {
	return &s3Location{client: client}
}

// Name renders the location as the bucket and prefix the operator configured.
func (l *s3Location) Name() string {
	if prefix := strings.TrimSuffix(l.client.config.Prefix, "/"); prefix != "" {
		return "s3://" + l.client.config.Bucket + "/" + prefix
	}
	return "s3://" + l.client.config.Bucket
}

// List returns the backup objects under the configured prefix. An empty prefix
// yields an empty slice and no error.
func (l *s3Location) List(ctx context.Context) ([]backupRef, error) {
	return l.client.List(ctx)
}

// Delete removes one backup object. The ref carries only a base name, so the full
// key is rebuilt the same way uploads build it; the name is re-checked against the
// backup pattern first so a ref which did not come from List can never reach an
// unrelated object.
func (l *s3Location) Delete(ctx context.Context, ref backupRef) error {
	if _, ok := parseBackupName(ref.Name); !ok {
		return fmt.Errorf("refusing to delete %q from %s: not a backup archive name", ref.Name, l.Name())
	}

	return l.client.Delete(ctx, l.client.buildS3Key(ref.Name))
}

var (
	_ storageLocation = (*localLocation)(nil)
	_ storageLocation = (*s3Location)(nil)
)

// pruneOutcome is the result of applying the policy at one storage location. It is
// what callers report to the operator, and it carries the leg's error rather than
// letting a failure hide behind a successful sibling leg.
type pruneOutcome struct {
	// Location is the storageLocation's Name().
	Location string
	// Kept are the refs retention retained, whether by a rule or by the floor.
	Kept []backupRef
	// Pruned are the refs deleted — or, in dry-run, exactly the refs a real run
	// would delete.
	Pruned []backupRef
	// Err is the joined error for this leg; nil when the leg fully succeeded.
	Err error
}

// applyRetention evaluates the policy at every location and removes what it does
// not keep, returning one outcome per location plus the joined errors of the legs
// that failed.
//
// Failure handling is best-effort in both dimensions: every location is visited
// even after an earlier one failed, and within a location every candidate is
// attempted even after an individual deletion failed, so one undeletable backup
// cannot strand the rest of the reclaimable space (FR-008). Errors are collected
// and reported together instead of aborting the run at the first failure.
//
// With dryRun set nothing is deleted and Pruned reports exactly the set a real run
// would delete (SC-004).
//
// An inactive policy prunes nothing here, because selectPrunable claims every
// backup for keeping; callers still gate on Active() per FR-004.
func applyRetention(ctx context.Context, locations []storageLocation, policy RetentionPolicy, dryRun bool) ([]pruneOutcome, error) {
	// One evaluation instant for every location, so ages cannot drift between legs.
	now := time.Now()

	outcomes := make([]pruneOutcome, 0, len(locations))
	var legErrs []error

	for _, location := range locations {
		outcome := pruneAtLocation(ctx, location, policy, now, dryRun)
		outcomes = append(outcomes, outcome)
		if outcome.Err != nil {
			legErrs = append(legErrs, outcome.Err)
		}
	}

	return outcomes, errors.Join(legErrs...)
}

// pruneAtLocation applies the policy at a single location. It never returns an
// error directly: the leg's failures belong to its outcome so the caller can
// continue with the remaining legs and still report everything.
func pruneAtLocation(ctx context.Context, location storageLocation, policy RetentionPolicy, now time.Time, dryRun bool) pruneOutcome {
	name := location.Name()
	outcome := pruneOutcome{Location: name}

	refs, err := location.List(ctx)
	if err != nil {
		outcome.Err = fmt.Errorf("retention failed at %s: %w", name, err)
		return outcome
	}

	keep, prune := selectPrunable(refs, policy, now)
	outcome.Kept = keep
	logrus.Debugf("Retention at %s: %d backup(s) kept, %d candidate(s) to prune", name, len(keep), len(prune))

	var deleteErrs []error
	for _, ref := range prune {
		if dryRun {
			logrus.Infof("Would prune backup %s from %s (dry run)", ref.Name, name)
			outcome.Pruned = append(outcome.Pruned, ref)
			continue
		}

		if err := location.Delete(ctx, ref); err != nil {
			// Best-effort: surface the failure now and keep working through the
			// remaining candidates.
			logrus.Warnf("Failed to prune backup %s from %s: %v", ref.Name, name, err)
			deleteErrs = append(deleteErrs, err)
			continue
		}

		logrus.Infof("Pruned backup %s from %s", ref.Name, name)
		outcome.Pruned = append(outcome.Pruned, ref)
	}

	if len(deleteErrs) > 0 {
		outcome.Err = fmt.Errorf("retention failed at %s: %w", name, errors.Join(deleteErrs...))
	}

	return outcome
}
