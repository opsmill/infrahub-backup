# Implementation Report: Storage Figures for the Task Manager Database in Troubleshooting Bundles

## Run summary

- Feature: storage figures for the task manager database in `infrahub-collect` bundles (issue #181)
- Spec directory: `/root/emdash/worktrees/infrahub-backup-07d2b3e1/fac-report-taskmgr-storage-usage-d8y7s/specs/006-taskmgr-db-storage`
- Branch: `fac/report-taskmgr-storage-usage-d8y7s`
- Base commit: `a8b9c76` (docs(specs): add PRD alignment check for task manager database storage)
- Head commit before this report: `493749d`
- Wall-clock time: about 2 hours 55 minutes, from 2026-10-26T17:45Z to 2026-10-26T20:40Z
- Status: DONE. All 46 tasks are ticked, and every new or modified test has local-pass evidence.
- Rebased on `origin/main` at `fa67557` on 2026-10-27. Unless a line says that it ran after the rebase, the commit SHAs in this report are from before the rebase. The section "Rebase on `main`" lists the SHA of each commit after the rebase.

## Results of each chunk

| # | Chunk (tasks.md phase) | Tasks | ✅ / ⚠️ / ❌ | Commits | Flagged by the subagent |
|---|---|---|---|---|---|
| 1 | Phase 1: Setup (T001) | 1 | 1 / 0 / 0 | `5dcda69` | `make lint` panics on the host's Go 1.27; the CI-style lint command (`GOTOOLCHAIN=go1.25.0 go run …golangci-lint@v2.7.2 run`) works and was used for every later chunk. |
| 2 | Phase 2, part 1: credentials (T002 to T005) | 4 | 4 / 0 / 0 | `30f30e5`, `a47de65` | The backup tool's warning for an unparsable URL lost its cause in the refactor (fixed in review). The seven constants share one `const` block so the `unused` linter accepted them before use. When `task-manager-db` set no database name, the database defaulted to `prefect` (changed in review). |
| 3 | Phase 2, part 2: command, error kinds, runner, skip, primary pod (T006 to T013) | 8 | 8 / 0 / 0 | `629a155`, `6e591d5` | The runner masked a successful query's stdout too, which corrupts the output when the password is a common word such as the default `prefect` (changed in chunk 4). The allowlist is a package variable; limits are rounded up to 1 ms so `statement_timeout` is never 0; the classifier also skips kubectl's `command terminated with exit code` line. |
| 4 | Phase 3, Go: storage report and collector (T014 to T019), plus the runner change | 6 | 6 / 0 / 0 | `c7fa7d2`, `464e169` | `storageQuery` does not call `pg_total_relation_size`; the total is data + index + TOAST from one set of size calls, so the sizes always add up. `TestCollectBundle_RegisteredPlan` also lists the plan and was updated. The query was checked against a throwaway `postgres:18-alpine`. |
| 5 | Phase 3, end-to-end tests (T020 to T022) | 3 | 3 / 0 / 0 | `1eccf40` | The assertion that the breakdown collector is `skipped` with `not requested` was deferred to chunk 7, because that collector did not exist yet. The database owner and name are read with `printenv` in `task-manager-db`. The storage collector took 0 to 1 second. |
| 6 | Phase 4, Go: flag, breakdown SQL, parser, collector (T023 to T030) | 8 | 8 / 0 / 0 | `7472b37`, `b48075e` | Flag registration and option reading moved into `addCreateFlags` and `resolveCollectOptions` so they can be tested. Each breakdown query is `SELECT COALESCE(json_agg(b.g), '[]'::json) FROM (… GROUP BY …) b`. The catalog query returns a JSON object of table names. The SQL was checked against a throwaway `postgres:18-alpine`, including a locked `log` table with a 3-second limit. |
| 7 | Phase 4, end-to-end tests (T031 to T034), plus the deferred T021 assertion | 4 | 4 / 0 / 0 | `90e745b` | `task-worker` runs Prefect 3.6.13. The T034 lock is held for up to 300 seconds instead of 120, because the lock also makes the existing `task-manager-state` collector wait for its 60-second limit, so a run takes about 85 seconds. |
| 8 | Phase 5, user documentation (T035 to T039) | 5 | 5 / 0 / 0 | `384c0f6` | Added a paragraph to "Safe to run at any time" in `index.mdx`. Vale reports "autovacuum" as a spelling error, so the pages say "manual and automatic vacuum times". |
| 9 | Phase 5, ADR, developer pages, AGENTS.md, changelog (T040 to T044) | 5 | 5 / 0 / 0 | `fabe088`, `e93ff50` | `AGENTS.md` does not list flags where it describes the collect commands. `plan.md` calls the masking function `maskSecret`; the code calls it `maskSecretValue`. |
| 10 | Phase 5, final gates and the CloudNativePG check (T045, T046) | 2 | 2 / 0 / 0 | `8c0813f` | Scenarios 2 to 4 were cited from chunk 7's runs because no Go or test file changed after them. Scenario 5 ran on a real Compose stack. `quickstart.md` scenario 5 was edited (see "Decisions made without the user"). Before the rebase, the branch lacked `main`'s pod lookup fixes `f673202` and `1e0e522`. |
| R | Review fixes (not a tasks.md phase) | n/a | 7 fixes ✅ | `bad601f`, `b5e28d6`, `493749d` | pgx error text quotes part of the password, so the backup tool's warning does not restore the parse cause. The signatures of `prefectConnectionFromEnv` and `applyPrefectConnection` changed. |

## Tasks not completed

None. All 46 tasks in `tasks.md` are `[X]`, including the optional T046.

## Local-pass evidence

Command keys used in the table:

- U1: `go test -count=1 -tags untested_go_version ./src/internal/app/ -run 'TestPrefectConnectionFromEnv|TestParsePrefectConnection|TestResolveTaskManagerDBCredentials|TestApplyPrefectConnection|TestLoadCredentialsFromEnvironment' -v`
- U2: `make test` (chunk 3)
- U3: `make test` (chunk 4)
- U4: `go test -count=1 -tags untested_go_version ./src/cmd/infrahub-collect/ -v`
- U5: `go test -count=1 -tags untested_go_version ./src/internal/app/ -run 'TestBreakdown|TestParseBreakdown|TestStorageBreakdown|TestTaskManagerDBStorageBreakdownCollector|TestCollectPlanOrdering|TestCollectBundle_RegisteredPlan' -v`
- U6: `go test -count=1 -tags untested_go_version ./src/internal/app/ -run '^(TestApplyPrefectConnection|TestPrefectConnectionFromEnv|TestLoadCredentialsFromEnvironment_WarnsOnUnparsableURL|TestFetchPostgresCredentials_WarnsOnUnparsableURL|TestResolveTaskManagerDBCredentials|TestResolveTaskManagerDBCredentials_WarnsOnUnparsableURL|TestTaskManagerDBStorageCollector_PasswordNeverStaged|TestTaskManagerDBStorageBreakdownCollector_PasswordNeverStaged)$' -v` (the review-fix subagent reported this run as "these 8 tests"; the regex lists the eight tests it named)
- E1: `uv run pytest -v -m docker tests/e2e/test_docker_collect.py -p no:cacheprovider` (run by the orchestrator on `493749d`)
- E2: `VCLUSTER_DIR=<scratch>/vcluster/config.json uv run pytest -v -m k8s tests/e2e/test_k8s_collect.py` (run in chunk 7 on `90e745b`)
- U7: `GOTOOLCHAIN=go1.25.0 go test -count=1 ./...` (run after the rebase on `6f21171`)
- U8: `GOTOOLCHAIN=go1.25.0 go test -count=1 ./src/internal/app/ -run '^(TestGetPodForServiceWith_SelectsPrimaryPod|TestPodResolutionWithPostgresOperatorLabels)$' -v` (run after the rebase on `6f21171`)
- E3: `GOTOOLCHAIN=go1.25.0 uv run pytest -v -m docker tests/e2e/test_docker_collect.py -p no:cacheprovider` (run after the rebase on `6f21171`)
- E4: `GOTOOLCHAIN=go1.25.0 VCLUSTER_DIR=<scratch>/vcluster/config.json uv run pytest -v -m k8s tests/e2e/test_k8s_collect.py -p no:cacheprovider` (run after the rebase on `6f21171`)

All unit tests ran on go1.27.3 linux/amd64 with fake backends and no containers, except U7 and U8, which ran on go1.25.0 linux/amd64. A test that was modified in a later chunk is listed with its latest passing run. The end-to-end tests and `TestGetPodForServiceWith_SelectsPrimaryPod` are listed with their runs after the rebase. A conflict was resolved in the file of that unit test.

| Test id | Type | Run command | Passed at (ISO 8601) | Environment context | Verbatim pass line |
|---|---|---|---|---|---|
| `TestPrefectConnectionFromEnv` (6 subtests) | unit | U6 | 2026-10-26T20:30:26Z | n/a | `--- PASS: TestPrefectConnectionFromEnv (0.00s)` |
| `TestParsePrefectConnection` (5 subtests) | unit | U1 | 2026-10-26T17:55:28Z | n/a | `--- PASS: TestParsePrefectConnection (0.00s)` |
| `TestApplyPrefectConnection` (existing, modified) | unit | U6 | 2026-10-26T20:30:26Z | n/a | `--- PASS: TestApplyPrefectConnection (0.00s)` |
| `TestLoadCredentialsFromEnvironment_WarnsOnUnparsableURL` | unit | U6 | 2026-10-26T20:30:26Z | n/a | `--- PASS: TestLoadCredentialsFromEnvironment_WarnsOnUnparsableURL (0.00s)` |
| `TestFetchPostgresCredentials_WarnsOnUnparsableURL` | unit | U6 | 2026-10-26T20:30:26Z | n/a | `--- PASS: TestFetchPostgresCredentials_WarnsOnUnparsableURL (0.00s)` |
| `TestResolveTaskManagerDBCredentials` (11 subtests) | unit | U6 | 2026-10-26T20:30:26Z | n/a | `--- PASS: TestResolveTaskManagerDBCredentials (0.00s)` |
| `TestResolveTaskManagerDBCredentials_ResolvesOncePerRun` (2 subtests) | unit | U1 | 2026-10-26T17:55:28Z | n/a | `--- PASS: TestResolveTaskManagerDBCredentials_ResolvesOncePerRun (0.00s)` |
| `TestResolveTaskManagerDBCredentials_LogsSourceOnly` | unit | U1 | 2026-10-26T17:55:28Z | n/a | `--- PASS: TestResolveTaskManagerDBCredentials_LogsSourceOnly (0.00s)` |
| `TestResolveTaskManagerDBCredentials_WarnsOnUnparsableURL` | unit | U6 | 2026-10-26T20:30:26Z | n/a | `--- PASS: TestResolveTaskManagerDBCredentials_WarnsOnUnparsableURL (0.00s)` |
| `TestBuildPsqlCommand` (5 subtests) | unit | U2 | 2026-10-26T18:06:50Z | n/a | `--- PASS: TestBuildPsqlCommand (0.00s)` |
| `TestEffectiveDBQueryLimit` (12 subtests) | unit | U2 | 2026-10-26T18:06:50Z | n/a | `--- PASS: TestEffectiveDBQueryLimit (0.00s)` |
| `TestClassifyDBQueryFailure` (38 subtests) | unit | U2 | 2026-10-26T18:06:50Z | n/a | `--- PASS: TestClassifyDBQueryFailure (0.00s)` |
| `TestMaskSecretValue` (3 subtests) | unit | U2 | 2026-10-26T18:06:50Z | n/a | `--- PASS: TestMaskSecretValue (0.00s)` |
| `TestRunTaskManagerDBQuery_ExecArguments` (2 subtests) | unit | U2 | 2026-10-26T18:06:50Z | n/a | `--- PASS: TestRunTaskManagerDBQuery_ExecArguments (0.00s)` |
| `TestRunTaskManagerDBQuery_Timeouts` (4 subtests) | unit | U2 | 2026-10-26T18:06:50Z | n/a | `--- PASS: TestRunTaskManagerDBQuery_Timeouts (0.00s)` |
| `TestRunTaskManagerDBQuery_MasksPassword` (3 subtests) | unit | U3 | 2026-10-26T18:18:48Z | n/a | `--- PASS: TestRunTaskManagerDBQuery_MasksPassword (0.00s)` |
| `TestRunTaskManagerDBQuery_SuccessKeepsQueryResult` | unit | U3 | 2026-10-26T18:18:48Z | n/a | `--- PASS: TestRunTaskManagerDBQuery_SuccessKeepsQueryResult (0.00s)` |
| `TestRunTaskManagerDBQuery_BackendWithoutBoundedExec` | unit | U2 | 2026-10-26T18:06:50Z | n/a | `--- PASS: TestRunTaskManagerDBQuery_BackendWithoutBoundedExec (0.00s)` |
| `TestSkipWhenTaskManagerDBAbsent` (4 subtests) | unit | U2 | 2026-10-26T18:06:50Z | n/a | `--- PASS: TestSkipWhenTaskManagerDBAbsent (0.00s)` |
| `TestGetPodForServiceWith_SelectsPrimaryPod` (4 subtests) | unit | U8 | 2026-10-27T21:22:04Z | n/a, HEAD `6f21171` | `--- PASS: TestGetPodForServiceWith_SelectsPrimaryPod (0.00s)` |
| `TestParseStorageReport` | unit | U3 | 2026-10-26T18:18:48Z | n/a | `--- PASS: TestParseStorageReport (0.00s)` |
| `TestParseStorageReport_SizesAddUp` | unit | U3 | 2026-10-26T18:18:48Z | n/a | `--- PASS: TestParseStorageReport_SizesAddUp (0.00s)` |
| `TestParseStorageReport_Nulls` (2 subtests) | unit | U3 | 2026-10-26T18:18:48Z | n/a | `--- PASS: TestParseStorageReport_Nulls (0.00s)` |
| `TestParseStorageReport_EqualSizesOrder` | unit | U3 | 2026-10-26T18:18:48Z | n/a | `--- PASS: TestParseStorageReport_EqualSizesOrder (0.00s)` |
| `TestParseStorageReport_Malformed` (5 subtests) | unit | U3 | 2026-10-26T18:18:48Z | n/a | `--- PASS: TestParseStorageReport_Malformed (0.00s)` |
| `TestStorageReport_JSONFieldNames` | unit | U3 | 2026-10-26T18:18:48Z | n/a | `--- PASS: TestStorageReport_JSONFieldNames (0.00s)` |
| `TestTaskManagerDBStorageCollector_Success` | unit | U3 | 2026-10-26T18:18:48Z | n/a | `--- PASS: TestTaskManagerDBStorageCollector_Success (0.00s)` |
| `TestTaskManagerDBStorageCollector_DefaultPassword` | unit | U3 | 2026-10-26T18:18:48Z | n/a | `--- PASS: TestTaskManagerDBStorageCollector_DefaultPassword (0.00s)` |
| `TestTaskManagerDBStorageCollector_NotDeployed` | unit | U3 | 2026-10-26T18:18:48Z | n/a | `--- PASS: TestTaskManagerDBStorageCollector_NotDeployed (0.00s)` |
| `TestTaskManagerDBStorageCollector_Failures` (6 subtests) | unit | U3 | 2026-10-26T18:18:48Z | n/a | `--- PASS: TestTaskManagerDBStorageCollector_Failures (0.00s)` |
| `TestTaskManagerDBStorageCollector_PasswordNeverStaged` (3 subtests) | unit | U6 | 2026-10-26T20:30:26Z | n/a | `--- PASS: TestTaskManagerDBStorageCollector_PasswordNeverStaged (0.00s)` |
| `TestBreakdownTables_ListOrder` | unit | U5 | 2026-10-26T18:48:45Z | n/a | `--- PASS: TestBreakdownTables_ListOrder (0.00s)` |
| `TestBreakdownQuery` (7 subtests) | unit | U5 | 2026-10-26T18:48:45Z | n/a | `--- PASS: TestBreakdownQuery (0.00s)` |
| `TestBreakdownCatalogQuery` | unit | U5 | 2026-10-26T18:48:45Z | n/a | `--- PASS: TestBreakdownCatalogQuery (0.00s)` |
| `TestParseBreakdownCatalog` (4 subtests) | unit | U5 | 2026-10-26T18:48:45Z | n/a | `--- PASS: TestParseBreakdownCatalog (0.00s)` |
| `TestParseBreakdownGroups` | unit | U5 | 2026-10-26T18:48:45Z | n/a | `--- PASS: TestParseBreakdownGroups (0.00s)` |
| `TestParseBreakdownGroups_Order` | unit | U5 | 2026-10-26T18:48:45Z | n/a | `--- PASS: TestParseBreakdownGroups_Order (0.00s)` |
| `TestParseBreakdownGroups_OptionalFields` (3 subtests) | unit | U5 | 2026-10-26T18:48:45Z | n/a | `--- PASS: TestParseBreakdownGroups_OptionalFields (0.00s)` |
| `TestParseBreakdownGroups_NoGroups` | unit | U5 | 2026-10-26T18:48:45Z | n/a | `--- PASS: TestParseBreakdownGroups_NoGroups (0.00s)` |
| `TestParseBreakdownGroups_Malformed` (7 subtests) | unit | U5 | 2026-10-26T18:48:45Z | n/a | `--- PASS: TestParseBreakdownGroups_Malformed (0.00s)` |
| `TestStorageBreakdown_JSONFieldNames` | unit | U5 | 2026-10-26T18:48:45Z | n/a | `--- PASS: TestStorageBreakdown_JSONFieldNames (0.00s)` |
| `TestTaskManagerDBStorageBreakdownCollector_NotRequested` (2 subtests) | unit | U5 | 2026-10-26T18:48:45Z | n/a | `--- PASS: TestTaskManagerDBStorageBreakdownCollector_NotRequested (0.00s)` |
| `TestTaskManagerDBStorageBreakdownCollector_NotDeployed` | unit | U5 | 2026-10-26T18:48:45Z | n/a | `--- PASS: TestTaskManagerDBStorageBreakdownCollector_NotDeployed (0.00s)` |
| `TestTaskManagerDBStorageBreakdownCollector_Success` | unit | U5 | 2026-10-26T18:48:45Z | n/a | `--- PASS: TestTaskManagerDBStorageBreakdownCollector_Success (0.00s)` |
| `TestTaskManagerDBStorageBreakdownCollector_AbsentTable` | unit | U5 | 2026-10-26T18:48:45Z | n/a | `--- PASS: TestTaskManagerDBStorageBreakdownCollector_AbsentTable (0.00s)` |
| `TestTaskManagerDBStorageBreakdownCollector_TableFails` (4 subtests) | unit | U5 | 2026-10-26T18:48:45Z | n/a | `--- PASS: TestTaskManagerDBStorageBreakdownCollector_TableFails (0.00s)` |
| `TestTaskManagerDBStorageBreakdownCollector_CatalogFails` (4 subtests) | unit | U5 | 2026-10-26T18:48:45Z | n/a | `--- PASS: TestTaskManagerDBStorageBreakdownCollector_CatalogFails (0.00s)` |
| `TestTaskManagerDBStorageBreakdownCollector_ProgressOutput` | unit | U5 | 2026-10-26T18:48:45Z | n/a | `--- PASS: TestTaskManagerDBStorageBreakdownCollector_ProgressOutput (0.00s)` |
| `TestTaskManagerDBStorageBreakdownCollector_StopsWhenInterrupted` | unit | U5 | 2026-10-26T18:48:45Z | n/a | `--- PASS: TestTaskManagerDBStorageBreakdownCollector_StopsWhenInterrupted (0.00s)` |
| `TestTaskManagerDBStorageBreakdownCollector_PasswordNeverStaged` (3 subtests) | unit | U6 | 2026-10-26T20:30:26Z | n/a | `--- PASS: TestTaskManagerDBStorageBreakdownCollector_PasswordNeverStaged (0.00s)` |
| `TestCollectPlanOrdering` (existing, modified) | unit | U5 | 2026-10-26T18:48:45Z | n/a | `--- PASS: TestCollectPlanOrdering (0.00s)` |
| `TestCollectBundle_RegisteredPlan` (existing, modified) | unit | U5 | 2026-10-26T18:48:45Z | n/a | `--- PASS: TestCollectBundle_RegisteredPlan (0.00s)` |
| `TestResolveCollectOptions_IncludeTaskManagerStorage` (5 subtests) | unit | U4 | 2026-10-26T18:48:45Z | n/a | `--- PASS: TestResolveCollectOptions_IncludeTaskManagerStorage (0.00s)` |
| `TestAddCreateFlags_IncludeTaskManagerStorageHelp` | unit | U4 | 2026-10-26T18:48:45Z | n/a | `--- PASS: TestAddCreateFlags_IncludeTaskManagerStorageHelp (0.00s)` |
| `tests/e2e/test_docker_collect.py::TestDockerCollect::test_collect_full_bundle` (modified) | e2e | E3 | 2026-10-27T21:21:13Z to 21:27:07Z (run window) | Docker Compose project created by the test fixture (its name is not in the log), HEAD `6f21171` | `tests/e2e/test_docker_collect.py::TestDockerCollect::test_collect_full_bundle PASSED [ 14%]` |
| `tests/e2e/test_docker_collect.py::TestDockerCollect::test_collect_task_manager_storage_breakdown` (new) | e2e | E3 | 2026-10-27T21:21:13Z to 21:27:07Z (run window) | as above | `tests/e2e/test_docker_collect.py::TestDockerCollect::test_collect_task_manager_storage_breakdown PASSED [ 57%]` |
| `tests/e2e/test_docker_collect.py::TestDockerCollect::test_collect_task_manager_storage_time_limit` (new) | e2e | E3 | 2026-10-27T21:21:13Z to 21:27:07Z (run window) | as above | `tests/e2e/test_docker_collect.py::TestDockerCollect::test_collect_task_manager_storage_time_limit PASSED [ 71%]` |
| `tests/e2e/test_docker_collect.py::TestDockerCollect::test_collect_project_selection_without_flag` (unchanged; affected by the `tests/helpers/bundle.py` change) | e2e | E3 | 2026-10-27T21:21:13Z to 21:27:07Z (run window) | as above | `tests/e2e/test_docker_collect.py::TestDockerCollect::test_collect_project_selection_without_flag PASSED [ 28%]` |
| `tests/e2e/test_docker_collect.py::TestDockerCollect::test_collect_log_lines_flag_and_env` (unchanged; affected by the helper change) | e2e | E3 | 2026-10-27T21:21:13Z to 21:27:07Z (run window) | as above | `tests/e2e/test_docker_collect.py::TestDockerCollect::test_collect_log_lines_flag_and_env PASSED [ 42%]` |
| `tests/e2e/test_docker_collect.py::TestDockerCollect::test_collect_degraded_cache` (unchanged; affected by the helper change) | e2e | E3 | 2026-10-27T21:21:13Z to 21:27:07Z (run window) | as above | `tests/e2e/test_docker_collect.py::TestDockerCollect::test_collect_degraded_cache PASSED [ 85%]` |
| `tests/e2e/test_docker_collect.py::TestDockerCollect::test_collect_include_backup` (unchanged; affected by the helper change) | e2e | E3 | 2026-10-27T21:21:13Z to 21:27:07Z (run window) | as above | `tests/e2e/test_docker_collect.py::TestDockerCollect::test_collect_include_backup PASSED [100%]` |
| `tests/e2e/test_k8s_collect.py::test_collect_k8s_bundle` (modified) | e2e | E4 | 2026-10-27T21:27:07Z to 21:28:51Z (run window) | vcluster `pytest-78891b09-4469-4d90-8f1f-b266e8f2336f`, kubeconfig `/tmp/pytest-of-root/pytest-2082/vcluster0/kubeconfig`, default Helm chart, HEAD `6f21171` | `tests/e2e/test_k8s_collect.py::test_collect_k8s_bundle PASSED            [100%]` |

Before the rebase, the Docker summary line of E1 was `=================== 7 passed, 1 warning in 350.17s (0:05:50) ===================`, and the Kubernetes test ran on `90e745b` (E2), before the review fixes. After the rebase, on `6f21171`:

- E3: `=================== 7 passed, 1 warning in 351.66s (0:05:51) ===================`
- E4: `=================== 1 passed, 1 warning in 101.75s (0:01:41) ===================`
- U7: `ok  	infrahub-ops/src/internal/app	3.254s`, and `ok` for `src/cmd/infrahub-backup`, `src/cmd/infrahub-collect` and `src/internal/updater`.

In E3 and E4, the one warning is a deprecation warning that the `kr8s` library raises for `asyncio.iscoroutinefunction`.

Checks that are not committed tests:

- Real PostgreSQL checks of the SQL, with throwaway `postgres:18-alpine` containers and test files behind a build tag used only in the scratch directory:
  - `TestPGCheckStorageQuery` passed at 2026-10-26T18:17:59Z (container `pgcheck-storage-d8y7s-2585915`).
  - `TestPGCheck_Queries` and `TestPGCheck_Collector` passed at about 2026-10-26T18:43Z (container `pgcheck-d8y7s-breakdown`).
- Quickstart scenario 5 on a real Compose stack, with a throwaway pytest file: `1 passed, 1 warning in 180.42s` at 2026-10-26T19:39:04Z.
- Manual check T046 on kind cluster `d8y7s-cnpg` with CloudNativePG operator 1.30.1 and two PostgreSQL 18.6 instances. The queries ran in the primary pod before and after a switchover. Details are in [t046-cnpg-check.md](t046-cnpg-check.md).
- Gates on the final code, run by the review-fix subagent:
  - `make fmt` left no diff.
  - `make vet` and `make test` passed.
  - golangci-lint v2.7.2 reported `0 issues.`
  - `ruff format --check` and `ruff check` passed.
  - Vale reported `✔ 0 errors, 0 warnings and 0 suggestions in 2 files.`
  - rumdl reported `Success: No issues found in 3 files`.
  - `go.mod` and `go.sum` are unchanged against `a8b9c76`.

## Review findings

The review ran six aspects, one after another: code, errors, tests, types, comments and simplify. The review config file `.specify/extensions/review/review-config.yml` does not exist, so the extension defaults applied and all six aspects were enabled. The code aspect reported no finding at confidence 80 or higher.

| Severity | File | Summary | Status |
|---|---|---|---|
| High | `src/internal/app/collect_taskmanager_db.go`, `src/internal/app/app_config.go` | A connection URL that does not parse, from the operator's environment or from `task-manager`, was ignored with a debug log only. The backup tool's warning named the wrong variable. | Fixed in `bad601f`: a warning names the variable and its source, without the value. |
| High | `src/internal/app/collect_taskmanager_db.go:66` | A comment said the credential lookup reads `iops.config`; it does not. | Fixed in `bad601f` |
| Medium | `src/internal/app/collect_taskmanager_db.go` (`credentialsFromPostgresEnv`) | When `task-manager-db` set no database name, the database defaulted to `prefect`, while the official image names it after `POSTGRES_USER`. | Fixed in `b5e28d6` |
| Medium (tests, 7 of 10) | `src/internal/app/collect_storage.go` | FR-017 was not tested when `psql` succeeds and its output does not parse. | Fixed in `bad601f`: both tests fail with a nil mask. |
| Medium | `src/internal/app/collect_taskmanager_db.go:199` | A comment said the password is never in the command. It is on the `docker` or `kubectl` command line. | Fixed in `bad601f` |
| Medium | `docs/docs/collect/create.mdx`, `docs/docs/collect/index.mdx`, `dev/knowledge/infrahub-collect.md` | The lock behavior left out the tables that breakdown queries join, and said the size functions wait for "any lock". | Fixed in `493749d` |
| Medium | `docs/docs/collect/create.mdx` | The `pg_terminate_backend` instruction named one user; the tool can log in as other users. | Fixed in `493749d` |
| Medium | `src/internal/app/collect_taskmanager_db.go` | When the lookup falls back to the default credentials, nothing in the bundle says why the earlier sources were skipped. | Deferred |
| Medium | `src/internal/app/collect_storage.go` (`breakdownTable`) | A failed breakdown table keeps one detail line and gets no `.err.txt`. | Deferred |
| Medium | `tests/e2e/test_docker_collect.py` (T034 cleanup) | If the terminate query raises, the lock subprocess is not reaped, and later tests could hit the lock. | Deferred |
| Medium (simplify) | `src/internal/app/collect_storage.go` | The failure handling of the storage and catalog queries is written twice (about 20 lines). | Deferred |
| Medium (simplify) | `src/internal/app/collect_storage_test.go` | Test setup is duplicated (about 45 lines). | Deferred |
| Low (comments) | 4 places, including `t046-cnpg-check.md` and `tests/helpers/bundle.py` | Comments and docstrings did not match the code. | Fixed in `bad601f` |
| Low (comments) | `src/cmd/infrahub-collect/main.go`, `docs/docs/reference/commands.mdx`, `changelog/181.added.md` | "its largest tables": the breakdown always reads the same seven tables, whatever their size. | Deferred |
| Low (comments) | `dev/knowledge/infrahub-collect.md` | The page calls `storageQuery` a "catalog query". | Deferred |
| Low (types) | `src/internal/app/collect_storage.go` | `tableBreakdown.Status` is a plain string, not a named type. | Deferred |
| Low (types) | `src/internal/app/collect_taskmanager_db.go` | `taskManagerDBCredentials` has no `String()` method that hides the password. | Deferred |
| Low (types) | `src/internal/app/collect_storage_test.go` | No test asserts that `countWithoutStartTime` is set only for a `start_time` column. | Deferred |
| Low (tests, 6 of 10) | `tests/e2e/test_docker_collect.py` | The seeded data has no task runs or logs, so grouping `task_run` and `log` by flow name is checked only as SQL text. | Deferred |
| Low (tests, 5 or less) | several | SC-002 is not checked against statistics, SC-004 has no real-stack test, and smaller items. | Deferred |
| Low (simplify) | several | `cmp.Or`, `errorOutputLines`, building the catalog query from the table list, unused flag variables, and `bundle.py` helpers. | Deferred |

## Decisions made without the user

- Chunking: ten chunks.
  - Phases 2, 3 and 4 each have more than 8 tasks, so each was split into two chunks: code and tests, or Go code and end-to-end tests.
  - Phase 5 was split into user documentation, developer pages, and final gates.
- Password masking differs from the wording of research R10. A successful query's stdout is returned unmasked, because:
  - it is the parsed query result and never holds the command line;
  - masking the default password `prefect` would corrupt the database name and event names such as `prefect.flow-run.Completed`.
  The command, stderr, error text and a failed query's stdout are masked. ADR 0008 and `dev/guidelines/collectors.md` record this. research.md was not edited.
- Value-based masking has a side effect: with the default credentials, the masked command in an `.err.txt` shows `-d ********`, because the database name equals the password. This was left as is.
- The T021 assertion that the breakdown collector is `skipped` with `not requested` was moved from chunk 5 to chunk 7. Chunk 7 added it.
- T034 holds the table lock for up to 300 seconds instead of 120 seconds. Scenario 4 of `quickstart.md` still says 120 seconds and was not changed.
- The chunk 10 subagent edited `quickstart.md` scenario 5. The example wrong password `x` turned the reason into `role "nobody" does not e********ist`, so the example now uses `wrong-secret`. The workflow does not plan edits to spec documents during implementation.
- T045 cited chunk 7's end-to-end runs for quickstart scenarios 2 to 4 instead of running them again, because no Go or test file had changed.
- Review triage went further than the High findings. It also fixed Medium and Low items that touched the same code or pages and needed only unit tests or documentation checks. It deferred the items that change the bundle layout or the end-to-end tests:
  - a per-table `.err.txt`;
  - a record of why each credential source was skipped;
  - the T034 lock cleanup.
- The Docker end-to-end file was run again after the review fixes (E1). The Kubernetes test was not run again, for two reasons:
  - Its test code did not change after `90e745b`.
  - The Go changes after it only add warnings for URLs that do not parse, fix the database-name fallback in the third credential source, and change comments. On the default Helm chart, the credentials come from the `task-manager` environment. This was confirmed on kind in T046.

  After the rebase on `main`, both end-to-end files were run again on `6f21171` (E3 and E4), so the Kubernetes test now also covers the review fixes. The CloudNativePG check (T046) was not run again.
- The review-fix subagent did not restore a parse cause in the backup tool's warning. pgx error text can quote part of the password, for example `invalid port ":pa" after host`.
- The review-fix subagent changed the signatures of `prefectConnectionFromEnv` and `applyPrefectConnection` to pass the variable name and its source.
- The documentation fix states two facts that the review-fix subagent could not quote from a source:
  - While a schema change waits behind a query, PostgreSQL makes later queries on that table wait behind the schema change.
  - Infrahub's Docker Compose uses the official PostgreSQL image. `docker ps` showed `postgres:18-alpine` for `task-manager-db` during run E1.
- The `speckit-checkpoint-commit` skill does not exist in this repository, so every chunk committed with `git commit`.

## Rebase on `main`

On 2026-10-27, the branch was rebased from `d4e4b34` to `origin/main` at `fa67557`, so it now contains the pod lookup fixes `f673202` and `1e0e522`. Each commit kept its original committer name and date. Two commits had conflicts. In both, the two sides had added text at the same place, so the resolution keeps both:

- `src/internal/app/environment_kubernetes_test.go` (`6e591d5`): `main` added `TestPodResolutionWithPostgresOperatorLabels` and the commit added `TestGetPodForServiceWith_SelectsPrimaryPod`. The file keeps the test from `main` first. The commit no longer adds the `slices` import, because `main` already adds it.
- `dev/knowledge/infrahub-collect.md` (`fabe088`): `main` added a paragraph to the telemetry section and the commit added the section "Task manager database storage". The page keeps the paragraph from `main` first.

`git range-diff` shows that the other 22 commits apply the same changes as before. The branch changes the same files as before the rebase.

| Before the rebase | After the rebase | Commit |
|---|---|---|
| `44bdfa8` | `982913b` | docs(specs): add spec for task manager database storage figures |
| `c984826` | `6961ad0` | docs(specs): add plan for task manager database storage figures |
| `d8a8a91` | `5e67dac` | docs(specs): apply critique to task manager database storage plan |
| `c1881ed` | `4ae32b6` | docs(specs): add tasks for task manager database storage figures |
| `a8b9c76` | `3766fb6` | docs(specs): add PRD alignment check for task manager database storage |
| `5dcda69` | `af580e6` | chore(specs): record green baseline for task manager database storage |
| `30f30e5` | `e9955f8` | refactor(app): extract Prefect connection parsers |
| `a47de65` | `4533bda` | feat(collect): resolve task manager database credentials |
| `629a155` | `8668b59` | feat(collect): run read-only, time-limited queries in task-manager-db |
| `6e591d5` | `400e0d8` | test(k8s): cover primary pod selection for task-manager-db |
| `c7fa7d2` | `5193416` | fix(collect): keep successful task manager database query output unmasked |
| `464e169` | `be9e381` | feat(collect): report task manager database storage in every bundle |
| `1eccf40` | `44ea644` | test(e2e): check task manager database storage figures in bundles |
| `7472b37` | `ea93016` | feat(collect): add --include-task-manager-storage |
| `b48075e` | `0e452c1` | feat(collect): break down the largest task manager tables by record kind |
| `90e745b` | `36eeb17` | test(e2e): check the task manager storage breakdown and its time limit |
| `384c0f6` | `58b66d3` | docs(collect): document the task manager database storage figures |
| `fabe088` | `9f26631` | docs(dev): record database-enforced query limits for task manager queries |
| `e93ff50` | `6e0413a` | chore(changelog): add fragment for task manager database storage |
| `8c0813f` | `cc507d9` | chore(specs): record final gates for task manager database storage |
| `bad601f` | `66e4670` | fix(collect): warn when a task manager connection URL does not parse |
| `b5e28d6` | `0b34492` | fix(collect): name the task-manager-db database after POSTGRES_USER by default |
| `493749d` | `ef24df0` | docs(collect): correct lock and query-termination guidance |
| `3c1f7cd` | `6f21171` | docs(specs): add implementation report for task manager database storage |

Checks on `6f21171`, the head after the rebase, all with `GOTOOLCHAIN=go1.25.0`:

- `go build ./...` and `go vet ./...` passed.
- golangci-lint v2.7.2 reported `0 issues.`
- U7 passed (see "Local-pass evidence").
- rumdl reported no issues in the changed documentation pages, `dev/knowledge/infrahub-collect.md` and `AGENTS.md`.
- Both end-to-end files passed again (E3 and E4).

## Suggested next steps

1. Open the pull request.
   - Paste [t046-cnpg-check.md](t046-cnpg-check.md) into the description.
   - Mark the high-risk area: spawning processes with credentials in `task-manager-db`, covered by FR-017 and research R10.
2. Decide on the deferred Medium findings:
   - Write `storage-breakdown.<table>.err.txt` for a failed table. This changes `contracts/manifest-outcomes.md` and the bundle layout checks.
   - Record why each credential source was skipped.
   - Make the T034 lock cleanup robust.
3. Change scenario 4 of `quickstart.md` to hold the lock for longer than the run takes (about 85 seconds), as T034 does.
4. After the merge, run `speckit-opsmill-extract` on this spec directory if the project wants the knowledge extracted.
