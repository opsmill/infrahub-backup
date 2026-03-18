# Quickstart: Plakar-Backed Infrahub Backups

## Prerequisites

- `infrahub-backup` binary built with Plakar support
- Running Infrahub instance (Docker Compose or Kubernetes)

## Create a Backup with Plakar

```bash
# First backup — repository is created automatically
infrahub-backup create --backend plakar --repo /var/backups/infrahub

# Subsequent backups — deduplicated against existing snapshots
infrahub-backup create --backend plakar --repo /var/backups/infrahub
```

## List Available Snapshots

```bash
infrahub-backup snapshots list --repo /var/backups/infrahub
```

Output:
```
SNAPSHOT ID    DATE                  INFRAHUB VERSION  COMPONENTS
a3f2b1c8      2026-03-18 14:30:00   1.2.0            neo4j, task-manager-db
e7d9a4f1      2026-03-17 02:00:00   1.2.0            neo4j, task-manager-db
```

## Restore from a Snapshot

```bash
# Restore the latest snapshot
infrahub-backup restore --backend plakar --repo /var/backups/infrahub

# Restore a specific snapshot
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
