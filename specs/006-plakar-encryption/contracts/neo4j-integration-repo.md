# Contract: `opsmill/plakar-integration-neo4j`

**Feature**: `006-plakar-encryption` (workstream B) · **Date**: 2026-08-07

Supersedes `specs/003-upstream-plakar-integrations/contracts/neo4j-integration.md`, which specified the module as `github.com/PlakarKorp/integration-neo4j` destined for their monorepo. Plakar have since advised that community integrations need not be upstreamed, so the module is OpsMill-owned and distributed through a hub recipe instead.

A generic Plakar integration for Neo4j. No Infrahub-specific assumptions — it must be usable by a Plakar user who has never heard of Infrahub.

## Identity

| Field | Value |
|---|---|
| Repository | `github.com/opsmill/plakar-integration-neo4j` (public) |
| Go module | `github.com/opsmill/plakar-integration-neo4j` (at repository **root**) |
| First tag | `v0.1.0` (flat; independent of engine compatibility) |
| Engine compatibility | kloset `v1.1.0` — declared as `api_version` in the manifest |
| License | ISC, dual copyright: PlakarKorp 2025 (derived scaffolding), OpsMill 2026 (integration) |

## Repository layout

```text
.github/workflows/test.yml      # CI; must not assume a monorepo subdirectory
.gitignore
LICENSE
Makefile                        # all/build/package/install/uninstall/reinstall/test/clean
README.md
manifest.yaml
go.mod  go.sum
importer/importer.go            importer/schema.json
exporter/exporter.go            exporter/schema.json
manifest/manifest.go
neo4jconn/conn.go
plugin/neo4j-importer/          # sdk.EntrypointImporter(os.Args, importer.NewImporter)
plugin/neo4j-exporter/          # sdk.EntrypointExporter(os.Args, exporter.NewExporter)
tests/logical/                  tests/testhelpers/
```

## Manifest contract

Exactly **two** connectors, each serving both protocols. This is load-bearing: `importer.init()` registers `neo4j` and `neo4j+offline` against the *same* `NewImporter`, and the SDK passes the requested protocol through for `NewImporter` to dispatch on — so one binary per direction is correct, and the vendored copy's four-executable form declared connectors that nothing built.

```yaml
name: neo4j
display_name: Neo4j
description: Integration providing backup and restore for Neo4j databases — Enterprise online backup (neo4j://) and Community offline dump (neo4j+offline://).
api_version: v1.1.0
homepage: https://github.com/opsmill/plakar-integration-neo4j
license: ISC
tier: third-party
contact: mailto:support@opsmill.com
connectors:
  - type: importer
    executable: neo4jImporter
    protocols: [neo4j, neo4j+offline]
    validator: ./importer/schema.json
    class: database
    subclass: neo4j
  - type: exporter
    executable: neo4jExporter
    protocols: [neo4j, neo4j+offline]
    validator: ./exporter/schema.json
    class: database
    subclass: neo4j
```

**Invariants**: `tier` MUST NOT be `official`; `homepage`/`contact` MUST be OpsMill's; every `executable` MUST have a matching `plugin/` entrypoint and vice versa.

## Protocol contract

| Scheme | Neo4j edition | Mechanism | Downtime |
|---|---|---|---|
| `neo4j://` | Enterprise | online backup / restore | none |
| `neo4j+offline://` | Community | offline dump / load (`--from-path`) | database must be stopped |

Both schemes MUST remain functional after extraction, in-process and packaged alike (FR-021).

## Consumption contract — two models, coexisting

**In-process (this repository).** Blank-importing the packages triggers `init()` registration, letting `importer.NewImporter` / `exporter.NewExporter` dispatch on the URI scheme:

```go
import (
    _ "github.com/opsmill/plakar-integration-neo4j/exporter"
    _ "github.com/opsmill/plakar-integration-neo4j/importer"
)
```

**Out-of-process (external Plakar users).** `make build` produces `neo4jImporter` and `neo4jExporter`; `plakar pkg create ./manifest.yaml v0.1.0` packages them; `plakar pkg add` installs. Reached by `plakar pkg add neo4j` once the hub recipe is merged.

The models do not conflict: registration happens in `init()` of library packages, while the plugin entrypoints are separate `main` packages.

**Dependency boundary**: `go-kloset-sdk` and `testcontainers-go` are used *only* by `plugin/` and `tests/`. A consumer importing only `importer`/`exporter` MUST NOT acquire them as build dependencies.

## Hub recipe

`community/v1.1.0/neo4j/recipe.yaml` in `PlakarKorp/hub`:

```yaml
name: neo4j
version: v0.1.0
repository: https://github.com/opsmill/plakar-integration-neo4j
```

**Submission gate**: the repository must be public, tagged `v0.1.0`, and its package build verified first (FR-020). Note that no existing hub recipe points outside the PlakarKorp organisation — see research R8 for the basis and the fallback.

## Acceptance gates

| Gate | Check |
|---|---|
| Builds | `go build ./...` and `go vet ./...` clean |
| Binaries | `make build` produces `neo4jImporter` and `neo4jExporter` |
| Manifest agreement | every declared `executable` ↔ a `plugin/` entrypoint (VR-8) |
| Registration | both `neo4j` and `neo4j+offline` register for importer and exporter |
| Tests | `make test` green (testcontainers; needs a quiet Docker host) |
| Packaging | `plakar pkg create ./manifest.yaml v0.1.0` yields the `.ptar` |
| Provenance | `tier: third-party`, OpsMill homepage/contact, dual-copyright ISC `LICENSE` |
| Consumer isolation | SDK/testcontainers absent from a consumer's `go mod graph` |
