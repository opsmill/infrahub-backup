# 3. Collection is strictly read-only with respect to the workload lifecycle

**Status**: Accepted
**Date**: 2026-07-09
**Source**: specs/archive/003-collect-tool/spec.md (FR-010, SC-003)

## Context

`infrahub-backup` may stop and restart application containers while it snapshots the database (Neo4j Community). `infrahub-collect` is meant to be run against production instances at any time — including while they are degraded — and often by customers themselves. A diagnostic tool that could stop, restart, or scale a workload would be unsafe to hand out.

## Decision

`infrahub-collect` never stops, restarts, or scales any container, pod, or workload belonging to the deployment. It uses only read/exec operations (`pods/log`, `pods/exec`, `docker logs`, `docker exec`, `docker stats`, `kubectl top`). The stop/start/scale code paths in the shared backend are simply not reached by the collect plan.

Two deliberate exceptions, both operating on the **tool's own** transient resources, not the deployment's:

- `--benchmark` creates and then removes a short-lived benchmark container/pod it owns.
- `--include-backup` delegates to `CreateBackup`, whose documented behaviour may stop/restart app containers; this is opt-in and called out in the docs (see ADR 0006).

Neither exception extends to creating or deleting objects that stand in for the deployment's own. Backing up a database that lives **outside** the cluster (spec 007) needs a transient Pod and Secret in the deployment's namespace, so `--include-backup` **declines** when a database resolves as external, naming `infrahub-backup create` as the tool that performs it. The decision above is therefore unqualified: nothing `infrahub-collect` does creates, deletes, or scales a workload in the deployment's namespace. `CreateBackup` is still reused unmodified (ADR 0006) — the run stops at the external path precisely so that this guarantee stays true rather than becoming a carve-out.

## Consequences

- Safe to run against a live or degraded production instance without risk of an outage attributable to the tool.
- Kubernetes RBAC for the tool needs only read/exec verbs — no write or scale permissions.
- A deployment whose databases live outside the cluster gets no backup inside its bundle. That is the accepted cost: a bundle tool a customer can be handed because it provably cannot mutate their cluster is worth more than a backup collected alongside the diagnostics, and `infrahub-backup create` still takes that backup.
- Interrupt handling is simpler: there is no workload state to restore on SIGINT, only a staging directory to clean up.

## Alternatives Considered

- **Reuse the backup tool's stop/collect/start pattern for consistency** — rejected: it would make the tool unsafe for its core (production, degraded) use case and require elevated permissions.
