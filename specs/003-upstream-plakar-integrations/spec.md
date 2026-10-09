# Feature Specification: Rework Plakar backend onto upstream database integrations

**Feature Branch**: `003-upstream-plakar-integrations`
**Created**: 2026-06-29
**Status**: Draft
**Input**: User description: "Rework the Plakar backup backend so it stops re-implementing database dump/restore mechanics and instead delegates to upstream, reusable Plakar integrations."

## Overview

The Plakar backup backend currently carries a custom `StreamingImporter` plus hand-rolled `pg_dump`/`neo4j-admin` command construction and restore logic. The same dump/restore mechanics now exist (Postgres) or can be contributed (Neo4j) as official, reusable Plakar integrations maintained upstream.

This feature replaces the tool's bespoke dump/restore mechanics with two upstream integrations:

- **Postgres** (task-manager / Prefect database) → the existing upstream `integration-postgresql`, consumed as-is.
- **Neo4j** (Infrahub graph database) → a **new, generic** Neo4j integration that this project authors and contributes to the `PlakarKorp/integrations` monorepo, covering both Community (offline dump) and Enterprise (online backup) editions.

The tool keeps everything that is genuinely Infrahub-specific (environment detection, lifecycle orchestration, task draining, snapshot grouping/tagging, edition detection, redaction). The dump/restore mechanics move out into integrations that anyone can reuse.

This spec combines two deliverables (the upstream Neo4j integration and the tool rework) so they are designed together; they are split into separate implementation plans afterward.

## Clarifications

### Session 2026-06-29

- Q: How are database credentials obtained and provided to the co-located runner? → A: Auto-discover from the running deployment by default, with an explicit operator override via flags/environment (hybrid).
- Q: Must restore support single-component recovery, or only the whole backup group? → A: Both — full-group restore and selective per-component restore (Neo4j-only or Postgres-only).
- Q: Is Kubernetes in scope for this rework, or a follow-up? → A: Both Docker Compose and Kubernetes at full parity within this rework; neither is deferred.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Back up the task-manager database via the upstream Postgres integration (Priority: P1)

An operator runs a backup of an Infrahub deployment. The task-manager (Prefect) PostgreSQL database is captured by delegating to the upstream Postgres integration rather than the tool's custom dump path, producing a snapshot in the standard integration layout, grouped with the rest of the backup.

**Why this priority**: The upstream Postgres integration already exists, so this is the smallest viable slice that proves the new "delegate to an upstream integration, run it co-located with the database, no host dependencies" model end to end. It de-risks everything else.

**Independent Test**: Against a running Compose deployment, run a backup that includes only the task-manager database; confirm the snapshot contains the upstream Postgres layout (cluster globals + per-database dump + manifest), carries the tool's grouping tags, and restores into a clean Postgres instance with data intact — all without installing any database client tools on the host.

**Acceptance Scenarios**:

1. **Given** a running Infrahub deployment, **When** the operator triggers a backup, **Then** the task-manager database is captured via the upstream Postgres integration and the resulting snapshot uses the integration's standard layout.
2. **Given** a host with no PostgreSQL client tools installed, **When** the operator triggers a backup, **Then** the backup succeeds (the dump runs co-located with the database, not on the host).
3. **Given** a Postgres snapshot produced by this flow, **When** the operator restores it into a clean Postgres instance, **Then** all databases, roles, and data are present and consistent.

---

### User Story 2 - Back up and restore Neo4j Enterprise via the new integration (online) (Priority: P1)

An operator backs up a deployment whose Neo4j runs Enterprise edition. The graph database is captured online (no downtime) by delegating to the new Neo4j integration, and can be restored from that snapshot.

**Why this priority**: Neo4j is the primary Infrahub datastore; an online, no-downtime backup of Enterprise is the most common production case and exercises the new integration's main path.

**Independent Test**: Against an Enterprise Neo4j, run a backup and confirm it completes without stopping the database, the snapshot includes the integration's manifest (version, edition, database, counts), and restoring into a clean Enterprise instance reproduces the graph.

**Acceptance Scenarios**:

1. **Given** a running Enterprise Neo4j, **When** the operator triggers a backup, **Then** the database is captured online without being stopped, and the snapshot carries a manifest describing the captured database.
2. **Given** a Neo4j Enterprise snapshot, **When** the operator restores it into a clean Enterprise instance, **Then** the graph (nodes, relationships, indexes, constraints) is reproduced.
3. **Given** the Enterprise backup port is unreachable, **When** the operator triggers a backup, **Then** the operation fails fast with a clear, actionable error rather than producing a partial or silent result.

---

### User Story 3 - Back up and restore Neo4j Community via the new integration (offline) (Priority: P2)

An operator backs up a deployment whose Neo4j runs Community edition. Because Community has no online backup, the tool drains tasks, stops the application and the database, the integration performs an offline dump with direct store-file access, and then the tool restarts everything. The snapshot can be restored into a clean Community instance.

**Why this priority**: Community is widely used in non-production and smaller deployments; it must remain supported, and it is the case that forces the co-located (in-container) execution model.

**Independent Test**: Against a Community Neo4j, run a backup and confirm the tool stops the database for the dump and restarts it afterward, the offline dump is captured, and restoring into a clean Community instance reproduces the graph.

**Acceptance Scenarios**:

1. **Given** a Community Neo4j, **When** the operator triggers a backup, **Then** the tool stops the application and database, the integration performs the offline dump, and the tool restarts the services afterward.
2. **Given** the database is still running, **When** the integration is asked to perform an offline dump, **Then** it fails fast with a clear precondition error (the integration does not stop the database itself).
3. **Given** a Neo4j Community snapshot, **When** the operator restores it into a clean Community instance, **Then** the graph is reproduced.

---

### User Story 4 - The Neo4j integration is generic and reusable (Priority: P3)

A Plakar user who has nothing to do with Infrahub installs the Neo4j integration and backs up their own Neo4j database directly with Plakar. The integration carries no Infrahub-specific assumptions and follows the conventions of the `PlakarKorp/integrations` monorepo so it can be reviewed and accepted upstream.

**Why this priority**: Reusability is the whole rationale for moving mechanics upstream — it removes a maintenance burden from this project and benefits the community — but the tool rework can proceed against a fork build before upstream acceptance, so it is not blocking.

**Independent Test**: With only Plakar and the integration installed (no Infrahub tooling), run a backup and restore of a standalone Neo4j (both editions) and confirm success; confirm the integration's own automated test suite passes.

**Acceptance Scenarios**:

1. **Given** only Plakar and the Neo4j integration, **When** a user backs up and restores a standalone Neo4j, **Then** it works without any Infrahub component present.
2. **Given** the integration source, **When** it is built and packaged, **Then** it follows the monorepo's layout and contribution conventions and can be installed into Plakar.

---

### Edge Cases

- **Partial backup**: one component (Postgres or Neo4j) fails while the other succeeds — the backup group is marked incomplete and the failure is reported; a half-written group is never presented as restorable.
- **Backup repository unreachable from inside the container/pod** (e.g., a local backup directory not mounted, or object-store credentials missing) — fails fast with a clear error before the database is touched.
- **Runner missing a required tool** (e.g., `neo4j-admin` or Postgres client tools absent in the co-located environment) — detected and reported as a setup error, not a backup data error.
- **Restore target version mismatch** for editions/engines where the backup format is version-sensitive — surfaced as a clear compatibility error.
- **Attempting to restore a legacy snapshot** produced by the old custom importer with the new tool — the tool clearly reports that the legacy layout is not supported and directs the operator to the prior tool version (clean break).
- **Community offline dump with the database left running** — the precondition fails fast (the integration never assumes it may stop the database).

## Requirements *(mandatory)*

### Functional Requirements

**Backup mechanics via upstream integrations**

- **FR-001**: The tool MUST capture the task-manager (Prefect) PostgreSQL database by delegating to the upstream Postgres integration, producing that integration's standard snapshot layout.
- **FR-002**: The tool MUST capture the Neo4j database by delegating to the new Neo4j integration, selecting the online path for Enterprise and the offline path for Community.
- **FR-003**: The tool MUST NOT retain its own database dump/restore mechanics — the custom streaming importer, hand-rolled dump-command construction, and export-then-rebuild restore logic are removed.

**Runtime / co-located execution**

- **FR-004**: Database dump and restore MUST run co-located with the target database (inside the container/pod), preserving zero host dependencies: the host running the tool MUST NOT require database client tools or network port-forwarding to the database.
- **FR-005**: The execution environment used to run the integrations MUST provide the required engine tooling (Postgres client tools, `neo4j-admin`) and MUST be able to reach the configured backup repository (local directory or object store).
- **FR-006**: For Neo4j Community offline dumps, the execution environment MUST have direct access to the database store files.
- **FR-023**: The tool MUST supply the required database credentials to the co-located execution by auto-discovering them from the running deployment by default, and MUST allow the operator to override them explicitly (flags/environment) for deployments where auto-discovery does not apply.

**Lifecycle orchestration (retained in the tool)**

- **FR-007**: The tool MUST continue to detect the deployment environment (Docker Compose or Kubernetes) and target the correct database services.
- **FR-025**: The co-located runner model (both backup and restore) MUST work at full parity in both Docker Compose and Kubernetes environments within this rework; neither environment is deferred to a follow-up.
- **FR-008**: The tool MUST continue to drain running tasks before a backup unless explicitly forced.
- **FR-009**: For Neo4j Community, the tool MUST stop the application and the database before the offline dump and restart them afterward, including on failure paths.
- **FR-010**: The integrations MUST NOT manage database lifecycle; they MUST document their preconditions and fail fast when a precondition (e.g., "database stopped" for Community offline) is not met.
- **FR-011**: The tool MUST continue to support redaction of attribute values prior to backup.

**Snapshots, grouping, and repository**

- **FR-012**: The tool MUST continue to group the components of one backup under a single backup identifier and apply its existing tag scheme (backup id, component, status, edition, versions, redaction flag, component list).
- **FR-013**: The tool MUST continue to support both local-filesystem and object-store backup repositories.
- **FR-014**: A backup group MUST record whether it is complete (all expected components present) or incomplete, and listing/inspection MUST reflect this.

**Restore**

- **FR-015**: The tool MUST restore Postgres and Neo4j (both editions) from snapshots in the new integration layout, using the integrations' restore paths co-located with the target databases.
- **FR-016**: The tool MUST only create and restore the new upstream integration layout (clean break); it MUST clearly reject legacy custom-importer snapshots and direct the operator to the prior tool version.
- **FR-024**: The tool MUST support both full-group restore (all components of a backup group) and selective per-component restore (Neo4j only, or Postgres only) from a backup group.

**The new Neo4j integration (upstream deliverable)**

- **FR-017**: The Neo4j integration MUST be generic (no Infrahub-specific assumptions) and MUST follow the `PlakarKorp/integrations` monorepo conventions (per-integration branch, sub-directory layout, module path, manifest, packaging).
- **FR-018**: The integration MUST support an Enterprise online-backup path and a Community offline-dump path, each with a corresponding restore path.
- **FR-019**: Each Neo4j snapshot MUST include a manifest describing at least the engine version, edition, database name, and store format, plus live entity counts when they can be obtained without violating the backup's preconditions.
- **FR-020**: The integration MUST ship automated tests that exercise backup and restore for both editions.

**Consumability / availability**

- **FR-021**: The tool MUST be able to consume the Neo4j integration built from this project's fork before upstream acceptance, so delivery is never blocked on an upstream merge.
- **FR-022**: The decision on how the tool consumes the integrations (in-process vs. via the Plakar CLI/plugin model) MUST be gated on an explicit feasibility check (a compile/version-compatibility spike) performed before the consumption path is committed.

### Key Entities *(include if feature involves data)*

- **Backup group**: the set of component snapshots produced by one backup run, identified by a shared backup id, with an overall complete/incomplete status and aggregate metadata (date, versions, edition, component list).
- **Component snapshot**: one captured database (Postgres or Neo4j) stored in its integration's standard layout, tagged for grouping.
- **Integration manifest**: structured metadata written inside a snapshot describing the captured database (engine version, edition, database name, structure, counts where available) without requiring a restore to inspect.
- **Neo4j edition**: Community vs. Enterprise — determines online-vs-offline capture and whether the tool must stop the database.
- **Execution environment (runner)**: the co-located context (inside the container/pod) that runs the integration with the required tooling and repository access.
- **Backup repository**: the destination store (local filesystem or object store) holding snapshots.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: An operator can back up and restore a complete Infrahub deployment (Postgres + Neo4j) for **both** Neo4j editions, with the restored databases matching the source (same row/entity counts and schema) in 100% of test runs.
- **SC-002**: Backups and restores succeed on a host with **no** database client tools installed and **no** network port-forwarding to the databases — confirming zero host dependencies are introduced.
- **SC-003**: Neo4j Enterprise backups complete **without** stopping the database (zero downtime); Neo4j Community backups stop and restart the database and the deployment returns to a healthy running state afterward in 100% of test runs.
- **SC-004**: The project no longer maintains custom database dump/restore mechanics — the bespoke importer and hand-rolled dump/restore code are removed, with the equivalent behavior provided by the integrations.
- **SC-005**: The Neo4j integration backs up and restores a standalone Neo4j (no Infrahub present) for both editions, and its automated test suite passes.
- **SC-006**: A backup in which one component fails is always reported as incomplete and is never offered as a successful, restorable backup.
- **SC-007**: Attempting to restore a legacy custom-importer snapshot with the new tool produces a clear, actionable message rather than a confusing failure.
- **SC-008**: The backup/restore success criteria (SC-001 through SC-003) hold in **both** Docker Compose and Kubernetes deployments.

## Assumptions & Dependencies

- **Existing upstream integration**: the upstream `integration-postgresql` is used as-is for the task-manager database; this project does not modify it.
- **New upstream integration**: this project authors the Neo4j integration and contributes it to `PlakarKorp/integrations` (branch `integration/neo4j`, sub-directory layout, module path matching the mirror convention). A fork build is consumed until upstream accepts/publishes it; engaging upstream maintainers early is assumed.
- **Consumption model is spike-gated**: a feasibility spike determines whether the integrations' library packages compile/register against this project's current Plakar core version, or whether the Plakar CLI/plugin model is used instead. The co-located execution decision and code-availability both favor the CLI/plugin model, but the spike makes the call.
- **Co-located execution artifact**: an execution environment (a runner image, or equivalent) containing Plakar, both integrations, and the required engine tooling is a new build artifact required by the co-located model.
- **Enterprise backup port**: the Neo4j Enterprise online-backup port is assumed to be enabled and reachable from the co-located runner in the Infrahub deployment; this must be verified.
- **Repository reachability**: the backup repository (local directory mount or object-store credentials) must be reachable from inside the co-located environment; straightforward for Compose, requires care for Kubernetes.
- **Encryption**: backups remain plaintext by default; no new encryption capability is introduced by this feature.

## Out of Scope (Non-Goals)

- The separate classic tar.gz backup/restore backend (untouched).
- Making the Neo4j integration Infrahub-specific.
- Encryption beyond today's plaintext-default policy.
- Supporting the old custom-importer snapshot layout in the new tool (clean break).
- Modifying the upstream Postgres integration.
