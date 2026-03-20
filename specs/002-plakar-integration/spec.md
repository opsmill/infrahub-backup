# Feature Specification: Embed Plakar Backup Engine into infrahub-backup

**Feature Branch**: `002-plakar-integration`
**Created**: 2026-03-18
**Status**: Draft
**Input**: User description: "Embed Plakar (github.com/PlakarKorp/plakar) into infrahub-backup with approach similar to Proxmox third-party integration"

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Deduplicated Backup Creation (Priority: P1)

As a platform operator, I want to create Infrahub backups using Plakar as the storage engine, so that I get automatic deduplication, content-addressable storage, and incremental backup capabilities that significantly reduce storage costs for repeated backups.

**Why this priority**: Deduplication and incremental backups are the primary value proposition. Large database dumps share significant data between backup runs, making deduplication highly valuable for storage reduction — especially in environments with frequent backup schedules.

**Independent Test**: Can be fully tested by running a backup command with the Plakar backend selected and a repository path, then verifying a Plakar snapshot exists containing all expected backup components.

**Acceptance Scenarios**:

1. **Given** a running Infrahub instance and a Plakar repository path, **When** the operator runs a backup with the Plakar backend, **Then** one snapshot per component is created (Neo4j database, PostgreSQL task-manager database, and metadata), grouped by a shared backup-id tag
2. **Given** a second backup run against the same repository with minimal data changes, **When** the backup completes, **Then** the incremental storage used is significantly smaller than a full backup due to deduplication
3. **Given** no existing Plakar repository at the specified path, **When** a backup is run, **Then** a new repository is initialized automatically before creating the snapshot
4. **Given** a backup in progress, **When** the process is interrupted (e.g., Ctrl+C, power loss), **Then** no partial or corrupt snapshot is committed to the repository

---

### User Story 2 - Restore from Plakar Snapshot (Priority: P1)

As a platform operator, I want to restore an Infrahub instance from a Plakar snapshot, so that I can recover from data loss or migrate to a new environment using the same deduplicated storage.

**Why this priority**: Restore is the complement to backup and equally critical for disaster recovery workflows. Without restore, backup has no value.

**Independent Test**: Can be fully tested by restoring from a previously created Plakar snapshot and verifying all Infrahub databases contain the expected data.

**Acceptance Scenarios**:

1. **Given** a Plakar repository with one or more backup groups, **When** the operator runs a restore specifying a backup-id, **Then** all component snapshots in that group are restored (Neo4j and PostgreSQL databases)
2. **Given** a Plakar repository with backup groups, **When** the operator runs a restore without specifying a backup-id, **Then** the most recent complete backup group is used automatically
3. **Given** a backup-id that does not exist in the repository, **When** restore is attempted, **Then** a clear error message identifies the problem and lists available backup groups
4. **Given** an incomplete backup group, **When** the operator explicitly requests restore of that group, **Then** available components are restored with a warning about missing components

---

### User Story 3 - Backward Compatibility with Archive Backups (Priority: P1)

As an existing user of infrahub-backup, I want the default behavior to remain unchanged (tar.gz archives), so that the Plakar integration is fully opt-in and does not break my existing automation or workflows.

**Why this priority**: Backward compatibility is non-negotiable. Existing deployments rely on the current tar.gz format for backup pipelines, monitoring, and disaster recovery procedures.

**Independent Test**: Running the backup tool without opting into the Plakar backend produces the same tar.gz output as before the integration.

**Acceptance Scenarios**:

1. **Given** an upgraded infrahub-backup with Plakar support, **When** a backup is created without specifying the Plakar backend, **Then** the output is a tar.gz archive identical in format to previous versions
2. **Given** a tar.gz backup file created by any version, **When** the operator runs a restore with that file, **Then** the restore completes using the original archive-based restore flow
3. **Given** command documentation and help output, **When** the operator reviews available options, **Then** the Plakar backend is clearly documented as an optional alternative

---

### User Story 4 - List and Inspect Snapshots (Priority: P2)

As a platform operator, I want to list available Plakar snapshots and see their metadata (creation date, Infrahub version, included components), so that I can make informed decisions about which snapshot to restore.

**Why this priority**: Snapshot listing enables informed restore decisions but is not strictly required for core backup/restore functionality.

**Independent Test**: Can be fully tested by creating several backups and listing snapshots to confirm all are shown with correct metadata.

**Acceptance Scenarios**:

1. **Given** a Plakar repository with multiple backup groups, **When** the operator requests a snapshot list, **Then** backups are displayed grouped by backup-id with creation timestamp, Infrahub version, Neo4j edition, components present, and status (complete/incomplete)
2. **Given** an empty or newly initialized Plakar repository, **When** listing is requested, **Then** a clear message indicates no snapshots are available

---

### User Story 5 - Remote Repository Storage (Priority: P3)

As a platform operator, I want to use S3-compatible storage as a Plakar repository backend, so that backups are stored off-site automatically without a separate upload step.

**Why this priority**: Remote storage builds on the foundation of local Plakar integration and extends it for production disaster recovery use cases where off-site storage is required.

**Independent Test**: Can be tested by running a backup against an S3 repository URI and verifying the snapshot is retrievable from S3.

**Acceptance Scenarios**:

1. **Given** S3-compatible storage credentials and endpoint, **When** a backup is run targeting an S3 repository URI, **Then** the Plakar repository and snapshot are stored in S3
2. **Given** an existing Plakar repository in S3, **When** a restore is run using the same S3 URI, **Then** the snapshot is restored successfully from remote storage

---

### User Story 6 - Stream Backups Directly into Kloset (Priority: P1)

As a platform operator, I want the Neo4j and PostgreSQL dumps to be streamed directly from the container (via docker exec / kubectl exec stdout) into the Plakar repository without being written to the local filesystem first, so that I avoid duplicating large backup files across temp directories and reduce disk usage and I/O overhead.

Currently, each Plakar backup goes through multiple stages of file copying: dump inside container → copy to local temp → walk temp files → import into kloset. This means the backup data exists in at least 3 places simultaneously. Streaming via Plakar's stdio connector eliminates the local temp copy entirely.

**Why this priority**: This is a fundamental efficiency improvement. Without streaming, Plakar backups still require as much local disk space as archive backups, undermining the storage benefit. The stdio connector is the natural fit for piping exec output directly into kloset.

**Independent Test**: Can be fully tested by running `infrahub-backup create --backend plakar` and verifying that no local temp directory is created for database dumps while the snapshot is still created successfully in the Plakar repository.

**Acceptance Scenarios**:

1. **Given** a running Infrahub deployment with Plakar backend configured, **When** the operator runs a Plakar backup, **Then** database dump data is streamed from container exec stdout directly into a Plakar snapshot without writing intermediate files to the local filesystem.
2. **Given** a running Infrahub deployment on Kubernetes, **When** the operator runs a Plakar backup, **Then** the kubectl exec output is streamed directly into the Plakar repository, same as the Docker Compose path.
3. **Given** a Plakar backup is in progress and the container exec stream is interrupted mid-transfer, **When** the streaming connection drops, **Then** the backup fails cleanly with a descriptive error and no partial snapshot is committed to the repository.

---

### User Story 7 - Uncompressed Dumps for Effective Deduplication (Priority: P2)

As an operator running incremental Plakar backups, I want Neo4j and PostgreSQL to produce uncompressed dump output when using the Plakar backend, so that Plakar's content-defined chunking and deduplication can identify and deduplicate unchanged data blocks across snapshots, significantly reducing repository storage over time.

Compressed dumps (pg_dump -Fc, Neo4j's default compressed backup) produce output where a small change in input causes a cascade of changes throughout the compressed output. This defeats block-level deduplication. Uncompressed output preserves locality of changes.

**Why this priority**: Deduplication effectiveness is the core value of using Plakar. Without uncompressed dumps, storage savings from dedup would be negligible, undermining User Story 1's value proposition.

**Independent Test**: Can be tested by running two consecutive backups with a small data change between them, then checking that the Plakar repository size grew by significantly less than a full backup size (demonstrating effective dedup).

**Acceptance Scenarios**:

1. **Given** Plakar backend is configured, **When** a Neo4j Enterprise backup is created, **Then** the dump is produced without compression so that Plakar dedup can operate on raw data blocks.
2. **Given** Plakar backend is configured, **When** a PostgreSQL backup is created, **Then** pg_dump outputs in custom format without compression (-Fc -Z0) instead of the default compressed custom format, preserving `pg_restore` compatibility while enabling streaming to stdout and effective deduplication.
3. **Given** Plakar backend is configured and two backups are taken with minimal data changes between them, **When** comparing the Plakar repository size after the second backup to the combined size of two full dumps, **Then** the repository demonstrates meaningful deduplication (repository growth is substantially less than a full dump size).
4. **Given** the legacy (non-Plakar) backend is used, **When** a backup is created, **Then** the existing compressed dump formats are preserved (no behavioral change for non-Plakar paths).

---

### Edge Cases

- What happens when the Plakar repository is corrupted or damaged? The system should detect corruption during open or restore and report a clear error before proceeding
- What happens when disk space is exhausted during snapshot creation? No partial snapshot should be committed; the system should clean up and report the failure
- What happens when two operators run concurrent backups to the same repository? The system should use locking to prevent concurrent writes and report a clear "repository locked" error to the second operator
- What happens when the repository format is incompatible with the embedded Plakar version? The system should detect version incompatibility at open time and report the expected vs. actual version
- What happens when archive-specific S3 flags are combined with the Plakar backend? The system should reject with a clear error message explaining the two S3 mechanisms are incompatible and directing the operator to use `--repo s3://...`
- What happens when the docker exec / kubectl exec stream stalls or hangs indefinitely during a streaming backup? The system should enforce a configurable timeout
- What happens when the container runs out of memory while producing an uncompressed dump (uncompressed dumps are larger than compressed)? The system should detect the exec failure and report the error clearly
- What happens when the Plakar repository storage is full or unreachable during streaming? The system should fail cleanly without leaving partial state
- What happens when Neo4j Community Edition offline dump is used with streaming? The offline dump writes to a file inside the container (not stdout), so the system should stream the dump file out via exec after creation rather than requiring docker cp

## Requirements *(mandatory)*

### Functional Requirements

**Backup Operations**:
- **FR-001**: System MUST support an opt-in backend selection to choose between the existing archive format and Plakar as the storage engine
- **FR-002**: System MUST support specifying a Plakar repository location (local filesystem path or remote URI)
- **FR-003**: System MUST automatically initialize a new Plakar repository when none exists at the specified location
- **FR-004**: System MUST create one snapshot per component (Neo4j database, PostgreSQL task-manager database, metadata), grouped by a shared backup identifier tag (e.g., `infrahub.backup-id=<timestamp>`)
- **FR-004a**: If a component snapshot fails, the system MUST keep previously completed component snapshots and tag the backup group as incomplete (e.g., `infrahub.backup-status=incomplete`), allowing operators to use partial backups
- **FR-005**: System MUST store Infrahub backup metadata (Infrahub version, tool version, Neo4j edition, included components, redaction status) as snapshot-level tags on each component snapshot, queryable without extracting backup data

**Restore Operations**:
- **FR-006**: System MUST support restoring from a specific backup group identified by its backup-id tag, restoring all component snapshots in that group
- **FR-006a**: System MUST support restoring individual components by snapshot ID for partial recovery scenarios
- **FR-007**: System MUST default to the most recent complete backup group when no specific backup or snapshot is requested; if only incomplete groups exist, the system MUST warn the operator and require explicit confirmation
- **FR-008**: System MUST validate snapshot data integrity before beginning the database restore process
- **FR-009**: System MUST support the same restore options as the archive path (exclude task-manager, migrate Neo4j format)

**Snapshot Management**:
- **FR-010**: System MUST provide a command to list all snapshots in a Plakar repository
- **FR-011**: Snapshot listing MUST group snapshots by backup-id and display creation timestamp, Infrahub version, Neo4j edition, components present, and backup status (complete/incomplete)

**Backward Compatibility**:
- **FR-012**: Default behavior (no backend selection) MUST remain unchanged — producing tar.gz archives
- **FR-013**: Existing restore from tar.gz files MUST continue to work without modification
- **FR-014**: All existing CLI flags and environment variables MUST continue to function as documented

**Storage**:
- **FR-015**: System MUST support local filesystem paths as Plakar repository locations
- **FR-016**: System SHOULD support S3-compatible remote storage as Plakar repository locations

**Security**:
- **FR-019**: System MUST create Plakar repositories in plaintext (unencrypted) mode by default, consistent with the existing unencrypted tar.gz archive behavior
- **FR-020**: System SHOULD support an opt-in encryption flag that enables passphrase-protected repositories for operators who require encryption at rest

**Streaming**:
- **FR-022**: System MUST stream Neo4j backup output from the database container directly into a Plakar snapshot via the stdio connector, without writing intermediate files to the local filesystem. For Enterprise Edition (which produces a directory), the backup directory is tar-streamed via exec stdout into kloset
- **FR-023**: System MUST stream PostgreSQL dump output from the task-manager-db container directly into a Plakar snapshot via the stdio connector, without writing intermediate files to the local filesystem
- **FR-024**: System MUST include backup metadata (backup_information.json) in the Plakar snapshot without requiring a local temp directory
- **FR-025**: System MUST support streaming for both Docker Compose and Kubernetes environments
- **FR-026**: For Neo4j Community Edition (which writes dump files, not stdout), the system MUST stream the dump file out from the container via exec after creation, rather than using docker cp / kubectl cp
- **FR-026a**: Restore operations MUST continue to use local temporary storage (exporting from kloset to local temp, then copying into containers), as database restore tools require files on disk

**Deduplication Effectiveness**:
- **FR-027**: System MUST produce Neo4j backup output without compression when using the Plakar backend, to enable effective content-based deduplication
- **FR-028**: System MUST produce PostgreSQL dump output in custom format without compression (-Fc -Z0) when using the Plakar backend, to enable streaming to stdout, effective content-based deduplication, and `pg_restore` compatibility
- **FR-029**: System MUST preserve existing compressed dump formats when using the legacy (non-Plakar) backup backend, ensuring no behavioral change for existing users

**Operational**:
- **FR-017**: System MUST prevent concurrent writes to the same Plakar repository
- **FR-018**: System MUST not commit partial snapshots on failure or interruption
- **FR-021**: System MUST reject with a clear error when archive-specific S3 flags (`--s3-upload`, `--s3-bucket`, etc.) are used together with the Plakar backend, directing the operator to use `--repo s3://...` instead

### Key Entities

- **Plakar Repository**: A content-addressable, deduplicated storage location that holds backup snapshots. Can reside on local filesystem or remote storage. Created automatically on first use
- **Snapshot**: A single component backup stored in a Plakar repository (one snapshot per component: Neo4j, PostgreSQL, or metadata). Identified by a unique ID. Includes queryable tags for backup-id, component type, and Infrahub metadata
- **Backup Group**: A set of component snapshots sharing the same backup-id tag, representing a single backup run. Can be complete (all expected components present) or incomplete (some components failed)
- **Backup Backend**: The user's choice of storage engine — either the existing archive format (default) or Plakar for deduplication

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A second backup of the same unchanged Infrahub instance uses at least 50% less additional storage compared to creating a second archive backup
- **SC-002**: Backup and restore via Plakar complete successfully with full data integrity — restored databases match the original state
- **SC-003**: Existing archive-based backup and restore workflows produce identical results with no changes required from operators
- **SC-004**: Operators can list all available snapshots with metadata and select a specific snapshot for restore
- **SC-005**: All existing automated tests continue to pass without modification
- **SC-006**: Plakar backups complete without creating any local temporary files for database dumps (zero local disk usage beyond the tool's own memory)
- **SC-007**: After two consecutive backups with less than 5% data change, the Plakar repository grows by less than 20% of a single full backup size (demonstrating effective deduplication with uncompressed dumps)

## Clarifications

### Session 2026-03-18

- Q: Should Plakar repositories default to encrypted or plaintext? → A: Plaintext by default (consistent with existing tar.gz); encryption available as opt-in flag
- Q: What happens when archive-specific S3 flags are combined with Plakar backend? → A: Reject with clear error directing operator to use `--repo s3://...` instead

### Session 2026-03-19

- Q: PostgreSQL dump format for Plakar dedup — plain (SQL text, needs psql to restore), directory (-Fd, one file per table, keeps pg_restore), or keep -Fc? → A: Custom format without compression (-Fc -Z0) — streams to stdout (unlike -Fd), internal per-table structure aids dedup, preserves pg_restore compatibility. Research showed -Fd can't stream to stdout; -Fc -Z0 is strictly better for streaming
- Q: Snapshot composition — single snapshot with all components, or one snapshot per component? → A: One snapshot per component (Neo4j, PostgreSQL, metadata), grouped by a shared backup-id tag. Cleaner streaming model and enables independent component restore
- Q: Partial backup handling — if one component fails, keep or delete successful snapshots? → A: Keep partial backups, tag as incomplete. Operators decide whether to use them
- Q: Should restore also stream directly from kloset into containers (zero local storage), or use local temp? → A: Restore uses local temp (current approach). Restore tools expect files on disk, and restore is less frequent than backup
- Q: Neo4j Enterprise backup produces a directory, not stdout — how to stream it? → A: tar the uncompressed backup directory inside the container and stream the tar output via exec stdout into kloset

## Assumptions

- Operators have sufficient local disk space or S3 access for the Plakar repository
- The Plakar repository cache directory (for deduplication state) can persist between backup runs
- Operators understand that Plakar and archive backups are separate — snapshots cannot be created from archives or vice versa
- The Plakar project's library is stable enough for production embedding (v1.1.0+)
- Plakar's kloset library supports an importer interface that can accept streaming data (io.Reader/stdin-style) rather than requiring files on disk
- The `docker exec` and `kubectl exec` commands reliably stream stdout for the duration of long-running dump operations
- PostgreSQL `pg_dump` supports outputting plain (uncompressed) dumps to stdout (its default behavior when no -f flag is specified)
- The increased memory footprint of uncompressed dumps in the container is acceptable
- The Plakar stdio connector handles back-pressure appropriately (slow repository writes won't cause the container exec to fail)

## Out of Scope

- Plakar web UI or dashboard integration
- Plakar scheduling (operators use existing CronJob/cron mechanisms)
- Advanced encryption key management (key rotation, HSM integration); basic passphrase-based encryption is supported as opt-in
- Migration tool to convert existing tar.gz archives into Plakar snapshots
- Snapshot pruning or retention policies (operators use Plakar CLI directly for this)
- Multi-repository replication
