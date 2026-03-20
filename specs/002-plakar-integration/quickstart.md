# Quickstart: Plakar-Backed Infrahub Backups

## Prerequisites

- `infrahub-backup` binary built with Plakar support
- Running Infrahub instance (Docker Compose or Kubernetes)

## Create a Backup with Plakar

```bash
# First backup — repository is created automatically
# Streams directly from containers into kloset (no local temp files)
infrahub-backup create --backend plakar --repo /var/backups/infrahub

# Subsequent backups — deduplicated against existing snapshots
infrahub-backup create --backend plakar --repo /var/backups/infrahub
```

Each backup creates one snapshot per component (Neo4j, PostgreSQL, metadata), grouped by a backup-id.

## List Available Backups

```bash
infrahub-backup snapshots list --repo /var/backups/infrahub
```

Output:
```
BACKUP ID            DATE                  STATUS      INFRAHUB VERSION  NEO4J EDITION  COMPONENTS
20260318_143000      2026-03-18 14:30:00   complete    1.2.0            enterprise     neo4j, postgres, metadata
20260317_020000      2026-03-17 02:00:00   complete    1.2.0            enterprise     neo4j, postgres, metadata
```

## Restore from a Backup

```bash
# Restore the latest complete backup group
infrahub-backup restore --backend plakar --repo /var/backups/infrahub

# Restore a specific backup group
infrahub-backup restore --backend plakar --repo /var/backups/infrahub --backup-id 20260318_143000

# Restore a single component (partial recovery)
infrahub-backup restore --backend plakar --repo /var/backups/infrahub --snapshot a3f2b1c8
```

## Using S3 Storage

```bash
# Backup to S3
infrahub-backup create --backend plakar --repo s3://my-bucket/infrahub-backups

# Restore from S3
infrahub-backup restore --backend plakar --repo s3://my-bucket/infrahub-backups
```

## Default Behavior (Unchanged)

```bash
# Still works exactly as before — produces tar.gz
infrahub-backup create
infrahub-backup restore backup-20260318.tar.gz
```
