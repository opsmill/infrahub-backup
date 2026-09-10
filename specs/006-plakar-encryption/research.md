# Phase 0 Research: Encrypt the Plakar backend at rest, and extract the Neo4j integration

**Feature**: `006-plakar-encryption` · **Date**: 2026-06-30, extended 2026-08-07

Resolves the spec's open items across both workstreams. For **A (encryption)** the gating spike (key-management feasibility) is **done** and changed the design from keypair to passphrase — R1–R5. For **B (extraction)** the findings are R6–R10, all verified against the live repositories rather than inferred.

---

## R1. Key-management model — SPIKE RESULT (decision: passphrase / symmetric)

**Decision**: Use kloset's **native symmetric** repository encryption, keyed by a **passphrase**.

**Spike evidence** (kloset v1.1.0 source):

- `connectors/storage.Configuration.Encryption` is `*encryption.Configuration` — kloset's **symmetric** config (`encryption/symmetric.go`): `{ SubKeyAlgorithm: AES256-KW, DataAlgorithm: AES256-GCM-SIV, KDFParams (default ARGON2ID), Canary }`.
- Repository decryption is symmetric: `repository.New(ctx, secret, store, config)` → `encryption.DecryptStream(config.Encryption, r.secret, …)`. `secret == nil` ⇒ plaintext (today).
- `encryption/keypair` is **ed25519** (`Generate/Sign/Verify`, `FromPublicKey/FromPrivateKey`) used for snapshot **signing/verification** (`snapshot/verify*`), **not** encryption.

**Consequence / rationale**: There is no asymmetric repo-encryption path. Writing an encrypted repo (and reading existing chunks for dedup) requires the symmetric secret, so an asymmetric "backup needs only the public key" model is impossible natively. Passphrase (symmetric) is the native, dedup-preserving option.

**Alternatives considered**:

- *Keypair (asymmetric)* — ruled out by the spike (not how kloset encrypts repos).
- *Custom ECIES key-wrapping* (wrap a random symmetric repo key under an ECIES public key, reuse `encryption.go`) — would give "public-key-only" backup **only for a fresh repo**; incremental backups + restore still need the symmetric secret, and it's bespoke crypto on top of kloset. Rejected: high risk, partial benefit.

---

## R2. Encrypted-repo create & open flow (kloset API)

**Create (encrypted)** — in `openOrCreateRepo`:

```text
enc := encryption.NewDefaultConfiguration()        // Argon2id + AES256-GCM-SIV
secret, _ := encryption.DeriveKey(enc.KDFParams, []byte(passphrase))
enc.Canary, _ = encryption.DeriveCanary(enc, secret)
storageConfig.Encryption = enc                      // (today: = nil)
// serialize + storage.Create as today, then:
repository.New(kctx, secret, store, configBytes)    // (today: secret = nil)
```

**Open (existing)** — in `openRepo`/`openOrCreateRepo`:

```text
store, configBytes, _ := storage.Open(kctx, sc)
cfg, _ := storage.NewConfigurationFromBytes(version, <unwrapped config>)
if cfg.Encryption != nil {                          // encrypted repo
    if passphrase == "" { return error("repository is encrypted; a passphrase is required") }
    secret, _ := encryption.DeriveKey(cfg.Encryption.KDFParams, []byte(passphrase))
    if !encryption.VerifyCanary(cfg.Encryption, secret) { return error("incorrect passphrase for encrypted repository") }  // FR-012, fail-fast
    repository.New(kctx, secret, store, configBytes)
} else {
    if passphrase != "" { warn("repository is not encrypted; passphrase ignored") }
    repository.New(kctx, nil, store, configBytes)
}
```

**Decision**: add a `secret []byte` (derived) path to `plakar.go`'s repo helpers; **Canary** gives the FR-012 fail-fast check. **Alternatives**: attempting `New(nil)` and catching a decrypt error — rejected; the canary is the intended, clean check and we must read `KDFParams` from the repo config to derive the key anyway.

---

## R3. Passphrase source & **secure injection into the runner**

**Decision**:

- The host tool obtains the passphrase from (in order) an explicit flag/file → env var (e.g. `INFRAHUB_BACKUP_PASSPHRASE`). Non-interactive (FR-011).
- **Host-side ops** (repo create, the in-process metadata snapshot, `snapshots list`) use the passphrase directly.
- **Runner ops**: pass the passphrase to the `__run-connector` worker via **stdin** (`docker run -i`, worker reads stdin), NOT via argv and NOT via `-e`. Rationale (FR-007/FR-011): `docker inspect` exposes both the command line and env, so argv/`-e` would leak the secret into persisted container metadata; stdin does not. The worker gets a `--credentials-stdin` flag and reads one JSON object. It carries the passphrase, the database password and any object-store credentials together: the same `docker inspect` argument that applies to the passphrase applies to a password embedded in a connector URI, and the review found exactly that — so one channel covers all of them rather than protecting the passphrase alone.

**Alternatives**: `-e INFRAHUB_BACKUP_PASSPHRASE` (rejected — visible in `docker inspect`/env dump); a tmpfs-mounted secret file (viable but more moving parts than stdin); argv `--passphrase` (rejected — visible in `docker inspect` + process list).

---

## R4. Flag/CLI surface

**Decision**: reuse the existing `--encrypt` flag to mean "encrypt the plakar repo", and accept the passphrase via `INFRAHUB_BACKUP_PASSPHRASE` (env) or `--passphrase-file` (path); wire `--encrypt` + the passphrase through `CreateBackup → CreatePlakarBackup` and into `RestorePlakarBackup` + `snapshots list` (all currently drop them on the plakar path). `--encrypt-key` (the legacy ECIES public-key path) is **not** reused for the plakar backend (it's asymmetric/file-level; symmetric passphrase is the model). Document that `--encrypt-key` remains tarball-only.

**Rationale**: minimal new surface; consistent verb (`--encrypt`); non-interactive secret via env/file. **Alternatives**: a brand-new `--passphrase` inline flag (rejected — secrets on argv leak via the process list); interactive prompt only (rejected — breaks unattended/scheduled backups).

---

## R5. Scope confirmations

- **Create-time only**: encryption is fixed when the repo is created (kloset writes `Encryption` into the repo CONFIG). A plaintext repo stays plaintext; no in-place toggle (FR-008, non-goal).
- **Both editions + Postgres**: encryption is a property of the *repository*, orthogonal to which connector writes into it — so neo4j (enterprise/community) + postgres + metadata all inherit it once the repo is encrypted. The runner just needs the passphrase on every op.
- **Compose-first**: matches 003; K8s runner still pending (non-goal here).
- **No cache encryption beyond kloset's**: the local pebble dedup cache is out of scope (non-goal).

---

## Resolved unknowns

| Unknown | Resolution |
|---|---|
| Key model | Passphrase / symmetric (spike: kloset repo encryption is symmetric-only; keypair = ed25519 signing) — R1 |
| Create/open API | `NewDefaultConfiguration` + `DeriveKey` + `DeriveCanary`/`VerifyCanary`; `repository.New(secret)`; detect via `cfg.Encryption` — R2 |
| Wrong-key fail-fast | `VerifyCanary` before read/write (FR-012) — R2 |
| Secure runner injection | passphrase via **stdin** to the worker (not argv/`-e`) — R3 |
| CLI surface | `--encrypt` + `INFRAHUB_BACKUP_PASSPHRASE`/`--passphrase-file`; `--encrypt-key` stays tarball-only — R4 |

---

## R6. Which tree is the source of truth — fork subtree vs. `contrib/` *(workstream B begins here)*

**Decision**: seed the new repository from the **fork's `neo4j/` subtree** (`BeArchiTek/integrations`, branch `integration/neo4j`), not from `contrib/integration-neo4j`.

**Evidence** (direct comparison of both trees):

| File | fork | `contrib/` | verdict |
|---|---|---|---|
| `exporter/exporter.go` | 179 lines | 179 lines | **identical** |
| `importer/importer.go` | 225 | 225 | **identical** |
| `neo4jconn/conn.go` | 164 | 164 | **identical** |
| `manifest/manifest.go` | 70 | 70 | **identical** |
| `manifest.yaml` | 21 (2 connectors) | 33 (4 connectors) | differs — see R10 |
| `go.mod` | 86 lines (SDK + testcontainers) | 22 (kloset only) | fork is complete |
| `plugin/neo4j-{importer,exporter}/` | present | **absent** | fork only |
| `tests/` (+ `testhelpers/`) | present | **absent** | fork only |
| `LICENSE`, `.gitignore`, `.github/workflows/test.yml` | present | **absent** | fork only |
| `Makefile` | working targets | present but **empty** | fork only |

**Rationale**: the Go logic is byte-identical, so there is nothing to merge — but `contrib/` is a *reduced* copy that cannot be packaged (no plugin entrypoints) or tested (no suite). Extraction is therefore a relocation plus a metadata correction.

**Timeline check** that rules out hidden divergence: the fork's single neo4j commit `fa0f304` is dated 2026-07-01, and this repo's last connector fix `a17434a` is also 2026-07-01 — the fork was pushed carrying that fix. (An initial shallow clone appeared to show one commit as proof of a clean history; that was a `--depth 1` artifact and was re-verified against a full clone.)

**Alternatives**: seeding from `contrib/` and re-adding the missing scaffolding by hand — rejected as strictly more work with more room for error.

---

## R7. Repository shape, naming, and version scheme

**Decision**: standalone public repository `opsmill/plakar-integration-neo4j`, Go module at the repository **root**, flat tags, first release **`v0.1.0`**.

**Evidence**: `PlakarKorp/integration-proxmox` demonstrates that the hub accepts a standalone per-plugin repository with a **flat** tag at the `v1.1.0` compatibility level (its tag is `v1.1.0-rc.1`), rather than requiring the prefixed `<name>/<version>` tags that their monorepo uses. Its root layout — `.github/ LICENSE Makefile README.md exporter/ go.mod go.sum importer/ internal/ manifest.yaml plugin/` — is the shape being copied.

**Rationale**: one integration does not need a monorepo, and flat tags are simpler for both `go get` and the hub builder. `v0.1.0` keeps the integration's version independent of kloset's: engine compatibility is already carried twice (the manifest's `api_version` and the hub recipe's `community/v1.1.0/` directory), so spending the git tag on it a third time would force a version bump on every engine release and would assert a v1 Go API stability promise this module has not earned with one consumer.

**Alternatives**: a monorepo `opsmill/plakar-integrations` with `neo4j/` and prefixed tags — rejected as YAGNI; tags `v1.1.0` or `v1.1.0-rc.1` — rejected for the coupling above.

---

## R8. Hub distribution — recipe format, and the out-of-org question

**Recipe format** (verified; it is three fields):

```yaml
name: neo4j
version: v0.1.0
repository: https://github.com/opsmill/plakar-integration-neo4j
```

placed at `community/v1.1.0/neo4j/recipe.yaml`. The hub's own README documents the path convention `{tier}/{kloset-compat-version}/{plugin}/recipe.yaml`, and each recipe directory contains only `recipe.yaml`.

**Finding that corrected an earlier assumption**: a survey of **all 19** `community/v1.1.0` recipes shows every one points at a repository **inside the PlakarKorp organisation** — 18 at `PlakarKorp/integrations`, and proxmox at `PlakarKorp/integration-proxmox`. Proxmox's *manifest* credits an outside maintainer (`gillesdubois`, `tier: third-party`, their own contact), but the repository the builder fetches is in-org. So proxmox is precedent for the **shape** (standalone repo, flat tag, third-party provenance) but **not** for out-of-org hosting.

**Decision**: proceed with OpsMill hosting on the strength of Plakar's explicit statement that a community integration may live on any git host, treating the recipe submission as the confirmation. Fallback if the builder cannot fetch it: adopt the proxmox arrangement — Plakar host or mirror the repository while the manifest continues to name OpsMill as maintainer. The OpsMill repository remains the development home under either outcome, so this risk does not gate any other step.

**Alternatives**: confirming on Discord before creating the repository (rejected — blocks work that pays off regardless); donating the repository to PlakarKorp from the outset (rejected — premature, and reversible into if needed).

---

## R9. Consumption model and the engine-coupling it introduces

**Decision**: consume the integration as an external, version-pinned module and keep the **in-process** registration model unchanged.

Only the import path moves — `src/internal/app/connectors.go` keeps its blank imports, because `importer.init()` calls `iimporter.Register("neo4j", …)` and `Register("neo4j+offline", …)`, and `exporter.init()` the matching pair. The `plugin/` entrypoints are separate `main` packages, so packaging and in-process use coexist without conflict.

**New risk this creates**: Go's minimal-version selection compiles the integration against whichever kloset version wins across the build. Previously a kloset upgrade that broke the connector was a one-line fix in `contrib/`; now it fails inside a module this repo cannot patch locally. **Mitigation** (FR-024): don't adopt an engine version without a compatible integration release, with a *temporary* `replace` as the sanctioned unblock that must be absent from any merged state.

**Dependency-leak question**: the new module requires `go-kloset-sdk` and `testcontainers-go`, used only by `plugin/` and `tests/`. Go 1.17+ module-graph pruning should keep them out of this repo's build, since it imports neither package. Treated as a **verification step against `go mod graph`**, not an assumption (SC-010).

**Alternatives**: keeping `contrib/` with a retargeted `replace` (rejected — preserves the two-copy drift that caused R10); a git submodule (rejected — adds clone/CI/Nix ceremony for no gain over a tagged module).

---

## R10. Manifest correctness — a live defect the extraction fixes

**Finding**: `contrib/integration-neo4j/manifest.yaml` declares **four** executables — `neo4jImporter`, `neo4jOfflineImporter`, `neo4jExporter`, `neo4jOfflineExporter` — and a repo-wide search finds those names in **no other file**. Nothing builds them; `contrib/` has no `plugin/` directory at all. The fork's manifest instead declares **two** connectors, each listing `protocols: [neo4j, neo4j+offline]`, matching its two plugin entrypoints.

**Why the fork's form is the correct one**: both schemes are registered against the *same* constructor (`NewImporter` / `NewExporter`), and each plugin entrypoint passes the SDK the requested protocol, which `NewImporter` dispatches on. One binary per direction genuinely serves both schemes.

**Why it went unnoticed**: in-process registration happens in `init()`, so `manifest.yaml` is dead metadata inside this repository — the defect could only have surfaced the first time someone packaged the integration for external use.

**Decision**: adopt the fork's 2-connector manifest, and require manifest-to-entrypoint agreement as a verifiable gate (FR-016, SC-007) so the class of defect cannot recur silently.

**Provenance corrections applied at the same time**: `tier: official` → `third-party` (the vendored value falsely claimed Plakar maintenance), `contact: mailto:help@plakar.io` → `mailto:support@opsmill.com`, and `homepage` from `PlakarKorp/integration-neo4j` to the OpsMill repository.

---

## Resolved unknowns — workstream B

| Unknown | Resolution |
|---|---|
| Source of truth for the new repo | Fork's `neo4j/` subtree; Go files identical, fork uniquely has plugin/tests/LICENSE/Makefile/CI — R6 |
| Repo shape, name, module path | Standalone `opsmill/plakar-integration-neo4j`, module at root, flat tags — R7 |
| First version tag | `v0.1.0`, independent of kloset compat — R7 |
| Hub recipe format & location | Three fields at `community/v1.1.0/neo4j/recipe.yaml` — R8 |
| Whether out-of-org hosting works | Unproven by precedent (all 19 recipes are in-org); proceeding on Plakar's statement, proxmox fallback available — R8 |
| Consumption model | External tagged module; in-process registration unchanged; only import paths move — R9 |
| Engine-coupling policy | Gate + temporary `replace` removed before merge (FR-024) — R9 |
| Whether SDK/testcontainers leak into this build | Expected no (graph pruning); **verify** via `go mod graph` — R9 |
| Which manifest form is correct | Fork's 2-connector form; `contrib/`'s 4 executables are phantom — R10 |

No `NEEDS CLARIFICATION` remain in either workstream.
