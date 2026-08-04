# Research: Restore the Latest Backup Without Naming an Archive

**Feature**: `specs/005-restore-latest-backup` | **Date**: 2026-08-04

No `NEEDS CLARIFICATION` markers survived the spec phase (the maintainer-confirmed
idea brief on issue #152 resolved every scope decision), so this document records
the technical decisions the design rests on, each grounded in the current code.

## D1 — "Latest" is computed from the archive filename, via the retention primitives

**Decision**: Reuse the retention machinery landed by spec 004 verbatim:
`storageLocation.List()` (which already filters through `parseBackupName` /
`backupNamePattern`, so only `infrahub_backup_<YYYYMMDD_HHMMSS>.tar.gz[.enc]`
participates), then `sortBackupRefsNewestFirst` (filename timestamp descending,
name-descending tiebreak), then take the first ref. An empty listing is an error.

**Rationale**: FR-005 requires selection to be order-identical to retention so
"the backup retention always keeps" and "the backup `--latest` restores" can never
disagree. Reuse makes that a structural property instead of a convention: there is
one ranking function, not two that must be kept in sync. `localLocation` reports a
missing directory as an error (not an empty pool), which FR-008 inherits for free.

**Alternatives considered**:
- *Embedded metadata timestamp* — most trustworthy in theory, but requires
  downloading and opening every candidate archive (S3 pool: N downloads to pick 1).
  Rejected; also diverges from retention's definition of "newest".
- *Filesystem/object mtime* — destroyed by copies, transfers, and bucket
  migrations. Rejected.
- *Reimplementing selection independently of retention* — two ranking
  implementations that can drift. Rejected.

## D2 — CLI surface: `--latest` and `--s3` flags on `restore`; strict conflicts

**Decision**: Two new booleans on `restoreCmd`:
- `--latest` — mutually exclusive with the positional `[backup-file]` argument
  (both → error, nothing restored).
- `--s3` — valid **only together with `--latest`**; switches the pool from the
  local backup directory to the configured S3 bucket/prefix. The pools are never
  merged. `--s3` without `--latest` is an error pointing at the positional
  `s3://` URI form, which already covers "restore this exact remote archive".
- Tarball backend, bare `restore` (no arg, no flag): keeps failing `Args`
  validation, with the message extended to mention `--latest`.
- Plakar backend: `--latest` is accepted and routes to the existing no-argument
  path (`RestorePlakarBackup`, which already restores the latest complete group);
  the bare no-argument form keeps working unchanged. `--latest --s3` on plakar is
  an error — the plakar repository location is configured via `--repo`, and the
  tarball S3 pool concept does not apply.

**Rationale**: Mirrors the convention spec 004 established on `prune`: S3 is never
touched without an explicit `--s3`. Keeping bare `restore` an error on tarball was
an explicit brief decision (a typo must not become a data-overwriting default).

**Alternatives considered**:
- *No-argument defaults to latest on tarball* — brief explicitly rejected (out of
  scope, safety).
- *`--latest` taking a value (e.g. `--latest=s3`)* — non-standard for cobra bools,
  and diverges from the established `--s3` opt-in convention. Rejected.
- *Rejecting `--latest` on plakar* — would break the "one flag, both backends"
  story for the Helm CronJob, which should not need to know the backend. Rejected.

## D3 — Orchestration: resolve first, then delegate to the existing restore path

**Decision**: New shared-core entry point `(iops *InfrahubOps)
RestoreLatestBackup(s3 bool, <existing restore params>)` in a new
`src/internal/app/restore_latest.go`:

1. Build the pool: `newLocalLocation(cfg.BackupDir)`, or with `s3` —
   `cfg.S3.ValidateConfig()` + `NewS3Client(cfg.S3)` + `newS3Location(client)`
   (exactly how `retentionLegsForPrune` builds the S3 leg).
2. Resolve: `resolveLatestBackup(ctx, loc)` — a small pure function (List → sort →
   first, empty → error) unit-testable with the same fake locations
   `retention_test.go` already uses.
3. Fail fast (FR-007): if the resolved name has the `.enc` suffix and no decrypt
   key was provided, return an error **here** — before the sleep wait, before any
   download, before any container action.
4. Log (FR-009): `Restoring latest backup <name> from <location.Name()>` at info
   level before delegating.
5. Delegate: local → `RestoreBackup(filepath.Join(cfg.BackupDir, name), …)`;
   S3 → download with the **same configured client that performed the listing**
   to a collision-proof temporary path
   (`os.CreateTemp(cfg.BackupDir, "restore-latest-*.download")` — a name that
   deliberately does not match `backupNamePattern`), then
   `RestoreBackup(<tempPath>, …)`, removing the temp file afterwards.

**Rationale**: `RestoreBackup` already owns decryption detection,
metadata/checksum/version validation, container lifecycle, and streaming — the
constitution II guarantees. Selection composes in front of it instead of forking
it. The S3 leg deliberately does **not** round-trip through the positional
`s3://` URI path: `downloadBackupFromS3` writes to `BackupDir/<basename>` via
`os.Create` (truncating any existing file) and `RestoreBackup` deletes that path
after an S3-URI restore — so on a host where `create --s3-upload` kept the local
copy (both pools then hold the same newest name), URI delegation would truncate
and then delete the operator's local backup archive. A collision-proof temp name
removes that failure mode entirely (critique E1/X1); keeping it inside
`BackupDir` preserves the same-filesystem/same-volume properties the existing
download relies on. The temp name must never match `backupNamePattern`, so an
interrupted run can never pollute the pool seen by retention or a subsequent
`--latest` (covered by a unit test, critique E3).

**Alternatives considered**:
- *Extending `RestoreBackup` with a `latest bool` parameter* — grows an
  already-wide signature and mixes "choose" with "restore". Rejected.
- *Delegating the S3 case as `RestoreBackup("s3://<bucket>/<key>")`* — reuses
  `downloadBackupFromS3`, whose download-to-`BackupDir/<basename>`-then-delete
  behavior destroys a same-named pre-existing local archive in keep-local flows.
  Rejected as a Principle II violation (critique E1/X1). The positional-URI form
  keeps its existing behavior — pre-existing and out of scope; a follow-up issue
  is worth filing.

## D4 — Encrypted fail-fast is a name check, not a content check

**Decision**: The pre-flight check in step 3 above is
`strings.HasSuffix(name, ".enc") && decryptKey == ""`. The existing content-based
`IsEncryptedFile` check inside `RestoreBackup` stays as-is and still runs after
download (defense in depth for mislabeled files).

**Rationale**: FR-007 requires rejection *before any S3 download*, so only the name
is available at decision time. The name is trustworthy here because the pool was
filtered by `backupNamePattern`, which is also what the encryption path itself uses
when writing archives (`.enc` suffix is the format contract, spec 004 codified it
in the pattern).

## D5 — Testing strategy

**Decision**:
- **Unit (Go, table-driven — constitution IV)**: `resolveLatestBackup` ordering
  (newest wins, tie → name-descending, non-matching names excluded by List, empty
  pool errors, List error propagation) against fake `storageLocation`s; encrypted
  fail-fast matrix (enc+key, enc+nokey, plain); the S3-leg temp download name
  never matches `backupNamePattern` (critique E3); CLI `Args`/flag-conflict
  validation in `src/cmd/infrahub-backup/main_test.go` (flag+arg conflict, `--s3`
  without `--latest`, bare-restore error text mentions `--latest`).
- **E2E (pytest, existing suites)**: `tests/e2e/test_docker_tarball.py` gains a
  create-twice-restore-latest scenario asserting the newer archive is restored and
  the resolved name is logged; `tests/e2e/test_docker_s3.py` gains the
  `--latest --s3` equivalent against the MinIO endpoint the suite already runs.

**Rationale**: Matches how spec 004 covered the analogous surface (unit tables for
pure logic, e2e legs for the flows that touch containers and S3).

## D6 — No configuration surface changes

**Decision**: No new env vars, no viper bindings for the two new flags. `--latest`
and `--s3` are per-invocation switches, like `prune`'s `--dry-run`/`--force`/`--s3`.
S3 connection settings arrive through the existing flags/env (`INFRAHUB_S3_BUCKET`
etc.). No `go.mod` changes, therefore no `flake.nix` vendorHash update.

**Rationale**: Spec assumption ("no new configuration surface") and the spec 004
precedent that per-invocation behavior switches are intentionally not configurable.
