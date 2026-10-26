<!-- Extracted from specs/003-collect-tool on 2026-07-09; updated from specs/006-taskmgr-db-storage on 2026-10-26 -->

# Collector Guidelines

## Overview

Conventions for writing and maintaining `infrahub-collect` collectors (`src/internal/app/collect*.go`). A collector gathers one category of diagnostic data into the staging bundle. These rules keep collection safe on degraded instances and keep the bundle trustworthy. Background: [ADR 0002](../adr/0002-non-fatal-timeout-bounded-collectors.md), [ADR 0003](../adr/0003-read-only-collection.md), [ADR 0004](../adr/0004-key-name-secret-masking.md), [ADR 0008](../adr/0008-database-enforced-query-limits.md).

## Time-bound every subprocess

A collector runs against a possibly-wedged container. Never call the context-free `runCommand`/`Exec` on the collect path — a hang there stalls the whole run. Use the bounded variants and the standard bounds:

- exec / status dumps → `collectExecTimeout` (60s)
- log downloads and file copies → `collectTransferTimeout` (5 min)
- queries in the task manager database → the limit that PostgreSQL enforces, plus `collectDBQueryGrace` (15s) on the tool's side (see [Query the task manager database through `runTaskManagerDBQuery`](#query-the-task-manager-database-through-runtaskmanagerdbquery))

A timeout must surface as a `*timeoutError` so the orchestrator records the manifest reason verbatim as `timed out after <duration>`. When aggregating several sub-commands into one collector error, `%w`-wrap a sub-command timeout so `errors.As` still finds it — do not flatten it into a `%v` string.

Exception: a collector that reports several named sub-results may return a plain error whose text names each failed item and its reason, so the manifest keeps the names. The storage breakdown does this with one result per table (`partial storage breakdown: log: timed out after 300s; …`). If it wrapped a table's `*timeoutError` with `%w`, the orchestrator would replace the whole reason with `timed out after <duration>` and drop the table names. Each item's outcome is also recorded in the collector's own file (`storage-breakdown.json`).

## Failures are non-fatal; distinguish absent from failed

Return a wrapped error on failure; the orchestrator logs a `WARN` and records the collector as `failed` with the reason. Never call `os.Exit` or print errors directly (constitution Principle V). The run must continue and still exit 0.

- **Service not deployed** → record `skipped` with a reason (via the skip precondition). A genuine "nothing matched" enumeration returns `(nil, nil)`.
- **Service present but the command failed** → record `failed`. Do not collapse an execution error into "not deployed"; that produces a misleadingly-empty bundle.

Do not mark a collector `success` when it collected nothing due to a swallowed error — an empty file that looks intentional is worse than a recorded failure.

## Route sensitive output through the masking choke-point

Any env or configuration dump that can carry secrets MUST pass through `masking.go` before it is written, and MUST be added to the enumerated masked-output set (see [ADR 0004](../adr/0004-key-name-secret-masking.md)). Pick the matching helper: `maskEnvOutput` (KEY=VALUE), `maskConfigPairs` (Redis alternating lines), `maskErlangConfig` (rabbitmqctl tuples), `maskJSON` (JSON). Never write a new sensitive dump without masking it.

## Query the task manager database through `runTaskManagerDBQuery`

A collector that reads the task manager database sends its SQL through `runTaskManagerDBQuery` (`collect_taskmanager_db.go`) and never builds its own `psql` call. The runner applies [ADR 0008](../adr/0008-database-enforced-query-limits.md):

- Bounded exec into `task-manager-db`: `psql` runs through `ExecSeparateContext`, bounded at the query's limit plus `collectDBQueryGrace`. There is no fallback to the unbounded `Exec`.
- Read-only session: `PGOPTIONS` sets `default_transaction_read_only=on`. The SQL is built in Go (`storageQuery`, `breakdownCatalogQuery`, `breakdownQuery`) and never contains `SET TRANSACTION READ WRITE`, which would override the read-only default.
- A limit that PostgreSQL enforces: `PGOPTIONS` sets `statement_timeout` from `dbQuery.limit`, so the query stops on the server even when the tool has stopped waiting. Use `collectExecTimeout` for a query that reads only the catalog and statistics and `collectDBQueryTimeout` for a query that reads every row of a table, and pass the limit through `effectiveDBQueryLimit` so the end-to-end tests can lower it. Set `dbQuery.breakdown` on a query that reads every row, so hash joins, merge joins and parallel workers are off and the query writes no temporary files.
- Only parameters that any user can set: PostgreSQL rejects the connection when the login cannot set a parameter in `PGOPTIONS`. Check a new parameter on a database owner without superuser rights, then add it to `dbSessionParameterAllowlist`.
- Value-based password masking: the runner replaces the password (`maskSecretValue`) in the command, stderr and the error text, and in stdout only when the query fails, because a failed query's stdout goes to `<file>.err.txt`. A successful query's stdout is the result that the collector parses, and it never contains the command line. Masking it would corrupt the result when the password is a common word: the default password `prefect` is also the database name and part of event names such as `prefect.flow-run.Completed`. Pass the same mask to `writeDumpFailure`.
- Credentials: get the login from `resolveTaskManagerDBCredentials`, which resolves it once per run. The `env` output it reads from `task-manager` and `task-manager-db` holds secrets: never write it to the bundle or the log. Log only where the credentials came from.
- Errors: a failed query returns a `*dbQueryError` whose text is the manifest reason, `<kind>: <cause>` or `timed out after <limit>`. Return it from the collector as it is, or, in a collector with several named sub-results, use its text as the item's reason.

## Stay read-only

A collector must never stop, restart, or scale a deployment workload. Use only read/exec operations. The sole permitted mutations are the tool's **own** transient resources (the `--benchmark` container/pod it creates and deletes) and the opt-in `--include-backup` delegation.

## Don't mutate shared backend helpers — add bounded variants

`EnvironmentBackend` helpers (`CopyFrom`, `Exec`, `getPodForService`, `getInfrahubVersion`, …) are shared with `infrahub-backup`/`infrahub-taskmanager`. When collect needs a timeout-bounded or otherwise different behaviour, **add a collect-only variant** (e.g. `CopyFromContext`, `getPodForServiceContext`, `ExecContext`) rather than changing the shared method's signature or behaviour. Keep compile-time `var _ collectBackend = (*DockerBackend)(nil)` / `(*KubernetesBackend)(nil)` assertions so a seam change fails at build time, not at runtime.

## Non-interactive invocations must not gate on unbounded waits

Anything collect calls must complete without waiting on an operator or on an open-ended condition. When reusing an interactive path, pass the non-interactive flag: `--include-backup` calls `CreateBackup(force=true, …)` so it skips the unbounded `waitForRunningTasks` loop (see [ADR 0006](../adr/0006-include-backup-reuses-createbackup.md)).

## Testing

- Unit-test pure logic table-driven (masking, manifest shape, filename derivation, arg construction, replica parsing) — no mocking framework; use a fake `collectBackend`.
- Cover the failure branches: a collector that fails still exits 0 and records `failed`; a read-only guard test asserts no `Stop`/`Start`/scale is called across the full plan.
- Container-touching flows are covered by the `-m docker` / `-m k8s` pytest e2e suites (Docker and Kubernetes) in CI. Note the CI edition matrix (`community`/`enterprise`): behaviours that differ by Neo4j edition (e.g. the backup path) must be validated on both.
