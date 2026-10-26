# CloudNativePG primary-pod check (T046)

Manual check of [quickstart.md](quickstart.md) scenario 6 and [FR-014](spec.md): on Kubernetes with a CloudNativePG cluster, the task manager database queries run on the primary pod. Run on 2026-10-26 with `bin/infrahub-collect` built by `make build` from commit e93ff50.

## Setup

- kind v0.31.0 cluster `d8y7s-cnpg` (Kubernetes v1.35.0) with its own kubeconfig.
- CloudNativePG Helm chart `cloudnative-pg-0.29.1` (operator 1.30.1).
- CloudNativePG Cluster `taskmanagerdb` with 2 instances, image `ghcr.io/cloudnative-pg/postgresql:18.6-system-trixie`, database and owner `prefect`, and the label `infrahub/service: task-manager-db` on its pods through `inheritedMetadata`, as in the Infrahub high availability example ([`development/k8s/taskmanagerdb.yaml`](https://github.com/opsmill/infrahub/tree/stable/development/k8s)).
- Infrahub Helm chart `infrahub-4.35.0` (Infrahub 1.11.9) with `tests/e2e/fixtures/helm/infrahub-values.yaml`, the storage class `standard`, `prefect-server.postgresql.enabled: false`, and `prefect-server.secret` pointing at `taskmanagerdb-rw`, database `prefect`. The `prefect-server` pod connected through `PREFECT_SERVER_DATABASE_CONNECTION_URL`, and the `prefect` database held 36 tables.
- The binary ran with a `PATH` that held only a `kubectl` wrapper, which logged every call, so the tool used the Kubernetes backend:
  `infrahub-collect --k8s-namespace infrahub --output-dir <dir> create [--include-task-manager-storage]`.
- Between runs b and c, the primary was switched from `taskmanagerdb-1` to `taskmanagerdb-2` by setting `status.targetPrimary`, which is what `kubectl cnpg promote` does. This checks that the tool picks the primary and not the first pod in the list.

## Results

| Run (UTC) | Flag | Primary pod (`cnpg.io/instanceRole=primary`) | Pod of every `psql` call | `task-manager-db-storage` | `task-manager-db-storage-breakdown` |
|---|---|---|---|---|---|
| a, 19:36:49 | none | `taskmanagerdb-1` | `taskmanagerdb-1` (1 call) | `success` | `skipped`, `not requested` |
| b, 19:37:07 | `--include-task-manager-storage` | `taskmanagerdb-1` | `taskmanagerdb-1` (9 calls) | `success` | `success`, 7 of 7 tables `collected` |
| c, 19:38:21 | `--include-task-manager-storage` | `taskmanagerdb-2` | `taskmanagerdb-2` (9 calls) | `success` | `success`, 7 of 7 tables `collected` |
| d, 19:38:38, merged binary (see below) | `--include-task-manager-storage` | `taskmanagerdb-2` | `taskmanagerdb-2` (9 calls) | `success` | `success`, 7 of 7 tables `collected` |

- Every run exited 0. The tool listed the pods labeled `infrahub/service=task-manager-db`, read the `cnpg.io/instanceRole` label of each pod in list order until one was `primary`, and ran `psql` in that pod. In run c, it read `taskmanagerdb-1` (`replica`), then `taskmanagerdb-2` (`primary`).
- `storage.json` names the database `prefect`, the CloudNativePG database, and lists 36 tables. `storage-breakdown.json` has the time limit `300s` per table.
- Credentials: the tool read them from `env` in the `task-manager` pod (`prefect-server-...`). It ran no `env` in a `task-manager-db` pod, and the queries logged in as `prefect`.
- The database password, a random value chosen for this check, is in no bundle file and not in the progress output of any run.
- Tables with a non-null `last_autovacuum`: 1 in run a and 5 in run b with `taskmanagerdb-1` as primary; 0 in run c, 17 seconds after the switchover, because the new primary had not run autovacuum yet; 1 in run d. Right after a switchover, this quickstart check does not show which pod answered. The `kubectl exec` target does.

## Pod lookup for Neo4j on this branch

This branch does not contain the pod lookup changes on `main` (f673202, 1e0e522), which try `infrahub/service` first. In runs a to c, `logs/database` and `database-logs` copied `/logs/neo4j.log` from the CloudNativePG primary pod, because CloudNativePG labels its pods `app.kubernetes.io/component=database`. The manifest records them as `success`.

Run d used a binary built from the merge of this branch with `origin/main` (fa67557). It copied the Neo4j logs from `infrahub-database-0`, and it ran the `task-manager-db` queries on the primary pod, as runs a to c did. The merge has conflicts in `dev/knowledge/infrahub-collect.md` and `src/internal/app/environment_kubernetes_test.go`. The binary build does not read either file.
