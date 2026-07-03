# Implementation Plan: Infrahub Collect (Troubleshooting Bundle Tool)

**Branch**: `fac/collect-tool-implem-opip6` (feature `003-collect-tool`) | **Date**: 2026-07-03 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `/specs/003-collect-tool/spec.md`

## Summary

Add a third CLI binary `infrahub-collect` whose `create` command gathers a troubleshooting bundle — per-service logs (all replicas, plus previous-container logs on Kubernetes), parity diagnostics (Neo4j, RabbitMQ, Redis, Prefect worker/manager, server info), and container metrics — into a single local `support_bundle_<timestamp>.tar.gz` with a `bundle_information.json` manifest recording an explicit outcome per collector.

Technical approach: a thin Cobra entry point at `src/cmd/infrahub-collect` mirroring the two existing binaries, with all logic in `src/internal/app`. Collection reuses the existing `EnvironmentBackend` abstraction (Docker Compose / Kubernetes detection, exec, copy, CNPG primary targeting) and `CommandExecutor`, extended with net-new read-only primitives for replica enumeration, log streaming, and resource metrics. A new collector framework runs each collector independently (failures are non-fatal), masks secrets in env/config output by key name, stages files under a working directory, and packages them with the existing `createTarball` helper. `--include-backup` delegates to the existing `CreateBackup` unmodified; `--benchmark` is opt-in and degrades to skipped when its image is unavailable. The user-facing contract (commands, flags, env vars, bundle layout, manifest fields) is already published in `docs/docs/` and is treated as normative.

## Technical Context

**Language/Version**: Go 1.25.0 (`CGO_ENABLED=0`, `-tags untested_go_version`; cross-compiles linux/darwin/windows × amd64/arm64 via `make build-all`)

**Primary Dependencies**: cobra v1.10.2, viper v1.21.0, logrus v1.9.4 (all existing). Docker and Kubernetes interaction shells out to the `docker` and `kubectl` CLIs through the existing `CommandExecutor` — no client-go or Docker SDK. **No new Go module dependencies expected**; if any are added, `scripts/update-vendor-hash.sh` must be run (constitution, Operational Constraints).

**Storage**: local filesystem only — bundle staging directory and final `.tar.gz` under `--output-dir` (default `./infrahub_bundles`, env `INFRAHUB_OUTPUT_DIR`). No network egress beyond the target deployment (FR-012); S3 upload explicitly out of scope (INFP-581).

**Testing**: `go test` table-driven unit tests in `src/internal/app/*_test.go` for pure logic (masking, manifest construction, collector outcome recording, replica/log-arg building) using a fake backend; Python/pytest end-to-end tests in `tests/e2e/` (`-m docker` / `-m k8s` markers) invoking the built binary as a subprocess, following the existing `backup_binary` fixture pattern.

**Target Platform**: operator workstations and CI runners (Linux/macOS/Windows) with Docker CLI or kubectl access to the target deployment; runs outside the cluster.

**Project Type**: CLI — third binary in an established split-binary, shared-core Go repository.

**Performance Goals**: complete a bundle from a reference deployment in under 5 minutes with a single command (SC-001); stream progress per collector in real time (Principle V).

**Constraints**: strictly read-only with respect to workload lifecycle — no stop/restart/scale ever (FR-010, SC-003); fully offline by default — no image pulls, no egress (FR-012); individual collector failures are non-fatal and the command still exits 0 with a partial bundle (FR-009, SC-005); identical command surface and bundle layout across Docker and Kubernetes (FR-006, SC-004); per-service log cap configurable, default 100,000 lines (FR-011).

**Scale/Scope**: 8 Infrahub services × N replicas per service (Kubernetes); log volume up to `--log-lines` per container (default 100k lines); one archive per run.

## Constitution Check

*GATE: evaluated against constitution v1.1.0 — pre-Phase-0 PASS, re-checked post-Phase-1 PASS.*

| Principle | Verdict | Evidence |
|-----------|---------|----------|
| I. Split-Binary, Shared-Core | ✅ PASS | Constitution v1.1.0 already enumerates `src/cmd/infrahub-collect` (amendment ratified 2026-07-02). Entry point is thin Cobra wiring only; all collection logic lands in `src/internal/app`. Mandated follow-up: AGENTS.md/CLAUDE.md still say "two specialized CLI binaries" and MUST be updated in this feature (constitution Sync Impact Report). |
| II. Operational Safety & Data Integrity | ✅ PASS | Collection is stricter than backup: read-only, never stops/starts workloads (FR-010). No destructive operations, so no confirmation flags required. `--include-backup` delegates to the existing `CreateBackup` path unmodified, preserving its stop/restart-on-failure and metadata/checksum guarantees. Bundle manifest is the collect-side analog of backup metadata (FR-007). |
| III. Self-Contained, Cross-Platform Binaries | ✅ PASS | No new runtime assets beyond possibly small embedded scripts via the existing `go:embed scripts/*` pattern. Depends only on docker/kubectl already present in the target environment; absence fails fast with the existing `ErrCLIUnavailable` actionable error (FR requirement, edge case "Missing CLI prerequisites"). `CGO_ENABLED=0` build via existing Makefile matrix. |
| IV. Quality Gates: Test, Lint, Vet | ✅ PASS | Plan includes table-driven unit tests for all new pure logic (masking, manifest, collector outcomes) and new pytest e2e collect tests for Docker and Kubernetes flows. `make test`, `make vet`, `make lint`, `make fmt` gate merge as usual. |
| V. Explicit Errors & Streaming Feedback | ✅ PASS | Collectors wrap errors with `fmt.Errorf`/`%w` and return them to the orchestrator, which records them in the manifest and logs warnings; no `os.Exit` in `src/internal/app`. Per-collector progress streams via logrus as each collector runs (docs show the exact expected output). |
| Operational Constraints | ✅ PASS | Docker Compose service names / K8s label selectors reused as-is (public deployment contract, unchanged). No change to backup snapshot layout. Docs already exist and passed Vale/rumdl in prior commits. `go.mod` unchanged → no vendor-hash update expected. |

**Gate result**: PASS — no violations, Complexity Tracking not required.

## Project Structure

### Documentation (this feature)

```text
specs/003-collect-tool/
├── spec.md              # Feature specification (complete)
├── plan.md              # This file
├── research.md          # Phase 0 output
├── data-model.md        # Phase 1 output
├── quickstart.md        # Phase 1 output
├── contracts/           # Phase 1 output
│   ├── cli.md                   # Command/flag/env/exit-code contract (normative)
│   ├── bundle-layout.md         # Archive layout contract
│   └── manifest.schema.json     # bundle_information.json JSON Schema
├── checklists/requirements.md   # Spec quality checklist (complete)
└── tasks.md             # Phase 2 output (/speckit-tasks — NOT created by plan)
```

### Source Code (repository root)

```text
src/
├── cmd/
│   ├── infrahub-backup/main.go          # unchanged
│   ├── infrahub-taskmanager/main.go     # unchanged
│   └── infrahub-collect/main.go         # NEW: thin Cobra wiring — root cmd, create cmd,
│                                        #      flags (--output-dir, --log-lines, --include-backup,
│                                        #      --include-queries, --benchmark), version cmd,
│                                        #      ConfigureRootCommand + AttachEnvironmentCommands
└── internal/app/
    ├── collect.go                       # NEW: CollectBundle orchestrator — staging dir, collector
    │                                    #      loop (non-fatal failures), archive, cleanup
    ├── collect_manifest.go              # NEW: BundleManifest, CollectorResult, status constants,
    │                                    #      manifest construction + JSON writing
    ├── collect_logs.go                  # NEW: service-log collectors (all replicas, previous logs)
    ├── collect_diagnostics.go           # NEW: parity collectors — database, message-queue, cache,
    │                                    #      task-worker, task-manager, server info
    ├── collect_metrics.go               # NEW: container metrics (docker stats / kubectl top)
    ├── collect_extras.go               # NEW: opt-in collectors — include-backup, benchmark
    ├── masking.go                       # NEW: key-name secret masking (password|secret|token|key)
    ├── environment.go                   # EXTEND: replica/log/metrics read-only primitives
    ├── environment_docker.go            # EXTEND: compose ps -q replica listing, docker logs, stats
    ├── environment_kubernetes.go        # EXTEND: pod replica listing w/ restart counts,
    │                                    #         kubectl logs [--previous], kubectl top
    ├── cli.go                           # unchanged (shared flags already cover --project,
    │                                    #            --k8s-namespace, --log-format)
    └── *_test.go                        # NEW: masking_test.go, collect_manifest_test.go,
                                         #      collect_logs_test.go (fake backend)

tests/e2e/
├── test_docker_collect.py               # NEW: compose collect e2e (+ degraded-service case)
└── test_k8s_collect.py                  # NEW: k8s collect e2e (replicas, previous logs)

Makefile                                 # EXTEND: BINARIES += infrahub-collect
flake.nix                                # EXTEND: third buildGoModule package + symlinkJoin entry
AGENTS.md                                # EXTEND: two → three binaries (constitution follow-up)
```

**Structure Decision**: single Go project, existing layout. The new binary is a sibling of the two existing entry points; all new logic lives in `src/internal/app` as `collect_*.go` files plus a shared `masking.go`. Backend extensions are added as methods on the existing `DockerBackend`/`KubernetesBackend` types and surfaced to collectors through a narrow collect-side interface so collectors are unit-testable with a fake. The Dockerfile and Homebrew/Docker release jobs remain `infrahub-backup`-only in v1 (no collect container image is promised in docs; the S3 binary upload in `release.yml` picks up the new binary automatically through `BINARIES`).

## Complexity Tracking

No constitution violations — table not required.
