# Data Model: Plakar Integration for infrahub-backup

**Feature Branch**: `002-plakar-integration`
**Date**: 2026-03-18

## Entities

### PlakarConfig

Configuration for the Plakar backend, extending the existing `Configuration` struct.

| Field | Type | Description |
|-------|------|-------------|
| RepoPath | string | Plakar repository location (local path or URI like `s3://bucket/prefix`) |
| CacheDir | string | Local cache directory for Plakar dedup state (default: `~/.cache/infrahub-backup/plakar/`) |
| SnapshotID | string | Specific snapshot ID for restore (empty = latest) |
| Plaintext | bool | Repository is unencrypted (default: true, consistent with existing tar.gz behavior) |
| Encrypt | bool | Opt-in passphrase-based encryption for the repository (default: false) |

### BackendType

Enumeration for backup backend selection.

| Value | Description |
|-------|-------------|
| `tarball` | Default — existing tar.gz archive behavior |
| `plakar` | Plakar content-addressable storage with deduplication |

### InfrahubImporter (implements `importer.Importer`)

Custom Plakar importer that produces Records from Infrahub database operations.

| Method | Behavior |
|--------|----------|
| `Origin()` | Returns hostname of the Infrahub instance |
| `Type()` | Returns `"infrahub"` |
| `Root()` | Returns `"/"` |
| `Flags()` | Returns `FLAG_STREAM` (single Import() call, sequential records) |
| `Ping()` | Verifies Infrahub environment is accessible |
| `Import()` | Executes database dumps, sends Records for each component |
| `Close()` | Cleans up temp files |

Records produced by Import():
- `/neo4j/` — Directory entry
- `/neo4j/<files>` — Neo4j backup files (directory for enterprise, .dump for community)
- `/taskmanager/prefect.dump` — PostgreSQL dump (when included)
- `/metadata/backup_information.json` — Backup metadata JSON

### Snapshot Tags (metadata stored in Plakar snapshot)

| Tag Key | Description |
|---------|-------------|
| `infrahub.version` | Infrahub version at time of backup |
| `infrahub.backup-tool-version` | infrahub-backup tool version |
| `infrahub.neo4j-edition` | `enterprise` or `community` |
| `infrahub.components` | Comma-separated list: `neo4j,task-manager-db` |
| `infrahub.redacted` | `true` if backup was redacted |

## Relationships

```
Configuration (existing)
├── BackendType (new field)
└── PlakarConfig (new field, nil when backend != plakar)

InfrahubOps (existing)
├── CreateBackup() → branches on BackendType
│   ├── tarball → existing tar.gz flow (unchanged)
│   └── plakar → InfrahubImporter → kloset snapshot
└── RestoreBackup() → branches on BackendType
    ├── tarball → existing extract + restore flow (unchanged)
    └── plakar → kloset snapshot.Export → fs exporter → existing restore functions

PlakarConfig → kloset repository → kloset snapshots
```

## State Transitions

### Plakar Backup Lifecycle

```
[Init] → detect environment → create/open repo → create importer
  → dump databases to temp → Import() sends Records
  → builder.Backup() → builder.Commit() → [Done]

On error at any stage → cleanup temp files, no snapshot committed → [Failed]
```

### Plakar Restore Lifecycle

```
[Init] → detect environment → open repo → load snapshot
  → fs exporter extracts to temp dir → stop services
  → restore Neo4j from temp → restore PostgreSQL from temp
  → restart services → cleanup temp → [Done]

On error → attempt service restart → cleanup temp → [Failed]
```
