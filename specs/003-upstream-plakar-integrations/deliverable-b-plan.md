# Deliverable B — tool rework: implementation plan (E2E-informed)

**Feature**: `003-upstream-plakar-integrations` · **Date**: 2026-06-30
**Status**: foundation started (runner Dockerfile landed); Go core rework pending.

This plan encodes the architecture **validated end-to-end against the live Infrahub** (see the E2E commits) and sequences the build-breaking Go rework so each landing is green.

## Architecture (validated)

- **Consumption = in-process** (spike PASSED): the tool binary embeds kloset + `integration-fs`/`-s3` + the `postgres`/`neo4j` connectors. No `plakar` CLI.
- **Runner = our binary in a one-shot container** co-located with the DB (`build/runner/Dockerfile`). One-shot (not exec-into-existing) because it can **mount the host `fs://` repo** (`-v`) — resolving repo reachability — and carries `neo4j-admin` + pg client tools.
- **`__run-connector` worker** (the proven E2E logic): opens the kloset repo, runs ONE connector op. Dependency-light (no docker/kubectl). This is exactly the validated E2E harness, folded into the tool binary.
- **Tool orchestrates**: env/edition detect → lifecycle (drain; community stop/start) → launch runner per component (creds as env, repo mounted/s3, DB network, neo4j volume as needed) → apply grouping tags.

## Green-landing sequence (the bump is build-breaking; land as one unit)

1. **deps**: `go.mod` → kloset `v1.1.0`; add `integration-postgresql v1.1.0-beta.7`; add `integration-neo4j` (+ `replace` → `./contrib/integration-neo4j`); keep `integration-fs`/`-s3` `v1.1.0-beta.5` (verified to build against kloset v1.1.0 in the E2E). Then `scripts/update-vendor-hash.sh`.
2. **delete**: `importer.go` (StreamingImporter), `backup_neo4j_watchdog.go` + embedded watchdog assets.
3. **new `connectors.go`**: blank-import `integration-fs/-s3` (storage) + `integration-postgresql/{importer,exporter}` + `integration-neo4j/{importer,exporter}` to register connectors.
4. **new `run_connector.go`**: the `__run-connector` cobra subcommand = the validated E2E worker. `backup <repo> <src-uri> [opt] [--tag k=v]`: `snapshot.Create` (tags) → `snapshot.NewSource(ctx, imp)` (NOTE v1.1.0: variadic, no `0`) → `Backup` → `Commit`. `restore <repo> <dst-uri> <snap> [opt]`: `snapshot.Load` → `snap.Export(exp, "/", &ExportOptions{SkipPermissions:true})`.
5. **new `runner.go`**: build+launch the one-shot runner (Compose: `RunEphemeralContainer`; K8s: `CreateEphemeralJob`), inject creds (reuse `app_config.go` discovery; FR-023), mount repo / pass s3 creds (R8), mount neo4j volume for community/restore, reap orphans (label).
6. **rewrite `plakar_backup.go`**: per component, launch the runner instead of building a `StreamingImporter`; keep grouping tags + completeness. Fix the `NewSource` call site if any remains.
7. **rewrite `plakar_restore.go`**: drive the integration exporters via the runner; add the clean-break guard (FR-016); restore selector full-group vs per-component (FR-024).
8. **`backup_neo4j.go`**: keep edition detect + community lifecycle (stop app + **stop the writer container**, replacing the SIGTERM+watchdog dance — R7); drop the hand-rolled `neo4j-admin` construction.
9. **`backup_taskmanager.go`**: drop the hand-rolled `pg_dump`; point the runner at `postgres://…@task-manager-db/prefect`.
10. **`make build` + `make lint` green**; then E2E against the live Infrahub.

Until step 10, the build is red — do steps 1–9 as one unit on this feature branch.

## Connector invocations (validated against real Neo4j 2025.10.1 + the prefect DB)

- Neo4j Enterprise online: `neo4j://neo4j:<pw>@database:6362/neo4j` (backup → single `.backup` artifact).
- Neo4j Community offline: `neo4j+offline:///<datadir>?database=neo4j` (writer stopped → `<db>.dump`).
- Postgres: `postgres://postgres:<pw>@task-manager-db:5432/prefect` (`compress=false`).
- Restore exporter passes the artifact **file** to `--from-path` (E2E fix), `overwrite=true` for `--overwrite-destination`.

## Open lifecycle items the E2E surfaced (must be handled + tested in step 10)

1. **neo4j restore data-dir/config**: `neo4j-admin restore` must write the data dir the live server reads. In the one-shot runner (neo4j-image base, mounting the deployment's `database_data` volume at `/data`), the default config maps `/data` — verify the restored store lands at `/data/databases/<db>`.
2. **store-file ownership**: run `neo4j-admin` as the `neo4j` user (or chown the output) so the server can read restored files (E2E: root-owned files → DB "unavailable").
3. **Enterprise catalog**: after restore, the DB must be mounted via `CREATE DATABASE <db>` in the `system` DB (lifecycle the tool drives; the connector only writes the store).
4. **community offline output**: `--to-path` must be writable by the `neo4j` user (E2E `AccessDenied`).
5. **runner image neo4j version** must match the deployment's Neo4j (restore is version-sensitive) — detect + select/build the matching tag.

## Done so far (Deliverable A, validated)

`contrib/integration-neo4j/` builds + `go vet` clean; both editions' `neo4j-admin` commands + the full in-process connector path (backup + restore) validated against the live Infrahub; exporter artifact-file-path bug fixed. Remaining A: SDK plugin entrypoints, testcontainers, manifest edition/counts, upstream PR.
