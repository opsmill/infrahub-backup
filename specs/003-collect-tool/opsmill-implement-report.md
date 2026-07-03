# Implementation Report: Infrahub Collect (003-collect-tool)

**Status**: DONE

**Feature**: Infrahub Collect — third CLI binary `infrahub-collect` for read-only troubleshooting-bundle collection from Docker Compose and Kubernetes Infrahub deployments (Jira INFP-415).

**Spec dir**: `specs/003-collect-tool`

**Base commit**: `b1e81e4` (tasks.md committed)

**Head commit**: `452b10e`

**Branch**: `fac/collect-tool-implem-opip6`

All 40 tasks in `tasks.md` are complete (`[X]`), all quality gates pass (`make fmt` / `vet` / `lint` (0 issues) / `test`), and both the Docker and Kubernetes end-to-end suites were observed passing locally. A four-lens review ran across the full diff; all HIGH and MEDIUM findings were fixed inline and re-verified.

## Chunk-by-chunk ledger

| # | Chunk (tasks) | Outcome | Commit(s) | Notes |
|---|---------------|---------|-----------|-------|
| 1 | Setup — binary skeleton & build wiring (T001–T004) | 4 ✅ | `9d66dd6` | Flagged: shared `ConfigureRootCommand` surfaces backup-oriented flags on collect's help (pre-existing, same as taskmanager; not in scope to refactor). |
| 2 | Foundational — framework, manifest, masking, orchestrator (T005–T013) | 9 ✅ | `8b89365` | Backend seam resolved via runtime type-assertion (compile-time `var _ collectBackend` assertions added later). Flagged the masking token list missed `requirepass`/`default_pass` → addressed in chunk 4. |
| 3 | US1 — k8s log primitives & service-log collector (T014–T017, T026) | 5 ✅ | `8ccbff4` | Per-container replica model (CNPG/sidecar-safe). |
| 4 | US1 — parity diagnostics, metrics, run plan (T018–T025) | 8 ✅ | `1c4083c` | Widened masking to `pass` substring (subsumes `password`; catches `requirepass`/`default_pass`); added `maskJSON` for the server API config dump. Live smoke run confirmed masking end-to-end. |
| — | Bug fix surfaced by the US1 e2e | ✅ | `e57ef94` | `GetAllPods` lacked the pod-name substring fallback `getPodForService` has, so `infrahub-server` logs were silently skipped under the official Helm chart labels (`infrahub/service=server`). Fixed before the e2e went green. |
| 5 | US1 — k8s e2e (T027–T028) | 2 ✅ | `e79d26e` | Passed locally against a real vcluster+Helm Infrahub 1.10.2 stack (111s): multi-replica logs, induced-restart `.previous.log`, SC-003 before/after read-only snapshot. |
| 6 | US2 — Docker primitives & docker e2e (T029–T032) | 4 ✅ | `df5ea15` | Discovered Infrahub services log to **stderr** → added a combined stdout+stderr pipe primitive (plain stdout would have produced near-empty logs). Stopped services report `failed`, not vanish. 4 docker e2e tests passed locally. |
| 7 | US3 — `--include-backup` (T033–T035) | 3 ✅ | `5ac844c` | Delegates to `CreateBackup` unmodified with `force=false` (preserves the Principle II running-task safety wait); added a documented optional `artifact` manifest field. Full docker e2e re-ran 5/5. |
| 8 | Opt-in benchmark (T036) | 1 ✅ | `86b7bde` | Ported the real image `registry.opsmill.io/opsmill/bench` from upstream `tasks/container_ops.py` (verified via `docker manifest inspect`), `INFRAHUB_BENCHMARK_IMAGE` override. Success + pull-failure (→ `skipped`) paths verified live. |
| 9 | Polish — AGENTS.md, CI wiring, docs (T037–T040) | 4 ✅ | `a78a82d`, `bf60289`, `f8bf345` | AGENTS.md updated to three-binary architecture (constitution follow-up); CI e2e jobs already select collect tests by marker (only relabeled the misleading build step); Go-version doc fix; corrected the benchmark "generates load" wording (it measures host disk/RAM/CPU). |

## Tasks not completed

None. All 40 tasks are `[X]` in `tasks.md`.

## Local-pass evidence

The implementation added **69 Go unit test functions** (89 including subtests, all passing) plus two Python e2e suites. Full unit evidence lives in the chunk transcripts; representative and safety-critical tests below (all run on Linux 6.8.0-94, Go 1.26.4, `-tags untested_go_version`, worktree `fac/collect-tool-implem-opip6`):

| Test id | Type | Run command | Passed at (ISO 8601) | Environment context | Verbatim pass line |
|---------|------|-------------|----------------------|---------------------|--------------------|
| `TestRunCollectPlan_OutcomeRecording` | unit | `go test -tags untested_go_version ./src/internal/app/` | 2026-07-03T17:31:25Z | n/a | `--- PASS: TestRunCollectPlan_OutcomeRecording (0.10s)` |
| `TestRunCollectPlan_ReadOnlyGuard` | unit | `make test` | 2026-07-03T20:xx (chunk-fix) | n/a | `--- PASS: TestRunCollectPlan_ReadOnlyGuard (0.00s)` |
| `TestRunCollectPlan_CollectorPanicRecovered` | unit | `make test` | 2026-07-03T20:xx | n/a | `--- PASS: TestRunCollectPlan_CollectorPanicRecovered (0.00s)` |
| `TestCollectDatabaseLogs_IncludeQueries` | unit | `make test` | 2026-07-03T20:xx | n/a | `--- PASS: TestCollectDatabaseLogs_IncludeQueries (0.00s)` |
| `TestAggregatingCollector_TimeoutReasonSurvivesAggregation` | unit | `make test` | 2026-07-03T20:xx | n/a | `--- PASS: TestAggregatingCollector_TimeoutReasonSurvivesAggregation (0.00s)` |
| `TestGetAllPodsWith_Branches` | unit | `make test` | 2026-07-03T20:xx | n/a | `--- PASS: TestGetAllPodsWith_Branches (0.00s)` |
| `TestIsSensitiveKey` / `TestMaskEnvOutput` / `TestMaskErlangConfig` / `TestMaskJSON` | unit | `go test -tags untested_go_version ./src/internal/app/` | 2026-07-03T17:31:25Z | n/a | `--- PASS: TestMaskJSON (0.00s)` |
| `TestBundleManifest_JSONShape` / `_Write` | unit | `make test` | 2026-07-03T17:31:25Z | n/a | `--- PASS: TestBundleManifest_JSONShape (0.00s)` |
| `TestReplicaLogFilename` / `TestParsePodContainerStatuses` | unit | `go test -tags untested_go_version ./src/internal/app/` | 2026-07-03T17:41:15Z | n/a | `--- PASS: TestReplicaLogFilename (0.00s)` |
| `TestParseComposePSContainers` / `TestRunCommandCombinedPipeContext_MergesStderr` | unit | `go test -tags untested_go_version ./src/internal/app/` | 2026-07-03T18:29:20Z | n/a | `--- PASS: TestParseComposePSContainers (0.00s)` |
| `TestIncludeBackupCollector_OutcomeRecording` | unit | `go test -tags untested_go_version ./src/internal/app/` | 2026-07-03T18:51:33Z | n/a | `--- PASS: TestIncludeBackupCollector_OutcomeRecording (0.00s)` |
| `TestBenchmarkCollector_OutcomeRecording` / `TestResolveBenchmarkImage` | unit | `go test -tags untested_go_version ./src/internal/app/` | 2026-07-03T19:17:27Z | n/a | `--- PASS: TestBenchmarkCollector_OutcomeRecording (0.00s)` |
| `tests/e2e/test_k8s_collect.py::test_collect_k8s_bundle` | e2e | `uv run pytest -v -m k8s tests/e2e/test_k8s_collect.py` | 2026-07-03T18:17:52Z | vcluster 0.33.0, chart `oci://registry.opsmill.io/opsmill/chart/infrahub` 4.29.4 (Infrahub 1.10.2, community); cluster torn down by fixture | `tests/e2e/test_k8s_collect.py::test_collect_k8s_bundle PASSED` / `1 passed, 1 warning in 111.09s` |
| `tests/e2e/test_docker_collect.py` (5 tests: full_bundle, project_selection, log_lines, degraded_cache, include_backup) | e2e | `uv run pytest -v -m docker tests/e2e/test_docker_collect.py` | 2026-07-03T20:31:16Z (post-fix re-run) | fixture compose project `infrahub-test-<uuid>`, image `registry.opsmill.io/opsmill/infrahub:1.8.2`, task-worker scaled to 2, docker 29.2.1 | `=================== 5 passed, 1 warning in 219.12s ===================` |

Whole-suite confirmation (orchestrator, post-fix): `make test` → `ok infrahub-ops/src/internal/app 0.478s`; 89 `--- PASS` lines, zero failures/skips.

## Review findings

A four-lens review (correctness/guidelines, error handling, test coverage, types/comments/simplify) ran across `b1e81e4..HEAD`. The types/comments/simplify lens was performed by the orchestrator inline after two subagent spawn failures. Findings and disposition:

| Severity | Lens | File | Finding | Disposition |
|----------|------|------|---------|-------------|
| HIGH | correctness | `collect_diagnostics.go` (via both backends' `CopyFrom`) | Neo4j-log copy used context-free `runCommand` → could hang the run forever on a wedged container (worse with `--include-queries`), defeating the "never hang" contract. | **Fixed** `4e91c27` — added `contextCopier`/`CopyFromContext` (bounded by `collectTransferTimeout`) on both backends; shared `CopyFrom` untouched for backup. |
| HIGH | errors | `environment_kubernetes.go` | `ServiceReplicas` mapped *every* `GetAllPods` error (incl. API/RBAC/cluster failure) to "service not deployed"/skipped → exit-0 empty bundle that looks intentional. | **Fixed** `4e91c27` — `errNoPodsMatched` sentinel; genuine empty match → skipped, real kubectl failure → recorded `failed`. |
| HIGH | tests | (missing) | FR-004 `--include-queries` (customer-data-sensitive path) had zero test coverage. | **Fixed** `452b10e` — `TestCollectDatabaseLogs_IncludeQueries` (default / enumerated / empty-fallback). |
| HIGH | tests | (missing) | FR-010 read-only had no cheap unit guard (only the env-gated k8s e2e). | **Fixed** `452b10e` — `TestRunCollectPlan_ReadOnlyGuard` drives the full plan and fails on any Start/Stop/scale. |
| MEDIUM | errors/correctness | `collect.go` loop | No `recover()` around collectors → a panic aborts the whole run (violates FR-009). | **Fixed** `4e91c27` — `safeRun`/`safeSkip` convert panics to a recorded `failed`. |
| MEDIUM | errors/correctness | aggregating collectors | Timeout `*timeoutError` lost through `%v`/`%s` string aggregation → manifest reason was a verbose composite, not the contract's bare `timed out after <d>`; the "covering" test bypassed the real path. | **Fixed** `4e91c27`/`452b10e` — timeout `%w`-wrapped through aggregation; new `TestAggregatingCollector_TimeoutReasonSurvivesAggregation` exercises the real path. |
| MEDIUM | correctness | `collect_diagnostics.go`, `app.go` | server-info version/internal-address lookups + k8s pod resolution on the collect path were unbounded. | **Fixed** `4e91c27` — bounded collect-local variants; shared helpers untouched for backup. |
| LOW | correctness | `collect_logs.go` | Docker combined-pipe reader not closed (fd leak). | **Fixed** `4e91c27` — `defer reader.Close()`. |
| LOW | errors | `collect_diagnostics.go` | `--include-queries` empty enumeration → silent empty `database/` success. | **Fixed** `4e91c27` — falls back to `neo4j.log`+`debug.log`. |
| LOW | errors | `collect_diagnostics.go` | `events.json` could contain a Python traceback on failure. | **Fixed** `4e91c27` — written only on success; failure output → `events.err.txt`. |
| LOW | errors | `collect_prefect_events.py` | Pagination could spin on a truthy `next_page` with zero events. | **Fixed** `4e91c27` — max-pages cap + break on empty page. |
| LOW-MED | tests | `collect_manifest_test.go` | Brittle `len==8` field-count assertion contradicts the schema's `additionalProperties: true`. | **Fixed** `452b10e` — asserts required-field presence instead. |
| LOW | types (retracted) | both backends | Suspected missing compile-time `var _ collectBackend` assertions. | **Not a defect** — assertions exist in a grouped `var(...)` block (initial grep false-negative). Retracted after verification. |
| LOW / by-design | correctness | `masking.go` | Key-name masking misses connection-string credentials whose key lacks pass/secret/token/key (e.g. `..._URL=postgresql://user:pw@…`); multi-line env values mask only the first line. | **Deferred** — explicit FR-008 parity-with-Python decision; user docs already warn to review bundles before sharing. |
| LOW | correctness | `utils.go` `createTarball` | A second SIGINT mid-archive-write could leave a truncated file under the final name (no temp-then-rename). | **Deferred** — very narrow window; single SIGINT is handled cleanly. |
| LOW / by-design | correctness | `backup_containers.go` (via `--include-backup`) | Inherited `waitForRunningTasks` can block on a busy instance. | **Deferred** — documented delegated backup behavior (R10), outside collect's own contract. |

## Autonomous decisions

- **Masking token list widened to `pass`** (chunk 4): FR-008 specifies the list as a *minimum* ("at least keys containing password/secret/token/key"), so adding `pass` to catch `requirepass`/`default_pass` is in-spec; over-masking is safe. `maskJSON` was added for the server API config dump named in research R5.
- **`--include-backup` uses `force=false`** (chunk 7): preserves the backup tool's running-task safety wait (Principle II) at the cost of a possible long wait on a busy instance — safety was chosen over bounded runtime. The unbounded wait is recorded as a deferred LOW review item.
- **Benchmark image ported, not guessed** (chunk 8): fetched the real reference from upstream `opsmill/infrahub` `tasks/container_ops.py` and verified it exists; the docs' "generates load" claim was corrected to reflect that it measures host requirements.
- **Types/comments/simplify review done inline** (Phase 6): the review subagent failed to run twice (0 tool uses, boilerplate output containing a spurious "System:"-style line, which was disregarded as data, not instruction); rather than retry a third time, the orchestrator performed that lens directly and verified each finding against the code — which caught and retracted one false-positive.
- **Review scope** was `b1e81e4..HEAD` (this run), not the broader `main...HEAD` the detect script reports — earlier branch commits (backup docs) predate this run and were out of scope.
- **k8s e2e not re-run after the fixes**: the fixes' Kubernetes logic (FIX-2 sentinel, FIX-5 bounded resolution) is covered by new unit tests (`TestGetAllPodsWith_Branches`, pod-resolution tests); the docker e2e was re-run 5/5 green because the fixes touched shared diagnostics/backend code.

## Suggested next steps

1. **Open a PR** for `fac/collect-tool-implem-opip6` → `main`. CI will run the Docker and Kubernetes e2e suites (both already carry the right pytest markers, per T038).
2. **Watch the first CI run's wall-clock**: the collect e2e adds ~2–6 min per suite; the existing 30-min job budget was left unchanged (flagged in chunk 9) — bump it if CI comes close.
3. **Optional follow-ups** for the deferred LOW items if desired: temp-then-rename in `createTarball` for the double-SIGINT window, and a docs note about connection-string credentials not being masked (the guide already warns generally).
4. Run `/speckit.opsmill.extract` to capture reusable knowledge/ADRs from this feature (the manual follow-up step this pipeline intentionally leaves to you).
