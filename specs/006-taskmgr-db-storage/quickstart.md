# Quickstart Validation: Storage Figures for the Task Manager Database

**Feature**: [spec.md](spec.md) | Contracts: [cli.md](contracts/cli.md), [manifest-outcomes.md](contracts/manifest-outcomes.md), [storage.schema.json](contracts/storage.schema.json), [storage-breakdown.schema.json](contracts/storage-breakdown.schema.json)

These scenarios show that the feature works from start to end. They reference the contracts instead of repeating them.

## Prerequisites

- The Go toolchain, used through `make` (the Makefile adds the build tags that the local toolchain needs).
- In a fresh worktree, run `make build-watchdog` once, or the unit tests of `src/internal/app` fail on a missing embedded file.
- Docker with Docker Compose, for scenarios 2 to 5.
- kind, kubectl and helm, for scenario 6.
- `uv` and pytest for the end-to-end suites in `tests/e2e/`.
- `jq`, to read the bundle files.

## 1. Build and quality gates

```bash
make build
make test
make vet
PATH="$(go env GOPATH)/bin:$PATH" make lint
```

Expected: the three binaries build, and every gate passes. The new unit tests cover FR-002 to FR-005, FR-008 to FR-014, FR-016 and FR-017 (see [research.md](research.md), R14).

## 2. Storage figures in a default bundle (User Story 1, FR-001 to FR-003)

On a Docker Compose deployment of Infrahub:

```bash
bin/infrahub-collect --project <project> --output-dir ./bundles create
tar -xzf bundles/support_bundle_*.tar.gz -C ./out
jq '.database_size, (.tables[:3] | map({name, total_size, toast_size, dead_rows}))' out/bundle/task-manager-db/storage.json
jq '.collectors[] | select(.name | startswith("task-manager-db"))' out/bundle/bundle_information.json
```

Expected:

- `storage.json` matches [storage.schema.json](contracts/storage.schema.json), and for every table, data, index and TOAST sizes add up to the total.
- `database_size.bytes` equals the value PostgreSQL reports:

  ```bash
  docker compose -p <project> exec task-manager-db psql -U <owner> -d <database> -Atc "SELECT pg_database_size(current_database())"
  ```

  The two values can differ by a few pages if the task manager writes between the two queries.
- The manifest records `task-manager-db-storage` as `success` and `task-manager-db-storage-breakdown` as `skipped` with the reason `not requested` (FR-004).
- The run takes no more than 10 seconds longer than before the change (SC-002). Compare the time between the `Collecting task-manager-db-storage` line and the next collector's line.

## 3. Storage breakdown with the flag (User Story 2, FR-005 to FR-007, FR-013)

Seed a flow run that is cancelled before it starts, through the Prefect client in a `task-worker` container. The end-to-end test does this in a fixture; the commands below are for a manual check:

```bash
bin/infrahub-collect --project <project> --output-dir ./bundles create --include-task-manager-storage
INFRAHUB_INCLUDE_TASK_MANAGER_STORAGE=true bin/infrahub-collect --project <project> --output-dir ./bundles create
jq '.tables[] | {name, status, rows, groups: (.groups // [])[:3]}' out/bundle/task-manager-db/storage-breakdown.json
```

Expected:

- The progress output has one `Collecting storage breakdown of table <table>` line for each of the seven tables (FR-013).
- `storage-breakdown.json` matches [storage-breakdown.schema.json](contracts/storage-breakdown.schema.json). Every table is `collected`.
- The `flow_run` group of the seeded flow and the `CANCELLED` state type has `rows_without_start_time` equal to 1 (FR-007).
- For every collected table, `rows` equals the sum of its groups' `rows`, and equals `SELECT count(*)` on the table, run in the database right after the collection on an idle deployment.
- Both ways of requesting the breakdown, the flag and the environment variable, give the same outcome (FR-004).

## 4. The time limit that PostgreSQL enforces (FR-009, FR-016)

In one terminal, hold a lock that blocks the `artifact` breakdown:

```bash
docker compose -p <project> exec task-manager-db psql -U <owner> -d <database> \
  -c "BEGIN; LOCK TABLE artifact IN ACCESS EXCLUSIVE MODE; SELECT pg_sleep(120); COMMIT;"
```

In a second terminal:

```bash
INFRAHUB_TASK_MANAGER_DB_QUERY_TIMEOUT=5s bin/infrahub-collect --project <project> --output-dir ./bundles create --include-task-manager-storage
docker compose -p <project> exec task-manager-db psql -U <owner> -d <database> \
  -Atc "SELECT count(*) FROM pg_stat_activity WHERE application_name = 'infrahub-collect'"
```

Expected:

- `task-manager-db-storage` is `failed` with the reason `timed out after 5s`, because measuring the size of `artifact` waits for the same lock ([research.md](research.md), R5).
- `artifact` is `failed` with the reason `timed out after 5s`; the other six tables are `collected`.
- The manifest records `task-manager-db-storage-breakdown` as `failed` with the reason `partial storage breakdown: artifact: timed out after 5s`.
- The `pg_stat_activity` query prints `0`: PostgreSQL stopped the query.
- Setting the variable to `10m` keeps the 60-second and 5-minute limits: the variable can only lower them.

## 5. Failures leave a usable bundle (FR-010, FR-011, FR-017, SC-004)

| Setup | Expected outcome for both collectors |
|---|---|
| `docker compose -p <project> stop task-manager-db` | `failed`, `not reachable: ...` |
| `PREFECT_API_DATABASE_CONNECTION_URL=postgresql+asyncpg://nobody:wrong-secret@task-manager-db:5432/prefect` in the operator's environment | `failed`, `login rejected: ... role "nobody" does not exist` |
| `docker compose -p <project> stop task-manager task-manager-background-svc` (the database keeps running) | `success`: the owner is read from the `task-manager-db` environment |
| A deployment without a `task-manager-db` service | `skipped`, the reason names an external PostgreSQL server |

In each case, the command exits 0, the archive exists, and `grep -r <password> out/bundle` finds nothing. Start `task-manager-db` again after the first case.

The tool replaces every occurrence of the password with `********` (FR-017, [research.md](research.md) R10), so a password that also occurs in other text masks that text too. With the password `x`, the reason reads `role "nobody" does not e********ist`. When the password equals the user or database name, the masked diagnostics hide that name and `grep` finds the name in the logs. This happens on the end-to-end Compose deployment, where the user and the password are both `postgres`, and with the tool's default credentials, where the database and the password are both `prefect`. Use a password that differs from the user name, the database name and any word of the expected output.

## 6. Kubernetes, including CloudNativePG (FR-014)

```bash
uv run pytest -m k8s tests/e2e/test_k8s_collect.py
```

Expected: the bundle holds `task-manager-db/storage.json` and the manifest records `task-manager-db-storage` as `success`.

The CI cluster has no CloudNativePG. To check primary-pod selection by hand, run Prefect on a CloudNativePG cluster with two instances in a local kind cluster, then run `infrahub-collect create`. `storage.json` has non-null `last_autovacuum` values for active tables, which a standby cannot produce, and the tool's `kubectl exec` call targets the pod labeled `cnpg.io/instanceRole=primary`. To view the `kubectl` calls, put a wrapper `kubectl` on the binary's `PATH` that logs its arguments.

## 7. End-to-end suites

```bash
uv run pytest -m docker tests/e2e/test_docker_collect.py
uv run pytest -m k8s tests/e2e/test_k8s_collect.py
```

Expected: both suites pass, including the new storage and breakdown tests, and the updated expected collector set in `tests/helpers/bundle.py`.
