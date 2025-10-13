<!-- markdownlint-disable -->
![Infrahub Logo](https://assets-global.website-files.com/657aff4a26dd8afbab24944b/657b0e0678f7fd35ce130776_Logo%20INFRAHUB.svg)
<!-- markdownlint-restore -->

# Infrahub Ops CLI

[Infrahub](https://github.com/opsmill/infrahub) by [OpsMill](https://opsmill.com) acts as a central hub to manage the data, templates and playbooks that powers your infrastructure. At its heart, Infrahub is built on 3 fundamental pillars:

- **A Flexible Schema**: A model of the infrastructure and the relation between the objects in the model, that's easily extensible.
- **Version Control**: Natively integrated into the graph database which opens up some new capabilities like branching, diffing, and merging data directly in the database.
- **Unified Storage**: By combining a graph database and git, Infrahub stores data and code needed to manage the infrastructure.

## Introduction

The Infrahub Ops CLI allows you to run maintenance commands on your running Infrahub instances:

- Easy database backup and restore
- Housekeeping tasks
- Automated troubleshooting bundle collection

## Available executables

Each operational area is exposed as its own binary:

- `infrahub-backup` – create and restore Infrahub backups
- `infrahub-environment` – inspect running deployments and environment metadata
- `infrahub-taskmanager` – maintain Prefect flow runs and task queues
- `infrahub-version` – display Infrahub and CLI version information

## Using the CLI

Documentation for using the Infrahub Ops tooling is available [here](https://docs.infrahub.app/infrahubops/)