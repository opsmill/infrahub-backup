# Implementation Plan: Encrypt the Plakar backend at rest, and extract the Neo4j integration

**Branch**: `004-plakar-encryption` (directory renumbered to `006-`; see spec.md) | **Date**: 2026-08-07 | **Spec**: [spec.md](./spec.md)
**Input**: Feature specification from `specs/006-plakar-encryption/spec.md`

This branch carries **two independent workstreams**. Workstream A (encryption) is implemented and E2E-validated. Workstream B (integration extraction) is planned here and not yet started.

## Summary

**A — Encryption at rest.** Make the reworked Plakar backend (003) able to write **encrypted** repositories using kloset's **native symmetric** encryption, keyed by a **passphrase** (the spike ruled out asymmetric — kloset repo encryption is symmetric; its keypair is ed25519 *signing*; see [research.md](./research.md)). When `--encrypt` is set, the repo is created with an `encryption.Configuration` (Argon2id KDF + AES256-GCM-SIV) and a canary; the passphrase-derived secret is supplied on every open (create, backup, restore, list), including inside the 003 co-located runner — passed to the runner via **stdin** so it never lands on the command line, env dump, or logs. Wrong/absent passphrase fails fast via the canary. Plaintext repos and the no-`--encrypt` path are unchanged.

**B — Extract the Neo4j integration.** Move the integration out of the vendored `contrib/integration-neo4j` tree into its own public repository, `opsmill/plakar-integration-neo4j` (module `github.com/opsmill/plakar-integration-neo4j`, first tag `v0.1.0`), and consume it here as an external version-pinned dependency. The source for the new repository is the **fork's `neo4j/` subtree**, not `contrib/`: the Go sources are byte-identical, but only the fork carries the SDK plugin entrypoints, the testcontainers suite, `LICENSE`, a working `Makefile`, and CI. Publishing corrects provenance the vendored copy got wrong (`tier: official` → `third-party`, Plakar's contact → OpsMill's) and repairs a manifest that declared four connectors nothing builds. A hub recipe is authored but submitted only after the repository is public, tagged, and package-build verified.

## Technical Context

**Language/Version**: Go 1.25.0

**Primary Dependencies**:

- *(A)* `github.com/PlakarKorp/kloset` v1.1.0 — `encryption` (symmetric: `NewDefaultConfiguration`, `DeriveKey`, `DeriveCanary`, `VerifyCanary`), `connectors/storage` (`Configuration.Encryption`, `NewConfigurationFromBytes`), `repository.New(secret, …)`; builds on the 003 runner (`runner.go`, `run_connector.go`)
- *(B)* `github.com/opsmill/plakar-integration-neo4j` v0.1.0 — replaces the `replace`-directed `github.com/PlakarKorp/integration-neo4j`. The new module itself requires kloset v1.1.0, plus `go-kloset-sdk` v1.1.0 and `testcontainers-go` used **only** by its `plugin/` entrypoints and `tests/` (must not enter this repo's build graph)

**Storage**: kloset repository (`fs://` / `s3://`), optionally symmetric-encrypted (KDF + cipher params + canary in the repo CONFIG)

**Testing**: `go test`; E2E backup→restore round-trip with a passphrase against a throwaway Infrahub (both Neo4j editions + Postgres), plus a wrong/absent-passphrase failure check. *(B)* adds the new repository's own testcontainers suite, which requires a working, uncontended Docker host.

**Target Platform**: tool binaries Linux/Darwin/Windows; runner image Linux; Docker Compose deployments (K8s deferred, per 003)

**Project Type**: single Go CLI project (extends 003), now with one **external** Go module extracted out of it

**Performance Goals**: KDF cost is per-open (Argon2id default); negligible vs. dump time. *(B)* no runtime performance dimension — extraction is a relocation.

**Constraints**: passphrase never logged / never on the runner's argv or `-e` env (use stdin); encryption fixed at repo-create time; no in-place re-encryption. *(B)* no `replace` directive and no vendored copy in any merged state; `scripts/update-vendor-hash.sh` must run on the `go.mod` change; the integration versions independently of kloset.

**Scale/Scope**: one deployment, two databases; one repo per backup target. *(B)* one integration extracted; Postgres/fs/S3 integrations stay upstream.

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

`.specify/memory/constitution.md` is the unpopulated template — no ratified principles, so there are no gates to evaluate. The plan follows `CLAUDE.md` conventions (Cobra commands; shared `src/internal/app`; `fmt.Errorf` wrapping; `scripts/update-vendor-hash.sh` on any `go.mod` change).

**Initial gate**: PASS.

**Post-design**: PASS, with one convention consequence worth naming explicitly — workstream B **does** change `go.mod`, so unlike workstream A it requires a vendor-hash regeneration. Workstream A introduced no new module dependencies and left the hash untouched.

## Project Structure

### Documentation (this feature)

```text
specs/006-plakar-encryption/
├── plan.md · research.md · data-model.md · quickstart.md
├── contracts/encryption-and-keys.md      # workstream A
├── contracts/neo4j-integration-repo.md   # workstream B (this plan)
├── checklists/requirements.md
└── tasks.md            # (/speckit.tasks)
```

### Source Code — workstream A (all in this repo)

```text
src/internal/app/
├── plakar.go            # openOrCreateRepo/openRepo: create with Encryption+canary; open derives secret + VerifyCanary
├── app.go               # Configuration/PlakarConfig: add Passphrase (source: env/file) + Encrypt flag plumbing
├── backup.go            # CreateBackup: pass --encrypt + passphrase to CreatePlakarBackup (currently dropped)
├── plakar_backup.go     # CreatePlakarBackup: accept encrypt+passphrase; encrypted ensurePlakarRepo + metadata; pass passphrase to runner
├── plakar_restore.go    # RestorePlakarBackup: accept passphrase; open encrypted repo; pass passphrase to runner
├── snapshots.go         # ListSnapshots/openRepo path: supply passphrase for an encrypted repo
├── runner.go            # LaunchComposeBackup/Restore: inject passphrase via STDIN (docker run -i), not argv/-e
└── run_connector.go     # __run-connector: add --credentials-stdin; derive secret; open repo encrypted
src/cmd/infrahub-backup/main.go   # wire --encrypt (plakar) + INFRAHUB_BACKUP_PASSPHRASE / --passphrase-file into create/restore/snapshots;
                                  # enforce 12-char min passphrase (FR-013); reject --encrypt-key with --backend plakar (clarify)
```

### Source Code — workstream B

**New external repository** `opsmill/plakar-integration-neo4j` (root-level Go module, seeded from the fork's `neo4j/` subtree):

```text
.github/workflows/test.yml   # retargeted: drop the monorepo `neo4j/` working-directory assumption
LICENSE                      # ISC; dual copyright (PlakarKorp 2025 derived scaffolding, OpsMill 2026)
Makefile                     # build/package/install/test targets; VERSION → v0.1.0
README.md                    # fork's as base, folding in contrib/'s operational detail; URLs retargeted
manifest.yaml                # fork's 2-connector form; tier third-party; OpsMill homepage + support@opsmill.com
go.mod                       # module github.com/opsmill/plakar-integration-neo4j
importer/ · exporter/        # byte-identical to contrib/ (schema.json alongside each)
manifest/ · neo4jconn/       # byte-identical to contrib/
plugin/neo4j-importer/       # SDK EntrypointImporter — import path follows the module rename
plugin/neo4j-exporter/       # SDK EntrypointExporter — import path follows the module rename
tests/                       # testcontainers offline round-trip + helpers
```

**Changes in this repository:**

```text
contrib/integration-neo4j/        # DELETED (10 tracked files)
go.mod                            # require github.com/opsmill/plakar-integration-neo4j v0.1.0; drop the replace
go.sum                            # regenerated by `go mod tidy`
flake.nix                         # vendorHash regenerated via scripts/update-vendor-hash.sh
src/internal/app/connectors.go    # two blank imports + the scheme-mapping comment retargeted
specs/003-upstream-plakar-integrations/{tasks,quickstart,research,contracts}  # annotate statements the hub path supersedes (notably T040)
CLAUDE.md                         # Active Technologies: integration-neo4j "(new, fork build)" → the opsmill module
```

**Structure Decision**: Workstream A is an in-place extension of the 003 plakar backend + runner — no new packages, modules, or build artifacts; encryption is a property of the repository (set at create, supplied at open), so it composes with the per-component runner flow without touching the integrations. Workstream B moves a module *out* rather than adding one: this repo's package layout is unchanged apart from deleting `contrib/` and retargeting two import paths. The in-process registration model is deliberately retained — `plugin/` entrypoints exist so external Plakar users can `pkg add`, and the two consumption models coexist because registration happens in `init()` while the plugin binaries are separate `main` packages.

## Workstream B sequencing

Ordered by dependency, with the gate each step must clear before the next begins.

| Step | Work | Gate |
|---|---|---|
| B1 | Seed the new repository from the fork's `neo4j/` subtree; rewrite module path and plugin imports; correct `manifest.yaml` provenance; dual-copyright `LICENSE`; merge READMEs; retarget CI | `go build ./...` and `go vet ./...` clean; `make build` yields `neo4jImporter` + `neo4jExporter` |
| B2 | Verify the manifest/entrypoint agreement that `contrib/` broke | every `executable:` in `manifest.yaml` has a `plugin/` entrypoint; both schemes register (FR-016, SC-007) |
| B3 | Run the integration's own test suite | `make test` green on a quiet Docker host |
| B4 | Verify packaging | `plakar pkg create ./manifest.yaml v0.1.0` produces the `.ptar` |
| B5 | Publish: fresh `git init`, single commit, push public repo, tag `v0.1.0` | repo public and tag resolvable by `go get` |
| B6 | Rewire this repo: delete `contrib/`, swap the require, drop the `replace`, `go mod tidy`, `scripts/update-vendor-hash.sh` | `make build test lint vet` clean; no `replace`; SDK/testcontainers absent from `go mod graph` (SC-008, SC-010) |
| B7 | Re-run the Neo4j round-trips through the external module | Enterprise online + Community offline both round-trip (FR-021, SC-009) |
| B8 | Correct stale references across specs, `CLAUDE.md`, and docs | no stray `PlakarKorp/integration-neo4j` or fork references outside deliberate history (FR-022, SC-011) |
| B9 | Author the hub recipe | recipe ready; **submission gated** on B5 + B4 (FR-020) |

B6 is the point of no return for the vendored copy, so B2–B4 deliberately front-load every check that is cheaper to run before the tree is deleted.

## Risks & Mitigations

| Risk | Mitigation |
|---|---|
| Passphrase leakage (logs, `docker inspect` argv/env, process list) | Pass to the runner via **stdin** only (R3); never log it; redact in errors; host-side ops hold it in memory only. SC-004 scans for leaks. |
| Wrong passphrase → corrupt/partial output | `VerifyCanary` **before** any read/write (FR-012); fail fast with a clear message. |
| Lost passphrase ⇒ unrecoverable backups | Document prominently; encryption is opt-in; no key escrow in scope. |
| Runner stdin plumbing (docker run -i + writing the secret to the worker) | Small, testable; covered by the E2E (encrypted round-trip both editions + postgres). |
| Mixing encrypted/plaintext repos | Detect via `cfg.Encryption` on open; clear errors for passphrase-without-encryption and encryption-without-passphrase. |
| **(B)** Hub builder cannot serve an out-of-org repository — no existing recipe does | Verified: all 19 recipes point inside PlakarKorp. Basis is Plakar's explicit statement, not precedent. Submission is gated and non-blocking; fallback is the proxmox arrangement (they host, manifest still credits OpsMill). The OpsMill repo stays the development home either way. |
| **(B)** A kloset upgrade here breaks the integration's source, which can no longer be patched in place | FR-024: don't adopt an engine version without a compatible integration release; a **temporary** `replace` unblocks work and must be absent from any merged state. |
| **(B)** Plugin/test-only deps (`go-kloset-sdk`, `testcontainers-go`) leak into this repo's build | Go 1.17+ module-graph pruning should exclude them since only `plugin/` and `tests/` use them — verify against `go mod graph` rather than assume (SC-010). |
| **(B)** Deleting `contrib/` before the external module is proven | Sequencing: B2–B4 verify build, manifest agreement, tests, and packaging *before* B6 removes the tree; the tag at B5 is immutable and recoverable. |
| **(B)** Stale vendor hash breaks the Nix build | Regenerate only via `scripts/update-vendor-hash.sh` (CLAUDE.md mandate), never by hand. |
| **(B)** Publishing a public repo seeded from ISC-licensed upstream scaffolding | Retain ISC; dual copyright crediting PlakarKorp for the derived layout and OpsMill for the integration (FR-019, SC-012). |

## Complexity Tracking

No constitutional violations — the constitution has no ratified principles. Workstream A adds no dependencies or artifacts. Workstream B **removes** a vendored tree and converts it into one external dependency, which lowers rather than raises structural complexity; its only new obligation is the vendor-hash regeneration that any `go.mod` change already requires. Nothing to justify.
