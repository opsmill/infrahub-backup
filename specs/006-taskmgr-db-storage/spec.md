# Feature Specification: Storage Figures for the Task Manager Database in Troubleshooting Bundles

**Feature Branch**: `fac/report-taskmgr-storage-usage-d8y7s`

**Created**: 2026-10-26

**Status**: Draft

**Input**: The PRD for [opsmill/infrahub-backup#181](https://github.com/opsmill/infrahub-backup/issues/181), published at <https://github.com/opsmill/infrahub-backup/issues/181#issuecomment-6422690269>, and the scoping comment at <https://github.com/opsmill/infrahub-backup/issues/181#issuecomment-6420264056>.

## Problem

The task manager (Prefect) stores its data in a PostgreSQL database, the `task-manager-db` service. When support investigates a deployment, they need to know how much storage this database uses and what uses most of it. A bundle created with `infrahub-collect create` holds the container logs of `task-manager-db`, lists of Prefect records, and container resource usage. None of these include the disk space the database or its tables use, so support has to send the customer SQL queries to run.

From a bundle alone, a support engineer must be able to answer four questions about the task manager database:

1. How big is the database?
2. Which tables use the space?
3. Is that space live data, or deleted rows that PostgreSQL has not reclaimed yet? VACUUM is the PostgreSQL command that reclaims the space of deleted rows.
4. Within the largest tables, which kinds of records use the space?

Every bundle answers questions 1 to 3. The operator adds a flag when support asks for question 4, because answering it reads every row of the largest tables. The answers tell support whether to recommend `infrahub-taskmanager flush flow-runs` with a shorter retention period, a VACUUM, or neither.

## Clarifications

### Session 2026-10-26

The PRD left three questions open. They were resolved from the sources below, without asking the user, as the preparation workflow requires.

- Q: What is the name of the flag that requests the breakdown? → A: `--include-task-manager-storage`, with the environment variable `INFRAHUB_INCLUDE_TASK_MANAGER_STORAGE`. This is the name the PRD proposes, and it follows the `--include-backup` and `--include-queries` flags of the same command.
- Q: Does `infrahub tasks flush flow-runs`, which `infrahub-taskmanager flush flow-runs` runs before it falls back to [`clean_old_tasks.py`](../../src/internal/app/scripts/clean_old_tasks.py), filter flow runs the same way as that script? → A: Yes. Both delete flow runs whose `start_time` is before the cutoff and whose state type is `COMPLETED`, `FAILED` or `CANCELLED`. In the opsmill/infrahub repository, the filter is in `backend/infrahub/task_manager/flow_run/retention.py` (`FlowRunRetention.purge`) today, and it was the same in the commit that added the flush command (`51a684bc76`, first released in Infrahub 1.4.13). So the start times in the breakdown predict what a flush removes.
- Q: Must `infrahub-collect` support deployments that run Prefect 2.x? → A: No. Every Infrahub release that ships a task manager pins Prefect 3: Infrahub 1.0.0 pins Prefect 3.0.3 and Infrahub 1.1.0 pins Prefect 3.0.11, and the 0.16 releases do not depend on Prefect. The breakdown targets the Prefect 3 schema. A table that a Prefect version does not have is recorded as absent (FR-012).
- Q: In what format does the manifest record a table that reaches the 5-minute limit? → A: `timed out after 300s`. The PRD wrote `timed out after 5m0s`, but every collector already records timeouts as `timed out after <duration>` with whole seconds written as plain seconds (`formatCommandTimeout` in [`command_executor.go`](../../src/internal/app/command_executor.go), [ADR 0002](../../dev/adr/0002-non-fatal-timeout-bounded-collectors.md)). Using the same format lets support read every timeout reason the same way. The limit itself does not change.
- Q: Does "`flow_run` by flow name and by state type" mean two separate groupings or one grouping by both? → A: One grouping by both: one group for each combination of flow name and state type. The figures for flow name alone or state type alone are sums of these groups, and support needs both together to estimate a flush, because a flush removes only some state types. The same applies to `task_run` (parent flow name and state type) and `log` (parent flow name and log level).

## User Scenarios & Testing *(mandatory)*

Both journeys ship in the first release.

### User Story 1 - Support reads the size of the database and of each table (Priority: P1)

A support engineer opens a bundle and, without asking the customer anything, reads the total size of the task manager database, the tables that use the most space, and how many deleted rows in each table PostgreSQL has not reclaimed. This tells them whether the database explains a full disk, and whether a VACUUM would free space that a flush would not.

**Why this priority**: These figures are cheap to collect and answer the first question support asks on every disk-use case. They are in every bundle, so support does not need a second round trip with the customer.

**Independent Test**: On a Docker Compose deployment, run `infrahub-collect create` without any new flag, then open `task-manager-db/storage.json` in the bundle. The total size matches what PostgreSQL reports for the Prefect database, and every table in the database has an entry.

**Acceptance Scenarios**:

1. **Given** a deployment where `task-manager-db` runs, **When** the operator runs `infrahub-collect create`, **Then** the bundle's `task-manager-db/storage.json` holds:
   - the total size of the Prefect database
   - the size of each table, split into data, indexes and TOAST storage (where PostgreSQL stores large values)
   - the live and dead row counts of each table
   - the last vacuum and autovacuum time of each table
2. **Given** the same deployment, **When** the support engineer reads `storage.json`, **Then** each size appears both as a number of bytes and as readable text, for example `12 GB`.
3. **Given** a deployment where `task-manager-db` is not deployed, for example because the task manager uses an external PostgreSQL server, **When** the operator runs `infrahub-collect create`, **Then** the bundle is produced and the manifest records the storage collector as `skipped`, with a reason saying that the task manager may use an external PostgreSQL server.
4. **Given** a deployment where `task-manager-db` is stopped, rejects the login, or has no `psql` command, **When** the operator runs `infrahub-collect create`, **Then** the bundle is produced and the manifest records the storage collector as `failed`, with a reason that names the cause.

---

### User Story 2 - Support reads which kinds of records fill the largest tables (Priority: P2)

When the size figures show that a few tables use most of the space, support asks the operator to run the collection again with `--include-task-manager-storage`. The support engineer then reads, for each table on the breakdown list, the exact row count and the oldest and newest timestamps of each group of records. This tells them which flows, events or logs fill the database, and what a flush with a given retention period would remove.

**Why this priority**: The breakdown answers the question that decides the recommendation, but it reads every row of the largest tables. It runs only when the operator asks for it, and only after the size figures show that it is needed.

**Independent Test**: On a Docker Compose deployment seeded with known flow runs, including one cancelled before it starts, and known events, run `infrahub-collect create --include-task-manager-storage`. `task-manager-db/storage-breakdown.json` holds the seeded counts and timestamps, and the manifest records the breakdown collector as `success`.

**Acceptance Scenarios**:

1. **Given** a deployment whose `events` table holds 5 million rows, **When** the operator runs `infrahub-collect create` with the flag, **Then** the bundle's `task-manager-db/storage-breakdown.json` lists the event names that account for those rows, each with its row count and its oldest and newest `occurred` time.
2. **Given** the same deployment, **When** the operator runs `infrahub-collect create` without the flag, **Then** the manifest records the breakdown collector as `skipped` with the reason `not requested`.
3. **Given** a deployment with a flow run that was cancelled before it started, **When** the operator runs the collection with the flag, **Then** the `flow_run` group of that flow name and the `CANCELLED` state type counts one row with no start time.
4. **Given** a breakdown query on one table that runs longer than 5 minutes, **When** the limit is reached, **Then** PostgreSQL stops the query, the table is recorded as failed with the reason `timed out after 300s`, and the remaining tables are still collected.
5. **Given** the operator runs the collection with the flag, **When** the breakdown starts on each table, **Then** the progress output names that table.

---

### Edge Cases

- `task-manager-db` is stopped, rejects the login, or has no `psql` command: the collectors are recorded as failed with the cause, and the bundle is still produced (FR-011).
- The task manager uses an external PostgreSQL server: the collectors are recorded as skipped with that reason (FR-010).
- A table on the breakdown list does not exist in the deployed Prefect version: the table is recorded as absent (FR-012).
- A task run or log record has no parent flow run: it is counted in a group with no flow name, so the groups of a table add up to the table's row count.
- The operator interrupts the run during a breakdown query: the tool removes its staging directory as it does today. Killing the `docker exec` or `kubectl exec` process is not enough to stop the query. By default, PostgreSQL notices a lost client only the next time it reads from or writes to the connection ([`client_connection_check_interval`](https://www.postgresql.org/docs/current/runtime-config-connection.html)). The 5-minute limit that PostgreSQL enforces ([`statement_timeout`](https://www.postgresql.org/docs/current/runtime-config-client.html)) is what stops the query.
- The task manager's database schema changes while a breakdown query runs, for example during an upgrade: a read-only query holds an `ACCESS SHARE` lock, and many forms of `ALTER TABLE` need an `ACCESS EXCLUSIVE` lock, which conflicts with it ([PostgreSQL explicit locking](https://www.postgresql.org/docs/current/explicit-locking.html)). The schema change waits for up to 5 minutes. The documentation for the flag says not to use it while the task manager is being upgraded (FR-015).
- The Kubernetes deployment runs CloudNativePG replicas: a standby does not run VACUUM or autovacuum ([PostgreSQL hot standby](https://www.postgresql.org/docs/current/hot-standby.html)), so the queries run on the primary pod (FR-014).

## Requirements *(mandatory)*

### Functional Requirements

Each requirement ends with how to test it.

- **FR-001**: When `task-manager-db` is deployed, the system MUST record the total size of the Prefect database in `task-manager-db/storage.json` on every run.
  - Test: an end-to-end run on Docker Compose. The value matches `pg_database_size` for the same database.
- **FR-002**: For each table in the Prefect database, the system MUST record:
  - the total, data, index and TOAST sizes
  - the live and dead row counts
  - the last vacuum and autovacuum times
  - Test: a unit test parses a fixed query result into the expected file.
- **FR-003**: The system MUST give each size both as a number of bytes and as readable text, for example `12 GB`.
  - Test: a unit test.
- **FR-004**: Users MUST be able to request the breakdown with the `create` flag `--include-task-manager-storage` or the environment variable `INFRAHUB_INCLUDE_TASK_MANAGER_STORAGE`. Without either, the breakdown collector MUST be recorded as `skipped` with the reason `not requested`, as `--include-backup` and `--benchmark` are today.
  - Test: a unit test of the collector plan.
- **FR-005**: With the flag, the system MUST record exact row counts for these groups:
  - `events` by event name
  - `flow_run` by flow name and state type
  - `task_run` by the flow name of the parent flow run and state type
  - `flow_run_state` and `task_run_state` by state type
  - `log` by the flow name of the parent flow run and log level
  - `artifact` by artifact type
  - Test: an end-to-end run on a deployment seeded with known flow runs and events.
- **FR-006**: For each group, the system MUST record the oldest and newest timestamp from these columns:
  - `flow_run` and `task_run`: `start_time`, the column that the flush filters on
  - `events`: `occurred`
  - `log`, `flow_run_state` and `task_run_state`: `timestamp`
  - `artifact`: `created`
  - Test: the same seeded end-to-end run.
- **FR-007**: For `flow_run` and `task_run`, each group MUST also count its rows with no `start_time`. A flush never removes these rows.
  - Test: seed a flow run that is cancelled before it starts, then check the count.
- **FR-008**: Every query MUST run in a read-only transaction.
  - Test: a unit test on the command the tool builds.
- **FR-009**: The breakdown of each table MUST stop within 5 minutes, and PostgreSQL itself MUST enforce the limit. A table that reaches the limit MUST be recorded as failed with the reason `timed out after 300s`, and the other tables MUST still be collected. The always-collected storage queries MUST keep the existing 60-second limit for diagnostic commands, and PostgreSQL MUST enforce that limit as well.
  - Test: an end-to-end run with the limit lowered for the test. After the limit, `pg_stat_activity` shows that the query is no longer running.
- **FR-010**: When `task-manager-db` is not deployed, both collectors MUST be recorded as `skipped`, with a reason saying that the task manager may use an external PostgreSQL server.
  - Test: a unit test with a fake backend.
- **FR-011**: When `task-manager-db` is deployed but cannot be reached, rejects the login, or has no `psql` command, the collectors MUST be recorded as `failed` with a reason that names the cause, and the bundle MUST still be produced.
  - Test: unit tests with a fake backend, one for each cause.
- **FR-012**: A table on the breakdown list that does not exist in the deployed Prefect version MUST be recorded as absent, not as a failure.
  - Test: a unit test.
- **FR-013**: While the breakdown runs, the progress output MUST name each table as the tool starts on it.
  - Test: a unit test on the log output.
- **FR-014**: On Kubernetes with a CloudNativePG cluster, the queries MUST run on the primary pod.
  - Test: a unit test on pod selection. The existing primary-pod selection by the `cnpg.io/instanceRole` label already has tests.
- **FR-015**: The documentation MUST describe the two new bundle files in the "What a bundle contains" section, and MUST describe the new flag and environment variable in the flag reference. The description of the flag MUST say not to use it while the task manager is being upgraded.
  - Test: the changed pages pass Vale and rumdl, and a reviewer checks the warning.
- **FR-016**: The manifest MUST tell support why breakdown figures are missing: the breakdown collector's reason MUST name each table that failed and the cause, for example `log: timed out after 300s`. `storage-breakdown.json` MUST record, for each table on the breakdown list, whether it was collected, absent or failed, with the reason.
  - Test: a unit test with a fake backend where one table times out and one is absent.
- **FR-017**: No database password MUST appear in the bundle files, the manifest or the progress output, including in the diagnostics written when a query fails.
  - Test: a unit test where a query fails and the tool writes its failure diagnostics; the password does not appear in any staged file.

### Key Entities

- **`task-manager-db` service** (existing): the PostgreSQL database behind the task manager. The tool already reaches it to run `pg_dump` for backups ([`backup_taskmanager.go`](../../src/internal/app/backup_taskmanager.go)). No new service is added to the deployment contract.
- **Prefect tables** (existing, read only): `events`, `flow_run`, `task_run`, `flow_run_state`, `task_run_state`, `log`, `artifact`, and `flow` for flow names. The names come from [Prefect's ORM models](https://github.com/PrefectHQ/prefect/blob/main/src/prefect/server/database/orm_models.py).
- **Storage report** (`task-manager-db/storage.json`, new): the total size of the Prefect database, and one entry for each table with its sizes, row counts and vacuum times.
- **Storage breakdown** (`task-manager-db/storage-breakdown.json`, new): for each table on the breakdown list, whether it was collected, absent or failed, and, when collected, its groups with their row counts and oldest and newest timestamps.
- **Bundle and manifest** (existing): a new `task-manager-db/` directory sits next to `task-manager/`. The layout is the same on Docker and Kubernetes. The manifest's list of collectors gains two entries, `task-manager-db-storage` and `task-manager-db-storage-breakdown`, one for each file.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: For every support case about the disk use of the task manager database, support answers the four questions from the bundle alone and does not send the customer SQL queries to run.
- **SC-002**: Without the flag, the storage figures add no more than 10 seconds to a bundle run, whatever the size of the database.
- **SC-003**: With the flag, each table's breakdown finishes or stops within 5 minutes, so the breakdown of the seven tables adds no more than 40 minutes to a run.
- **SC-004**: In 100% of runs where `task-manager-db` is stopped, cannot be reached or rejects the login, the bundle is still produced, and the manifest gives the reason the figures are missing.
- **SC-005**: While the figures are collected, the task manager keeps scheduling and running flow runs, and no flow run fails because of the collection.

SC-002 and SC-004 can be tested on every change. SC-001 and SC-005 can only be checked on real support cases and deployments.

## Assumptions

- The table and column names are those of Prefect 3, as listed in [Prefect's ORM models](https://github.com/PrefectHQ/prefect/blob/main/src/prefect/server/database/orm_models.py). They were checked against Prefect 3.8.6.
- The database owner can read the statistics views and every Prefect table, because it owns them. The tool logs in as the database owner that the backup tool already uses, taken from `PREFECT_SERVER_DATABASE_CONNECTION_URL` or `PREFECT_API_DATABASE_CONNECTION_URL` ([`app_config.go`](../../src/internal/app/app_config.go)).
- The `task-manager-db` images used with Infrahub ship `psql`. FR-011 covers the case where one does not.
- The live and dead row counts in `storage.json` come from the statistics that PostgreSQL keeps for each table, so they are estimates. Counting every row would break SC-002 on a large database. The row counts in `storage-breakdown.json` are exact.
- The always-collected queries keep the existing 60-second limit for diagnostic commands ([ADR 0002](../../dev/adr/0002-non-fatal-timeout-bounded-collectors.md)), and PostgreSQL enforces it as well (FR-009).
- Adding collectors and files does not change the manifest version, because [ADR 0005](../../dev/adr/0005-uniform-bundle-and-manifest.md) bumps the version only for breaking changes.
- The change needs an `added` changelog fragment.
- The queries do not run in the Prefect server container: a collector that ran a large listing there caused the server to be killed for running out of memory ([`infrahub-collect.md`](../../dev/knowledge/infrahub-collect.md)).

## Out of Scope

- External PostgreSQL servers. Support for them will follow [#172](https://github.com/opsmill/infrahub-backup/pull/172) ([comment](https://github.com/opsmill/infrahub-backup/issues/181#issuecomment-6420264056)).
- Byte sizes for each group in the breakdown. Measuring them means reading every stored value, such as the parameters of every flow run.
- Free space on the database volume, and growth over time.
- Interpreting the figures or recommending actions inside the bundle.
- Running VACUUM or a flush from `infrahub-collect`.
- Storage figures for the Neo4j database.
- Deployments that run Prefect 2.x (see Clarifications).
