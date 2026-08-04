# Data Model: Restore the Latest Backup Without Naming an Archive

**Feature**: `specs/005-restore-latest-backup` | **Date**: 2026-08-04

This feature introduces **no new entities and no new persisted state**. It composes
two existing concepts and adds one transient selection flow.

## Existing Entities (reused, unchanged)

### Backup archive (`backupRef`)

| Aspect | Detail |
|---|---|
| Represents | One backup artifact at a storage location |
| Identity | Base name `infrahub_backup_<YYYYMMDD_HHMMSS>.tar.gz[.enc]` (`backupNamePattern`) |
| Derived attribute | `createdAt()` — creation time parsed from the name (host-local); the sole ranking key, with name-descending tiebreak |
| Validation | Names not matching the pattern never become refs (`parseBackupName`); this feature adds no new name forms |
| Source of truth | `src/internal/app/retention.go` |

### Storage location (`storageLocation`)

| Aspect | Detail |
|---|---|
| Represents | One pool of backup archives |
| Implementations | `localLocation` (the configured `BackupDir`); `s3Location` (configured bucket/prefix via `S3Client`) |
| Used capability | `List(ctx)` and `Name()` only — this feature never calls `Delete` |
| Invariant | Exactly **one** location is consulted per `--latest` invocation; pools are never merged (FR-006) |
| Error semantics | A missing local directory is an error, not an empty pool — inherited by FR-008 |
| Source of truth | `src/internal/app/retention.go` |

### Backup group / snapshot (plakar backend)

Untouched. `--latest` on plakar routes to the existing `RestorePlakarBackup` /
`findLatestCompleteGroup` path; no schema, tag, or layout change.

## Selection Flow (transient, per invocation)

```text
flags (--latest, --s3, decrypt-key)
        │
        ▼
[1] Pool construction        local: newLocalLocation(BackupDir)
                             --s3:  ValidateConfig → NewS3Client → newS3Location
        │
        ▼
[2] Resolution               List(ctx) → sortBackupRefsNewestFirst → refs[0]
                             empty list → ERROR (FR-008)
        │
        ▼
[3] Fail-fast gate           name ends ".enc" ∧ no decrypt key → ERROR (FR-007)
        │                    (before sleep, download, container action)
        ▼
[4] Audit log                "Restoring latest backup <name> from <location>" (FR-009)
        │
        ▼
[5] Delegation               local  → RestoreBackup(BackupDir/<name>, …)
                             --s3   → RestoreBackup("s3://<bucket>/<key>", …)
                             (existing path: download, decrypt, validate metadata +
                              checksums + version, stop/restore/start containers)
```

**State transitions**: none persisted. The only state this feature creates is the
resolved `backupRef` held for the duration of one process. Failure at any step
leaves the deployment untouched because steps 1–4 precede every side effect and
step 5 is the existing restore path with its existing guarantees (constitution II).

## Validation Rules (from requirements)

| Rule | Enforced at | Requirement |
|---|---|---|
| `--latest` ⊕ positional arg (exactly one) | CLI `Args` validation | FR-002 |
| Bare `restore` on tarball → error naming `--latest` | CLI `Args` validation | FR-003 |
| `--s3` requires `--latest` | CLI validation | FR-006 (surface) |
| Only pattern-matching names participate | `storageLocation.List` (existing) | FR-005/FR-008 |
| Ranking = retention's ranking | `sortBackupRefsNewestFirst` (existing, reused) | FR-005 |
| Empty pool → error | `resolveLatestBackup` | FR-008 |
| Encrypted newest + no key → error before side effects | `RestoreLatestBackup` pre-flight | FR-007 |
| Resolved name + location logged pre-restore | `RestoreLatestBackup` | FR-009 |
