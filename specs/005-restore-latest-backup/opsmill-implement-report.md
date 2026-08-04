# Implementation Report: Restore the Latest Backup Without Naming an Archive

**Status**: ✅ COMPLETE — all 17 tasks done, local-pass evidence complete, no `MISSING` rows

| | |
|---|---|
| Feature | `005-restore-latest-backup` |
| Spec dir | `specs/005-restore-latest-backup` |
| Base commit | `9b459cb` (`docs(spec): spec/ask alignment check — aligned (005)`) |
| Head commit | `7734f16` (`fix(005): correct overclaimed guarantees and refuse plakar pool selection`) |
| Commits | 13 (12 implementation + 1 review fix) |
| Wall-clock | ~3h20m (13:12Z → 16:30Z), dominated by e2e runs at ~150–235s each |
| Gates at head | `make build` ✅ · `make test` ✅ · `make vet` ✅ · `make lint` ✅ `0 issues.` |

## 1. Chunk-by-chunk ledger

### Chunk 1 — Phase 1: Setup (1 task)

✅ 1 · ⚠️ 0 · ❌ 0 — commit `8391b2a`

Baseline was fully green before any change; `go.mod`, `go.sum`, and `flake.nix` all
unmodified, so `scripts/update-vendor-hash.sh` was correctly not needed — as the plan
predicted (no new dependencies).

**Flagged up**: `make lint` needs `$(go env GOPATH)/bin` on `PATH`; `golangci-lint` was
already installed. Passed to every later chunk.

### Chunk 2 — Phase 2: Foundational (6 tasks)

✅ 6 · ⚠️ 0 · ❌ 0 — commits `34a3ae8`, `0baab45`, `3d0978f`

Selection core, orchestrator, and the full CLI surface. 7 new test functions, ~30 subtests.

**Flagged up**:

- Factored the FR-007 fail-fast gate and the FR-009 log line into a shared
  `restoreLatestFrom(ctx, loc, decryptKey, deliver)` seam so the S3 arm inherits the
  ordering rather than re-implementing it. Good structural call — the `deliver` seam is
  also what makes "no delegation happened" observable in tests without touching containers.
- `--latest <file>` and `--s3`-without-`--latest` are rejected *before* the backend branch,
  so both backends reject them identically — matching the contract's "(any)" rows.
- **Pre-existing defect found**: `restoreCmd` binds `decrypt-key` / `reset-deployment-id`
  to viper, but `RunE` reads the local flag variables, so `INFRAHUB_DECRYPT_KEY` never
  reaches a restore. `--latest` inherits it. → follow-up draft 3.

### Chunk 3 — Phase 3: User Story 1, the S3 leg (4 tasks)

✅ 4 · ⚠️ 0 · ❌ 0 — commits `93e0aa2`, `a5d7066`

**Flagged up**:

- Introduced a 2-method `s3RestoreClient` interface (`buildS3Key`, `Download`) so the
  download leg is unit-testable without a bucket; download context bounded at 30 min,
  matching the existing positional-URI download.
- Added `os.MkdirAll` before `os.CreateTemp` (mirroring `downloadBackupFromS3`) — without
  it the P1 flow fails on a staging host that has never taken a local backup.
- **Pre-existing defect found**: `restoreNeo4j` loads a stale leftover dump. → draft 2.
- **Deviation**: T011's "newer local-only archive" is a fabricated name, not a second real
  `create`. See §6.

### Chunk 4 — Phase 4: User Story 2, local journey (2 tasks)

✅ 2 · ⚠️ 0 · ❌ 0 — commits `240d6a6`, `ec357f4`

**Flagged up**:

- **Deviation**: T012's older pool entry is also a fabricated name. Its first attempt used
  two real `create`s and the restore **failed hard** —
  `Not a valid Neo4j archive: /tmp/infrahubops/neo4j.dump`, exit 1. See §6.
- T013 went into a new `tests/e2e/test_docker_encryption.py`: there was no existing
  encryption e2e coverage to sit alongside (`test_docker_retention.py` only fabricates
  `.enc` *names*).
- Added `run_cli` (non-raising) and `compose_container_runtimes` to `tests/helpers/utils.py`.

### Chunk 5 — Phase 5: Polish (4 tasks)

✅ 4 · ⚠️ 0 · ❌ 0 — commits `215d6cf`, `f722b28`, `f0d1003`, `23d8a4e`

**Flagged up**:

- **Deviation**: T014 named `README.md`, but this README is a 47-line pointer with no
  command documentation. Substance went to `docs/docs/backup/restore.mdx` and
  `docs/docs/reference/commands.mdx` (where spec 004 put its equivalent), with a README
  pointer. See §6.
- **T016 filed nothing on GitHub** — drafts only, by orchestrator instruction. See §6.
- **Corrected the orchestrator's prediction**: quickstart Scenario 1 did *not* fail. It
  exited 0 with the correct FR-009 line, because there the leftover dump happens to *be*
  the requested archive. It reproduced the defect directly instead, and more damningly:
  `restore <older archive by name>` reported "Restore completed successfully", exit 0, and
  the marker tag unique to that archive was **absent** afterwards — no `--latest` involved.
- Minor pre-existing defect: plakar runs leave an untracked `2.0.0/store/` kloset cache in
  the repo root; it is not in `.gitignore` and T015's test will produce it in CI. Removed,
  not committed, not drafted as an issue.
- `vale` is not installed in this environment, so the Vale style pass could not run.

## 2. Tasks not completed

None. All 17 tasks are `[X]` in `tasks.md`; `grep -c '^- \[ \]'` returns 0.

One task completed in modified form: **T016** produced ready-to-file issue drafts instead
of filing GitHub issues, by orchestrator instruction (§6). Nothing was silently dropped.

## 3. Local-pass evidence

Every test added or modified by this run, with an observed passing run. Environment shared
by all Go rows: repo worktree `fac/restore-latest-backup-xmj2m`, `export PATH="$PATH:$(go env GOPATH)/bin"`,
Go 1.25, no deployment or S3 endpoint required. Environment shared by all e2e rows: live
Infrahub Docker Compose deployment via `infrahub-testcontainers` 1.8.2 (project
`infrahub-test-<hex>`, passed to the binary with `--project`), Neo4j Community, Docker
29.2.1, pytest 9.0.2 / Python 3.14.3, `bin/infrahub-backup` rebuilt by the `backup_binary`
fixture.

| Test id | Type | Run command | Passed at (ISO 8601) | Environment context | Verbatim pass line |
|---|---|---|---|---|---|
| `TestResolveLatestBackup` (6 subtests) — `src/internal/app` | unit | `go test ./src/internal/app -run 'TestResolveLatestBackup' -v` | 2026-08-04T13:20:50Z | n/a | `--- PASS: TestResolveLatestBackup (0.00s)` / `ok  infrahub-ops/src/internal/app 0.004s` |
| `TestResolveLatestBackupMatchesRetentionRanking` — `src/internal/app` | unit | same as above | 2026-08-04T13:20:50Z | n/a | `--- PASS: TestResolveLatestBackupMatchesRetentionRanking (0.00s)` |
| `TestRestoreLatestFromFailFast` (5 subtests) — `src/internal/app` | unit | `go test ./src/internal/app -run 'TestRestoreLatest' -v` | 2026-08-04T13:21:32Z | n/a | `--- PASS: TestRestoreLatestFromFailFast (0.00s)` |
| `TestRestoreLatestFromPropagatesDeliveryFailure` — `src/internal/app` | unit | same as above | 2026-08-04T13:21:32Z | n/a | `--- PASS: TestRestoreLatestFromPropagatesDeliveryFailure (0.00s)` |
| `TestRestoreLatestBackupLocalPool` (5 subtests, incl. the plakar guard added at review) — `src/internal/app` | unit | `go test ./src/internal/app -count=1 -run 'TestRestoreLatestBackupLocalPool' -v` | 2026-08-04T16:24:11Z | n/a | `--- PASS: TestRestoreLatestBackupLocalPool (0.00s)` / `ok  infrahub-ops/src/internal/app 0.004s` |
| `TestDownloadLatestS3BackupUsesACollisionProofTempPath` — `src/internal/app` | unit | `go test ./src/internal/app/ -count=1 -run 'TestDownloadLatestS3Backup\|TestRestoreLatestFromS3Pool\|TestRestoreLatestBackupLocalPool' -v` | 2026-08-04T14:03:44Z | n/a | `--- PASS: TestDownloadLatestS3BackupUsesACollisionProofTempPath (0.00s)` |
| `TestDownloadLatestS3BackupCleansUpTheTempDownload` (2 subtests) — `src/internal/app` | unit | same as above | 2026-08-04T14:03:44Z | n/a | `--- PASS: TestDownloadLatestS3BackupCleansUpTheTempDownload (0.00s)` |
| `TestRestoreLatestFromS3Pool` (2 subtests) — `src/internal/app` | unit | same as above | 2026-08-04T14:03:44Z | n/a | `--- PASS: TestRestoreLatestFromS3Pool (0.00s)` |
| `TestResolveRestoreInvocation` (14 subtests) — `src/cmd/infrahub-backup` | unit | `go test ./src/cmd/infrahub-backup -run 'TestResolveRestoreInvocation\|TestRestoreCommandValidatesBeforeRunning' -v` | 2026-08-04T13:23:20Z | n/a | `--- PASS: TestResolveRestoreInvocation (0.00s)` |
| `TestRestoreCommandValidatesBeforeRunning` (8 subtests) — `src/cmd/infrahub-backup` | unit | same as above | 2026-08-04T13:23:20Z | n/a | `--- PASS: TestRestoreCommandValidatesBeforeRunning (0.00s)` / `ok  infrahub-ops/src/cmd/infrahub-backup 0.004s` |
| `tests/e2e/test_docker_s3.py::TestDockerS3::test_restore_latest_from_s3` | e2e | `uv run pytest -v -m docker tests/e2e/test_docker_s3.py -k test_restore_latest_from_s3` | 2026-08-04T14:03:13Z | + session-scoped `minio_docker` (`minio/minio:RELEASE.2025-09-07T16-13-09Z`, bucket `backups`, per-run `--s3-prefix restore-latest-<hex>`) | `... PASSED [100%]` / `1 passed, 1 deselected, 1 warning in 147.92s (0:02:27)` |
| `tests/e2e/test_docker_tarball.py::TestDockerTarball::test_restore_latest_from_local_pool` | e2e | `uv run pytest -v -s -m docker tests/e2e/test_docker_tarball.py -k restore_latest` | 2026-08-04T14:23:17Z | one real `create --force` + one fabricated older archive name in `tmp_path/latest-pool` | `... PASSED` / `2 passed, 1 deselected, 1 warning in 145.82s (0:02:25)` |
| `tests/e2e/test_docker_tarball.py::TestDockerTarball::test_restore_latest_empty_pool_is_an_error` | e2e | same as above | 2026-08-04T14:23:17Z | fresh empty `BACKUP_DIR=/tmp/pytest-of-root/pytest-1186/test_restore_latest_empty_pool0/empty-pool`, passed as env var | `... PASSED` / `2 passed, 1 deselected, 1 warning in 145.82s (0:02:25)` |
| `tests/e2e/test_docker_encryption.py::TestDockerEncryption::test_restore_latest_encrypted_newest` | e2e | `uv run pytest -v -s -m docker tests/e2e/test_docker_encryption.py` | 2026-08-04T14:25:46Z | per-test `keygen`; real `create --force --encrypt-key` → `.tar.gz.enc`; fabricated older unencrypted name as the no-fallback control | `PASSED` / `1 passed, 1 warning in 146.64s (0:02:26)` |
| `tests/e2e/test_docker_plakar.py::TestDockerPlakar::test_restore_latest_matches_bare_restore` | e2e | `uv run pytest -v -m docker tests/e2e/test_docker_plakar.py -k test_restore_latest_matches_bare_restore` | 2026-08-04T14:37:05Z | plakar repository on local fs under pytest `tmp_path` | `... PASSED [100%]` / `1 passed, 2 deselected, 1 warning in 234.46s (0:03:54)` |

No `MISSING` rows. No `deferred — local E2E not supported` rows: every e2e test added by
this feature was executed locally against real containers.

**Notable evidence detail** — the FR-007 fail-fast assertion is the strongest single piece:
`restore --latest --sleep 5m` against an encrypted newest archive with no key was refused in
**0.00s** measured wall-clock against a 300s wait (asserted `< 30.0`), corroborated by the
absence of `Sleeping for` / `Starting backup restore` in the output and by
`compose_container_runtimes` being byte-identical before and after.

Full-suite caveat: the complete e2e suite (`uv run pytest -m docker tests/e2e`) was **not**
run end to end. Each new test was run individually, plus a live manual quickstart walk. CI
runs the full suite.

## 4. Review findings

Six review agents (code, errors, tests, types, comments, simplify) ran in parallel over
`9b459cb..23d8a4e`. All were briefed to skip the already-known pre-existing defects.
Fixed inline in `7734f16`; everything else deferred.

| Sev | File | Finding | Disposition |
|---|---|---|---|
| high | `restore_latest.go:180`, `docs/docs/backup/restore.mdx:138` | Both claimed the restore performs "version compatibility checks". No such check exists — versions are only logged. | **fixed** |
| high | `restore_latest.go:88` | Comment claimed everything that can refuse a run happens before delivery; `RestoreBackup` still rejects `--decrypt-key`-on-unencrypted *after* the sleep and download. | **fixed** (comment now names the remaining gate) |
| high | `main_test.go:493` | Comment claimed the test pins the published flag names; it registers its own flags, so a rename would not fail it. | **fixed** |
| high | `restore_latest.go:182` | `RestoreLatestBackup` had no backend precondition; on plakar it would list, download, discard, and restore the repo instead — reporting success. Unreachable from the CLI, but the invariant sat in the wrong package (constitution I). | **fixed** + test |
| medium | `backup.go` decrypt path | **Data loss.** Decrypting `…_X.tar.gz.enc` writes to `…_X.tar.gz` and then deletes it, so a pool holding both members of a same-timestamp pair loses one. Pre-existing, but `--latest`'s `.enc`-preferring tiebreak reaches it without the operator naming the archive. | **deferred** → draft 4 |
| high→med | `restore_latest_test.go:577` | Tests agent claimed the S3 branch has no unit coverage and a `filepath.Join(dir, ref.Name)` regression would pass. **Overstated** — the tests call the real `downloadLatestS3Backup` (lines 429, 529, 587), so that regression *would* fail. True narrower gap: the `if s3` composition (`ValidateConfig` → `NewS3Client` → same client to both) is e2e-only. | **deferred**, severity corrected |
| high | `restore_latest.go:182-191` | No test asserts the 7 parameters are forwarded to `RestoreBackup`; four adjacent bools are transposable and every unit call site exercises only refusal paths. | **deferred** — closing it needs a DI seam |
| medium | `restore_latest.go:104` | The encryption gate is one-directional; no single `--latest` command line works for a pool whose newest archive may or may not be encrypted. | **deferred** — a contract addition, not a bug against the contract as written (see §6) |
| medium | `restore_latest.go:138` | Orphaned `restore-latest-*.download` temps leak unboundedly if the host dies mid-download; invisible to `prune`, no warning. | deferred |
| medium | `restore_latest.go:161` | The hard 30-min download cap is indistinguishable from a network fault and is undocumented. | deferred |
| medium | `restore_latest.go:196` | `ValidateConfig` error returned bare, no `%w`, message written for a different feature. Matches the `prune` precedent. | deferred |
| medium | `restore_latest.go:214-219` | Local leg has no analogue of the S3 leg's local-copy safety. | deferred (same root as draft 4) |
| medium | `restore_latest_test.go:282` | "missing directory" subtest asserts only `err != nil` + dir-in-message, which the empty-pool error also satisfies. | deferred |
| medium | `test_docker_encryption.py:116` | Both fail-fast assertions would also pass if `--sleep` were never plumbed in; no test asserts `--latest` honours `--sleep` on a success path. Threshold itself is sound. | deferred |
| medium | `main_test.go:560` | Drives a replica cobra command, so reverting `restoreCmd.Args` leaves the suite green. | deferred |
| medium | `docs/…/restore.mdx:163` | Conflates the empty-pool and missing-directory errors, which differ. | deferred |
| medium | `docs/…/restore.mdx:180` | Cron recipe sets bucket/prefix but no credentials; `/etc/cron.d` inherits almost no environment, so a copy-paste fails at download. | deferred |
| medium | `main.go:77` | `--s3`-without-`--latest` error points plakar users at a positional form plakar ignores. | deferred |
| medium | types | `loc` and `deliver` are unbound parallel values that must agree on the pool; `restoreRequest` can represent states its validator never produces. | deferred |
| high (reuse) | `tests/e2e/*.py` | `_archives` is byte-identical in three e2e files, and `_older_archive_name`/`_newer_archive_name` are the same function sign-flipped — 5 copies where 2 shared helpers do. | deferred (advisory) |
| low | various | Discarded `os.Remove` error; plakar silently discards a positional archive; `--force` inert on tarball restore; 2× disk transiently for S3+encrypted; critique-ID residue in test comments; `--force` undocumented; invented default path in docs; `2.0.0/` not gitignored; several simplifications. | deferred |

Simplify agent confirmed **no duplicated ranking or name-parsing logic** — both legs rank
through `sortBackupRefsNewestFirst` and filter through `backupNamePattern`, and not reusing
`retentionLegsForPrune` was correct (it erases the concrete client the download needs and
always prepends the local leg, which the one-pool rule forbids).

## 5. Autonomous decisions

1. **T016 drafts instead of filed issues.** Publishing to `opsmill/infrahub-backup` is
   outward-facing and not reversible by me, so I instructed Chunk 5 not to run
   `gh issue create`. Four drafts sit in `specs/005-restore-latest-backup/followups/`
   awaiting approval. Revisit if you wanted them filed directly.
2. **T016 widened from one defect to four.** It was scoped to the positional `s3://`
   footgun; three more surfaced during implementation and review. Drafting all four kept
   the "must not be silently dropped" intent.
3. **The one-directional encryption gate was NOT changed.** Two agents flagged it. Adding a
   reverse pre-flight check would *narrow accepted input* in a way `contracts/cli.md` does
   not specify, and would reject an encrypted-content archive whose name lacks `.enc` — a
   case the current content-based check handles. I corrected the false comment and deferred
   the semantic change as a spec decision. **This is the call most worth revisiting**: the
   operational complaint is real — a nightly job cannot pass one command line that works
   whether or not tonight's newest archive is encrypted.
4. **Fabricated archive names in three e2e tests (T011, T012, T013).** Deviation from task
   text. T012's was demonstrably forced — two real `create`s made the restore fail on the
   pre-existing Neo4j defect. **Consequence: no test exercises a local pool holding two
   genuinely-created archives.** Chunk 5 later ran that exact scenario successfully in the
   quickstart walk, so the substitution may not have been necessary; the two observations
   conflict and the surface symptom is evidently state-dependent.
5. **T014 documented in `docs/docs/` rather than `README.md`.** The README is a 47-line
   pointer; a flag table there would fight the repo's docs architecture. Substance went
   where spec 004 put its equivalent, with a README pointer. Verified both files exist.
6. **Review agents run in parallel, not sequentially.** The skill's default is sequential;
   they are read-only, so parallel is safe and saved ~6× wall-clock.
7. **Severity corrected downward on one agent finding** (the S3-coverage HIGH) after
   checking the test file directly. Agents are not taken at face value.
8. **The Neo4j defect was not fixed.** It is pre-existing, independent of `--latest`, and
   sits in the shared restore path; a fix needs its own spec. Escalated as draft 2.

## 6. Suggested next steps

> **Update after the report was written**: the four drafts were approved and filed on
> `opsmill/infrahub-backup` — [#156](https://github.com/opsmill/infrahub-backup/issues/156)
> (Neo4j stale dump), [#157](https://github.com/opsmill/infrahub-backup/issues/157) (decrypt
> overwrites a same-timestamp archive), [#158](https://github.com/opsmill/infrahub-backup/issues/158)
> (positional `s3://` footgun), [#159](https://github.com/opsmill/infrahub-backup/issues/159)
> (dead viper bindings) — each labelled `type: bug` + `claude-code-assisted`. Step 2 below is
> therefore done. The branch was pushed and a draft PR opened.

1. **Read [#156](https://github.com/opsmill/infrahub-backup/issues/156) first.** `restore` after a `create` on the same host does not
   reliably restore the requested archive — it reads a leftover dump from `/tmp/infrahubops`.
   Observed both as a silent wrong-data restore ("Restore completed successfully", exit 0,
   marker tag absent) and as a hard failure. This is a data-integrity defect in a backup
   tool and it is independent of this feature; it deserves attention before `--latest` is
   relied on for an unattended prod→staging sync.
2. **Decide on the four issue drafts** in `specs/005-restore-latest-backup/followups/` and
   file the ones you want. Nothing has been pushed to GitHub.
3. **Decide the one-directional gate question** (§5.3). If a nightly job should be able to
   pass a single command line for a possibly-encrypted pool, that is a small spec change
   plus a few lines in `restoreLatestFrom`.
4. **Consider closing the parameter-forwarding test gap** (§4). An options struct would fix
   the transposition hazard and the test gap together, but it touches `RestoreBackup`'s
   signature — a deliberate decision, not a review fix.
5. Add `2.0.0/` to `.gitignore` before T015's plakar test runs in CI.
6. Open a PR. Nothing has been pushed; the branch is 13 commits ahead of `9b459cb`.
