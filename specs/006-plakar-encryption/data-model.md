# Data Model: Encrypt the Plakar backend at rest, and extract the Neo4j integration

**Feature**: `006-plakar-encryption` · **Date**: 2026-06-30, extended 2026-08-07

No application schema is introduced by either workstream. For **A (encryption)** the entities are the repository's encryption configuration and the runtime key material. For **B (extraction)** they are packaging and distribution artefacts — the integration's manifest, its connectors, and the dependency edge into this repository.

## Entities — workstream A (encryption)

### Encrypted repository

A kloset repository whose stored contents are symmetric-encrypted.

- **Attributes**: location (`fs://`/`s3://`); `encrypted` (bool); the engine **Encryption configuration** (below) persisted in the repo CONFIG at create.
- **State**: created plaintext **or** encrypted — fixed at create; not toggleable in place (FR-008).
- **Relationships**: holds the same component snapshots (neo4j/postgres/metadata) as a plaintext repo; encryption is orthogonal to the connectors.

### Encryption configuration (in the repo CONFIG)

The engine's symmetric encryption parameters, written when the repo is created.

- **Attributes**: `KDFParams` (default Argon2id), `SubKeyAlgorithm` (AES256-KW), `DataAlgorithm` (AES256-GCM-SIV), `Canary` (a value encrypted with the derived key, used to verify a supplied passphrase).
- **Source**: `encryption.NewDefaultConfiguration()`; `Canary = DeriveCanary(config, secret)` at create.
- **Identity**: the KDF salt/params make each repo's derived key unique even for the same passphrase.

### Passphrase (key material)

The operator secret.

- **Attributes**: the passphrase string (never persisted by the tool, never logged).
- **Derivation**: `secret = DeriveKey(KDFParams, passphrase)` → the symmetric repository key.
- **Sources**: env `INFRAHUB_BACKUP_PASSPHRASE` or `--passphrase-file <path>`; required when `--encrypt` is set and when opening an encrypted repo.
- **Lifecycle**: held in memory by the host tool; passed to the runner via stdin for the duration of one op; never written to argv/env/logs.

### Runner (from 003)

- **Receives**: the passphrase on **stdin** (`--passphrase-stdin`) for any op against an encrypted repo; derives the secret + verifies the canary inside the container.

## Validation rules

- **VR-1**: `--encrypt` without an available passphrase ⇒ refuse before creating anything (FR-006).
- **VR-2**: opening an encrypted repo without a passphrase, or with one that fails `VerifyCanary`, ⇒ fail fast, no read/write (FR-005, FR-012).
- **VR-3**: a passphrase supplied for a **plaintext** repo ⇒ warn + ignore (don't error the whole op).
- **VR-4**: the passphrase MUST NOT appear in logs, error text, the runner command line, `-e` env, or `docker inspect` output (FR-007, FR-011).
- **VR-5**: encryption applies to every component (neo4j enterprise/community, postgres, metadata) once the repo is encrypted, and to create/backup/restore/list alike (FR-002, FR-003).
- **VR-6**: on `create --encrypt`, a passphrase shorter than **12 characters** is rejected **before** the repository is created (FR-013).
- **VR-7**: `--encrypt-key` (legacy tarball ECIES) passed with `--backend plakar` is **rejected with an error** redirecting to `--encrypt` + passphrase — never silently ignored.

## State transitions (encrypted backup/restore)

```text
create:  --encrypt + passphrase -> NewDefaultConfiguration() + DeriveKey + DeriveCanary -> storage.Create(encrypted)
open:    storage.Open -> cfg.Encryption? -> DeriveKey(KDFParams, passphrase) -> VerifyCanary -> repository.New(secret)
backup:  host derives nothing for the runner; passphrase -> runner stdin -> worker derives secret -> writes encrypted snapshot
restore: passphrase -> runner stdin -> worker opens encrypted repo (canary-verified) -> exporter restores
```

## Entities — workstream B (integration extraction)

### Integration

The distributable unit: one repository, one manifest, one installable package.

- **Attributes**: `name` (`neo4j`); `display_name`; `description`; `api_version` (engine compatibility, `v1.1.0`); `license` (ISC); `tier` (`third-party`); `homepage` + `contact` (OpsMill); own version tag (`v0.1.0`, independent of `api_version` — see R7).
- **Identity**: the repository `opsmill/plakar-integration-neo4j` and its Go module path `github.com/opsmill/plakar-integration-neo4j`.
- **Relationships**: declares 1..n **Connectors**; is referenced by exactly one **Hub recipe**; is consumed by this repository through one **Module dependency**.

### Connector (a manifest entry)

An importer or exporter binding, declared inside the integration's manifest.

- **Attributes**: `type` (`importer` | `exporter`); `executable` (the plugin binary name); `protocols` (the URI schemes it serves); `validator` (a JSON-schema path); `class` (`database`) / `subclass` (`neo4j`).
- **Cardinality here**: exactly **two** — one importer, one exporter — each declaring `protocols: [neo4j, neo4j+offline]`.
- **Relationships**: each connector MUST map 1:1 to a **Plugin executable** (see VR-8, the rule the vendored copy violated).

### Plugin executable

The built binary implementing one connector, for out-of-process use by Plakar.

- **Attributes**: name matching the connector's `executable` (`neo4jImporter`, `neo4jExporter`); built from `plugin/neo4j-{importer,exporter}/` via the SDK entrypoints.
- **Lifecycle**: built by `make build`; packaged by `plakar pkg create`; installed by `plakar pkg add`.
- **Note**: irrelevant to this repository's own use, which registers the same code **in-process** via `init()` — the two models coexist.

### Hub recipe

The three-field entry that makes the integration installable.

- **Attributes**: `name`; `version` (the git tag to build); `repository` (the URL to clone).
- **Location**: `community/v1.1.0/neo4j/recipe.yaml` in `PlakarKorp/hub` — the directory encodes engine compatibility, not the integration's version.

### Module dependency (this repository → the integration)

- **Attributes**: module path; pinned version (`v0.1.0`); **no** `replace` directive; a Nix `vendorHash` derived from the resulting `go.sum`.
- **Invariant**: exactly one source of truth — an external module **or** (transiently, never merged) a `replace` to a local checkout.

## Validation rules — workstream B

- **VR-8**: every `executable` declared in the manifest MUST have a corresponding plugin entrypoint, and vice versa — no connector declared but unbuilt, no binary built but undeclared (FR-016, SC-007). This is the defect `contrib/` carried: four executables declared, zero built.
- **VR-9**: the manifest MUST NOT claim `tier: official`; provenance MUST name OpsMill as maintainer and contact (FR-018, SC-012).
- **VR-10**: no merged state of this repository may contain a `replace` directive for the integration or a vendored copy of its source (FR-015, FR-024, SC-008).
- **VR-11**: the integration's plugin- and test-only dependencies (`go-kloset-sdk`, `testcontainers-go`) MUST be absent from this repository's build graph (SC-010).
- **VR-12**: both `neo4j://` and `neo4j+offline://` MUST remain functional after extraction, in-process and packaged alike (FR-021, SC-009).
- **VR-13**: the `LICENSE` MUST remain ISC and credit both PlakarKorp (derived scaffolding, 2025) and OpsMill (the integration, 2026) (FR-019, SC-012).
- **VR-14**: the integration's tag MUST NOT be derived from the engine's version; an engine release alone MUST NOT force a version bump (FR-023).
- **VR-15**: the `go.mod` change MUST be followed by `scripts/update-vendor-hash.sh`; the hash MUST NOT be hand-edited (CLAUDE.md).

## State transitions (extraction)

```text
seed:     fork neo4j/ subtree -> module path rewrite + manifest provenance fix -> build/vet/test/package verified
publish:  git init -> single commit -> public repo -> tag v0.1.0            (immutable reference point)
rewire:   delete contrib/ -> require v0.1.0 -> drop replace -> go mod tidy -> update-vendor-hash.sh
verify:   make build/test/lint/vet + go mod graph + both scheme round-trips
distribute: author recipe -> [GATE: repo public + tagged + package verified] -> submit hub PR
```
