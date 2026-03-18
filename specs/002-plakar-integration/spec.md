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

1. **Given** a running Infrahub instance and a Plakar repository path, **When** the operator runs a backup with the Plakar backend, **Then** a snapshot is created containing all backup components (Neo4j database, PostgreSQL task-manager database, and metadata)
2. **Given** a second backup run against the same repository with minimal data changes, **When** the backup completes, **Then** the incremental storage used is significantly smaller than a full backup due to deduplication
3. **Given** no existing Plakar repository at the specified path, **When** a backup is run, **Then** a new repository is initialized automatically before creating the snapshot
4. **Given** a backup in progress, **When** the process is interrupted (e.g., Ctrl+C, power loss), **Then** no partial or corrupt snapshot is committed to the repository

---

### User Story 2 - Restore from Plakar Snapshot (Priority: P1)

As a platform operator, I want to restore an Infrahub instance from a Plakar snapshot, so that I can recover from data loss or migrate to a new environment using the same deduplicated storage.

**Why this priority**: Restore is the complement to backup and equally critical for disaster recovery workflows. Without restore, backup has no value.

**Independent Test**: Can be fully tested by restoring from a previously created Plakar snapshot and verifying all Infrahub databases contain the expected data.

**Acceptance Scenarios**:

1. **Given** a Plakar repository with one or more snapshots, **When** the operator runs a restore specifying a snapshot ID, **Then** the Neo4j and PostgreSQL databases are restored from that exact snapshot
2. **Given** a Plakar repository with snapshots, **When** the operator runs a restore without specifying a snapshot, **Then** the most recent snapshot is used automatically
3. **Given** a snapshot ID that does not exist in the repository, **When** restore is attempted, **Then** a clear error message identifies the problem and lists available snapshots

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

1. **Given** a Plakar repository with multiple snapshots, **When** the operator requests a snapshot list, **Then** all snapshots are displayed with creation timestamp, Infrahub version, Neo4j edition, and component information
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

### Edge Cases

- What happens when the Plakar repository is corrupted or damaged? The system should detect corruption during open or restore and report a clear error before proceeding
- What happens when disk space is exhausted during snapshot creation? No partial snapshot should be committed; the system should clean up and report the failure
- What happens when two operators run concurrent backups to the same repository? The system should use locking to prevent concurrent writes and report a clear "repository locked" error to the second operator
- What happens when the repository format is incompatible with the embedded Plakar version? The system should detect version incompatibility at open time and report the expected vs. actual version
- What happens when archive-specific S3 flags are combined with the Plakar backend? The system should reject with a clear error message explaining the two S3 mechanisms are incompatible and directing the operator to use `--repo s3://...`

## Requirements *(mandatory)*

### Functional Requirements

**Backup Operations**:
- **FR-001**: System MUST support an opt-in backend selection to choose between the existing archive format and Plakar as the storage engine
- **FR-002**: System MUST support specifying a Plakar repository location (local filesystem path or remote URI)
- **FR-003**: System MUST automatically initialize a new Plakar repository when none exists at the specified location
- **FR-004**: System MUST create one snapshot per backup containing all selected components (Neo4j database, PostgreSQL task-manager database, metadata)
- **FR-005**: System MUST store Infrahub backup metadata (Infrahub version, tool version, Neo4j edition, included components, redaction status) as snapshot-level metadata queryable without extracting backup data

**Restore Operations**:
- **FR-006**: System MUST support restoring from a specific Plakar snapshot identified by its unique ID
- **FR-007**: System MUST default to the most recent snapshot when no specific snapshot is requested
- **FR-008**: System MUST validate snapshot data integrity before beginning the database restore process
- **FR-009**: System MUST support the same restore options as the archive path (exclude task-manager, migrate Neo4j format)

**Snapshot Management**:
- **FR-010**: System MUST provide a command to list all snapshots in a Plakar repository
- **FR-011**: Snapshot listing MUST display creation timestamp, Infrahub version, Neo4j edition, and included components

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

**Operational**:
- **FR-017**: System MUST prevent concurrent writes to the same Plakar repository
- **FR-018**: System MUST not commit partial snapshots on failure or interruption
- **FR-021**: System MUST reject with a clear error when archive-specific S3 flags (`--s3-upload`, `--s3-bucket`, etc.) are used together with the Plakar backend, directing the operator to use `--repo s3://...` instead

### Key Entities

- **Plakar Repository**: A content-addressable, deduplicated storage location that holds backup snapshots. Can reside on local filesystem or remote storage. Created automatically on first use
- **Snapshot**: A point-in-time backup stored in a Plakar repository, containing all selected Infrahub components. Identified by a unique ID. Includes queryable metadata (Infrahub version, creation date, components)
- **Backup Backend**: The user's choice of storage engine — either the existing archive format (default) or Plakar for deduplication

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A second backup of the same unchanged Infrahub instance uses at least 50% less additional storage compared to creating a second archive backup
- **SC-002**: Backup and restore via Plakar complete successfully with full data integrity — restored databases match the original state
- **SC-003**: Existing archive-based backup and restore workflows produce identical results with no changes required from operators
- **SC-004**: Operators can list all available snapshots with metadata and select a specific snapshot for restore
- **SC-005**: All existing automated tests continue to pass without modification

## Clarifications

### Session 2026-03-18

- Q: Should Plakar repositories default to encrypted or plaintext? → A: Plaintext by default (consistent with existing tar.gz); encryption available as opt-in flag
- Q: What happens when archive-specific S3 flags are combined with Plakar backend? → A: Reject with clear error directing operator to use `--repo s3://...` instead

## Assumptions

- Operators have sufficient local disk space or S3 access for the Plakar repository
- The Plakar repository cache directory (for deduplication state) can persist between backup runs
- Operators understand that Plakar and archive backups are separate — snapshots cannot be created from archives or vice versa
- The Plakar project's library is stable enough for production embedding (v1.1.0+)

## Out of Scope

- Plakar web UI or dashboard integration
- Plakar scheduling (operators use existing CronJob/cron mechanisms)
- Advanced encryption key management (key rotation, HSM integration); basic passphrase-based encryption is supported as opt-in
- Migration tool to convert existing tar.gz archives into Plakar snapshots
- Snapshot pruning or retention policies (operators use Plakar CLI directly for this)
- Multi-repository replication
