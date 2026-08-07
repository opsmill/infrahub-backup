# Deliverable B — tool rework: implementation plan (E2E-informed)

> **⚠️ One status line superseded (2026-08-07).** Where the remaining Deliverable-A work is listed as including an "upstream PR", that item is dropped — the integration is not upstreamed. Remaining integration work is tracked as 004 workstream B. See [SUPERSEDED-BY-004.md](./SUPERSEDED-BY-004.md).

**Feature**: `003-upstream-plakar-integrations` · **Date**: 2026-06-30
**Status**: foundation + orchestration primitive DONE and tested against the live Infrahub; create/restore-flow wiring + lifecycle pending.

### Progress (green + tested, committed)

- [x] **Step 1** deps: kloset → v1.1.0; integration-postgresql + integration-neo4j (replace) added. (`make build` green; only break was the 1-line `NewSource` fix.) ⚠️ `flake.nix` vendorHash stale — `update-vendor-hash.sh` uses `grep -P` (fails on macOS); refresh in CI/Linux.
- [x] **Step 3** `connectors.go` — postgres + neo4j registered in-process.
- [x] **Step 4** `run_connector.go` — `__run-connector` worker; **tested**: tool's own worker backs up live neo4j → tagged snapshot → `snapshots list` reads it.
- [x] **Step 5 (partial)** `runner.go` — `LaunchComposeBackup`/`LaunchComposeRestore`: one-shot runner = DB's own image + tool binary + repo bind-mount, on the DB network. **Tested through the tool**: host tool launches the co-located runner → snapshot in the HOST repo.
- [x] **Step 6 (backup)** `CreatePlakarBackup` rewritten to launch the runner per DB component (Enterprise neo4j `database:6362`, postgres `task-manager-db:5432`) + in-process metadata snapshot. **Tested E2E** against the live Enterprise Infrahub: neo4j + postgres + metadata grouped under one backup-id, `status=complete`.
- [x] **Step 7 (restore)** `plakar_restore.go` rewritten to the runner model (clean break): `restoreComponentViaRunner` drives the connector exporter per component. Neo4j lifecycle: stop writer → runner restore `--volumes-from` quiesced volume → restart (`--overwrite-destination` replaces the default db's store; no CREATE DATABASE). **Tested E2E** against a throwaway Enterprise Infrahub: backup 6 nodes → wipe → restore → 6 nodes back. Postgres restore wired (untested — needs cred setup). `composeContainerID` uses `docker ps -aq` so the stopped writer is found.
- [x] **Step 8** Community lifecycle: stop the writer → runner `--volumes-from` for offline dump (create) / load (restore) → restart. `backupNeo4jComponent` + `restoreComponentViaRunner` are edition-aware. **Tested E2E** on a throwaway Community Infrahub: offline backup 3 nodes → wipe → offline restore → 3 nodes back. Postgres backup+restore also **tested E2E** (Enterprise sandbox, `demo` table 5 rows round-trip). Both editions + both DBs now round-trip.
- [ ] **Step 2** delete `importer.go`'s StreamingImporter + `backup_neo4j_watchdog.go` + the now-dead `neo4jStreamFactory`/`postgresStreamFactory` (keep `NewMemoryImporter` for the in-process metadata snapshot). Do with the restore rewrite.
- [ ] **Step 9** Kubernetes `CreateEphemeralJob` equivalent (create flow guards K8s as pending).
- [ ] `make lint` green + refresh `flake.nix` vendorHash (CI/Linux).

### Restore-online lifecycle (test against a throwaway Infrahub)

The runner restore (neo4j image, `--volumes-from` the target neo4j) should fix the
E2E data-dir issue (the neo4j image carries the config that maps `/data`), but must
be verified: target neo4j **stopped** → `neo4j-admin database restore` writes
`/data/databases/<db>` → run as the `neo4j` user (ownership) → restart neo4j →
Enterprise `CREATE DATABASE <db>` to mount. Postgres restore: `pg_restore`/`psql`
via the exporter against `task-manager-db`.



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
