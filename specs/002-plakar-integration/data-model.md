# Data Model: Plakar Integration for infrahub-backup

**Feature Branch**: `002-plakar-integration`
**Date**: 2026-03-18 (updated 2026-03-20)

## Entities

### PlakarConfig

Configuration for the Plakar backend, extending the existing `Configuration` struct.

| Field | Type | Description |
|-------|------|-------------|
| RepoPath | string | Plakar repository location (local path or URI like `s3://bucket/prefix`) |
| CacheDir | string | Local cache directory for Plakar dedup state (default: `~/.cache/infrahub-backup/plakar/`) |
| SnapshotID | string | Specific snapshot ID for restore (empty = latest) |
| BackupID | string | Specific backup-id tag for restore (empty = latest complete group) |
| Plaintext | bool | Repository is unencrypted (default: true, consistent with existing tar.gz behavior) |
| Encrypt | bool | Opt-in passphrase-based encryption for the repository (default: false) |

### BackendType

Enumeration for backup backend selection.

| Value | Description |
|-------|-------------|
| `tarball` | Default — existing tar.gz archive behavior |
| `plakar` | Plakar content-addressable storage with deduplication |

### StreamingImporter (implements `importer.Importer`)

Custom Plakar importer that produces a single Record from a streaming exec command. One importer instance per component.

| Method | Behavior |
|--------|----------|
| `Origin()` | Returns hostname of the Infrahub instance |
| `Type()` | Returns `"infrahub"` |
| `Root()` | Returns `"/"` |
| `Scan()` | Returns channel with a single ScanRecord whose data func starts exec and returns stdout pipe |
| `Close()` | Waits for exec command to finish, checks exit code |

Importer variants per component:
- **Neo4j Enterprise**: Exec runs `neo4j-admin database backup --compress=false --to-path=/tmp/x && tar cf - -C /tmp x`. Single ScanRecord at `/neo4j-backup.tar`.
- **Neo4j Community**: Exec runs `neo4j-admin database dump --to-path=/tmp/x && cat /tmp/x/<db>.dump`. Single ScanRecord at `/neo4j.dump`.
- **PostgreSQL**: Exec runs `pg_dump -Fc -Z0 -U user -d db`. Single ScanRecord at `/prefect.dump`.
- **Metadata**: No exec — wraps in-memory JSON bytes. Single ScanRecord at `/metadata/backup_information.json`.

### Snapshot Tags (metadata stored on each Plakar snapshot)

| Tag Key | Description |
|---------|-------------|
| `infrahub.backup-id` | Shared identifier grouping component snapshots from one backup run (timestamp format: `20260318_143000`) |
| `infrahub.component` | Component type: `neo4j`, `postgres`, or `metadata` |
| `infrahub.backup-status` | `complete` (set after all components succeed) or `incomplete` (set if any component fails) |
| `infrahub.version` | Infrahub version at time of backup |
| `infrahub.backup-tool-version` | infrahub-backup tool version |
| `infrahub.neo4j-edition` | `enterprise` or `community` |
| `infrahub.components` | Comma-separated list of components in this backup group: `neo4j,postgres,metadata` |
| `infrahub.redacted` | `true` if backup was redacted |

### BackupGroup (logical entity, derived from tags)

Represents a set of component snapshots from one backup run. Not a stored entity — assembled at query time by grouping snapshots with matching `infrahub.backup-id` tag.

| Field | Type | Description |
|-------|------|-------------|
| BackupID | string | Shared `infrahub.backup-id` tag value |
| Snapshots | []SnapshotInfo | Component snapshots in this group |
| Status | string | `complete` if all expected components present, `incomplete` otherwise |
| Timestamp | time.Time | Creation time of the earliest snapshot in the group |
| InfrahubVersion | string | From `infrahub.version` tag |
| Neo4jEdition | string | From `infrahub.neo4j-edition` tag |
| Components | []string | List of component types present |

## Relationships

```
Configuration (existing)
├── BackendType (new field)
└── PlakarConfig (new field, nil when backend != plakar)

InfrahubOps (existing)
├── CreateBackup() → branches on BackendType
│   ├── tarball → existing tar.gz flow (unchanged)
│   └── plakar → StreamingImporter per component → one snapshot each → grouped by backup-id
└── RestoreBackup() → branches on BackendType
    ├── tarball → existing extract + restore flow (unchanged)
    └── plakar → find backup group by tag → export each snapshot to temp → existing restore functions

PlakarConfig → kloset repository → kloset snapshots (multiple per backup)
```

## State Transitions

### Plakar Streaming Backup Lifecycle

```
[Init] → detect environment → create/open repo → generate backup-id

→ [Neo4j Component]
  → create streaming importer (exec → stdout pipe)
  → snapshot.Create → builder.Backup → builder.Close
  → tag: backup-id, component=neo4j, status=complete
  → on error: tag previous snapshots as incomplete → [Failed]

→ [PostgreSQL Component] (if not excluded)
  → create streaming importer (exec → stdout pipe)
  → snapshot.Create → builder.Backup → builder.Close
  → tag: backup-id, component=postgres, status=complete
  → on error: tag previous snapshots as incomplete → [Failed]

→ [Metadata Component]
  → create in-memory importer (JSON bytes)
  → snapshot.Create → builder.Backup → builder.Close
  → tag: backup-id, component=metadata, status=complete

→ [Done] all component snapshots tagged as complete
```

### Plakar Restore Lifecycle (unchanged — uses local temp)

```
[Init] → detect environment → open repo → find backup group by tag
  → for each component snapshot in group:
    → export to temp dir via fs exporter
  → stop services
  → restore Neo4j from temp → restore PostgreSQL from temp
  → restart services → cleanup temp → [Done]

On error → attempt service restart → cleanup temp → [Failed]
```
