# Neo4j integration for Plakar (DRAFT)

> **Status: draft — compile-verified + neo4j-admin commands validated against a
> real Neo4j Enterprise 2025.10.1.** The kloset connector wiring follows the
> verified patterns of `integration-mysql`/`integration-postgresql` (builds +
> `go vet` clean against kloset v1.1.0). The **online (`neo4j://`) backup and
> restore commands and output layout are confirmed against a live Enterprise
> instance** (2026-06-30): `database backup --compress=false --from --to-path <db>`
> produces a single `<db>-<ts>.backup` artifact; `database restore --from-path
> <dir> [--overwrite-destination] <db>` restores it. The **offline
> (`neo4j+offline://`) Community path is also validated** against Neo4j Community
> 2025.10.1: `database dump --to-path … <db>` → single `<db>.dump`; `database load
> --from-path … --overwrite-destination` round-trips with data intact (the output
> path must be writable by the `neo4j` user). **Still pending:** the full connector
> Go path under a real run (Import→kloset→Export), the testcontainers suite, SDK
> plugin entrypoints, and edition/Bolt-count manifest.
> Not yet submitted to `PlakarKorp/integrations` (outward action, pending a green
> test suite + review).

Backup and restore of Neo4j databases with [Plakar](https://github.com/PlakarKorp/plakar),
encapsulating the `neo4j-admin` commands so database backups are as easy as any
other Plakar backup.

## Protocols

| URI | Edition | Mechanism | DB state |
|-----|---------|-----------|----------|
| `neo4j://[user:pass@]host:6362/<db>` | Enterprise | `neo4j-admin database backup --compress=false --to-path …` (online) | running |
| `neo4j+offline:///<datadir>?database=<db>` | Community | `neo4j-admin database dump --to-path …` (offline) | **stopped** (caller's responsibility) |

Restore (exporter): `neo4j://` → `neo4j-admin database restore --from-path`;
`neo4j+offline://` → `neo4j-admin database load --from-path`. Pass `overwrite=true`
for `--overwrite-destination`.

Each snapshot includes `/manifest.json` (version, edition, database, backup mode;
live node/relationship counts via Bolt are a pending enhancement for the online case).

## Layout

```
neo4j/
├── go.mod                 # module github.com/PlakarKorp/integration-neo4j
├── neo4jconn/conn.go      # URI parsing + neo4j-admin locating
├── importer/importer.go   # neo4j:// + neo4j+offline:// (init-registered)
├── importer/schema.json
├── exporter/exporter.go   # restore / load (init-registered)
├── exporter/schema.json
├── manifest/manifest.go   # /manifest.json
└── manifest.yaml          # connector declarations for plugin packaging
```

## Consumption

This module's `importer`/`exporter` packages register their connectors via `init()`
(like `integration-postgresql`), so they can be blank-imported **in-process** —
the consumption model verified for `infrahub-backup` (kloset v1.1.0 compile spike).

## TODO before upstream contribution

- [x] Validate `neo4j-admin` online backup/restore flags + output layout against a real
      Neo4j Enterprise (done 2026-06-30 against 2025.10.1).
- [x] Validate Community offline `database dump`/`database load` round-trip (done
      2026-06-30 against Community 2025.10.1; data intact; output-path perms noted).
- [x] Run the full in-process connector path end-to-end against live data (2026-06-30,
      Neo4j Enterprise 2025.10.1, 18,553 nodes): **backup** = Import→kloset snapshot
      (66 MB `.backup` artifact captured, snapshot committed to an `fs://` repo);
      **restore** = snapshot→Export→stage (full artifact byte-for-byte)→`neo4j-admin
      restore` (RESTORE_OK). Surfaced + fixed a real exporter bug (pass the artifact
      **file** to `--from-path`, not the stage dir — a directory makes neo4j-admin
      match by target-db name and fail when restoring to a renamed target).
- [ ] **Runner responsibility (not connector):** ensure `neo4j-admin restore` runs with
      the live Neo4j config/home so it writes the data dir the server reads, runs as the
      `neo4j` user (store-file ownership), and the Enterprise catalog mounts the restored
      db (`CREATE DATABASE`). The connector mechanics are validated; this DB-online step
      is the tool's lifecycle job.
- [ ] Add SDK plugin entrypoints (`plugin/neo4j-importer/main.go`, etc.) + align the
      `go-kloset-sdk` version for `.ptar` packaging (`plakar pkg build`).
- [ ] testcontainers suite automating both round-trips + offline-against-running fail-fast.
- [ ] Edition detection + Bolt-based node/relationship counts in the manifest (online).
- [ ] Verify temp-dir lifecycle (refcounted cleanup) under partial consumption.
- [ ] Move to a `PlakarKorp/integrations` fork (branch `integration/neo4j`, subdir `neo4j/`) and open the PR.
