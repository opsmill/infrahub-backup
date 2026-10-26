# Implementation Plan: Storage Figures for the Task Manager Database in Troubleshooting Bundles

**Branch**: `fac/report-taskmgr-storage-usage-d8y7s` | **Date**: 2026-10-26 | **Spec**: [spec.md](spec.md)

**Input**: Feature specification from `specs/006-taskmgr-db-storage/spec.md`

## Summary

Support cannot view from a bundle how much storage the task manager database uses or what uses it. Two new collectors in `infrahub-collect` run `psql` inside the `task-manager-db` container, in the same way the backup tool runs `pg_dump`:

- `task-manager-db-storage` runs on every `create`. One catalog query of about 50 ms writes `task-manager-db/storage.json`: the database size, and for each table the data, index and TOAST sizes, the live and dead row estimates, and the last vacuum and autovacuum times.
- `task-manager-db-storage-breakdown` runs only with `--include-task-manager-storage`. For each of seven Prefect tables, it writes exact row counts and oldest and newest timestamps for each group of records into `task-manager-db/storage-breakdown.json`.

Every query runs in a read-only session with a time limit that PostgreSQL enforces (60 seconds for the storage query, 5 minutes for each breakdown table). The breakdown turns off hash joins, merge joins and parallel workers, so it never writes temporary files to a disk that may already be nearly full ([research.md](research.md), R2 and R4).

## Technical Context

**Language/Version**: Go 1.25.0 (pinned in `go.mod`)

**Primary Dependencies**: no new dependency. Uses cobra, viper, logrus, and `pgx` (already used only to parse the connection URL). Deployment access stays through the `docker` and `kubectl` command-line tools (ADR 0001).

**Storage**: none. The tool reads the task manager's PostgreSQL database (Prefect 3 schema) in read-only sessions and writes two JSON files into the bundle staging directory.

**Testing**: `make test` (Go unit tests with fake collect backends), `make vet`, `make lint`; pytest end-to-end suites `tests/e2e/test_docker_collect.py` (`-m docker`) and `tests/e2e/test_k8s_collect.py` (`-m k8s`).

**Target Platform**: the `infrahub-collect` binary on Linux, macOS and Windows (amd64 and arm64, `CGO_ENABLED=0`), against Docker Compose or Kubernetes deployments. `psql` must be present in the `task-manager-db` image.

**Project Type**: command-line tool (three binaries sharing `src/internal/app`).

**Performance Goals**: the storage figures add no more than 10 seconds to a run (SC-002); measured at 50 ms for the query on a 1.7 GB database. Each breakdown table finishes or stops within 5 minutes (SC-003).

**Constraints**:

- Read-only: no write to the database, and no stop, restart or scale of any workload (ADR 0003, FR-008).
- PostgreSQL enforces each time limit, so an interrupted run cannot leave a query running for longer (FR-009).
- No temporary files on the database volume from the breakdown, and one PostgreSQL process per query (SC-005, research R4).
- No password in any bundle file, manifest reason or progress line (FR-017).
- Non-fatal: every failure is a manifest outcome, and the bundle is still produced (ADR 0002, FR-011).

**Scale/Scope**: seven Prefect tables in the breakdown, each possibly holding tens of millions of rows; every user table in the storage report.

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| Principle | Status | How the design meets it |
|---|---|---|
| I. Split-binary, shared-core architecture | Pass | All logic is in `src/internal/app` (two new `collect_*.go` files). `src/cmd/infrahub-collect/main.go` only registers the flag. No new binary. |
| II. Operational safety and data integrity | Pass | The collectors only read, in read-only sessions. They never flush, cancel or vacuum. The breakdown is opt-in, bounded per table by PostgreSQL, and avoids temporary files on the database volume. Backup and restore paths do not change. |
| III. Self-contained, cross-platform binaries | Pass | The SQL is built in Go from an embedded list, with no file shipped next to the binary. `psql` is a runtime that is already in the target container; when it is missing, the collector fails with the reason `psql missing: ...` (FR-011). No CGO, no new module. |
| IV. Quality gates | Pass | Unit tests for the query runner, the report types, the SQL builder and both collectors; end-to-end tests on Docker and Kubernetes. |
| V. Explicit errors and streaming feedback | Pass | Errors are wrapped with `%w` and returned to the orchestrator. The breakdown logs each table as it starts and ends (FR-013). |
| Deployment contract | Pass | Uses the existing `task-manager-db` and `task-manager` services. No service is added or renamed. |
| Nix vendor hash | Not affected | `go.mod` and `go.sum` do not change. |
| Documentation | Pass | Four docs pages change (see [contracts/cli.md](contracts/cli.md)); Vale and rumdl must pass. |

Post-design re-check (after Phase 1): still passes. The design adds one documented exception to a repository guideline, not to the constitution: the breakdown collector does not wrap per-table `timeoutError`s with `%w`, so the manifest reason keeps the table names (research R11). The guideline file is updated in the same change.

## Project Structure

### Documentation (this feature)

```text
specs/006-taskmgr-db-storage/
├── spec.md
├── plan.md                 # this file
├── research.md             # Phase 0
├── data-model.md           # Phase 1
├── quickstart.md           # Phase 1
├── contracts/
│   ├── cli.md
│   ├── manifest-outcomes.md
│   ├── storage.schema.json
│   └── storage-breakdown.schema.json
├── checklists/requirements.md
└── tasks.md                # Phase 2 (speckit-tasks)
```

### Source Code (repository root)

```text
src/cmd/infrahub-collect/main.go            # + --include-task-manager-storage flag and viper binding
src/internal/app/
├── collect.go                              # + CollectOptions.IncludeTaskManagerStorage, collectDBQueryTimeout, plan order
├── collect_taskmanager_db.go               # new: credentials (bounded), psql command builder, PGOPTIONS,
│                                           #      error kinds, password mask, query runner
├── collect_taskmanager_db_test.go          # new
├── collect_storage.go                      # new: report types, storage query, breakdown list and SQL builder,
│                                           #      both collectors, test-only limit override
├── collect_storage_test.go                 # new
├── app_config.go                           # extract a pure parser of `env` output; behavior unchanged
└── app_config_test.go                      # + parser test
tests/helpers/bundle.py                     # + expected collectors, opt-in set, task-manager-db directory
tests/e2e/test_docker_collect.py            # + seeded breakdown test, time-limit test, storage assertions
tests/e2e/test_k8s_collect.py               # + storage.json assertions
docs/docs/collect/index.mdx                 # "What a bundle contains"
docs/docs/collect/create.mdx                # how to collect the breakdown, upgrade warning
docs/docs/reference/commands.mdx            # flag and bundle tables
docs/docs/reference/configuration.mdx       # flag and environment variable
dev/adr/0008-database-enforced-query-limits.md   # new ADR (research R15)
dev/guidelines/collectors.md                # timeout-wrapping exception, database query rule
dev/knowledge/infrahub-collect.md           # bundle layout and flag table
changelog/181.added.md                      # towncrier fragment
AGENTS.md                                   # collect file list
```

**Structure Decision**: the feature follows the existing collector layout: one `collect_<area>.go` file for each area, with its tests next to it. The query runner is in its own file because the two collectors share it and because it holds the security-sensitive parts (credentials, password masking, error classification).

## Design Overview

### Query runner (`collect_taskmanager_db.go`)

1. `resolveTaskManagerDBCredentials(cc)` runs once per run and caches the result on the collect context: operator environment, then a bounded `env` exec in `task-manager`, then defaults (research R8).
2. `runTaskManagerDBQuery(cc, query dbQuery) (string, error)` builds:
   - the command `psql -h localhost -U <owner> -d <database> -X -q -A -t -v ON_ERROR_STOP=1 -c <sql>`
   - the environment `PGPASSWORD`, `PGAPPNAME=infrahub-collect`, `PGCONNECT_TIMEOUT=10`, and `PGOPTIONS` with `default_transaction_read_only=on`, `statement_timeout`, `TimeZone=UTC`, and, for the breakdown, `enable_hashjoin=off`, `enable_mergejoin=off` and `max_parallel_workers_per_gather=0`
   - It runs the command with `ExecSeparateContext` on `task-manager-db`, bounded at the PostgreSQL limit plus 15 seconds, and returns stdout or a `*dbQueryError` (research R3, R9).
3. `maskSecret(password)` returns the mask that the runner applies to every string it returns and that the collectors pass to `writeDumpFailure` (research R10).

### Storage collector (`collect_storage.go`)

- Skip when `task-manager-db` has no replica, with the external-PostgreSQL reason.
- Run the storage query (60-second limit), parse the JSON, add readable sizes, sort, and write `storage.json`. On failure, write `storage.err.txt` and return the classified error.

### Breakdown collector (`collect_storage.go`)

- Skip with `not requested` without the flag; then skip when `task-manager-db` has no replica.
- Run the catalog query; on failure, write `storage-breakdown.err.txt` and fail.
- For each entry of the breakdown list, in order: log the start, mark absent tables, run the table's query with the 5-minute limit (or the lowered test limit), parse the groups, sum the rows, and log the result. Stop between tables if the run is interrupted.
- Write `storage-breakdown.json` with every table's status, then return `partial storage breakdown: ...` if any table failed.

### Plan order (`collect.go`)

`... task-manager-state, task-manager-db-storage, server-info, telemetry, metrics, task-manager-db-storage-breakdown, benchmark, backup` (research R12).

## Risks

| Risk | Mitigation |
|---|---|
| The breakdown loads the task manager's database during business hours | Opt-in flag; one PostgreSQL process per query; 5-minute limit per table; documentation says when to use it. |
| The breakdown fills a nearly full disk with temporary files | Hash and merge joins off, so joins use the primary-key index; aggregates have few groups (research R4). |
| A schema change during an upgrade waits behind a breakdown query | Limited to 5 minutes by PostgreSQL; documentation says not to use the flag during an upgrade. |
| The password leaks into `.err.txt` through the `env PGPASSWORD=...` prefix on Kubernetes | Value-based masking of every string the runner returns and writes; unit test (FR-017). |
| Credentials resolve to the wrong role when `task-manager` is not deployed | The collectors fail with `login rejected: ...`, which names the role; the bundle is still produced. |
| A future Prefect version renames a table or column | A missing table is `absent`; a missing column fails that table only, with the PostgreSQL message as the reason. |

## Complexity Tracking

No constitution violation needs a justification.
