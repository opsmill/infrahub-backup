# Phase 0 Research: Infrahub Collect

**Feature**: 003-collect-tool | **Date**: 2026-07-03

All unknowns from Technical Context were resolved by studying the existing codebase (backends, executor, backup pipeline, build surface), the published user docs (which define the normative CLI/bundle contract), and the spec's parity list. No external research tasks remained open.

## R1. Kubernetes and Docker interaction: shell out to CLIs, no client libraries

- **Decision**: All Docker and Kubernetes operations shell out to the `docker` and `kubectl` binaries through the existing `CommandExecutor`, exactly like the current backends. No client-go, no Docker SDK.
- **Rationale**: Consistency with the entire existing codebase (`environment_docker.go`, `environment_kubernetes.go`); zero new dependencies keeps `CGO_ENABLED=0` cross-compilation trivial and avoids a vendor-hash churn; kubectl handles kubeconfig/auth plugins (OIDC, cloud IAM) for free, which client-go would force us to reimplement.
- **Alternatives considered**: `k8s.io/client-go` (rejected: large dependency tree, auth-plugin complexity, inconsistent with existing backends); Docker Engine API via socket (rejected: breaks remote-context workflows that the docker CLI handles transparently).

## R2. Collector framework: named units with independent, non-fatal outcomes

- **Decision**: A `collector` is a named unit (`name`, `run(bundleDir) error`, optional skip precondition). The orchestrator (`CollectBundle`) iterates a fixed, ordered collector list; each failure is caught, logged as a warning, recorded in the manifest as `failed` with a reason, and the loop continues. Collectors that don't apply (benchmark not requested, optional service absent) record `skipped` with a reason. The command exits 0 whenever the archive is produced, even with failures (FR-009).
- **Rationale**: Directly encodes FR-007/FR-009/SC-005 (manifest accounts for 100% of attempted collectors with explicit outcomes). Sequential execution keeps output streaming readable (docs show sequential progress lines) and avoids hammering a degraded instance; per-collector wall time is dominated by log download, well within SC-001's 5-minute budget.
- **Alternatives considered**: Parallel collector execution (rejected for v1: interleaved streaming output, higher load on degraded instances, no evidence the 5-minute budget needs it); error-out on first failure (rejected: contradicts FR-009 — a degraded instance is the primary use case).

## R3. Log collection: extend both backends with read-only replica/log primitives

- **Decision**: Extend the backends with net-new read-only methods, surfaced to collectors via a narrow collect-side interface (defined next to the collectors, satisfied by both backends):
  - `ServiceReplicas(service) ([]Replica, error)` — Docker: `docker compose -p <proj> ps -q <service>` → one `Replica` per container ID; Kubernetes: existing `GetAllPods` + `kubectl get pod -o jsonpath` restart counts → one `Replica` per pod with `Restarted bool`.
  - `ReplicaLogs(replica, tailLines, previous) (io.ReadCloser, wait, error)` — Docker: `docker logs --tail <n> <container-id>` (previous unsupported → never requested); Kubernetes: `kubectl logs -n <ns> <pod> --tail=<n> [--previous]`. Built on the existing `runCommandPipe` streaming primitive so large logs stream to disk without buffering in memory.
- **Rationale**: `GetAllPods` (multi-replica) and `runCommandPipe` (streaming) already exist; only the log commands are new. Per-replica files match the promised bundle layout (`logs/<service>/` one file per replica, `*.previous.log` for restarted pods). Fetching previous logs only when `restartCount > 0` avoids a guaranteed-failing kubectl call per healthy pod.
- **Alternatives considered**: `docker compose logs <service>` (rejected: interleaves replicas into one stream and prefixes lines, breaking per-replica files and parity with k8s layout); always attempting `--previous` and swallowing errors (rejected: noisy, slower, indistinguishable from real failures in the manifest).

## R4. Container metrics: one-shot stats appropriate to each environment

- **Decision**: Docker: `docker stats --no-stream <container-ids...>` for the project's containers; Kubernetes: `kubectl top pods -n <namespace>`. Output captured as plain text into `metrics/`. On Kubernetes without metrics-server, the collector records `failed` with the kubectl error as reason — non-fatal.
- **Rationale**: Matches FR-005 (container stats on Docker, pod resource metrics on Kubernetes; no host metrics) and the docs' collectors table (`docker compose stats` / `kubectl top`). metrics-server absence is a routine cluster condition → belongs in the manifest, not a run failure.
- **Alternatives considered**: cAdvisor/Prometheus scraping (rejected: assumes infrastructure that may not exist, violates offline-by-default simplicity); skipping metrics on k8s without metrics-server silently (rejected: FR-007 requires explicit outcomes).

## R5. Secret masking: pure key-name matcher applied to env and config dumps

- **Decision**: A pure function in `masking.go`: case-insensitive substring match on key names — `password`, `secret`, `token`, `key` — replaces the value with `********`. Applied line-wise to `KEY=VALUE` env output and to key/value configuration dumps (e.g. Redis `CONFIG GET`) before writing them into the bundle. Logs are written as-is (parity with today's tool; documented in the docs warning).
- **Rationale**: Exact parity with FR-008 and the published docs. A pure function is trivially table-testable (Principle IV names configuration logic as a unit-test target).
- **Alternatives considered**: Pattern-based scrubbing of secret *values* inside logs (explicitly out of scope for v1 per spec assumptions); allowlist approach (rejected: inverts the current tool's behavior, breaking parity).

## R6. Bundle manifest: sibling of BackupMetadata, normative field set from docs

- **Decision**: `BundleManifest` struct in `collect_manifest.go` with JSON tags exactly matching the published example: `manifest_version` (const `2026070200`), `collect_id` (timestamp `YYYYMMDD_HHMMSS`), `created_at` (RFC3339 UTC), `tool_version` (`BuildRevision()`), `infrahub_version` (existing `getInfrahubVersion()`), `environment` (`docker`|`kubernetes`), `log_lines` (effective cap), `collectors` (array of `{name, status, reason?}`, status ∈ `success|failed|skipped`). Written as `bundle_information.json` via `json.MarshalIndent` at the bundle root.
- **Rationale**: Mirrors the proven `BackupMetadata` pattern (`metadataVersion` date-serial const, `MarshalIndent`, `os.WriteFile`) and the docs' manifest example verbatim, which is the user-facing contract.
- **Alternatives considered**: Reusing/extending `BackupMetadata` (rejected: different lifecycle and fields; spec names the manifest as a *sibling*, not an extension).

## R7. Archive packaging and interrupt safety

- **Decision**: Stage files in a temporary working directory created inside the output directory (`os.MkdirTemp(outputDir, ...)`), lay files out under `<workDir>/bundle/...`, then reuse the existing `createTarball(archivePath, workDir, "bundle/")` and remove the staging dir. Cleanup runs via `defer`; a signal handler (SIGINT/SIGTERM) also removes the staging dir so interrupts leave no temp files (edge case "Interrupted collection"). Since collection never stops workloads, no start/restart recovery is needed.
- **Rationale**: `createTarball` is exactly the mechanism backups use (`workDir` + path-in-tar prefix); staging inside the output dir keeps the final rename/write on one filesystem and confines all writes to the user-chosen location.
- **Alternatives considered**: Streaming tar writer without a staging dir (rejected: prevents retro-writing the manifest, which must be finalized *after* all collectors report); staging in `/tmp` (rejected: cross-device writes, files leak outside the user-designated directory).

## R8. Server info collection: reuse internal-address plumbing and exec

- **Decision**: The `server` collector execs inside `infrahub-server`: Infrahub version (existing `getInfrahubVersion`), installed packages (`pip list`), environment variables (`env`, masked per R5), and API information/configuration/schema fetched from inside the container against the existing `infrahubInternalAddress` (Python one-liners / an embedded script via the existing `script_executor.go` + `go:embed` pattern, since the server image ships Python but not necessarily curl).
- **Rationale**: `infrahubInternalAddress` and the embedded-script execution pipeline already exist and are constitution-compliant (Principle III: assets via `go:embed`). Exec-from-inside avoids any assumption about external API exposure — collection stays workable when the API is not reachable from the operator's workstation.
- **Alternatives considered**: Calling the Infrahub API from the collect binary over the network (rejected: requires reachable ingress + credentials; violates "no network access beyond the deployment" simplicity since exec transport already exists).

## R9. Parity diagnostics: fixed exec command sets per service

- **Decision**: Exec-based collectors issue the parity commands inside the official containers, targeting the CNPG/HA primary automatically through the existing `getPodForService`/`findPrimaryPod` behavior:
  - `database` (Neo4j): copy `neo4j.log` and `debug.log` via existing `CopyFrom`; with `--include-queries`, copy the full log directory including query logs (FR-004).
  - `message-queue` (RabbitMQ): `rabbitmqctl list_queues / list_exchanges / list_bindings / list_connections / list_channels / status / environment`.
  - `cache` (Redis): `redis-cli info / client list / config get '*' (masked) / slowlog get / dbsize`.
  - `task-worker`: Prefect worker CLI output, one directory per replica (via R3 replica enumeration).
  - `task-manager`: Prefect work pools, work queues, recent flow runs, events, automations via Prefect CLI/API inside the container (embedded-script pattern where CLI output is insufficient).
- **Rationale**: This is the FR-003 parity list; the spec's assumption confirms these CLIs exist inside the official images. Absent optional services (`task-manager-background-svc`) record `skipped` (edge case).
- **Alternatives considered**: Talking to RabbitMQ/Redis/Prefect over the network from the workstation (rejected: ports are typically not exposed; exec transport is the established pattern).

## R10. `--include-backup`: delegate to CreateBackup unmodified

- **Decision**: When `--include-backup` is set, the orchestrator invokes the existing `CreateBackup` entry point with collect-appropriate defaults (non-interactive, no S3 upload, no redaction/encryption), records the produced backup's identity/path in the manifest, and records `failed` (with reason) while still producing the bundle if backup errors (FR-014, US3 acceptance scenario 2). The backup artifact stays a separate file next to the bundle — it is referenced by, not embedded in, the archive.
- **Rationale**: FR-014 says "reuses the existing backup behavior unmodified"; embedding a multi-GB backup inside the bundle tarball would double disk usage and break the backup tool's own restore discovery. The docs' warning that `--include-backup` may stop containers documents the inherited behavior.
- **Alternatives considered**: Embedding the backup archive inside the bundle (rejected: disk-space doubling, restore tooling expects the standard backup location/naming); re-implementing a lighter read-only backup (rejected: violates "unmodified" and Principle II's guarantees).

## R11. `--benchmark`: opt-in, image-based, graceful degradation

- **Decision**: Default off (FR-013). When requested, the collector runs the OpsMill benchmark container against the deployment (Docker: `docker run` attached to the project network; Kubernetes: a one-off `kubectl run` pod in the namespace), captures its output into `benchmark/`, and removes the transient container/pod it created (which is permitted: FR-010 protects *the deployment's* workloads; the benchmark pod is the tool's own transient resource, and `--benchmark` explicitly opts into image download). The image reference is a build-time default overridable via `INFRAHUB_BENCHMARK_IMAGE`; the concrete default is ported from the Python tool's benchmark step during implementation. Pull or run failure → collector `skipped` with a warning in the manifest (air-gapped edge case).
- **Rationale**: Matches FR-013 and the docs (`--benchmark` requires image download, skipped with a warning when unavailable). Override env var keeps air-gapped-with-private-registry workflows possible.
- **Alternatives considered**: Embedding a Go-native benchmark (rejected: duplicates the maintained benchmark suite); making benchmark default-on like the Python tool (rejected: spec decision — offline-by-default won).

## R12. CLI wiring: flags, env vars, and shared commands

- **Decision**: `src/cmd/infrahub-collect/main.go` mirrors the taskmanager entry point: `app.SetVersion`, `NewInfrahubOps`, `ConfigureRootCommand` (brings `--project`, `--k8s-namespace`, `--log-format` + `INFRAHUB_*` env binding), `AttachEnvironmentCommands` (brings `environment detect|list`), a `version` command, and a `create` command. Collect-specific flags are registered in main.go and bound to viper: `--output-dir` (persistent, default `./infrahub_bundles`, `INFRAHUB_OUTPUT_DIR`), and on `create`: `--log-lines` (default 100000, `INFRAHUB_LOG_LINES`), `--include-backup`, `--include-queries`, `--benchmark` (each with its `INFRAHUB_*` equivalent per the docs' reference tables). Precedence flag > env > default comes from viper as today.
- **Rationale**: Exactly the published commands reference; flag registration in entry points is the established, constitution-sanctioned pattern (backup's main.go does the same). `--output-dir` is collect-specific, so it does not belong in the shared `ConfigureRootCommand` (backup already has its own `--backup-dir`).
- **Alternatives considered**: Adding `--output-dir` to `ConfigureRootCommand` (rejected: would surface a meaningless flag on the other two binaries).

## R13. Testing strategy

- **Decision**: Unit tests (Go, table-driven, no mocking framework — repo convention): masking matrix, manifest construction/serialization against the documented JSON shape, collector outcome recording (success/failed/skipped paths) using a fake collect-backend, log-command argument construction, replica parsing. E2E (pytest, existing markers/fixtures): `test_docker_collect.py` — full collect against the compose stack, archive integrity, manifest completeness, per-service logs present, masked env contains no plaintext secrets, degraded case (one service stopped → exit 0 + `failed` entry); `test_k8s_collect.py` — same against the kind+Helm stack plus multi-replica and previous-log assertions. New `collect_binary` fixture and `run_collect` helper mirror the backup ones.
- **Rationale**: Principle IV mandates table-driven unit tests for pure logic and e2e jobs for container-touching flows; the fake-backend seam exists naturally because collectors consume a narrow interface (R3).
- **Alternatives considered**: Introducing a mocking framework for `CommandExecutor` (rejected: against repo convention; the interface seam gives the same testability).

## R14. Build/release surface

- **Decision**: Add `infrahub-collect` to `BINARIES` in the Makefile (wires `build`, `build-all`, `install`); add a third `buildGoModule` package and symlinkJoin entry in `flake.nix`; `release.yml` uploads `./bin` wholesale so the new binary ships automatically at the documented URL pattern. Dockerfile and the Docker/Homebrew release jobs stay `infrahub-backup`-only (no collect image or formula is promised). Update AGENTS.md (and its CLAUDE.md routing) from "two specialized CLI binaries" to three — the constitution's Sync Impact Report flags this as a mandatory companion change.
- **Rationale**: Minimal, mechanical, matches the install docs (S3 download URL + `make build`). `go.mod` is untouched, so `vendorHash` stays as-is.
- **Alternatives considered**: Multi-binary Docker image (rejected for v1: not in docs, expands release surface without a user story).
