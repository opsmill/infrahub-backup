# 9. Transient workload lifecycle: a Pod manifest on stdin, deadline-bounded, owning its credential Secret

**Status**: Accepted
**Date**: 2026-09-07
**Source**: specs/archive/007-external-database-backup/research.md (R5, R7), spec FR-011/FR-014/FR-023/FR-028, data-model.md (`TransientWorkload`)

## Context

ADR 0008 settled that a database living outside the deployment is reached from a stand-in pod the
tool creates in the namespace. That pod carries the database password, needs a run-specific
identity, a resource and scratch allocation sized to the database, and a bounded life — and the
tool that created it can be killed at any point, including between creating the credential and
deleting the pod. FR-011 requires the workload gone on every exit path including abnormal
termination; FR-023 requires the credential unable to outlive the run; FR-014 forbids credentials
on a command line or in workload configuration a deployment observer can read.

ADR 0001 forbids a Kubernetes client library, and the backend already drives everything through
`kubectl` subprocesses, including a primitive that writes a command's stdin.

## Decision

- **A full `Pod` manifest, piped to `kubectl create -f -` on stdin** through the executor's
  write-pipe primitive, and reaped with `kubectl delete`. A manifest rather than `kubectl run`,
  because the pod needs labels, a deadline, resource limits, ephemeral storage sizing, a
  restricted security context and a Secret reference, all of which a manifest states directly.
- **The lifecycle bound lives on the object.** `restartPolicy: Never`; `activeDeadlineSeconds`
  computed per role as readiness timeout + (calls the pod hosts × the bound each runs under) +
  margin, in one function (`externalDBWorkloadDeadline`), so the cluster reclaims the pod after
  the tool's own timeouts would have reported a stall, never before; a run-ID label; and a marker
  label (`infrahub-backup/transient=true`) plus a name prefix (`infrahub-backup-xdb`) that contains
  no service name.
- **Credentials travel in a Secret referenced by `envFrom`, owned by the pod.** The Secret is
  created after the pod so it can carry an `ownerReference` to the pod's UID, and the cluster's
  garbage collector removes it with the pod. The UID is read from the create's stdout alone: a
  kubectl notice on stderr merged into it produces an owner reference no live pod matches, and a
  Secret that outlives the run.
- **Three roles, each sized for its work**: a *probe* (fixed 1 GiB scratch, short deadline) that
  reads version, edition, store size and member roles before anything is stopped; a *capture*
  (scratch = 2× the queried store size, or `--external-db-scratch-size`) created around the
  capture itself; a *restore* workload (10 GiB default) created at the gate and held for the run.
  Probe and capture never hold scratch in the namespace at the same time.
- **A stray reaper** runs before a workload is created: pods and Secrets carrying the marker from
  an earlier run are deleted when they are terminated or past their own deadline. A live peer's
  workload is left alone, and an unreadable field leaves an object for the cluster's deadline
  rather than guessing.
- **FR-028 exclusion**: every pod listing drops transient pods, reading the marker label first and
  the name prefix second, so the stand-in never appears in service enumeration, replica counts or
  diagnostic collection.

## Consequences

- The namespace must grant `create`, `get`, `list`, `watch` and `delete` on pods and secrets, plus
  `create` on `pods/exec`. `watch` is not optional: readiness is awaited with `kubectl wait`. This
  is the first write permission these tools require, and only the external path needs it.
- A tool killed mid-run leaves nothing that the cluster will not clean up by itself: the deadline
  removes the pod, ownership removes the Secret.
- Each workload costs five kubectl calls (create pod, read UID, create Secret, wait, delete) plus
  scheduling and an image pull; a backup of one external database creates two workloads.
- `ExecOptions.Env` becomes an `env KEY=VALUE` prefix on the exec command line, which is exactly
  the exposure FR-014 forbids, so client commands never receive credentials that way; they read
  them from the Secret the pod mounts.
- The deadline arithmetic has a history: budgeting for one bounded call where the pod hosted two
  put the deadline inside the window a capture was still legitimately working in, and the cluster
  deleted a completed capture part-way through the copy out. The arithmetic is therefore one
  function, and only the per-role call count and bound vary.

## Alternatives Considered

- **`kubectl run` with `--overrides`** — the same result with more awkward escaping.
- **A `Job` rather than a `Pod`** — adds a controller and a second object to reap, and the tool
  wants to `exec` into a specific pod anyway.
- **`kubectl debug`** — attaches to an existing pod, which is the case that does not exist here.
- **Inline `env` values in the pod spec** — readable by anyone who can `get pod`.
- **Credentials on stdin** — works for `pg_dump` through a passfile indirection, not uniformly for
  `neo4j-admin`.
- **A projected service-account token** — these are database credentials, not cluster credentials.
- **Cleanup by the tool remembering to delete the Secret** — the requirement is precisely that the
  tool need not survive to do it.
