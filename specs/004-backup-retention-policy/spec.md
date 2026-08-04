# Feature Specification: Backup Retention Policy

**Feature Branch**: `fac/backup-retention-policy-fouxa`

**Created**: 2026-08-04

**Status**: Draft

**Input**: User description: "Backup retention policy for infrahub-backup (issue opsmill/infrahub-backup#151). `infrahub-backup create` never removes old archives, so backup storage grows without bound. Add a retention policy: two optional rules (days / count, union keep-semantics), applied automatically by `create` when configured and available as a standalone `prune` command, with an unconditional keep-newest floor. v1 covers local archive files and uploaded S3 objects, evaluated independently per storage location; Plakar snapshot-group pruning is a separate lower-priority slice. Primary user: operator running unattended scheduled backups (driver: opsmill/infrahub-helm#80 nightly prod→staging sync)."

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Scheduled backup with automatic retention (Priority: P1)

An operator schedules `infrahub-backup create` (host cron or Kubernetes CronJob) with a retention policy expressed as "keep backups newer than N days" and/or "keep the newest N backups". After each successful backup, the tool automatically deletes the backups that fall outside the policy — from the local backup directory and, when S3 upload is in use, from the configured S3 bucket/prefix — so backup storage stays bounded forever without any out-of-band cleanup scripts.

**Why this priority**: This is the driving scenario. Unbounded growth fills the backup volume and eventually breaks backups; every operator today reinvents `find`-based cleanup. A retention policy built into the scheduled run is what makes the tool safe to run unattended, and it is a prerequisite for the nightly prod → staging sync (opsmill/infrahub-helm#80).

**Independent Test**: Can be fully tested by running `create` with retention options against a backup directory (and an S3 bucket) seeded with older archives, then verifying that exactly the out-of-policy archives are gone, the new backup and all in-policy archives remain, and the process exited successfully.

**Acceptance Scenarios**:

1. **Given** a backup directory containing archives older than 7 days and more than 14 total, **When** a scheduled `create` run with "keep 7 days, keep newest 14" completes a successful backup, **Then** every archive claimed by neither rule is deleted, every archive claimed by either rule remains, each deletion is reported in the output, and the run exits with success.
2. **Given** S3 upload is enabled for the run and the bucket/prefix contains out-of-policy backup objects, **When** the scheduled run completes, **Then** the same policy is evaluated independently over the S3 objects and out-of-policy backup objects are deleted there too, keeping the newest object in that location regardless of policy.
3. **Given** no retention option is configured, **When** `create` runs, **Then** no pruning occurs and behavior is identical to today.
4. **Given** the backup itself fails, **When** the run ends, **Then** no pruning is attempted at any location.
5. **Given** the backup succeeds but a prune step fails (for example, missing delete permission on the bucket), **When** the run ends, **Then** the new backup is untouched, the other location was still pruned, and the run exits with a failure that clearly states the backup succeeded and only retention failed.

---

### User Story 2 - Manual prune with preview (Priority: P2)

An operator wants to reclaim disk space now, without taking a new backup: they run a standalone `prune` command with the same retention options, preview what would be deleted with a dry-run mode, then confirm the actual deletion interactively (or bypass the prompt explicitly for scripted use).

**Why this priority**: Applying a new policy retroactively and previewing destructive actions are what make retention adoptable on existing installations with months of accumulated archives. It shares the same policy evaluation as User Story 1 and ships together with it.

**Independent Test**: Can be fully tested by running `prune` in dry-run mode against a seeded backup directory (verifying nothing is deleted and the candidate list is exact), then running it for real (verifying the prompt, the bypass option, and that exactly the previewed set is deleted).

**Acceptance Scenarios**:

1. **Given** a backup directory of accumulated archives, **When** the operator runs `prune` with a retention policy in dry-run mode, **Then** the exact set of archives that a real run would delete is listed and nothing is removed.
2. **Given** the same directory and policy, **When** the operator runs `prune` without dry-run, **Then** they are asked to confirm before anything is deleted, and declining aborts with no changes.
3. **Given** the operator passes the explicit confirmation-bypass option, **When** `prune` runs, **Then** deletion proceeds without a prompt (suitable for scripts).
4. **Given** no retention option is provided, **When** `prune` runs, **Then** it fails with a validation error explaining that at least one retention rule is required.
5. **Given** the operator explicitly requests S3 pruning on the `prune` command, **When** it runs, **Then** the policy is also applied to the configured bucket/prefix; without that explicit request, S3 is never touched even if S3 settings are present in configuration.

---

### User Story 3 - Retention for Plakar snapshot backups (Priority: P3)

An operator using the Plakar backend (snapshot groups in a Plakar repository instead of archive files) applies the same retention policy: out-of-policy snapshot groups are removed from the repository and the space they used is reclaimed, while the newest complete group always survives.

**Why this priority**: The Plakar backend is the strategic storage engine but not the default today; the pain being solved right now is accumulated archive files. This slice reuses the same policy semantics and ships independently after User Stories 1–2.

**Independent Test**: Can be fully tested by seeding a Plakar repository with several complete snapshot groups, running `create`-with-retention or `prune`, and verifying that out-of-policy groups are gone, repository space is reclaimed, and the newest complete group remains restorable.

**Acceptance Scenarios**:

1. **Given** a Plakar repository with more complete snapshot groups than the policy allows, **When** retention runs, **Then** out-of-policy groups are removed, the newest complete group survives, and remaining groups stay restorable.
2. **Given** snapshot groups have been removed by retention, **When** the operation completes, **Then** repository storage space is actually reclaimed, not merely marked unreferenced.
3. **Given** incomplete snapshot groups exist (for example from an interrupted backup), **When** retention runs, **Then** incomplete groups do not count toward the "newest N" rule, are eligible for pruning by age like any other group, and the newest group overall is never removed (it may be a backup in progress).

---

### Edge Cases

- **Stalled schedule**: the schedule was dead for longer than the age rule (for example 10 days with "keep 7 days"). In the `create` path, pruning runs only after a fresh backup exists; in the `prune` path, the keep-newest floor retains the newest backup. A location that had at least one backup can never be left with zero.
- **Foreign or decoy files**: files or objects in the backup location that do not match the backup naming pattern — including names that match superficially but carry an unparseable timestamp — are never considered and never deleted.
- **Empty location**: pruning an empty backup directory or empty bucket/prefix is a successful no-op.
- **Partial permissions**: credentials allow upload but not list/delete on the bucket. The local location is still pruned; the run exits with a failure naming the location that could not be pruned, and the error text distinguishes backup success from retention failure.
- **Encrypted archives**: encrypted backups are selected and aged exactly like plain ones, by name only; their contents are never read.
- **Archives produced from local dump files**: archives created by the `from-files` flow use the same naming pattern and receive the same retention treatment — this is intentional.
- **Invalid policy values**: zero or negative values for either rule are rejected with a validation error before anything runs.
- **Concurrent runs**: the tool has no cross-process locking today; scheduled jobs are assumed to be singletons (see Assumptions).

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: System MUST support two independent retention rules — maximum age in days and maximum kept count — each optional, each requiring a value of at least 1, with at least one rule required to activate retention; zero or negative values MUST be rejected at validation time before any operation runs.
- **FR-002**: System MUST retain a backup if **either** active rule claims it (union semantics); only backups claimed by no active rule are eligible for pruning.
- **FR-003**: System MUST NOT delete the most recent backup at a storage location, under any combination of options; no override for this floor exists.
- **FR-004**: `create` MUST apply the retention policy automatically, without interactive prompting, only when at least one retention rule is configured, and only after the backup (including its upload, when upload is requested) has fully succeeded. When the backup fails, no pruning occurs at any location.
- **FR-005**: System MUST provide a standalone `prune` command that applies the identical policy evaluation, prompts for confirmation before deleting unless an explicit bypass option is passed, and offers a dry-run mode that deletes nothing and lists exactly the set a real run would delete.
- **FR-006**: Pruning MUST consider only files and objects that match the established backup naming pattern (including the encrypted variant) with a parseable embedded timestamp; anything else at the location MUST remain untouched. A backup's age MUST be derived from that embedded timestamp.
- **FR-007**: When S3 pruning is active, the policy MUST be evaluated independently over the objects under the configured bucket/prefix, with the keep-newest floor applied per location. On `create`, the S3 leg is active exactly when the run itself uploaded to S3; on `prune`, the S3 leg is active only when explicitly requested by the operator — the mere presence of S3 configuration MUST NOT trigger S3 deletion.
- **FR-008**: When the backup succeeds but any prune leg fails, the run MUST exit non-zero with an error that distinguishes "backup succeeded; retention failed", MUST NOT remove or roll back the newly created backup, and MUST still attempt every other prune leg before exiting.
- **FR-009**: Every deletion — and every dry-run candidate — MUST be reported to the operator as it is processed, and every error MUST carry the operation context in which it occurred.
- **FR-010**: On the Plakar backend (P3), the same policy MUST remove out-of-policy snapshot groups and reclaim repository space; the newest group overall and the newest complete group MUST never be removed; incomplete groups MUST NOT count toward the kept-count rule; remaining snapshots MUST stay restorable, including those created by prior released versions of the tool.
- **FR-011**: Retention configuration MUST be expressible the same way as all existing options of the tool (command-line options, environment variables, configuration file), so unattended schedulers can supply it without wrapping scripts.

### Key Entities

- **Backup**: one restorable unit. On the archive backend: a single archive file (plain or encrypted) in the local backup directory, or a single uploaded object under the S3 bucket/prefix. On the Plakar backend: one snapshot group (all component snapshots sharing a backup identity). Its age is defined by its embedded creation timestamp.
- **Retention policy**: the pair of optional rules (maximum age in days, maximum kept count) supplied per invocation. It is ephemeral — no state is persisted between runs.
- **Storage location**: a place where backups accumulate and against which the policy is evaluated independently — the local backup directory, the configured S3 bucket/prefix, or a Plakar repository.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: With retention configured as D days and C count at a backup frequency of f per day, the number of backups per storage location never exceeds max(C, D×f + 1) + 1, no matter how many scheduled runs execute.
- **SC-002**: The documented scheduled-backup setup contains zero out-of-band cleanup steps — an operator can delete any external pruning scripts after enabling retention.
- **SC-003**: 100% of prune operations leave at least one backup at every storage location that had at least one before the run — no tolerated exceptions.
- **SC-004**: The dry-run preview matches the set actually deleted by an immediately following real run in 100% of tested scenarios.
- **SC-005**: Zero files or objects that are not backups are deleted by any retention operation, across all tested scenarios.
- **SC-006**: An operator can enable retention on an existing scheduled backup by adding options to the existing command line — no new services, sidecars, or wrapper scripts.

## Assumptions

- A backup's age comes from the timestamp embedded in its name at creation (host-local time); day-granularity retention tolerates timezone skew between hosts.
- Scheduled backup jobs are singletons; coordination between concurrent `create`/`prune` invocations is out of scope (the tool has no cross-process locking today, and retention does not add one).
- Wiring the schedule itself into the Helm chart (CronJob, values) is opsmill/infrahub-helm#80's scope, not this feature's.
- Per-project retention inside a *shared* backup directory is not implementable on the archive backend (names carry no project identity, and encrypted archives cannot be inspected); operators who need distinct policies use one backup directory per instance. Documentation states this.
- S3 credentials used for retention need list and delete permissions in addition to today's upload permission; this is a documented operational requirement, not an authentication code change.
- Restoring "the latest backup" (opsmill/infrahub-backup#152) always has a target because of the keep-newest floor; the two features compose without coordination.

### Out of Scope (v1)

- Tiered (grandfather-father-son / daily-weekly-monthly) retention rules.
- Mirroring local deletions to S3 or treating local + S3 as one merged timeline; evaluation is strictly per location.
- Per-project or per-instance policies within a single shared storage location.
- Any retention state persisted between runs.
- Scheduling machinery itself (cron, CronJob) — retention only makes existing schedules safe.
