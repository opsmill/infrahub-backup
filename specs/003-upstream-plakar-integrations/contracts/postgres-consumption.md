# Contract: consuming `integration-postgresql` (as-is)

The task-manager / Prefect PostgreSQL database is captured by the upstream `integration-postgresql` logical connector. We do **not** modify the integration.

## Source URI (backup)
```
postgres://<user>:<pass>@<task-manager-db-host>:5432/<prefect-db>
```
Options applied:
| Option | Value | Why |
|---|---|---|
| `database` | the single Prefect DB name | back up only the app DB, not the whole cluster |
| `compress` | `false` | let kloset content-defined chunking dedup |
| `ssl_mode` | deployment default | match the deployment |

Credentials come from auto-discovery (`PREFECT_SERVER_DATABASE_CONNECTION_URL`, 2.x fallback) injected into the runner, with operator override (FR-023).

## Snapshot layout produced
```
/manifest.json
/00000-globals.sql          # pg_dumpall --globals-only
/00001-<prefect-db>.dump    # pg_dump -Fc
```

## Destination URI (restore)
```
postgres://<user>:<pass>@<task-manager-db-host>:5432/<prefect-db>
```
Options:
| Option | When | Effect |
|---|---|---|
| `clean` / `recreate` | restoring over an existing DB | `pg_restore --clean --if-exists` / `-C --clean` |
| `no_owner` | target roles differ | `pg_restore --no-owner` |
| `no_globals` | DB-only restore | skip the globals file |

`.dump` → `pg_restore`; `00000-globals.sql` → `psql`.

## Runner requirements
- `pg_dump`, `pg_dumpall`, `pg_restore`, `psql` present in the runner image.
- Network reachability from the runner to `task-manager-db` (co-located on the deployment network).

## Tool responsibilities (not the integration's)
- Discover credentials and the Prefect DB name; inject into the runner.
- Apply grouping tags to the resulting snapshot (`infrahub.component=postgres`, shared `backup-id`).
- Decide single-component vs full-group restore (FR-024).
