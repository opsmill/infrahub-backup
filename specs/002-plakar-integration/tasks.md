# Tasks: Plakar Integration for infrahub-backup

**Input**: Design documents from `/specs/002-plakar-integration/`
**Prerequisites**: plan.md (required), spec.md (required for user stories), research.md, data-model.md, contracts/

**Tests**: Not explicitly requested in the spec. Unit tests are included for new code as per plan.md structure.

**Organization**: Tasks are grouped by user story to enable independent implementation and testing of each story.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel (different files, no dependencies)
- **[Story]**: Which user story this task belongs to (e.g., US1, US2, US3)
- Include exact file paths in descriptions

## Path Conventions

- **Single project**: `src/` at repository root, Go module structure
- Paths use existing project layout from plan.md

---

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: Add Plakar dependencies and extend core types

- [x] T001 Add `github.com/PlakarKorp/kloset` and `github.com/PlakarKorp/integration-fs` dependencies to `go.mod` via `go get`
- [x] T002 Add `BackendType` string type and `PlakarConfig` struct to `src/internal/app/app.go` — fields: `RepoPath string`, `CacheDir string`, `SnapshotID string`, `Plaintext bool`, `Encrypt bool`; add `Backend BackendType` and `Plakar *PlakarConfig` fields to `Configuration` struct
- [x] T003 Add shared Plakar CLI flags (`--backend`, `--repo`) to `src/internal/app/cli.go` in `ConfigureRootCommand()` — bind to viper with `INFRAHUB_` prefix; set `BackendType` default to `"tarball"`

**Checkpoint**: Project compiles with new dependencies and types. `make build` succeeds. Existing behavior unchanged.

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: Core Plakar infrastructure that MUST be complete before ANY user story can be implemented

**⚠️ CRITICAL**: No user story work can begin until this phase is complete

- [x] T004 Create `src/internal/app/plakar.go` — implement `initPlakarContext()` returning `*kcontext.KContext` with hostname, CWD, cache directory (`~/.cache/infrahub-backup/plakar/` default from `PlakarConfig.CacheDir`), logrus-backed logger, and `caching.Manager` (pebble backend)
- [x] T005 [P] Implement `openOrCreateRepo()` in `src/internal/app/plakar.go` — if repo path exists: `storage.Open()` then `repository.NewNoRebuild()`; if not: create new plaintext repo via `storage.Create()` with default `storage.Configuration`; handle `PlakarConfig.Encrypt` for passphrase-protected repos
- [x] T006 [P] Implement `closeRepo()` and `closePlakarContext()` cleanup helpers in `src/internal/app/plakar.go`
- [x] T007 Add S3 flag conflict validation in `src/cmd/infrahub-backup/main.go` — in `create` and `restore` command `RunE`, before calling app logic: if `--backend plakar` and any of `--s3-upload`, `--s3-bucket`, `--s3-prefix`, `--s3-endpoint`, `--s3-region`, `--s3-keep-local` are set, return error: `"--s3-upload and related S3 flags cannot be used with plakar backend; use --repo s3://... instead"`
- [x] T008 Add `--backend plakar` requires `--repo` validation in `src/cmd/infrahub-backup/main.go` — if backend is `plakar` and `--repo` is empty, return error: `"--repo is required when using plakar backend"`
- [x] T009 Verify `make build` and `make test` pass with all changes — no regressions in existing tests

**Checkpoint**: Foundation ready — `plakar.go` can init context, open/create repos, and validate CLI flags. User story implementation can now begin.

---

## Phase 3: User Story 1 — Deduplicated Backup Creation (Priority: P1) 🎯 MVP

**Goal**: Create Infrahub backups as Plakar snapshots with deduplication

**Independent Test**: Run `infrahub-backup create --backend plakar --repo /tmp/test-repo` against a running Infrahub instance; verify a Plakar snapshot is created with all components

### Implementation for User Story 1

- [x] T010 [P] [US1] Create `src/internal/app/importer.go` — implement `InfrahubImporter` struct satisfying `importer.Importer` interface: `Origin()` returns hostname, `Type()` returns `"infrahub"`, `Root()` returns `"/"`, `Flags()` returns `FLAG_STREAM`; `Ping()` returns nil; `Close()` cleans up temp dir
- [x] T011 [US1] Implement `Import()` method on `InfrahubImporter` in `src/internal/app/importer.go` — accepts `InfrahubOps` reference and dump config; in Import(): (1) call existing `backupDatabase()` to temp dir, (2) call existing `backupTaskManagerDB()` to temp dir (unless excluded), (3) generate `backup_information.json` metadata, (4) walk temp dir and send each file as `connectors.Record` with pathname, FileInfo, and lazy ReadCloser into records channel
- [x] T012 [US1] Create `src/internal/app/plakar_backup.go` — implement `CreatePlakarBackup()` on `InfrahubOps`: (1) init Plakar context via `initPlakarContext()`, (2) open/create repo via `openOrCreateRepo()`, (3) create `InfrahubImporter`, (4) create `snapshot.NewSource()`, (5) create `snapshot.Create()` builder with tags from backup metadata (infrahub.version, infrahub.neo4j-edition, infrahub.components, infrahub.redacted), (6) call `builder.Backup(source)`, (7) call `builder.Commit()`, (8) cleanup
- [x] T013 [US1] Modify `CreateBackup()` in `src/internal/app/backup.go` — add early check: if `config.Backend == "plakar"`, delegate to `CreatePlakarBackup()` with same parameters and return; existing tarball flow remains untouched below
- [x] T014 [US1] Add `--backend` and `--repo` flags to `createCmd` in `src/cmd/infrahub-backup/main.go` — wire flag values through to `iops.Config()` before calling `CreateBackup()`; add `--encrypt` flag (optional, for FR-020)
- [x] T015 [US1] Verify `make build` succeeds and `make test` passes with no regressions

**Checkpoint**: `infrahub-backup create --backend plakar --repo /path` creates a Plakar snapshot. Default `create` (no --backend) still produces tar.gz.

---

## Phase 4: User Story 2 — Restore from Plakar Snapshot (Priority: P1)

**Goal**: Restore Infrahub databases from a Plakar snapshot

**Independent Test**: After creating a Plakar backup (US1), run `infrahub-backup restore --backend plakar --repo /tmp/test-repo` and verify databases are restored

**Dependencies**: Requires US1 (need a snapshot to restore from)

### Implementation for User Story 2

- [x] T016 [US2] Create `src/internal/app/plakar_restore.go` — implement `RestorePlakarBackup()` on `InfrahubOps`: (1) init Plakar context, (2) open repo, (3) resolve snapshot: if `SnapshotID` set, load by ID via `snapshot.Load()`; if empty, find latest snapshot from repo state, (4) create temp dir, (5) create fs exporter targeting temp dir, (6) call `snapshot.Export()` to extract all files, (7) delegate to existing `restoreNeo4j()` and `restorePostgreSQL()` using extracted files in temp dir, (8) cleanup
- [x] T017 [US2] Modify `RestoreBackup()` in `src/internal/app/backup.go` — add early check: if `config.Backend == "plakar"`, delegate to `RestorePlakarBackup()` with same parameters; existing tarball flow remains untouched
- [x] T018 [US2] Add `--snapshot` flag to `restoreCmd` in `src/cmd/infrahub-backup/main.go` — optional string flag, defaults to empty (latest); bind to viper as `INFRAHUB_SNAPSHOT`; modify restore command `Args` to accept 0 args when `--backend plakar` (positional backup-file not required)
- [x] T019 [US2] Handle snapshot-not-found error in `RestorePlakarBackup()` in `src/internal/app/plakar_restore.go` — if snapshot ID doesn't exist, list available snapshots in error message
- [x] T020 [US2] Verify `make build` succeeds and `make test` passes with no regressions

**Checkpoint**: Full backup→restore cycle works via Plakar. Default restore from tar.gz still works.

---

## Phase 5: User Story 3 — Backward Compatibility with Archive Backups (Priority: P1)

**Goal**: Verify and ensure existing tar.gz backup/restore workflows are completely unchanged

**Independent Test**: Run `infrahub-backup create` (no --backend flag) and confirm tar.gz output; restore from that tar.gz

### Implementation for User Story 3

- [x] T021 [US3] Verify default `create` produces tar.gz — run `infrahub-backup create` without `--backend` flag; confirm output is `.tar.gz` file with `backup_information.json` inside
- [x] T022 [US3] Verify default `restore` from tar.gz works — restore from a tar.gz file produced by the previous step
- [x] T023 [US3] Verify S3 flag conflict rejection works — run `infrahub-backup create --backend plakar --repo /tmp/r --s3-upload` and confirm error message matches contract: `"--s3-upload and related S3 flags cannot be used with plakar backend; use --repo s3://... instead"`
- [x] T024 [US3] Verify unknown backend rejection — run `infrahub-backup create --backend invalid` and confirm error: `"unknown backend: invalid, expected 'tarball' or 'plakar'"`
- [x] T025 [US3] Run `make test` to confirm all existing unit tests pass without modification

**Checkpoint**: All backward compatibility acceptance scenarios verified. No existing workflow broken.

---

## Phase 6: User Story 4 — List and Inspect Snapshots (Priority: P2)

**Goal**: Provide a command to list all Plakar snapshots with Infrahub metadata

**Independent Test**: After creating multiple Plakar backups, run `infrahub-backup snapshots list --repo /path` and verify all snapshots appear with correct metadata

### Implementation for User Story 4

- [x] T026 [US4] Create `src/internal/app/snapshots.go` — implement `ListSnapshots()` on `InfrahubOps`: (1) init Plakar context, (2) open repo (read-only), (3) iterate all snapshots in repo state, (4) for each snapshot: extract tags (infrahub.version, infrahub.neo4j-edition, infrahub.components), creation timestamp, snapshot ID prefix, (5) format output as table (text) or JSON array (when `--log-format json`)
- [x] T027 [US4] Add `snapshots list` subcommand in `src/cmd/infrahub-backup/main.go` — create `snapshotsCmd` parent and `listCmd` child; `listCmd` requires `--repo` flag; calls `iops.ListSnapshots()`
- [x] T028 [US4] Handle empty repository in `ListSnapshots()` in `src/internal/app/snapshots.go` — if no snapshots exist, log info message: `"No snapshots found in repository"`
- [x] T029 [US4] Verify `make build` succeeds and output matches contract format in `specs/002-plakar-integration/contracts/cli-interface.md`

**Checkpoint**: `infrahub-backup snapshots list --repo /path` displays all snapshots with metadata.

---

## Phase 7: User Story 5 — Remote Repository Storage (Priority: P3)

**Goal**: Support S3-compatible storage as Plakar repository backend

**Independent Test**: Run backup with `--repo s3://bucket/prefix` and verify snapshot is stored in S3; restore from same URI

### Implementation for User Story 5

- [ ] T030 [US5] Add `github.com/PlakarKorp/integration-s3` dependency to `go.mod` via `go get`
- [ ] T031 [US5] Import `integration-s3` storage registration in `src/internal/app/plakar.go` — add blank import or explicit `init()` registration so S3 URI scheme (`s3://`) is recognized by kloset's storage layer
- [ ] T032 [US5] Verify `openOrCreateRepo()` in `src/internal/app/plakar.go` handles S3 URIs transparently — kloset's `storage.Open()`/`storage.Create()` should route `s3://` URIs to the S3 backend automatically via registered scheme
- [ ] T033 [US5] Verify `make build` succeeds and existing local-repo tests pass

**Checkpoint**: Backup and restore work against S3 repositories via `--repo s3://bucket/prefix`.

---

## Phase 8: Polish & Cross-Cutting Concerns

**Purpose**: Improvements that affect multiple user stories

- [x] T034 [P] Add backend validation helper in `src/internal/app/app.go` — validate `BackendType` is either `"tarball"` or `"plakar"`; return clear error for unknown values
- [ ] T035 [P] Add `--encrypt` passphrase handling in `src/internal/app/plakar.go` — if `PlakarConfig.Encrypt` is true: read passphrase from `INFRAHUB_PLAKAR_PASSPHRASE` env var or prompt stdin; derive key and pass to `repository.New()`/`repository.Inexistent()`
- [ ] T036 Run `make lint` and fix any golangci-lint issues in new files
- [x] T037 Run full `make test` and verify all tests pass
- [ ] T038 Verify quickstart examples from `specs/002-plakar-integration/quickstart.md` work end-to-end

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (Phase 1)**: No dependencies — can start immediately
- **Foundational (Phase 2)**: Depends on Setup completion — BLOCKS all user stories
- **US1 Backup (Phase 3)**: Depends on Foundational — first story to implement
- **US2 Restore (Phase 4)**: Depends on US1 (needs snapshots to restore from)
- **US3 Backward Compat (Phase 5)**: Depends on US1+US2 (validation of the branching pattern)
- **US4 Snapshot List (Phase 6)**: Depends on Foundational only — can run in parallel with US1/US2
- **US5 Remote Storage (Phase 7)**: Depends on Foundational — can run in parallel with US1/US2
- **Polish (Phase 8)**: Depends on all desired user stories being complete

### User Story Dependencies

- **US1 (P1 Backup)**: Can start after Foundational (Phase 2) — no dependencies on other stories
- **US2 (P1 Restore)**: Depends on US1 — needs a Plakar snapshot to restore from
- **US3 (P1 Backward Compat)**: Depends on US1+US2 — validates that branching didn't break defaults
- **US4 (P2 List Snapshots)**: Can start after Foundational — independent of US1/US2 at code level (but needs snapshots for testing)
- **US5 (P3 Remote Storage)**: Can start after Foundational — independent of other stories at code level

### Within Each User Story

- Models/types before services
- Services before CLI wiring
- Core implementation before validation
- Story complete before moving to next priority

### Parallel Opportunities

- **Phase 2**: T005 and T006 can run in parallel (different functions in same file)
- **Phase 3**: T010 (importer.go) can start in parallel with T012 (plakar_backup.go) scaffolding
- **Phase 6 + Phase 3/4**: US4 (snapshot listing) code can be written in parallel with US1/US2
- **Phase 7 + Phase 3/4**: US5 (S3 storage) dependency addition can be done in parallel with US1/US2
- **Phase 8**: T034, T035 can run in parallel (different files/concerns)

---

## Parallel Example: User Story 1

```bash
# Launch importer and backup flow scaffolding in parallel:
Task: "Create InfrahubImporter struct in src/internal/app/importer.go"
Task: "Create CreatePlakarBackup() scaffold in src/internal/app/plakar_backup.go"

# Then wire them together:
Task: "Implement Import() method connecting importer to backup flow"
Task: "Modify CreateBackup() to branch on BackendType in src/internal/app/backup.go"
```

---

## Implementation Strategy

### MVP First (User Story 1 Only)

1. Complete Phase 1: Setup (T001–T003)
2. Complete Phase 2: Foundational (T004–T009)
3. Complete Phase 3: User Story 1 — Backup (T010–T015)
4. **STOP and VALIDATE**: Create a Plakar backup, verify snapshot exists with correct metadata
5. Default `create` still produces tar.gz

### Incremental Delivery

1. Setup + Foundational → Foundation ready
2. Add US1 (Backup) → Test backup independently → **MVP!**
3. Add US2 (Restore) → Test full backup→restore cycle
4. Add US3 (Backward Compat) → Validate no regressions
5. Add US4 (Snapshot List) → Test listing with metadata
6. Add US5 (Remote Storage) → Test S3 repositories
7. Polish → Lint, encrypt support, end-to-end validation

### Parallel Team Strategy

With multiple developers:

1. Team completes Setup + Foundational together
2. Once Foundational is done:
   - Developer A: US1 (Backup) → US2 (Restore) → US3 (Backward Compat)
   - Developer B: US4 (Snapshot Listing) code → US5 (Remote Storage)
3. Developer B's work can be code-reviewed and tested once Developer A has snapshots to test against

---

## Notes

- [P] tasks = different files, no dependencies
- [Story] label maps task to specific user story for traceability
- Each user story should be independently completable and testable
- Commit after each task or logical group
- Stop at any checkpoint to validate story independently
- US2 (Restore) depends on US1 (Backup) since you need snapshots to restore
- US3 (Backward Compat) is primarily validation — the compatibility is achieved by design in US1/US2
- The `integration-fs` exporter is used for restore (D3) — no custom exporter needed
