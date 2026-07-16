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

The task-manager collector captures its state through the Prefect CLI (`taskManagerDumps`, run inside the `task-manager` container), plus recent events via an embedded script (`collect_prefect_events.py` → `events.json`) because the CLI has no non-streaming events command. Beyond the recent `flow-runs.txt` (unfiltered, `--limit 200`), two extra dumps capture the **pending and running** work operators reach for when the task manager is stuck: `flow-runs-pending-running.txt` and `task-runs-pending-running.txt`, each a `prefect <resource> ls --state-type PENDING --state-type RUNNING --limit 200`. Filtering explicitly matters because the unfiltered `flow-run ls` returns only the most recent runs regardless of state (so in-flight work is buried beyond the limit on a busy instance), and there is no unfiltered task-run listing at all. `--state-type` is stable across Prefect 2.x and 3.x. An events-script failure preserves its output in `events.err.txt` (never a bogus `events.json`, FIX-8) and marks the collector's partial failure.

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
