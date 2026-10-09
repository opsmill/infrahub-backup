# Contract: co-located runner interface

A one-shot execution context launched co-located with a target database to run an integration backup/restore. New plumbing in both environment backends (today: exec-only).

## Runner image
- **In-process build** (if Task 0 spike passes): the project binary (embeds kloset + fs/s3 + postgres/neo4j connectors) + `pg_dump`/`pg_restore`/`psql` + `neo4j-admin` + JRE.
- **Plakar-CLI build** (fallback): `plakar` + postgresql & neo4j plugins + the same client tools.
- Built and versioned with the binaries (Make target + flake + CI). Tagged with the tool version.

## Launch inputs (per component op)
| Input | Source |
|---|---|
| operation | `backup` or `restore` |
| connector URI | `postgres://…` / `neo4j://…` / `neo4j+offline://…` |
| connector options | per the integration contracts |
| repository | `fs:///<mounted-path>` or `s3://bucket/prefix` |
| credentials (env) | discovered + override (`PGPASSWORD`, neo4j auth, AWS_* for s3) |
| mounts | backup dir (fs repo) and/or neo4j store volume (community offline) |
| snapshot tags | the tool's grouping/edition/version tags |

## Backend implementations
### Docker Compose — `RunEphemeralContainer`
- Discover compose project + network; discover target service mounts (`docker inspect … .Mounts`).
- `docker run --rm --network <project>_default [-v <repo>] [-v <neo4j-store>] [-e <creds>] <runner-image> <args>`.
- Capture stdout/stderr/exit code.

### Kubernetes — `CreateEphemeralJob`
- Discover target pod volumes/PVCs (`kubectl get pod -o yaml`).
- Render a Job (`restartPolicy: Never`) mounting the relevant PVC(s), creds as env, co-located via affinity; `kubectl apply`; poll completion; collect logs; clean up.
- `fs://` repos in K8s require a shared/host volume; `s3://` is the clean path (document).

## Outputs / contract guarantees
- **Exit 0** ⇒ the connector op completed; for backup, the snapshot exists with tags applied.
- **Non-zero** ⇒ surfaced to the tool, which marks the component failed and the group incomplete (VR-1).
- **Pre-flight**: missing client tool or unreachable repository ⇒ fail **before** touching the DB (VR-4).
- The runner never manages DB lifecycle; the tool stops/starts (community) around the runner (VR-2).

## Failure handling
- Runner launch failure, image-missing, or mount-discovery failure ⇒ actionable setup error (Edge Cases).
- On any backup-path failure, the tool still runs the community restart/cleanup deferred logic.
