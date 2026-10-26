# Data Model: Storage Figures for the Task Manager Database

**Feature**: [spec.md](spec.md) | **Research**: [research.md](research.md)

The feature stores nothing. It reads the task manager's database and writes two JSON files into the bundle. This page describes those files, the internal types that produce them, and the collector outcomes. The JSON Schemas in [`contracts/`](contracts/) are the authoritative file contracts.

## Bundle files

Both files are in `bundle/task-manager-db/`, on Docker and Kubernetes alike.

### Size

Every size in both files has this shape (FR-003):

| Field | Type | Meaning |
|---|---|---|
| `bytes` | integer | The size in bytes, for scripts. |
| `readable` | string | The same size as `formatBytes` writes it, for example `1.6 GB` (base 1024, one decimal). |

### `storage.json` (always collected, FR-001 to FR-003)

| Field | Type | Meaning |
|---|---|---|
| `collected_at` | string, RFC 3339 UTC | When the query ran. |
| `database` | string | The name of the Prefect database. |
| `database_size` | Size | `pg_database_size` of that database. |
| `statistics_reset_at` | string, RFC 3339 UTC, or null | When PostgreSQL last reset the database's statistics (`pg_stat_database.stats_reset`). The row counts and vacuum times cover the period since then. Null when they were never reset. |
| `tables` | array of table entries | Every table in `pg_stat_user_tables`, largest total size first. |

Table entry:

| Field | Type | Meaning |
|---|---|---|
| `schema` | string | The table's schema, usually `public`. |
| `name` | string | The table name. |
| `total_size` | Size | Data, indexes and TOAST together. |
| `data_size` | Size | The table itself, with its free space map and visibility map, without TOAST. |
| `index_size` | Size | All indexes of the table. |
| `toast_size` | Size | The TOAST table and its index, where PostgreSQL stores large values. 0 bytes when the table has none. |
| `live_rows` | integer | The estimate PostgreSQL keeps of live rows. |
| `dead_rows` | integer | The estimate PostgreSQL keeps of deleted rows not reclaimed yet. |
| `last_vacuum` | string, RFC 3339 UTC, or null | The last manual VACUUM. |
| `last_autovacuum` | string, RFC 3339 UTC, or null | The last VACUUM run by autovacuum. |

Validation rules:

- `data_size.bytes + index_size.bytes + toast_size.bytes == total_size.bytes` for every table.
- `tables` is ordered by `total_size.bytes`, largest first; ties are ordered by schema, then name.

### `storage-breakdown.json` (only with the flag, FR-005 to FR-007, FR-012, FR-016)

| Field | Type | Meaning |
|---|---|---|
| `collected_at` | string, RFC 3339 UTC | When the breakdown started. |
| `database` | string | The name of the Prefect database. |
| `time_limit_per_table` | string | The limit PostgreSQL enforced on each table, for example `300s`. |
| `tables` | array of table breakdowns | One entry per table on the breakdown list, in list order. |

Table breakdown:

| Field | Type | Meaning |
|---|---|---|
| `name` | string | The table name. |
| `status` | `collected`, `absent` or `failed` | The outcome for this table. |
| `reason` | string | Present when `status` is `absent` or `failed`, for example `timed out after 300s`. |
| `grouped_by` | array of strings | The group key names, for example `["flow_name", "state_type"]`. |
| `timestamp_column` | string | The column the oldest and newest times come from. |
| `rows` | integer | Present when `collected`: the exact row count, the sum of `rows` over the groups. |
| `groups` | array of groups | Present when `collected`: ordered by `rows`, largest first, then by key values. |

Group:

| Field | Type | Meaning |
|---|---|---|
| `group` | object of string or null | One field per name in `grouped_by`. Null when the value is missing, for example a log record without a parent flow run. |
| `rows` | integer | The exact number of rows in the group. |
| `rows_without_start_time` | integer | Only for `flow_run` and `task_run`: rows whose `start_time` is null. A flush never removes these rows. |
| `oldest` | string, RFC 3339 UTC, or null | The oldest value of `timestamp_column` in the group. Null when every row has no value. |
| `newest` | string, RFC 3339 UTC, or null | The newest value of `timestamp_column` in the group. |

Group key names and values:

| Table | `grouped_by` | Values |
|---|---|---|
| `events` | `event_name` | The `event` column, for example `prefect.flow-run.Completed`. |
| `flow_run` | `flow_name`, `state_type` | The flow's name, and the run's state type, for example `COMPLETED`. |
| `task_run` | `flow_name`, `state_type` | The name of the parent flow run's flow, or null; the run's state type. |
| `flow_run_state` | `state_type` | The `type` column. |
| `task_run_state` | `state_type` | The `type` column. |
| `log` | `flow_name`, `log_level` | The name of the parent flow run's flow, or null; the level name for the standard levels (10 `DEBUG`, 20 `INFO`, 30 `WARNING`, 40 `ERROR`, 50 `CRITICAL`), otherwise the number as text. |
| `artifact` | `artifact_type` | The `type` column, or null. |

Status transitions for one table, in the order the collector decides them:

1. The catalog query reports that the table does not exist → `absent`, with the reason `table does not exist in this Prefect version`. No query runs.
2. The breakdown query succeeds → `collected`.
3. The breakdown query fails → `failed`, with the reason from the query runner, for example `timed out after 300s` or `query failed: <cause>`.

## Internal types

Names are indicative; they follow the existing `collect*.go` naming.

- `breakdownTableSpec`: one entry of the breakdown list (research R7). Fields: table name, group key names with the SQL expression and joins for each, the timestamp column, and whether to count rows without `start_time`. The list is a package-level slice; `breakdownQuery(spec)` builds its SQL.
- `taskManagerDBCredentials`: owner, password and database, resolved once per run and cached on the collect context (research R8).
- `dbQuery`: the SQL, the time limit PostgreSQL enforces, and whether the breakdown planner settings apply (research R4).
- `dbQueryError`: a failed query, with a `kind` (`timed out`, `psql missing`, `login rejected`, `not reachable`, `query failed`) and the line of the error output that names the cause. For the `timed out` kind it unwraps to `*timeoutError`, so code that checks for timeouts keeps working.
- `storageReport` and `storageBreakdown`: the Go types of the two files, marshalled with `json.MarshalIndent` like the other bundle documents.

## Collectors and manifest entries

The manifest's `collectors` list gains two entries. The manifest version does not change ([ADR 0005](../../dev/adr/0005-uniform-bundle-and-manifest.md)).

| Name | Runs | Files |
|---|---|---|
| `task-manager-db-storage` | every run, after `task-manager-state` | `task-manager-db/storage.json`, or `task-manager-db/storage.err.txt` when the query fails |
| `task-manager-db-storage-breakdown` | with `--include-task-manager-storage`, first among the opt-in collectors | `task-manager-db/storage-breakdown.json`, or `task-manager-db/storage-breakdown.err.txt` when the catalog query fails |

The outcome and reason for each case are in [`contracts/manifest-outcomes.md`](contracts/manifest-outcomes.md).
