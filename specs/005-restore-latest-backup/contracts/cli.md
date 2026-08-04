# CLI Contract: Restore the Latest Backup

**Feature**: `specs/005-restore-latest-backup` | **Date**: 2026-08-04

The CLI surface is this feature's public contract. Everything below is additive:
no existing invocation of `restore` changes behavior. The only user-visible change
to an existing path is the bare-`restore` error message on the tarball backend,
which now also points at `--latest`.

## `infrahub-backup restore` — new flags

```text
--latest    Restore the most recent backup from the selected pool instead of
            naming an archive. Mutually exclusive with the positional
            [backup-file] argument.
--s3        With --latest: select from the configured S3 bucket/prefix instead
            of the local backup directory. Only valid together with --latest.
```

## Invocation matrix

| Invocation (backend) | Behavior |
|---|---|
| `restore <file>` (tarball) | Unchanged: restore the named local archive |
| `restore s3://bucket/key` (tarball) | Unchanged: download then restore that exact object |
| `restore --latest` (tarball) | List `BackupDir`, rank as retention does (filename timestamp desc, name-desc tiebreak), restore the first ref |
| `restore --latest --s3` (tarball) | Same, over the configured S3 bucket/prefix; local pool never consulted |
| `restore` (tarball) | Unchanged error (exit ≠ 0); message now names `--latest` as the no-filename option |
| `restore --latest <file>` (any) | Error: `--latest` and a named archive are mutually exclusive; nothing restored |
| `restore --s3` without `--latest` (any) | Error: `--s3` requires `--latest`; message points at the positional `s3://` URI form for restoring an exact remote archive |
| `restore` (plakar) | Unchanged: restores the latest complete backup group |
| `restore --latest` (plakar) | Identical to the line above — explicit alias, same resolution path |
| `restore --latest --s3` (plakar) | Error: the S3 pool selector does not apply to the plakar backend (repository location comes from `--repo`) |

All existing `restore` flags (`--exclude-taskmanager`, `--migrate-format`,
`--sleep`, `--decrypt-key`, `--force`, `--reset-deployment-id`) compose with
`--latest` unchanged.

## Selection contract (`--latest`, tarball)

- **Pool**: exactly one location per invocation — `BackupDir`, or the configured
  S3 bucket/prefix under `--s3`. Never both, never merged (FR-006).
- **Membership**: only base names matching
  `infrahub_backup_<YYYYMMDD_HHMMSS>.tar.gz[.enc]` participate; everything else
  in the directory/prefix is ignored (FR-005/FR-008).
- **Order**: identical to retention — embedded timestamp descending, tie broken
  by name descending (FR-005). Consequence: the archive `--latest` restores is
  always one that retention's "most recent always survives" rule protects.
- **Empty pool**: error, exit ≠ 0, no side effects (FR-008). A missing local
  backup directory is an error, not an empty pool.

## Fail-fast ordering (FR-007)

For `--latest`, the following are checked **in order, before any side effect**
(before the `--sleep` wait, before any S3 download, before any container action):

1. Flag/argument conflicts (exit ≠ 0)
2. Pool resolution — listing errors and the empty pool (exit ≠ 0)
3. Encrypted-newest gate — resolved name carries `.enc` and no decrypt key was
   provided → exit ≠ 0 with an error naming the archive and `--decrypt-key`.
   **Never** falls back to an older archive.

The pool snapshot is therefore taken **before** any `--sleep` wait: an archive
transferred into place during the sleep is not considered by `--latest` (the
sleep exists so a *named* file can be transferred in; with `--latest`, selection
has already happened).

The existing content-based encryption detection inside the restore path is
unchanged and still applies after download (defense in depth).

## Local-copy safety (`--latest --s3`)

The S3 leg downloads the selected object to a temporary path inside the backup
directory whose name can never match the backup archive pattern, and removes it
after the restore. Consequence: a pre-existing local archive with the same name
as the selected S3 object (the `create --s3-upload` keep-local flow) is **never
overwritten and never deleted** by `restore --latest --s3`.

## Observability contract (FR-009)

Before the restore begins, exactly one info-level line identifies the selection:

```text
Restoring latest backup <archive-name> from <location>
```

where `<location>` is the same rendering retention uses (`local:<dir>` or
`s3://<bucket>/<prefix>`). Every scheduled sync is auditable from captured output
alone (SC-003).

## Exit semantics

| Outcome | Exit | Notes |
|---|---|---|
| Latest resolved and restored | 0 | Existing restore success path |
| Flag/argument conflict | ≠ 0 | Nothing listed, nothing restored |
| Empty pool / listing failure | ≠ 0 | No side effects |
| Encrypted newest, no key | ≠ 0 | No download, no container action, no fallback |
| Restore itself fails | ≠ 0 | Existing restore failure semantics (incl. container restart guarantees) |

## Compatibility

- No new environment variables; no viper bindings for the new flags
  (per-invocation switches, like `prune --s3`).
- No changes to archive naming, metadata, checksums, or the plakar snapshot
  layout — backups created by prior released versions remain restorable, and
  archives selected by `--latest` are validated exactly as named ones are.
