# Quickstart Validation: Backup Retention Policy

**Feature**: `specs/004-backup-retention-policy` | **Date**: 2026-08-04

Runnable scenarios proving the feature end-to-end. Flag/exit-code details:
[contracts/cli.md](./contracts/cli.md). Selection semantics: [data-model.md](./data-model.md).

## Prerequisites

- `make build` succeeded; `bin/infrahub-backup` on PATH (or use `./bin/infrahub-backup`).
- A scratch directory to act as `BackupDir` (scenarios below use `/tmp/ret-test`).
- Scenario 4 only: an S3-compatible endpoint (e.g. local minio) with credentials that
  can list/put/delete, exported the same way the existing `--s3-upload` docs describe.

## Seed fixture (used by all scenarios)

Create fake archives whose embedded timestamps span the policy boundary — 3 "old"
(20+ days), 3 "recent" (today/yesterday), plus decoys that must never be touched:

```bash
mkdir -p /tmp/ret-test && cd /tmp/ret-test
for d in 20 25 30; do
  touch "infrahub_backup_$(date -d "-${d} days" +%Y%m%d_%H%M%S).tar.gz"
done
touch "infrahub_backup_$(date +%Y%m%d_%H%M%S).tar.gz"
touch "infrahub_backup_$(date -d '-1 day' +%Y%m%d_%H%M%S).tar.gz.enc"
touch "infrahub_backup_$(date -d '-2 days' +%Y%m%d_%H%M%S).tar.gz"
# decoys — same directory, must survive every scenario:
touch notes.txt infrahub_backup_garbage.tar.gz somebackup.tar.gz
```

## Scenario 1 — Dry-run previews exactly the prunable set (US2, SC-004)

```bash
infrahub-backup prune --backup-dir /tmp/ret-test --retention-days 7 --dry-run
```

**Expected**: lists exactly the 3 old archives as candidates; exits 0; `ls` shows
nothing was deleted (9 files still present).

## Scenario 2 — Confirmation prompt, decline, force (US2)

```bash
infrahub-backup prune --backup-dir /tmp/ret-test --retention-days 7   # answer: n
```

**Expected**: same candidate list, `y/N` prompt; answering `n` aborts, nothing deleted.

```bash
infrahub-backup prune --backup-dir /tmp/ret-test --retention-days 7 --force
```

**Expected**: the 3 old archives are deleted (each deletion logged); the 3 recent
archives and all 3 decoys remain; exits 0.

## Scenario 3 — Floor: policy matching everything keeps the newest (FR-003, SC-003)

```bash
cd /tmp/ret-test && rm -f infrahub_backup_*.tar.gz*   # reset
for d in 40 50 60; do
  touch "infrahub_backup_$(date -d "-${d} days" +%Y%m%d_%H%M%S).tar.gz"
done
infrahub-backup prune --backup-dir /tmp/ret-test --retention-days 7 --force
```

**Expected**: the two oldest are deleted; the newest (40-day-old) archive **survives**
despite matching the age rule; output states it was kept as the most recent backup.

## Scenario 4 — Validation errors (FR-001, US2 scenario 4)

```bash
infrahub-backup prune --backup-dir /tmp/ret-test                      # no rule
infrahub-backup prune --backup-dir /tmp/ret-test --retention-days 0   # invalid value
infrahub-backup prune --backup-dir /tmp/ret-test --retention-days 7 --dry-run --force
```

**Expected**: each exits non-zero with a validation error (at least one rule required /
value must be ≥ 1 / contradictory flags); nothing deleted.

## Scenario 5 — `create` with retention against a live deployment (US1)

Against a running Docker Compose Infrahub instance (same prerequisites as the
existing backup docs), with `BackupDir` pre-seeded with old-timestamp fixtures:

```bash
infrahub-backup create --retention-days 7 --retention-count 14
```

**Expected**: backup completes first; pruning runs after success with no prompt;
out-of-policy fixtures are gone; the brand-new archive plus in-policy fixtures
remain; exits 0. Re-run *without* retention flags: no pruning occurs (US1 scenario 3).

## Scenario 6 — S3 leg (US1 scenario 2, FR-007) *(requires S3 endpoint)*

Seed the bucket/prefix with old-named objects (`aws s3 cp` / `mc cp` the fixture
files), then:

```bash
infrahub-backup create --retention-days 7 --s3-upload ...   # S3 leg active: uploaded this run
infrahub-backup prune --retention-days 7 --s3 --force ...   # S3 leg on prune: explicit --s3
infrahub-backup prune --retention-days 7 --force ...        # no --s3: bucket untouched
```

**Expected**: per-location evaluation — S3 keeps its own newest object regardless of
what exists locally; without `--s3` (or without this-run upload on `create`) no
object is ever deleted. With upload-only credentials, the S3 leg fails, the local
leg still prunes, and the run exits non-zero with "backup succeeded … retention
failed" (create) / leg error (prune).

## Automated equivalents

- Selection/floor/parsing/validation: `go test ./src/internal/app -run 'Retention|Prune'`
- End-to-end (local leg): pytest cases added under `tests/e2e/` (see tasks.md)
- Full gates: `make test && make vet && make lint`
