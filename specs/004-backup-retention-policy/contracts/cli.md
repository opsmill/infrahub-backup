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
- Each location logs one `Retention at <location>: N backup(s) kept, M candidate(s) to
  prune` summary at info, and each deletion is logged as it happens
  (`Pruned backup <name> from <location>`-level messages).
- Validation errors abort the run before any backup work starts: any value either
  channel supplies that is not a whole number of at least 1 (an explicit 0, a
  negative, a fraction, or non-numeric text).
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
- Without `--force`, a leg that cannot be **listed** leaves the preview incomplete: the
  run returns that leg's error before the confirmation, so nothing is deleted at any
  location. Confirming a partial preview would mean answering for a set the tool does
  not fully know, and a bucket it cannot list is not a bucket with nothing to prune.
  `--force` keeps the attempt-every-leg behavior for scripted runs.
- The confirmed set is exactly the deleted set: one plan is made, both the preview and
  the deletions derive from it, and nothing is re-listed and no fresh evaluation instant
  is taken after the confirmation (SC-004). A candidate already absent at deletion time
  counts as satisfied, not as a failure, so a run can delete fewer backups than it
  confirmed and still succeed.

**Exit semantics**:

| Outcome | Exit |
|---|---|
| Nothing to prune / dry-run / user declined prompt | 0 |
| All requested legs pruned successfully | 0 |
| Validation error (no rule, bad values, contradictory flags, `plakar` backend) | ≠ 0 |
| A leg could not be listed, without `--force` — nothing deleted at any location, no confirmation asked | ≠ 0 |
| A leg failed with `--force` — every other leg is still attempted | ≠ 0 |
| A deletion failed after a complete preview — the leg's remaining candidates are still attempted | ≠ 0 |

## Configuration equivalents (FR-011)

The retention rules are configurable through command-line flags and environment
variables (prefix `INFRAHUB_`). Those are the binary's only two configuration
channels — it reads no configuration file (see the corrections note below):

| Flag | Environment variable |
|---|---|
| `--retention-days` | `INFRAHUB_RETENTION_DAYS` |
| `--retention-count` | `INFRAHUB_RETENTION_COUNT` |

Precedence: the flag on the invoked command wins where both channels supply a rule;
a rule no channel supplies stays inactive.

The two retention flags are deliberately **not** viper-bound, unlike most flags on
this binary. Viper coerces an environment value to an integer, and 0 means "rule
inactive", so a mistyped variable would be indistinguishable from an unset rule —
silently switching retention off on exactly the unattended runs that depend on it.
The cmd layer instead reads each variable with `os.LookupEnv` and passes the raw
text to `app.ResolveRetentionConfig`, the single place precedence and validation are
decided. Any value a channel supplies that is not a whole number of at least 1 (an
explicit 0, a negative, a fraction, or non-numeric text) aborts the run before any
work; an environment variable that is present but empty or whitespace-only resolves
as unset, because that is what Compose and Kubernetes render for a variable with
nothing to substitute.

`--dry-run`, `--force`, and `--s3` are per-invocation switches on `prune` and are
deliberately **not** environment configurable (a persistent `force=true` would
defeat Principle II).

## Operational requirements (documented, not enforced by the tool)

- S3 pruning requires list and delete permissions on the bucket/prefix in addition
  to today's upload permission. With upload-only credentials the S3 leg fails and is
  reported per the exit semantics above. Whether the local leg still prunes depends on
  which right is missing and on `--force`:
  - list denied + `--force` → local leg pruned, run exits non-zero;
  - list denied without `--force` → the preview is incomplete, so the run aborts before
    the confirmation and the local leg does **not** prune either;
  - delete denied only → the preview completes, so the local deletions succeed and the
    S3 deletions fail on both the forced and the confirmed path.
- Age is read from the archive's filename timestamp, parsed in the local time of the
  host running the command, against a fixed 24-hour day. Hosts in different timezones
  therefore place the age boundary differently for the same directory.
- Backups from multiple instances sharing one `BackupDir`/prefix share one policy;
  operators needing distinct policies use distinct directories/prefixes (spec
  Assumptions).

## Corrections after implementation (2026-08-04)

Recorded so this contract matches the shipped, tested binary rather than the design as
first written:

1. **Configuration-file channel removed.** The original "Configuration equivalents"
   section listed a `Config-file key` column and asserted the flags were viper-bound.
   Neither is true: the binary reads no configuration file (there is no
   `SetConfigName`/`AddConfigPath`/`ReadInConfig` anywhere in `src/`, only
   `SetEnvPrefix`/`SetEnvKeyReplacer`/`AutomaticEnv` in `src/internal/app/cli.go`), and
   the two `viper.BindPFlag` calls for the retention keys were removed during
   implementation because nothing read them. Flags and environment variables are the
   only channels. FR-011 in `spec.md` carries the matching correction.
2. **Interactive runs abort on an incomplete preview.** The exit-semantics table
   previously implied every leg is attempted whenever any leg fails. That holds for
   `--force` and for `create`, but a non-forced `prune` whose preview could not be
   completed returns the listing error before confirming and deletes nothing anywhere.
   The operational note on upload-only S3 credentials ("the local leg is unaffected")
   was wrong for that path and has been replaced with the per-path breakdown above.
