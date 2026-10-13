# 18. The transient workload is a Job that owns its pod and its credential Secret

**Status**: Accepted
**Date**: 2026-10-12
**Supersedes**: the choice of a bare `Pod` in ADR 0009, and the owner of the credential Secret (the rest of ADR 0009 stands)
**Source**: `src/internal/app/transient_workload.go` (`transientFinishedTTL`, `transientPodSpec`, `buildTransientWorkloadManifest`); spec FR-011/FR-023; [PR #172](https://github.com/opsmill/infrahub-backup/pull/172)

## Context

ADR 0009 decided that the temporary workload for an external database is a bare `Pod`, piped to
`kubectl create -f -`, bounded by `activeDeadlineSeconds`, and owning its credential Secret
through an `ownerReference`. It rejected a `Job` because a Job adds a controller and a second
object to remove.

FR-011 requires the workload to be gone on every exit path, including when the tool is killed.
FR-023 requires the credential to be unable to outlive the run. A pod that reaches
`activeDeadlineSeconds` is marked `Failed`, but the cluster does not delete a bare pod object, so
the pod and the Secret it owns stay in the namespace until something deletes them. When the tool
is killed, nothing does until a later run's stray reaper reaches them.

The code moved to a `Job` during the implementation of spec 007. The change was not recorded as a
decision at the time.

## Decision

- **The workload is a `Job`** with `backoffLimit: 0`, an `activeDeadlineSeconds` computed as in
  ADR 0009, and `ttlSecondsAfterFinished` (`transientFinishedTTL`, 60 seconds). The Job creates
  one pod with `restartPolicy: Never`.
- **The Job owns the credential Secret**, when the run creates one. The Secret is created after
  the Job, with an `ownerReference` to the Job's UID. When the run references the deployment's own
  Secret keys instead, no Secret is created.
- **The pod restates the Job's deadline** in its own `activeDeadlineSeconds`. The Job controller
  does not copy the Job's deadline onto its pod, and the stray reaper lists pods, not Jobs. The
  kubelet also enforces the pod's deadline when the Job controller does not.

## Consequences

- When the tool is killed, the cluster removes everything the run created without help, as long
  as its Job, time-to-live and garbage-collection controllers are running: the deadline ends the
  Job, the time-to-live deletes it, and the garbage collector deletes its pod and its credential
  Secret. Without the Job controller, the kubelet still ends the pod at its own deadline, but the
  Job and its Secret stay until that controller runs again.
- The namespace must grant `create`, `get` and `delete` on jobs, in addition to the pod
  permissions. ADR 0009 lists the full set.
- Two deadlines must agree: the Job's and the pod's. Both come from
  `externalDBWorkloadDeadline`, so they cannot drift apart.
- The tool still execs into a specific pod, so it has to resolve the Job's pod after creating the
  Job. This is the controller and the extra object ADR 0009 wanted to avoid.

## Alternatives Considered

- **A bare `Pod` (ADR 0009 as written)**: one object fewer and no pod lookup, but a pod that
  reaches its deadline is not deleted, so its Secret outlives a killed run until a later run
  removes it. That fails FR-023 in exactly the case it exists for.
- **A bare `Pod` that the stray reaper always cleans up**: depends on a later run happening,
  which is the dependency FR-011 rules out.
