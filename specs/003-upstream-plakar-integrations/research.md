# Phase 0 Research: Rework Plakar backend onto upstream database integrations

> **⚠️ Partly superseded (2026-08-07).** R-decisions about consuming the Neo4j integration "from the fork via `go.mod replace` until upstream merges/publishes the standalone mirror" no longer apply — there is no upstream merge to wait for. It now lives at `opsmill/plakar-integration-neo4j` and is consumed as a tagged external dependency. See [SUPERSEDED-BY-004.md](./SUPERSEDED-BY-004.md).

**Feature**: `003-upstream-plakar-integrations` · **Date**: 2026-06-29

This document resolves the unknowns flagged in the spec's Assumptions & Dependencies and Technical Context. Each item is recorded as Decision / Rationale / Alternatives.

---

## R1. Consumption model — in-process Go-import vs. Plakar CLI/plugin (the spike gate)

**Decision**: Treat the consumption model as a gated decision resolved by a runnable compile spike (Task 0). Evidence strongly indicates **in-process Go-import is feasible**, which in turn enables the cleanest runtime artifact: **the runner is our own binary** (which already embeds `kloset` + `integration-fs`/`-s3`), extended to register the `postgres`/`neo4j` connectors, plus the database client tools. The Plakar-CLI/plugin model is the documented fallback if the spike fails.

**Rationale**:
- The kloset connector API surface that `integration-postgresql` uses is **identical** between the version it targets (`kloset v1.1.0-beta.2`) and the version this repo pins (`v1.1.0-beta.6`): the `Importer`/`Exporter` interfaces, `importer.Register`/`exporter.Register`, `connectors.Record`/`NewRecord`/`Options`/`Result`, and `location.Flags`/`FLAG_*` are unchanged. `integration-postgresql/importer` and `/exporter` import **only** these stable `kloset` packages (not the SDK), so a `go.mod` alignment (a `replace`/bump of kloset to a single version) should compile without shims.
- `kloset` and `go-kloset-sdk` now have **stable `v1.1.0`** releases (and `plakar v1.1.3`); aligning this repo and the integrations to stable `v1.1.0` is preferable to mixing betas.
- The inside-container runtime decision (R5) means whatever runs the dump must live in an image co-located with the DB. If in-process works, that image is just *our binary* + client tools — one artifact we already build, no separate `plakar` binary or `.ptar` plugin management.

**Alternatives considered**:
- **Plakar CLI + plugins** (`plakar pkg add postgresql/neo4j`, drive via subprocess): officially-documented distribution, version-decoupled. Rejected as the default because it adds a second runtime (the `plakar` binary) and plugin-package management inside the runner image, and the neo4j plugin would have to be built from our fork anyway. Retained as the **fallback** if the spike shows the in-process import cannot be made to compile/register cleanly.
- **Keep the custom importer**: rejected — this is exactly what the feature removes.

**Spike definition (Task 0, gating)**: in a throwaway module, add `integration-postgresql` (latest, currently `v1.1.0-beta.7`) + this repo's `kloset`, blank-import its `importer`/`exporter`, build, and confirm the `postgres` connector registers. Pass → in-process; fail → Plakar-CLI fallback. Record the outcome in plan.md before the runner work commits to an approach.

**✅ SPIKE RESULT (2026-06-30 — PASSED)**: A throwaway module requiring `integration-postgresql@v1.1.0-beta.7` + `kloset@v1.1.0` with blank imports of its `importer` and `exporter` packages **builds cleanly** (`go build` success). MVS resolved `kloset` to the stable `v1.1.0` (higher than the integration's `beta.2` requirement), confirming the connector API surface is compatible — **no shims needed**. **Decision locked: in-process Go-import.** The runner artifact is therefore our own binary (already embeds kloset + fs/s3) extended to register the `postgres`/`neo4j` connectors, plus DB client tools — no separate `plakar` binary or `.ptar` plugins. The Plakar-CLI fallback is not needed.

---

## R2. Plakar invocation (CLI reference, used by the fallback and for restore semantics)

**Decision**: Document both surfaces; the implementation uses whichever R1 selects.

- **Repository**: location schemes `fs:///abs/path` (local) and `s3://bucket/prefix` (object store); created with `plakar repo create [--passphrase …] <location>`; operated with `plakar at <location> <cmd>`. Encryption/passphrase is set at repo-create time, not per-backup. (This matches the current in-process `storeConfig` behavior in `plakar.go`.)
- **Backup**: `plakar at <repo> backup <source-uri> [opt=val …]` — e.g. `postgres://user:pass@host:5432/db`, options as URI query/k=v.
- **Restore**: `plakar destination add <name> <target-uri> [opt=val]` then `plakar restore -to @<name> <snapid>`; or `plakar restore -to ./dir <snapid>` for plain file restore.
- **Plugins (fallback only)**: `plakar pkg build <name>` / `plakar pkg create <manifest> <ver>` → `.ptar`; `plakar pkg add <ptar|name>`; `plakar pkg show`.

**Rationale**: Needed for the fallback path and to mirror the integrations' option semantics in our connector config maps. **Alternatives**: none — this is reference material.

---

## R3. PostgreSQL integration consumption (task-manager / Prefect DB)

**Decision**: Consume `integration-postgresql` **as-is** via the logical `postgres://` connector for the single `prefect`/task-manager database. Backup options: `database=<prefect-db>` (single DB), `compress=false` (let kloset dedup), default SSL. Restore via the `postgres://` exporter with `clean`/`recreate`/`no_owner` as needed, `database=<prefect-db>`.

**Rationale**:
- The importer produces `/00000-globals.sql` (pg_dumpall globals), `/0000N-<db>.dump` (pg_dump `-Fc`), and `/manifest.json`; the exporter dispatches `.dump`→`pg_restore`, globals→`psql`. This is a clean superset of what `backup_taskmanager.go` does today.
- Importer options available: `database`, `exclude_databases`, `compress`, `schema_only`, `data_only`, `pg_bin_dir`, `ssl_mode`/`ssl_*`, host/port/username/password overrides. Exporter adds: `databases`, `clean`, `recreate`, `no_globals`, `no_owner`, `exit_on_error`.
- Connection URI: `postgres://[user[:password]@][host][:port][/database]`.

**Alternatives**: `postgres+bin://` (physical `pg_basebackup`) — rejected; logical is correct for a single application DB and is version-portable. `postgres+aws://` (IAM) — not applicable to the in-cluster Prefect DB.

---

## R4. Neo4j integration design (new upstream deliverable)

**Decision**: Build a generic Neo4j integration mirroring the **`mysql`** integration's layout, encapsulating the exact `neo4j-admin` commands the tool runs today. Two importer protocols + matching exporters.

**Layout** (mirror `integrations/mysql/`): `importer/importer.go`, `exporter/exporter.go`, `manifest/{manifest,metadata}.go`, `neo4jconn/conn.go`, `plugin/*/main.go` (SDK `EntrypointImporter`/`EntrypointExporter`), `manifest.yaml`, `Makefile`, `tests/` (testcontainers), `README.md`, `neo4j.1`, `go.mod` → `module github.com/PlakarKorp/integration-neo4j`.

**URI schemes**:
- `neo4j://[user:pass@]host:6362/<db>` → **Enterprise online backup**. Importer runs `neo4j-admin database backup --to-path=<tmp> [--include-metadata=…] --compress=false <db>` against the backup service; streams the produced artifact into snapshot records. DB stays up.
- `neo4j+offline:///<datadir>?database=<db>` → **Community offline dump**. Importer runs `neo4j-admin database dump --to-stdout <db>` (or `--to-path`) against the on-disk store; **requires the DB stopped** (precondition, fail-fast, not managed by the integration).

**Exporters**:
- `neo4j://` → `neo4j-admin database restore --from-path=… [--overwrite-destination] <db>` (Enterprise).
- `neo4j+offline://` → `neo4j-admin database load --from-path=…/--from-stdin <db>` (Community).

**Manifest** (`/manifest.json`, written before data): engine version, edition, database name, store format, and node/relationship counts via a Bolt query **when reachable** (Enterprise online); counts omitted for Community offline (DB stopped → no Bolt). Edition/version detect via `neo4j-admin server --version` / config.

**Snapshot record layout**: `/manifest.json` + the backup artifact (Enterprise: the backup files under a directory prefix; Community: a single `/<db>.dump`). Restore dispatches on the layout.

**Rationale**: These are precisely the commands the current `backup_neo4j.go` runs (`neo4j-admin database backup --compress=false --to-path`, `neo4j-admin database dump --to-stdout`, `neo4j-admin database load --from-stdin`, `neo4j-admin database restore --from-path`), now packaged generically. `--compress=false` keeps data dedup-friendly for kloset. The two-scheme split mirrors postgres `postgres://` (network) vs `postgres+bin://` (data-dir), and is the honest model: Enterprise backup connects to a service port, Community dump needs store-file access.

**Alternatives**:
- Single `neo4j://` scheme auto-detecting edition — rejected: online (host:port) vs offline (datadir path) have fundamentally different connection semantics; one scheme would overload the URI confusingly.
- Integration manages start/stop — rejected: a generic connector has no orchestration authority over the user's DB; lifecycle stays with the caller (FR-010).

**Open confirmations** (do not block design; verify in implementation): exact `neo4j-admin backup` remote-vs-colocated constraints per Neo4j version, and `--include-metadata` availability. Mirror whatever the current tool already relies on.

---

## R5. Runtime: co-located runner launch (net-new plumbing, both Compose and K8s)

**Decision**: Add a one-shot **runner** launched co-located with each database. Today the tool is **exec-only** (`docker compose exec` / `kubectl exec` via subprocess; no client-go, no ephemeral-workload creation) — so this is new code in both backends.

- **Docker Compose**: new `RunEphemeralContainer()` — discover the compose network and the target service's volume mounts (`docker inspect … .Mounts`), then `docker run --rm --network <project>_default -v <store/repo mounts> -e <creds> <runner-image> <cmd>`. Stream/capture output.
- **Kubernetes**: new `CreateEphemeralJob()` — render a Job/Pod with `restartPolicy: Never`, the target's PVC(s) mounted (discovered via `kubectl get pod -o yaml` → `.spec.volumes`/`volumeMounts`), creds as env, co-location via node/pod affinity; `kubectl apply`, poll `kubectl get job`, collect `kubectl logs`. Subprocess `kubectl` (consistent with current code).

**Runner image** (new build artifact): if R1 = in-process → **our binary** + `pg_dump`/`pg_restore`/`psql` + `neo4j-admin` (+ JRE). If R1 = Plakar-CLI → `plakar` + postgresql/neo4j plugins + the same client tools. Built and versioned alongside the binaries (Make + flake + CI).

**Rationale**: Preserves the zero-host-dependency property (FR-004) and is the only model that covers Community offline dumps (needs store-file access, R6/R7). **Alternatives**: host + port-forward (rejected in spec — host deps + no Community offline); exec into the existing DB container (rejected — DB images lack `plakar`/our binary, and Community needs the DB process stopped while something else reads the store).

---

## R6. Credential discovery and injection

**Decision**: Reuse the existing auto-discovery and inject results into the runner as env vars; add an explicit override (FR-023).

**Rationale**: `app_config.go` already discovers credentials by exec-ing `env` in the deployment: Neo4j from `infrahub-server` (`INFRAHUB_DB_DATABASE`/`USERNAME`/`PASSWORD`, default `neo4j:admin`), Postgres/Prefect from `task-manager` (`PREFECT_SERVER_DATABASE_CONNECTION_URL`, 2.x fallback) parsed into user/pass/db/host; `loadCredentialsFromEnvironment()` already lets host env vars take precedence. The runner receives these as `-e`/Job env (e.g. `PGPASSWORD`, the neo4j auth) and/or baked into the connector URI. **Alternatives**: operator-only supply (rejected — regression from today's zero-config); reading mounted secret files (possible later, not required).

---

## R7. Neo4j Community stop/start in the runner model

**Decision**: Replace the in-container SIGTERM-process + watchdog + SIGCONT dance with: **stop the Neo4j writer** (stop the container/scale the workload down) → launch the runner mounting the now-quiesced store volume → run the offline dump/load → restart the writer. The watchdog binary becomes unnecessary.

**Rationale**: Today `backup_neo4j.go`/`backup_neo4j_watchdog.go` SIGTERM the neo4j process inside the *running* container and use an embedded watchdog to prevent auto-restart, because the dump ran via exec in that same container. With a separate runner mounting the volume, the store must simply be quiescent — achieved by stopping the writer container/workload, which is cleaner and removes the watchdog. The tool still owns this lifecycle (FR-009). **Alternatives**: keep SIGTERM+watchdog and exec the runner into the stopped-but-present container — rejected: the DB image lacks the runner; mounting the volume into a purpose-built runner is simpler and uniform with K8s.

**Risk**: ensure the writer is fully stopped (not just paused) before the offline dump to avoid store inconsistency; verify volume is released.

---

## R8. Backup repository reachability from the runner

**Decision**: For `fs://` local repos, mount the host backup directory into the runner (Compose `-v`, K8s a hostPath/PVC) and pass the in-container path; for `s3://`, pass the URI + inject `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY` (and `INFRAHUB_S3_ENDPOINT`) into the runner. Mirrors current `plakar.go storeConfig` credential resolution, just relocated into the runner env.

**Rationale**: The runner, not the host tool, now writes the kloset repo, so the repo must be reachable from inside it. Compose mount is straightforward; K8s needs a volume or an object-store repo (a local `fs://` repo in K8s requires a shared/host volume — document this; `s3://` is the clean K8s path). **Alternatives**: tool writes the repo and streams data from the runner (rejected — re-introduces the split the custom importer existed to bridge; defeats using the integration as a whole connector).

---

## R9. Snapshot grouping, tags, and edition detection (retained)

**Decision**: Keep the existing tag scheme and grouping (`snapshots.go`): one snapshot per component, all sharing `infrahub.backup-id`, with `component`/`backup-status`/`version`/`neo4j-edition`/`components`/`redacted` tags; group completeness from the tag set. Keep `detectNeo4jEditionInfo` to choose `neo4j://` vs `neo4j+offline://` and the Community lifecycle. Keep `--redact`.

**Rationale**: These are Infrahub-specific and orthogonal to where the dump runs; the integrations add their own `/manifest.json` inside each snapshot without disturbing tags. **Alternatives**: rely solely on integration manifests for grouping — rejected; cross-component grouping is the tool's concern.

---

## R10. Versions & upstream contribution path

**Decision**: Target stable **`kloset v1.1.0`** / **`go-kloset-sdk v1.1.0`** / **`plakar v1.1.3`**; consume `integration-postgresql` at its latest (`v1.1.0-beta.7`). Develop `integration-neo4j` on a fork of `PlakarKorp/integrations` (orphan branch `integration/neo4j`, subdir `neo4j/`, `module github.com/PlakarKorp/integration-neo4j`); consume it from the fork via `go.mod replace` (in-process) or `plakar pkg build` (fallback) until upstream merges/publishes the standalone mirror. Open the upstream PR (base branch `integration/neo4j`) in parallel.

**Rationale**: Aligning to stable removes beta drift; the fork-build keeps delivery unblocked on upstream acceptance (FR-021). **Alternatives**: pin betas (rejected — churn); wait for upstream publish (rejected — blocks delivery).

---

## Resolved unknowns summary

| Unknown | Resolution |
|---|---|
| In-process vs CLI | In-process feasible (API stable beta.2↔beta.6); gated by Task 0 spike; runner = our binary if it passes, else Plakar-CLI |
| Plakar/connector invocation | Documented (R2); option maps from integration schemas (R3/R4) |
| Neo4j integration shape | mysql-template layout; `neo4j://` (online) + `neo4j+offline://` (offline); manifest + neo4j-admin commands (R4) |
| Runner launch | New ephemeral-container (Compose) / Job (K8s) plumbing; subprocess-based (R5) |
| Credentials | Reuse auto-discovery, inject as runner env, add override (R6) |
| Community stop/start | Stop writer + mount quiesced volume; drop watchdog (R7) |
| Repo reachability | Mount `fs://` dir or inject `s3://` creds into runner (R8) |
| Grouping/tags/edition | Retained as-is (R9) |
| Versions / upstream | Stable v1.1.0 line; fork-build of integration-neo4j; parallel PR (R10) |

No `NEEDS CLARIFICATION` markers remain.
