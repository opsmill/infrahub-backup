# Contract: `integration-neo4j` (new upstream connector)

> **⚠️ SUPERSEDED (2026-08-07)** by [`specs/006-plakar-encryption/contracts/neo4j-integration-repo.md`](../../006-plakar-encryption/contracts/neo4j-integration-repo.md). The module is **not** `github.com/PlakarKorp/integration-neo4j` and is not destined for their monorepo — it is `github.com/opsmill/plakar-integration-neo4j`, in its own repository, distributed via a `PlakarKorp/hub` recipe. The *technical* contract below (protocols, layout, behaviour) still describes the integration accurately; only its identity and distribution changed. Note also that the manifest form recorded here was later found to declare connector executables that nothing builds — see [SUPERSEDED-BY-006.md](../SUPERSEDED-BY-006.md).

Generic Plakar integration for Neo4j. No Infrahub-specific assumptions. Mirrors the `integration-mysql` layout. Module: `github.com/PlakarKorp/integration-neo4j`.

## Importer protocols (source URIs)

### `neo4j://[user:pass@]host:6362/<db>` — Enterprise online backup
- **Registers**: `importer.Register("neo4j", FLAG_STREAM, NewImporter)`.
- **Behavior**: runs `neo4j-admin database backup --to-path=<tmp> --compress=false [--include-metadata=…] <db>` against the Enterprise backup service; emits `/manifest.json` then the backup artifact as snapshot records. DB stays online.
- **Precondition**: backup service reachable on the host:port (default 6362); fail fast otherwise.

### `neo4j+offline:///<datadir>?database=<db>` — Community offline dump
- **Registers**: `importer.Register("neo4j+offline", FLAG_STREAM, NewOfflineImporter)`.
- **Behavior**: runs `neo4j-admin database dump --to-stdout <db>` (or `--to-path`) against the on-disk store at `<datadir>`; emits `/manifest.json` then `/<db>.dump`.
- **Precondition**: the database is **STOPPED** and the store files are accessible; fail fast with a clear error if the DB appears running. The integration does **not** stop/start the DB.

## Importer options
| Option | Applies | Meaning |
|---|---|---|
| `database` | both | database name (also in URI path/query) |
| `compress` | online | pass-through to `neo4j-admin` (default `false` for kloset dedup) |
| `include_metadata` | online | include restore metadata if supported |
| `neo4j_admin_path` | both | explicit `neo4j-admin` path (else `$PATH`) |
| `data_dir` | offline | store directory (also in URI) |
| `auth` / `username`+`password` | online | Bolt creds for manifest counts |

## Exporter protocols (restore)
- `neo4j://` → `neo4j-admin database restore --from-path=<extracted> [--overwrite-destination] <db>` (Enterprise; DB stopped for the restore window).
- `neo4j+offline://` → `neo4j-admin database load --from-path=<extracted>/--from-stdin <db>` (Community; DB stopped).
- **Exporter options**: `overwrite` (→ `--overwrite-destination`), `database`, `neo4j_admin_path`.

## Manifest (`/manifest.json`, written before data)
```json
{
  "version": 1,
  "engine_version": "Neo4j 5.x",
  "edition": "enterprise|community",
  "database": "neo4j",
  "store_format": "…",
  "node_count": 0,            // present only when Bolt is reachable (online)
  "relationship_count": 0     // omitted for offline
}
```

## Snapshot record layout
- Online: `/manifest.json` + backup files under a directory prefix.
- Offline: `/manifest.json` + `/<db>.dump`.
- Restore dispatches on the presence of a `.dump` (load) vs a backup directory (restore).

## Packaging
- Library packages (`importer/`, `exporter/`) importable in-process **and** `plugin/*/main.go` SDK entrypoints + `manifest.yaml` for `.ptar` packaging. `manifest.yaml` declares importer connectors for `[neo4j]` and `[neo4j+offline]` and matching exporters, each with a `schema.json` validator, `class: database`, `subclass: neo4j`.

## Tests (testcontainers)
- Enterprise: online backup of a seeded DB → restore into a fresh container → counts match.
- Community: stop → offline dump → load into a fresh container → counts match.
- Precondition failure: offline dump against a running DB → fails fast.

## Non-goals
- Lifecycle management (start/stop) — the caller owns it.
- Cluster/causal-clustering topology backup beyond single-database backup/dump.
