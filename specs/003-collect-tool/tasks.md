# Tasks: Infrahub Collect (Troubleshooting Bundle Tool)

**Input**: Design documents from `/specs/003-collect-tool/`

**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/ (cli.md, bundle-layout.md, manifest.schema.json), quickstart.md

**Tests**: Included — constitution Principle IV mandates table-driven unit tests for pure logic and e2e coverage for container-touching flows.

**Organization**: Tasks are grouped by user story. US1 (Kubernetes, P1) is the MVP; the environment-agnostic collector framework lands in Foundational, shared parity collectors land in US1 (earliest story that needs them) and run unchanged on Docker for US2.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel (different files, no dependencies on incomplete tasks)
- **[Story]**: US1 / US2 / US3 (user story phases only)

## Path Conventions

Single Go project: binaries in `src/cmd/<name>/main.go`, all logic in `src/internal/app/`, e2e in `tests/e2e/`. Design references: `specs/003-collect-tool/`.

---

## Phase 1: Setup (binary skeleton and build surface)

**Purpose**: A buildable third binary with the full CLI surface wired (create initially a stub), so every later phase is testable via `make build`.

- [X] T001 Create `src/cmd/infrahub-collect/main.go` mirroring `src/cmd/infrahub-taskmanager/main.go`: `app.SetVersion`, `app.NewInfrahubOps`, `app.ConfigureRootCommand`, `app.AttachEnvironmentCommands`, `version` command, and a `create` command (stub RunE) registering flags per `specs/003-collect-tool/contracts/cli.md` — persistent `--output-dir` (default `./infrahub_bundles`) and create-local `--log-lines` (default 100000), `--include-backup`, `--include-queries`, `--benchmark`, each viper-bound for `INFRAHUB_*` env equivalents
- [X] T002 [P] Add `infrahub-collect` to the `BINARIES` variable in `Makefile` (wires `build`, `build-all`, `install`)
- [X] T003 [P] Add an `infrahub-collect` `buildGoModule` package and symlinkJoin entry in `flake.nix` (no `vendorHash` change — `go.mod` untouched)
- [X] T004 Verify `make build` produces `bin/infrahub-collect` and that `--help`, `create --help`, `version`, `environment detect|list` match `specs/003-collect-tool/contracts/cli.md`

**Checkpoint**: `bin/infrahub-collect` builds; CLI surface matches the published contract.

---

## Phase 2: Foundational (collector framework — blocks all user stories)

**Purpose**: Environment-agnostic core: masking, manifest, timeout-bounded execution, orchestrator, and the backend seam collectors consume.

- [X] T005 [P] Implement `src/internal/app/masking.go`: pure key-name masking per research R5 — case-insensitive substring match on `password|secret|token|key` replaces values with `********`; helpers for `KEY=VALUE` env dumps and key/value config dumps (Redis `CONFIG GET`, `rabbitmqctl environment`/`status`)
- [X] T006 [P] Table-driven tests in `src/internal/app/masking_test.go` (match/no-match matrix, case-insensitivity, env-line and config-pair formats, empty values)
- [X] T007 [P] Implement `src/internal/app/collect_manifest.go`: `manifestVersion = 2026070200`, `BundleManifest` + `CollectorResult` + status constants per `specs/003-collect-tool/data-model.md`, constructor, and writer producing `bundle_information.json` via `json.MarshalIndent` (mirror `backup_metadata.go` conventions)
- [X] T008 [P] Table-driven tests in `src/internal/app/collect_manifest_test.go`: JSON field names/values validate against `specs/003-collect-tool/contracts/manifest.schema.json` semantics (collect_id pattern, status enum, reason required on failed/skipped)
- [X] T009 Define the collect backend seam in `src/internal/app/collect.go`: `Replica` struct (Service, Pod, Container, Restarted per data-model.md) and a narrow `collectBackend` interface (embeds `EnvironmentBackend`; adds `ServiceReplicas(service)`, `ReplicaLogs(replica, tailLines, previous)`, `Metrics()`) to be satisfied by both backends and a test fake
- [X] T010 Add context/timeout-bounded execution to `src/internal/app/command_executor.go` (`exec.CommandContext` variants of `runCommand`/`runCommandPipe`; research R2: 60s default for exec dumps, 5 min for log/copy transfers; timeout error distinguishable so the manifest reason reads `timed out after <duration>`)
- [X] T011 Implement the orchestrator `CollectBundle(opts CollectOptions)` in `src/internal/app/collect.go`: staging dir via `os.MkdirTemp(outputDir, ...)`, ordered collector loop with skip preconditions and non-fatal failures (WARN + manifest entry, FR-009), per-collector timeouts, per-collector INFO progress lines, manifest finalized last, archive via existing `createTarball(archivePath, workDir, "bundle/")`, staging cleanup on success/failure/SIGINT/SIGTERM, final log with archive path + size, hard error (exit 1 path) only for no-environment/unwritable-output/archive failure per contracts/cli.md
- [X] T012 Unit tests in `src/internal/app/collect_test.go` with a fake `collectBackend`: outcomes recorded for success/failed/skipped/timed-out collectors, run continues past failures, manifest accounts for every planned collector (SC-005), staging dir removed
- [X] T013 Wire the `create` command RunE in `src/cmd/infrahub-collect/main.go` to build `CollectOptions` from viper and call `iops.CollectBundle(opts)`

**Checkpoint**: Framework complete — collectors can be added independently; user story phases can begin.

---

## Phase 3: User Story 1 — Collect from a Kubernetes deployment (Priority: P1) 🎯 MVP

**Goal**: One command against a Helm-chart deployment yields a complete bundle: all-replica logs (with previous-container logs), parity diagnostics, server info, metrics, manifest.

**Independent Test**: Deploy Infrahub via the Helm chart into a test cluster, run `infrahub-collect create` with only a kubeconfig, verify per-service logs, diagnostics, server info, and manifest in the archive (spec US1 acceptance scenarios 1–4).

### Implementation for User Story 1

- [ ] T014 [US1] Implement `ServiceReplicas` on `KubernetesBackend` in `src/internal/app/environment_kubernetes.go`: existing `GetAllPods` + `kubectl get pod -o jsonpath` over `status.containerStatuses` → one `Replica` per pod **container** with per-container `restartCount` (research R3 / critique E2)
- [ ] T015 [US1] Implement `ReplicaLogs` on `KubernetesBackend`: `kubectl logs -n <ns> <pod> -c <container> --tail=<n> [--previous]`, streamed via the timeout-bounded pipe primitive (T010)
- [ ] T016 [P] [US1] Implement `Metrics` on `KubernetesBackend`: `kubectl top pods -n <namespace>`; missing metrics-server surfaces as a normal error (manifest `failed`, research R4)
- [ ] T017 [US1] Implement the service-log collector in `src/internal/app/collect_logs.go` (environment-agnostic via the T009 seam): iterate the canonical service list, per-replica files named per data-model.md (`<container-name>.log` / `<pod>.log` / `<pod>_<container>.log`), `.previous.log` when `Restarted`, per-service manifest entries `logs/<service>`, absent optional services → `skipped`
- [ ] T018 [P] [US1] Implement the database-log collector in `src/internal/app/collect_diagnostics.go`: `CopyFrom` Neo4j `neo4j.log` + `debug.log` into `bundle/database/`; with `--include-queries` copy the full log directory including query logs (FR-004)
- [ ] T019 [P] [US1] Implement the message-queue collector in `src/internal/app/collect_diagnostics.go`: `rabbitmqctl list_queues / list_exchanges / list_bindings / list_connections / list_channels / status / environment` into `bundle/message-queue/`, masking `environment` and `status` output (research R5)
- [ ] T020 [P] [US1] Implement the cache collector in `src/internal/app/collect_diagnostics.go`: `redis-cli info / client list / config get '*' (masked) / slowlog get / dbsize` into `bundle/cache/`
- [ ] T021 [P] [US1] Implement the task-worker collector in `src/internal/app/collect_diagnostics.go`: Prefect worker CLI output per replica into `bundle/task-worker/<replica>/` (uses T009 `ServiceReplicas`)
- [ ] T022 [P] [US1] Implement the task-manager collector in `src/internal/app/collect_diagnostics.go`: work pools, work queues, recent flow runs, events, automations via Prefect CLI (embedded script via `script_executor.go` + `go:embed` where CLI output is insufficient) into `bundle/task-manager/`
- [ ] T023 [P] [US1] Implement the server-info collector in `src/internal/app/collect_diagnostics.go`: existing `getInfrahubVersion`, `pip list`, masked `env` dump, API info/config/schema fetched inside `infrahub-server` against `infrahubInternalAddress` (research R8) into `bundle/server/`
- [ ] T024 [US1] Implement the metrics collector in `src/internal/app/collect_metrics.go` using the backend `Metrics` primitive, output into `bundle/metrics/`
- [ ] T025 [US1] Register the full ordered run plan in `CollectBundle` (`src/internal/app/collect.go`): logs per service → database → message-queue → cache → task-worker → task-manager → server → metrics (+ opt-in extras), populating `infrahub_version`, `environment`, `log_lines` in the manifest
- [ ] T026 [US1] Unit tests in `src/internal/app/collect_logs_test.go`: log filename derivation (docker/k8s single/multi-container, previous), kubectl/docker argument construction, replica parsing from jsonpath output
- [ ] T027 [US1] Add `collect_binary` session fixture in `tests/e2e/conftest.py` (mirrors `backup_binary`: `make build` → `bin/infrahub-collect`) and `run_collect` helper in `tests/helpers/utils.py`
- [ ] T028 [US1] E2E test `tests/e2e/test_k8s_collect.py` (`-m k8s`): full collect against the kind+Helm stack — archive integrity, manifest validates against `specs/003-collect-tool/contracts/manifest.schema.json`, one log file per replica (scale task-worker ≥ 2), `.previous.log` for an induced restart, layout per `specs/003-collect-tool/contracts/bundle-layout.md`, zero pod restarts/scale events caused by the run (SC-003)

**Checkpoint**: US1 fully functional — Kubernetes bundle collection works end-to-end (MVP).

---

## Phase 4: User Story 2 — Collect from Docker Compose without the repo (Priority: P2)

**Goal**: The same binary and command produce a parity bundle on Docker Compose; the shared collectors from US1 run unchanged through the Docker backend.

**Independent Test**: Start an Infrahub compose project, run `infrahub-collect create` with only the binary and Docker installed, compare bundle contents against the parity list (spec US2 acceptance scenarios).

### Implementation for User Story 2

- [ ] T029 [US2] Implement `ServiceReplicas` on `DockerBackend` in `src/internal/app/environment_docker.go`: `docker compose -p <proj> ps <service>` → one `Replica` per container using the container **name** (critique P2)
- [ ] T030 [US2] Implement `ReplicaLogs` on `DockerBackend`: `docker logs --tail <n> <container>` via the timeout-bounded pipe primitive; `previous` never requested on Docker
- [ ] T031 [P] [US2] Implement `Metrics` on `DockerBackend`: `docker stats --no-stream` over the project's containers
- [ ] T032 [US2] E2E test `tests/e2e/test_docker_collect.py` (`-m docker`): full bundle with parity content and schema-validated manifest; project selection with/without `--project` (US2 scenario 2); degraded case — stop `cache`, expect exit 0 + `{"name": "cache-status", "status": "failed"}` (FR-009/SC-005); masked env output contains no plaintext secrets (FR-008); `--log-lines=500` and `INFRAHUB_LOG_LINES` precedence recorded in manifest (FR-011)

**Checkpoint**: US1 and US2 both work; bundle layout identical across environments (SC-004).

---

## Phase 5: User Story 3 — Include a backup for issue reproduction (Priority: P3)

**Goal**: `--include-backup` produces a standard backup alongside the bundle in one run, recorded in the manifest; backup failure never loses the bundle.

**Independent Test**: Run `infrahub-collect create --include-backup` against a test deployment; verify the backup artifact exists next to the bundle and is referenced in the manifest (spec US3 acceptance scenarios).

### Implementation for User Story 3

- [ ] T033 [US3] Implement the include-backup collector in `src/internal/app/collect_extras.go`: invoke existing `CreateBackup` unmodified (non-interactive defaults: no S3 upload, no redaction, no encryption — research R10), record the backup's identity/path in the manifest; backup failure → manifest `failed` + bundle still produced (US3 scenario 2)
- [ ] T034 [US3] Unit test in `src/internal/app/collect_extras_test.go`: include-backup outcome recording for success and failure paths (fake backup runner seam), skipped when flag unset
- [ ] T035 [US3] Extend `tests/e2e/test_docker_collect.py` with an `--include-backup` case: backup artifact created next to the bundle, manifest references it, collect exit 0

**Checkpoint**: All three user stories independently functional.

---

## Phase 6: Opt-in benchmark (FR-013)

**Purpose**: Cross-environment opt-in benchmark collector — not tied to a user story; required by FR-013 and the published `--benchmark` flag.

- [ ] T036 Implement the benchmark collector in `src/internal/app/collect_extras.go`: port the benchmark image reference from the Python `invoke bundle collect` implementation (critique Q1) with `INFRAHUB_BENCHMARK_IMAGE` override; Docker: `docker run` attached to the project network; Kubernetes: one-off `kubectl run` pod in the namespace; capture output to `bundle/benchmark/`; always delete the transient container/pod; image pull/run failure → manifest `skipped` + warning (research R11); default off → `skipped`/`not requested`; add an `INFRAHUB_BENCHMARK_IMAGE` row to `docs/docs/reference/configuration.mdx` (Vale/rumdl must pass)

---

## Phase 7: Polish & Cross-Cutting Concerns

- [ ] T037 [P] Update `AGENTS.md` project overview and architecture sections from two to three binaries, adding `infrahub-collect` command summary (constitution v1.1.0 Sync Impact Report mandates this alongside implementation)
- [ ] T038 [P] Wire collect e2e tests into the existing docker/k8s e2e jobs in `.github/workflows/ci.yml` (same markers/fixtures; critique E4)
- [ ] T039 [P] Fix `docs/docs/guides/install-collect.mdx` prerequisite "Go 1.21 or later" → Go 1.25 (matches `go.mod`; Vale/rumdl must pass)
- [ ] T040 Run `specs/003-collect-tool/quickstart.md` validation scenarios 1–4 and 6 locally (build, CLI smoke, Docker collection, degraded case, flag/env precedence) and full gates: `make fmt && make vet && make lint && make test`

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (Phase 1)**: no dependencies; T002/T003 parallel after T001 exists (T004 needs all three)
- **Foundational (Phase 2)**: depends on Setup; **blocks all user stories**. T005–T008 fully parallel; T009+T010 before T011; T011 before T012/T013
- **US1 (Phase 3)**: depends on Foundational. T014 before T015; T017 needs T014/T015; T018–T023 parallel (same file `collect_diagnostics.go` for T018–T023 — parallel authoring only if split carefully; safe order is sequential within that file); T025 needs T017–T024; T027 before T028
- **US2 (Phase 4)**: depends on Foundational + T017/T024/T025 (shared collectors from US1). T029 before T030; T032 needs T027
- **US3 (Phase 5)**: depends on Foundational + T025 (run-plan registration); independent of US2
- **Benchmark (Phase 6)**: depends on Foundational + T025
- **Polish (Phase 7)**: T037–T039 anytime after their subjects stabilize; T040 last

### User Story Dependencies

- **US1 (P1)**: only Foundational — the MVP
- **US2 (P2)**: Foundational + the environment-agnostic collectors delivered in US1 (T017–T025); its own backend primitives are independent
- **US3 (P3)**: Foundational + run-plan registration (T025); no dependency on US2

### Parallel Opportunities

- Phase 2: T005+T006, T007+T008 pairs in parallel (different files)
- Phase 3: T016 parallel with T014/T015; T018–T023 are distinct collectors (parallelize by pre-splitting `collect_diagnostics.go` sections or serialize)
- Phase 4: T031 parallel with T029/T030
- Phase 7: T037, T038, T039 all parallel

## Implementation Strategy

**MVP first**: Phases 1–3 deliver US1 (Kubernetes) — the zero-alternative gap and the feature's core value. Stop, validate with quickstart scenario 5 / e2e `-m k8s -k collect`, then increment.

**Incremental delivery**: Phase 4 turns on Docker parity (US2) with only three backend primitives + one e2e file. Phase 5 (US3) and Phase 6 (benchmark) are small, independent additions. Phase 7 closes the constitution follow-up (AGENTS.md), CI wiring, and full-gate validation.

**Notes**: commit after each task or logical group; `make fmt && make vet && make lint && make test` must stay green at every checkpoint (Principle IV); no `go.mod` changes are expected — if one occurs, run `scripts/update-vendor-hash.sh` (constitution).
