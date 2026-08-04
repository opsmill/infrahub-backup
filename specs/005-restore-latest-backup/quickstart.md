# Quickstart: Validate `restore --latest`

**Feature**: `specs/005-restore-latest-backup` | **Date**: 2026-08-04

Runnable scenarios proving the feature end-to-end. Contract details live in
[contracts/cli.md](contracts/cli.md); selection semantics in
[data-model.md](data-model.md).

## Prerequisites

- A running Infrahub Docker Compose deployment (the e2e suite's standard target).
- `bin/infrahub-backup` built: `make build`.
- For the S3 scenarios: a reachable S3 endpoint (the e2e suite uses MinIO) and
  `INFRAHUB_S3_BUCKET` / endpoint configuration exported, as for `create --s3-upload`.

## Scenario 1 — Newest local backup wins (US2 / FR-001, FR-005, FR-009)

```bash
bin/infrahub-backup create                      # older archive
bin/infrahub-backup create                      # newer archive
bin/infrahub-backup restore --latest
```

**Expected**: output contains `Restoring latest backup infrahub_backup_<ts2> from local:<backup-dir>`
where `<ts2>` is the second archive's timestamp; restore completes; exit 0.

## Scenario 2 — S3 pool, never merged (US1 / FR-006)

```bash
bin/infrahub-backup create --s3-upload          # uploads to bucket/prefix
bin/infrahub-backup create                      # newer, local-only archive
bin/infrahub-backup restore --latest --s3
```

**Expected**: the *S3* archive is selected even though a newer local one exists;
log line names the `s3://` location; exit 0.

## Scenario 3 — Conflicts are hard errors (FR-002, FR-003, surface of FR-006)

```bash
bin/infrahub-backup restore --latest infrahub_backup_x.tar.gz; echo "exit=$?"
bin/infrahub-backup restore; echo "exit=$?"                      # tarball backend
bin/infrahub-backup restore --s3; echo "exit=$?"
```

**Expected**: three non-zero exits; the second error mentions `--latest`; nothing
is restored in any of the three.

## Scenario 4 — Empty pool (FR-008)

```bash
BACKUP_DIR=$(mktemp -d) bin/infrahub-backup restore --latest; echo "exit=$?"
```

**Expected**: clear "no backups found" error naming the directory; non-zero exit;
deployment untouched.

## Scenario 5 — Encrypted newest without a key fails fast (FR-007)

```bash
bin/infrahub-backup keygen -o /tmp/bk.key
bin/infrahub-backup create --encrypt-key /tmp/bk.key.pub        # newest is .enc
bin/infrahub-backup restore --latest --sleep 5m; echo "exit=$?"
```

**Expected**: immediate non-zero exit (no 5-minute sleep — proves the check runs
before the wait), error names the `.enc` archive and `--decrypt-key`; no container
was stopped. Then verify the happy path:

```bash
bin/infrahub-backup restore --latest --decrypt-key /tmp/bk.key
```

**Expected**: the encrypted archive restores normally; exit 0.

## Scenario 6 — Plakar parity (FR-004)

```bash
bin/infrahub-backup --backend plakar --repo /path/to/repo restore --latest
bin/infrahub-backup --backend plakar --repo /path/to/repo restore
```

**Expected**: both invocations resolve and restore the same (latest complete)
backup group.

## Automated equivalents

- Unit: `make test` — selection ordering, fail-fast matrix, CLI validation
  (`src/internal/app/restore_latest_test.go`, `src/cmd/infrahub-backup/main_test.go`).
- E2E: `tests/e2e/test_docker_tarball.py` (Scenario 1), `tests/e2e/test_docker_s3.py`
  (Scenario 2), run as in CI (`ci.yml`).
- Gates: `make test && make vet && make lint` must pass (constitution IV).
