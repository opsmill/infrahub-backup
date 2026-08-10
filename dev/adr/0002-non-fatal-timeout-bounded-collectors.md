# 2. Non-fatal, timeout-bounded collector framework

**Status**: Accepted
**Date**: 2026-07-09
**Source**: specs/archive/003-collect-tool/research.md (R2)

## Context

The primary use case for a troubleshooting bundle is a **degraded** Infrahub instance — a service is down, a pod is crash-looping, an exec hangs. A collection tool that aborts on the first failure, or that hangs on a wedged container, is useless exactly when it is needed most (spec FR-009, SC-005).

## Decision

Collection is a fixed, ordered list of named `collector` units run sequentially by an orchestrator (`CollectBundle` / `runCollectPlan`). Each collector:

- Runs every subprocess under a context timeout (`exec.CommandContext`): 60s for exec/status dumps, 5 minutes for log downloads and file copies. A timeout is recorded verbatim as `timed out after <duration>`.
- Is **non-fatal**: an error is caught, logged as a `WARN`, and recorded in the bundle manifest as `failed` (with a reason) or `skipped` (when the collector does not apply); the run continues and the command still exits 0 with a partial bundle.
- A panic in a collector is recovered and converted into a recorded `failed` outcome, so no single collector can abort the whole run.

Only a missing environment, an unwritable output directory, or a failure to write the archive is fatal (exit 1).

## Consequences

- A bundle from a broken instance is still produced, and the manifest accounts for 100% of attempted collectors with an explicit outcome.
- The tool never hangs on a degraded dependency — timeouts convert a hang into a recorded failure.
- Sequential execution keeps the streamed progress readable and avoids hammering a fragile instance; wall time stays within the 5-minute budget (SC-001).

## Alternatives Considered

- **Parallel collectors** — rejected for v1: interleaved output, higher load on a degraded instance, and no evidence the time budget needs it.
- **Abort on first failure** — rejected: contradicts the degraded-instance use case.
- **Unbounded subprocesses** — rejected: a single wedged `exec`/`cp` would stall the entire run (the exact failure mode a troubleshooting tool must survive).
