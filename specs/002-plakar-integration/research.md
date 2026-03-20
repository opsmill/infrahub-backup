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

## R3: Backup Data Flow (Streaming Architecture)

**Decision**: Stream database dumps directly from container exec stdout into Plakar snapshots. Create one snapshot per component (Neo4j, PostgreSQL, metadata) grouped by a shared backup-id tag.

**Rationale**: The kloset importer interface supports streaming natively. `NewScanRecord` accepts a lazy `func() (io.ReadCloser, error)` that is only called when data is needed. This function can return an exec stdout pipe instead of a file handle.

The backup flow per component:
1. Initialize `kcontext.KContext` and open/create repository (once)
2. For each component (Neo4j, PostgreSQL, metadata):
   a. Create a dedicated importer that returns a single ScanRecord
   b. The ScanRecord's data function starts the exec command and returns stdout
   c. Create snapshot builder: `snapshot.Create(repo, ...)`
   d. Execute: `builder.Backup(imp, opts)` with backup-id and component tags
   e. Close builder (commits snapshot)

Streaming patterns per component:
- **Neo4j Enterprise**: `exec database neo4j-admin database backup --compress=false --to-path=/tmp/x && tar cf - -C /tmp x` → stdout
- **Neo4j Community**: `exec database neo4j-admin database dump --to-path=/tmp/x && cat /tmp/x/<db>.dump` → stdout
- **PostgreSQL**: `exec task-manager-db pg_dump -Fc -Z0 -U user -d db` → stdout (streams directly, no intermediate files on host)
- **Metadata**: Generated in-memory as JSON bytes, wrapped in `io.NopCloser(bytes.NewReader(...))`

**Alternatives considered**:
- Temp files then import (original R3): Works but defeats the purpose of streaming — duplicates data on local disk
- Single snapshot with all components: Harder to stream multiple exec commands into one importer; multi-snapshot is cleaner and enables partial recovery

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

## R10: Streaming Importer Interface Compatibility

**Decision**: The kloset importer interface natively supports streaming from exec stdout. No special flags or modifications needed.

**Rationale**: Research of kloset v1.0.13 source confirms:
- `importer.NewScanRecord()` accepts a `func() (io.ReadCloser, error)` wrapped in `LazyReader`
- The function is only called when `Read()` is first invoked on the record
- This means the closure can start an exec command and return its stdout pipe
- Multiple ScanRecords can be sent sequentially on the channel from different exec sources
- The `Scan()` method returns `<-chan *importer.ScanResult` — we control the goroutine
- `importer.Options` has `Stdin/Stdout/Stderr` fields (msgpack-excluded) available for implementations

No changes to kloset are required.

**Alternatives considered**:
- Custom streaming connector bypassing importer interface: Unnecessary — the standard interface already supports this pattern

## R11: Multi-Snapshot Per Backup and Tag-Based Querying

**Decision**: Create sequential snapshots per component using the same repository handle. Use `key=value` tags for grouping and client-side filtering.

**Rationale**:
- Sequential calls to `snapshot.Create()` / `builder.Backup()` / `builder.Close()` on the same repo work correctly. Each gets a unique `Identifier` via `objects.RandomMAC()`.
- Tags are stored as `[]string` in `snapshot.Header.Tags`, set via `BackupOptions.Tags`
- Tag convention: `infrahub.backup-id=<timestamp>`, `infrahub.component=neo4j|postgres|metadata`, `infrahub.backup-status=complete|incomplete`
- Querying: kloset has no built-in tag filter API. Must iterate `repo.ListSnapshots()`, load each snapshot, check `snap.Header.HasTag(tag)`. Headers are cached so repeated loads are fast.
- Grouping for listing: load all snapshots, parse tags into map, group by `infrahub.backup-id`

**Alternatives considered**:
- Single snapshot with all components: Can't easily stream multiple exec commands into one importer; also prevents independent component restore
- External metadata store (SQLite index): Over-engineering — snapshot count is small enough for linear scan

## R12: Neo4j Backup Compression Control

**Decision**: Use `--compress=false` for Enterprise Edition. Community Edition dump is already uncompressed.

**Rationale**:
- Enterprise `neo4j-admin database backup` supports `--compress[=true|false]` (default: true)
- Adding `--compress=false` to the existing backup command produces uncompressed backup artifacts
- Output format: still backup artifact files in a directory, just uncompressed
- Community `neo4j-admin database dump` has no `--compress` flag — the `.dump` file is not inherently compressed
- For streaming: Enterprise backup dir → `tar cf -` inside container → stdout; Community dump file → `cat` inside container → stdout

**Alternatives considered**:
- External decompression on host: Defeats streaming purpose
- Accept compressed dumps with reduced dedup: Undermines Plakar's core value proposition

## R13: PostgreSQL Dump Format for Streaming + Dedup

**Decision**: Use `-Fc -Z0` (custom format, no compression) instead of the originally planned `-Fd` (directory format).

**Rationale**: Research revealed that `-Fd` (directory format) **cannot stream to stdout** — it requires writing multiple files to a directory path. This makes it incompatible with the streaming architecture. `-Fc -Z0` is strictly better because:
- Streams to stdout directly (single binary stream)
- Internal per-table structure preserves table boundaries for Plakar's content-defined chunking
- `-Z0` disables compression so Plakar handles dedup on raw data
- Restores with `pg_restore` (not `psql`) — no change to restore path
- Simpler pipeline: `exec task-manager-db pg_dump -Fc -Z0 -U user -d db` → pipe to kloset importer

**Alternatives considered**:
- `-Fd` with tar wrapper: Can't stream to stdout, requires two-step (dump dir + tar); adds complexity
- `-Fp` (plain SQL): Streams to stdout but produces monolithic SQL; worse dedup than `-Fc` internal structure; requires `psql` instead of `pg_restore` for restore
- `-Fc` with default compression: Streams to stdout but compressed output defeats dedup
