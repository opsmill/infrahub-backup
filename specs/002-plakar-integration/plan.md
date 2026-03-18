# Implementation Plan: Plakar Integration for infrahub-backup

**Branch**: `002-plakar-integration` | **Date**: 2026-03-18 | **Spec**: [spec.md](./spec.md)
**Input**: Feature specification from `/specs/002-plakar-integration/spec.md`

## Summary

Embed Plakar's `kloset` library into infrahub-backup to provide content-addressable, deduplicated backup storage as an alternative to the existing tar.gz archive approach. The integration uses kloset's importer/exporter connector model: a custom `InfrahubImporter` produces Records from database dumps for backup, and restore uses kloset's fs exporter to extract snapshots to temp directories before applying existing restore logic. Repositories default to plaintext (unencrypted) with opt-in encryption. Archive-specific S3 flags are rejected when the Plakar backend is active.

## Technical Context

**Language/Version**: Go 1.25.0 (already in go.mod)
**Primary Dependencies**: `github.com/PlakarKorp/kloset` (core library), `github.com/PlakarKorp/integration-fs` (filesystem storage/importer/exporter), cobra, logrus, viper
**Storage**: Plakar repository (local filesystem via integration-fs, S3 via integration-s3)
**Testing**: `go test` (existing infrastructure, `make test`)
**Target Platform**: Linux (primary), Darwin (development)
**Project Type**: Single Go module with two binary entry points (`infrahub-backup`, `infrahub-taskmanager`)
**Performance Goals**: Backup/restore time comparable to tar.gz; deduplication should reduce storage by ≥50% on second backup of unchanged data (SC-001)
**Constraints**: Must not break existing tar.gz backup/restore workflow (FR-012/013/014); kloset dependency adds to binary size; Go 1.25.0 required by kloset
**Scale/Scope**: Single Infrahub instance backups; database dumps typically 100MB–10GB

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

Constitution is unconfigured (template placeholders only). No gates to enforce. **PASS**.

**Post-Phase 1 re-check**: No gates defined. **PASS**.

## Project Structure

### Documentation (this feature)

```text
specs/002-plakar-integration/
├── plan.md              # This file
├── research.md          # Phase 0 output (9 research decisions)
├── data-model.md        # Phase 1 output
├── quickstart.md        # Phase 1 output
├── contracts/           # Phase 1 output
│   └── cli-interface.md
├── checklists/
│   └── requirements.md  # Spec quality validation
└── tasks.md             # Phase 2 output (created by /speckit.tasks)
```

### Source Code (repository root)

```text
src/
├── cmd/
│   └── infrahub-backup/
│       └── main.go              # MODIFY: add --backend, --repo, --snapshot, --encrypt flags;
│                                #         add snapshots subcommand; add S3 flag validation
├── internal/
│   └── app/
│       ├── app.go               # MODIFY: add PlakarConfig and BackendType to Configuration
│       ├── backup.go            # MODIFY: branch on BackendType in CreateBackup()
│       ├── cli.go               # MODIFY: add shared Plakar flags (--backend, --repo)
│       ├── plakar.go            # NEW: kloset context init, repo open/create, snapshot helpers
│       ├── plakar_backup.go     # NEW: CreatePlakarBackup() — Plakar-backed backup flow
│       ├── plakar_restore.go    # NEW: RestorePlakarBackup() — Plakar-backed restore flow
│       ├── importer.go          # NEW: InfrahubImporter implementing importer.Importer
│       ├── snapshots.go         # NEW: ListSnapshots() for snapshots list command
│       ├── plakar_test.go       # NEW: unit tests for Plakar context/repo init
│       ├── importer_test.go     # NEW: unit tests for InfrahubImporter
│       └── snapshots_test.go    # NEW: unit tests for snapshot listing
└── ...

go.mod                          # MODIFY: add kloset, integration-fs dependencies
go.sum                          # MODIFY: updated automatically
```

**Structure Decision**: Extend the existing single-module structure. New Plakar functionality is isolated in dedicated files (`plakar*.go`, `importer.go`, `snapshots.go`) within the existing `app` package. The backup/restore entry points in `backup.go` branch based on `BackendType` to route to either tar.gz or Plakar flow. No new packages needed.

## Design Decisions

### D1: Backend Selection Pattern

The existing `CreateBackup()` and `RestoreBackup()` methods check `BackendType` early and delegate to `CreatePlakarBackup()` / `RestorePlakarBackup()` respectively. The existing code path remains completely untouched for the `tarball` backend. (FR-001, FR-012)

### D2: Importer Strategy — Dump-Then-Import

Database dumps are written to temp files first using existing `backupDatabase()` and `backupTaskManagerDB()` methods, then the custom `InfrahubImporter` reads them as Plakar `connectors.Record` entries. This reuses all existing dump logic without modification. (FR-004, Research R3)

### D3: Restore Strategy — Export-Then-Restore

For restore, use kloset's `integration-fs` exporter to extract the snapshot to a temp directory, then call the existing `restoreNeo4j()` / `restorePostgreSQL()` functions on that directory. This avoids duplicating restore logic and ensures parity with the archive restore path. (FR-006, FR-009, Research R4)

### D4: Repository Auto-Init

When `--repo` points to a non-existent or empty path, automatically create a new Plakar repository with plaintext configuration. On subsequent runs, the existing repository is opened. (FR-003, FR-019)

### D5: Metadata as Snapshot Tags + File

Infrahub backup metadata (version, components, neo4j edition, redaction status) is stored both as Plakar snapshot tags (enabling `snapshots list` without extraction) and as `backup_information.json` inside the snapshot (for compatibility with the existing restore metadata validation flow). (FR-005, FR-011)

### D6: Plaintext Default, Opt-In Encryption

Repositories are created in plaintext mode by default, consistent with the existing unencrypted tar.gz archives. An `--encrypt` flag enables passphrase-protected repositories. When encryption is enabled, the passphrase is read from stdin or an environment variable. (FR-019, FR-020, Research R8)

### D7: S3 Flag Conflict Rejection

When `--backend plakar` is active and any archive-specific S3 flag (`--s3-upload`, `--s3-bucket`, `--s3-prefix`, `--s3-endpoint`, `--s3-region`, `--s3-keep-local`) is also set, the command fails immediately with a clear error directing the operator to use `--repo s3://...` instead. (FR-021, Research R9)

### D8: kloset Context Initialization

A dedicated `plakar.go` file handles kloset context setup: `kcontext.KContext` with hostname, CWD, cache directory, logger, and `caching.Manager` (pebble backend). The cache directory defaults to `~/.cache/infrahub-backup/plakar/` and persists between runs for deduplication tracking. (Research R5)

## Complexity Tracking

No constitution violations to justify.

## Phase 1 Design Artifacts

| Artifact | Path | Description |
|----------|------|-------------|
| Research | [research.md](./research.md) | 9 decisions: architecture, connectors, data flows, CLI, encryption, S3 conflicts |
| Data Model | [data-model.md](./data-model.md) | Entities, relationships, state transitions |
| CLI Contract | [contracts/cli-interface.md](./contracts/cli-interface.md) | Flags, commands, error conditions, env vars |
| Quickstart | [quickstart.md](./quickstart.md) | User-facing usage examples |
| Spec Quality | [checklists/requirements.md](./checklists/requirements.md) | Specification validation checklist |
