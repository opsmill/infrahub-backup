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
- [0008-transient-workload-for-external-databases.md](adr/0008-transient-workload-for-external-databases.md) - A load-bearing image pull for external databases; offline-by-default survives it
- [0009-transient-workload-lifecycle-and-credential-ownership.md](adr/0009-transient-workload-lifecycle-and-credential-ownership.md) - Transient pod from a manifest on stdin, deadline-bounded, owning its credential Secret
- [0010-neo4j-restore-by-seed-uri.md](adr/0010-neo4j-restore-by-seed-uri.md) - Remote Neo4j restore by seeding from an object-store URI, confirmed by polling
- [0011-asymmetric-version-compatibility.md](adr/0011-asymmetric-version-compatibility.md) - Abort when the utility is older than the server, warn when newer; two version schemes
- [0012-location-from-positive-evidence.md](adr/0012-location-from-positive-evidence.md) - Database location from positive in-deployment evidence; a failed query is never "external"
- [0013-incomplete-captures-removed-metadata-additive.md](adr/0013-incomplete-captures-removed-metadata-additive.md) - Incomplete captures removed, completeness read from output, metadata additive
- [0014-separate-external-restore-authorisation.md](adr/0014-separate-external-restore-authorisation.md) - External restore needs its own authorisation; the granting channel is recorded
- [0015-verify-by-default-external-tls.md](adr/0015-verify-by-default-external-tls.md) - External connections encrypted and verified by default, one operator opt-out
- [0016-single-ownership-decider.md](adr/0016-single-ownership-decider.md) - One ownership decider from label evidence and release prefixes, never a bare name

## Current Knowledge

- [infrahub-collect.md](knowledge/infrahub-collect.md) - The troubleshooting-bundle tool: CLI, bundle layout, manifest, backend seam
- [external-database-backup.md](knowledge/external-database-backup.md) - Backing up and restoring databases outside the deployment: gate, transient workload, discovery, CLI, prerequisites, metadata, bounds
- [container-image-user-semantics.md](knowledge/container-image-user-semantics.md) - The official database images declare no user; what that means for a pod's security context

## Current Guidelines

- [collectors.md](guidelines/collectors.md) - Conventions for writing and maintaining collect collectors
- [kubernetes-backend.md](guidelines/kubernetes-backend.md) - Rules for code that talks to a Kubernetes deployment: stdout-only reads, one ownership decider, bounded operations, credentials, additive metadata
- [implementation-verification.md](guidelines/implementation-verification.md) - Grep for callers of every new symbol per chunk; verify external premises with a named command
