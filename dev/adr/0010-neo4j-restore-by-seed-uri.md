# 10. Remote Neo4j restore by seeding from an object-store URI, confirmed by polling database status

**Status**: Accepted
**Date**: 2026-09-07
**Source**: specs/archive/007-external-database-backup/research.md (R2), spec FR-007/FR-020/FR-021, contracts/artifact-and-metadata.md ("Restore-side staging")

## Context

Every restore path in this toolset runs the vendor's restore utility inside the database container
against the server's own store. An external Neo4j Enterprise has no container in the deployment
and its store is not reachable over any network protocol the tool speaks. The vendor documents
two Cypher-level ways to restore a backup artifact on standalone and clustered servers; both make
the *server* fetch the artifact. That inverts the data path: instead of the tool moving bytes into
a container, the tool has to put the artifact somewhere the server can read and then ask.

## Decision

- **Seed from a URI over Bolt**, from the restore workload:
  `CREATE OR REPLACE DATABASE <db> OPTIONS { existingData: 'use', seedURI: '<uri>' }` where the
  server version supports the atomic replace (2025.01 and later); an older server gets the
  two-statement form. Executed in the form the probed server version accepts, never guessed.
- **Cloud object stores only**: `s3`, `gs` and `azb` are the seed providers a server carries as
  shipped. Every other scheme (`http`, `https`, `file`) needs `dbms.databases.seed_from_uri_providers`
  configured on each server, which the tool cannot observe and must not depend on.
- **Staging reuses the operator's configured object store** (`--s3-*`), under one extra key
  segment: `s3://<bucket>/<prefix>/seed/<database>-<timestamp>.backup`. The raw vendor artifact is
  staged, extracted from the archive, not the archive itself. A run with no object store configured
  is refused before anything destructive, naming the flags.
- **Readability is verified by an independent reader before the destructive step** (FR-007): a
  client built from the URI that will be published, not the one that uploaded, stats the object.
  A prefix that does not compose or a bucket the endpoint resolves differently fails here, not as
  a database left offline.
- **Acceptance is not completion** (FR-021). Seed download and validation happen only when the new
  database starts, and a failure surfaces as `SHOW DATABASES` reporting the database offline with
  a `statusMessage`. The tool polls that listing every 5 seconds within the operation bound, reads
  every member's row, and reports success only when the database is online. A status that can no
  longer be read at all is attributed to the path to the database, not to the database.
- **The staged object is removed only after the database reports online.** Until then the server
  may still be fetching it. A failed restore leaves it in place with its URI in the error.
- **Both databases or neither** (FR-020). A restore that cannot reach one database refuses rather
  than restoring the other and leaving two recovery points.

## Consequences

- Customer prerequisites that no earlier feature had: each Neo4j server needs a route to the
  object store and, for the current `CloudSeedProvider`, the cloud provider's CLI and credentials
  configured server-side. These are documented, not assumed, and the tool cannot verify them from
  outside.
- The extra key segment keeps the seed invisible to retention, which recognises an object only
  where its key is exactly the one a backup's own name produces. No keep slot is consumed and no
  good backup is evicted.
- An external Neo4j Community database cannot be restored this way — seeding creates a database,
  which Community cannot do — and is refused with a message saying so (FR-008's restore side).
- Restore version tolerance is forward-only: an artifact restores into the same or a later server
  version.

## Alternatives Considered

- **`neo4j-admin database restore`** — requires filesystem access to the server's store.
- **`dbms.recreateDatabase()`** — a genuine second Cypher-level option; not chosen for v1 because
  seed-from-URI is the documented restore-from-backup path and simpler to reason about. Worth
  revisiting if the seed prerequisites prove onerous.
- **`neo4j-admin database copy` from a backup** (2026.03+) — too new to require of customers.
- **`S3SeedProvider` with `seedCredentials`** — deprecated in 5.26 and needs identical keystores on
  every cluster member; credentials would also travel inside a Cypher statement.
- **HTTP or file seeds** — need server-side provider configuration the tool cannot see.
- **Operator-staged artifact** — considered and left out of v1: it breaks the promise of no manual
  database-side steps.
- **Report success on `CREATE DATABASE` returning** — the silent data-loss failure Principle II
  exists to prevent.
