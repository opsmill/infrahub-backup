<!-- markdownlint-disable -->
![Infrahub Logo](https://assets-global.website-files.com/657aff4a26dd8afbab24944b/657b0e0678f7fd35ce130776_Logo%20INFRAHUB.svg)
<!-- markdownlint-restore -->

# Infrahub Backup

[Infrahub](https://github.com/opsmill/infrahub) by [OpsMill](https://opsmill.com) acts as a central hub to manage the data, templates and playbooks that powers your infrastructure. At its heart, Infrahub is built on 3 fundamental pillars:

- **A Flexible Schema**: A model of the infrastructure and the relation between the objects in the model, that's easily extensible.
- **Version Control**: Natively integrated into the graph database which opens up some new capabilities like branching, diffing, and merging data directly in the database.
- **Unified Storage**: By combining a graph database and git, Infrahub stores data and code needed to manage the infrastructure.

## Introduction

Infrahub Backup allows you to run maintenance commands on your running Infrahub instances:

- Easy database backup and restore
- Read-only troubleshooting-bundle collection for support

## Available executables

Each operational area is exposed as its own binary:

- `infrahub-backup` – create/restore backups, inspect environments, and show build metadata
- `infrahub-collect` – collect read-only troubleshooting bundles (service logs, diagnostics, metrics) for support

## Using the CLI

Documentation for using the Infrahub Backup is available in the [infrahub-backup documentation](https://docs.infrahub.app/backup/)

Documentation for using Infrahub Collect is available in the [infrahub-collect documentation](https://docs.infrahub.app/collect/)

### Restore without naming an archive

A scheduled restore cannot know a backup's filename in advance, so `restore` accepts `--latest` in place of an archive:

```bash
# Restore the newest archive in the local backup directory
infrahub-backup restore --latest

# Restore the newest archive in the configured S3 bucket and prefix
infrahub-backup restore --latest --s3
```

`--latest` ranks archives the same way the retention policy does — by the timestamp embedded in the filename, newest first — and consults exactly one location per run: the backup directory, or the bucket with `--s3`, never both. It logs which archive it selected and where it came from before the restore starts, and it fails instead of falling back when the location is empty or the newest archive is encrypted without a `--decrypt-key`.

See [Restore from a backup](https://docs.infrahub.app/backup/restore) for the full flag reference, the selection rules, and a nightly production-to-staging example.
