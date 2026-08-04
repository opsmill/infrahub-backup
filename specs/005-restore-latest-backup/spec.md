# Feature Specification: Restore the Latest Backup Without Naming an Archive

**Feature Branch**: `fac/restore-latest-backup-xmj2m`

**Created**: 2026-08-04

**Status**: Draft

**Input**: User description: "Restore the latest backup without naming an exact archive (GitHub issue opsmill/infrahub-backup#152), per the maintainer-confirmed idea brief at https://github.com/opsmill/infrahub-backup/issues/152#issuecomment-5179350853"

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Scheduled staging sync restores the newest production backup unattended (Priority: P1)

A nightly job on the staging deployment runs `infrahub-backup restore --latest --s3` and staging comes up with the newest production backup — no human involved, no filename known ahead of time. This is the primitive that the scheduled prod → staging sync (opsmill/infrahub-helm#80) depends on: a scheduled job cannot hard-code an archive filename.

**Why this priority**: Automation is the hard blocker today. An operator can work around the missing flag by listing files; a CronJob cannot. Removing this blocker directly enables the nightly staging sync.

**Independent Test**: Can be fully tested by seeding an S3 bucket/prefix with several backup archives, running `restore --latest --s3` with no positional argument, and verifying the archive with the newest embedded timestamp is downloaded and restored, with its name and source logged and exit code 0.

**Acceptance Scenarios**:

1. **Given** a shared S3 bucket/prefix holding several production backup archives and a staging deployment configured with that bucket, **When** the scheduled job runs `restore --latest --s3`, **Then** the archive with the newest filename-embedded timestamp in that prefix is downloaded and restored, its name and source location are logged before the restore begins, and the process exits 0.
2. **Given** the same setup but a newer archive exists in the staging host's local backup directory, **When** the job runs `restore --latest --s3`, **Then** the S3 archive is chosen — the local pool is never consulted when `--s3` is passed.

---

### User Story 2 - Operator restores the newest local backup interactively (Priority: P2)

An operator on the host runs `infrahub-backup restore --latest` instead of first listing the backup directory, eyeballing timestamps, and copy-pasting a filename.

**Why this priority**: Valuable friction removal for the most common interactive restore path, but an operator has a manual workaround today; the automation journey does not.

**Independent Test**: Can be fully tested by placing several backup archives in the configured local backup directory, running `restore --latest`, and verifying the newest one is restored.

**Acceptance Scenarios**:

1. **Given** several backup archives in the configured local backup directory, **When** the operator runs `restore --latest`, **Then** the newest archive (by filename-embedded timestamp) is restored and its name is logged before the restore begins.
2. **Given** the operator passes both `--latest` and a positional archive path, **When** the command is invoked, **Then** it fails immediately with an error stating the two are mutually exclusive, and nothing is restored.

---

### Edge Cases

- **Newest archive is encrypted, no decryption key given**: the command fails before the sleep wait, before any S3 download, and before any container is touched. It never falls back to an older unencrypted archive — a silently stale staging environment is worse than a visibly failed job.
- **Empty pool** (no archives matching the backup naming pattern in the selected location): clear error and non-zero exit. This is the expected state before the first backup ever runs.
- **Non-backup files in the pool** (unrelated files, partial uploads, foreign names): ignored — only names matching the established backup archive naming pattern participate, exactly as retention already behaves.
- **Timestamp ties** (two archives sharing a timestamp): deterministic name-descending tiebreak, identical to retention's ordering, so "the newest backup retention keeps" and "the backup `--latest` restores" can never disagree.
- **Retention pruning races the restore** (an S3 object is deleted between listing and download): the download fails with a clear error and non-zero exit. Acceptable for v1; no retry or re-list logic.
- **Bare `restore` with no argument and no `--latest` on the tarball backend**: remains a hard error (unchanged behavior), but the error message now points the user at `--latest`.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: Users MUST be able to run `restore --latest` to restore the most recent backup without naming an archive. *(Verification: with two backups present, the newer one is restored.)*
- **FR-002**: `--latest` and the positional `[backup-file]` argument MUST be mutually exclusive; passing both is an error. *(Verification: CLI test asserts the error and that nothing is restored.)*
- **FR-003**: On the tarball backend, bare `restore` (no argument, no flag) MUST keep erroring, and the error message MUST mention `--latest`. *(Verification: golden-output CLI test.)*
- **FR-004**: On the plakar backend, `--latest` MUST be accepted and behave identically to today's no-argument restore; the no-argument form MUST keep working unchanged. *(Verification: both invocations resolve the same backup group.)*
- **FR-005**: `--latest` MUST rank archives exactly as the retention policy ranks them: by filename-embedded timestamp, newest first, with a name-descending tiebreak. *(Verification: unit test reusing the retention ordering fixtures.)*
- **FR-006**: `restore --latest --s3` MUST select the newest archive from the configured S3 bucket/prefix instead of the local backup directory; the two pools MUST never be merged. *(Verification: with a newer local file and an older S3 object, the S3 object is chosen when `--s3` is passed.)*
- **FR-007**: If the archive selected by `--latest` is encrypted and no decryption key is provided, restore MUST fail before the sleep wait, before any S3 download, and before any container action; it MUST NOT fall back to an older archive. *(Verification: newest archive is encrypted, no key → non-zero exit, deployment untouched, no download issued.)*
- **FR-008**: An empty pool — no archives matching the backup naming pattern in the selected location — MUST produce a clear error and non-zero exit. *(Verification: empty directory and empty prefix tests.)*
- **FR-009**: The resolved archive name and its source location MUST be logged before the restore begins. *(Verification: log assertion in tests.)*

### Key Entities

- **Backup archive**: an existing artifact (`infrahub_backup_<timestamp>.tar.gz`, optionally with an encrypted suffix) — the thing `--latest` selects. No changes to its format or naming.
- **Storage location**: an existing concept from the retention work — the local backup directory or an S3 bucket/prefix. It is the pool over which "latest" is computed; exactly one location is consulted per invocation.
- **Backup group / snapshot (plakar backend)**: existing; behavior untouched except that `--latest` becomes an accepted explicit alias for what the no-argument form already does.

No new entities are introduced.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: Restoring the most recent backup takes exactly one command invocation with zero prior lookup or listing steps, both interactively and from a scheduled job.
- **SC-002**: Every defined failure path (empty pool, encrypted-without-key, flag-and-argument conflict) exits non-zero with the deployment left untouched — each path covered by an automated test.
- **SC-003**: Which archive was restored, and from which location, is determinable from the command's output alone — every scheduled sync is auditable after the fact without access to the machine.

## Assumptions

- Filename-timestamp ranking is order-consistent across hosts: the timestamp format sorts lexicographically in chronological order, so relative ordering does not depend on the host's time zone (the retention work's host-local-zone caveat affects age *boundaries*, not relative order).
- Scheduling itself, and guardrails preventing a scheduled restore from targeting the production release, live in the Helm chart work (opsmill/infrahub-helm#80); this feature only provides the restore-latest primitive.
- S3 configuration (bucket, prefix, credentials) arrives through the existing flags and environment variables; no new configuration surface is introduced.
- Passing `--latest` is itself the explicit, deliberate request that Operational Safety (constitution Principle II) requires for destructive operations — consistent with today's restore, which also runs without an interactive confirmation once invoked with an archive. No new confirmation prompt is added, because the primary user is an unattended job; the pre-restore log line (FR-009) preserves auditability.
- Restore's existing safety behavior (metadata validation, checksum verification, version compatibility checks, container stop/restart guarantees) applies unchanged to an archive selected by `--latest`; this feature only changes *how the archive is chosen*, not how it is restored.

## Out of Scope (v1)

- Making bare `restore` default to "latest" on the tarball backend (a typo must not become a data-overwriting default).
- Per-project or per-environment filtering of archives — operators separate environments by directory or S3 prefix, as retention already requires.
- A merged local + S3 pool, or any fallback to older archives on any failure.
- A `backup list` command for tarball archives.
- The CronJob / Helm chart work itself (opsmill/infrahub-helm#80).
