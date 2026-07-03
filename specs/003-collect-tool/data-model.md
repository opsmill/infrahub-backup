# Data Model: Infrahub Collect

**Feature**: 003-collect-tool | **Date**: 2026-07-03

All types live in `src/internal/app`. JSON field names below are normative (they appear in published docs); Go identifiers are indicative.

## BundleManifest

The collect-side sibling of `BackupMetadata`. Serialized as `bundle_information.json` at the bundle root (`bundle/bundle_information.json` inside the archive).

| Field (Go) | JSON tag | Type | Rules |
|------------|----------|------|-------|
| ManifestVersion | `manifest_version` | int | Const `manifestVersion = 2026070200` (date-serial, bump on breaking manifest change) |
| CollectID | `collect_id` | string | Timestamp `YYYYMMDD_HHMMSS` (UTC), same ID used in the archive filename |
| CreatedAt | `created_at` | string | RFC3339 UTC (`time.Now().UTC().Format(time.RFC3339)`) |
| ToolVersion | `tool_version` | string | `BuildRevision()` (existing) |
| InfrahubVersion | `infrahub_version` | string | From existing `getInfrahubVersion()`; empty string allowed when server unreachable (recorded as a collector failure, not a run failure) |
| Environment | `environment` | string | `docker` \| `kubernetes` (from detected backend `Name()`) |
| LogLines | `log_lines` | int | Effective per-container log cap for this run (FR-011) |
| Collectors | `collectors` | []CollectorResult | One entry per **attempted or skipped** collector; MUST account for every collector in the run plan (SC-005) |

**Validation rules**: manifest is written even when collectors fail; it is the last file staged before archiving so it reflects final outcomes. `Collectors` is never empty.

## CollectorResult

| Field (Go) | JSON tag | Type | Rules |
|------------|----------|------|-------|
| Name | `name` | string | Stable identifier, path-like for per-service collectors (e.g. `logs/infrahub-server`, `cache-status`, `benchmark`) |
| Status | `status` | string | `success` \| `failed` \| `skipped` (typed constants) |
| Reason | `reason,omitempty` | string | Required when status ≠ `success`; human-readable cause ("container not running", "not requested", "previous logs unavailable") |

**State transitions**: planned → running → exactly one of `success` / `failed` / `skipped`. A collector never aborts the run (FR-009); the orchestrator converts its returned error into `failed` + reason.

## Collector

Internal unit of collection (not serialized).

| Field | Type | Rules |
|-------|------|-------|
| Name | string | Manifest identifier (see CollectorResult.Name) |
| Run | `func(ctx *collectContext) error` | Writes files under the staging bundle dir; returns wrapped error on failure |
| Skip | optional precondition | Returns (skip bool, reason string) — e.g. benchmark not requested, optional service absent |

The run plan is an ordered slice built from `CollectOptions` + the canonical service list (`infrahub-server`, `task-worker`, `database`, `message-queue`, `cache`, `task-manager`, `task-manager-db`, `task-manager-background-svc` — the constitution's public deployment contract).

## CollectOptions

Input aggregate resolved from flags/env by the entry point and passed to `CollectBundle`.

| Field | Type | Default | Source |
|-------|------|---------|--------|
| OutputDir | string | `./infrahub_bundles` | `--output-dir` / `INFRAHUB_OUTPUT_DIR` |
| LogLines | int | 100000 | `--log-lines` / `INFRAHUB_LOG_LINES` |
| IncludeBackup | bool | false | `--include-backup` / `INFRAHUB_INCLUDE_BACKUP` |
| IncludeQueries | bool | false | `--include-queries` / `INFRAHUB_INCLUDE_QUERIES` |
| Benchmark | bool | false | `--benchmark` / `INFRAHUB_BENCHMARK` |

Environment/project selection reuses the existing `Configuration` fields (`DockerComposeProject`, `K8sNamespace`) — not duplicated here.

## Replica

Read-only descriptor returned by the backend replica-enumeration primitive (R3).

| Field | Type | Docker meaning | Kubernetes meaning |
|-------|------|----------------|--------------------|
| Service | string | Compose service name | Service name (label-selector resolved) |
| ID | string | Container ID | Pod name |
| Restarted | bool | always false | `restartCount > 0` on any container → previous logs are fetched |

## Support bundle (filesystem artifact)

- **Archive**: `<OutputDir>/support_bundle_<CollectID>.tar.gz`; all members under a top-level `bundle/` directory; layout identical across environments (FR-006). Normative tree in [contracts/bundle-layout.md](./contracts/bundle-layout.md).
- **Staging**: `os.MkdirTemp(OutputDir, ...)`, removed on success, failure, and interrupt.

## Relationships to existing entities

- `InfrahubOps` — orchestrator entry point hangs off it (`iops.CollectBundle(opts)`), reusing `ensureBackend`, `Exec*`, `CopyFrom`, `GetAllPods`, `getInfrahubVersion`, `infrahubInternalAddress`.
- `EnvironmentBackend` — extended with replica/log/metrics read-only primitives; collectors consume a narrow collect-side interface satisfied by both backends (and by the test fake).
- `BackupMetadata` / `CreateBackup` — referenced, unmodified, by the `--include-backup` collector; the backup remains a standard standalone artifact recorded in the manifest.
