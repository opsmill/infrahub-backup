# 8. Task manager database queries run read-only, with time limits that PostgreSQL enforces

**Status**: Accepted
**Date**: 2026-10-26
**Source**: specs/006-taskmgr-db-storage/research.md (R2 to R4, R10, R15), issue [#181](https://github.com/opsmill/infrahub-backup/issues/181)

## Context

`infrahub-collect` reports the storage use of the task manager database (PostgreSQL with the Prefect schema). It runs `psql` inside the `task-manager-db` container through the bounded `docker exec` or `kubectl exec` (ADR 0001, ADR 0002). Stopping the exec does not stop a query on the server: when the `docker exec` client was stopped after 3 seconds, `pg_stat_activity` still showed the query as active. A query that keeps running after the tool stops waiting still loads the database, and a schema change during a task manager upgrade waits behind it.

Support asks for these figures most often when the database volume is nearly full. The breakdown reads every row of the largest tables and joins `flow_run`, `task_run` and `log` to `flow`, the last two through `flow_run`. With default settings, PostgreSQL runs that join as a parallel hash join, and once the hash table no longer fits in `work_mem`, it writes temporary files to the database volume. A full volume stops PostgreSQL, and the task manager then stops scheduling flow runs. `temp_file_limit` would cap those files, but only a superuser can set it, and the tool connects as the database owner.

## Decision

Every query goes through one runner, `runTaskManagerDBQuery`, which runs `psql -h localhost -U <owner> -d <database> -X -q -A -t -v ON_ERROR_STOP=1 -c <query>` inside `task-manager-db`, through a short `sh` script that first reads the password from stdin (see below). The runner sets the session parameters in the `PGOPTIONS` environment variable, so PostgreSQL applies them to the session:

- `default_transaction_read_only=on`: every statement runs in a read-only transaction, without `BEGIN` and `COMMIT` around the query. The tool builds all SQL itself and never sends `SET TRANSACTION READ WRITE`.
- `statement_timeout`: 60 seconds (`collectExecTimeout`) for the storage query and the breakdown's catalog query, and 5 minutes (`collectDBQueryTimeout`) for each breakdown table. PostgreSQL cancels the query at the limit, also when the tool has stopped waiting for it.
- `TimeZone=UTC`, so every timestamp in the query results is in UTC.
- For the breakdown queries only, `enable_hashjoin=off`, `enable_mergejoin=off` and `max_parallel_workers_per_gather=0`. Each join then looks up `flow_run` and `flow` rows through their primary keys in a nested loop, which does not write temporary files. The grouping produces few groups (flow names by state types or log levels), so the aggregate stays in memory, and the query runs in one process.

`PGAPPNAME=infrahub-collect` names the tool's sessions in `pg_stat_activity`, and `PGCONNECT_TIMEOUT=10` makes an unreachable server fail within 10 seconds. Every parameter in `PGOPTIONS` must be one that any user can set, because PostgreSQL rejects the whole connection when the login cannot set one. `dbSessionParameterAllowlist` lists the allowed names, and a unit test fails when `buildPsqlCommand` sets a name that is not on it.

The tool also bounds each exec at the PostgreSQL limit plus 15 seconds (`collectDBQueryGrace`). The extra time lets PostgreSQL stop the query and report it first, and the tool's bound still stops a `docker` or `kubectl` call that does not return. Whichever side stops the query, the manifest reason is `timed out after <PostgreSQL limit>`, in the format of ADR 0002.

The password goes on stdin, never in an argument or in a variable that `docker` or `kubectl` passes as an argument. The exec runs `sh -c 'IFS= read -r PGPASSWORD; export PGPASSWORD; exec psql "$@"'` with the `psql` arguments, and the runner writes the password to the first line of stdin through `ExecSeparateInputContext` (`docker compose exec` forwards stdin, and `kubectl exec -i` does too). The password is then in the environment of `psql` only, which other users of the operator's host cannot read. `sh` reports a missing `psql` as `psql: not found` with exit code 127, in dash, bash and busybox.

The runner also adds value-based masking (`passwordMask`, `maskSecretValue`) to the key-name masking of ADR 0004, in case an error message quotes the password. It replaces the password with `********` in the recorded command, in stderr and in the error text. It masks stdout only when the query fails, because that stdout then goes to `<file>.err.txt`. A successful query's stdout is the result that the collectors parse, and masking it would corrupt the result when the password is a common word, such as part of event names like `prefect.flow-run.Completed`. A password equal to the user or the database name is not masked: the name is in the command and in PostgreSQL's errors anyway, and masking it would only hide the name. This is the case for the default login, whose password `prefect` is also the database name, and for the Docker Compose login `postgres`/`postgres`.

## Consequences

- When a run is interrupted or the tool stops waiting for a query, PostgreSQL still stops the query at its limit, so no query of the tool runs longer than that in the task manager database.
- A statement that writes fails with `cannot execute ... in a read-only transaction`.
- The breakdown is slower but uses one process instead of three: on 2.25 million log records, the `log` breakdown took 1.0 seconds instead of 0.3 seconds. The 5-minute limit per table bounds the slower run.
- Reading rows can still make PostgreSQL write pages: a sequential scan sets hint bits on recently changed rows. This happens for any read and does not change data.
- A new session parameter must first be checked on a database owner without superuser rights, then added to `dbSessionParameterAllowlist`.
- The password is in no argument of the `docker`, `kubectl`, `sh` or `psql` process, so other users of the operator's host cannot view it in the process list, and a Kubernetes API server that logs exec request URIs does not record it. `infrahub-backup create` still passes it as `-e PGPASSWORD=<password>` or `env PGPASSWORD=<password>`.
- `task-manager-db` needs `sh`. The official `postgres`, Bitnami and CloudNativePG images include it.

## Alternatives Considered

- `BEGIN READ ONLY` and `SET LOCAL statement_timeout` as separate `-c` commands: rejected, because `psql` then prints extra command results and the query takes four round trips, for the same guarantee.
- `docker compose exec -e PGPASSWORD` without a value, which takes the value from the environment of the `docker` process: rejected, because `kubectl exec` has no equivalent, and stdin works on both backends.
- Only the tool's time limit: rejected, because stopping the exec does not stop the query on the server.
- Equal limits for PostgreSQL and the tool: rejected, because connecting takes time, so the tool's limit would often stop the exec before PostgreSQL's timeout error arrives.
- `temp_file_limit`: rejected, because only a superuser can set it, and a parameter that the login cannot set makes the connection fail.
- A larger `work_mem` for the session: rejected, because it moves the risk from the disk to the memory of the database pod.
- Grouping `task_run` and `log` by `flow_run_id` before joining: rejected, because the number of groups then equals the number of flow runs, and the aggregate writes temporary files in the same way.
