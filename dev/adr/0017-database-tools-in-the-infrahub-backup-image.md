# 17. The database tools ship in the infrahub-backup image

**Status**: Accepted
**Date**: 2026-10-12
**Supersedes**: the image choice in ADR 0008 (the rest of ADR 0008 stands)
**Source**: review of [PR #172](https://github.com/opsmill/infrahub-backup/pull/172) on ADR 0008 ("ship `neo4j-admin` and `pg_dump` `pg_restore` within our infrahub-backup docker image instead, would be easier"), decided by the PR author

## Context

ADR 0008 settled that a database outside the deployment is reached from a temporary workload in
the deployment's namespace, and that the workload runs the vendors' own images
(`neo4j:<version>-enterprise` for `neo4j-admin` and `cypher-shell`, `postgres:18-alpine` for
`pg_dump`, `pg_restore` and `psql`). Both images are pinned in the binary.

That choice puts two third-party images on the external path. An air-gapped operator has to
mirror both, alongside the `infrahub-backup` image they may already mirror to run the tool in the
cluster. The tool also cannot run the database tools itself when it already runs as a pod in the
namespace, so even that case creates a second pod.

## Decision

- **The `infrahub-backup` image carries the database tools**: `neo4j-admin` and `cypher-shell`
  at the Neo4j version the binary previously pinned for the vendor image, plus `pg_dump`,
  `pg_restore` and `psql` for PostgreSQL 18. The tool versions are pinned in the `Dockerfile`, so
  one image tag has one known tool set.
- **The temporary workload runs the `infrahub-backup` image** of the same version as the running
  binary, instead of a vendor image. One flag points it at a mirror, in place of the two
  per-vendor image flags. ADR 0008's reason for not reading that flag from the environment
  (FR-027) still applies.
- **When `infrahub-backup` itself runs as a pod in the target namespace and finds the tools in
  its own image, it runs them directly** and creates no temporary workload. Running from an
  operator's machine still uses the temporary workload.

ADR 0008's other decisions stand. A failed pull on the external path is still a failed backup,
and deployments whose databases are all internal still pull nothing.

## Consequences

- An air-gapped operator mirrors one image, `infrahub-backup`, instead of three.
- Running the tool as a pod in the namespace needs no permission to create workloads for the
  external path, because nothing is created.
- The image grows by a Java runtime and the Neo4j tool libraries. The size has not been measured
  yet. The growth applies to every user of the image, including users whose databases are all
  internal.
- OpsMill now redistributes Neo4j Enterprise tool binaries in this image. The PR author confirmed
  that this is cleared, as OpsMill already redistributes Neo4j Enterprise with
  `infrahub-enterprise`.
- A Neo4j server upgrade that needs a newer `neo4j-admin` now needs an `infrahub-backup` release,
  where before it needed only a new pinned vendor tag. The version gate (FR-006) still refuses a
  tool older than the server.
- A development build has no published image of its own version, so it has to be given the image
  explicitly. The end-to-end tests have to build the image and load it into the test cluster.
- Two code paths now run the tools: in a temporary workload, and in the tool's own process. Both
  must keep FR-014: credentials stay out of every command line.

## Alternatives Considered

- **Keep the vendor images (ADR 0008 as written)**: rejected in review. It needs no image
  changes and follows the vendors' own releases, but leaves three images to mirror and a second
  pod even when the tool already runs in the cluster.
- **A separate tooling image published next to `infrahub-backup`**: keeps the main image small,
  but leaves two images to mirror and two release artefacts to version together. Not chosen
  because the licensing question was cleared and one image is the simpler contract.
- **Run the tools only when `infrahub-backup` runs in the cluster, and refuse external databases
  from an operator's machine**: removes the temporary workload entirely, but drops a way of
  running the tool that ADR 0008 supports. Rejected.
