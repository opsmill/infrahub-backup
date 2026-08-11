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
- **Cost**: two code paths, and the snapshot layout has to be aligned (see below) or a backup taken on one backend will not restore on the other.

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

## Snapshot layout: align the two paths — decided

**Decision: the streaming path adopts the connector's output.** Not for tidiness — without it a
Docker-taken backup cannot be restored to a Kubernetes deployment.

The structure is not the problem. The exporter **skips** `manifest.json` when staging
(`exporter.go:93`) and requires exactly one non-manifest data artifact, choosing offline-load
versus online-restore from the destination URI rather than from the manifest. A snapshot without
a manifest still stages correctly.

What actually breaks is the artifact itself:

| Component | Connector emits | `main`'s streaming emitted | Interchangeable? |
|---|---|---|---|
| Neo4j Community | `<db>.dump` | hardcoded `/neo4j.dump` | only when the database is named `neo4j` — `database load` looks for `<targetdb>.dump` in `--from-path` |
| Neo4j Enterprise | one `.backup` artifact file | **a tar of the backup directory** | **no** — a tar is not a valid artifact for `database restore --from-path` |

`main` was self-consistent rather than compatible: it streamed in both directions, so its own tar
came back out through its own untar. A two-transport design is what turns that into a real
cross-path incompatibility.

So the streaming path must:

1. Stream the **raw artifact**, not a tar. 003 verified the online backup produces a single
   `<db>-<ts>.backup` file, so there is nothing to tar.
2. Name the entry from the **target database**, not the literal `neo4j.dump`. This is the same
   cross-name restore trap already fixed once in `a17434a`.
3. Emit `/manifest.json` for parity, so both paths carry the same metadata.

**Share the emission code rather than reimplementing it.** Two implementations obliged to
produce byte-identical output is exactly the failure mode this branch has already hit twice: the
`fs://` spelling classified three different ways in three places, and `--user root` honoured by
one image and silently defeated by another. `manifest.Emit` is already exported from the
integration; `emitDir` is not. Exporting an emission helper there, used by both the in-pod
connector and the tool-side streaming path, makes the layouts identical by construction instead
of by vigilance.

Do this while it is still free: nothing has shipped, so there is no migration burden. Once
backups exist in the wild, changing the layout means supporting both forever.

## Revision after implementing step 1: no streaming importer is needed

Building the shared emission changed the plan for the better. The two components need
co-location for **different reasons**, and only one of them actually needs it:

| Component | Tool | Why | Kubernetes transport |
|---|---|---|---|
| Neo4j | `neo4j-admin` | manipulates files in the data directory | must run **in the pod**: exec it there, `CopyFrom` the artifact, emit via `NewStagedImporter` |
| PostgreSQL | `pg_dump` | speaks the wire protocol | only needs **reachability**: a port-forward lets the tool run the real connector in-process |

So neither component needs `main`'s streaming importer, and **neither produces a second
snapshot layout**:

- Neo4j goes through `importer.NewStagedImporter` (shipped in the integration at
  **v0.2.0**), which runs the same `manifest.Emit` + `emitDir` as the in-place connector.
- PostgreSQL goes through the *real* `integration-postgresql` importer, in-process. Its
  emission (`/00000-globals.sql`, `/0000N-<db>.dump`, plus a logical manifest) is
  therefore identical by construction, with **no upstream change required** — which
  matters, because that repository is not ours and exposes no staged entry point.

This is strictly better than reinstating the stream helpers: alignment stops being
something to maintain and becomes something that cannot break, for both components.

The one new capability needed is a **port-forward** for the PostgreSQL component on
Kubernetes. Verified absent today: `KubernetesBackend` has `Exec`, `ExecStreamPipe`,
`ExecWritePipe`, `CopyTo`, `CopyFrom`, `Start`, `Stop`, `ServiceReplicas` — and nothing
that forwards a port.

### Restore needs the symmetric seam

The exporter also runs `neo4j-admin` itself, and its `load()` carries knowledge worth not
duplicating: exactly-one-artifact validation, the verified rule that `database restore`
wants the artifact **file** while `database load` wants the **directory**, and the
`--` separator guarding a `-`-prefixed database name.

So do not reimplement it tool-side. Add the mirror of `NewStagedImporter`: let the
integration stage records to a caller-provided directory and report the `neo4j-admin`
argv to run, and let the tool decide *where* to run it. The integration keeps owning what
to run and what the layout is; the tool owns only where. That split is what keeps the two
transports honest.

## Work breakdown

1. **Align the snapshot layout** per the decision above — raw artifact, database-derived name, manifest emitted, emission code shared with the integration rather than duplicated. Everything else depends on it.
2. Reinstate the stream helpers from `dcb15f7^`, keeping them backend-agnostic — they call
   `iops.Exec`/`ExecStreamPipe`, so they must not acquire Docker-specific assumptions.
3. Route by backend in `plakar_backup.go` / `plakar_restore.go`: Docker → runner, Kubernetes →
   streaming. Replace the two hard refusals.
4. Restore path for Kubernetes: stream a staged export **into** the pod with `ExecWritePipe`,
   then run `neo4j-admin load`/`restore` in the pod.
5. Confirm `StopServices`/`StartServices` semantics on Kubernetes (scaling a Deployment to 0,
   presumably) and that `waitForNeo4jBolt` works there — it uses `iops.Exec`, so it should, but
   scaling down and back up is a slower and different sequence than a container restart.
6. Encryption must hold on this path: secrets reach a Docker runner over
   `--credentials-stdin`. Running in-process keeps them off any argv at all, so this path is
   structurally safer — but confirm SC-004 still holds and that an encrypted repository
   round-trips on Kubernetes. Note the Postgres port-forward puts a listener on localhost for
   the duration of the backup; check what that exposes before relying on it.

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

## T062 and T071 are one fix — findings from a first implementation attempt

T071 (Enterprise needs `server.backup.listen_address` exposed) and T062 (Kubernetes refused)
have the same cause and the same fix: **run `neo4j-admin` inside the database's own container
or pod** rather than in a sibling. `main` did exactly that and had neither problem.

Doing so removes the need for a Kubernetes-specific transport entirely, because the two
primitives it needs — `Exec` and `CopyFrom`/`CopyTo` — are implemented by both
`DockerBackend` and `KubernetesBackend`. The sibling-runner model is what created both
symptoms, so it is the thing to retire for this component, not each symptom in turn.

### Shape that works

Backup: make a stage dir in the container → `staged.DumpArgs(remoteStage)` → `Exec`
neo4j-admin there → `CopyFrom` the artifact to a local stage → build the snapshot in this
process from `importer.NewStagedImporter`. Restore: `exporter.NewStagedExporter` stages
locally → `CopyTo` the container → `Exec` the argv from `RestoreArgs(remoteStage)`.

### Details established, worth not rediscovering

- **Omit the host from the online location** (`neo4j:///<db>` rather than
  `neo4j://…@database:6362/<db>`). The integration adds `--from` only when a host is set, and
  without it neo4j-admin uses loopback — which is the entire point of running inside the
  container, and what makes the T071 prerequisite disappear.
- **`cp` onto an existing directory nests rather than replaces.** Copy to a *non-existent*
  destination so it becomes a copy of the source, and clear the container-side stage before
  copying in as well as after copying out.
- **`Neo4jMigration.Requested()` is a method, not a field**, and `run(binDir)` executes
  locally — the in-place path needs the argv to `Exec` instead, so that knowledge wants
  exposing the same way `DumpArgs`/`RestoreArgs` were.
- **Four helpers do not exist yet** and are the bulk of the remaining work:
  `snapshotFromImporter` (factor out of `writeMetadataSnapshot`, which already does exactly
  this for the metadata component), `exportSnapshot` + a small exporter interface (factor out
  of `runConnectorRestore`), and a migrate-argv accessor.
- **Postgres is unaffected and still needs its own answer.** `pg_dump` speaks the wire
  protocol, so it needs reachability rather than co-location: the runner on Docker, a
  port-forward on Kubernetes. `KubernetesBackend` has no port-forward today.

### Then the refusals go

`plakar_backup.go:39` and `plakar_restore.go:34` can be deleted once Neo4j is in-place and
Postgres has a Kubernetes route. Acceptance stays as stated above: `test_k8s_plakar.py` and
`test_k8s_plakar_s3.py` green, the Docker suites unbroken, and a backup taken on one backend
restoring on the other.
