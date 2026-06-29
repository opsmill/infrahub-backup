# Implementation Plan: Rework Plakar backend onto upstream database integrations

**Branch**: `003-upstream-plakar-integrations` | **Date**: 2026-06-29 | **Spec**: [spec.md](./spec.md)
**Input**: Feature specification from `specs/003-upstream-plakar-integrations/spec.md`

## Summary

Replace the Plakar backend's bespoke dump/restore mechanics (the custom `StreamingImporter`, hand-rolled `pg_dump`/`neo4j-admin` commands, and export-then-rebuild restore) with two upstream Plakar integrations: the existing `integration-postgresql` (task-manager/Prefect DB, as-is) and a **new generic `integration-neo4j`** authored here and contributed to `PlakarKorp/integrations`. The integrations run **co-located with each database** inside a one-shot runner (preserving zero host dependencies and covering Neo4j Community offline dumps); the tool keeps all Infrahub-specific orchestration (environment detection, lifecycle/stop-start, task drain, snapshot tags & grouping, edition detection, redaction). A runnable compile spike gates whether the integrations are consumed **in-process** (evidence says feasible → runner = our own binary) or via the **Plakar CLI/plugin** fallback. Clean break: only the new integration layout is created/restored; Compose **and** Kubernetes at parity. See [research.md](./research.md) for the resolved decisions.

## Technical Context

**Language/Version**: Go 1.25.0
**Primary Dependencies**: `github.com/PlakarKorp/kloset` (target stable v1.1.0), `go-kloset-sdk` v1.1.0 (fallback path), `integration-postgresql` v1.1.0-beta.7, `integration-neo4j` (new, fork build), `integration-fs`/`integration-s3`, `cobra`, `viper`, `logrus`, `pgx/v5` (retained for the separate task-manager tool); `testcontainers-go` for integration tests
**Storage**: kloset repository — `fs://` (local dir) or `s3://` (object store); plaintext by default (unchanged)
**Testing**: `go test`; `testcontainers-go` for the neo4j integration (Community + Enterprise) and for tool E2E against a Docker Compose Infrahub deployment
**Target Platform**: tool binaries — Linux/Darwin/Windows (amd64/arm64); runner image — Linux; deployments — Docker Compose and Kubernetes
**Project Type**: single Go CLI project (this repo) **plus** one external Go module (the upstream `integration-neo4j`, developed in a fork worktree)
**Performance Goals**: backup/restore bounded by database size and kloset content-defined chunking; no fixed numeric latency/throughput target (deferred — see spec Deferred items)
**Constraints**: zero host dependencies (no host DB client tools, no port-forwarding); dump/restore co-located with the DB; clean break from the legacy custom-importer layout; Compose + K8s parity
**Scale/Scope**: one Infrahub deployment per invocation; two databases (Neo4j graph + Prefect Postgres); single-named-database backups

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

The project constitution (`.specify/memory/constitution.md`) is the **unpopulated template** — it defines no ratified principles, so there are no constitutional gates to evaluate. In their absence, the plan adheres to the repository conventions documented in `CLAUDE.md`:

- **Cobra command-pattern** with two binaries (`infrahub-backup`, `infrahub-taskmanager`) over shared `src/internal/app` logic — preserved.
- **Embedded assets** via Go `embed` (e.g. the watchdog binary, Python scripts) — the neo4j watchdog asset is *removed* (R7), not added to.
- **Nix vendor hash**: any `go.mod` change runs `scripts/update-vendor-hash.sh` (do not hand-edit `flake.nix`).
- **Lint/format/vet**: `make lint` (golangci-lint, errcheck disabled), `make fmt`, `make vet`.
- **Error wrapping** with `fmt.Errorf`; errors returned up to Cobra handlers.

**Initial gate**: PASS (no principles to violate). **Post-design re-check**: PASS — the design adds one new build artifact (runner image) and one external module (integration-neo4j); both are justified in Complexity Tracking and neither contradicts a (non-existent) constitutional rule.

## Project Structure

### Documentation (this feature)

```text
specs/003-upstream-plakar-integrations/
├── plan.md              # This file
├── research.md          # Phase 0 — decisions (done)
├── data-model.md        # Phase 1 — entities & snapshot layouts
├── quickstart.md        # Phase 1 — operator + contributor walkthrough
├── contracts/           # Phase 1 — connector URI/option & runner contracts
│   ├── neo4j-integration.md
│   ├── postgres-consumption.md
│   └── runner-interface.md
├── checklists/
│   └── requirements.md  # spec quality checklist (done)
├── critique.md          # Phase 3 (critique) output
├── alignment-check.md   # Phase 5 (alignment) output
└── tasks.md             # /speckit.tasks output
```

### Source Code (repository root)

```text
# Deliverable B — this repo (Go CLI tool)
src/
├── cmd/
│   ├── infrahub-backup/main.go         # unchanged entry point
│   └── infrahub-taskmanager/main.go    # unchanged entry point
└── internal/app/
    ├── plakar.go                       # repo open/create (kloset) — retained; runner repo-reachability added
    ├── plakar_backup.go                # REWORK: orchestrate runner + integrations instead of StreamingImporter
    ├── plakar_restore.go               # REWORK: drive integration exporters; drop export-then-rebuild
    ├── importer.go                     # DELETE (custom StreamingImporter)
    ├── backup_neo4j.go                 # REWORK: edition detect + lifecycle; drop dump-command construction
    ├── backup_neo4j_watchdog.go        # DELETE (replaced by writer-stop + volume mount, R7)
    ├── backup_taskmanager.go           # REWORK: drop pg_dump construction; point integration at the DB
    ├── snapshots.go                    # retained (tags & grouping)
    ├── environment_docker.go           # ADD: RunEphemeralContainer + mount/network discovery
    ├── environment_kubernetes.go       # ADD: CreateEphemeralJob + PVC/mount discovery
    ├── runner.go                       # NEW: runner abstraction (launch, env/cred injection, repo mount)
    └── connectors.go                   # NEW (in-process path): register postgres/neo4j connectors

# Runner image (new build artifact)
build/runner/Dockerfile                 # NEW: our binary (or plakar+plugins) + pg client + neo4j-admin + JRE

tests/                                  # E2E (Compose) for both editions + Postgres

# Deliverable A — external module (developed in a fork worktree, contributed upstream)
<fork of PlakarKorp/integrations>@integration/neo4j : neo4j/
├── go.mod                              # module github.com/PlakarKorp/integration-neo4j
├── importer/importer.go                # neo4j:// (online) + neo4j+offline:// (offline)
├── exporter/exporter.go                # restore / load
├── neo4jconn/conn.go                   # URI + config → connection / admin invocation
├── manifest/{manifest,metadata}.go     # /manifest.json
├── plugin/*/main.go                    # SDK entrypoints (fallback packaging)
├── manifest.yaml                       # connector declarations
├── Makefile · README.md · neo4j.1
└── tests/                              # testcontainers (Community + Enterprise)
```

**Structure Decision**: Single Go CLI project for the tool rework (Deliverable B), modifying `src/internal/app` in place and adding a `build/runner/` image. The generic Neo4j integration (Deliverable A) is a **separate Go module** developed in a fork worktree of `PlakarKorp/integrations` (so it can be contributed upstream) and consumed via `go.mod replace` (in-process) or `plakar pkg build` (fallback). The two deliverables map to the two implementation plans the spec calls for; `/speckit.tasks` will phase them so Deliverable A (+ the Task 0 spike) precedes the parts of Deliverable B that depend on it.

## Complexity Tracking

> No constitutional violations (no ratified constitution). The two additions below are inherent to the spec's decisions, recorded for transparency.

| Addition | Why Needed | Simpler Alternative Rejected Because |
|----------|------------|--------------------------------------|
| New runner image artifact | Inside-container execution (FR-004) requires Plakar/our-binary + DB client tools co-located with the DB; the stock DB images lack them | Host + port-forward rejected in spec (host deps; no Community offline). Exec into existing DB container rejected — images lack the runner and Community needs the writer stopped |
| External `integration-neo4j` module | No upstream Neo4j integration exists; the feature's premise is reusable upstream mechanics (FR-017) | Keeping a custom in-repo importer is exactly what the feature removes; an Infrahub-private integration would not be reusable/contributable |
