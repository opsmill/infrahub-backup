# Feature Specification: External Database Backup and Restore

**Feature Branch**: `007-external-database-backup`

**Created**: 2026-09-02

**Status**: Extracted

**Input**: User description: "Several customers have their Neo4j database and sometimes the PostgreSQL database outside their Kubernetes cluster. Right now it is not possible to back those up because the tool is not able to find them. The prerequisite will be that the backup port needs to be open from the place we run the backup tool. What else is missing?"

## Clarifications

### Session 2026-09-02

- Q: Version compatibility rule — abort on any client/server mismatch, or asymmetric? → A: Asymmetric: abort when the client tooling is older than the server; warn when it is newer
- Q: What reclaims the stored credentials if the run is killed before it can clean up? → A: Tie the credential object's lifetime to the workload's, so the cluster deletes it automatically
- Q: SC-004 requires recording whether the source endpoint was a follower, but which endpoint served a multi-endpoint capture is not exposed by a documented interface. How should the recovery-point caveat be recorded? → A: Record the endpoint list supplied plus each member's role as observed at capture time, without claiming which one served the capture
- Q: How does the tool learn the database size needed to size scratch space before a capture? → A: Query the server for its store size before capture, apply a headroom factor, and let the flag override

## User Scenarios & Testing *(mandatory)*

### Context: why "cannot find them" understates the gap

Every backup and restore path in this toolset executes *inside* the database container: the
tool resolves a deployment service name to a running container, then runs the vendor dump or
restore utility in that container and moves the resulting bytes. No code path opens a network
connection to a database. When a customer's Neo4j or PostgreSQL lives outside the deployment,
there is no container to execute in, and the run fails while resolving the service name.

Consequently, opening a database port toward the operator does not by itself enable anything —
it only matters for a design where the tool dials the database directly. The requirements below
are written against the behaviour operators need, not against that assumed prerequisite.

---

### User Story 1 - Round-trip a deployment whose databases live outside the cluster (Priority: P1)

An operator responsible for an Infrahub deployment on Kubernetes whose Neo4j Enterprise
database — and optionally whose PostgreSQL task-manager database — runs outside the cluster
takes a complete backup with the same command they use today, and later restores it, without
performing manual steps on the database hosts.

**Why this priority**: These customers have no backup or recovery path through this toolset at
all today. A backup that cannot be restored would be a compliance artefact rather than a
recovery capability, so the round trip is one indivisible slice — recoverability is the product
(Constitution, Principle II).

**Independent Test**: Stand up an Infrahub deployment on Kubernetes with Neo4j Enterprise and
PostgreSQL running outside the namespace, take a backup, destroy the data, restore it, and
verify Infrahub serves the pre-backup data. Delivers the entire value of the feature on its own.

**Acceptance Scenarios**:

1. **Given** a deployment whose Neo4j Enterprise database runs outside the cluster and whose
   PostgreSQL runs inside it, **When** the operator takes a backup, **Then** the tool reports
   which endpoint each database was read from before reading any data, and produces a single
   backup artefact covering both databases.
2. **Given** a deployment whose Neo4j Enterprise and PostgreSQL databases both run outside the
   cluster, **When** the operator takes a backup, **Then** both databases are captured and the
   artefact records, per database, the endpoint it came from.
3. **Given** a backup artefact taken from external databases and an operator who has explicitly
   authorised restore into externally-managed databases, **When** the operator restores it,
   **Then** Infrahub workloads are quiesced first, both databases are restored, the workloads
   are returned to their prior scale, and Infrahub serves the restored data.
4. **Given** the backup utility available to the tool is incompatible with the live database
   server version, **When** the operator takes a backup, **Then** the run fails with a message
   naming both versions and no backup artefact is produced.
5. **Given** an operator who has not authorised restore into externally-managed databases,
   **When** they attempt such a restore, **Then** the tool refuses and names the authorisation
   it requires, leaving the databases untouched.
6. **Given** a deployment whose databases are all inside the cluster, **When** the operator
   takes a backup or restore, **Then** behaviour and artefact layout are unchanged from today.

---

### User Story 2 - Diagnose an unsupported or misconfigured external deployment (Priority: P2)

An operator whose deployment cannot be backed up — because the database is a Neo4j edition with
no remote-capable mechanism, because the endpoint cannot be discovered, or because the tool
lacks the cluster permissions it needs — learns precisely which condition applies and what to
change, instead of receiving a service-resolution failure.

**Why this priority**: Today's failure message names a missing service, which sent at least one
customer to the wrong conclusion ("the tool cannot find my database"). Every condition below is
reachable in the field, and each has a different remedy. This ships independently of P1 and
raises the floor for customers P1 will never serve.

**Independent Test**: Provoke each condition in turn against a test deployment and assert the
message names the condition and the operator-actionable remedy.

**Acceptance Scenarios**:

1. **Given** an external Neo4j Community database, **When** the operator takes a backup,
   **Then** the tool refuses with a message stating that the Community backup mechanism requires
   direct access to the database's own storage and is therefore unavailable for an externally
   hosted database.
2. **Given** a deployment where no database container is present and no endpoint can be
   discovered from the deployment's own configuration, **When** the operator takes a backup,
   **Then** the tool reports that the endpoint could not be determined and names the override
   available to supply it.
3. **Given** a deployment where the tool cannot query the cluster for containers because
   permission is denied, **When** the operator takes a backup, **Then** the run fails reporting
   the permission problem and does **not** fall back to treating the database as external.
4. **Given** a deployment where the tool lacks permission to create the workload it needs to
   reach an external database, **When** the operator takes a backup, **Then** the run fails
   naming the missing permission.
5. **Given** a deployment with one or both databases outside the cluster, **When** the operator
   runs environment detection — the first command an operator reaches for when a backup fails —
   **Then** the report states, per database, that it is external and where it was resolved to.

---

### User Story 3 - Run the backup from the operator's own host (Priority: P3)

An operator whose cluster cannot grant the tool permission to create workloads, but who can
reach the database directly from their own machine, takes a backup without the cluster's
involvement.

**Why this priority**: This is the shape the original request assumed, and it remains the only
option where the required cluster permissions are unavailable. It is P3 because it makes a
self-contained binary depend on a version-matched database toolchain being installed on the
operator's host — which conflicts with the tools being copied onto hosts that have no package
manager (Constitution, Principle III) — and because it requires the database to accept inbound
connections from wherever the operator sits, which is the harder ask in practice.

**Independent Test**: With the database reachable from the operator's host and the required
utilities installed, take and restore a backup with no cluster-side workload created.

**Acceptance Scenarios**:

1. **Given** an operator host with the required database utilities present and network reach to
   an external Neo4j Enterprise database, **When** the operator takes a backup in this mode,
   **Then** the artefact is byte-compatible with one taken via P1 and restorable by either path.
2. **Given** an operator host missing a required database utility, **When** the operator takes a
   backup in this mode, **Then** the tool fails before contacting the database, naming the
   missing utility.

---

### Edge Cases

- **Highly available Neo4j exposing several endpoints**: the tool must accept and use the full
  set of discovered endpoints rather than assuming one. Any of them may be a cluster follower, so
  the artefact can lag the write leader by the replication window. Since which member served the
  capture is not knowable, the artefact records the endpoints supplied and the roles observed at
  capture time — enough for an operator to see that a follower could have served it, rather than
  discovering the possibility at restore time.
- **Mixed topology with partial failure**: one database external and captured, the other
  in-cluster and failed (or vice versa). A partial capture must never be presented as a complete
  restore point.
- **A run killed mid-flight**: any workload the tool created to reach an external database holds
  database credentials and must not survive the run, including when the tool never regains
  control.
- **Credentials leaving the cluster boundary**: the credential for an externally-managed database
  must not be observable to anyone who can merely inspect the workload or its process list.
- **A restore artefact the database itself cannot reach**: the restore path requires the database
  server to fetch the artefact. If the database host has no route to the artefact store, restore
  cannot proceed and must say so before any destructive step.
- **Unattended restore**: the scheduled restore capability already shipped can reach this new
  destructive path without an operator present.
- **Repeated or concurrent runs**: two runs against the same deployment must not adopt each
  other's transient workloads or leave the deployment quiesced.

## Requirements *(mandatory)*

### Functional Requirements

**Discovery and mode selection**

- **FR-001**: The tool MUST determine independently, for each database it backs up, whether that
  database is inside or outside the deployment, so that a deployment with one of each is
  supported without additional operator input.
- **FR-002**: The tool MUST treat an external database as external only when it has positively
  established that no in-deployment database is present. A failure to query the deployment
  (permission denied, unreachable control plane, misidentified deployment) MUST surface as an
  error and MUST NOT be treated as evidence that the database is external.
- **FR-003**: The tool MUST discover an external database's endpoint and credentials from the
  configuration already present in the deployment's running components, and MUST report the
  endpoint it resolved before opening any connection to it.
- **FR-004**: Operators MUST be able to override the discovered endpoint for deployments whose
  configuration does not expose it.
- **FR-005**: The tool MUST accept multiple endpoints for a single logical database, and MUST
  record the endpoints it supplied together with each member's role as observed at capture time.
  It MUST NOT claim which member served the capture: with several endpoints supplied, that is not
  exposed by any documented interface, and asserting it would be a guess presented as provenance.

**Safety before data movement**

- **FR-006**: The tool MUST determine the live server version before reading or writing any data,
  and compare it against the version of the utility it will use. Where the utility is **older**
  than the server it MUST abort, naming both versions. Where the utility is **newer** it MUST warn
  and continue. Version comparison MUST handle both the `5.x.y` and calendar (`YYYY.MM.p`)
  numbering schemes, since ordering them as if they were one scheme misorders them silently.
- **FR-007**: The tool MUST verify, before any destructive restore step, that the database server
  can itself read the restore artefact, and MUST abort leaving the database untouched if it
  cannot.
- **FR-008**: The tool MUST refuse an external Neo4j Community backup with a message stating why
  the mechanism is unavailable, rather than reporting a missing component.

**Destructive-operation authorisation**

- **FR-009**: Restoring into a database the deployment does not manage MUST require an
  authorisation that is distinct both from the existing restore override and from enabling
  scheduled restore, so that neither confers this capability implicitly.
- **FR-010**: An unattended restore into an externally-managed database MUST be possible only
  when that authorisation has been supplied through configuration.

**Lifecycle and integrity**

- **FR-011**: Any transient workload the tool creates in order to reach an external database MUST
  be removed on every exit path, including failure and abnormal termination, and MUST be bounded
  so the deployment reclaims it even if the tool never returns.
- **FR-023**: Any object holding external database credentials MUST have its lifetime bound to
  that of the transient workload, so the deployment reclaims it by the same mechanism rather than
  by the tool remembering to. Credentials MUST NOT be able to outlive the run that created them,
  including when the tool is killed before it can clean up. Verification: kill the run between
  credential creation and workload deletion, then assert no credential object remains once the
  workload is gone.
- **FR-012**: An incomplete capture MUST NOT be retained. Either every requested database is
  captured, or the run fails and the partial artefact is removed from every location it reached.
  Completeness MUST be determined from what the backup operation reported, not from its exit status
  alone, because a backup that succeeded against only some of several endpoints is not
  distinguishable by exit status from one that failed outright. Retention and restore-point
  selection MUST NOT be required to interpret artefact metadata in order to honour this: an
  incomplete artefact never persists for them to consider. Verification: fail one component, assert
  the run fails, no artefact remains at any location, and the retention policy's decisions are
  unchanged from a run where the failure had happened before any artefact was written.
- **FR-021**: After a restore, the tool MUST confirm the restored database has reached a usable
  state before reporting success, and MUST report failure otherwise. An acknowledgement that the
  restore was accepted is not evidence that the data loaded. Verification: seed from a corrupt
  artefact, assert the run fails rather than reporting success.
- **FR-022**: Scratch space available to the backup operation MUST be explicitly sized to at
  least the size of the database being captured. The tool MUST determine that size by querying the
  server before the capture begins and applying a headroom factor, and operators MUST be able to
  override the result. Where the size cannot be determined and no override was supplied, the tool
  MUST fail naming the override rather than guess a size. Verification: capture a database larger
  than the default scratch allocation and assert the run either succeeds or fails cleanly without
  exhausting shared storage on the host it runs on; separately, make the size query fail and
  assert the run stops with the override named.
- **FR-013**: The tool MUST restore quiesced Infrahub workloads to their prior scale on every
  exit path of a restore, including failure.
- **FR-014**: Credentials for an externally-managed database MUST NOT be exposed on a process
  command line or in workload configuration readable by deployment observers.

**Compatibility**

- **FR-015**: Backups of deployments whose databases are all internal MUST be unchanged in
  behaviour and artefact layout.
- **FR-016**: Additions to backup metadata MUST be additive, so that artefacts produced by
  previously released versions remain restorable without operator action.
- **FR-024**: Metadata additions MUST NOT change the artefact's declared metadata version, and
  MUST be safe for a previously released version of the tool to ignore. Compatibility MUST hold in
  both directions: a new tool reads an old artefact, and an old tool reads a new one. Verification:
  pin both directions with tests — an older tool must neither refuse a new artefact nor act
  incorrectly on the fields it cannot see. This is only safe because FR-012 guarantees an
  incomplete artefact never reaches a reader.
- **FR-025**: Every operation against an external database MUST be time-bounded, with an
  overridable limit. A database that accepts a connection and then stops responding MUST NOT be
  able to hang the run indefinitely — particularly on the restore path, where Infrahub is scaled
  down while the operation is in flight. Verification: point the tool at an endpoint that accepts
  and then stalls; assert the run terminates and Infrahub is returned to its prior scale.
- **FR-026**: Before the destructive step of a restore, the tool MUST confirm that Infrahub
  workloads have actually stopped, not merely that a scale-down was requested. After the restore,
  Infrahub MUST serve the restored data once workloads return to their prior scale, with no manual
  intervention. Verification: assert data is served after scale-up without an additional restart.
- **FR-027**: The transient workload MUST declare a security context that satisfies a restricted
  pod-security policy, and its image MUST come from a pinned default or an explicitly supplied
  flag — never from ambient environment alone, since the tool holds workload-creation rights and
  an environment-settable image would make those rights a means of running arbitrary images.
  Verification: run against a namespace enforcing restricted pod security and assert the workload
  is admitted.
- **FR-028**: The transient workload MUST NOT be discoverable as a replica of the service it
  stands in for. Registering it so that execution resolves to it MUST NOT make it appear in
  service enumeration, replica counts, or diagnostic collection. Verification: assert service
  enumeration does not return it while it exists.
- **FR-029**: Connections to an external database MUST verify the server's certificate unless the
  operator explicitly opts out. An insecure-TLS setting discovered from the deployment MUST NOT
  silently disable verification for credentials the tool sends.
- **FR-017**: The tool MUST NOT introduce a new assumed deployment service name; external
  databases MUST be addressed under the existing documented service names.
- **FR-018**: The tool MUST detect the absence of any external runtime it depends on and fail
  with a message naming what is missing and where it was expected.
- **FR-019**: Permission to create and remove a transient workload in the deployment is a
  documented prerequisite of User Story 1. The tool MUST NOT attempt to work around its absence,
  and MUST fail naming the missing permission (see User Story 2, scenario 4).
- **FR-020**: A route from the database server to the artefact store is a documented prerequisite
  of restore. Where it is absent, the tool MUST refuse before any destructive step (FR-007) and
  MUST name the prerequisite. The tool MUST NOT offer a partial restore that leaves the two
  databases at different recovery points.

### Key Entities

- **External database endpoint**: the network location, protocol, and credentials of a Neo4j or
  PostgreSQL database that serves the deployment but is not part of it. Discovered from the
  deployment's own configuration; may be a set rather than a single value.
- **Transient backup workload**: a short-lived execution context the tool creates inside the
  deployment for the sole purpose of reaching an external database endpoint. Owned by exactly one
  run, identifiable as belonging to that run, and guaranteed not to outlive it.
- **Backup artefact** (existing): gains, per database, the endpoint it was read from, whether
  that endpoint was a replication follower, and whether the capture is complete.
- **Restore authorisation** (new): an explicit operator grant permitting destructive restore into
  a database the deployment does not manage. Separate from the existing restore override.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: An operator with an external Neo4j Enterprise database and an external PostgreSQL
  database completes a full backup and restore round trip using the documented commands, with
  zero manual steps performed on either database host beyond the documented one-time
  prerequisites.
- **SC-002**: Taking a backup requires no database port to be reachable from outside the
  deployment's own network boundary. Verified by completing a successful backup with the database
  reachable only from within that boundary.
- **SC-003**: 100% of conditions that would yield an unusable artefact — version incompatibility,
  an artefact the database cannot read, missing permissions, an unsupported database edition —
  are reported before any data is read or written. No test case produces an artefact that first
  fails at restore time.
- **SC-004**: Every backup states the endpoints it was taken from and the role each held at
  capture time, so an operator can judge the recovery point without inspecting the database — and
  can see when a follower may have served it rather than being left to assume it did not.
- **SC-005**: A destructive restore into an externally-managed database cannot occur unless an
  operator has explicitly authorised that specific capability; enabling scheduled restore alone
  never confers it.
- **SC-006**: No run leaves a transient workload, or a quiesced Infrahub deployment, behind —
  including runs terminated abnormally. Verified across the full set of interruption points
  exercised by the end-to-end suite.
- **SC-007**: Every existing backup and restore scenario for fully-internal deployments passes
  unchanged, and artefacts from the previous released version restore without operator action.
- **SC-008**: An operator hitting an unsupported configuration can identify the remedy from the
  error message alone, without reading source or filing a support request.

## Assumptions

- **Reachability is already proven in the direction that matters.** The deployment reaches the
  external database today, because Infrahub is running against it. The P1 design depends on that
  existing path and requires no new network permission at the database for backup.
- **Documented one-time prerequisites on the customer's side** (referenced by SC-001; established
  by Phase 0 research and not assumed):
    1. The Neo4j backup service must listen on a non-loopback address. It is enabled by default
       but bound to localhost, so backups taken from anywhere other than the database host itself
       require a server configuration change — not merely a firewall rule, which is what the
       original request assumed was sufficient.
    2. Each Neo4j server must be able to retrieve the backup artefact from the object store, which
       for the current seed mechanism means having the cloud provider's command-line tooling
       installed and credentials configured server-side. Object stores are supported without any
       additional server-side provider configuration; other artefact sources are not.
    3. If the backup service requires TLS, the tool must be given a compatible client policy.
- **The deployment knows where its databases are — but not everything needed.** Components still
  running inside the deployment carry the external host list and credentials in their
  configuration, and already express a multi-member database as a comma-separated host list, so
  discovery needs no new operator input for those. It cannot supply the backup service's port,
  because Infrahub never uses that port and does not record it; that value is defaulted and
  overridable. FR-004 covers both exceptions.
- **Scope is Kubernetes only for v1.** Docker Compose deployments with external databases are out
  of scope; the same reasoning applies to them and can be added later.
- **External Neo4j Community is out of scope, permanently for this feature.** Its backup
  mechanism requires the database to be stopped and its storage directly accessible, which no
  network-reachable design can satisfy. Community deployments remain on the in-deployment path.
- **Neo4j Enterprise is the target edition**, consistent with the customers requesting this.
- **A remote restore mechanism exists for Neo4j Enterprise.** The restore design assumes the
  server can be directed to seed a database from an artefact it retrieves itself. This is
  load-bearing for User Story 1 and MUST be verified against the target server versions during
  planning research before implementation begins.
- **Backup utilities tolerate version skew in one direction.** A newer utility reads an older
  server; the reverse fails. For PostgreSQL this is documented and the failure is a loud refusal
  from the utility itself. For Neo4j no client/server matching rule is documented at all, so
  FR-006 enforces only the direction with a known failure mode and warns on the other. The exact
  tolerated envelope is measured by the end-to-end suite rather than asserted here — encoding a
  guessed rule in an abort condition would make the tool refuse work it could have done.
- **Operators know their database version** and can declare it, so the tool need not derive it
  before it has any means of asking the server.
- **Highly available external databases are in scope for backup** (FR-005), but measuring or
  bounding replication lag is not; the tool records the source endpoint and leaves the recovery
  point interpretation to the operator.
- **Customers will grant permission to create a transient workload** in the Infrahub namespace.
  This is the pivot of User Story 1 and is assumed rather than proven. It is a modest grant
  relative to one the tool already exercises — restore scales Infrahub workloads up and down —
  which is the basis for assuming it. If a customer refuses, they are served by User Story 3, not
  by a fallback inside User Story 1.
- **Running the dump utility on the operator's host is deferred, not rejected** (User Story 3).
  It stays specified so the permission assumption above has a defined consequence when it fails,
  rather than leaving the feature with no answer.
- **The database host can reach the artefact store.** For customers already backing Infrahub up
  to an object store, this is likely to be true incidentally. It is nonetheless a hard
  prerequisite of restore, and a locked-down database estate cannot be served by v1. An
  operator-staged artefact — where the operator places the artefact somewhere the database can
  read it and the tool drives the restore — was considered and left out of v1: it would break
  User Story 1's promise of no manual database-side steps, so adding it later means revising that
  story rather than extending it.
- **Partial restore is not an escape hatch.** Restoring only PostgreSQL where Neo4j cannot be
  reached was considered and rejected: it leaves the two databases at different recovery points,
  which is a worse outcome than refusing, and it re-opens the backup-only shape that was already
  ruled out on the grounds that a round trip which does not close is a compliance artefact rather
  than a recovery capability.
- **This does not reverse spec 006.** That work established that Kubernetes support depends on
  execution staying in the tool, with vendor utilities run inside a container and bytes streamed
  out through primitives both deployment backends implement. This feature reuses exactly that
  model and only supplies a container to execute in when the deployment has none. Spec 006
  separately declined a container-hosted *snapshot writer*; that is a different layer and remains
  declined.
