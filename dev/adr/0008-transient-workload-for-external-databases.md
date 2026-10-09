# 8. A load-bearing image pull for external databases; offline-by-default survives it

**Status**: Accepted
**Date**: 2026-09-03
**Source**: specs/archive/007-external-database-backup/research.md (R1, R5), spec FR-004/FR-015/FR-019/FR-027

## Context

ADR 0007 established offline-by-default as the posture for these tools: they run in air-gapped and
egress-restricted networks, so a default run performs no image pull, and the one feature that does
— `--benchmark` — is opt-in and records `skipped` when the image cannot be pulled.

Spec 007 backs up a Neo4j or PostgreSQL database that lives outside the deployment. Spec 006
settled that vendor utilities run inside a container and bytes stream out through the primitives
both deployment backends implement. An external database has no container in the deployment to run
those utilities in, and no remote protocol exists that would let this tool read a Neo4j store or a
PostgreSQL cluster without them. So reaching it means the tool creates a pod running a vendor
tooling image in the deployment's namespace — a backup client, in the vendor's own term (research
R1) — and that pod's image has to be pulled.

That pull is not the benchmark's. It cannot degrade to `skipped`, because there is nothing left to
do without it.

## Decision

The transient workload's image pull is **load-bearing**: when a database resolves as external, a
pull that fails is a failed backup, reported as a failure and not as a skip. There is no
degradation path, because a run that skipped it would have captured nothing.

Offline-by-default is narrowed rather than abandoned, and the narrowing is exactly one condition
wide:

- A deployment whose databases are all internal pulls nothing, consults no new setting to a
  different effect, and behaves as it did before (FR-015). ADR 0007's guarantee holds unchanged
  for every deployment shape it was written about.
- Only a database positively established as external reaches the pull, and only after the
  deployment query that established it succeeded. A query that failed is never read as "external"
  (FR-002), so a permission problem cannot manufacture a pull.
- The image is pinned in the binary (`neo4j:<version>-enterprise`, `postgres:18-alpine`) and can
  be pointed at a mirror by `--external-db-image-neo4j` / `--external-db-image-postgres`. Those
  two flags are deliberately **not** environment-bound (FR-027): the tool holds workload-creation
  rights in the customer's namespace, so an environment-settable image would turn those rights
  into a means of running arbitrary code there.
- What an operator's own cluster requires of a pod it did not author *is* environment-bound —
  `--external-db-image-pull-secrets`, `--external-db-service-account`,
  `--external-db-node-selector`, `--external-db-tolerations`, `--external-db-priority-class`.
  These say where a pod may run and which credentials fetch its image, never what it runs, so
  FR-027's reason does not reach them, and an unattended backup is configured by environment at
  least as often as on the command line.

## Consequences

- An air-gapped operator with an external database must mirror the tooling image into a registry
  the cluster can reach. That is now a stated prerequisite rather than a discovered one, and the
  pull secret for such a registry is nameable — without it the pod sat in `ImagePullBackOff` until
  the readiness bound expired.
- The namespace must grant `create, get, list, watch and delete on pods and secrets, plus create on
  pods/exec` (FR-019). This is the first write permission these tools require, and it is required
  only on the external path. `watch` is not optional padding: readiness is awaited with `kubectl
  wait`, which opens a watch, so a namespace granting everything else still fails there.
- `infrahub-collect` does not inherit any of this. `--include-backup` refuses when a database
  resolves as external, naming `infrahub-backup create` as the tool that performs the capture, so
  ADR 0003's read-only guarantee stays unqualified and collect still needs only read and exec
  verbs (see ADR 0003 and ADR 0006).
- External Neo4j Community is refused outright (FR-008). Its backup mechanism needs the database
  stopped and its storage directly accessible, which no network-reachable design satisfies, so
  there is no image that would make the pull worth attempting.
- The failure surface grows by one condition the operator can act on, which is the trade being
  made: a pull that fails is loud and names the flag that fixes it, rather than a capture that
  quietly did not happen.

## Alternatives Considered

- **Treat a failed pull as `skipped`, as the benchmark does** — rejected. The benchmark's skip
  leaves a complete bundle missing one optional section; this skip would leave a deployment with
  no recovery point while the run reported success. That is the silent failure constitution
  Principle II exists to prevent.
- **Run the vendor utility on the operator's host** — deferred rather than rejected (spec User
  Story 3). It needs a JVM on the operator's machine, and the vendor recommends a dedicated
  machine because the backup command uses significant memory and processing. It stays specified as
  the answer for a customer who will not grant workload-creation rights.
- **Exec the utility inside an existing application pod** — rejected. Infrahub's images carry no
  `neo4j-admin`, and borrowing an application pod's resources for a backup is what the vendor
  guidance argues against.
- **Vendor the database tooling into this tool's own binary** — rejected on licensing and size,
  and it reverses spec 006's decision that vendor utilities run in a container.
- **Write the backup straight to cloud storage with `--to-path`** — rejected as the primary path
  in research R1: it bypasses the snapshot pipeline and with it repeat-block removal, encryption,
  retention and checksums, and still needs local scratch space. It is documented as a manual
  escape hatch for customers who cannot meet the restore prerequisites, not as a way to avoid the
  pull.
