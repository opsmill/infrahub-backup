# Quickstart Validation Results

**Feature**: `specs/005-restore-latest-backup` | **Task**: T017 | **Date**: 2026-08-04

Records the outcome of walking [quickstart.md](quickstart.md) Scenarios 1–6 against a live
deployment, plus the constitution IV gates and the SC-002 coverage confirmation.

## Environment

| Item | Value |
|------|-------|
| Deployment | Docker Compose, `infrahub_testcontainers` stack, project `infrahub-test-99d5eab8` |
| Infrahub | 1.8.2 |
| Neo4j | Community edition |
| Binary | `bin/infrahub-backup` from `make build` on this branch |
| S3 endpoint | MinIO `RELEASE.2025-09-07T16-13-09Z`, `http://localhost:39600`, bucket `backups` |
| Plakar repository | `fs:///tmp/qs-s6-plakar-repo` |

`create --force` was used throughout: bare `create` refuses while tasks are running, which is
orthogonal to what these scenarios exercise.

## Gates (constitution IV)

| Gate | Result |
|------|--------|
| `make fmt` | pass — no files reformatted |
| `make build` | pass — all three binaries |
| `make test` | pass — `ok infrahub-ops/src/cmd/infrahub-backup`, `ok infrahub-ops/src/internal/app`, `ok infrahub-ops/src/internal/updater` |
| `make vet` | pass — no findings |
| `make lint` | pass — `0 issues.` (golangci-lint v2) |
| `uv run invoke lint-ruff` | pass — `24 files already formatted`, `All checks passed!` |
| `uv run invoke lint-markdown` | pass on everything this chunk touched; the only 2 findings are pre-existing MD032 in the generated `CLAUDE.md` |

Vale was not run: `vale` is not installed in this environment.

## Scenario results

| # | Scenario | Verdict |
|---|----------|---------|
| 1 | Newest local backup wins | pass (with caveat — see below) |
| 2 | S3 pool, never merged | pass |
| 3 | Conflicts are hard errors | pass |
| 4 | Empty pool | pass |
| 5 | Encrypted newest fails fast | pass |
| 6 | Plakar parity | pass |

### Scenario 1 — Newest local backup wins (FR-001, FR-005, FR-009)

```bash
bin/infrahub-backup --project $P --backup-dir /tmp/qs-s1-backups create --force   # exit 0
bin/infrahub-backup --project $P --backup-dir /tmp/qs-s1-backups create --force   # exit 0
bin/infrahub-backup --project $P --backup-dir /tmp/qs-s1-backups restore --latest
```

Pool after the two backups:

```text
infrahub_backup_20260804_144405.tar.gz
infrahub_backup_20260804_144446.tar.gz
```

Restore output (abridged):

```text
level=info msg="Restoring latest backup infrahub_backup_20260804_144446.tar.gz from local:/tmp/qs-s1-backups"
level=info msg="Starting backup restore" backup_file=/tmp/qs-s1-backups/infrahub_backup_20260804_144446.tar.gz …
level=info msg="Neo4j dump restored successfully"
level=info msg="Restore completed successfully"
exit=0
```

**Verdict: pass.** The newest archive was selected, the FR-009 line names it and the pool,
exit 0, and the seeded tag was present afterwards (`present=True`).

**Caveat, not a defect in this feature.** The Neo4j leg's correctness here is accidental. Per
follow-up issue 2, the restore loads `/tmp/infrahubops/<db>.dump` — the dump the previous
`create` left in the `database` container — rather than the dump inside the archive it was
given. In this scenario the requested archive *is* the one the second `create` produced, so
the leftover and the requested archive hold the same data and the outcome is correct anyway.
The same run also logged
`Failed to cleanup temporary Neo4j backup data (this is expected for community restore
method): exit status 128`, so the leftover survives the restore too.

The prior chunk observed the failing variant (`Not a valid Neo4j archive:
/tmp/infrahubops/neo4j.dump`, exit 1) on this same scenario; it did not reproduce in this
run. The silent-wrong-data variant was reproduced directly in this session, using a **named**
archive rather than `--latest` — see
[followups/issue-2-neo4j-restore-reads-stale-leftover-dump.md](followups/issue-2-neo4j-restore-reads-stale-leftover-dump.md).
That confirms the defect is in the shared Neo4j restore path, predates this feature, and is
independent of how the archive was chosen.

### Scenario 2 — S3 pool, never merged (FR-006, local-copy safety)

```bash
bin/infrahub-backup … --s3-bucket backups --s3-endpoint http://localhost:39600 \
  create --force --s3-upload --s3-keep-local
# then a NEWER local-only archive is placed in the same directory
bin/infrahub-backup … restore --latest --s3
```

```text
level=info msg="Restoring latest backup infrahub_backup_20260804_144658.tar.gz from s3://backups"
level=info msg="Downloading s3://backups/infrahub_backup_20260804_144658.tar.gz to /tmp/qs-s2-backups/restore-latest-3063500820.download"
level=info msg="Starting backup restore" backup_file=/tmp/qs-s2-backups/restore-latest-3063500820.download …
level=info msg="Restore completed successfully"
exit=0
```

**Verdict: pass.** The S3 object was chosen even though `infrahub_backup_20260804_154723.tar.gz`
sat newer in the local directory, so the pools were not merged. The download went to
`restore-latest-*.download`, never to the archive's name. The local copy of the selected
archive was byte-identical afterwards
(`8cec9d3a3b4f70f2f1088a469e5318e7a37b3088ce79d73aa196670b15b6c6eb` before and after), and no
`restore-latest-*` file was left behind.

### Scenario 3 — Conflicts are hard errors (FR-002, FR-003, FR-006 surface)

All three exited 1 with nothing restored:

```text
$ restore --latest infrahub_backup_x.tar.gz
Error: --latest and a named archive are mutually exclusive: pass either --latest or infrahub_backup_x.tar.gz, not both

$ restore
Error: requires exactly 1 arg(s), only received 0: pass the backup archive to restore, or --latest to restore the most recent backup without naming one

$ restore --s3
Error: --s3 selects the pool --latest chooses from and requires it; to restore one exact remote archive, pass its s3://bucket/key URI as the argument instead
```

**Verdict: pass.** Three non-zero exits; the bare-`restore` error names `--latest` (FR-003).

### Scenario 4 — Empty pool (FR-008)

```bash
BACKUP_DIR=$(mktemp -d) bin/infrahub-backup --project $P restore --latest
```

```text
Error: no backups found at local:/tmp/tmp.q5ZWGV4nyu: nothing for --latest to restore
exit=1
```

**Verdict: pass.** The error names the directory. A `docker inspect` snapshot of every
container in the project (status + `StartedAt`) was identical before and after, so no
container was touched, and the rejected directory was not written into.

### Scenario 5 — Encrypted newest without a key fails fast (FR-007)

```bash
bin/infrahub-backup keygen -o /tmp/bk.key
bin/infrahub-backup … create --force --encrypt-key /tmp/bk.key.pub   # infrahub_backup_20260804_144834.tar.gz.enc
bin/infrahub-backup … restore --latest --sleep 5m
```

```text
Error: latest backup infrahub_backup_20260804_144834.tar.gz.enc at local:/tmp/qs-s5-backups is encrypted: pass --decrypt-key with its private key PEM file; --latest never falls back to an older archive
exit=1  elapsed=0s
```

Then with the key:

```text
level=info msg="Restoring latest backup infrahub_backup_20260804_144834.tar.gz.enc from local:/tmp/qs-s5-backups"
level=info msg="Decrypting backup archive..."
level=info msg="Restore completed successfully"
exit=0
```

**Verdict: pass.** `elapsed=0s` against `--sleep 5m` is the ordering proof: the gate ran
before the wait. The container snapshot was unchanged across the refusal, and the pool held
only the `.enc` archive afterwards — the plaintext copy the decryption made was not left
behind.

### Scenario 6 — Plakar parity (FR-004)

Two backup groups were created in `fs:///tmp/qs-s6-plakar-repo`:

```text
BACKUP ID             DATE                       STATUS      …
20260804_145053       2026-08-04T14:50:53Z       complete    …
20260804_145013       2026-08-04T14:50:26Z       complete    …
```

```text
$ restore --latest
level=info msg="Restoring from backup group" backup_id=20260804_145053 components=3 status=complete
exit=0

$ restore
level=info msg="Restoring from backup group" backup_id=20260804_145053 components=3 status=complete
exit=0
```

**Verdict: pass.** Both invocations resolved the same group, and it is the newer of the two.
`restore --latest --s3` on this backend was rejected as the contract requires:

```text
Error: --s3 does not apply to the plakar backend: the repository location comes from --repo, and a plakar restore already resolves the latest complete backup group
exit=1
```

The automated equivalent is `TestDockerPlakar::test_restore_latest_matches_bare_restore`
(T015), observed passing in `234.46s`.

## SC-002 coverage confirmation

> Every defined failure path (empty pool, encrypted-without-key, flag-and-argument conflict)
> exits non-zero with the deployment left untouched — each path covered by an automated test.

| Failure path | Test | What it asserts |
|---|---|---|
| Empty pool | `src/internal/app/restore_latest_test.go::TestResolveLatestBackup` | an empty listing returns an error naming the location; a `List` failure propagates wrapped |
| Empty pool | `src/internal/app/restore_latest_test.go::TestRestoreLatestBackupLocalPool` | the orchestrator surfaces the empty/missing pool without delegating to a restore |
| Empty pool | `tests/e2e/test_docker_tarball.py::test_restore_latest_empty_pool_is_an_error` | non-zero exit, error names the directory, no restore attempted, `compose_container_runtimes` identical before and after, pool not written into |
| Encrypted newest, no key | `src/internal/app/restore_latest_test.go::TestRestoreLatestFromFailFast` | error names the archive and `--decrypt-key`, and the delivery callback was never invoked — so no download and no container action |
| Encrypted newest, no key | `src/internal/app/restore_latest_test.go::TestRestoreLatestFromS3Pool` | the S3 leg's gate refuses before any download is issued |
| Encrypted newest, no key | `tests/e2e/test_docker_encryption.py::test_restore_latest_encrypted_newest` | non-zero exit within 30 s against `--sleep 5m`, no `Sleeping for`, no `Starting backup restore`, container snapshot unchanged, the older readable archive never chosen |
| `--latest` + positional argument | `src/cmd/infrahub-backup/main_test.go::TestResolveRestoreInvocation` | the mutual-exclusion error on both backends |
| `--latest` + positional argument | `src/cmd/infrahub-backup/main_test.go::TestRestoreCommandValidatesBeforeRunning` | the command's `Args`/`RunE` reject the invocation *before* `RunE` reaches any restore call |
| `--s3` without `--latest` | `main_test.go::TestResolveRestoreInvocation` + `TestRestoreCommandValidatesBeforeRunning` | error points at the positional `s3://` URI form; nothing runs |
| plakar `--latest --s3` | `main_test.go::TestResolveRestoreInvocation` + `TestRestoreCommandValidatesBeforeRunning` | rejected; nothing runs |

Every SC-002 path therefore has an automated test asserting a non-zero exit, and for the two
paths that could reach the deployment (empty pool, encrypted newest) an e2e test asserts the
containers were untouched. **SC-002 confirmed.**

## Follow-ups raised, not filed

Three pre-existing, out-of-scope defects are drafted under
[followups/](followups/) and are **not** filed on GitHub — filing is pending approval:

1. `issue-1-s3-uri-local-copy-footgun.md` — the positional `s3://` URI restore truncates and
   then deletes a same-named local archive (critique E1, research D3).
2. `issue-2-neo4j-restore-reads-stale-leftover-dump.md` — Neo4j Community restore loads the
   dump a previous `create` left in `/tmp/infrahubops` instead of the requested archive's.
   Reproduced in this session. Explains the Scenario 1 caveat above.
3. `issue-3-restore-viper-bindings-never-read.md` — `restore` binds `--decrypt-key` and
   `--reset-deployment-id` to viper but reads the local flag variables, so
   `INFRAHUB_DECRYPT_KEY` never reaches a restore.

None of the three is caused by this feature, and none affects the selection behavior
`restore --latest` implements.
