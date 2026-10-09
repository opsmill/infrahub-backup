<!-- Extracted from specs/007-external-database-backup on 2026-09-07 -->

# Kubernetes Backend Guidelines

## Overview

Conventions for code that talks to a Kubernetes deployment through the backend in
`src/internal/app/environment_kubernetes.go`, `kubernetes_*.go` and the external-database path
(`external_*.go`, `transient_workload.go`). Every one of these rules exists because its absence
shipped a defect; the background is in ADRs 0008 to 0016 and the retrospective archived with
spec 007.

## Read kubectl values from stdout alone

Anything parsed as data — a pod name, a phase, a UID, a replica count, a JSONPath listing — goes
through a `podRunner` that returns the command's stdout and folds stderr into the error
(`stdoutRunner` unbounded, `boundedRunner`/`separatedRunner` under a timeout). Never pass the
merged `executor.runCommand` as a runner: kubectl writes deprecation and kubeconfig notices to
stderr on healthy calls, and merged output puts the notice in front of the value.

```go
// yes
return k.getPodStatusesWith(k.stdoutRunner(), service)
// no — a warning becomes part of the UID, the ownerReference matches nothing, the Secret leaks
return k.getPodStatusesWith(k.executor.runCommand, service)
```

`runCommand` stays correct for commands whose output is not parsed: `scale`, `cp`, `exec` of a
utility whose output is only logged.

## Decide ownership only in `kubernetes_ownership.go`

Whether a pod or workload is the deployment's is answered by `deploymentClaimsResource`, and
nowhere else. Do not compare a name against a service with `strings.Contains`, `HasPrefix` or a
regular expression in a caller. `nameMatchesService` says what a name looks like and must not be
used to *decide*; the acceptance check is

```sh
grep -rn 'nameMatchesService(' src/ | grep -v _test   # definition + ownership-file call sites only
```

Listings are parsed by `parseLabelledPods` only. If a new listing needs another field, add it to
`podOwnershipJSONPath` and `labelledPod`; do not write a second parser.

## A failed cluster query is never evidence of absence

Collect kubectl errors and return them. An empty result on the strength of queries that failed
must not become "not deployed", "external", or "nothing to scale" (FR-002). The pattern the
resolvers use:

```go
if len(selectors) > 0 && len(selectorErrs) == len(selectors) {
    return nil, fmt.Errorf("…: every label selector query failed: %w", errors.Join(selectorErrs...))
}
return nil, fmt.Errorf("…: %w", errNoPodsMatched)
```

The two returns are different facts with opposite remedies; keep them distinct in wording and in
sentinel.

## Bound every operation against an external database

Nothing on the external path calls the unbounded `Exec`. Use `execBoundedAgainst(bound,
operation, target, service, command, opts)` with the right bound — `externalDBBound` for the
operation itself, `externalDBProbeBound` for probe statements, `externalDBControlBound` for
kubectl reads and housekeeping — so a timeout names what it was waiting on.

A transient pod's `activeDeadlineSeconds` comes from `externalDBWorkloadDeadline(calls, bound)`:
count every bounded call the pod hosts and multiply by the bound each runs under. Budgeting for one
call where the pod hosts two put the deadline inside the window a capture was still working in,
and the cluster deleted a completed capture during the copy out.

## Credentials never touch the exec command line

`ExecOptions.Env` becomes an `env KEY=VALUE` prefix on the exec command, visible in API-server
audit logs and the pod's process list. It carries TLS mode and trust paths, never a password. Client
tools read credentials from the Secret the transient pod mounts through `envFrom`.

## Read cypher-shell results by header name

Use `cypherResult`/`parseCypherRow` and read columns by the name the statement asked for. Never
take the first non-empty line as the header or read a column by position: cypher-shell prints
notices above the result, and a server may return columns in another order. Column names are
matched case-insensitively and trimmed in one place (`cypherColumnKey`).

## Images are flag-only; placement is environment-bound

A flag that chooses *what runs* in the customer's namespace (`--external-db-image-*`) is never
bound to viper, because the tool holds workload-creation rights and an environment-settable image
turns those into arbitrary code execution. Flags that say *where* a pod may run and *which
credentials* fetch its image are bound. Register a new external-database string flag in the
`externalDBStrings` table in `cli.go`, which registers, binds and reads it in one place.

## Metadata additions are additive

A new metadata field needs a documented meaning for absence, must not change the metadata
version, and must be safe for an older tool to ignore. Retention never reads metadata; do not make
it. An artifact that must not be restored is removed, not marked.

## Testing

- Every resolver takes a `podRunner` (`…With(run, …)`); test through it with a fake runner rather
  than a stubbed binary where possible.
- A fake listing must round-trip through the real parser (`ownershipLine`), so a filter that reads
  a label field can be tested at all.
- A new exported or package-level symbol needs a non-test caller in the same change; see
  [implementation-verification.md](implementation-verification.md).
