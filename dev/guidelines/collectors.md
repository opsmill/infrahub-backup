<!-- Extracted from specs/003-collect-tool on 2026-07-09 -->

# Collector Guidelines

## Overview

Conventions for writing and maintaining `infrahub-collect` collectors (`src/internal/app/collect*.go`). A collector gathers one category of diagnostic data into the staging bundle. These rules keep collection safe on degraded instances and keep the bundle trustworthy. Background: [ADR 0002](../adr/0002-non-fatal-timeout-bounded-collectors.md), [ADR 0003](../adr/0003-read-only-collection.md), [ADR 0004](../adr/0004-key-name-secret-masking.md).

## Time-bound every subprocess

A collector runs against a possibly-wedged container. Never call the context-free `runCommand`/`Exec` on the collect path — a hang there stalls the whole run. Use the bounded variants and the standard bounds:

- exec / status dumps → `collectExecTimeout` (60s)
- log downloads and file copies → `collectTransferTimeout` (5 min)

A timeout must surface as a `*timeoutError` so the orchestrator records the manifest reason verbatim as `timed out after <duration>`. When aggregating several sub-commands into one collector error, `%w`-wrap a sub-command timeout so `errors.As` still finds it — do not flatten it into a `%v` string.

## Failures are non-fatal; distinguish absent from failed

Return a wrapped error on failure; the orchestrator logs a `WARN` and records the collector as `failed` with the reason. Never call `os.Exit` or print errors directly (constitution Principle V). The run must continue and still exit 0.

- **Service not deployed** → record `skipped` with a reason (via the skip precondition). A genuine "nothing matched" enumeration returns `(nil, nil)`.
- **Service present but the command failed** → record `failed`. Do not collapse an execution error into "not deployed"; that produces a misleadingly-empty bundle.

Do not mark a collector `success` when it collected nothing due to a swallowed error — an empty file that looks intentional is worse than a recorded failure.

## Route sensitive output through the masking choke-point

Any env or configuration dump that can carry secrets MUST pass through `masking.go` before it is written, and MUST be added to the enumerated masked-output set (see [ADR 0004](../adr/0004-key-name-secret-masking.md)). Pick the matching helper: `maskEnvOutput` (KEY=VALUE), `maskConfigPairs` (Redis alternating lines), `maskErlangConfig` (rabbitmqctl tuples), `maskJSON` (JSON). Never write a new sensitive dump without masking it.

## Stay read-only

A collector must never stop, restart, or scale a deployment workload. Use only read/exec operations. The sole permitted mutations are the tool's **own** transient resources (the `--benchmark` container/pod it creates and deletes) and the opt-in `--include-backup` delegation.

## Don't mutate shared backend helpers — add bounded variants

`EnvironmentBackend` helpers (`CopyFrom`, `Exec`, `getPodForService`, `getInfrahubVersion`, …) are shared with `infrahub-backup`/`infrahub-taskmanager`. When collect needs a timeout-bounded or otherwise different behaviour, **add a collect-only variant** (e.g. `CopyFromContext`, `getPodForServiceContext`, `ExecContext`) rather than changing the shared method's signature or behaviour. Keep compile-time `var _ collectBackend = (*DockerBackend)(nil)` / `(*KubernetesBackend)(nil)` assertions so a seam change fails at build time, not at runtime.

## Non-interactive invocations must not gate on unbounded waits

Anything collect calls must complete without waiting on an operator or on an open-ended condition. When reusing an interactive path, pass the non-interactive flag: `--include-backup` calls `CreateBackup(force=true, …)` so it skips the unbounded `waitForRunningTasks` loop (see [ADR 0006](../adr/0006-include-backup-reuses-createbackup.md)).

## Testing

- Unit-test pure logic table-driven (masking, manifest shape, filename derivation, arg construction, replica parsing) — no mocking framework; use a fake `collectBackend`.
- Cover the failure branches: a collector that fails still exits 0 and records `failed`; a read-only guard test asserts no `Stop`/`Start`/scale is called across the full plan.
- Container-touching flows are covered by the `-m docker` / `-m k8s` pytest e2e suites (Docker and Kubernetes) in CI. Note the CI edition matrix (`community`/`enterprise`): behaviours that differ by Neo4j edition (e.g. the backup path) must be validated on both.
