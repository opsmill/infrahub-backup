# 12. Database location is decided from positive in-deployment evidence; a failed query is never "external"

**Status**: Accepted
**Date**: 2026-09-07
**Source**: specs/archive/007-external-database-backup/research.md (R4), spec FR-001/FR-002/FR-003/FR-004, data-model.md (`DatabaseEndpoint`)

## Context

Before spec 007 the tool resolved a service name to a container and failed when none existed. The
external path gives that failure a second meaning — "the database lives outside the deployment" —
and the two meanings have opposite remedies. A permission problem, an unreachable control plane
or a misidentified namespace also yields "no container"; reading any of those as "external" would
send the tool to create workloads and pull images on the strength of a question that was never
answered, and would tell an operator to configure an external database they do not have.

Discovery also has a limit: the components running inside the deployment carry the external host
list, protocol, client port and TLS settings in their environment, but the Neo4j *backup* listener
port is not part of Infrahub's configuration at all, because Infrahub never uses it.

## Decision

- **Location is decided per database at a gate, before anything is stopped**, and a run obtains
  its database targets only from that gate. Holding a target is the evidence the question was
  answered; both the capture and the restore preparers refuse a target that did not come through
  it.
- **Internal** when a pod or workload the deployment claims exists (see ADR 0016 for what "claims"
  means). **External** only when the namespace listing *succeeded* and nothing claimed the service.
- **A query that failed is an error, never a location.** The refusal names the query that failed
  and the permission that would be missing, and explicitly does not say the database is absent.
  This is what keeps a permission problem from manufacturing a transient pod or an image pull.
- **The endpoint is read from the deployment's own components**: `INFRAHUB_DB_ADDRESS` (a
  comma-separated `host[:port]` list, already the shape the backup command wants), `_PORT` (the
  Bolt port), `_PROTOCOL`, `_TLS_ENABLED`/`_TLS_INSECURE`/`_TLS_CA_FILE`; for PostgreSQL, the
  Prefect connection URL — reading only what the string itself states, because `pgx` fills an
  absent host from the *operator's* `PGHOST` and would make the operator's laptop the discovered
  database.
- **The backup port is defaulted (6362) and overridable**, and every discovered value has a
  per-database flag override. The resolved endpoint is reported before any connection is opened.
- **The decision is memoised per run**: a database does not move out of the deployment
  mid-backup, and a restore's scale-down changes which pods exist, not where the database lives.

## Consequences

- A deployment whose databases are all internal never reaches a new setting, flag or pull; its path
  is unchanged (FR-015).
- Discovery is partial by construction and says so: the message for an undiscoverable endpoint
  names where it looked and the override flag, while the message for settings that could *not be
  read* names the cluster's reason and both remedies — two different conditions, two different
  texts.
- Overriding one database's endpoint never changes the other's resolution.
- Every downstream reader of "where does this database live" can read a memo instead of asking
  the cluster again.

## Alternatives Considered

- **Treat "no container found" as external** — the original failure mode, restated as a feature.
  Rejected: a denied listing would silently become a workload-creating path.
- **Operator declares the location** — pushes onto the operator a fact the deployment already
  records, and offers no protection against a wrong declaration.
- **Dial the database from the operator's host** — deferred as User Story 3; needs the database to
  accept inbound connections from wherever the operator sits, the harder ask in practice.
- **Adopt `pgx`'s parsed host and port** — pgx always supplies both, from libpq defaults, so a URL
  with no host resolved to the machine running the tool.
