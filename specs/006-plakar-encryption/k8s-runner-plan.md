# Plakar on Kubernetes — restoring parity with `main` (T062)

**Status**: preparation only — no code written. **Date**: 2026-08-10
**Blocks**: merging PR #163, because merging as-is regresses `main`.

## The regression, precisely

`plakar_backup.go:39` and `plakar_restore.go:34` refuse Kubernetes outright:

> `the plakar runner backend currently supports Docker Compose only; Kubernetes support is pending`

`main` ships `tests/e2e/test_k8s_plakar.py` and `test_k8s_plakar_s3.py`, and **both k8s e2e
suites pass on `main`** (run 30986539590: `e2e-tests-k8s (community)` and `(enterprise)` both
success). So plakar on Kubernetes worked before this branch and does not now.

## Why the 003 runner is structurally Docker-only

Three constraints, each verified rather than assumed:

1. **The connector must run where `neo4j-admin` and the data directory are.** The
   integration's importer builds `neo4j-admin database dump --to-path=…` and runs it
   (`importer/importer.go:116`); the exporter stages records then runs
   `neo4j-admin restore/load --from-path` (`exporter/exporter.go:117`). Neither can run in the
   tool's process for a database inside a container or pod.
2. **The runner is built from Docker primitives.** `composeRunnerArgs` takes the database
   container's image, joins its compose network, bind-mounts the repo and the tool binary, and
   uses `--volumes-from` to share the data volume. None of these exist as-is on Kubernetes.
3. **The repository lives on the tool's local disk.** `test_k8s_plakar.py` passes
   `--repo fs://{tmp_path}` — a directory on the machine running the tool, not in the cluster.
   A runner **pod cannot reach it**, so "run the connector in a pod" cannot satisfy that test.

Constraints 1 and 3 are in direct tension: the connector must be co-located with the database,
but the repository is not. The Docker runner resolves this with a bind-mount. Kubernetes has no
equivalent for a path on the operator's workstation.

## How `main` avoided the problem

`main` never co-located the connector. It streamed the dump **out** of the container or pod and
wrote the kloset snapshot **in the tool's process**:

- `backupNeo4jCommunityStream` / `backupNeo4jEnterpriseStream` / `backupTaskManagerDBStream`
  produced an `io.ReadCloser` via `iops.ExecStreamPipe`.
- `plakar_backup.go` fed that into a `NewStreamingImporter` and closed a snapshot builder.
- It logged *"Plakar **streaming** backup completed successfully"*.

Every primitive it used — `Exec`, `ExecStreamPipe`, `ExecWritePipe`, `CopyTo`, `CopyFrom` — is
implemented by **both** `DockerBackend` and `KubernetesBackend`
(`environment_kubernetes.go:109-156`). That is the whole reason it worked on Kubernetes: the
design was backend-agnostic because execution stayed in the tool.

Those helpers are the ones deleted in `dcb15f7` as unused. They are recoverable:

```bash
git show dcb15f7^:src/internal/app/backup_neo4j.go       # the three neo4j stream helpers + execReadCloser
git show dcb15f7^:src/internal/app/backup_taskmanager.go # backupTaskManagerDBStream
git show origin/main:src/internal/app/plakar_backup.go   # the streaming builder + StreamingImporter wiring
```

## Options

### Option 1 — two transports, one per backend (recommended)

Keep the co-located runner for Docker; restore the streaming path for Kubernetes.

- Satisfies both k8s tests: the local `fs://` repo works because the tool writes it, and
  `s3://` works because the tool can reach S3 too.
- Reuses a design already proven green on `main`.
- No pod creation, no RBAC, no PVC access-mode problems, no image-pull concerns in-cluster.
- **Cost**: two code paths, and the snapshot-shape question below must be settled.

### Option 2 — pod-based runner, Kubernetes restricted to `s3://`

Mirror the Docker runner with a Job built from the database pod's image, mounting its PVC.

- Works for `s3://`, where the pod reaches the repository directly, and keeps the connector
  (and therefore one snapshot shape) on both backends.
- **Rejects** `fs://` on Kubernetes, so `test_k8s_plakar.py` would have to change — removing
  functionality that works on `main`. That is a regression dressed as a design choice.
- Also needs: RBAC to create Jobs, `ReadWriteOnce` PVC handling (the database pod holds the
  volume; the offline path scales it down anyway), and Job cleanup on failure.

### Option 3 — stage locally, then run the connector

Rejected. It only works if the connector can read a staged dump from the tool's disk, and
constraint 1 says it cannot: the importer runs `neo4j-admin` against a live data directory.

## The question to settle first: are snapshots interchangeable?

This decides how much of Option 1 is acceptable, so resolve it before writing code.

| Path | What lands in the snapshot |
|---|---|
| Docker, via the connector | one record per file from `emitDir`, **plus** `manifest.Emit(...)` |
| Kubernetes, streaming | a single entry, `/neo4j.dump` (and `/prefect.dump`) |

A backup taken on Docker must restore on Kubernetes and vice versa — operators move backups
between environments, and the whole point of a backup is that it restores somewhere else. Two
shapes means either the exporter tolerates both, or a Docker backup cannot be restored to a
Kubernetes deployment.

**Do this first**: take a Docker (connector) backup and restore it through the streaming path,
and the reverse. If they are incompatible, the streaming path must emit the connector's layout
(same pathnames, same manifest record) rather than its own.

## Work breakdown, once the shape question is answered

1. **Decide and record** the snapshot-shape contract (above). Everything else depends on it.
2. Reinstate the stream helpers from `dcb15f7^`, keeping them backend-agnostic — they call
   `iops.Exec`/`ExecStreamPipe`, so they must not acquire Docker-specific assumptions.
3. Route by backend in `plakar_backup.go` / `plakar_restore.go`: Docker → runner, Kubernetes →
   streaming. Replace the two hard refusals.
4. Restore path for Kubernetes: stream a staged export **into** the pod with `ExecWritePipe`,
   then run `neo4j-admin load`/`restore` in the pod.
5. Confirm `StopServices`/`StartServices` semantics on Kubernetes (scaling a Deployment to 0,
   presumably) and that `waitForNeo4jBolt` works there — it uses `iops.Exec`, so it should, but
   scaling down and back up is a slower and different sequence than a container restart.
6. Encryption must hold on this path: the passphrase reaches a Docker runner over
   `--passphrase-stdin`. Streaming keeps everything in-process, so there is no argv exposure —
   confirm SC-004 still holds and that an encrypted repository round-trips on Kubernetes.

## Acceptance criteria

- `tests/e2e/test_k8s_plakar.py` and `test_k8s_plakar_s3.py` pass, matching `main`.
- The Docker suites stay green (no regression from the routing change).
- A backup taken on one backend restores on the other, per the contract decided in step 1.
- An **encrypted** repository round-trips on Kubernetes, with the passphrase absent from any
  pod spec, argv, or environment.

## Note on scope

This is feature work, not conflict resolution. It is also the last thing standing between
#163 and `main` — every other CI check is green, and T064 (the database container restart
disrupting Infrahub's workers) is a separate behavioural decision recorded in `tasks.md`.
