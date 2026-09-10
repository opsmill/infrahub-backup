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

## Encrypted Plakar backups

The Plakar backend (`--backend plakar`) can write **encrypted at-rest** repositories
using the engine's native symmetric encryption (Argon2id KDF + AES‑256‑GCM‑SIV). A
leaked or stolen repository is unreadable without the passphrase.

> **Neo4j needs nothing exposed.** The Plakar backend runs `neo4j-admin` inside the
> database container or pod, so an Enterprise online backup reaches the backup service
> over loopback and Neo4j's default `127.0.0.1` listener is enough. An earlier revision
> ran it in a short-lived container beside the database, which did require
> `NEO4J_server_backup_listen__address` to be published; that requirement is gone.

```bash
# Create an encrypted repository (passphrase via env or file)
export INFRAHUB_BACKUP_PASSPHRASE='correct horse battery staple'
infrahub-backup create --backend plakar --repo fs:///backups/infra --encrypt

# The same passphrase is required for every later operation on the repo:
infrahub-backup --backend plakar --repo fs:///backups/infra snapshots list

# Restore the latest complete backup group, a named group, or one component:
infrahub-backup restore --backend plakar --repo fs:///backups/infra
infrahub-backup restore --backend plakar --repo fs:///backups/infra --backup-id 20260810_020000
infrahub-backup restore --backend plakar --repo fs:///backups/infra --snapshot 8f3a1c2d

# Read the passphrase from a file instead of the environment:
infrahub-backup create --backend plakar --repo fs:///backups/infra --encrypt \
  --passphrase-file /run/secrets/backup-pass
```

Notes:

- The plakar backend selects what to restore with `--backup-id` (a backup group) or
  `--snapshot` (one component), never with a positional argument. Passing one is
  rejected rather than ignored.
- Encryption is fixed when the repository is **created**; it is not applied
  retroactively and cannot be toggled in place. Create a new repository to change it.
- The passphrase must be **at least 12 characters**. It is supplied non‑interactively
  via `INFRAHUB_BACKUP_PASSPHRASE` or `--passphrase-file` (first line) and is **never**
  written to logs or to the co-located runner's command line / environment.
- **There is no key escrow.** If you lose the passphrase, the backups are
  unrecoverable. Store it securely.
- `--encrypt-key` is the **tarball** backend's public-key (ECIES) flag; using it with
  `--backend plakar` is rejected — use `--encrypt` with a passphrase instead.
- Repositories created without `--encrypt` stay plaintext and behave exactly as before.
