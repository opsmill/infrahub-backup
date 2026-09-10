# 6. `--include-backup` reuses `CreateBackup` as a standalone artifact, invoked non-interactively

**Status**: Accepted
**Date**: 2026-07-09
**Source**: specs/archive/003-collect-tool/research.md (R10)

## Context

Support frequently asks for "logs plus a backup" so they can reproduce a customer issue locally (spec US3, FR-014). The backup logic already exists in `CreateBackup` and carries the operational-safety guarantees of constitution Principle II (metadata, checksums, restart-what-you-stopped). Collection must reuse it, not reimplement it.

## Decision

`--include-backup` runs a collector that delegates to the existing `CreateBackup` unmodified. The produced backup is a **standalone** `infrahub_backup_*.tar.gz` in the standard backup directory — referenced by the manifest's `artifact` field, **not embedded** in the bundle. The collector runs last in the plan (after every read-only collector), because the delegated backup may stop/restart app containers and must not taint the diagnostics. A backup failure is non-fatal: the bundle is still produced (US3 scenario 2).

"Unmodified" has one boundary, and it is a refusal rather than a modification. `CreateBackup` against a database that lives outside the cluster (spec 007) creates and then deletes a transient Pod and Secret in the deployment's namespace, which ADR 0003 forbids `infrahub-collect` without qualification. So a collection run is marked as one that may not capture an external database, and the delegated backup stops at that gate — before anything is stopped and before any object is created — with a message naming `infrahub-backup create`. The delegation itself is untouched: no divergent backup path exists, and the internal path is byte-for-byte the one the backup tool takes.

The backup is invoked with `force=true`. Collection is non-interactive and designed never to hang; `CreateBackup(force=false)` runs `waitForRunningTasks`, an unbounded loop that returns only when no Prefect tasks are running/pending, which never fully drains on a busy instance (e.g. the Enterprise edition's recurring background tasks). `--force` skips only that consistency gate; it does not weaken the backup's own integrity guarantees.

## Consequences

- Backups keep their full metadata/checksum guarantees; no divergent "lite" backup path to maintain.
- The bundle tarball stays small; the backup is discoverable by the standard backup tooling at its normal location.
- The backup runs to completion non-interactively rather than blocking on an unbounded task-drain wait — consistent with the tool's "never hang" contract.
- `CreateBackup` does not return the path it wrote, so the artifact is identified by diffing the backup directory listing around the call.

## Alternatives Considered

- **Embed the backup archive inside the bundle** — rejected: doubles disk usage and breaks the backup tool's own restore discovery (it expects the standard location/naming).
- **`force=false` to preserve the running-tasks wait** — rejected after it caused the Enterprise e2e to hang/fail with no artifact; the wait is a consistency nicety, not a safety guarantee, and `--force` is the supported non-interactive path.
- **A lighter read-only reproduction backup** — rejected: violates "reuse unmodified" and Principle II.
