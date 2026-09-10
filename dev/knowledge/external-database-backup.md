<!-- Extracted from specs/007-external-database-backup on 2026-09-07 -->

# External Database Backup and Restore

## Overview

`infrahub-backup` can back up and restore an Infrahub deployment on Kubernetes whose Neo4j
Enterprise database, PostgreSQL task-manager database, or both run **outside** the deployment's
namespace. Every backup and restore path in this toolset runs the vendor's utility inside a
container and streams bytes out through the exec, stream and copy primitives; an external
database has no such container, so the tool creates one — a short-lived *transient workload* in
the namespace — and registers it under the existing service name. The layers above cannot tell
the difference, and a deployment whose databases are all internal is untouched.

Code: `src/internal/app/external_*.go`, `transient_workload.go`, `kubernetes_ownership.go`,
`kubernetes_pod_match.go`, `kubernetes_workloads.go`. Operator documentation:
`docs/docs/backup/external-databases.mdx`. Decisions: ADRs 0008 to 0016.

Scope of v1: Kubernetes only. Neo4j **Enterprise** only for the external path — external
Community is refused, because its backup mechanism needs the database stopped and its storage
directly accessible.

## How a run decides where a database lives

Location is settled per database at a *gate* (`resolveDatabaseTargets`) before anything is
stopped, and it is the only way a run obtains a `databaseTarget`. Holding one is the evidence the
question was answered; the capture and restore preparers refuse a target that did not come through
the gate.

| Evidence | Location |
|---|---|
| A pod or workload the deployment *claims* (label declaration, or a name the declared release prefixes claim — ADR 0016) | internal |
| The namespace listing succeeded and nothing claimed the service | external |
| The listing failed (permission, unreachable control plane) | **error** — never "external" (ADR 0012) |

The decision is memoised for the run (`serviceLocations`); a restore's scale-down changes which
pods exist, not where a database lives.

## Endpoint discovery

The endpoint is read from the environment of the components still running inside the deployment.
Reading these is additive; a deployment that does not set them behaves as before.

| Variable | Used for |
|---|---|
| `INFRAHUB_DB_ADDRESS` | Neo4j host list, comma-separated `host[:port]`, already the shape `--from` wants |
| `INFRAHUB_DB_PORT` | Bolt client port (default 7687) — **not** the backup port |
| `INFRAHUB_DB_PROTOCOL` | `bolt` or `neo4j` (routed form for multi-member deployments) |
| `INFRAHUB_DB_DATABASE`, `_USERNAME`, `_PASSWORD` | Already read before this feature |
| `INFRAHUB_DB_TLS_ENABLED`, `_TLS_INSECURE`, `_TLS_CA_FILE` | Client-connection TLS; `_TLS_INSECURE` is reported, never obeyed |
| Prefect connection URL (task-manager) | PostgreSQL host, port, database, user, password, `sslrootcert` |

Two things discovery cannot supply: the Neo4j **backup** port (6362 by default, Infrahub never
records it) and any value the deployment does not export. The PostgreSQL URL is parsed for what
it *states* — an absent host stays absent, because `pgx` would otherwise fill it from the
operator's own `PGHOST`. The resolved endpoint is logged before any connection is opened.

## The transient workload

One pod per role per external database, created from a full manifest piped to `kubectl create`
on stdin, with a credential Secret that carries an `ownerReference` to the pod (ADR 0009).

| Role | When | Scratch | Hosts |
|---|---|---|---|
| probe | at the gate, before anything is stopped | 1 GiB fixed | version, edition, store size, member roles |
| capture | around the capture itself | 2× queried store size, or `--external-db-scratch-size` | `neo4j-admin database backup --from=…` or `pg_dump`, then the copy out |
| restore | at the gate, held for the run | 10 GiB default, overridable | the seed statement and status poll, or the dump copy-in and `pg_restore` |

Identity and lifecycle:

- name prefix `infrahub-backup-xdb`, label `infrahub-backup/transient=true`, a run-ID label;
  `restartPolicy: Never`; `activeDeadlineSeconds` = readiness timeout + calls hosted × bound +
  margin (`externalDBWorkloadDeadline`).
- images pinned in the binary (`neo4j:2025.10.1-enterprise`, `postgres:18-alpine`), overridable by
  flag only; the pull is load-bearing (ADR 0008).
- restricted pod-security context; the official images declare no user, so `runAsUser` is set
  explicitly — see [container-image-user-semantics.md](container-image-user-semantics.md).
- a stray reaper deletes marker-labelled pods and Secrets from earlier runs that are terminated or
  past their deadline, before a new workload is created.
- excluded from every pod listing by the marker label first and the name prefix second, so it never
  appears in service enumeration, replica counts or `infrahub-collect` output.

## CLI surface

Flags follow the repository convention (root command, viper-bound, `INFRAHUB_` environment
equivalent with `-` → `_`) except where marked.

| Flag | Default | Env-bound | Purpose |
|---|---|---|---|
| `--neo4j-address` | discovered | yes | Override the Neo4j host list |
| `--neo4j-backup-port` | 6362 | yes | Backup listener port (not discoverable) |
| `--postgres-address` | discovered | yes | Override the PostgreSQL endpoint |
| `--external-db-scratch-size` | 2× store size | yes | Scratch for the capture workload |
| `--external-db-timeout` | 2h | yes | Bound on any single external operation |
| `--external-db-insecure-tls` | false | yes | The only TLS opt-out (ADR 0015) |
| `--allow-external-restore` | false | **separate channel** | Authorisation for a destructive external restore; the env key `INFRAHUB_ALLOW_EXTERNAL_RESTORE` is read on its own so flag and config are distinguishable (ADR 0014) |
| `--external-db-image-neo4j`, `--external-db-image-postgres` | pinned | **no** | Tooling images; flag-only because the tool holds workload-creation rights |
| `--external-db-image-pull-secrets`, `--external-db-service-account`, `--external-db-node-selector`, `--external-db-tolerations`, `--external-db-priority-class` | none | yes | What the operator's cluster requires of a pod it did not author |

## Prerequisites

Customer side (documented, verified by the end-to-end suite, not assumable by the tool):

1. The Neo4j backup service listens on a non-loopback address (`server.backup.listen_address`); it
   is bound to localhost by default, so this is a server configuration change, not a firewall rule.
2. Each Neo4j server can fetch from the object store: a route, plus the cloud provider's CLI and
   credentials configured server-side for `CloudSeedProvider`.
3. A privately signed server certificate has its CA recorded in the deployment (`_TLS_CA_FILE` or
   `sslrootcert`), or the operator opts out of verification.
4. In an air-gapped cluster, the tooling image is mirrored and its pull secret named.

Cluster side: `create`, `get`, `list`, `watch`, `delete` on pods and secrets, and `create` on
`pods/exec`, in the Infrahub namespace. `watch` is needed by `kubectl wait`. These are the first
write verbs the tools require and only the external path needs them. `infrahub-collect` needs none
of this and refuses `--include-backup` for an external database (ADR 0003).

## Backup

1. Gate: location per database, edition and version probe (abort when the utility is older than
   the server, warn when newer — ADR 0011), store size for scratch sizing, member roles.
2. Capture: `neo4j-admin database backup --from=<host:port>,…` with the full endpoint list in try
   order, or `pg_dump` from the member that answered the probe. Artifact layout is byte-compatible
   with an internal capture, so either restores by the same code.
3. Completeness is read from the command's output, not its exit status (exit `1` means "failed" or
   "succeeded with uncontactable servers"). An incomplete capture fails the run and is removed from
   every location it reached (ADR 0013).

## Restore

1. Gate: `--allow-external-restore` (or its env key for unattended runs), location, and a restore
   workload created and probed before anything is stopped.
2. Neo4j: the raw `<database>-<timestamp>.backup` is staged at
   `s3://<bucket>/<prefix>/seed/…` in the operator's configured object store, verified readable by
   an independent client, then `CREATE OR REPLACE DATABASE … seedURI` is issued over Bolt. The tool
   polls `SHOW DATABASES` every 5 s within the operation bound and reports success only when the
   database is online; a status that cannot be read is attributed to the path to the database, not
   the database. The staged object is removed only then (ADR 0010).
3. PostgreSQL: the dump is copied into the restore workload and loaded with the client tooling.
4. Infrahub workloads are quiesced first, confirmed stopped (not merely asked), and returned to
   their prior scale on every exit path. Both databases restore or neither does.

## Metadata

Additive fields, metadata version unchanged; absence keeps its meaning (ADR 0013).

| Field | Absent means |
|---|---|
| `source_endpoints` (per database, try order) | a pre-feature artifact |
| `source_roles` (endpoint → leader/follower/unknown) | roles not observed; `unknown` is a real value |
| `capture_complete` | complete — should never be read as `false`, that artifact was removed |
| `external_location` (per database) | internal |

No field claims which endpoint served the capture; with several supplied that is not knowable.

## Bounds

| Bound | Value | Applies to |
|---|---|---|
| operation bound (`--external-db-timeout`) | 2 h default | capture, copy out, seed, restore, status poll window |
| probe bound | 5 min | each probe statement |
| control bound | 2 min | `kubectl` reads, scratch cleanup, CA placement |
| workload readiness | 10 min | `kubectl wait` after create (covers the image pull) |
| deadline margin | 10 min | added to every pod's `activeDeadlineSeconds` |
| quiesce confirmation | 5 min | Infrahub workloads reporting stopped |
| return to scale | 10 min | Infrahub workloads reporting running again |

## Failure-message contract

Each message names the condition, the resource and the operator action. The distinctions that
matter most: a deployment query that *failed* is never reported as an absent database; settings
that could not be *read* are reported differently from settings that record no address; a
restored database that did not come online is reported differently from one whose status cannot be
read at all. The full table is in `specs/archive/007-external-database-backup/contracts/cli-surface.md`.

## Gotchas

- The CA is read from a running component at the gate. A path that needs it after an offline
  capture has scaled that component away gets the memoised copy; a fresh read there would fail and
  degrade to the system trust store.
- An explicit `--k8s-namespace` that fails a soft `kubectl` check can still fall back to Docker
  detection on a host running a live compose project; pin the namespace and check
  `environment detect` first (residual finding T156).
- The store-size metric the probe queries is absent on Neo4j 2025.x, so those captures need
  `--external-db-scratch-size` (residual finding T158).
- Every kubectl value the backup path parses is read from stdout alone; a kubectl notice on stderr
  used to be prepended to a Secret's owner UID and leave the credential behind.
