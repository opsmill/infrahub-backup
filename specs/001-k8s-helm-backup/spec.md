---
title: "Add Kubernetes support for Infrahub backup/restore via Helm chart for users without direct kubectl access"
created: 2026-01-26
status: Draft
authors:
  - fatih
JPD: INFP-396
---

# Feature Specification: Kubernetes Helm Chart Support for Infrahub Backup

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Scheduled Backup to S3 (Priority: P1)

As a platform operator using ArgoCD/Flux to manage my Infrahub deployment, I want to configure automated periodic backups that store artifacts in S3-compatible storage, so that I have reliable disaster recovery without requiring direct cluster access.

**Why this priority**: This is the primary use case - most production environments need automated, reliable backups with external storage for disaster recovery. S3 storage ensures backups persist independently of the cluster.

**Independent Test**: Can be fully tested by deploying the Helm chart with CronJob enabled and S3 configuration, then verifying backup artifacts appear in the S3 bucket on schedule.

**Acceptance Scenarios**:

1. **Given** an Infrahub instance running on Kubernetes and the backup Helm chart deployed with CronJob enabled, **When** the scheduled time arrives, **Then** a backup Job runs and uploads the backup artifact to the configured S3 bucket
2. **Given** a CronJob backup configuration with S3 credentials, **When** the backup completes, **Then** the backup artifact is stored with a timestamped filename in the S3 bucket
3. **Given** a CronJob backup in progress, **When** the S3 upload fails, **Then** the Job reports failure status and logs the error details
4. **Given** a CronJob backup configuration, **When** the previous backup Job is still running, **Then** the new Job does not start (concurrency policy prevents overlap)

---

### User Story 2 - One-Shot Backup to S3 (Priority: P2)

As a platform operator, I want to trigger a single backup on-demand via Helm chart deployment, so that I can create backups before maintenance windows or major changes.

**Why this priority**: On-demand backups are essential for planned maintenance but less critical than automated scheduled backups for ongoing operations.

**Independent Test**: Can be fully tested by deploying the Helm chart with Job mode (not CronJob) and S3 configuration, then verifying a single backup artifact appears in S3.

**Acceptance Scenarios**:

1. **Given** an Infrahub instance running on Kubernetes, **When** the backup Helm chart is deployed with one-shot Job mode and S3 configuration, **Then** a single backup Job runs and uploads the artifact to S3
2. **Given** a one-shot backup Job, **When** the backup completes successfully, **Then** the Job pod enters Completed status and the artifact exists in S3
3. **Given** a one-shot backup Job, **When** the backup fails, **Then** the Job pod enters Failed status with error logs accessible

---

### User Story 3 - Restore from S3 (Priority: P2)

As a platform operator, I want to restore my Infrahub instance from a backup stored in S3, so that I can recover from data loss or migrate to a new cluster.

**Why this priority**: Restore capability is equally important as backup for disaster recovery, but typically used less frequently.

**Independent Test**: Can be fully tested by deploying the Helm chart in restore mode with S3 artifact location, then verifying Infrahub data matches the backup state.

**Acceptance Scenarios**:

1. **Given** a backup artifact in S3 and restore Helm chart deployed, **When** the restore Job runs, **Then** the artifact is downloaded from S3 and restored to the Infrahub databases
2. **Given** invalid S3 credentials in restore configuration, **When** the restore Job starts, **Then** the Job fails with a clear authentication error message
3. **Given** a non-existent backup artifact path, **When** the restore Job attempts download, **Then** the Job fails with a clear "artifact not found" error
4. **Given** a restore in progress, **When** the restore completes successfully, **Then** the Infrahub instance contains the data from the backup

---

### User Story 4 - Local Backup Storage (Priority: P3)

As a platform operator with restricted network access, I want to store backups locally within the backup pod, so that I can manually retrieve the artifact when S3 is not available.

**Why this priority**: Provides a fallback option when S3 is not available, but requires manual intervention to retrieve backups, making it less suitable for production disaster recovery.

**Independent Test**: Can be fully tested by deploying the Helm chart with local storage mode, running a backup, then using kubectl cp or a shared volume to retrieve the artifact.

**Acceptance Scenarios**:

1. **Given** the backup Helm chart deployed with local storage mode, **When** a backup Job completes, **Then** the backup artifact is stored in a specified path within the pod or a PersistentVolume
2. **Given** a completed backup with local storage, **When** the operator accesses the pod/volume, **Then** the backup artifact can be retrieved via kubectl cp or volume mount

---

### Edge Cases

- What happens when the backup runs during high database load? The backup should complete but may take longer; timeout configuration should accommodate this
- What happens when S3 bucket permissions are insufficient? Job should fail fast with clear permission error
- What happens when the Infrahub database is unavailable? Job should fail with connection error and not create partial backup
- What happens when storage quota is exceeded (S3 or local)? Job should fail with storage error before corrupting existing backups
- What happens when restore is run on an Infrahub instance with existing data? The restore should overwrite existing data (destructive operation - warn in docs)
- What happens when backup artifact is corrupted in S3? Restore should validate checksum and fail with corruption error

## Requirements *(mandatory)*

### Functional Requirements

**Helm Chart Structure**:
- **FR-001**: The Helm chart MUST be installable via standard Helm commands (helm install/upgrade)
- **FR-002**: The Helm chart MUST support deployment via GitOps tools (ArgoCD, Flux) without requiring kubectl access
- **FR-003**: The Helm chart MUST include configurable RBAC resources for the backup/restore pods

**Backup Operations**:
- **FR-004**: System MUST support CronJob-based scheduled backups with configurable schedule (cron expression)
- **FR-005**: System MUST support one-shot Job-based backups triggered by Helm deployment
- **FR-006**: System MUST backup Neo4j database, PostgreSQL (task-manager-db) as a single consistent backup
- **FR-007**: System MUST generate backup artifacts in tar.gz format with metadata JSON (consistent with existing infrahub-backup tool)
- **FR-008**: Backup Jobs MUST use the existing infrahub-backup container image (Dockerfile and release process to be created)

**Storage Options**:
- **FR-009**: System MUST support uploading backup artifacts to S3-compatible storage (AWS S3, MinIO, etc.)
- **FR-010**: System MUST support local storage of backup artifacts (within pod filesystem or PersistentVolume)
- **FR-011**: S3 configuration MUST support: bucket name, region, endpoint URL (for non-AWS S3), access key, secret key
- **FR-012**: S3 credentials MUST be configurable via Kubernetes Secrets (not plain values in Helm values)

**Restore Operations**:
- **FR-013**: System MUST support restore via Job deployment that downloads artifact from S3
- **FR-014**: Restore MUST require mandatory S3 configuration: bucket, artifact path, and credentials
- **FR-015**: Restore Job MUST validate backup artifact integrity before applying restore
- **FR-016**: Restore Jobs MUST use the existing infrahub-backup container image

**Configuration**:
- **FR-017**: Helm chart MUST provide toggle to switch between CronJob mode and one-shot Job mode for backups
- **FR-018**: Helm chart MUST provide toggle to enable restore mode (mutually exclusive with backup mode)
- **FR-019**: All sensitive values (credentials) MUST be injectable via existing Kubernetes Secrets
- **FR-020**: Helm chart MUST support configuring resource limits (CPU, memory) for backup/restore pods

**Observability**:
- **FR-021**: Backup/restore Jobs MUST output logs accessible via standard Kubernetes logging
- **FR-022**: Jobs MUST set appropriate exit codes (0 for success, non-zero for failure)

### Key Entities

- **Backup Artifact**: tar.gz archive containing Neo4j dump, PostgreSQL dump and metadata.json with version info and checksums
- **Helm Values**: Configuration structure defining backup mode (cronjob/job), storage type (s3/local), S3 settings, schedule, and resource limits
- **CronJob**: Kubernetes CronJob resource for scheduled backups
- **Job**: Kubernetes Job resource for one-shot backup or restore operations
- **Secret**: Kubernetes Secret containing S3 credentials (access key, secret key)

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: Users can deploy backup functionality via Helm without any kubectl commands (verified by successful ArgoCD/Flux deployment)
- **SC-002**: Scheduled backups run reliably at configured intervals with 99%+ success rate over 30-day period
- **SC-003**: Backup artifacts are uploadable to S3-compatible storage within 15 minutes for databases up to 10GB
- **SC-004**: Restore operations complete successfully and Infrahub functions normally after restore
- **SC-005**: Failed backup/restore Jobs provide actionable error messages in logs within 30 seconds of failure
- **SC-006**: Helm chart passes standard Helm lint and template validation
- **SC-007**: Users can configure and deploy the chart using only Helm values (no post-install manual steps required)

## Assumptions

- Users have an existing Infrahub deployment on Kubernetes (deployed via the main Infrahub Helm chart)
- The backup Helm chart will be deployed in the same namespace as Infrahub
- S3-compatible storage is accessible from within the Kubernetes cluster (network policies permit egress)
- Users understand that restore is a destructive operation that overwrites existing data
- The existing infrahub-backup tool supports Kubernetes environments (environment detection is already implemented)

## Out of Scope

- Backup encryption at rest (rely on S3 bucket encryption settings)
- Backup retention policies in S3 (rely on S3 lifecycle rules)
- Multi-cluster backup replication
- Backup verification/validation jobs (automated restore testing)
- Notifications/alerts for backup failures (rely on external monitoring of Job status)
- GUI/dashboard for backup management
