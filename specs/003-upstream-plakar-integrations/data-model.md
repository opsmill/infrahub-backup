# Data Model: Rework Plakar backend onto upstream database integrations

**Feature**: `003-upstream-plakar-integrations` · **Date**: 2026-06-29

This feature is infrastructure tooling, so the "data model" is the set of runtime entities, their attributes/relationships, and the on-disk **snapshot layouts** produced by the integrations. No application database schema is introduced.

## Entities

### Backup group
The set of component snapshots produced by one backup run.
- **Attributes**: `backup-id` (timestamp, identity), `date`, `infrahub-version`, `backup-tool-version`, `neo4j-edition`, `components[]` (expected), `status` (complete | incomplete), `redacted` (bool).
- **Relationships**: contains 1..N **Component snapshots** sharing its `backup-id`.
- **Source**: derived from snapshot tags at list time (`snapshots.go`), unchanged from today.
- **State**: `complete` iff every expected component is present; otherwise `incomplete`. A half-written group is never presented as restorable (FR-014, SC-006).

### Component snapshot
One captured database stored in its integration's standard layout.
- **Attributes**: `component` (`neo4j` | `postgres`), `backup-status`, `neo4j-edition` (for neo4j), the integration's `/manifest.json`, plus the dump artifact(s).
- **Relationships**: belongs to one Backup group (`backup-id` tag); produced by one Integration.
- **Tags** (retained scheme): `infrahub.backup-id`, `infrahub.component`, `infrahub.backup-status`, `infrahub.version`, `infrahub.backup-tool-version`, `infrahub.neo4j-edition`, `infrahub.components`, `infrahub.redacted`.

### Integration manifest (`/manifest.json`)
Structured metadata written **before** the dump data, inspectable without a restore.
- **Postgres** (from `integration-postgresql`): server version, host, cluster id, config, roles, tablespaces, and per-database schema/relation detail.
- **Neo4j** (from `integration-neo4j`): engine version, edition, database name, store format; node/relationship counts **when reachable** (Enterprise online via Bolt), omitted for Community offline.

### Neo4j edition
- **Values**: `community` | `enterprise` (+ `detected` flag; fallback `community`).
- **Drives**: capture path (`neo4j+offline://` vs `neo4j://`), whether the tool stops the DB, and the manifest's count availability.
- **Source**: `detectNeo4jEditionInfo` (retained).

### Execution environment (runner)
The co-located one-shot context that runs an integration.
- **Attributes**: image ref, target DB endpoint/store path, injected credentials (env), mounted backup repo (path or s3 creds), command (backup/restore + URI + options).
- **Relationships**: launched by the Environment backend (Docker/K8s) co-located with one database; reaches one Backup repository.
- **Lifecycle**: created → runs one connector op → exits; `--rm`/`restartPolicy: Never`.

### Backup repository (kloset store)
- **Attributes**: location (`fs:///abs` | `s3://bucket/prefix`), encryption (none/plaintext default).
- **Reachability**: must be reachable **from inside the runner** — `fs://` mounted in, `s3://` creds injected (R8).

## Snapshot layouts (on disk, per component)

### Postgres (`integration-postgresql`, logical)
```
/manifest.json            # cluster metadata
/00000-globals.sql        # pg_dumpall --globals-only (roles, tablespaces)
/00001-<db>.dump          # pg_dump -Fc per database (here: the single prefect DB)
```
Restore: `.dump` → `pg_restore`; `00000-globals.sql` → `psql` (unless `no_globals`). Lexicographic order guarantees globals first.

### Neo4j — Enterprise online (`neo4j://`)
```
/manifest.json            # version, edition, db, store format, counts (Bolt)
/<backup-artifact>…       # neo4j-admin database backup output (under a prefix)
```
Restore: `neo4j-admin database restore --from-path=… [--overwrite-destination] <db>`.

### Neo4j — Community offline (`neo4j+offline://`)
```
/manifest.json            # version, edition, db, store format (no live counts)
/<db>.dump                # neo4j-admin database dump output
```
Restore: `neo4j-admin database load --from-path=… <db>` (DB stopped).

## Validation rules

- **VR-1**: A backup group is restorable only when `status == complete` (FR-014, SC-006).
- **VR-2**: Community offline capture/restore requires the Neo4j writer **stopped**; the integration fails fast if the precondition is unmet (FR-010); the tool guarantees the stop/start (FR-009).
- **VR-3**: Restore rejects any snapshot **not** in the new integration layout (clean break) with an actionable message pointing to the prior tool version (FR-016, SC-007).
- **VR-4**: The runner must have the required client tool for the operation (`pg_*`, `neo4j-admin`) and a reachable repository, else fail before touching the DB (Edge Cases).
- **VR-5**: Credentials resolve as: explicit override → auto-discovered deployment values → engine default (FR-023, R6).

## State transitions (backup run)

```
detect env + neo4j edition
  → drain tasks (unless --force)
  → [community only] stop app containers + stop neo4j writer
  → for each component: launch runner co-located → integration backup → snapshot (+ tags)
  → [community only] restart neo4j writer + app containers   (also on failure paths)
  → group marked complete iff all components succeeded
```
