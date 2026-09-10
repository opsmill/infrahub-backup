# Phase 0 Research: External Database Backup and Restore

**Feature**: 007-external-database-backup | **Date**: 2026-09-02

The spec carried two load-bearing assumptions and several unverified mechanics. All are resolved
below. Six findings change what the spec must say; they are collected in [Spec impact](#spec-impact).

---

## R1 — Remote online backup for Neo4j Enterprise

**Decision**: Use `neo4j-admin database backup --from=<host:port>[,<host:port>…] --to-path=…`,
executed from a transient pod inside the namespace, streaming the artifact out through the
existing execution primitives.

**Rationale**: This is not a workaround — it is the vendor's documented topology. From the
operations manual:

> It is best practice, but not mandatory, to perform the backup from a server on the same network
> as the database, but that is not part of the cluster. You should install Neo4j on that machine
> to make the `neo4j-admin` command available. This machine is known as a **backup client**.

The transient pod *is* a backup client. The manual also notes the command "uses a significant
amount of resources, such as memory and CPU. Therefore, it is recommended to perform the backup
on a separate dedicated machine" — which is an additional argument against User Story 3 running
it on an operator's laptop, and against running it inside an application pod.

`--from` takes a "comma-separated list of host and port of Neo4j instances, each of which are
tried in order", and providing several is *recommended* for clusters "since that may allow a
backup to succeed even if some server is down, or not all databases are hosted on the same
servers". This is precisely FR-005.

**Alternatives considered**:

- *Operator-host execution* (User Story 3): still valid, still P3. The vendor guidance about
  dedicated resources and the JVM prerequisite both count against it as the primary path.
- *`--to-path` pointing directly at cloud storage* (supported from 2025.03): rejected. It would
  bypass the Plakar snapshot pipeline and with it deduplication, encryption, retention and
  checksums. It also does not avoid local disk: "Backing up directly to cloud storage still
  requires local disk space on the machine running the backup command."

---

## R2 — Remote restore for Neo4j Enterprise

**Decision**: Restore by seeding from a URI over Bolt —
`CREATE DATABASE <db> OPTIONS { existingData: 'use', seedURI: 's3://…' }` — against an object
store, using the built-in cloud seed provider. Poll `SHOW DATABASES` to confirm the seed actually
loaded.

**Rationale**: Confirmed available and explicitly listed as a supported way to restore a backup
artifact on both standalone and clustered servers. The artifact produced by R1 is directly usable:
"The seed can be a full backup, a differential backup, or a dump from an existing database."

Three findings make this materially better than the spec assumed:

1. **Cloud storage is built in; HTTP and file are not.** "Amazon S3, Google Cloud Storage, and
   Azure Cloud Storage are supported by default, but the other providers require configuration of
   `dbms.databases.seed_from_uri_providers`." The spec assumed the opposite — that object storage
   needed a plugin and HTTP was built in. The chosen medium is the one that needs no server-side
   provider configuration.
2. **`CREATE OR REPLACE DATABASE` accepts a seed URI** from 2025.01, so no separate `DROP` is
   required. The operation is still destructive, but it is a single atomic replace rather than a
   drop-then-create window in which the database does not exist.
3. **Custom S3 endpoints are supported** via `AWS_ENDPOINT_URL_S3` / `aws.endpointUrlS3`, named
   for Ceph, MinIO and LocalStack. This lines up with the existing `--s3-endpoint` flag.

**Critical correctness finding**: seeding is **asynchronous and fails silently to the caller**.

> Download and validation of the seed is only performed as the new database is started. If it
> fails, the database is not available and it has the `statusMessage`: `Unable to start database`
> of the `SHOW DATABASES` command.

A successful `CREATE DATABASE` therefore does **not** mean the restore worked. The tool MUST poll
`SHOW DATABASES` for `currentStatus` and `statusMessage` and treat "created but not online" as a
failed restore. Reporting success here would be exactly the silent data-loss failure Principle II
exists to prevent.

**Seed provider choice**: `CloudSeedProvider` is current and supports backup chains and
`seedRestoreUntil`, but **requires the AWS CLI installed on each Neo4j server** plus
`~/.aws/credentials`. `S3SeedProvider` needs no AWS CLI (bundled libraries) but is **deprecated in
5.26** and requires `seedCredentials` with identical keystores configured across every cluster
server. Neither is free.

**Decision**: target `CloudSeedProvider` and document the AWS CLI plus server-side credential
configuration as a customer prerequisite. Rationale: it is the non-deprecated path, it needs no
keystore setup across cluster members, and credentials stay in the customer's own server
configuration rather than travelling inside a Cypher statement.

**Alternatives considered**:

- `neo4j-admin database restore`: rejected, requires filesystem access to the server's store.
- `dbms.recreateDatabase()`: a genuine second Cypher-level option, listed for both standalone and
  clustered restore. Not chosen for v1 — seed-from-URI is the documented restore-from-backup path
  and is simpler to reason about. Worth revisiting if the seed prerequisites prove onerous.
- `neo4j-admin database copy` from a backup (2026.03+): too new to require of customers.
- `S3SeedProvider` with `seedCredentials`: rejected, deprecated and needs cluster-wide keystores.

---

## R3 — Version compatibility

**Decision**: The transient pod's image version is operator-declared (FR-004) and defaults to
matching the discovered server version. The tool probes the server version over Bolt and warns —
rather than aborting — when the client is *newer* than the server; it aborts when the client is
*older*.

**Rationale**: The documentation does **not** state a strict client/server matching rule for the
backup command, so the spec's assumption of strict major.minor matching is unverified and probably
too strong. What is documented, for restore, is forward tolerance: a backup artifact "can be
restored within the same or to a later Neo4j version". Extrapolating the same direction to the
backup client is reasonable but unproven, so the rule is enforced asymmetrically and the exact
compatibility envelope is established empirically in the end-to-end suite rather than asserted.

Neo4j has moved to calendar versioning (2026.07 current, 5.26 LTS), so version comparison must
handle both `5.x.y` and `YYYY.MM.p` schemes. A naive semver comparison silently misorders them.

**PostgreSQL**: `pg_dump` reads servers older than itself, and refuses servers newer than itself;
a custom-format dump restores into the same or a later major version. So "use a client at least as
new as the server" is the safe rule, and the failure mode when violated is a loud refusal from the
tool itself rather than a corrupt dump. This is the asymmetry FR-006 already describes.

**Alternatives considered**: two-phase discovery (spin a default image, probe, respin matched).
Rejected in the design session and still rejected — two pod lifecycles per run and a second failure
surface, to derive something the operator knows.

---

## R4 — Endpoint discovery

**Decision**: Read the external endpoint from the environment of the in-namespace Infrahub
components. Use `INFRAHUB_DB_ADDRESS` for the host list, `INFRAHUB_DB_PROTOCOL`, `INFRAHUB_DB_PORT`,
and the TLS settings for the Bolt connection; default the **backup** port to 6362 with a flag
override. PostgreSQL host and port come from `pgx.ParseConfig` of the Prefect connection URL —
fields already parsed and currently discarded.

**Rationale**: Verified against Infrahub's own `DatabaseSettings` (env prefix `INFRAHUB_DB_`):

| Setting | Default | Relevance |
|---|---|---|
| `address` | `localhost` | **"Database host, or a comma-separated list of cluster members in `host[:port]` format. Members without an explicit port use the value of the port setting."** |
| `port` | `7687` | Bolt, **not** the backup port |
| `protocol` | `bolt` | `neo4j://` for routed/HA deployments |
| `database` | *(none)* | pattern-validated name |
| `policy` | *(none)* | routing policy |
| `tls_enabled` / `tls_insecure` / `tls_ca_file` | `false` / `false` / *(none)* | Bolt TLS for the restore and version-probe connections |

Two consequences:

1. **Infrahub already models the multi-endpoint case in exactly the shape `--from` wants** — a
   comma-separated `host[:port]` list. FR-005 needs no new concept, just faithful propagation.
2. **Discovery is partial by construction.** `INFRAHUB_DB_PORT` is the Bolt port; the backup
   listener port is not part of Infrahub's configuration at all, because Infrahub never uses it.
   It must be defaulted to 6362 and overridable. The spec's FR-003 implies discovery yields
   everything needed; it does not.

Note this repo's own docs and code reference only `INFRAHUB_DB_DATABASE`, `_USERNAME` and
`_PASSWORD`. `ADDRESS`, `PORT`, `PROTOCOL` and the TLS keys are real Infrahub settings that this
tool has simply never read.

---

## R5 — Creating and reaping the transient pod without a Kubernetes client library

**Decision**: Apply a full `Pod` manifest through `kubectl` on stdin, using the executor's
existing write-pipe primitive. Reap with `kubectl delete`. No client library.

**Rationale**: ADR-0001 and Principle III both forbid taking on a Kubernetes client library, and
nothing here needs one. The Kubernetes backend already drives every operation through `kubectl`
subprocesses and already owns a stdin-writing primitive (`runCommandWritePipe`), so a manifest
applied on stdin is a use of existing machinery rather than a new mechanism. A manifest is required
rather than a bare `kubectl run` because the pod needs a run-ID label, a deadline, resource limits,
ephemeral storage sizing, and credentials from a Secret — all of which a manifest expresses
directly.

**Required permissions** (to be documented as the FR-019 prerequisite): `pods` create/delete/get/
list, `pods/exec` create, `pods/log` get, and `secrets` create/delete if credentials are passed
that way. `get`/`list` on pods is already required today.

**Lifecycle bounding**: `activeDeadlineSeconds` on the pod, so the cluster reclaims it even if the
tool never returns; `restartPolicy: Never`; a run-ID label so a later run can identify and reap
strays. This satisfies FR-011 without the tool having to survive its own termination.

**Newly discovered sizing constraint**: the backup command stages the store and transaction logs
into a local temporary directory before producing the artifact, and that location "must have free
space at least equal to the size of the database being backed up". The transient pod therefore
needs explicitly sized ephemeral storage or a volume. Left implicit, a large database fills the
node's disk and takes neighbouring workloads with it — a backup operation damaging the running
instance, which Principle II forbids.

**Alternatives considered**: `kubectl run` with `--overrides` (same result, more awkward escaping);
a `Job` rather than a `Pod` (adds a controller and a second object to reap, and the tool wants to
`exec` into a specific pod anyway); `kubectl debug` (attaches to an existing pod, which is the case
that does not exist here).

---

## R6 — Distinguishing partial success from failure

**Decision**: Do not rely on the backup command's exit code alone to decide artifact completeness.
Treat a non-zero exit as failure by default, and parse the command's own output to identify the
"succeeded but some servers were uncontactable" case, which is reported as an incomplete-source
warning recorded in metadata rather than as a successful backup.

**Rationale**: The exit codes conflate the two outcomes. Code `1` means "Backup failed, or
**succeeded but encountered problems such as some servers being uncontactable**", and for multiple
databases, "One or several backups failed, **or succeeded with problems**". There is no exit code
that distinguishes them. Since FR-012 forbids presenting a partial capture as complete, and FR-005
actively encourages passing several endpoints (which makes the partial case *more* likely), this
distinction cannot be skipped.

**Alternatives considered**: back up from a single endpoint to avoid the ambiguity. Rejected — it
throws away the availability benefit that made multi-endpoint the vendor recommendation.

---

## R7 — Credential handling

**Decision**: Pass database credentials to the transient pod through a Secret referenced by
`envFrom`, created and deleted within the run's lifecycle and labelled with the same run ID.

**Rationale**: FR-014 forbids exposure on a process command line or in readable workload
configuration. Inline `env` values in a pod spec are readable by anyone who can `get pod`, and the
existing `ExecOptions{Env}` path places values on the exec command line, visible in API-server
audit logs and in-pod process listings. A Secret is the only option here that is both available
without new dependencies and not readable from the pod object itself.

**Alternatives considered**: stdin (works for `pg_dump` via `PGPASSFILE`-style indirection but not
uniformly for `neo4j-admin`); a projected service-account token (not applicable — these are
database credentials, not cluster credentials).

---

## R8 — Test strategy

**Decision**: Extend the existing end-to-end suite with external-topology variants alongside the
current `tests/e2e/test_k8s_*.py` files: a database deployed outside the Infrahub namespace, with
the Infrahub components pointed at it by `INFRAHUB_DB_ADDRESS`.

**Rationale**: Principle IV requires "CI-level end-to-end backup/restore jobs for flows that touch
containers", and the suite already covers the Kubernetes tarball, S3 and Plakar paths. The external
topology is a fixture change — where the database runs and what the application's environment says
— not a new test framework. The compatibility envelope left open in R3 is established here.

---

## Spec impact

Six findings require the spec to change. Five are additive; one is a requirement correction.

| # | Finding | Spec change |
|---|---|---|
| 1 | The backup listener defaults to `127.0.0.1:6362` and "needs to be changed if backups are to be taken from another machine" | **New prerequisite** in Assumptions: a non-loopback `server.backup.listen_address` on the customer's server. This is a server-side configuration change, not merely a firewall rule — the original request assumed only the latter. |
| 2 | Cloud seed providers are built in; the AWS CLI must be present on each Neo4j server for `CloudSeedProvider` | **New prerequisite** in Assumptions, and it replaces the spec's inverted assumption that object storage needed a plugin while HTTP was built in. |
| 3 | Seeding validates only at database start; `CREATE DATABASE` returning success proves nothing | **New requirement**: confirm the restored database reaches an online status before reporting success. FR-007's pre-flight does not cover this, because the failure happens after the destructive step. |
| 4 | Exit code `1` conflates outright failure with partial success | **Strengthen FR-012**: completeness must be determined from command output, not exit status alone. |
| 5 | The backup command needs local scratch space at least the size of the database | **New requirement**: the transient pod's ephemeral storage must be explicitly sized, or a large database threatens the node and therefore the running instance (Principle II). |
| 6 | No documented strict client/server version match for the backup command; restore is documented as forward-tolerant | **Correct FR-006**: abort when the client is older than the server, warn when newer. As written, FR-006 asserts a symmetric rule the vendor documentation does not support, and would abort on combinations that work. |

Item 6 changes an existing requirement rather than adding one, so it needs the feature owner's
agreement before the spec is edited.

## Remaining uncertainty

The exact backup client/server compatibility envelope (R3) is not documented and is deliberately
left to be measured by the end-to-end suite. This is recorded as a known unknown rather than
resolved on the page, because guessing a rule and encoding it in an abort condition is how a tool
starts refusing work it could have done.
