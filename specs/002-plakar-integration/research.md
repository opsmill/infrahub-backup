# Research: Plakar Integration for infrahub-backup

**Feature Branch**: `002-plakar-integration`
**Date**: 2026-03-18

## R1: Plakar Architecture and Module Structure

**Decision**: Use `github.com/PlakarKorp/kloset` as the core library dependency, not the `plakar` CLI repo.

**Rationale**: Plakar has been decomposed into multiple Go modules:
- `github.com/PlakarKorp/kloset` — Core library: repository, snapshot, connectors, objects, caching, context
- `github.com/PlakarKorp/plakar` — CLI application wrapping kloset (subcommands, UI, plugin loading)
- `github.com/PlakarKorp/integration-fs` — Filesystem storage, importer, exporter (reference implementation)
- `github.com/PlakarKorp/integration-*` — Other backends (s3, grpc, docker, k8s)

The CLI repo imports kloset; we should do the same for library-level embedding.

**Alternatives considered**:
- Shelling out to `plakar` CLI: Rejected because it adds a runtime dependency, prevents tight integration, and loses type safety
- Out-of-process plugin via gRPC (plakar pkg system): Rejected as overkill; library embedding is simpler and more maintainable

## R2: Connector Interfaces (Importer/Exporter/Storage)

**Decision**: Implement a custom `importer.Importer` for backup and `exporter.Exporter` for restore. Use `integration-fs` storage backend initially.

**Rationale**: Plakar uses three connector types with clean interfaces:

### Importer Interface (for backup — data source)
```go
type Importer interface {
    Origin() string
    Type() string
    Root() string
    Flags() location.Flags
    Ping(context.Context) error
    Import(context.Context, chan<- *connectors.Record, <-chan *connectors.Result) error
    Close(context.Context) error
}
```
The `Import()` method sends `*connectors.Record` items into the records channel. Each Record contains pathname, FileInfo, optional symlink target, extended attributes, and a lazy `io.ReadCloser` for content.

### Exporter Interface (for restore — data destination)
```go
type Exporter interface {
    Origin() string
    Type() string
    Root() string
    Flags() location.Flags
    Ping(context.Context) error
    Export(context.Context, <-chan *connectors.Record, chan<- *connectors.Result) error
    Close(context.Context) error
}
```

### Storage Interface (repository backend)
```go
type Store interface {
    Create(context.Context, []byte) error
    Open(context.Context) ([]byte, error)
    Ping(context.Context) error
    Origin() string
    Type() string
    Root() string
    Flags() location.Flags
    Mode(context.Context) (Mode, error)
    Size(context.Context) (int64, error)
    List(context.Context, StorageResource) ([]objects.MAC, error)
    Put(context.Context, StorageResource, objects.MAC, io.Reader) (int64, error)
    Get(context.Context, StorageResource, objects.MAC, *Range) (io.ReadCloser, error)
    Delete(context.Context, StorageResource, objects.MAC) error
    Close(ctx context.Context) error
}
```

Registration uses URI scheme mapping: `importer.Register("infrahub", flags, factory)`.

**Alternatives considered**:
- Using fs importer directly (back up temp dir): Simpler but loses control over the data flow and requires writing full dumps to disk first, then re-reading them
- Single combined connector: Not possible; Plakar architecture separates import/export

## R3: Backup Data Flow

**Decision**: Generate database dumps to temp files, then produce Plakar Records pointing to those files.

**Rationale**: The backup flow is:
1. Initialize `kcontext.KContext` with hostname, CWD, cache dir, logger
2. Open or create repository: `storage.Open()` → `repository.NewNoRebuild()`
3. Create importer (our custom Infrahub importer)
4. Create source: `snapshot.NewSource(ctx, flags, importers...)`
5. Create snapshot builder: `snapshot.Create(repo, type, tmpDir, nilMac, options)`
6. Execute: `builder.Backup(source)` — importer sends Records through channel
7. Commit: `builder.Commit()` — persists snapshot

Our custom importer's `Import()` method will:
1. Execute Neo4j backup (via existing `backupDatabase()` to temp dir)
2. Execute PostgreSQL dump (via existing `backupTaskManagerDB()` to temp dir)
3. Generate metadata JSON
4. Send each file as a `connectors.Record` with pathname, FileInfo, and ReadCloser

**Alternatives considered**:
- Streaming directly from docker exec: More complex, requires FLAG_STREAM/FLAG_NEEDACK, higher risk of partial data
- Using fs importer on temp dir: Simpler but doesn't allow custom metadata or control over what's included

## R4: Restore Data Flow

**Decision**: Export snapshot to temp directory, then use existing restore functions.

**Rationale**: The restore flow is:
1. Open repository
2. Load snapshot: `snapshot.Load(repo, snapshotMAC)`
3. Create exporter (fs exporter to temp dir)
4. Export: `snapshot.Export(exporter, "/", options)` — writes files to temp dir
5. Use existing `restoreNeo4j()` and `restorePostgreSQL()` on extracted files

Using the fs exporter for restore is simpler than a custom exporter because the existing restore logic already operates on files in a directory.

**Alternatives considered**:
- Custom exporter that feeds directly to docker exec: Higher complexity, tighter coupling, harder to test
- Custom exporter that writes to specific paths: Same as fs exporter with extra code

## R5: Context and Cache Initialization

**Decision**: Create a dedicated `plakar.go` module in `src/internal/app/` that handles kloset context setup.

**Rationale**: Plakar requires:
- `kcontext.KContext` — with hostname, CWD, cache directory, logger, caching.Manager
- `caching.Manager` — with pebble (default) or sqlite backend for deduplication state
- Cache directory — persistent for deduplication across runs (use `~/.cache/infrahub-backup/plakar/` or configurable)

This is non-trivial but well-isolated initialization code.

**Alternatives considered**:
- Lazy initialization on first use: Risk of failure mid-operation; better to fail fast
- No caching (in-memory only): Loses deduplication tracking between runs

## R6: Go Version Compatibility

**Decision**: Compatible — project already uses Go 1.25.0.

**Rationale**: Both `go.mod` files specify `go 1.25.0`. No version conflict.

## R7: CLI Interface Design

**Decision**: Add `--backend` flag to select between `tarball` (default) and `plakar`, plus `--repo` flag for Plakar repository path. Add `snapshots` subcommand.

**Rationale**: This keeps backward compatibility while cleanly separating the two paths. The `--backend` pattern is extensible if more backends are added later.

CLI additions:
```
infrahub-backup create --backend plakar --repo /path/to/repo [existing flags]
infrahub-backup restore --backend plakar --repo /path/to/repo [--snapshot <id>] [existing flags]
infrahub-backup snapshots list --repo /path/to/repo
```

**Alternatives considered**:
- Separate `plakar` subcommand: More isolated but duplicates create/restore verb structure
- Config file only: Less discoverable; flags are more explicit for this use case

## R8: Repository Encryption Default

**Decision**: Plaintext (unencrypted) repositories by default. Encryption available as opt-in flag.

**Rationale**: The existing tar.gz backups are unencrypted. Defaulting to plaintext keeps operational consistency — operators who don't need encryption avoid passphrase management overhead. Encryption is offered as an opt-in (`--encrypt` flag) for operators who require encryption at rest, using Plakar's built-in passphrase-protected repository mode.

**Alternatives considered**:
- Encrypted by default: Rejected because it adds mandatory passphrase management; passphrase loss = unrecoverable backups. Too high a barrier for default behavior.
- No encryption support at all: Rejected because some operators need encryption at rest for compliance.

## R9: Archive S3 Flags vs. Plakar S3 Repository

**Decision**: Reject with clear error when archive-specific S3 flags (`--s3-upload`, `--s3-bucket`, etc.) are combined with the Plakar backend.

**Rationale**: The tool has two distinct S3 mechanisms — the existing archive upload (`--s3-upload` + `--s3-bucket`) and Plakar's native S3 repository (`--repo s3://...`). These are incompatible. Silently ignoring flags would confuse operators. An explicit error message directs them to the correct approach.

**Alternatives considered**:
- Silent ignore: Rejected because operators may assume their S3 upload is happening when it's not.
- Allow both (Plakar local + S3 upload): Rejected because it defeats the purpose of Plakar's native S3 support and creates redundant storage.
