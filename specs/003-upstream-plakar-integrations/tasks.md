---
description: "Task list for feature 003 — rework Plakar backend onto upstream database integrations"
---

# Tasks: Rework Plakar backend onto upstream database integrations

**Input**: Design documents from `/specs/003-upstream-plakar-integrations/`
**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/

**Tests**: Test tasks are INCLUDED — the spec requests testcontainers tests for the integration and E2E tests for the tool (both editions + Postgres, Compose + K8s).

**Organization**: Grouped by user story. Note the cross-deliverable dependency: the Neo4j integration (Deliverable A, mapped to US4) is built **before** US2/US3, which consume it. US1 (Postgres) is fully independent and is the MVP.

## Path Conventions

- Tool (Deliverable B): `src/internal/app/`, `build/runner/`, `tests/` at repo root.
- Integration (Deliverable A): a fork worktree of `PlakarKorp/integrations`, branch `integration/neo4j`, subdir `neo4j/` (module `github.com/PlakarKorp/integration-neo4j`).

---

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: Resolve the gating spike, align versions, scaffold the new artifacts.

- [x] T001 Run the consumption-model spike (gating): in a throwaway module, add `integration-postgresql@latest` + `kloset@v1.1.0`, blank-import its `importer`/`exporter`, `go build`, and confirm the `postgres` connector registers. Record PASS (in-process) / FAIL (plakar-CLI fallback) in `plan.md`. **✅ DONE 2026-06-30 — PASSED: builds clean against kloset v1.1.0; decision = in-process (see research.md R1).**
- [ ] T002 Align module versions in `go.mod`: `kloset` → `v1.1.0`, `integration-fs`/`integration-s3` to the matching line; then run `scripts/update-vendor-hash.sh` (never hand-edit `flake.nix`).
- [ ] T003 [P] Add the pinned `integration-postgresql` dependency to `go.mod` (exact version); run `scripts/update-vendor-hash.sh`.
- [ ] T004 [P] Scaffold the runner image at `build/runner/Dockerfile` (base image + `pg_dump`/`pg_restore`/`psql` + `neo4j-admin` + JRE; binary/plakar layer filled per the T001 decision).
- [ ] T005 [P] Scaffold the `integration-neo4j` module skeleton in a fork worktree of `PlakarKorp/integrations` (orphan branch `integration/neo4j`, `neo4j/` subdir, `go.mod` → `module github.com/PlakarKorp/integration-neo4j`), mirroring the `mysql/` layout.

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: Core runner + consumption + lifecycle infrastructure shared by ALL stories. No story can complete before this phase.

- [ ] T006 Implement the dependency-light connector-run entrypoint in `src/internal/app/run_connector.go` (hidden `__run-connector` subcommand: opens the kloset repo, runs ONE connector op; NO docker/kubectl deps) — critique E1.
- [ ] T007 Implement consumption wiring in `src/internal/app/connectors.go`: in-process blank-import + registration of `postgres`/`neo4j` connectors (if T001 PASS), else a `plakar`-CLI invocation shim.
- [ ] T008 Implement the runner abstraction in `src/internal/app/runner.go` (launch one connector op co-located with a DB; env/cred injection; repo mount; label + reap orphaned runners) — critique E2.
- [ ] T009 [P] Implement `RunEphemeralContainer` + compose network/volume-mount discovery (`docker inspect … .Mounts`) in `src/internal/app/environment_docker.go`.
- [ ] T010 [P] Implement `CreateEphemeralJob` + PVC/mount discovery, node affinity, and log collection (`kubectl` subprocess) in `src/internal/app/environment_kubernetes.go`.
- [ ] T011 Wire credential discovery → runner injection with override flags in `src/internal/app/app_config.go` + `src/internal/app/runner.go` (reuse existing `fetchDatabaseCredentials`; FR-023; prefer non-`-e` secret passing per critique E3).
- [ ] T012 Implement repo reachability from the runner (mount `fs://` dir; inject `s3://`/`AWS_*` creds) in `src/internal/app/plakar.go` + `runner.go`.
- [ ] T013 Ensure runner-produced snapshots receive the existing grouping/edition/version tags in `src/internal/app/plakar_backup.go` + `snapshots.go` (retain the `infrahub.*` scheme).
- [ ] T014 DELETE the custom importer `src/internal/app/importer.go` and the watchdog `src/internal/app/backup_neo4j_watchdog.go` (+ embedded watchdog binaries); remove all references (FR-003, R7).
- [ ] T015 Implement the clean-break restore guard in `src/internal/app/plakar_restore.go` (detect & reject legacy custom-importer layout with an actionable message naming the prior tool version; FR-016).

---

## Phase 3: User Story 1 — Postgres via upstream integration (Priority: P1) 🎯 MVP

**Goal**: Capture and restore the task-manager/Prefect Postgres DB via `integration-postgresql`, co-located, zero host deps.

**Independent Test**: On a Compose deploy with no host PG client tools, back up only the task-manager DB → snapshot has the integration layout + grouping tags → restore into a clean PG → data matches.

- [ ] T016 [P] [US1] Build the `postgres://` source URI + options (`database=<prefect-db>`, `compress=false`) from discovered creds in `src/internal/app/backup_taskmanager.go` (drop the hand-rolled `pg_dump` construction).
- [ ] T017 [US1] Orchestrate the postgres backup through the runner co-located with `task-manager-db` in `src/internal/app/plakar_backup.go`.
- [ ] T018 [US1] Implement postgres restore via the `postgres://` exporter (`clean`/`no_owner` as needed) through the runner in `src/internal/app/plakar_restore.go`.
- [ ] T019 [P] [US1] E2E test (`tests/e2e_postgres_test.go`): Compose deploy → backup → restore into clean PG → assert databases/roles/data match; assert success with no host PG client tools (SC-001, SC-002).

**Checkpoint**: US1 is independently shippable — the MVP — before any neo4j work.

---

## Phase 4: Deliverable A — generic `integration-neo4j` (maps to US4; prerequisite for US2/US3)

**Goal**: A generic, reusable Neo4j Plakar integration (no Infrahub assumptions) supporting Enterprise online + Community offline, contributed upstream.

**Independent Test**: With only Plakar + the integration (no Infrahub), back up & restore a standalone Neo4j for both editions; the integration's own test suite passes (SC-005).

- [ ] T020 [US4] Implement `neo4j/neo4jconn/conn.go`: parse `neo4j://` and `neo4j+offline://` URIs + config map; locate/invoke `neo4j-admin`.
- [ ] T021 [US4] Implement `neo4j/importer/importer.go` online path: register `neo4j` (`FLAG_STREAM`); run `neo4j-admin database backup --compress=false --to-path …`; emit `/manifest.json` then artifact records.
- [ ] T022 [US4] Implement the offline path: register `neo4j+offline`; run `neo4j-admin database dump`; emit `/manifest.json` + `/<db>.dump`; fail fast if the DB appears running (precondition; FR-010).
- [ ] T023 [US4] Implement `neo4j/manifest/{manifest,metadata}.go` (`/manifest.json`: version, edition, db, store format; Bolt node/relationship counts when online, omitted offline).
- [ ] T024 [US4] Implement `neo4j/exporter/exporter.go`: dispatch restore (`database restore --from-path`) vs load (`database load`) by snapshot layout (`neo4j://` / `neo4j+offline://`).
- [ ] T025 [P] [US4] Add `neo4j/manifest.yaml` (connector declarations for `[neo4j]`/`[neo4j+offline]` importers + exporters) + `neo4j/plugin/*/main.go` SDK entrypoints + importer/exporter `schema.json`.
- [ ] T026 [P] [US4] testcontainers tests in `neo4j/tests/`: enterprise online round-trip; community offline round-trip; offline-against-running fail-fast (SC-005).
- [ ] T027 [P] [US4] Write `neo4j/README.md`, `neo4j/neo4j.1`, `neo4j/Makefile`.

**Checkpoint**: integration-neo4j builds and its tests pass standalone; consumable via fork build (`go.mod replace` or `plakar pkg build`).

---

## Phase 5: User Story 2 — Neo4j Enterprise online (Priority: P1)

**Goal**: Online (zero-downtime) Enterprise backup + restore via `integration-neo4j`.

**Independent Test**: Against Enterprise Neo4j, back up without stopping the DB (manifest present), restore into a clean Enterprise instance, graph matches; unreachable backup port fails fast.

- [ ] T028 [US2] Consume `integration-neo4j` from the fork in `go.mod` (`replace` for in-process, or runner-image `plakar pkg build`); run `scripts/update-vendor-hash.sh`.
- [ ] T029 [US2] Wire enterprise online backup (edition detect → `neo4j://…@database:6362/<db>` source → runner co-located with `database`) in `src/internal/app/backup_neo4j.go` + `plakar_backup.go`; drop the hand-rolled `neo4j-admin` construction.
- [ ] T030 [US2] Implement enterprise restore via the `neo4j://` exporter through the runner in `src/internal/app/plakar_restore.go`.
- [ ] T031 [P] [US2] E2E test (`tests/e2e_neo4j_enterprise_test.go`): online backup (assert DB never stopped) → restore → graph matches; backup-port-unreachable → fail-fast (SC-001, SC-003).

---

## Phase 6: User Story 3 — Neo4j Community offline (Priority: P2)

**Goal**: Offline Community backup + restore, with the tool owning the stop/start lifecycle.

**Independent Test**: Against Community Neo4j, the tool stops app+DB, runner does the offline dump on the mounted store, services restart healthy; restore into a clean Community instance matches.

- [ ] T032 [US3] Implement the community lifecycle in `src/internal/app/backup_neo4j.go`: drain → stop app containers → stop the Neo4j writer (container/workload) → **verify fully stopped** → … → restart on success AND failure paths (replaces the watchdog; FR-009, critique E2).
- [ ] T033 [US3] Wire community offline backup (`neo4j+offline:///<datadir>?database=<db>` source + neo4j store-volume mount into the runner) in `src/internal/app/plakar_backup.go`.
- [ ] T034 [US3] Implement community restore via the `neo4j+offline://` exporter (`database load`) through the runner in `src/internal/app/plakar_restore.go`.
- [ ] T035 [P] [US3] E2E test (`tests/e2e_neo4j_community_test.go`): backup (assert stop→restart healthy) → restore → graph matches; offline-dump-against-running → fail-fast (SC-003, SC-006).

---

## Phase 6b: Restore selector (cross-story; closes FR-024)

- [ ] T043 Implement the restore selector in `src/internal/app/plakar_restore.go` + the backup CLI (`src/cmd/infrahub-backup/main.go`): full-group restore (all components of a `backup-id`) and selective per-component restore via `--component neo4j|postgres` (FR-024). Depends on T018/T030/T034.
- [ ] T044 [P] Test the restore selector (`tests/e2e_restore_selector_test.go`): full-group restore vs Neo4j-only vs Postgres-only on the same backup group (FR-024).

---

## Phase 7: Polish & Cross-Cutting Concerns

- [ ] T036 [P] Finalize `build/runner/Dockerfile` per the T001 decision (our binary OR plakar+plugins) + all client tools; add a `make runner-image` target, flake output, and CI build.
- [ ] T037 [P] Add the beta-`integration-postgresql` backup→restore round-trip validation as a CI release gate (critique E7).
- [ ] T038 [P] Provision CI test infra: kind/k3d for K8s E2E parity (SC-008) and Docker for testcontainers; gate heaviest E2E as nightly if needed (critique E5).
- [ ] T039 [P] Tag the last pre-rework tool release (for legacy-snapshot restore) and reference it in the clean-break rejection message (FR-016, critique P4); update `README.md` + quickstart.
- [ ] T040 Open the upstream PR for `integration-neo4j` against base branch `integration/neo4j` in `PlakarKorp/integrations` (FR-017, FR-021).
- [ ] T041 Run `scripts/update-vendor-hash.sh` after final `go.mod` changes; `make fmt lint vet test` all green; remove dead references to the deleted importer/watchdog.
- [ ] T042 Verify SC-001…SC-008 across BOTH Compose and Kubernetes; confirm SC-004 (custom dump/restore removed) and SC-007 (legacy rejection message).
- [ ] T045 Retain & regression-test the orchestration behaviors that survive the rework: task drain `waitForRunningTasks` (FR-008), `--redact` (FR-011), and group complete/incomplete status (FR-014) — add/keep targeted tests so the rework does not silently drop them.

---

## Dependencies & Execution Order

- **Phase 1 (Setup)** → **Phase 2 (Foundational)** block everything. T001 (spike) gates T004/T007/T036 (runner/consumption approach).
- **US1 (Phase 3)** depends only on Phases 1–2 → **MVP, shippable first**.
- **Deliverable A (Phase 4)** depends on Phase 1 (T005) and is the **prerequisite for US2 (Phase 5) and US3 (Phase 6)**.
- **US2** and **US3** depend on Phase 2 + Phase 4; they are independent of each other.
- **Phase 7 (Polish)** after the stories it references.

```
Setup → Foundational → ┬→ US1 (MVP) ───────────────┐
                       └→ Deliverable A (neo4j) → ┬→ US2 ┐
                                                  └→ US3 ┴→ Polish
```

## Parallel Opportunities

- Phase 1: T003, T004, T005 in parallel after T002.
- Phase 2: T009 (Compose) ∥ T010 (K8s) after T008.
- Phase 4: T025, T026, T027 ∥ after T020–T024.
- Phase 7: T036, T037, T038, T039 ∥.
- Cross-stream: once Phase 2 is done, US1 (Phase 3) and Deliverable A (Phase 4) can proceed in parallel by different contributors.

## Implementation Strategy

- **MVP** = Phases 1–2 + **US1** (Postgres). Delivers the new co-located-runner model end-to-end on the integration that already exists, de-risking everything.
- **Increment 2** = Deliverable A (integration-neo4j) + **US2** (Enterprise online).
- **Increment 3** = **US3** (Community offline) + Polish.
- Maps to the spec's two plans: Deliverable A (Phase 4 + its polish) and Deliverable B (Phases 1–3, 5–7).

**Total tasks**: 45 · US1: 4 · US4/Deliverable A: 8 · US2: 4 · US3: 4 · restore-selector (FR-024): 2 · Setup/Foundational/Polish: 23.
