# Implementation Plan: Plakar Integration with Streaming Backup

**Branch**: `002-plakar-integration` | **Date**: 2026-03-20 | **Spec**: [spec.md](spec.md)
**Input**: Feature specification from `/specs/002-plakar-integration/spec.md`

## Summary

Embed Plakar (kloset) as an opt-in backup engine for infrahub-backup, streaming database dumps directly from container exec stdout into deduplicated Plakar snapshots — bypassing local temp storage entirely. One snapshot per component (Neo4j, PostgreSQL, metadata) grouped by backup-id tags. Restore uses local temp (export from kloset → push to containers). Existing tar.gz behavior is unchanged.

## Technical Context

**Language/Version**: Go 1.25.0
**Primary Dependencies**: kloset v1.0.13 (Plakar core), integration-fs (storage), cobra, logrus, viper
**Storage**: Plakar repository (local filesystem or S3 via integration backends)
**Testing**: `go test` (make test, make test-coverage)
**Target Platform**: Linux (primary), Darwin, Windows (cross-compiled)
**Project Type**: Single project — two CLI binaries sharing internal app logic
**Performance Goals**: Streaming backup should be equal or faster than current file-copy approach; dedup should reduce storage by ≥50% on unchanged data
**Constraints**: Zero local temp files for Plakar backup path; backward compatibility with existing tar.gz flow
**Scale/Scope**: Typical Infrahub databases are 100MB–10GB; backup frequency ranges from hourly to daily

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

Constitution is unpopulated (template placeholders). No gates to evaluate. Proceeding.

## Project Structure

### Documentation (this feature)

```text
specs/002-plakar-integration/
├── plan.md              # This file
├── research.md          # Phase 0 output (R1-R13)
├── data-model.md        # Phase 1 output
├── quickstart.md        # Phase 1 output
├── contracts/
│   └── cli-interface.md # Phase 1 output
└── tasks.md             # Phase 2 output (created by /speckit.tasks)
```

### Source Code (repository root)

```text
src/
├── cmd/
│   ├── infrahub-backup/
│   │   └── main.go          # Root command, create/restore/snapshots subcommands
│   └── infrahub-taskmanager/
│       └── main.go          # Unchanged
├── internal/
│   └── app/
│       ├── app.go           # InfrahubOps struct, config, backend branching
│       ├── cli.go           # Shared CLI config (--backend, --repo flags)
│       ├── backup.go        # CreateBackup — branches on backend
│       ├── backup_neo4j.go  # Neo4j backup (add --compress=false for Plakar path)
│       ├── backup_taskmanager.go  # PostgreSQL backup (add -Fc -Z0 streaming for Plakar path)
│       ├── plakar.go        # kloset context init, repo open/create (existing)
│       ├── plakar_backup.go # CreatePlakarBackup — refactor to streaming multi-snapshot
│       ├── plakar_restore.go # RestorePlakar — refactor for backup-group-based restore
│       ├── importer.go      # StreamingImporter — refactor from file-walking to exec-stdout
│       ├── snapshots.go     # ListSnapshots — refactor for backup-group display
│       └── ...
└── go.mod / go.sum
```

**Structure Decision**: Existing single-project layout. New files already exist from initial Plakar integration. Primary changes are refactoring `plakar_backup.go`, `importer.go`, and `snapshots.go` for streaming and multi-snapshot architecture.

## Phase 0: Research (Complete)

All unknowns resolved in [research.md](research.md):

| ID | Topic | Decision |
|----|-------|----------|
| R1 | Module structure | Use kloset as core library |
| R2 | Connector interfaces | Custom importer for backup, fs exporter for restore |
| R3 | Backup data flow | Stream exec stdout → lazy ScanRecord → one snapshot per component |
| R4 | Restore data flow | Export to local temp → existing restore functions |
| R5 | Context/cache init | Dedicated plakar.go module |
| R6 | Go version | Compatible (1.25.0) |
| R7 | CLI design | --backend/--repo flags + snapshots subcommand |
| R8 | Encryption default | Plaintext default, opt-in --encrypt |
| R9 | S3 flag conflict | Reject with clear error |
| R10 | Streaming importer | kloset natively supports lazy ReadCloser from exec stdout |
| R11 | Multi-snapshot + tags | Sequential snapshot.Create per component, client-side tag filtering |
| R12 | Neo4j compression | --compress=false for Enterprise, Community already uncompressed |
| R13 | PostgreSQL format | -Fc -Z0 (custom format, no compression) — streams to stdout, pg_restore compatible |

## Phase 1: Design (Complete)

### Data Model

See [data-model.md](data-model.md). Key entities:
- **PlakarConfig**: Extended with BackupID field for restore
- **StreamingImporter**: One per component, lazy exec stdout pipe
- **Snapshot Tags**: `infrahub.backup-id`, `infrahub.component`, `infrahub.backup-status`
- **BackupGroup**: Logical grouping of component snapshots (derived from tags at query time)

### Contracts

See [contracts/cli-interface.md](contracts/cli-interface.md). Key changes from initial plan:
- `create` now produces multiple snapshots (one per component)
- `restore` accepts `--backup-id` (group) or `--snapshot` (single component)
- `snapshots list` groups output by backup-id with status (complete/incomplete)
- New env vars: `INFRAHUB_BACKUP_ID`

### Quickstart

See [quickstart.md](quickstart.md). Updated for backup-id-based restore UX.

## Phase 2: Implementation Approach

### Work Streams

**Stream A: Streaming Infrastructure** (P1 — enables everything else)
1. Refactor `importer.go`: Replace file-walking `InfrahubImporter` with `StreamingImporter` that accepts a command factory producing exec stdout
2. Add `ExecStream()` method to `CommandExecutor` — returns `io.ReadCloser` (stdout pipe) instead of captured string output
3. Unit test: StreamingImporter with mock exec producing known data

**Stream B: Multi-Snapshot Backup** (P1 — depends on Stream A)
1. Refactor `plakar_backup.go`: Loop over components (neo4j, postgres, metadata), create one snapshot per component with shared backup-id tag
2. Add `infrahub.component` and `infrahub.backup-id` tags to `buildSnapshotTags()`
3. Handle partial failure: if component N fails, tag previous snapshots as incomplete
4. Integration test: backup produces 3 snapshots with matching backup-id

**Stream C: Uncompressed Dumps** (P2 — independent)
1. Neo4j Enterprise: add `--compress=false` to backup command when backend=plakar
2. PostgreSQL: switch from `-Fc` to `-Fc -Z0` when backend=plakar, stream to stdout (no `-f` flag)
3. Neo4j Community: dump to file inside container, then `cat` to stream out
4. Guard: only apply format changes when `config.Backend == BackendPlakar`

**Stream D: Backup Group Restore** (P1 — depends on Stream B)
1. Refactor `plakar_restore.go`: find backup group by `infrahub.backup-id` tag, export each component snapshot to temp dir
2. Add `--backup-id` flag and `INFRAHUB_BACKUP_ID` env var
3. Default to latest complete group when no ID specified
4. Warn on incomplete groups, require `--force`
5. Support single-component restore via `--snapshot`

**Stream E: Snapshot Listing** (P2 — depends on Stream B)
1. Refactor `snapshots.go`: group snapshots by `infrahub.backup-id`, show status
2. Update text and JSON output formats per contract

**Stream F: Backward Compatibility Validation** (P1 — parallel)
1. Verify tar.gz backup/restore paths are untouched
2. Verify `--s3-upload` + `--backend plakar` rejection
3. Verify all existing flags still work

### Dependency Graph

```
Stream A (StreamingImporter) ──→ Stream B (Multi-Snapshot) ──→ Stream D (Group Restore)
                                       │                              │
                                       └──→ Stream E (Listing) ◄──────┘
Stream C (Uncompressed Dumps) ─── parallel with A/B
Stream F (Backward Compat) ─── parallel with everything
```

### Key Risks

| Risk | Mitigation |
|------|------------|
| Exec stdout pipe breaks mid-stream | kloset's builder.Backup() propagates io.Read errors; detect and tag as incomplete |
| Large uncompressed dumps exceed container memory | Neo4j writes to temp dir first (then tar-streamed); PostgreSQL streams from pg_dump (constant memory) |
| Concurrent snapshot creation on same repo | kloset handles locking internally; test concurrent access |
| Tag-based querying is O(n) on snapshot count | Acceptable — typical repos have <1000 snapshots; add warning if >500 |

## Complexity Tracking

No constitution violations to justify — constitution is unpopulated.
