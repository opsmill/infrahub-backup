# Neo4j integration for Plakar (DRAFT)

> **Status: draft — compile-verified, NOT behavior-verified.** The kloset connector
> wiring (importer/exporter registration, record/manifest emission) follows the
> verified patterns of `integration-mysql`/`integration-postgresql`. The
> `neo4j-admin` invocations and output-layout handling are implemented per
> documented behavior but **must be validated against a real Neo4j with a
> testcontainers suite before the upstream PR**. Not yet submitted to
> `PlakarKorp/integrations` (an outward action pending review + a green test suite).

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

- [ ] Add SDK plugin entrypoints (`plugin/neo4j-importer/main.go`, etc.) + align the
      `go-kloset-sdk` version for `.ptar` packaging (`plakar pkg build`).
- [ ] testcontainers suite: Enterprise online round-trip, Community offline round-trip,
      offline-against-running fail-fast — validate exact `neo4j-admin` flags + output layout.
- [ ] Edition detection + Bolt-based node/relationship counts in the manifest (online).
- [ ] Verify temp-dir lifecycle (refcounted cleanup) under partial consumption.
- [ ] Move to a `PlakarKorp/integrations` fork (branch `integration/neo4j`, subdir `neo4j/`) and open the PR.
