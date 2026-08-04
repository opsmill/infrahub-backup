# Research: Backup Retention Policy

**Feature**: `specs/004-backup-retention-policy` | **Date**: 2026-08-04

All Technical Context entries were resolvable from the codebase and prior specs; no
NEEDS CLARIFICATION markers remained after specify. This document records the decisions
that shape the design, with rationale and rejected alternatives.

## R1. Backup identity and age source

**Decision**: A prunable backup is a file/object whose base name matches
`^infrahub_backup_(\d{8}_\d{6})\.tar\.gz(\.enc)?$` and whose captured timestamp parses
with layout `20060102_150405` (host-local time, matching `generateBackupFilename` in
`src/internal/app/backup_metadata.go:126`). Age is derived exclusively from that
embedded timestamp. Anything else — including near-miss names with unparseable
timestamps — is invisible to retention.

**Rationale**: The filename timestamp is written by the tool itself at creation and
survives copies, downloads (`downloadBackupFromS3` preserves the base name), and
re-uploads. Encrypted archives (`.enc`) cannot be inspected, so name-based selection is
the only rule that treats plain and encrypted backups identically (FR-006).

**Alternatives considered**:
- *File mtime / S3 LastModified*: rejected — mutated by copies, restores from S3, and
  filesystem migrations; would make a freshly downloaded 30-day-old backup look new.
- *Embedded `backup_information.json` metadata*: rejected — requires opening every
  archive (slow) and is impossible for encrypted archives without the private key.

## R2. Selection semantics (union + floor) as a pure function

**Decision**: `selectPrunable(refs []backupRef, policy RetentionPolicy, now time.Time) (keep, prune []backupRef)`
— a pure function over already-parsed references. A ref is kept if
(days rule active AND age < days) OR (count rule active AND rank-by-recency ≤ count).
The newest ref at the location is always forced into `keep` (the floor), implemented
inside the selection function so every caller — create-integrated, prune, dry-run,
every backend — inherits it and it is table-testable with synthetic `now` values.

**Rationale**: Union semantics match the established borg/restic `--keep-*` convention
and make the "schedule stalled" footgun self-healing (FR-002, FR-003). Putting the
floor inside selection (not in callers) makes SC-003 an invariant provable by unit
test rather than a property of call discipline.

**Alternatives considered**:
- *Intersection semantics* (prune what fails either rule): rejected — surprising,
  deletes more than either rule alone, no precedent in comparable tools.
- *Floor enforced by callers*: rejected — one forgotten call site breaks the
  non-negotiable invariant; centralizing it makes the guarantee structural.

## R3. Storage-location seam

**Decision**: A small in-package interface —
`storageLocation { Name() string; List(ctx) ([]backupRef, error); Delete(ctx, ref) error }` —
with two v1 implementations: local directory (via `os.ReadDir` on
`Configuration.BackupDir`) and S3 (via `S3Client`). The orchestrator
(`InfrahubOps.applyRetention`) runs selection per location, logs each deletion,
aggregates per-leg errors with `errors.Join`, and never fails fast between legs
(FR-007, FR-008).

**Rationale**: Keeps policy evaluation identical per location ("evaluated
independently, floor per location" — the confirmed semantics), gives the P3 Plakar
slice an obvious third implementation, and lets unit tests inject fake locations to
prove ordering, floor, and error-aggregation behavior without filesystems or buckets.

**Alternatives considered**:
- *Separate code paths for local and S3*: rejected — duplicates the policy walk,
  which is exactly how per-location semantics drift apart.
- *A new package (`src/internal/retention`)*: rejected — Constitution I keeps
  application logic in `src/internal/app`; an in-package file suffices.

## R4. S3 listing and deletion

**Decision**: Extend the existing `S3Client` (`src/internal/app/s3.go`, minio-go v7)
with `List(ctx) ([]backupRef, error)` — `ListObjects` scoped to the configured
prefix, non-recursive, filtering keys through the same base-name regex — and
`Delete(ctx, key string) error` via `RemoveObject`. No new dependency.

**Rationale**: minio-go is already a direct dependency used for upload/download;
`ListObjects` handles pagination internally via its channel iterator. Filtering by
the shared regex guarantees FR-006 holds identically at both locations.

**Alternatives considered**:
- *aws-sdk-go-v2*: rejected — new dependency for capabilities minio-go already has
  (Constitution III: no new deps needed).
- *S3 bucket lifecycle rules only*: rejected by explicit product decision during
  grilling — v1 includes in-tool S3 pruning; lifecycle rules remain a documented
  complement for users who prefer them.
- *`RemoveObjects` batch API*: deferred — per-object `RemoveObject` keeps per-deletion
  logging (Principle V) simple; batching is an optimization with no v1 need at
  O(10⁴) objects.

## R5. Confirmation UX for standalone `prune`

**Decision**: `prune` without `--force` prints the exact candidate list (same output
as `--dry-run`), then asks a single `y/N` question on stdin; declining or a non-TTY
stdin aborts with an actionable error ("re-run with --force for non-interactive
use"). `--force` skips the prompt. `--dry-run` never prompts and never deletes;
`--dry-run` + `--force` is rejected as contradictory.

**Rationale**: Principle II demands explicit confirmation or an explicit bypass flag.
The repo already has both conventions: refuse-without-`--force` (`create --redact`)
and interactive-prompt-with-skip-flag (`update --yes`, `updater.Proceed`). `prune`
merges them: preview-then-prompt interactively, `--force` for scripts — matching the
spec's acceptance scenarios (US2) exactly. Flag name `--force` (not `--yes`) matches
the backup binary's existing destructive-op vocabulary.

**Alternatives considered**:
- *Refuse-always without `--force`* (redact style): rejected — spec US2 scenario 2
  requires an interactive confirm-or-abort.
- *`--yes` flag name*: rejected — `--force` is the established bypass on this binary
  (`create --force`, redact confirmation); consistency wins within the binary.

## R6. Configuration and flag surface

**Decision**: `Configuration` gains `Retention RetentionConfig{Days, Count int}`
(0 = rule inactive). Flags `--retention-days` and `--retention-count` are registered
on both `create` and `prune` and bound through viper like every existing flag, so
`INFRAHUB_RETENTION_DAYS` / `INFRAHUB_RETENTION_COUNT` and config-file keys work for
CronJobs (FR-011; viper `SetEnvPrefix("INFRAHUB")` + `AutomaticEnv` already in
`cli.go`). Validation (each ≥1 when set; at least one set to activate; `prune`
errors when none set) happens before any operation.

**Superseded during implementation (2026-08-04)** — this decision shipped differently
in two respects, recorded here so the research note is not read as a description of the
code:

- *No config-file keys.* The binary reads no configuration file at all; `cli.go` sets up
  only `SetEnvPrefix`/`SetEnvKeyReplacer`/`AutomaticEnv`. Flags and environment variables
  are the only channels, and FR-011 was narrowed to match.
- *Not viper-bound.* The two `viper.BindPFlag` calls for the retention keys were removed:
  viper coerces an environment value to an integer, and 0 means "rule inactive", so a
  mistyped variable was indistinguishable from an unset rule. The cmd layer now reads the
  variables with `os.LookupEnv` and hands the raw text to `app.ResolveRetentionConfig`,
  which rejects anything that is not a whole number ≥ 1 and treats a present-but-empty
  variable as unset. See `contracts/cli.md` § Corrections after implementation.

**Rationale**: Follows the binary's uniform flag/viper pattern; environment-variable
support is what makes the K8s CronJob path scriptless.

**Alternatives considered**:
- *Single combined flag* (e.g. `--retention 7d,14`): rejected — bespoke parsing,
  worse discoverability, breaks the one-flag-one-rule convention used everywhere else.
- *Persistent root-level flags*: rejected — retention is meaningful only on
  `create`/`prune`; root registration would advertise it on `restore`/`keygen`.

## R7. `create` integration point and failure semantics

**Decision**: In `CreateBackup` (tarball path only — the Plakar early-return at
`backup.go:28` is untouched in v1), after the archive is fully written, checksummed,
and — when `--s3-upload` was requested — successfully uploaded, call
`applyRetention` with the active legs: local always; S3 exactly when this run
uploaded (FR-007). Legs run sequentially (local, then S3), each attempted regardless
of the other's outcome; on any leg error `CreateBackup` returns
`fmt.Errorf("backup succeeded (%s); retention failed: %w", backupFilename, errors.Join(legErrs...))`
so Cobra exits non-zero while the message makes the split outcome unmistakable
(FR-008). The new backup file/object is never deleted by its own run's prune — the
floor guarantees this structurally (it is the newest).

**Rationale**: Pruning strictly after full success means a failed backup never
triggers deletion (US1 scenario 4) and the fresh backup always anchors the floor.
Sequential legs keep log output readable (Principle V) at negligible cost.

**Alternatives considered**:
- *Prune before backup* (free space first): rejected — a prune-then-fail-backup
  sequence shrinks the safety margin exactly when recoverability matters most;
  violates the spirit of Principle II.
- *Distinct exit code for "backup ok, prune failed"*: rejected in grilling — most
  cron monitors are zero/non-zero; the error text carries the distinction.

## R8. End-to-end test strategy

**Decision**: Extend the pytest e2e suite (`tests/e2e/`) with retention scenarios on
the local leg (seed `BackupDir` with fabricated old-timestamp archives, run
`prune --dry-run` / `prune --force` / `create` with retention, assert exact
survivor sets). The S3 leg is unit/integration-tested in Go behind the
`storageLocation` seam; if the existing e2e infrastructure already provisions
S3-compatible storage (minio), add one S3 scenario, otherwise document it as a
follow-up in tasks rather than building new fixture machinery in this feature.

**Rationale**: Constitution IV requires tests for behavior changes; the selection
core is pure-function tested exhaustively, so e2e focuses on wiring (flags → config →
deletion) where regressions would actually hide.

## R9. Plakar (P3) design constraints — recorded, not implemented

**Decision**: P3 reuses `RetentionPolicy` and the `storageLocation` seam with a
Plakar implementation listing complete snapshot groups (via the existing
`collectBackupGroups`), where `Delete` removes all component snapshots of a group and
triggers kloset maintenance for space reclamation. Constraints locked now: incomplete
groups never count toward the count rule; the newest group overall and the newest
complete group are never removed; remaining snapshots — including ones written by
prior released versions — must stay restorable (constitution Backup-engine clause).

**Rationale**: Recording the constraints in v1 keeps the flag surface and policy
semantics from forking when P3 lands; implementation is deliberately out of this
plan's task scope.
