package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"golang.org/x/term"
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

// RetentionConfig carries the retention rules supplied through command-line flags
// or environment variables — the binary's only two configuration channels. Its zero
// value activates nothing.
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
// error. An explicitly requested 0 is rejected by resolveRetentionRule instead: only
// the resolution knows which channel supplied a value, and therefore whether a 0 is
// an operator's request or an omitted rule.
func (p RetentionPolicy) Validate() error {
	if p.Days < 0 {
		return fmt.Errorf("retention-days must be at least 1 when set (got %d); omit it to disable the age rule", p.Days)
	}
	if p.Count < 0 {
		return fmt.Errorf("retention-count must be at least 1 when set (got %d); omit it to disable the count rule", p.Count)
	}
	return nil
}

// Names of the channels that configure the retention rules. The CLI layer registers
// its flags under these names and reads these environment variables, so the
// resolution errors below can quote the exact input an operator has to fix.
const (
	RetentionDaysFlag    = "retention-days"
	RetentionCountFlag   = "retention-count"
	RetentionDaysEnvVar  = "INFRAHUB_RETENTION_DAYS"
	RetentionCountEnvVar = "INFRAHUB_RETENTION_COUNT"
)

// retentionRuleNames is one rule's operator-facing vocabulary: the two channel names
// and the word that names the rule itself in an error message.
type retentionRuleNames struct {
	flag  string
	env   string
	label string
}

var (
	retentionDaysNames  = retentionRuleNames{flag: RetentionDaysFlag, env: RetentionDaysEnvVar, label: "age"}
	retentionCountNames = retentionRuleNames{flag: RetentionCountFlag, env: RetentionCountEnvVar, label: "count"}
)

// RetentionRuleInput is one retention rule as the CLI layer observed it, before any
// precedence or validation: the value the invoked command's own flag carries when the
// operator set it, and the environment variable's raw text when it is present.
//
// The environment value stays unparsed on purpose. A value the configuration library
// coerces to an integer cannot be told apart from an unset rule afterwards, and 0
// means "rule inactive" — so a mistyped variable would silently switch retention off
// on exactly the unattended runs that depend on it.
type RetentionRuleInput struct {
	// FlagValue is the flag's parsed value; it is only meaningful when FlagSet is true.
	FlagValue int
	// FlagSet reports whether the operator passed the flag on the invoked command.
	FlagSet bool
	// EnvValue is the environment variable's raw text, exactly as the operator set it.
	EnvValue string
	// EnvSet reports whether the environment variable is present at all. A variable
	// present with an empty value resolves as if it were absent, because that is what
	// an unsubstituted Compose or Kubernetes variable looks like.
	EnvSet bool
}

// RetentionInputs carries both rules' raw inputs for one invocation.
type RetentionInputs struct {
	Days  RetentionRuleInput
	Count RetentionRuleInput
}

// ResolveRetentionConfig applies the precedence and validation of both retention
// rules. It is the single place that decides what an operator's input means, and it
// runs before any backup or prune work so an unusable rule aborts the run instead of
// quietly resolving to "no retention" (FR-001, FR-011).
func ResolveRetentionConfig(inputs RetentionInputs) (RetentionConfig, error) {
	days, err := resolveRetentionRule(retentionDaysNames, inputs.Days)
	if err != nil {
		return RetentionConfig{}, err
	}

	count, err := resolveRetentionRule(retentionCountNames, inputs.Count)
	if err != nil {
		return RetentionConfig{}, err
	}

	config := RetentionConfig{Days: days, Count: count}
	// Defence in depth: every channel above already rejects anything below 1, so this
	// can only fire if a new channel is ever added without its own validation.
	if err := config.Policy().Validate(); err != nil {
		return RetentionConfig{}, err
	}

	return config, nil
}

// resolveRetentionRule resolves one rule from the channels that can supply it.
//
// The flag wins whenever the operator set it on the invoked command, the environment
// variable answers otherwise, and a rule no channel supplied stays inactive. Whatever
// a channel does supply must be a whole number of at least 1: an explicit 0, a
// negative, a fraction, or anything non-numeric is a configuration error, never a
// request to disable the rule. Silence is the one outcome an operator who configured
// retention must never get.
//
// The exception is an environment variable that is present but empty, which is a rule
// nobody configured rather than a value someone got wrong: `INFRAHUB_RETENTION_DAYS=${RETENTION_DAYS}`
// with nothing to substitute is how Docker Compose and Kubernetes render an unset
// variable, and failing those deployments would be reading an intent into them that
// their operators never expressed.
func resolveRetentionRule(names retentionRuleNames, input RetentionRuleInput) (int, error) {
	if input.FlagSet {
		if input.FlagValue < 1 {
			return 0, fmt.Errorf("--%s must be at least 1 when set; omit it to disable the %s rule", names.flag, names.label)
		}

		return input.FlagValue, nil
	}

	if !input.EnvSet {
		return 0, nil
	}

	// Trimmed first so that a value carrying stray whitespace is read as the number
	// the operator meant, and an empty or whitespace-only value as no value at all.
	raw := strings.TrimSpace(input.EnvValue)
	if raw == "" {
		return 0, nil
	}

	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 {
		return 0, fmt.Errorf("%s must be a whole number of at least 1 (got %q); omit it to disable the %s rule", names.env, input.EnvValue, names.label)
	}

	return value, nil
}

// backupRef is one backup discovered at a storage location, identified by the base
// name of its archive.
//
// The name is the ref's only state. The creation time retention ranks and ages by is
// derived from that name whenever it is needed (createdAt) rather than stored beside
// it, so no ref — however it was built — can carry a timestamp that disagrees with
// the archive a deletion would remove.
type backupRef struct {
	// Name is the archive's base name; it doubles as the deletion key.
	Name string
}

// backupTimestamp reads the creation time a backup archive name embeds. It reports
// false for any name that does not match the backup pattern and for names whose
// timestamp is not a real point in time (for example month 13), keeping both
// invisible to retention.
func backupTimestamp(name string) (time.Time, bool) {
	match := backupNamePattern.FindStringSubmatch(name)
	if match == nil {
		return time.Time{}, false
	}

	// generateBackupFilename formats the timestamp in host-local time, so age
	// must be measured against the same location.
	createdAt, err := time.ParseInLocation(backupNameTimestampLayout, match[1], time.Local)
	if err != nil {
		return time.Time{}, false
	}

	return createdAt, true
}

// parseBackupName recognizes a backup archive by its base name, which is what turns a
// listed file or object into a retention candidate (FR-006).
func parseBackupName(name string) (backupRef, bool) {
	if _, ok := backupTimestamp(name); !ok {
		return backupRef{}, false
	}

	return backupRef{Name: name}, true
}

// createdAt is the ref's creation time in host-local time, read from its name. A name
// that is not a backup archive has no creation time and reports the zero instant,
// which ranks it as the oldest backup at its location; it still cannot be deleted,
// because both Delete implementations re-check the name.
func (r backupRef) createdAt() time.Time {
	createdAt, _ := backupTimestamp(r.Name)

	return createdAt
}

// sortBackupRefsNewestFirst orders refs by creation time descending, breaking
// ties by name descending. The tiebreak keeps the order — and therefore the
// count rule — deterministic when several archives share a timestamp.
func sortBackupRefsNewestFirst(refs []backupRef) {
	slices.SortFunc(refs, func(a, b backupRef) int {
		if byTime := b.createdAt().Compare(a.createdAt()); byTime != 0 {
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
	if policy.Days >= 1 && now.Sub(ref.createdAt()) < time.Duration(policy.Days)*retentionDay {
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
	// operation context. A backup that is already absent is not a failure: the
	// state the deletion asked for holds, which keeps a backup someone else
	// removed between planning and deletion from failing the run.
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
		// An archive that is already gone is the outcome this deletion asked for.
		// Something else removed it — a concurrent prune, an operator, a cleanup
		// script — so the leg reports success rather than a failure nobody can act on.
		if errors.Is(err, fs.ErrNotExist) {
			logrus.Debugf("Backup %s was already gone from %s", ref.Name, l.Name())
			return nil
		}

		return fmt.Errorf("failed to delete backup %s: %w", path, err)
	}

	return nil
}

// s3Backend is the slice of S3Client an s3Location depends on: the filtered listing,
// the key an archive's base name maps to, the deletion of that key, and the
// bucket/prefix rendering used in logs and errors.
//
// The location depends on this interface rather than on the concrete client so the
// wiring between them — above all the key reconstruction Delete performs — can be
// exercised without a bucket. Passing a base name where the full key belongs is
// silently successful against real S3, so that line has to be testable.
type s3Backend interface {
	List(ctx context.Context) ([]backupRef, error)
	Delete(ctx context.Context, key string) error
	buildS3Key(name string) string
	locationName() string
}

// s3Location is the configured S3 bucket and prefix, reached through the shared
// S3Client. It is a thin adapter: key filtering and the context bounds live with
// the client, so both locations agree on what counts as a backup.
type s3Location struct {
	client s3Backend
}

func newS3Location(client s3Backend) *s3Location {
	return &s3Location{client: client}
}

// Name renders the location as the bucket and prefix the operator configured.
func (l *s3Location) Name() string {
	return l.client.locationName()
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
//
// An object that is already gone needs no special handling here: deleting a key that
// does not exist is a success in the S3 API, which is the behavior the storageLocation
// contract asks for.
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

// retentionMode says whether a retention run deletes anything. The two answers are
// named constants rather than a bool because at a call site that can delete backups a
// flipped literal is silent data loss no compiler catches. The zero value is the
// harmless one.
type retentionMode int

const (
	// retentionDryRun reports exactly what retentionExecute would delete, and deletes
	// nothing.
	retentionDryRun retentionMode = iota
	// retentionExecute deletes every candidate the policy did not keep.
	retentionExecute
)

// pruneOutcome is what retention did at one storage location — and, before a plan is
// executed, what it would do. It is what callers report to the operator, and it
// carries the leg's error rather than letting a failure hide behind a successful
// sibling leg.
//
// Candidates and Deleted are separate fields on purpose. On a destructive path, a
// single count that means "would delete" in one mode and "did delete" in another
// cannot be read correctly at a call site that does not know which mode produced it.
type pruneOutcome struct {
	// Location is the storageLocation's Name().
	Location string
	// Kept are the refs retention retained, whether by a rule or by the floor.
	Kept []backupRef
	// Candidates are the refs the policy did not keep: exactly what a real run
	// deletes. Planning fills it and executing leaves it alone, so a preview and the
	// run that follows it report the same set (SC-004).
	Candidates []backupRef
	// Deleted are the refs that actually went away. It is empty until a plan is
	// executed — a dry run never fills it — and is a strict subset of Candidates when
	// a deletion failed.
	Deleted []backupRef
	// Err is the joined error for this leg; nil when the leg fully succeeded.
	Err error
}

// retentionPlanEntry is one location's planned retention, bound to the location that
// will carry it out. Keeping the selection and its location in a single value is what
// makes executing a plan against the wrong place structurally impossible: there is no
// parallel slice to keep in step.
type retentionPlanEntry struct {
	location storageLocation
	// outcome carries the location's name, the refs the policy keeps, and the
	// Candidates the plan intends to delete. Executing the entry adds the refs that
	// actually went away as Deleted.
	outcome pruneOutcome
}

// planRetention lists every location and selects what the policy does not keep,
// without deleting anything.
//
// It is the only place a selection is computed. One evaluation instant is shared by
// every location so ages cannot drift between legs, and both the preview and the
// deletions are derived from this single plan, which is what makes the set an operator
// is shown the same set that is deleted (SC-004).
//
// A location that cannot be listed contributes an entry carrying that leg's error and
// no candidates; every other location is still planned (FR-008). An inactive policy
// selects nothing, because selectPrunable claims every backup for keeping; callers
// still gate on Active() per FR-004.
func planRetention(ctx context.Context, locations []storageLocation, policy RetentionPolicy) ([]retentionPlanEntry, error) {
	now := time.Now()

	plan := make([]retentionPlanEntry, 0, len(locations))
	var legErrs []error

	for _, location := range locations {
		entry := retentionPlanEntry{location: location, outcome: pruneOutcome{Location: location.Name()}}

		refs, err := location.List(ctx)
		if err != nil {
			entry.outcome.Err = fmt.Errorf("retention failed at %s: %w", entry.outcome.Location, err)
			legErrs = append(legErrs, entry.outcome.Err)
			plan = append(plan, entry)
			continue
		}

		entry.outcome.Kept, entry.outcome.Candidates = selectPrunable(refs, policy, now)
		plan = append(plan, entry)
	}

	return plan, errors.Join(legErrs...)
}

// executeRetentionPlan deletes exactly the refs the plan selected, each at the
// location its entry is bound to. Nothing is listed again and the policy is not
// re-evaluated, so a backup that arrived — or aged past a rule — after the plan was
// made is neither deleted nor spared retroactively.
//
// Failure handling is best-effort in both dimensions: every entry is executed even
// after an earlier one failed, and within an entry every candidate is attempted even
// after an individual deletion failed, so one undeletable backup cannot strand the
// rest of the reclaimable space (FR-008). Errors are collected and reported together.
//
// An entry whose planning failed is passed through unchanged: it has nothing to
// delete, and its error travels with the returned outcomes so callers report it
// without joining the planning error separately.
func executeRetentionPlan(ctx context.Context, plan []retentionPlanEntry) ([]pruneOutcome, error) {
	outcomes := make([]pruneOutcome, 0, len(plan))
	var legErrs []error

	for _, entry := range plan {
		outcome := entry.execute(ctx)
		outcomes = append(outcomes, outcome)
		if outcome.Err != nil {
			legErrs = append(legErrs, outcome.Err)
		}
	}

	return outcomes, errors.Join(legErrs...)
}

// execute deletes this entry's planned refs and reports what actually went away. It
// never returns an error directly: the leg's failures belong to its outcome so the
// caller can continue with the remaining legs and still report everything.
func (e retentionPlanEntry) execute(ctx context.Context) pruneOutcome {
	if e.outcome.Err != nil {
		return e.outcome
	}

	// The planned outcome travels forward unchanged, so the candidate set the operator
	// was shown survives execution and only Deleted grows.
	outcome := e.outcome

	var deleteErrs []error
	for _, ref := range e.outcome.Candidates {
		if err := e.location.Delete(ctx, ref); err != nil {
			// Best-effort: surface the failure now and keep working through the
			// remaining candidates.
			logrus.Warnf("Failed to prune backup %s from %s: %v", ref.Name, outcome.Location, err)
			deleteErrs = append(deleteErrs, err)
			continue
		}

		logrus.Infof("Pruned backup %s from %s", ref.Name, outcome.Location)
		outcome.Deleted = append(outcome.Deleted, ref)
	}

	if len(deleteErrs) > 0 {
		outcome.Err = fmt.Errorf("retention failed at %s: %w", outcome.Location, errors.Join(deleteErrs...))
	}

	return outcome
}

// planOutcomes projects a plan into the outcomes callers report and confirm.
func planOutcomes(plan []retentionPlanEntry) []pruneOutcome {
	outcomes := make([]pruneOutcome, 0, len(plan))
	for _, entry := range plan {
		outcomes = append(outcomes, entry.outcome)
	}

	return outcomes
}

// reportRetentionPlan logs one summary line per location. Every path reports its plan
// exactly once — dry run, confirmation preview, `--force`, and `create` — so the
// automatic, unattended runs account for what they touched at the level an operator
// sees by default, and a dry run does not report the same numbers twice.
func reportRetentionPlan(plan []retentionPlanEntry) {
	for _, entry := range plan {
		logrus.Infof("Retention at %s: %d backup(s) kept, %d candidate(s) to prune",
			entry.outcome.Location, len(entry.outcome.Kept), len(entry.outcome.Candidates))
	}
}

// reportRetentionCandidates names every backup a plan would delete, before anything is
// deleted. Only the paths that show the operator what is about to happen call it: the
// dry-run listing and the list the confirmation prompt refers to (FR-009). A dry run
// marks its lines, because for a dry run this listing is the whole result.
func reportRetentionCandidates(plan []retentionPlanEntry, mode retentionMode) {
	for _, entry := range plan {
		for _, ref := range entry.outcome.Candidates {
			if mode == retentionDryRun {
				logrus.Infof("Would prune backup %s from %s (dry run)", ref.Name, entry.outcome.Location)
				continue
			}

			logrus.Infof("Would prune backup %s from %s", ref.Name, entry.outcome.Location)
		}
	}
}

// applyRetention evaluates the policy at every location and, in retentionExecute mode,
// removes what it does not keep. It returns one outcome per location plus the joined
// errors of the legs that failed, and is the whole operation for the paths that do not
// ask anything: `create`, `prune --force`, and `prune --dry-run`.
//
// In retentionDryRun mode nothing is deleted: every outcome reports the set a real run
// would delete as its Candidates and nothing as Deleted (SC-004). Both answers come
// from the same planning code, not from two independent evaluations.
func applyRetention(ctx context.Context, locations []storageLocation, policy RetentionPolicy, mode retentionMode) ([]pruneOutcome, error) {
	plan, planErr := planRetention(ctx, locations, policy)

	if mode == retentionDryRun {
		reportRetentionCandidates(plan, mode)
		reportRetentionPlan(plan)

		return planOutcomes(plan), planErr
	}

	reportRetentionPlan(plan)

	// executeRetentionPlan carries every planning failure forward in its outcomes and
	// its error, so planErr must not be joined in again.
	return executeRetentionPlan(ctx, plan)
}

// ErrPruneNonInteractive is returned when `prune` needs a confirmation but stdin
// is not a terminal and the operator did not waive the prompt. Refusing is the
// only safe answer: a pipeline that cannot answer must not have its backups
// deleted on the strength of a prompt nobody read.
var ErrPruneNonInteractive = errors.New("cannot prompt for confirmation: stdin is not a terminal; re-run with --force to prune non-interactively")

// confirmFunc decides whether the previewed deletions may proceed. It receives the
// preview outcomes — one per location — so an implementation can report both what
// and where, and it reports an error only when no answer can be obtained at all.
type confirmFunc func(candidates []pruneOutcome) (bool, error)

// pruneIO is the terminal the default confirmation talks to. It travels in the
// invocation's PruneOptions rather than in package state, so one run's terminal is
// never visible to another.
type pruneIO struct {
	// in is where the answer is read from.
	in io.Reader
	// isTTY reports whether in is an interactive terminal.
	isTTY func() bool
	// out is where the question is written.
	out io.Writer
}

// withDefaults fills every seam the caller left unset with the process's own terminal,
// which is what every caller outside this package's tests wants.
func (p pruneIO) withDefaults() pruneIO {
	if p.in == nil {
		p.in = os.Stdin
	}
	if p.isTTY == nil {
		p.isTTY = defaultPruneStdinIsTTY
	}
	if p.out == nil {
		p.out = os.Stdout
	}

	return p
}

func defaultPruneStdinIsTTY() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// confirmPrune asks the single y/N question that guards a real prune. The candidate
// list has already been reported, so the question only has to name the scale of what
// is about to happen. Anything other than y/yes — including an empty line or a stdin
// that ends without an answer — declines.
//
// A stdin that cannot be read at all is not a decline: reporting an I/O failure as
// "the operator said no" would hide a broken pipeline behind a success. Only the
// end of input is read as an answer.
//
// The question is written to the terminal rather than logged: it must appear whatever
// the log level is, and it must not carry a newline before the answer is typed.
func (p pruneIO) confirmPrune(candidates []pruneOutcome) (bool, error) {
	if !p.isTTY() {
		return false, ErrPruneNonInteractive
	}

	fmt.Fprintf(p.out, "Prune %d backup(s) listed above? [y/N]: ", countPruneCandidates(candidates))

	line, err := bufio.NewReader(p.in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("failed to read the confirmation answer: %w", err)
	}

	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}

// countPruneCandidates totals the candidates across every previewed location: what a
// real run would delete.
func countPruneCandidates(outcomes []pruneOutcome) int {
	total := 0
	for _, outcome := range outcomes {
		total += len(outcome.Candidates)
	}

	return total
}

// countPrunedBackups totals the backups that actually went away across every executed
// location.
func countPrunedBackups(outcomes []pruneOutcome) int {
	total := 0
	for _, outcome := range outcomes {
		total += len(outcome.Deleted)
	}

	return total
}

// PruneOptions carries the per-invocation switches of the standalone `prune`
// command. They are deliberately not configurable through environment variables: a
// persistent Force would silently disarm the confirmation that makes pruning safe, and
// a persistent S3 would delete objects the operator only configured for upload.
type PruneOptions struct {
	// DryRun reports exactly what a real run would delete and deletes nothing.
	DryRun bool
	// Force waives the confirmation prompt for scripted use.
	Force bool
	// S3 adds the configured bucket/prefix as a pruned location. Without it S3 is
	// never touched, however complete the S3 configuration is (FR-007).
	S3 bool

	// confirm is asked once, with the preview, before anything is deleted; unset means
	// the y/N question on the process's terminal.
	//
	// It and io are unexported so the cmd layer constructs these options from the
	// three switches above and nothing else, while a test drives the accept, decline,
	// and non-interactive paths on its own options value — an isolation that holds by
	// construction rather than by no test running in parallel.
	confirm confirmFunc
	// io is the terminal the default confirmation talks to.
	io pruneIO
}

// withDefaults fills in the confirmation seams the caller left unset, which is all of
// them for every caller outside this package's tests.
func (o PruneOptions) withDefaults() PruneOptions {
	o.io = o.io.withDefaults()
	if o.confirm == nil {
		o.confirm = o.io.confirmPrune
	}

	return o
}

// validatePruneRequest rejects a prune that cannot mean what it says, before any
// location is listed and therefore before anything can be deleted.
//
// Validate runs first so a negative rule is reported as the bad value it is rather
// than as a missing rule; an inactive policy then fails because retention is the
// command's only job (FR-004 lets `create` skip silently, `prune` cannot).
func validatePruneRequest(cfg *Configuration, policy RetentionPolicy, opts PruneOptions) error {
	if err := policy.Validate(); err != nil {
		return err
	}

	if !policy.Active() {
		return errors.New("at least one retention rule is required: pass --retention-days and/or --retention-count")
	}

	// The Plakar backend has no retention implementation yet. `prune` exists only to
	// apply retention, so it fails outright instead of doing nothing quietly (FR-012).
	if cfg.Backend == BackendPlakar {
		return fmt.Errorf("retention is not yet supported for the %s backend; nothing was pruned", cfg.Backend)
	}

	if opts.DryRun && opts.Force {
		return errors.New("--dry-run and --force are contradictory: --dry-run never deletes and never prompts, so there is nothing to force")
	}

	return nil
}

// retentionLegsForPrune returns the storage locations a `prune` run evaluates.
//
// The local backup directory is always a leg — pruning it is the point of the
// command. S3 is a leg only when the operator asked for it explicitly, which is the
// difference from `create`, where the leg follows this run's upload (FR-007).
func retentionLegsForPrune(cfg *Configuration, includeS3 bool) ([]storageLocation, error) {
	legs := []storageLocation{newLocalLocation(cfg.BackupDir)}
	if !includeS3 {
		return legs, nil
	}

	if err := cfg.S3.ValidateConfig(); err != nil {
		return nil, err
	}

	client, err := NewS3Client(cfg.S3)
	if err != nil {
		return nil, fmt.Errorf("failed to create S3 client for retention: %w", err)
	}

	return append(legs, newS3Location(client)), nil
}

// Prune applies the retention policy on demand, without taking a backup first.
//
// It validates the request, resolves which locations the run touches, and hands both
// to pruneLocations, which owns the confirmation contract.
func (iops *InfrahubOps) Prune(policy RetentionPolicy, opts PruneOptions) error {
	if err := validatePruneRequest(iops.config, policy, opts); err != nil {
		return err
	}

	legs, err := retentionLegsForPrune(iops.config, opts.S3)
	if err != nil {
		return err
	}

	return pruneLocations(context.Background(), legs, policy, opts)
}

// pruneLocations applies the policy to already-resolved locations under `prune`'s
// confirmation contract: `--dry-run` previews and stops, `--force` deletes without
// asking, and an interactive run previews, asks once, and treats anything but yes as a
// decline. A declined run is a success — the operator got what they asked for — so it
// returns nil.
//
// The interactive path plans once and executes that same plan, so the set the operator
// confirmed is exactly the set that is deleted: nothing is listed again and no fresh
// evaluation instant is taken between the question and the deletions (SC-004).
//
// Failures are reported as the plan and execution phases aggregate them: every leg is
// attempted and the per-leg errors, already qualified with their location, are returned
// as they are.
func pruneLocations(ctx context.Context, legs []storageLocation, policy RetentionPolicy, opts PruneOptions) error {
	opts = opts.withDefaults()

	logrus.Infof("Applying retention policy (days: %d, count: %d) to %d location(s)", policy.Days, policy.Count, len(legs))

	if opts.DryRun {
		_, err := applyRetention(ctx, legs, policy, retentionDryRun)
		return err
	}

	if opts.Force {
		// The operator waived the preview; every deletion is still reported as it
		// happens, so the run is auditable after the fact.
		_, err := applyRetention(ctx, legs, policy, retentionExecute)
		return err
	}

	plan, planErr := planRetention(ctx, legs, policy)
	reportRetentionCandidates(plan, retentionExecute)
	reportRetentionPlan(plan)

	// A plan that could not be completed cannot be confirmed: the operator would be
	// answering for a set the tool does not fully know. Refuse rather than delete the
	// part that happened to list successfully — a mistyped --backup-dir or an S3 leg
	// that cannot be listed must not read as "nothing to prune". `--force` (above)
	// keeps the attempt-every-leg behavior for scripted runs.
	if planErr != nil {
		return planErr
	}

	candidates := planOutcomes(plan)
	if countPruneCandidates(candidates) == 0 {
		logrus.Info("Nothing to prune: the retention policy claims every backup found")
		return nil
	}

	confirmed, err := opts.confirm(candidates)
	if err != nil {
		return err
	}
	if !confirmed {
		logrus.Info("Prune aborted; no backups were deleted")
		return nil
	}

	outcomes, err := executeRetentionPlan(ctx, plan)
	logrus.Infof("Pruned %d of %d confirmed backup(s)", countPrunedBackups(outcomes), countPruneCandidates(candidates))

	return err
}
