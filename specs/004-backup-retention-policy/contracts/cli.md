# CLI Contract: Backup Retention Policy

**Feature**: `specs/004-backup-retention-policy` | **Date**: 2026-08-04

The CLI surface is this feature's public contract. Everything below is additive;
no existing flag or command changes behavior when retention is not configured
(US1 scenario 3).

## `infrahub-backup create` — new flags

```text
--retention-days N    Keep backups newer than N days (N >= 1). 0/unset = rule inactive.
--retention-count N   Keep the N most recent backups (N >= 1). 0/unset = rule inactive.
```

**Behavior**:
- Neither flag set → identical to today; no pruning of any kind.
- At least one set → after the backup fully succeeds (archive written and checksummed;
  S3 upload completed when `--s3-upload` was passed), retention is applied without any
  prompt:
  - **local leg** (always): files in `BackupDir` matching
    `infrahub_backup_<YYYYMMDD_HHMMSS>.tar.gz[.enc]` with a parseable timestamp;
  - **S3 leg** (exactly when this run uploaded to S3): objects under the configured
    bucket/prefix whose base name matches the same pattern.
- Each location is evaluated independently: a backup is kept if the days rule or the
  count rule claims it; the most recent backup at each location is always kept.
- Each deletion is logged as it happens
  (`Pruned backup <name> from <location>`-level messages).
- Validation errors (either flag set to 0 or negative) abort the run before any
  backup work starts.
- Plakar backend (`--backend plakar`) with retention flags set: the backup runs
  normally, a warning states that retention is not yet supported for this backend,
  no pruning occurs, exit 0 (FR-012).

**Exit semantics**:

| Outcome | Exit | Error text contract |
|---|---|---|
| Backup ok, retention ok (or inactive) | 0 | — |
| Backup failed | ≠ 0 | Existing behavior; **no pruning was attempted** |
| Backup ok, any prune leg failed | ≠ 0 | Message MUST begin by stating the backup succeeded (includes the new archive's name) and that only retention failed; all legs were attempted before exiting |

Within a leg, deletion is best-effort: after an individual deletion fails, the
remaining candidates are still attempted; all per-deletion errors are collected and
reported together (FR-008).

## `infrahub-backup prune` — new command

```text
infrahub-backup prune --retention-days N | --retention-count N [flags]

Flags:
  --retention-days N    As on create (>= 1 when set)
  --retention-count N   As on create (>= 1 when set)
  --dry-run             List exactly what a real run would delete; delete nothing;
                        never prompts
  --force               Skip the confirmation prompt (non-interactive / scripted use)
  --s3                  Also prune backup objects under the configured S3
                        bucket/prefix. Without this flag S3 is NEVER touched, even
                        if S3 configuration is present.
```

Inherits root persistent flags (`--backup-dir`, S3 configuration, `--log-level`, …).

**Behavior**:
- No retention flag set → validation error: at least one rule is required.
- Plakar backend (`--backend plakar`) → validation error: retention for the Plakar
  backend is not yet supported (FR-012); nothing is touched.
- `--dry-run` → print the candidate set (per location), delete nothing, exit 0.
- Without `--force` (interactive TTY) → print the candidate set, ask a single
  `y/N` confirmation; decline aborts with no changes (exit 0, "aborted" notice).
- Without `--force` (stdin not a TTY) → refuse with an actionable error telling the
  operator to pass `--force` for non-interactive use (no deletion).
- `--dry-run --force` → rejected as contradictory (validation error).
- Floor: the most recent backup at each pruned location always survives; there is no
  flag to override this.

**Exit semantics**:

| Outcome | Exit |
|---|---|
| Nothing to prune / dry-run / user declined prompt | 0 |
| All requested legs pruned successfully | 0 |
| Validation error (no rule, bad values, contradictory flags) | ≠ 0 |
| Any leg failed (e.g. S3 delete denied) — other legs still attempted | ≠ 0 |

## Configuration equivalents (FR-011)

Every flag is viper-bound like the rest of the binary (env prefix `INFRAHUB`):

| Flag | Environment variable | Config-file key |
|---|---|---|
| `--retention-days` | `INFRAHUB_RETENTION_DAYS` | `retention-days` |
| `--retention-count` | `INFRAHUB_RETENTION_COUNT` | `retention-count` |

`--dry-run`, `--force`, and `--s3` are per-invocation switches on `prune` and are
deliberately **not** config-file/env configurable (a persistent `force=true` would
defeat Principle II).

## Operational requirements (documented, not enforced by the tool)

- S3 pruning requires list and delete permissions on the bucket/prefix in addition
  to today's upload permission. With upload-only credentials the S3 leg fails and is
  reported per the exit semantics above; the local leg is unaffected.
- Backups from multiple instances sharing one `BackupDir`/prefix share one policy;
  operators needing distinct policies use distinct directories/prefixes (spec
  Assumptions).
