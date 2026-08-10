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

**Expected**: the 3 old archives are listed as `Would prune backup <name> from
local:/tmp/ret-test (dry run)`, followed by one
`Retention at local:/tmp/ret-test: 3 backup(s) kept, 3 candidate(s) to prune` summary;
exits 0; `ls` shows nothing was deleted (9 files still present).

## Scenario 2 — Confirmation prompt, decline, accept, force (US2)

```bash
infrahub-backup prune --backup-dir /tmp/ret-test --retention-days 7   # answer: n
```

**Expected**: the same 3 candidates listed as `Would prune backup …` — with **no**
`(dry run)` suffix, this being a real run — then the per-location summary, then
`Prune 3 backup(s) listed above? [y/N]:`. Answering `n` logs
`Prune aborted; no backups were deleted` and exits 0 with nothing deleted.

Answering `y` instead logs one `Pruned backup <name> from <location>` per deletion and
closes with `Pruned 3 of 3 confirmed backup(s)` — the confirmed set is the deleted set.

```bash
infrahub-backup prune --backup-dir /tmp/ret-test --retention-days 7 --force
```

**Expected**: no candidate listing and no prompt; the per-location summary, then the 3
old archives deleted (each logged); the 3 recent archives and all 3 decoys remain;
exits 0. Requires a re-seeded fixture if the accept path above already pruned.

## Scenario 3 — Floor: policy matching everything keeps the newest (FR-003, SC-003)

```bash
cd /tmp/ret-test && rm -f infrahub_backup_*.tar.gz*   # reset
for d in 40 50 60; do
  touch "infrahub_backup_$(date -d "-${d} days" +%Y%m%d_%H%M%S).tar.gz"
done
infrahub-backup prune --backup-dir /tmp/ret-test --retention-days 7 --force
```

The reset glob also removes the `infrahub_backup_garbage.tar.gz` decoy, so only
`notes.txt` and `somebackup.tar.gz` remain from the fixture's decoys here; re-seed if
you want to re-run Scenarios 1–2 afterwards.

**Expected**: the two oldest are deleted; the newest (40-day-old) archive **survives**
despite matching the age rule. The floor is visible in the summary line —
`Retention at local:/tmp/ret-test: 1 backup(s) kept, 2 candidate(s) to prune` — and in
the surviving file; there is no log line naming the kept archive or the reason it was
kept.

## Scenario 4 — Validation errors (FR-001, US2 scenario 4)

```bash
infrahub-backup prune --backup-dir /tmp/ret-test                      # no rule
infrahub-backup prune --backup-dir /tmp/ret-test --retention-days 0   # invalid value
infrahub-backup prune --backup-dir /tmp/ret-test --retention-days 7 --dry-run --force
```

**Expected**: each exits non-zero before any location is listed, with, respectively:

```text
at least one retention rule is required: pass --retention-days and/or --retention-count
--retention-days must be at least 1 when set; omit it to disable the age rule
--dry-run and --force are contradictory: --dry-run never deletes and never prompts, so there is nothing to force
```

The same values supplied through `INFRAHUB_RETENTION_DAYS` / `INFRAHUB_RETENTION_COUNT`
are rejected too, with the variable named in the message — for example
`INFRAHUB_RETENTION_DAYS must be a whole number of at least 1 (got "abc"); omit it to
disable the age rule`. A variable that is present but empty or whitespace-only is read
as unset instead, so it yields the "at least one retention rule is required" error rather
than a bad-value error.

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
object is ever deleted. Note that `create --s3-upload` without `--s3-keep-local`
removes the local copy of the fresh archive before retention runs, so the local leg's
newest is the *previous* archive.

With upload-only credentials the S3 leg fails and the run exits non-zero with "backup
succeeded … retention failed" (create) or the leg error (prune). Whether the local leg
still prunes depends on the path: it does on `create` and on `prune --force`, but a
non-forced `prune` that cannot **list** the bucket aborts before the confirmation and
deletes nothing at any location. Verified against an unreachable endpoint: the non-forced
run left all local archives intact, the same run with `--force` pruned every local
candidate.

## Automated equivalents

- Selection/floor/parsing/validation: `go test ./src/internal/app -run 'Retention|Prune'`
- End-to-end (local leg): pytest cases added under `tests/e2e/` (see tasks.md)
- Full gates: `make test && make vet && make lint`
