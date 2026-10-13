<!-- Extracted from specs/003-collect-tool on 2026-07-09 -->

# Infrahub Collect

## Overview

`infrahub-collect` is the third CLI binary in this repo. It gathers a **troubleshooting bundle** — service logs, diagnostic status, configuration, and metrics — from a Docker Compose or Kubernetes Infrahub deployment into a single local archive for OpsMill support. It replaces the Python `invoke bundle collect` script and adds first-class Kubernetes support. Collection is strictly read-only (see [ADR 0003](../adr/0003-read-only-collection.md)).

Like the other binaries, the entry point (`src/cmd/infrahub-collect`) is thin Cobra wiring; all logic lives in `src/internal/app` (`collect*.go`, `masking.go`).

## CLI surface

```text
infrahub-collect [global-flags] <command>
  create                Collect a troubleshooting bundle
  environment detect    Detect the deployment environment (shared)
  environment list      List detected Docker projects / K8s namespaces (shared)
  version               Print the build version (shared)
```

Flags and their `INFRAHUB_*` environment equivalents:

| Flag | Default | Env | Scope |
|------|---------|-----|-------|
| `--project` | auto-detect | `INFRAHUB_PROJECT` | global (shared) |
| `--k8s-namespace` | auto-detect | `INFRAHUB_K8S_NAMESPACE` | global (shared) |
| `--output-dir` | `./infrahub_bundles` | `INFRAHUB_OUTPUT_DIR` | global (collect) |
| `--log-format` | `text` | `INFRAHUB_LOG_FORMAT` | global (shared) |
| `--log-lines` | `100000` | `INFRAHUB_LOG_LINES` | `create` |
| `--include-backup` | `false` | `INFRAHUB_INCLUDE_BACKUP` | `create` |
| `--include-queries` | `false` | `INFRAHUB_INCLUDE_QUERIES` | `create` |
| `--benchmark` | `false` | `INFRAHUB_BENCHMARK` | `create` |
| `--telemetry-days` | `30` | `INFRAHUB_TELEMETRY_DAYS` | `create` |

`create` exits 0 whenever the archive is produced — including a partial bundle with failed collectors. It exits 1 only for no usable environment, an unwritable output directory, or an archiving failure.

## Bundle layout

Output: `<output-dir>/support_bundle_<YYYYMMDD_HHMMSS>.tar.gz`, everything under a top-level `bundle/`, identical on Docker and Kubernetes:

```text
bundle/
├── bundle_information.json   # manifest (see below)
├── logs/<service>/           # one file per replica; *.previous.log for restarted k8s containers
├── database/                 # Neo4j neo4j.log, debug.log (+ full /logs with --include-queries)
├── message-queue/            # RabbitMQ queues/exchanges/bindings/connections/channels/status/environment
├── cache/                    # Redis info/clients/config(masked)/slowlog/dbsize
├── task-worker/<replica>/    # Prefect worker state per replica
├── task-manager/             # work pools, queues, flow runs (+ pending/running flow & task runs), events, automations
├── server/                   # version, pip list, API info/config/schema, env (masked)
├── telemetry/                # infrahubctl telemetry export (last --telemetry-days, default 30)
├── metrics/                  # docker stats / kubectl top
└── benchmark/                # only when --benchmark ran successfully
```

The task-manager collector captures its state through the Prefect CLI (`taskManagerDumps`), plus recent events via an embedded script (`collect_prefect_events.py` → `events.json`) because the CLI has no non-streaming events command. Both run in a `task-worker` where one is deployed, falling back to the `task-manager` container otherwise (`taskManagerQueryService`) — the bundle layout is the same either way, only the exec host moves. A worker is already an API client of the task manager (its `PREFECT_API_URL` points there, which is why `taskWorkerDumps`' unwrapped `prefect work-pool ls` works), so the listings spend a worker's memory rather than the Prefect server's. The server is the single point of failure for the whole task pipeline: a field bundle showed `prefect flow-run ls --limit 200 --output json` SIGKILLed inside the server container (exit code 137, the CLI materializes every recent flow run including its parameters), after which the next dump reported a stream error and the one after that `container not found` — the collector had taken the task manager down with it. Beyond the recent `flow-runs.json` (unfiltered, `--limit 200`), two extra dumps capture the **pending and running** work operators reach for when the task manager is stuck: `flow-runs-pending-running.json` and `task-runs-pending-running.txt`, each a `prefect <resource> ls --state-type PENDING --state-type RUNNING --limit 200`. Filtering explicitly matters because the unfiltered `flow-run ls` returns only the most recent runs regardless of state (so in-flight work is buried beyond the limit on a busy instance), and there is no unfiltered task-run listing at all. `--state-type` is stable across Prefect 2.x and 3.x.

Dump format follows what the CLI supports: only `flow-run ls` accepts `--output json`, so the flow-run dumps are JSON (`.json`) while `work-pool ls`, `work-queue ls`, `task-run ls`, and `automation ls` — which have no JSON option on the list command (only their `inspect` subcommands do) — stay plain text (`.txt`). `--output json` is a Prefect 3.x flag, so on an older Prefect the flow-run dumps produce a partial-failure reason and a `.err.txt` holding the CLI error rather than JSON.

### Dump files hold the payload, failures go to `.err.txt`

Two rules keep a dump file trustworthy, both learned from a field bundle whose `task-manager/*.json` files held prose instead of JSON:

- **Only stdout reaches the dump file.** Exec dumps capture stdout and stderr separately (`separateExecer` → `ExecSeparateContext`/`ExecReplicaSeparate`, backed by `runCommandSeparateContext`). Merged output let kubectl's `Defaulted container "x" out of: …` notice — emitted on every multi-container pod — prepend prose to every dump, silently making each `.json` unparseable even on a **successful** run. A successful command's stderr is logged at debug level and not staged.
- **A failed dump never writes the file it names.** Its diagnostics go to a sibling `<name>.err.txt` (`dumpFailurePath`, `writeDumpFailure`) holding the command, the error, and both streams — masked with the dump's own mask. So a present `flow-runs.json` always parses, and `flow-runs.err.txt` explains why the JSON is absent. This generalizes what `events.err.txt` (FIX-8) and `telemetry-export.err.txt` already did; it applies to every exec dump, including the per-replica `task-worker/` dumps and `server/`'s API documents.

The telemetry collector runs `infrahubctl telemetry export --start-date <now-N days>` inside `task-worker` (the container whose infrahubctl already targets the API, per `infrahub-backup`'s `infrahubctl task list`), then copies the JSON out to `telemetry/telemetry-export.json`. It is read-only — the export pulls stored snapshots through the API — and always-on; a non-positive `--telemetry-days` falls back to 30. Telemetry is anonymous product-usage data (no PII/secrets), so the export is staged unmasked. Outcomes: `task-worker` not deployed → `skipped`; a failing export (no snapshots in the window, an Infrahub version without the telemetry API, a timeout) degrades to `skipped` — like `--benchmark` — with the CLI output preserved in `telemetry/telemetry-export.err.txt`; only a local staging error (creating the directory, copying the file out) is `failed`. `infrahubctl telemetry export` exits non-zero and writes no file when there are no snapshots, which is why a missing export is treated as skipped rather than failed.

## Manifest (`bundle_information.json`)

The collect-side sibling of `BackupMetadata`. Contract: `specs/archive/003-collect-tool/contracts/manifest.schema.json`. Fields: `manifest_version` (date-serial), `collect_id`, `created_at` (RFC3339 UTC), `tool_version`, `infrahub_version`, `environment` (`docker`|`kubernetes`), `log_lines`, and `collectors[]` of `{name, status, reason?, artifact?}` where `status` ∈ `success|failed|skipped`. The manifest accounts for every planned collector and is written last so it reflects final outcomes. See [ADR 0005](../adr/0005-uniform-bundle-and-manifest.md).

## The collect backend seam

Collectors consume a narrow interface on top of the shared `EnvironmentBackend`, satisfied by both `DockerBackend` and `KubernetesBackend` (compile-time `var _ collectBackend` assertions) and by a test fake:

- `ServiceReplicas(service) ([]Replica, error)` — one `Replica` per running unit. On Docker, one per container (by container **name**); on Kubernetes, one per **pod container** (multi-container pods such as CNPG or sidecar-bearing pods yield several), each carrying its own `restartCount`. A genuine "no pods matched" is an empty, non-error result (→ `skipped`); a kubectl execution failure is a real error (→ `failed`).
- `ReplicaLogs(replica, tailLines, previous)` — streams a replica's logs; `previous` fetches the prior container's logs (Kubernetes only, and only when that container restarted).
- `Metrics()` — one-shot `docker stats --no-stream` / `kubectl top pods`.

Every subprocess on the collect path is timeout-bounded (see [ADR 0002](../adr/0002-non-fatal-timeout-bounded-collectors.md)); shared unbounded helpers used by backup keep collect-only bounded variants (`CopyFromContext`, `getPodForServiceContext`).

## Gotchas

- **Docker services log to stderr.** `docker logs` demuxes a container's output onto the matching process streams, and Infrahub services write to stderr. Capturing stdout alone yields near-empty logs, so the Docker log path uses a **combined** stdout+stderr pipe (`runCommandCombinedPipeContext`). The Kubernetes `kubectl logs` path needs no such merge.
- **A stopped-but-deployed Docker service** still has its logs collected and is recorded `failed` (not `skipped`); `skipped` is reserved for a service that is not deployed at all.
<!-- Extracted from specs/007-external-database-backup on 2026-09-07 -->
- **`--include-backup` refuses a database that lives outside the deployment.** Capturing an external Neo4j or PostgreSQL needs a transient pod and a credential Secret created in the namespace, and collection creates no workload ([ADR 0003](../adr/0003-read-only-collection.md), [ADR 0008](../adr/0008-transient-workload-for-external-databases.md)). The refusal names `infrahub-backup create` as the tool that performs the capture; see [external-database-backup.md](external-database-backup.md).
