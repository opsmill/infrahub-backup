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
- Creates a Plakar snapshot instead of tar.gz archive
- All existing flags (`--force`, `--neo4jmetadata`, `--exclude-taskmanager`, `--redact`) continue to work
- `--s3-upload`, `--s3-keep-local`, `--s3-bucket`, `--s3-prefix`, `--s3-endpoint`, `--s3-region` MUST be rejected with an error directing the operator to use `--repo s3://...` instead
- `--sleep` continues to work
- Optional: `--encrypt` flag enables passphrase-protected repository (prompts for passphrase on create; requires passphrase on subsequent access)

### `infrahub-backup restore`

When `--backend plakar`:
- First positional argument (`<backup-file>`) is NOT required
- `--repo` flag is required
- New flag: `--snapshot <id>` (optional, defaults to latest)
- All existing flags (`--exclude-taskmanager`, `--migrate-format`, `--sleep`) continue to work

When `--backend tarball` (default):
- Behavior is completely unchanged
- First positional argument (`<backup-file>`) is required

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--snapshot` | string | (empty) | Plakar snapshot ID to restore (latest if empty) |

## New Commands

### `infrahub-backup snapshots list`

Lists all snapshots in a Plakar repository.

| Flag | Type | Required | Description |
|------|------|----------|-------------|
| `--repo` | string | Yes | Plakar repository path or URI |

**Output format** (text, default):
```
SNAPSHOT ID    DATE                  INFRAHUB VERSION  NEO4J EDITION  COMPONENTS
a3f2b1c8      2026-03-18 14:30:00   1.2.0            enterprise     neo4j, task-manager-db
e7d9a4f1      2026-03-17 02:00:00   1.2.0            enterprise     neo4j, task-manager-db
```

**Output format** (json, when `--log-format json`):
```json
[
  {
    "snapshot_id": "a3f2b1c8",
    "date": "2026-03-18T14:30:00Z",
    "infrahub_version": "1.2.0",
    "neo4j_edition": "enterprise",
    "components": ["neo4j", "task-manager-db"]
  }
]
```

## Error Conditions

| Condition | Exit Code | Error Message |
|-----------|-----------|---------------|
| `--backend plakar` without `--repo` | 1 | `--repo is required when using plakar backend` |
| `--repo` path doesn't exist and can't be created | 1 | `failed to initialize plakar repository: <reason>` |
| `--snapshot` ID not found | 1 | `snapshot not found: <id>` |
| Plakar repository corrupted | 1 | `failed to open plakar repository: <reason>` |
| `--backend` is unknown value | 1 | `unknown backend: <value>, expected 'tarball' or 'plakar'` |
| `--s3-upload`/`--s3-bucket` with `--backend plakar` | 1 | `--s3-upload and related S3 flags cannot be used with plakar backend; use --repo s3://... instead` |

## Environment Variables

All new flags follow the existing `INFRAHUB_` prefix pattern:

| Variable | Maps to Flag |
|----------|-------------|
| `INFRAHUB_BACKEND` | `--backend` |
| `INFRAHUB_REPO` | `--repo` |
| `INFRAHUB_SNAPSHOT` | `--snapshot` |
