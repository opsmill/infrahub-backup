# CLI Interface Contract: Plakar Integration

## New Global Flags (on root command)

| Flag | Type | Default | Env Var | Description |
|------|------|---------|---------|-------------|
| `--backend` | string | `tarball` | `INFRAHUB_BACKEND` | Backup backend: `tarball` or `plakar` |
| `--repo` | string | (empty) | `INFRAHUB_REPO` | Plakar repository path or URI (required when backend=plakar) |

## Modified Commands

### `infrahub-backup create`

When `--backend plakar`:
- Requires `--repo` flag
- Creates one Plakar snapshot per component (Neo4j, PostgreSQL, metadata) grouped by a shared backup-id tag
- Streams database dumps directly from container exec stdout into kloset (no local temp files)
- Neo4j Enterprise: uncompressed backup (`--compress=false`) tar-streamed
- Neo4j Community: dump file streamed via exec after creation
- PostgreSQL: custom format without compression (`-Fc -Z0`) streamed to stdout
- All existing flags (`--force`, `--neo4jmetadata`, `--exclude-taskmanager`, `--redact`) continue to work
- `--s3-upload`, `--s3-keep-local`, `--s3-bucket`, `--s3-prefix`, `--s3-endpoint`, `--s3-region` MUST be rejected with an error directing the operator to use `--repo s3://...` instead
- `--sleep` continues to work
- Optional: `--encrypt` flag enables passphrase-protected repository (prompts for passphrase on create; requires passphrase on subsequent access)
- If a component snapshot fails, previously completed component snapshots are kept and the backup group is tagged as incomplete

### `infrahub-backup restore`

When `--backend plakar`:
- First positional argument (`<backup-file>`) is NOT required
- `--repo` flag is required
- New flag: `--backup-id <id>` (optional, defaults to latest complete group)
- New flag: `--snapshot <id>` (optional, restores a single component snapshot for partial recovery)
- If only `--backup-id` is provided, restores all components in that group
- If only `--snapshot` is provided, restores that single component
- If neither is provided, restores the most recent complete backup group
- If only incomplete groups exist, warns and requires `--force` to proceed
- Restore uses local temp (exports from kloset to temp dir, then pushes into containers)
- All existing flags (`--exclude-taskmanager`, `--migrate-format`, `--sleep`) continue to work

When `--backend tarball` (default):
- Behavior is completely unchanged
- First positional argument (`<backup-file>`) is required

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--backup-id` | string | (empty) | Plakar backup group ID to restore (latest complete if empty) |
| `--snapshot` | string | (empty) | Plakar snapshot ID for single-component restore |

## New Commands

### `infrahub-backup snapshots list`

Lists all backup groups in a Plakar repository.

| Flag | Type | Required | Description |
|------|------|----------|-------------|
| `--repo` | string | Yes | Plakar repository path or URI |

**Output format** (text, default):
```
BACKUP ID            DATE                  STATUS      INFRAHUB VERSION  NEO4J EDITION  COMPONENTS
20260318_143000      2026-03-18 14:30:00   complete    1.2.0            enterprise     neo4j, postgres, metadata
20260317_020000      2026-03-17 02:00:00   incomplete  1.2.0            enterprise     neo4j, metadata
```

**Output format** (json, when `--log-format json`):
```json
[
  {
    "backup_id": "20260318_143000",
    "date": "2026-03-18T14:30:00Z",
    "status": "complete",
    "infrahub_version": "1.2.0",
    "neo4j_edition": "enterprise",
    "components": ["neo4j", "postgres", "metadata"],
    "snapshots": [
      {"id": "a3f2b1c8", "component": "neo4j"},
      {"id": "b4e3c2d9", "component": "postgres"},
      {"id": "c5f4d3ea", "component": "metadata"}
    ]
  }
]
```

## Error Conditions

| Condition | Exit Code | Error Message |
|-----------|-----------|---------------|
| `--backend plakar` without `--repo` | 1 | `--repo is required when using plakar backend` |
| `--repo` path doesn't exist and can't be created | 1 | `failed to initialize plakar repository: <reason>` |
| `--snapshot` ID not found | 1 | `snapshot not found: <id>` |
| `--backup-id` not found | 1 | `backup group not found: <id>` |
| Restore of incomplete group without `--force` | 1 | `backup group <id> is incomplete (missing: postgres); use --force to restore available components` |
| Plakar repository corrupted | 1 | `failed to open plakar repository: <reason>` |
| `--backend` is unknown value | 1 | `unknown backend: <value>, expected 'tarball' or 'plakar'` |
| `--s3-upload`/`--s3-bucket` with `--backend plakar` | 1 | `--s3-upload and related S3 flags cannot be used with plakar backend; use --repo s3://... instead` |
| Exec stream interrupted during backup | 1 | `streaming backup failed for <component>: <reason>` |

## Environment Variables

All new flags follow the existing `INFRAHUB_` prefix pattern:

| Variable | Maps to Flag |
|----------|-------------|
| `INFRAHUB_BACKEND` | `--backend` |
| `INFRAHUB_REPO` | `--repo` |
| `INFRAHUB_BACKUP_ID` | `--backup-id` |
| `INFRAHUB_SNAPSHOT` | `--snapshot` |
