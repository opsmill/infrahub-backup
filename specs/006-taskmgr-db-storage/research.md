# Research: Storage Figures for the Task Manager Database

**Feature**: [spec.md](spec.md) | **Plan**: [plan.md](plan.md) | **Date**: 2026-10-26

The decisions below were checked against the code on `main` (commit `d4e4b34`) and against a disposable `postgres:18-alpine` container, the image that Infrahub's `docker-compose.yml` uses for `task-manager-db`. The container had a database owner without superuser rights and Prefect-shaped tables seeded with 300,000 flow runs and 2.25 million log records. The probe scripts are not part of the repository; the observed results are quoted where a decision depends on them.

## R1. Run each query with `psql` inside `task-manager-db`

- Decision: run `psql` inside the `task-manager-db` container through the collect-only bounded exec, `ExecSeparateContext`, which both backends implement and which accepts environment variables. Connect over TCP to `localhost` as the database owner, with the password in `PGPASSWORD`, exactly as `backupTaskManagerDBStream` runs `pg_dump` ([`backup_taskmanager.go`](../../src/internal/app/backup_taskmanager.go)). The command is `psql -h localhost -U <owner> -d <database> -X -q -A -t -v ON_ERROR_STOP=1 -c <query>`:
  - `-X` ignores any `.psqlrc` file in the container.
  - `-A -t` prints the bare value, without alignment or headers.
  - Each query returns a single JSON value built by PostgreSQL (`json_build_object`, `json_agg`), so the tool parses JSON and never splits text on delimiters. Flow names can contain any character.
- Rationale: the PRD requires the queries to run inside `task-manager-db` in the same way as `pg_dump`. Running them in the Prefect server container is ruled out, because a large listing there once got the server killed for running out of memory ([`infrahub-collect.md`](../../dev/knowledge/infrahub-collect.md)).
- Alternatives considered:
  - A Go PostgreSQL client (`pgx` is already a dependency): rejected, because the tool would need network access to the database port, which `docker exec` and `kubectl exec` do not provide, and because ADR 0001 keeps all deployment access on the `docker` and `kubectl` command-line tools.
  - Unix-socket peer authentication as the `postgres` operating-system user, as `restorePostgreSQL` does: rejected, because it depends on the image. The bitnami image used by the Helm chart and CloudNativePG do not accept it for the database owner.

## R2. PostgreSQL enforces the read-only transaction and the time limit

- Decision: set the session parameters through the `PGOPTIONS` environment variable of the `psql` process:
  - `default_transaction_read_only=on`
  - `statement_timeout=<limit in ms>`
  - `TimeZone=UTC`, so every timestamp in the JSON has the `+00:00` offset
  - `application_name` is set with `PGAPPNAME=infrahub-collect`, so an operator or a test can find the tool's sessions in `pg_stat_activity`
  - `PGCONNECT_TIMEOUT=10`, so an unreachable server fails within 10 seconds instead of waiting for the exec time limit
- Observed on PostgreSQL 18:
  - `SELECT pg_sleep(5)` with `statement_timeout=3000` fails with `ERROR:  canceling statement due to statement timeout`, and `psql` exits with code 1.
  - `CREATE TABLE` fails with `ERROR:  cannot execute CREATE TABLE in a read-only transaction`.
  - When the `docker exec` client is killed after 3 seconds, `pg_stat_activity` still shows the query as `active` 2 seconds later. After `statement_timeout`, the session is gone. This confirms the PRD's edge case: only the limit that PostgreSQL enforces stops the query.
  - A parameter that the owner is not allowed to set makes the whole connection fail (`FATAL:  permission denied to set parameter "temp_file_limit"`). Every parameter in `PGOPTIONS` must therefore be one that any user can set. All the parameters chosen here are.
- Rationale: `default_transaction_read_only` makes every statement of the session run in a read-only transaction without wrapping the query in `BEGIN` and `COMMIT`, so `psql` prints exactly one result. The tool builds the SQL itself and never issues `SET TRANSACTION READ WRITE`, which would override the default.
- Alternatives considered:
  - `-c "BEGIN READ ONLY" -c "SET LOCAL statement_timeout ..." -c <query> -c COMMIT`: rejected, because it prints extra command results and splits the query into four round trips, for the same guarantee.
  - Only a time limit on the tool's side: rejected by FR-009 and by the probe above.

## R3. Time limits on the tool's side are a backstop

- Decision: each `psql` call runs under a tool-side exec bound equal to the PostgreSQL limit plus 15 seconds. Whichever side stops the query, the runner returns `&timeoutError{timeout: <PostgreSQL limit>}`, so the reason is always `timed out after <limit>`:
  - always-collected storage queries: 60 seconds (`collectExecTimeout`), as ADR 0002 and the PRD's assumptions require
  - each breakdown table: 5 minutes, a new constant `collectDBQueryTimeout`
- Rationale: if the two limits were equal, the tool's limit would often fire first, because connecting takes time. PostgreSQL's error then never arrives, and the query keeps running on the server. The 15-second margin lets PostgreSQL stop the query first and report it. The tool-side bound still catches a hung `kubectl` or Docker daemon.
- Reason format: `timeoutError` renders whole seconds as plain seconds, so the reason is `timed out after 300s` for the breakdown and `timed out after 60s` for the storage query. The PRD wrote `timed out after 5m0s`. The plan keeps the repository's documented format ([ADR 0002](../../dev/adr/0002-non-fatal-timeout-bounded-collectors.md), [`collectors.md`](../../dev/guidelines/collectors.md)), so that support reads one format across every collector. The spec's FR-009 and Clarifications record this.

## R4. Breakdown queries must not write temporary files

- Problem: support investigates the storage of this database most often when its disk is nearly full. A breakdown query that writes large temporary files to the database volume could fill the disk, and PostgreSQL stops when it cannot write. That would break SC-005: the task manager would stop scheduling flow runs.
- Observed with default settings: the `log` breakdown (grouped by the parent flow run's flow name, so it joins `log` to `flow_run` and `flow`) uses a parallel hash join with two extra worker processes. With 300,000 flow runs the hash table fits in memory (22 MB). The hash table grows with `flow_run`, and once it no longer fits in `work_mem` PostgreSQL splits the join into batches written to temporary files under `base/pgsql_tmp`, on the same volume.
- `temp_file_limit` would cap these files, but only a superuser can set it. The probe confirmed that the owner gets `permission denied`.
- Decision: the breakdown queries set `enable_hashjoin=off`, `enable_mergejoin=off` and `max_parallel_workers_per_gather=0` in `PGOPTIONS`. Observed with these settings: the plan is a nested loop over the `flow_run` primary key with Memoize (memory bounded by `work_mem`), followed by a hash aggregate with 61 groups using 48 kB. No temporary file is written. The query takes 1.0 seconds instead of 0.3 seconds and runs in one process.
- Rationale: grouping produces few groups (flow names times state types or log levels), so the aggregate stays small. Only the join can grow with the data, and a nested loop over a primary-key index never spills. One process instead of three also keeps the load on the task manager's database lower (SC-005), and the 5-minute limit bounds the slower run time.
- Known residual effect: a sequential scan sets hint bits on pages whose rows were changed recently, and PostgreSQL may write those pages (with data checksums on, the default in PostgreSQL 18, through the write-ahead log as well). This is ordinary PostgreSQL behavior for any read, limited to recently changed pages, and it does not change any data.
- Alternatives considered:
  - Raise `work_mem` for the session: rejected, because it moves the risk from the disk to the memory of the database pod.
  - Group `task_run` and `log` by `flow_run_id` first: rejected, because the number of groups then equals the number of flow runs, and the aggregate spills in the same way.

## R5. Storage figures come from catalog functions and table statistics

- Decision: one query builds `storage.json`. It reads every table in `pg_stat_user_tables`, which covers all schemas except the system schemas, and orders the tables by total size, largest first:
  - total: `pg_total_relation_size(relid)`
  - indexes: `pg_indexes_size(relid)`
  - TOAST: `pg_total_relation_size(reltoastrelid)` when the table has a TOAST table, else 0. This includes the TOAST table's own index.
  - data: `pg_table_size(relid)` minus TOAST, which leaves the main table with its free space map and visibility map
  - These parts add up: data + indexes + TOAST = total. A unit test checks this on the parsed result.
  - live and dead rows: `n_live_tup` and `n_dead_tup`
  - `last_vacuum` and `last_autovacuum`
  - database total: `pg_database_size(current_database())`
- Observed: the query returns in 50 ms on a 1.7 GB database. TOAST held 1.24 GB of the 1.29 GB of `flow_run`, because flow run parameters are large values, which shows why the split matters to support.
- Rationale: catalog functions read file sizes and statistics, not rows, so the run time does not depend on the size of the database (SC-002). The row counts are estimates kept by PostgreSQL, which the spec's Assumptions state.

## R6. Absent tables are found before the breakdown starts

- Decision: before the breakdown, one catalog query returns, for each table on the breakdown list, whether `to_regclass('<table>')` resolves. Absent tables are recorded as `absent` with the reason `table does not exist in this Prefect version`, and their breakdown query is not run.
- Rationale: an absent table is not a failure (FR-012). Running the breakdown query and parsing `relation "x" does not exist` would also work, but would turn a normal outcome into error parsing. The same catalog query is also the connection check: if it fails with a login, `psql` or reachability error, the collector fails with that cause and does not try the seven tables.

## R7. The breakdown list is data

- Decision: a Go slice of table specifications drives the breakdown. Each entry names the table, the columns to group by (with the joins needed to reach a flow name), the timestamp column, and whether to count rows without `start_time`. One function builds the SQL for an entry, and the unit tests check the SQL for each entry.

  | Table | Grouped by | Timestamp column | Rows without `start_time` |
  |---|---|---|---|
  | `events` | `event` | `occurred` | no |
  | `flow_run` | flow name (join `flow`), `state_type` | `start_time` | yes |
  | `task_run` | flow name of the parent flow run (join `flow_run`, then `flow`), `state_type` | `start_time` | yes |
  | `flow_run_state` | `type` | `timestamp` | no |
  | `task_run_state` | `type` | `timestamp` | no |
  | `log` | flow name of the parent flow run (join `flow_run`, then `flow`), `level` | `timestamp` | no |
  | `artifact` | `type` | `created` | no |

- The joins are `LEFT JOIN`, so a task run or log record without a parent flow run falls into a group whose flow name is null, and the groups of a table add up to its row count.
- Column names were checked against Prefect 3.8.6 (`prefect/server/database/orm_models.py`): `events.event` and `events.occurred`, `log.level`, `log.flow_run_id` and `log.timestamp`, `artifact.type` and `artifact.created`, `flow_run_state.type` and `flow_run_state.timestamp`, and `state_type` and `start_time` on both run tables.

## R8. Credentials come from the existing owner lookup, with a bounded exec

- Decision: resolve the owner, password and database once per run, before the first query, in this order:
  1. `PREFECT_SERVER_DATABASE_CONNECTION_URL` or `PREFECT_API_DATABASE_CONNECTION_URL` in the operator's environment (`loadCredentialsFromEnvironment`)
  2. the same variables in the `task-manager` container, read with a bounded exec of `env`
  3. the defaults `postgres`, `prefect` and database `prefect` (`applyPostgresDefaults`)
- The parsing of the `env` output moves out of `fetchPostgresCredentials` into a pure function that both paths call. The shared, unbounded `fetchPostgresCredentials` keeps its behavior, as [`collectors.md`](../../dev/guidelines/collectors.md) requires for shared helpers.
- The `env` output of `task-manager` holds secrets. It is never written to the bundle.
- Rationale: `infrahub-collect create` does not resolve PostgreSQL credentials today; only `DetectEnvironment` and the backup path call `fetchDatabaseCredentials`, through an unbounded exec. The PRD asks to reuse the backup tool's owner lookup.
- Observed: the official PostgreSQL image trusts TCP connections from `127.0.0.1` and `::1` (`pg_hba.conf`), so on Docker Compose a wrong password still logs in. A wrong role name fails with `FATAL:  role "nobody" does not exist`. On the Helm chart's PostgreSQL and on CloudNativePG, `localhost` connections need the password.

## R9. Four error kinds, classified from the exit code and the error output

- Decision: the runner turns a failed `psql` call into one of these kinds. The manifest reason is `<kind>: <the line of the error output that names the cause>`.

  | Kind | Recognized by | Example observed or documented |
  |---|---|---|
  | timed out | `canceling statement due to statement timeout`, or the tool-side bound | `ERROR:  canceling statement due to statement timeout` |
  | `psql` missing | exit code 127, and `executable file not found` or `can't execute 'psql'` or `psql: not found` | `exec: "psql": executable file not found in $PATH` (Docker), `env: can't execute 'psql': No such file or directory` (Kubernetes, where the command is prefixed with `env`) |
  | login rejected | `password authentication failed`, `role "..." does not exist`, `database "..." does not exist`, `no pg_hba.conf entry` | `FATAL:  role "nobody" does not exist` |
  | not reachable | `could not connect to server`, `connection refused`, `Connection timed out`, a container that is not running, a pod that cannot be found | `Error response from daemon: container ... is not running` |

- Any other failure is recorded as `query failed: <line>`.
- Observed: stopping PostgreSQL inside the official image stops the container, because PostgreSQL is its main process. On Docker, "server stopped" therefore appears as a container that is not running.

## R10. No password in the bundle or the progress output

- Finding: on Kubernetes, `prepareCommand` prefixes the command with `env PGPASSWORD=<password>`, and the failure diagnostics that collectors write to `<file>.err.txt` (`writeDumpFailure`) include the command. A failed storage query would therefore write the password into the bundle.
- Decision: the runner masks the resolved password in every string it returns or writes: the recorded command, both output streams and the error message. It passes a value-based mask to `writeDumpFailure` (`strings.ReplaceAll(s, password, "********")` when the password is not empty), on top of the key-name masking of ADR 0004. A unit test fails a query on a fake backend and checks every staged file and the manifest for the password (FR-017).
- The debug lines in `command_executor.go` also print the full command, but `infrahub-collect` has no flag that turns on debug logging, so they never reach the operator.

## R11. Outcomes in the manifest and in the breakdown file

- Decision:
  - `task-manager-db` has no replica: both collectors are `skipped` with the reason `service not deployed; the task manager may use an external PostgreSQL server`.
  - Breakdown not requested: `skipped` with the reason `not requested`. This takes precedence over the service check, as for `--benchmark`.
  - The storage query fails: `failed` with `<kind>: <cause>`, and the diagnostics go to `task-manager-db/storage.err.txt` (the existing rule that a failed dump never writes the file it names).
  - The breakdown's catalog query fails: `failed` with `<kind>: <cause>`, diagnostics in `storage-breakdown.err.txt`, and no table is attempted.
  - One or more breakdown tables fail: `storage-breakdown.json` is still written with every table's status, and the collector is `failed` with the reason `partial storage breakdown: <table>: <reason>; ...`.
  - Only absent tables, or none: `success`.
- Exception to the collector guideline: [`collectors.md`](../../dev/guidelines/collectors.md) asks collectors to wrap a sub-command `timeoutError` with `%w`. The orchestrator then replaces the whole reason with `timed out after <duration>`, which would drop the table names that FR-016 requires. The breakdown collector therefore returns a plain error whose text names each table. The guideline gains a sentence describing this exception.

## R12. Order in the collector plan

- Decision:
  - `task-manager-db-storage` runs right after `task-manager-state`, with the other always-on diagnostics.
  - `task-manager-db-storage-breakdown` runs first among the opt-in collectors: before `benchmark`, which loads the disk, and before `backup`, which may stop containers and must stay last.
- Rationale: the breakdown should read the database before anything else in the run loads or stops it, and a long breakdown should not delay the always-on figures.

## R13. Progress output

- Decision: the breakdown logs `Collecting storage breakdown of table <table> (time limit 300s)` before each table, and `Table <table>: <n> rows in <m> groups (<duration>)`, `Table <table>: absent`, or a warning with the failure reason after it (FR-013).

## R14. Testing approach

- Unit tests, with the fake backends already used in `collect_diagnostics_test.go`:
  - the command and environment the runner builds, including `PGOPTIONS` and the time limits (FR-008, FR-009)
  - each error kind, from recorded error outputs (FR-011)
  - password masking in staged diagnostics (FR-017)
  - parsing of fixed query results into `storage.json` and `storage-breakdown.json`, size formatting, and the size invariant (FR-002, FR-003)
  - the SQL built for each breakdown entry (FR-005 to FR-007)
  - collector outcomes for not requested, not deployed, absent tables, a timed-out table and a failed catalog query (FR-004, FR-010, FR-012, FR-016)
  - the plan order and the progress lines (FR-013)
  - Kubernetes pod selection: the existing `findPrimaryPod` tests, plus a test that the storage query runs on the pod that `getPodForServiceContext` selects (FR-014)
- End-to-end tests:
  - Docker: seed a flow, completed flow runs and one flow run cancelled before it starts, through the Prefect client in a `task-worker` container. Run `create --include-task-manager-storage`. Check both files against the seed, and assert the constraints of the JSON Schemas in `contracts/` by hand, as `tests/helpers/bundle.py` already does for the manifest, because `jsonschema` is not a test dependency.
  - Docker, time limit: hold `LOCK TABLE artifact IN ACCESS EXCLUSIVE MODE` in a separate session, run the breakdown with the limit lowered through the undocumented variable `INFRAHUB_TASK_MANAGER_STORAGE_TABLE_TIMEOUT`, then check that `artifact` failed with `timed out after <limit>`, that the other tables were collected, and that `pg_stat_activity` shows no `infrahub-collect` session.
  - Kubernetes: run `create` and check `storage.json`. The CI cluster has no CloudNativePG, so the primary-pod selection is covered by unit tests and checked manually with the kind and CloudNativePG procedure in [quickstart.md](quickstart.md).
- The test variable accepts only values lower than the 5-minute limit. A larger value is ignored with a warning, so it cannot weaken the guarantee in FR-009.

## R15. Architecture decision record

- Decision: add `dev/adr/0008-database-enforced-query-limits.md`. It records that queries in the task manager database run with time limits that PostgreSQL enforces, in read-only sessions, with join methods that never write temporary files. The PRD notes that a limit enforced by the database is new for collectors.
