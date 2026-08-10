# Developer Documentation

Durable engineering knowledge for `infrahub-ops-cli`, extracted from completed
feature specs (`specs/`) as they land. The lifecycle is
`specs/ → dev/{adr,knowledge,guidelines}/`, after which the spec is archived
under `specs/archive/`.

- **adr/** — Architecture Decision Records: why a structural choice was made, and the alternatives rejected.
- **knowledge/** — How the system works today (descriptive).
- **guidelines/** — Prescriptive conventions to follow in future code.

## Current ADRs

- [0001-shell-out-to-docker-kubectl-clis.md](adr/0001-shell-out-to-docker-kubectl-clis.md) - Use the `docker`/`kubectl` CLIs instead of client libraries
- [0002-non-fatal-timeout-bounded-collectors.md](adr/0002-non-fatal-timeout-bounded-collectors.md) - Non-fatal, timeout-bounded collector framework
- [0003-read-only-collection.md](adr/0003-read-only-collection.md) - Collection never mutates the workload lifecycle
- [0004-key-name-secret-masking.md](adr/0004-key-name-secret-masking.md) - Key-name secret masking through a single choke-point
- [0005-uniform-bundle-and-manifest.md](adr/0005-uniform-bundle-and-manifest.md) - Single `.tar.gz` bundle + manifest, uniform across environments
- [0006-include-backup-reuses-createbackup.md](adr/0006-include-backup-reuses-createbackup.md) - `--include-backup` reuses `CreateBackup` non-interactively
- [0007-offline-by-default-opt-in-benchmark.md](adr/0007-offline-by-default-opt-in-benchmark.md) - Offline by default; benchmark/image pulls opt-in

## Current Knowledge

- [infrahub-collect.md](knowledge/infrahub-collect.md) - The troubleshooting-bundle tool: CLI, bundle layout, manifest, backend seam

## Current Guidelines

- [collectors.md](guidelines/collectors.md) - Conventions for writing and maintaining collect collectors
