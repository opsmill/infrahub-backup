---

description: "Task list for storage figures of the task manager database in troubleshooting bundles"
---

# Tasks: Storage Figures for the Task Manager Database in Troubleshooting Bundles

**Input**: Design documents from `specs/006-taskmgr-db-storage/`

**Prerequisites**: [plan.md](plan.md), [spec.md](spec.md), [research.md](research.md), [data-model.md](data-model.md), [contracts/](contracts/), [quickstart.md](quickstart.md)

**Tests**: included. Every functional requirement in the spec names its test, and constitution principle IV requires behavior changes to ship with tests.

**Organization**: the query runner (credentials, `psql` command, error kinds, password masking) is shared by both user stories, so it is Foundational. User Story 1 adds the always-collected storage figures; User Story 2 adds the breakdown behind the new flag.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: can run in parallel (different files, no dependency on an unfinished task)
- **[Story]**: the user story the task belongs to (US1, US2)

## Path Conventions

Single project: Go code in `src/`, end-to-end tests in `tests/`, at the repository root, as in [plan.md](plan.md).

## Conventions for every task

- Run Go gates through `make` (`make test`, `make vet`), never bare `go test ./...`: the Makefile adds the build tags that the local toolchain needs. For a focused run, pass `-tags untested_go_version` yourself.
- On the collect path, use only the bounded exec variants (`ExecSeparateContext`, `execDumpTimeout`), as [`dev/guidelines/collectors.md`](../../dev/guidelines/collectors.md) requires.
- Never write the output of `env` from `task-manager` or `task-manager-db` to the bundle or the log.
- `go.mod` and `go.sum` must not change. If they do, the task is wrong: this feature adds no dependency (the logrus test hook `github.com/sirupsen/logrus/hooks/test` is part of the existing logrus module).

---

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: a green baseline before any change. The feature adds no dependency and no scaffolding.

- [X] T001 Record a green baseline: run `make build-watchdog` once (the unit tests of `src/internal/app` need its embedded binaries), then `make build`, `make test` and `make vet`, then lint with `PATH="$(go env GOPATH)/bin:$PATH" make lint`; if golangci-lint panics on the Go version, lint as CI does with `GOTOOLCHAIN=go1.25.0 go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.7.2 run`. All must pass before any feature change.

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: the query runner that both collectors use: credentials, the `psql` command and its session parameters, error classification, password masking, and the skip precondition ([research.md](research.md) R1 to R3, R8 to R10).

**CRITICAL**: no user story work can begin until this phase is complete.

- [ ] T002 In `src/internal/app/app_config.go`, extract two pure functions without changing the behavior of `fetchPostgresCredentials` or `applyPrefectConnection`: `prefectConnectionFromEnv(envOutput string) string`, which returns the first non-empty value of `prefectConnectionEnvVars` in preference order from `env` output, and `parsePrefectConnection(connStr string) (user, password, database string, ok bool)`, which holds the normalization and `pgx.ParseConfig` logic now inside `applyPrefectConnection`. Make `fetchPostgresCredentials` and `applyPrefectConnection` call them.
- [ ] T003 [P] Add table-driven tests for both functions in `src/internal/app/app_config_test.go`: the Prefect 3 variable wins over the Prefect 2 variable whatever the line order; no variable gives an empty string; `postgresql+asyncpg://prefect:secret@task-manager-db:5432/prefect` parses to `prefect`, `secret`, `prefect`; an unparsable string gives `ok == false`.
- [ ] T004 Create `src/internal/app/collect_taskmanager_db.go` with the constants and credentials: `taskManagerDBService = "task-manager-db"`, `taskManagerDBBundleDir = "task-manager-db"`, `taskManagerDBAppName = "infrahub-collect"`, `collectDBQueryTimeout = 5 * time.Minute` (each breakdown table), `collectDBQueryGrace = 15 * time.Second`, `dbQueryTimeoutEnvVar = "INFRAHUB_TASK_MANAGER_DB_QUERY_TIMEOUT"`, and `taskManagerDBNotDeployedReason = "service not deployed; the task manager may use an external PostgreSQL server"`. Add a `taskManagerDBCredentials` struct (user, password, database, source) and `resolveTaskManagerDBCredentials(cc *collectContext) taskManagerDBCredentials`, which tries, in order (research R8): (1) `prefectConnectionEnvVars` in the operator's environment through `os.Getenv`; (2) `env` in `task-manager` through `cc.execDumpTimeout(collectExecTimeout, "task-manager", []string{"env"})` and `prefectConnectionFromEnv`; (3) `env` in `task-manager-db` through the same bounded exec, using `POSTGRES_USER`, `POSTGRES_PASSWORD`, and `POSTGRES_DB` or else `POSTGRES_DATABASE`, only when user and password are both present; (4) `defaultPostgresUsername`, `defaultPostgresPassword`, `defaultPostgresDatabase`. Add a `dbCredentials *taskManagerDBCredentials` field to `collectContext` in `src/internal/app/collect.go` and cache the result there, so a run resolves credentials once. Log the source at debug level, never a value. Do not change `iops.config`.
- [ ] T005 [P] Add tests for `resolveTaskManagerDBCredentials` in `src/internal/app/collect_taskmanager_db_test.go`, with a fake collect backend whose `ExecContext` answers `env` per service and counts calls (pattern: the fake backends in `src/internal/app/collect_diagnostics_test.go`): the operator's environment wins (use `t.Setenv`); then `task-manager`; when the `task-manager` exec fails, `task-manager-db` with the official names and with `POSTGRES_DATABASE`; a `task-manager-db` environment with a user but no password falls through to the defaults; calling it twice on one context runs each `env` exec at most once (critique E1).
- [ ] T006 In `src/internal/app/collect_taskmanager_db.go`, add the command builder: a `dbQuery` struct (`sql string`, `limit time.Duration`, `breakdown bool`), the allowlist constant of the session parameters the tool may set (all settable by any user, research R2), and `buildPsqlCommand(creds taskManagerDBCredentials, q dbQuery) ([]string, map[string]string)`. The command is `psql -h localhost -U <user> -d <database> -X -q -A -t -v ON_ERROR_STOP=1 -c <sql>`. The environment holds `PGPASSWORD`, `PGAPPNAME=infrahub-collect`, `PGCONNECT_TIMEOUT=10`, and `PGOPTIONS` with `-c default_transaction_read_only=on -c statement_timeout=<limit in ms> -c TimeZone=UTC`, plus `-c enable_hashjoin=off -c enable_mergejoin=off -c max_parallel_workers_per_gather=0` when `breakdown` is true (research R4). Add `effectiveDBQueryLimit(base time.Duration, getenv func(string) string) time.Duration`, which returns the smaller of `base` and the parsed `INFRAHUB_TASK_MANAGER_DB_QUERY_TIMEOUT`, and logs a warning and returns `base` for a value that does not parse or is not positive ([contracts/cli.md](contracts/cli.md)).
- [ ] T007 [P] Add tests in `src/internal/app/collect_taskmanager_db_test.go` for the builder (FR-008, FR-009): the exact argument list; the environment keys and values; `statement_timeout=60000` for a 60-second query and `300000` for a 5-minute one; the planner settings present only for breakdown queries; every `-c` name in `PGOPTIONS` is in the allowlist. Table-test `effectiveDBQueryLimit` for both bases (60 seconds and 5 minutes): unset, `5s`, `10m` (keeps the base), `garbage` and `0` (warn and keep the base).
- [ ] T008 In `src/internal/app/collect_taskmanager_db.go`, add the error kinds (research R9): a `dbQueryError` struct with `kind` (`timed out`, `psql missing`, `login rejected`, `not reachable`, `query failed`), `detail` and `limit`. `Error()` returns `timed out after <formatCommandTimeout(limit)>` for the timeout kind and `<kind>: <detail>` otherwise; `Unwrap()` returns `&timeoutError{timeout: limit}` for the timeout kind only. Add `classifyDBQueryFailure(err error, stdout, stderr string, limit time.Duration) *dbQueryError`: a `*timeoutError` from the exec, or `canceling statement due to statement timeout`, gives the timeout kind; exit code 127 with `executable file not found`, `can't execute 'psql'` or `psql: not found` gives `psql missing`; `password authentication failed`, `role "` with `does not exist`, `database "` with `does not exist`, or `no pg_hba.conf entry` gives `login rejected`; `could not connect to server`, `connection refused`, `Connection timed out`, `is not running`, `no pods found` or `not found` for a pod gives `not reachable`; anything else gives `query failed`. The detail is the last line of the error output that contains `ERROR`, `FATAL`, `psql:`, `error` or `Error`, ignoring lines that start with `Defaulted container` (critique E5), else the last non-empty line, else the Go error text.
- [ ] T009 [P] Add table tests for `classifyDBQueryFailure` in `src/internal/app/collect_taskmanager_db_test.go`, using the outputs recorded in [research.md](research.md) R2, R8 and R9: `ERROR:  canceling statement due to statement timeout`; a tool-side `*timeoutError`; Docker `OCI runtime exec failed: ... exec: "psql": executable file not found in $PATH` with exit 127; Kubernetes `env: can't execute 'psql': No such file or directory` with exit 127; `FATAL:  role "nobody" does not exist`; `password authentication failed for user "prefect"`; `no pg_hba.conf entry`; `connection to server at "localhost" (::1), port 5432 failed: Connection refused`; `Error response from daemon: container ... is not running`; an unknown error; and each of these preceded by `Defaulted container "postgres" out of: postgres, bootstrap-controller (init)`. Assert the kind, the detail line, the `Error()` text, and that `errors.As(err, &*timeoutError)` holds only for the timeout kind.
- [ ] T010 In `src/internal/app/collect_taskmanager_db.go`, add the runner: `maskSecretValue(secret string) func(string) string`, which replaces every occurrence of a non-empty secret with `********` (research R10), and `runTaskManagerDBQuery(cc *collectContext, q dbQuery) (dbQueryResult, error)`, where `dbQueryResult` holds the masked command, stdout and stderr. It resolves the credentials (T004), builds the command (T006), runs it on `task-manager-db` through the backend's `separateExecer.ExecSeparateContext` with the bound `q.limit + collectDBQueryGrace`, and returns a clear `query failed` error if the backend does not implement `separateExecer`. On failure it returns the classified `*dbQueryError` (T008). The command, both streams and the error text it returns are all masked.
- [ ] T011 [P] Add runner tests in `src/internal/app/collect_taskmanager_db_test.go` with a fake backend implementing `ExecSeparateContext` that records its arguments: the service is `task-manager-db`; the bound is the limit plus 15 seconds; the environment reaches the backend; PostgreSQL's timeout output and a tool-side `*timeoutError` both produce `timed out after <limit>`; when the fake backend's error and output contain the password (as Kubernetes does with `env PGPASSWORD=<password> psql ...`), the password appears in none of the returned command, streams or error text (FR-017).
- [ ] T012 [P] Add a test for the primary-pod selection in `src/internal/app/environment_kubernetes_test.go` (FR-014; no test covers `findPrimaryPod` on `main`): call `getPodForServiceWith` for `task-manager-db` with a fake `podRunner` that returns three pods for the `infrahub/service=task-manager-db` selector and answers the `cnpg.io/instanceRole` label lookup with `replica`, `primary`, `replica`; assert the second pod is selected and cached. Add cases for the legacy `role=primary` label and for a single pod.
- [ ] T013 In `src/internal/app/collect_taskmanager_db.go`, add `skipWhenTaskManagerDBAbsent(cc *collectContext) (bool, string)`, which behaves like `skipWhenServiceAbsent(taskManagerDBService)` in `src/internal/app/collect_diagnostics.go` but returns `taskManagerDBNotDeployedReason` (FR-010). Enumeration errors return `false` so the run records a real failure.

**Checkpoint**: the query runner is complete and tested; both collectors can now be built.

---

## Phase 3: User Story 1 - Support reads the size of the database and of each table (Priority: P1)

**Goal**: every bundle holds `task-manager-db/storage.json` with the database size, and for each table the data, index and TOAST sizes, the live and dead row estimates, and the last vacuum and autovacuum times (FR-001 to FR-003).

**Independent Test**: run `infrahub-collect create` without any new flag on Docker Compose; `storage.json` matches [contracts/storage.schema.json](contracts/storage.schema.json) and its total matches `pg_database_size` ([quickstart.md](quickstart.md) scenario 2).

### Implementation for User Story 1

- [ ] T014 [US1] Create `src/internal/app/collect_storage.go` with the storage report (research R5, [data-model.md](data-model.md)): types `byteSize` (`Bytes int64` as `bytes`, `Readable string` as `readable`), `tableStorage` and `storageReport` with the JSON field names of [contracts/storage.schema.json](contracts/storage.schema.json); `newByteSize(n int64) byteSize` using `formatBytes`; the constant `storageQuery`, one `SELECT json_build_object(...)` with `current_database()`, `pg_database_size(current_database())`, `stats_reset` from `pg_stat_database` for the current database, and a `json_agg` over `pg_stat_user_tables` joined to `pg_class` returning schema, name, `pg_total_relation_size`, `pg_table_size`, `pg_indexes_size`, the TOAST size (`pg_total_relation_size(reltoastrelid)` when `reltoastrelid <> 0`, else 0), `n_live_tup`, `n_dead_tup`, `last_vacuum` and `last_autovacuum`; and `parseStorageReport(stdout string, collectedAt time.Time) (storageReport, error)`, which computes data size as table size minus TOAST, builds each `byteSize`, renders timestamps as RFC 3339 UTC, turns a null table list into an empty array, and sorts tables by total bytes descending, then schema, then name.
- [ ] T015 [P] [US1] Add tests for `parseStorageReport` in `src/internal/app/collect_storage_test.go` (FR-002, FR-003): a fixed query output shaped like the one in [research.md](research.md) R5 (`{"database" : "prefect", "database_bytes" : 1748424383, ...}`) gives the expected report; data + index + TOAST equals total for every table; readable text matches `formatBytes`; null vacuum times and a null `stats_reset` stay null; `"tables" : null` gives `[]`; the order is deterministic for equal sizes; malformed JSON returns an error.
- [ ] T016 [US1] In `src/internal/app/collect_storage.go`, add `taskManagerDBStorageCollector() collector` named `task-manager-db-storage`, with `skip: skipWhenTaskManagerDBAbsent` and a run function that calls `runTaskManagerDBQuery` with `dbQuery{sql: storageQuery, limit: effectiveDBQueryLimit(collectExecTimeout, os.Getenv)}`. On failure, it writes `task-manager-db/storage.err.txt` with `writeDumpFailure` (masked command, both streams, and the mask from `maskSecretValue`) and returns the `*dbQueryError`, so a timeout is recorded as `timed out after 60s`. On success, it writes `task-manager-db/storage.json` with `json.MarshalIndent(report, "", "    ")` and `writeDumpFile`.
- [ ] T017 [US1] In `src/internal/app/collect.go`, add `taskManagerDBStorageCollector()` to `collectPlan` right after `taskManagerCollector()`, and update the comment that lists the plan order.
- [ ] T018 [P] [US1] Add collector outcome tests in `src/internal/app/collect_storage_test.go`, running the collector through `runCollectPlan` with a fake backend (pattern: `TestTaskManagerDumps` in `src/internal/app/collect_diagnostics_test.go` and `TestIncludeBackupCollector_OutcomeRecording` in `src/internal/app/collect_extras_test.go`): success writes a valid `storage.json` and records `success`; no `task-manager-db` replica records `skipped` with the external-PostgreSQL reason (FR-010); a PostgreSQL timeout, a missing `psql`, a rejected login and an unreachable container each record `failed` with the matching reason, write `storage.err.txt` and no `storage.json`, and the archive is still produced (FR-011, SC-004); with a backend that echoes the password in its error output, no staged file and no manifest reason contains it (FR-017).
- [ ] T019 [US1] Update `TestCollectPlanOrdering` in `src/internal/app/collect_diagnostics_test.go` for `task-manager-db-storage` right after `task-manager-state`.
- [ ] T020 [P] [US1] In `tests/helpers/bundle.py`, add `task-manager-db-storage` to `EXPECTED_COLLECTORS`, map it to `task-manager-db` in `COLLECTOR_DIRS`, allow `task-manager-db` as a top-level bundle entry, and add `assert_storage_report(doc: dict) -> None`, which checks the constraints of [contracts/storage.schema.json](contracts/storage.schema.json) by hand (required fields, no extra fields, the size invariant, the readable-text pattern, RFC 3339 or null timestamps, the ordering), as `validate_manifest_schema` does for the manifest.
- [ ] T021 [US1] In `tests/e2e/test_docker_collect.py`, extend the default-run test (`test_collect_full_bundle`, or a new test placed before the disruptive tests) with `--log-format json`: `bundle/task-manager-db/storage.json` passes `assert_storage_report`; its `database_size.bytes` is within 1 MiB of `pg_database_size(current_database())` read with `docker compose -p <project> exec -T task-manager-db psql -U <owner> -d <database> -Atc ...` (FR-001); the manifest records `task-manager-db-storage` as `success` and `task-manager-db-storage-breakdown` as `skipped` with `not requested`; and the time between the `Collecting task-manager-db-storage` log line and the next collector's line is under 10 seconds (SC-002, critique P5).
- [ ] T022 [P] [US1] In `tests/e2e/test_k8s_collect.py`, assert that the bundle holds `task-manager-db/storage.json`, that it passes `assert_storage_report`, and that the manifest records `task-manager-db-storage` as `success`.

**Checkpoint**: User Story 1 is complete. Every bundle now answers questions 1 to 3 of the spec.

---

## Phase 4: User Story 2 - Support reads which kinds of records fill the largest tables (Priority: P2)

**Goal**: with `--include-task-manager-storage` or `INFRAHUB_INCLUDE_TASK_MANAGER_STORAGE`, the bundle holds `task-manager-db/storage-breakdown.json` with exact row counts and oldest and newest timestamps for each group of seven Prefect tables, each table bounded by a 5-minute limit that PostgreSQL enforces (FR-004 to FR-009, FR-012, FR-013, FR-016).

**Independent Test**: on a Docker Compose deployment seeded with a flow run cancelled before it starts, run `create --include-task-manager-storage`; `storage-breakdown.json` holds the seeded counts and the manifest records `success` ([quickstart.md](quickstart.md) scenarios 3 and 4).

### Implementation for User Story 2

- [ ] T023 [US2] Add the option and flag ([contracts/cli.md](contracts/cli.md)): `IncludeTaskManagerStorage bool` in `CollectOptions` in `src/internal/app/collect.go`; in `src/cmd/infrahub-collect/main.go`, a `BoolVar` named `include-task-manager-storage` on `createCmd` with the help text from the contract, `viper.BindPFlag("include-task-manager-storage", ...)`, and `IncludeTaskManagerStorage: viper.GetBool("include-task-manager-storage")`, following `--include-backup`.
- [ ] T024 [US2] In `src/internal/app/collect_storage.go`, add the breakdown list and SQL builder (research R7, [data-model.md](data-model.md)): a `breakdownTableSpec` struct (table, group keys each with its output name and SQL expression, the joins, the timestamp column, and whether to count rows without `start_time`); the package-level `breakdownTables` slice with the seven entries in the order `events`, `flow_run`, `task_run`, `flow_run_state`, `task_run_state`, `log`, `artifact`; `breakdownQuery(spec breakdownTableSpec) string`, which returns one `SELECT json_agg(...)` over `SELECT json_build_object('group', json_build_object(<keys>), 'rows', count(*), ['rows_without_start_time', count(*) FILTER (WHERE <alias>.start_time IS NULL),] 'oldest', min(<ts>), 'newest', max(<ts>)) FROM <table> <alias> LEFT JOIN ... GROUP BY <key expressions>`, where the `log` level maps 10, 20, 30, 40, 50 to `DEBUG`, `INFO`, `WARNING`, `ERROR`, `CRITICAL` and other values to their text; and the constant `breakdownCatalogQuery`, which returns, for each table name of `breakdownTables`, whether `to_regclass(<name>) IS NOT NULL` (research R6).
- [ ] T025 [P] [US2] Add tests for the SQL builder in `src/internal/app/collect_storage_test.go` (FR-005 to FR-007): for each entry, the query reads the right table, uses `LEFT JOIN flow_run` and `LEFT JOIN flow` only where research R7 says, groups by the documented keys, takes `min` and `max` of the documented timestamp column, and has the `start_time IS NULL` filter only for `flow_run` and `task_run`; the catalog query names all seven tables; every table and key name is one of the values in the enums of [contracts/storage-breakdown.schema.json](contracts/storage-breakdown.schema.json).
- [ ] T026 [US2] In `src/internal/app/collect_storage.go`, add the breakdown types with the JSON field names of [contracts/storage-breakdown.schema.json](contracts/storage-breakdown.schema.json) (`storageBreakdown`, `tableBreakdown`, `breakdownGroup` with `Group map[string]*string`) and `parseBreakdownGroups(stdout string) ([]breakdownGroup, int64, error)`, which decodes the `json_agg` output (null gives no groups), renders timestamps as RFC 3339 UTC, sorts groups by rows descending and then by key values with null last, and returns the row total.
- [ ] T027 [P] [US2] Add tests for `parseBreakdownGroups` in `src/internal/app/collect_storage_test.go`: the total is the sum of group rows; the order is deterministic; a null flow name stays null; `rows_without_start_time` is kept when present and omitted otherwise; null `oldest` and `newest` stay null; malformed output returns an error.
- [ ] T028 [US2] In `src/internal/app/collect_storage.go`, add `taskManagerDBStorageBreakdownCollector() collector` named `task-manager-db-storage-breakdown` (research R11, R13, [contracts/manifest-outcomes.md](contracts/manifest-outcomes.md)). `skip` returns `not requested` when `cc.opts.IncludeTaskManagerStorage` is false, then defers to `skipWhenTaskManagerDBAbsent`. The run function runs `breakdownCatalogQuery` with the 60-second limit; on failure it writes `task-manager-db/storage-breakdown.err.txt` (masked, as in T016) and returns the `*dbQueryError` without trying any table. Then, for each entry of `breakdownTables`: stop if `cc.ctx` is done; log `Collecting storage breakdown of table <table> (time limit <limit>)` at info level (FR-013); record an absent table as `absent` with `table does not exist in this Prefect version` and log `Table <table>: absent`; otherwise run `breakdownQuery` with `breakdown: true` and the limit `effectiveDBQueryLimit(collectDBQueryTimeout, os.Getenv)`, record `collected` with the rows and groups and log `Table <table>: <n> rows in <m> groups (<duration>)`, or record `failed` with the error text and log a warning. It then writes `task-manager-db/storage-breakdown.json` with `time_limit_per_table` from `formatCommandTimeout`, and returns `fmt.Errorf("partial storage breakdown: %s", strings.Join(failures, "; "))` when any table failed, where each failure is `<table>: <reason>`. This error deliberately does not wrap the per-table `*timeoutError` with `%w`, because the orchestrator would otherwise replace the reason with `timed out after <limit>` and drop the table names that FR-016 requires; say so in a comment that cites research R11.
- [ ] T029 [US2] In `src/internal/app/collect.go`, add `taskManagerDBStorageBreakdownCollector()` as the first opt-in collector in `collectPlan`, before `benchmarkCollector`, and update the plan comment; update `TestCollectPlanOrdering` in `src/internal/app/collect_diagnostics_test.go` (research R12).
- [ ] T030 [P] [US2] Add collector tests in `src/internal/app/collect_storage_test.go`, through `runCollectPlan` with a fake backend that answers each query by matching its SQL: without the option, `skipped` with `not requested`, even when `task-manager-db` is absent (FR-004); with the option and no replica, `skipped` with the external-PostgreSQL reason (FR-010); all seven collected gives `success` and a file whose row totals match the groups; one absent table gives `absent` and still `success` (FR-012); a PostgreSQL timeout on `log` gives `log` `failed` with `timed out after 300s`, the other tables collected, and the manifest reason `partial storage breakdown: log: timed out after 300s` (FR-009, FR-016); a failing catalog query writes `storage-breakdown.err.txt`, runs no table query and records the classified reason; a logrus test hook (`github.com/sirupsen/logrus/hooks/test`) sees one `Collecting storage breakdown of table <table>` line per table in list order (FR-013); cancelling the context after the first table stops the loop; the password appears in no staged file.
- [ ] T031 [P] [US2] In `tests/helpers/bundle.py`, add `task-manager-db-storage-breakdown` to `EXPECTED_COLLECTORS` and `OPT_IN_COLLECTORS`, and add `assert_storage_breakdown(doc: dict) -> None`, which checks the constraints of [contracts/storage-breakdown.schema.json](contracts/storage-breakdown.schema.json) by hand, including that `rows` and `groups` are present only for `collected` tables and `reason` only for the others.
- [ ] T032 [US2] In `tests/e2e/test_docker_collect.py`, add a seeding helper that runs a Python script with `docker compose -p <project> exec -T task-worker python -` (the worker has the Prefect client and `PREFECT_API_URL`): it creates the flow `e2e-storage-seed`, three flow runs moved through `Running` to `Completed`, and one flow run in `Scheduled` set to `Cancelled` with `force=True`, so its `start_time` stays null; then it polls `task-manager-db` with `psql` until the events of the seeded flow runs are stored (up to 60 seconds).
- [ ] T033 [US2] In `tests/e2e/test_docker_collect.py`, add `test_collect_task_manager_storage_breakdown`, placed before the disruptive tests: seed (T032), run `create --include-task-manager-storage`, then assert that the file passes `assert_storage_breakdown`; all seven tables are `collected`; the `flow_run` group `{"flow_name": "e2e-storage-seed", "state_type": "CANCELLED"}` has `rows == 1` and `rows_without_start_time == 1` (FR-007); the `COMPLETED` group of that flow has `rows == 3`, no row without a start time, and non-null `oldest` and `newest`; the `events` groups include `prefect.flow-run.Completed`; the manifest records `success`. Run it a second time with `INFRAHUB_INCLUDE_TASK_MANAGER_STORAGE=true` and no flag, and assert the same outcome (FR-004).
- [ ] T034 [US2] In `tests/e2e/test_docker_collect.py`, add `test_collect_task_manager_storage_time_limit`, placed before the disruptive tests (FR-009, critique E2): start `docker compose -p <project> exec -T task-manager-db psql -U <owner> -d <database> -c "BEGIN; LOCK TABLE artifact IN ACCESS EXCLUSIVE MODE; SELECT pg_sleep(120); COMMIT;"` with `subprocess.Popen` and wait until `pg_locks` shows the lock; run `create --include-task-manager-storage` with `INFRAHUB_TASK_MANAGER_DB_QUERY_TIMEOUT=5s`; assert that `task-manager-db-storage` is `failed` with `timed out after 5s` (measuring the size of `artifact` waits for the same lock, research R5), that `artifact` is `failed` with `timed out after 5s` while the other six tables are `collected`, that the manifest reason of the breakdown collector is `partial storage breakdown: artifact: timed out after 5s`, and that `SELECT count(*) FROM pg_stat_activity WHERE application_name = 'infrahub-collect'` returns 0. Terminate the lock session in a `finally` block.

**Checkpoint**: User Stories 1 and 2 both work; quickstart scenarios 2 to 5 pass.

---

## Phase 5: Polish and Cross-Cutting Concerns

**Purpose**: documentation (FR-015), project knowledge, the changelog fragment, and the final gates.

- [ ] T035 [P] In `docs/docs/collect/index.mdx`, add the `task-manager-db/` directory and its two files to "What a bundle contains", stating that `storage.json` is in every bundle and `storage-breakdown.json` only with the flag ([contracts/cli.md](contracts/cli.md)).
- [ ] T036 [P] In `docs/docs/collect/create.mdx`, add a section on collecting the storage breakdown when support asks for it, with every point listed for this page in [contracts/cli.md](contracts/cli.md): up to 5 minutes for each of the seven tables; a table that reaches the limit is recorded as failed and the others are still collected; do not use the flag while the task manager is being upgraded; how to stop the tool's query with `pg_terminate_backend` by application name; and that during an upgrade the always-collected figures can be missing with `timed out after 60s`.
- [ ] T037 [P] In `docs/docs/reference/commands.mdx`, add `--include-task-manager-storage` to the `infrahub-collect create` flag table (default `false`, environment variable `INFRAHUB_INCLUDE_TASK_MANAGER_STORAGE`) and the two files to the bundle contents table.
- [ ] T038 [P] In `docs/docs/reference/configuration.mdx`, add the flag and its environment variable next to `--include-backup`. Do not document `INFRAHUB_TASK_MANAGER_DB_QUERY_TIMEOUT`.
- [ ] T039 Run Vale and rumdl on the four changed pages and fix every finding: download the Vale version pinned as `VALE_VERSION` in `.github/workflows/ci.yml` into a scratch directory, run it on the pages; run `uv sync --only-group dev` then `uv run rumdl check` on them. Optionally build the docs from `docs/` with `pnpm install --frozen-lockfile --ignore-workspace` and Node 22.
- [ ] T040 [P] Create `dev/adr/0008-database-enforced-query-limits.md` in the format of the other ADRs (Status, Date, Source, Context, Decision, Consequences, Alternatives Considered), from [research.md](research.md) R2 to R4 and R15: queries in the task manager database run in read-only sessions, with time limits that PostgreSQL enforces through `PGOPTIONS` and a tool-side backstop, and the breakdown turns off hash joins, merge joins and parallel workers so it never writes temporary files. Add it to the ADR list in `dev/README.md`.
- [ ] T041 [P] In `dev/guidelines/collectors.md`, add a section on database queries (bounded exec into `task-manager-db`, read-only session, PostgreSQL-enforced limit, value-based password masking, link to ADR 0008), and add to "Time-bound every subprocess" the exception from research R11: a collector that reports several named sub-results may return a plain error that names each failed item, so the manifest keeps the names.
- [ ] T042 [P] In `dev/knowledge/infrahub-collect.md`, add `--include-task-manager-storage` to the flag table and `task-manager-db/` to the bundle layout, with a short section on the two collectors, the credential lookup order, the lock behavior of the size functions, and the test-only variable.
- [ ] T043 [P] In `AGENTS.md`, add `collect_taskmanager_db.go` and `collect_storage.go` to the list of `src/internal/app/collect*.go` files in "Core Components", add `--include-task-manager-storage` where the collect commands are described if they list flags, and change the "Recent Changes" line for `006-taskmgr-db-storage` from "Planned" to "Added".
- [ ] T044 [P] Create the changelog fragment `changelog/181.added.md` with `uv run towncrier create -c "<text>" 181.added.md`, for example: "Troubleshooting bundles now include the storage use of the task manager database: the database and table sizes in every bundle, and, with `--include-task-manager-storage`, the row counts and timestamps of the records that fill its largest tables." Preview it with `uv run towncrier build --draft --version 0.0.0`.
- [ ] T045 Run the final gates: `make fmt` leaves no diff; `make test`, `make vet` and the lint command from T001 pass; `git diff main -- go.mod go.sum flake.nix` is empty. If a Docker Compose Infrahub is available, run [quickstart.md](quickstart.md) scenarios 2 to 5; otherwise run `uv run pytest -m docker tests/e2e/test_docker_collect.py` and `uv run pytest -m k8s tests/e2e/test_k8s_collect.py`, or rely on the CI end-to-end jobs and say so in the pull request.
- [ ] T046 Optional, manual: check primary-pod selection on CloudNativePG with [quickstart.md](quickstart.md) scenario 6 on a local kind cluster, and record the result in the pull request description.

---

## Dependencies and Execution Order

### Phase dependencies

- Setup (Phase 1): no dependency.
- Foundational (Phase 2): after Setup. Blocks both user stories.
- User Story 1 (Phase 3): after Foundational.
- User Story 2 (Phase 4): after Foundational. It shares `collect_storage.go` with User Story 1, so if one person does both, finish T014 to T017 first to avoid conflicting edits; T023 to T027 do not depend on User Story 1 code.
- Polish (Phase 5): T035 to T044 after the behavior they describe exists; T045 last.

### Task dependencies inside phases

- T002 before T004 (credentials use the extracted parsers).
- T004 before T006, T010 and T013; T006 and T008 before T010.
- T014 before T016; T016 before T017 and T018; T017 before T019.
- T024 and T026 before T028; T028 before T029 and T030; T032 before T033 and T034.
- T020 before T021 and T022; T031 before T033.

### Parallel opportunities

- Phase 2: T003, T005, T007, T009, T011 and T012 are tests in separate files or separate test functions and can be written alongside their implementation tasks once those exist; T012 can start immediately.
- Phase 3: T015, T018, T020 and T022 in parallel once T014 and T016 exist.
- Phase 4: T025, T027, T030 and T031 in parallel once their implementation tasks exist.
- Phase 5: T035 to T038 and T040 to T044 all touch different files.

### Parallel example: User Story 1

```text
After T014 and T016:
  T015 parse tests            src/internal/app/collect_storage_test.go
  T020 bundle helper          tests/helpers/bundle.py
  T022 Kubernetes e2e         tests/e2e/test_k8s_collect.py
```

### Parallel example: User Story 2

```text
After T024 and T026:
  T025 SQL builder tests      src/internal/app/collect_storage_test.go
  T031 bundle helper          tests/helpers/bundle.py
  T032 seeding helper         tests/e2e/test_docker_collect.py
```

---

## Implementation Strategy

### First increment: User Story 1

1. Phase 1 and Phase 2.
2. Phase 3. Stop and check quickstart scenario 2: every bundle now answers how big the database is, which tables use the space, and how many deleted rows are not reclaimed.

### Second increment: User Story 2

1. Phase 4. Check quickstart scenarios 3 to 5.
2. Phase 5: documentation, ADR, knowledge, changelog, final gates.

The PRD ships both journeys in the first release, so both increments land in the same pull request.

### Risk notes for review

- High-risk area: process spawning with credentials inside `task-manager-db` (AGENTS.md lists spawn and credential handling as sensitive). Reviewers check T010, T011 and T018 against FR-017 and research R10.
- Constitution principle II: the collectors only read. Reviewers check that no SQL outside `storageQuery`, `breakdownCatalogQuery` and `breakdownQuery` is ever sent, and that every query goes through `runTaskManagerDBQuery`.
