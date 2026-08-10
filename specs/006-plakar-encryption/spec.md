# Feature Specification: Encrypt the Plakar backup backend at rest

**Feature Branch**: `004-plakar-encryption` — the feature directory was renumbered to `006-` because `004-` and `005-` were taken by features that landed on `main` first; the branch keeps its original name because PRs are open against it.
**Created**: 2026-06-30
**Status**: Draft
**Input**: User description: "Add at-rest encryption to the Plakar (kloset) backup backend, which is currently plaintext."

## Overview

The reworked Plakar backend (feature 003) writes **plaintext** backup repositories — anyone who obtains the repository (a local directory or an object-store bucket) can read every backed-up database. Feature 003 deferred encryption as an explicit non-goal. The legacy tar.gz backend already encrypts its archive (public-key encryption with an optional custom key and a `keygen` helper); the equivalent flags exist on the backup command but are silently ignored on the Plakar path.

This feature makes Plakar backups **encrypted at rest**: a leaked or stolen repository is unreadable without the key. Encryption is established when the repository is first created and the key must be supplied for every operation against it (backup, restore, and listing/inspection), including when those run inside the co-located runner introduced in 003.

### Second workstream: extract the Neo4j integration (added 2026-08-05)

Feature 003 built a generic Neo4j integration and planned to upstream it into `PlakarKorp/integrations`. Plakar have since advised that this is unnecessary: their monorepo hosts *their* integrations, while community integrations stay wherever their maintainer prefers and become installable through a recipe in `PlakarKorp/hub`. This spec therefore also covers extracting the integration out of `contrib/integration-neo4j` into its own OpsMill-owned repository, consumed here as a normal versioned dependency.

The two workstreams are independent — encryption concerns the repository format, extraction concerns where the integration's source lives — but they share this branch because neither has shipped yet.

**Terminology.** This spec uses Plakar's own vocabulary. An **integration** is the distributable unit: one repository, one `manifest.yaml`, one installable package. It contains **connectors** — importer and exporter entries, each bound to the URI schemes it serves. Each connector is built as a **plugin** executable. So the Neo4j *integration* ships two *connectors* (an importer and an exporter) covering both `neo4j://` and `neo4j+offline://`, built as two *plugin* binaries. This matches existing usage in `src/internal/app/connectors.go`, which registers the importer/exporter pairs for Neo4j and Postgres.

## Clarifications

### Session 2026-08-06

- Q: What is the integration repository's canonical name, and therefore its Go module path? → A: **`opsmill/plakar-integration-neo4j`** → module `github.com/opsmill/plakar-integration-neo4j`. The `plakar-` prefix is retained because, inside an organisation that ships integrations for several ecosystems, an unqualified `integration-neo4j` would not say which ecosystem it belongs to.
- Q: What version tag should the integration's first release carry? → A: **`v0.1.0`**, versioned independently of kloset. Plakar compatibility is already carried by the manifest's `api_version` and by the hub recipe's directory, so the git tag is free to track the integration's own maturity; `v0.x` also correctly signals that its exported Go API may still change, having had one consumer so far.
- Q: Externalising the integration means a backup-engine upgrade here can break source this repository owns but can no longer patch in place. What is the policy? → A: **Gate plus a temporary escape hatch.** This repository does not adopt an engine version until a compatible integration release exists; to unblock development or CI in the meantime, a **temporary** `replace` to a local checkout or commit SHA is sanctioned and MUST be removed before merge, so the escape hatch cannot become a permanent second copy.
- Q: No hub recipe currently points at a repository outside the Plakar organisation. What happens if the builder cannot serve one? → A: **Proceed as designed, with the proxmox arrangement as fallback.** The repository, its history, its corrected provenance and the external-dependency rewiring are all worthwhile regardless of the hub outcome, and the submission is already gated. Should the builder require an in-organisation repository, ask Plakar to host or mirror it there while the manifest continues to name OpsMill as maintainer — the OpsMill repository stays the development home either way.
- Q: "Connector", "integration" and "plugin" were being used interchangeably. Which is canonical for what? → A: **Adopt Plakar's own hierarchy** (see Terminology in the Overview): **integration** = the distributable unit (repository, manifest, package); **connector** = an importer or exporter entry within it, bound to URI schemes; **plugin** = the built executable for a connector. This also matches existing usage in `src/internal/app/connectors.go`, so only this spec's prose needed correcting.

### Session 2026-08-05

- Q: Where should the Neo4j integration live, given Plakar's guidance that community integrations need not be upstreamed? → A: **Its own public repository, `opsmill/plakar-integration-neo4j`** — a Go module at the repository root with flat version tags, following the standalone repository shape of `PlakarKorp/integration-proxmox`. Not a fork of their monorepo and not a subdirectory of one.
- Q: How should `infrahub-backup` consume it? → A: **As an external, version-pinned module dependency.** Delete `contrib/integration-neo4j` and the `replace` directive. The existing in-process blank-import registration in `connectors.go` is unchanged — only the import path moves.
- Q: How much history should the new repository carry? → A: **Fresh `git init`, single commit.** No fork relationship and none of the `PlakarKorp/integrations` monorepo history, since the repository is now independently maintained.
- Q: Which source of truth should the new repository be built from — `contrib/integration-neo4j` or the fork's `neo4j/` subtree? → A: **The fork's subtree.** The four Go source files are byte-identical between them, but the fork additionally carries the SDK plugin entrypoints, the testcontainers suite, `LICENSE`, a working `Makefile`, and CI — all of which were stripped from the in-repo copy. `contrib/`'s `manifest.yaml` had also drifted to declare four executables that nothing builds; the fork's two-connector form is correct, because both `neo4j` and `neo4j+offline` register against the same constructor.
- Q: What provenance should the published manifest claim? → A: `tier: third-party` (not `official`, which would be a false claim), an OpsMill homepage, and `mailto:support@opsmill.com` as contact. ISC license retained with dual copyright: PlakarKorp 2025 for the derived scaffolding, OpsMill 2026 for the integration.

### Session 2026-06-30

- Q: Which key-management model should the encrypted Plakar repository use? → A (initial): keypair (asymmetric). **→ Revised after the plan-phase spike: PASSPHRASE (symmetric).** The spike found kloset repository encryption is **symmetric-only** (`storage.Configuration.Encryption` is `*encryption.Configuration`; `encryption.DeriveKey(passphrase)` → a secret used by `repository.New`), and kloset's `encryption/keypair` is **ed25519 for snapshot *signing*, not encryption**. An asymmetric "backup needs only the public key" model is therefore not achievable natively (writing + dedup against the repo require the symmetric secret). Decision: use the native **symmetric passphrase**, supplied non-interactively (env/file) for backup, restore, and listing.
- Q: Should the tool validate passphrase strength? → A: **Yes — enforce a minimum length.** At create time, reject a passphrase shorter than 12 characters with a clear message (before any repository is created); document the requirement. (The Argon2id KDF additionally slows brute force.)
- Q: What happens if `--encrypt-key` (the legacy tarball ECIES flag) is passed with `--backend plakar`? → A: **Error.** Reject it with a clear message redirecting the user to `--encrypt` + `INFRAHUB_BACKUP_PASSPHRASE` — never silently ignore a flag the user believes controls encryption.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Create an encrypted backup (Priority: P1)

An operator runs a backup with encryption enabled. The resulting repository is encrypted: its contents cannot be read or restored without the key. Subsequent backups to the same repository continue to use that encryption.

**Why this priority**: This is the core protection — without it a leaked repository exposes all backed-up data. It is the foundation every other scenario builds on.

**Independent Test**: Run an encrypted backup into a fresh repository; confirm the repository's stored bytes do not reveal database contents (no plaintext dump recoverable) and that the backup group is recorded as complete.

**Acceptance Scenarios**:

1. **Given** encryption is requested and a key is provided, **When** the operator backs up, **Then** an encrypted repository is created and the backup succeeds.
2. **Given** an existing encrypted repository and the correct key, **When** the operator backs up again, **Then** the new snapshots are added under the same encryption.
3. **Given** a stored encrypted repository, **When** it is inspected without the key, **Then** the database contents are not recoverable from the stored bytes.

---

### User Story 2 - Restore from an encrypted backup (Priority: P1)

An operator restores a deployment from an encrypted repository by supplying the key. The databases are restored exactly as a plaintext restore would, for both Neo4j editions and Postgres.

**Why this priority**: An encrypted backup is only valuable if it can be restored; backup and restore must round-trip.

**Independent Test**: Take an encrypted backup, restore it into a clean deployment with the key, and confirm the restored databases match the source.

**Acceptance Scenarios**:

1. **Given** an encrypted repository and the correct key, **When** the operator restores, **Then** Neo4j (Enterprise online and Community offline) and Postgres are restored with data intact.
2. **Given** an encrypted repository and a **missing or wrong** key, **When** the operator restores, **Then** the operation fails fast with a clear "cannot open encrypted repository — key required/incorrect" message and changes nothing.

---

### User Story 3 - List/inspect an encrypted repository (Priority: P2)

An operator lists the backup groups in an encrypted repository. With the key, the listing works as for a plaintext repository; without it, the listing fails clearly rather than showing partial or garbled output.

**Why this priority**: Operators routinely inspect repositories before restoring; this must behave consistently with backup/restore.

**Independent Test**: List an encrypted repository with the key (groups shown) and without it (clear failure).

**Acceptance Scenarios**:

1. **Given** an encrypted repository and the key, **When** the operator lists snapshots, **Then** the backup groups and their status are shown.
2. **Given** no/incorrect key, **When** the operator lists snapshots, **Then** a clear key-required error is returned.

---

### User Story 4 - Install the Neo4j integration standalone (Priority: P2)

A Plakar user who has never heard of Infrahub wants to back up their own Neo4j database. They install the integration through Plakar's package tooling and use it directly, with no OpsMill software involved.

**Why this priority**: This is what makes the integration a genuine community contribution rather than an implementation detail of this repository. It is also the only path by which Plakar's own users benefit from the work.

**Independent Test**: Build the integration's package from a clean checkout of its repository, install it into a Plakar installation, and confirm both URI schemes are usable for a backup and a restore against a throwaway Neo4j instance.

**Acceptance Scenarios**:

1. **Given** a clean checkout of the integration repository, **When** the package is built, **Then** a plugin executable is produced for every connector its manifest declares, with none declared but missing.
2. **Given** the built package, **When** it is installed into Plakar, **Then** both `neo4j://` (Enterprise online) and `neo4j+offline://` (Community offline) are registered and usable.
3. **Given** the integration's published manifest, **When** a user inspects its provenance, **Then** it identifies OpsMill as maintainer and does not claim Plakar-official status.

---

### User Story 5 - Consume the integration as a versioned dependency (Priority: P2)

A maintainer of this repository needs to pick up a fix in the integration. They bump a version and rebuild — they do not hand-edit a vendored copy, and there is no second copy that can silently drift.

**Why this priority**: The vendored copy already drifted once, producing a `manifest.yaml` that declared four connectors nothing built. Making the dependency external removes the class of bug rather than the instance.

**Independent Test**: From a clean checkout, build and test this repository with no vendored integration tree and no `replace` directive present.

**Acceptance Scenarios**:

1. **Given** the integration published at a tag, **When** this repository builds, **Then** it resolves the integration as an external module with no `replace` directive and no in-tree copy.
2. **Given** a new integration release, **When** a maintainer bumps the version, **Then** picking up the change requires only that bump and a vendor-hash refresh.
3. **Given** the external dependency, **When** the build graph is inspected, **Then** the integration's plugin- and test-only dependencies have not become build dependencies of this repository.

---

### Edge Cases

- **Wrong key against an encrypted repo** — fail fast with a clear message; never partially read or corrupt the repository.
- **Key provided for a plaintext repo, or no key for an encrypted repo** — detect the mismatch and report it clearly (don't silently produce an unusable result).
- **Encryption requested but no key/passphrase available** — refuse to start rather than create a repository the operator can't reproduce the key for.
- **Key material exposure** — the key/passphrase must never appear in logs, error messages, or persisted process metadata, including where it is handed to the co-located runner.
- **Mixing**: targeting an existing encrypted repository for a new backup without the key — fail clearly (cannot append to an encrypted repo blindly).
- **`--encrypt-key` with `--backend plakar`** — reject with a clear error pointing to `--encrypt` + a passphrase (the plakar backend does not use the legacy ECIES public key); never silently ignore it.
- **Passphrase too short on create** — reject (minimum 12 characters) before creating the repository.

#### Integration extraction

- **Manifest declares a connector no plugin entrypoint builds** — the packaged integration would advertise a connector that fails at runtime. The manifest's declared executables and the repository's plugin entrypoints must agree (this is the defect the vendored copy already carried).
- **Two copies of the integration** — a vendored tree plus an external module invites silent divergence; exactly one source of truth must remain.
- **Plugin/test dependencies leaking into this repository's build** — the integration's SDK and testcontainers dependencies serve only its plugin entrypoints and its own tests, and must not become build dependencies here.
- **Stale references to the old module path or the fork** — imports, specs, and project documentation that still name `PlakarKorp/integration-neo4j` or the fork would mislead the next maintainer.
- **Provenance overclaim** — a published manifest asserting `tier: official` would falsely present an OpsMill integration as Plakar-maintained.
- **Nix vendor hash left stale after the dependency change** — the Nix build breaks unless the hash is regenerated by the repository's script.
- **Backup-engine upgrade breaks the integration's source** — minimal-version-selection compiles the integration against whichever engine version wins, so an upgrade here can fail inside a module this repository owns but no longer patches locally. The upgrade waits on a compatible integration release; a temporary `replace` may unblock work in progress but must not reach the mainline.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The tool MUST be able to create an **encrypted** Plakar repository when encryption is requested, establishing the encryption at repository-create time.
- **FR-002**: The tool MUST supply the key material for **every** operation against an encrypted repository — create, backup, restore, and list/inspection — including when the operation runs inside the co-located runner.
- **FR-003**: A full backup→restore round-trip against an encrypted repository MUST preserve data for **both** Neo4j editions (Enterprise online, Community offline) and Postgres.
- **FR-004**: The tool MUST wire encryption into the Plakar **create** and **restore** paths (the existing encryption flags are currently ignored on the Plakar path), plus a key/passphrase input suitable for unattended use (e.g., an environment variable or file, not only an interactive prompt).
- **FR-005**: Opening an encrypted repository **without** the correct key (for restore, listing, or appending a backup) MUST fail fast with a clear, actionable message and make no changes.
- **FR-006**: Requesting encryption **without** available key material MUST be refused before any repository is created.
- **FR-007**: The key/passphrase MUST NOT be written to logs, error output, or persisted process/command metadata — including when injected into the runner.
- **FR-008**: Plaintext repositories MUST continue to work unchanged; encryption is chosen at create time and is **not** applied retroactively to existing plaintext repositories.
- **FR-009**: Encryption MUST be optional — backups remain plaintext unless encryption is explicitly requested (no behavior change for existing 003 users who don't opt in).
- **FR-010**: The key-management model MUST be a **passphrase** that derives the repository's symmetric encryption key (the engine's native KDF). The **same passphrase** is required to create, back up to, restore from, and list/inspect an encrypted repository.
- **FR-011**: The passphrase MUST be suppliable **non-interactively** (environment variable or file) so scheduled/unattended backups work, and MUST be injected into the runner without appearing on its command line, environment dump, or logs.
- **FR-012**: The tool MUST verify the supplied passphrase against the encrypted repository **before** reading or writing, so a wrong/absent passphrase fails fast and clearly instead of producing corrupt or partial output.
- **FR-013**: When creating an encrypted repository, the tool MUST reject a passphrase shorter than **12 characters** with a clear message, **before** creating the repository (preventing trivially-weak keys).

#### Integration extraction

- **FR-014**: The Neo4j integration MUST live in its own **public** repository under the OpsMill organisation, named **`opsmill/plakar-integration-neo4j`** and declaring the module path **`github.com/opsmill/plakar-integration-neo4j`**, as a Go module at the repository root with its own version tags.
- **FR-015**: This repository MUST consume the integration as an **external, version-pinned module dependency** — no vendored copy of its source and no `replace` directive.
- **FR-016**: The integration repository MUST be buildable into an installable Plakar package, and every connector its manifest declares MUST have a corresponding plugin entrypoint (no connector declared but missing).
- **FR-017**: The integration MUST remain usable **in-process** by this repository through scheme registration at import time, while simultaneously being packageable as standalone plugin binaries — the two consumption models coexist.
- **FR-018**: The integration's published manifest MUST state accurate provenance: third-party tier, an OpsMill homepage, and an OpsMill contact address. It MUST NOT claim Plakar-official status.
- **FR-019**: The integration repository MUST retain the ISC license, attributing PlakarKorp for the derived scaffolding alongside OpsMill's own copyright.
- **FR-020**: A hub recipe naming the OpsMill repository and its published tag MUST be authored and ready to submit, so that Plakar users can install the integration once it is accepted. Authoring the recipe is in scope; **submitting** it is a gated follow-up, permitted only after the repository is public, tagged, and its package build verified.
- **FR-021**: Both URI schemes the integration's connectors serve (`neo4j://` Enterprise online, `neo4j+offline://` Community offline) MUST remain functional through the extraction, in-process and packaged alike.
- **FR-022**: Documentation and specifications in this repository that describe the integration's location or its planned upstreaming MUST be corrected to reflect the OpsMill-owned repository and the hub-recipe distribution path.
- **FR-023**: The integration MUST be versioned **independently of the backup engine**, with its first release tagged **`v0.1.0`**. Engine compatibility is expressed by the manifest's `api_version` and the hub recipe's compatibility directory — never by the integration's own tag — so an engine release MUST NOT compel an integration version bump, and an integration fix MUST be releasable without implying an engine change.
- **FR-024**: This repository MUST NOT adopt a backup-engine version for which no compatible integration release exists. A **temporary** `replace` directive to a local checkout or commit SHA is permitted to unblock development or CI during such an upgrade, and MUST be absent from any merged state — so that the steady-state requirement of exactly one integration source of truth (FR-015) always holds on the mainline.

### Key Entities *(include if data involved)*

- **Encrypted repository**: a Plakar repository whose stored contents are encrypted; its encryption is fixed at create time and recorded in the repository's own configuration.
- **Key material**: a **passphrase** that derives the repository's symmetric encryption key (engine KDF). The same passphrase is needed for every operation (create, backup, restore, list); there is no public/private split.
- **Runner**: the co-located one-shot context (from 003) that performs backup/restore; it receives the **passphrase** for every operation, securely (never on its command line / env dump / logs).
- **Integration repository**: the OpsMill-owned public repository holding the Neo4j integration as a root-level Go module — the single source of truth for its code, replacing the vendored `contrib/` tree.
- **Integration manifest**: the integration's published descriptor, declaring its display metadata, its provenance (maintainer, tier, license, contact), and its connectors.
- **Connector**: an importer or exporter entry within the manifest, naming the plugin executable that implements it, the URI schemes it serves, and its configuration validator. The Neo4j integration declares two — one importer, one exporter — each serving both schemes.
- **Hub recipe**: the entry submitted to Plakar's hub that makes the integration installable, naming the integration, the repository to fetch, and the tag to build.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: An encrypted repository's stored bytes contain **no recoverable plaintext** of the backed-up databases (verified by inspecting the repository without the key).
- **SC-002**: An encrypted backup→restore round-trip restores **both** Neo4j editions and Postgres with data matching the source in 100% of test runs.
- **SC-003**: Attempting to restore, list, or append-backup an encrypted repository **without** the correct key fails with a clear error and zero changes, in 100% of test runs.
- **SC-004**: The key/passphrase never appears in tool logs, error messages, or the runner's command line / persisted metadata (verified by scanning output + process/command inspection).
- **SC-005**: Existing plaintext (003) repositories and the no-encryption path continue to work unchanged (no regression for users who don't opt in).
- **SC-006**: A wrong or absent passphrase against an encrypted repository is rejected by a key-verification check **before** any read/write (no corrupt/partial output), in 100% of test runs.

### Integration extraction

- **SC-007**: A clean checkout of the integration repository builds, vets, and passes its own test suite, and produces a plugin executable for every connector its manifest declares — with zero declared but missing.
- **SC-008**: This repository builds, tests, lints, and vets clean against the external integration module, with no `replace` directive and no in-tree copy remaining.
- **SC-009**: Both `neo4j://` and `neo4j+offline://` round-trip a backup and restore after the extraction, for Enterprise online and Community offline respectively — matching the results already recorded for 003.
- **SC-010**: The integration's plugin- and test-only dependencies are absent from this repository's **build closure** — measured by `go list -deps` and `go mod why`, not by `go mod graph`. The module graph legitimately contains an edge for each of them, because the integration's own `go.mod` declares them; that edge is not a build dependency, so grepping `go mod graph` would report a false failure.
- **SC-011**: No reference to the old module path or to the personal fork remains in this repository's code, specifications, or project documentation, except where deliberately recorded as history.
- **SC-012**: The integration's published manifest states a third-party tier and an OpsMill contact, and its license file credits both PlakarKorp and OpsMill.

## Assumptions & Dependencies

- **Builds on 003**: depends on the reworked Plakar backend + the co-located runner (branch `003-upstream-plakar-integrations`). Encryption is wired through that same backup/restore/runner flow.
- **Native repository encryption**: uses the backup engine's built-in **symmetric** repository encryption (a passphrase-derived key supplied when the repository is opened), rather than encrypting files after the fact. A plan-phase spike confirmed the engine's repository encryption is symmetric-only (its `keypair` is ed25519 *signing*, not encryption), so an asymmetric model is not used.
- **Encryption at create time**: a repository is either encrypted or plaintext from creation; switching requires creating a new repository.
- **Docker Compose first**: consistent with 003, which targets Compose; the Kubernetes runner remains pending and is out of scope here.
- **Unattended-friendly key input**: key material can be supplied non-interactively (env/file) so scheduled backups work.

### Integration extraction

- **Plakar's guidance (2026-08-05)**: Plakar confirmed that their integrations monorepo is for integrations they maintain, that community integrations may live on any git host, and that distribution comes from a recipe in their hub. This supersedes 003's plan to open a pull request against `PlakarKorp/integrations`.
- **Per-plugin repositories with flat tags are supported by the hub**: `PlakarKorp/integration-proxmox` is hub-listed from a standalone per-plugin repository carrying a flat version tag, confirming that the repository shape and tag scheme chosen here are workable.
- **Out-of-organisation repositories are unproven in the hub**: every current hub recipe points at a repository inside the Plakar organisation — most at their monorepo, and proxmox at a per-plugin repository that is likewise theirs, despite its manifest crediting an outside maintainer. The basis for expecting an OpsMill-hosted repository to be servable is therefore Plakar's own statement that a community integration may live on any git host, **not** existing precedent. The recipe submission is the confirmation; if the builder cannot fetch an external repository, the fallback is the proxmox arrangement — hosted in the Plakar organisation with the manifest still naming OpsMill as maintainer and contact. Under either outcome the OpsMill repository remains the integration's development home.
- **Integration source is already settled**: the fork's subtree and the vendored copy hold byte-identical importer and exporter sources, so extraction is a relocation and a metadata correction — not a merge of divergent implementations.
- **Docker is required for the integration's own tests**: its suite provisions real Neo4j containers, so it needs a host with working, uncontended Docker.
- **Nix packaging**: any dependency change here requires regenerating the vendor hash through the repository's existing script rather than by hand.

## Out of Scope (Non-Goals)

- Changing the legacy tar.gz backend's existing encryption.
- In-place re-encryption or key rotation of an existing repository (create a new encrypted repository instead).
- Encrypting the local dedup/cache state beyond what the backup engine already does.
- Kubernetes support (the 003 K8s runner is still pending).
- Upstreaming the integration into `PlakarKorp/integrations` (Plakar have advised against it; superseded by the hub recipe).
- Extracting or relocating any other integration — the Postgres, filesystem, and S3 integrations continue to be consumed as upstream Plakar releases.
- Retiring or altering the personal fork; it is left in place for now.
- Applying OpsMill's standard repository governance (branch protection, shared CI conventions) to the new repository — worth doing, tracked separately.

**Deferred rather than excluded**: opening the hub pull request. The recipe is authored as part of this work (FR-020) but submitted only after the repository is public, tagged, and package-build verified.
