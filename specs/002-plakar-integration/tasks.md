# Tasks: Plakar Integration with Streaming Backup

**Input**: Design documents from `/specs/002-plakar-integration/`
**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/cli-interface.md

**Organization**: Tasks grouped by user story. US1 (Backup) and US6 (Streaming) are combined since streaming IS the backup mechanism.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel (different files, no dependencies)
- **[Story]**: Which user story (US1, US2, etc.)
- Exact file paths included

---

## Phase 1: Setup

**Purpose**: Configuration and shared infrastructure changes needed before any user story work

- [X] T001 Add `BackupID` field to `PlakarConfig` struct in `src/internal/app/app.go`
- [X] T002 Add `--backup-id` flag and `INFRAHUB_BACKUP_ID` env var to CLI config in `src/internal/app/cli.go`
- [X] T003 Add `ExecStreamPipe()` method to `EnvironmentBackend` interface and `InfrahubOps` — returns `(io.ReadCloser, func() error, error)` (stdout pipe, wait func, error) instead of captured string. Implement in `src/internal/app/app.go`, `src/internal/app/environment_docker.go`, `src/internal/app/environment_kubernetes.go`

**Checkpoint**: Shared infrastructure ready — user story implementation can begin

---

## Phase 2: US1+US6 - Streaming Multi-Snapshot Backup (P1) MVP

**Goal**: Plakar backups stream database dumps directly from container exec stdout into kloset, producing one snapshot per component grouped by backup-id tag. Zero local temp files.

**Independent Test**: Run `infrahub-backup create --backend plakar --repo /tmp/test-repo` and verify: (1) 3 snapshots created with matching backup-id tag, (2) no local temp directory created for dumps, (3) each snapshot tagged with correct component type.

### Implementation

- [X] T004 [US1] Refactor `StreamingImporter` in `src/internal/app/importer.go` — replace file-walking `Scan()` with a constructor that accepts a pathname, FileInfo, and a `func() (io.ReadCloser, error)` data factory. `Scan()` returns a channel with a single `ScanResult` whose lazy reader starts the exec pipe on first Read.

- [X] T005 [US1] Add `infrahub.backup-id`, `infrahub.component`, and `infrahub.backup-status` tag keys to `buildSnapshotTags()` in `src/internal/app/plakar_backup.go`. Update tag builder to accept component type and backup-id parameters.

- [X] T006 [US1] Refactor `CreatePlakarBackup()` in `src/internal/app/plakar_backup.go` — replace single-snapshot flow with a loop over components:
  1. Generate backup-id timestamp
  2. For Neo4j: create `StreamingImporter` with exec pipe factory (tar of backup dir for Enterprise, cat of dump for Community)
  3. For PostgreSQL (if not excluded): create `StreamingImporter` with exec pipe factory (`pg_dump -Fc -Z0` to stdout)
  4. For metadata: create `StreamingImporter` with in-memory JSON bytes factory
  5. For each component: `snapshot.Create()` → `builder.Backup(imp, opts)` → `builder.Close()`
  6. Tag all component snapshots with shared backup-id

- [X] T007 [US1] Implement partial failure handling in `CreatePlakarBackup()` in `src/internal/app/plakar_backup.go` — if component N fails, log the error, tag previously completed component snapshots with `infrahub.backup-status=incomplete`, and return the error. Do not delete successful snapshots.

- [X] T008 [US1] Create Neo4j Enterprise streaming backup helper in `src/internal/app/backup_neo4j.go` — add `backupNeo4jEnterpriseStream()` that returns a `func() (io.ReadCloser, error)` factory. The factory calls `ExecStreamPipe("database", ["sh", "-c", "neo4j-admin database backup --expand-commands --include-metadata=... --compress=false --to-path=/tmp/infrahubops <db> && tar cf - -C /tmp infrahubops"])` and returns the stdout pipe.

- [X] T009 [US1] Create Neo4j Community streaming backup helper in `src/internal/app/backup_neo4j.go` — add `backupNeo4jCommunityStream()` that returns a factory. The factory runs the dump command via `Exec()` (writes file in container), then calls `ExecStreamPipe("database", ["cat", "/tmp/infrahubops/<db>.dump"])` and returns the stdout pipe.

- [X] T010 [US1] Create PostgreSQL streaming backup helper in `src/internal/app/backup_taskmanager.go` — add `backupTaskManagerDBStream()` that returns a factory. The factory calls `ExecStreamPipe("task-manager-db", ["pg_dump", "-Fc", "-Z0", "-h", "localhost", "-U", user, "-d", db])` with `PGPASSWORD` env and returns the stdout pipe.

- [X] T011 [US1] Create metadata streaming helper in `src/internal/app/plakar_backup.go` — add `metadataStreamFactory()` that generates the backup metadata JSON in memory and returns a `func() (io.ReadCloser, error)` wrapping `io.NopCloser(bytes.NewReader(jsonBytes))`.

- [X] T012 [US1] Remove local temp directory creation from `CreatePlakarBackup()` in `src/internal/app/plakar_backup.go` — delete `os.MkdirTemp`, `os.MkdirAll(backupDir)`, and `os.RemoveAll(workDir)`. The streaming path should not touch the local filesystem for backup data.

- [X] T013 [US1] Update `src/cmd/infrahub-backup/main.go` to wire `--backup-id` flag to `PlakarConfig.BackupID` for the restore command.

**Checkpoint**: Plakar backups now stream directly, produce multi-snapshot groups. Verify with `make build && bin/infrahub-backup create --backend plakar --repo /tmp/test`.

---

## Phase 3: US2 - Restore from Backup Group (P1)

**Goal**: Restore all components from a backup group identified by backup-id tag. Default to latest complete group. Support single-component restore via --snapshot.

**Independent Test**: Create a Plakar backup, then run `infrahub-backup restore --backend plakar --repo /tmp/test-repo` and verify databases are restored. Also test `--backup-id` and `--snapshot` flags.

### Implementation

- [X] T014 [US2] Add `findBackupGroup()` function in `src/internal/app/snapshots.go` — given a repo and optional backup-id, find all component snapshots in the group. If no backup-id specified, find the most recent complete group. Return a `BackupGroup` struct with snapshot MACs, component types, status.

- [X] T015 [US2] Add `findLatestCompleteGroup()` helper in `src/internal/app/snapshots.go` — iterate all snapshots, group by `infrahub.backup-id` tag, sort by timestamp, return the newest group where all expected components are present. If only incomplete groups exist, return the newest incomplete group with a warning flag.

- [X] T016 [US2] Refactor `RestorePlakar()` in `src/internal/app/plakar_restore.go` — replace single-snapshot restore with backup-group restore:
  1. If `--snapshot` is provided: restore single component (existing flow)
  2. If `--backup-id` is provided: call `findBackupGroup()`, export each component snapshot to temp dir, run restore for each
  3. If neither: call `findLatestCompleteGroup()`, warn if incomplete and require `--force`
  4. Restore uses local temp (FR-026a) — export from kloset → temp → push to containers

- [X] T017 [US2] Add incomplete group warning and `--force` gate in `src/internal/app/plakar_restore.go` — if the selected backup group has `infrahub.backup-status=incomplete`, log a warning listing missing components and return an error unless `--force` is set.

- [X] T018 [US2] Add error message with available backup groups when backup-id not found in `src/internal/app/plakar_restore.go` — list available groups with their backup-id, date, and status.

**Checkpoint**: Full backup→restore cycle works with backup groups. Test: create backup, restore latest, verify data integrity.

---

## Phase 4: US3 - Backward Compatibility (P1)

**Goal**: Verify existing tar.gz flows are unaffected. Validate S3 flag rejection with Plakar backend.

**Independent Test**: Run `infrahub-backup create` (no --backend) and verify tar.gz output. Run with `--backend plakar --s3-upload` and verify clear error.

### Implementation

- [X] T019 [P] [US3] Add S3-flag-with-Plakar rejection in `src/cmd/infrahub-backup/main.go` — in the `create` command's PreRunE, check if `backend=plakar` and any of `--s3-upload`, `--s3-bucket`, etc. are set; return error per contract: `--s3-upload and related S3 flags cannot be used with plakar backend; use --repo s3://... instead`

- [X] T020 [P] [US3] Verify tarball backup path is unchanged in `src/internal/app/backup.go` — ensure `CreateBackup()` still calls existing tar.gz flow when `config.Backend != BackendPlakar`. No changes needed if the branching `if iops.config.Backend == BackendPlakar` at line 18 is preserved.

- [X] T021 [US3] Verify tarball restore path is unchanged in `src/cmd/infrahub-backup/main.go` — ensure restore command still requires positional `<backup-file>` argument when backend is tarball, and uses Plakar flow only when backend is plakar.

**Checkpoint**: Existing tar.gz workflows verified. S3 flag conflict validated.

---

## Phase 5: US7 - Uncompressed Dumps for Dedup (P2)

**Goal**: Neo4j Enterprise uses `--compress=false` and PostgreSQL uses `-Fc -Z0` when Plakar backend is selected. Legacy path unchanged.

**Independent Test**: Run two Plakar backups with minimal data change, check Plakar repo grows by <20% of a full dump.

### Implementation

- [X] T022 [P] [US7] Add `--compress=false` to Neo4j Enterprise backup command in `src/internal/app/backup_neo4j.go` — in the streaming helper `backupNeo4jEnterpriseStream()` (created in T008), the command already uses `--compress=false`. If the non-streaming Enterprise backup path still exists for legacy, guard compression flag with `if config.Backend == BackendPlakar`.

- [X] T023 [P] [US7] Verify PostgreSQL `-Fc -Z0` in `src/internal/app/backup_taskmanager.go` — the streaming helper `backupTaskManagerDBStream()` (created in T010) already uses `-Fc -Z0`. Verify the non-streaming legacy path preserves `-Fc` without `-Z0`.

- [X] T024 [US7] Guard all dump format changes with backend check — review `src/internal/app/backup_neo4j.go` and `src/internal/app/backup_taskmanager.go` to ensure uncompressed flags are ONLY applied when `config.Backend == BackendPlakar`. Legacy tarball path MUST use original compressed formats.

**Checkpoint**: Uncompressed dumps verified for Plakar path, compressed preserved for legacy.

---

## Phase 6: US4 - List and Inspect Snapshots (P2)

**Goal**: `infrahub-backup snapshots list` groups snapshots by backup-id with status, components, and metadata.

**Independent Test**: Create multiple backups, run `snapshots list`, verify output shows grouped backup-ids with correct metadata and status.

### Implementation

- [X] T025 [US4] Refactor `ListSnapshots()` in `src/internal/app/snapshots.go` — replace flat snapshot list with grouped output:
  1. Iterate all snapshots, parse tags into map
  2. Group by `infrahub.backup-id` tag
  3. For each group: determine status (complete/incomplete), collect components
  4. Sort groups by timestamp descending
  5. Return `[]BackupGroupInfo` struct

- [X] T026 [US4] Add `BackupGroupInfo` struct in `src/internal/app/snapshots.go` — fields: BackupID, Timestamp, Status, InfrahubVersion, Neo4jEdition, Components []string, Snapshots []SnapshotInfo (id + component).

- [X] T027 [US4] Update text output format in `src/cmd/infrahub-backup/main.go` — format per contract: `BACKUP ID | DATE | STATUS | INFRAHUB VERSION | NEO4J EDITION | COMPONENTS`. Handle empty repo with "No backups found" message.

- [X] T028 [US4] Update JSON output format in `src/cmd/infrahub-backup/main.go` — when `--log-format json`, output array of backup group objects per contract schema including nested snapshots array.

**Checkpoint**: Snapshot listing shows grouped backup-ids with correct metadata.

---

## Phase 7: US5 - Remote Repository Storage (P3)

**Goal**: S3-compatible storage as Plakar repository backend via `--repo s3://...`.

**Independent Test**: Run backup with `--repo s3://bucket/prefix` and verify snapshot stored in S3. Restore from S3 and verify.

### Implementation

- [X] T029 [US5] Add `integration-s3` dependency to `src/go.mod` — add `github.com/PlakarKorp/integration-s3` and run `go mod tidy`.

- [X] T030 [US5] Register S3 storage backend in `src/internal/app/plakar.go` — import integration-s3 and register the S3 storage factory so `s3://` URIs are recognized by kloset's connector resolution.

- [X] T031 [US5] Handle S3 credentials and endpoint configuration in `src/internal/app/plakar.go` — parse `s3://` URI from `--repo`, configure AWS credentials from environment (standard AWS SDK env vars or explicit flags if needed).

- [X] T032 [US5] Verify backup and restore work with S3 repository in `src/internal/app/plakar_backup.go` and `src/internal/app/plakar_restore.go` — the streaming backup and group restore flows should work transparently once S3 storage is registered.

**Checkpoint**: S3-backed Plakar backups work end-to-end.

---

## Phase 8: Polish & Cross-Cutting Concerns

**Purpose**: Cleanup, edge case handling, and validation

- [X] T033 Add stream interruption error handling in `src/internal/app/importer.go` — if the exec stdout pipe returns an error mid-stream, ensure the error propagates cleanly through the ScanRecord's ReadCloser and the snapshot builder fails without committing.

- [X] T034 Add configurable stream timeout — if `ExecStreamPipe()` produces no data for a configurable duration, cancel the exec and fail the component backup. Add timeout parameter to streaming helpers in `src/internal/app/backup_neo4j.go` and `src/internal/app/backup_taskmanager.go`.

- [X] T035 [P] Clean up old file-walking importer code in `src/internal/app/importer.go` — remove `InfrahubImporter.tempDir` field, `filepath.Walk` logic, and `emptyReader` struct if no longer used.

- [X] T036 [P] Run `make lint` and `make vet` — fix any issues introduced by refactoring.

- [X] T037 Run `make test` — verify all existing tests pass.

- [ ] T038 Run quickstart.md validation — manually verify the commands in `specs/002-plakar-integration/quickstart.md` work as documented. (Requires running Infrahub instance — manual testing)

---

## Dependencies & Execution Order

### Phase Dependencies

- **Phase 1 (Setup)**: No dependencies — start immediately
- **Phase 2 (US1+US6 Backup)**: Depends on Phase 1 (T003 ExecStreamPipe needed)
- **Phase 3 (US2 Restore)**: Depends on Phase 2 (needs multi-snapshot backups to exist)
- **Phase 4 (US3 Compat)**: Can run in parallel with Phase 2 (independent files)
- **Phase 5 (US7 Dedup)**: Can run in parallel with Phase 2 (verifies format flags)
- **Phase 6 (US4 Listing)**: Depends on Phase 2 (needs backup-id tags)
- **Phase 7 (US5 S3)**: Depends on Phase 2 (needs streaming backup working)
- **Phase 8 (Polish)**: Depends on all prior phases

### User Story Dependencies

- **US1+US6 (Backup+Streaming)**: Foundational — all other stories depend on this
- **US2 (Restore)**: Depends on US1 (needs backup groups to restore from)
- **US3 (Backward Compat)**: Independent — can run in parallel
- **US4 (Listing)**: Depends on US1 (needs backup-id tagged snapshots)
- **US5 (S3)**: Depends on US1 (extends storage backend)
- **US7 (Dedup)**: Mostly independent — verifies format flags set in US1

### Parallel Opportunities

- T019, T020 (US3) can run in parallel with Phase 2
- T022, T023 (US7) can run in parallel with Phase 2
- T025, T026 (US4) can run in parallel within Phase 6
- T029, T030 (US5) can run in parallel within Phase 7
- T033–T036 (Polish) marked [P] can run in parallel

---

## Parallel Example: Phase 2 (US1+US6)

```
# Sequential within Phase 2 (dependencies):
T004 (StreamingImporter) → T006 (CreatePlakarBackup refactor)
T005 (Tag builder) → T006
T008, T009, T010, T011 (stream helpers) → T006
T006 → T007 (partial failure) → T012 (remove temp)

# Can overlap with Phase 2:
T019, T020 (US3 backward compat checks)
T022, T023 (US7 format verification)
```

---

## Implementation Strategy

### MVP First (Phase 1 + Phase 2 Only)

1. Complete Phase 1: Setup (T001–T003)
2. Complete Phase 2: US1+US6 Streaming Backup (T004–T013)
3. **STOP and VALIDATE**: Create a backup, verify 3 snapshots, no local temp files
4. This alone delivers the core value: streaming dedup backups

### Incremental Delivery

1. Phase 1 + 2 → Streaming backup works (MVP)
2. Add Phase 3 (US2) → Full backup/restore cycle
3. Add Phase 4 (US3) → Backward compat verified
4. Add Phase 5 (US7) → Dedup optimized
5. Add Phase 6 (US4) → Operator UX (listing)
6. Add Phase 7 (US5) → S3 remote storage
7. Phase 8 → Polish

---

## Notes

- [P] tasks = different files, no dependencies on incomplete tasks
- [Story] label maps task to specific user story
- Existing code already has initial Plakar integration — tasks focus on the streaming + multi-snapshot refactoring delta
- `ExecStream()` already exists but returns `(string, error)` — T003 adds `ExecStreamPipe()` returning `(io.ReadCloser, func() error, error)`
- Commit after each task or logical group
