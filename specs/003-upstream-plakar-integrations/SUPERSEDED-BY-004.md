# What in 003 is superseded by 004 (workstream B)

**Date**: 2026-08-07 · **Supersedes**: parts of this feature's plan for the Neo4j integration · **Authority**: `specs/004-plakar-encryption/spec.md` + `specs/004-plakar-encryption/contracts/neo4j-integration-repo.md`

The 003 documents are kept as the record of what was designed and built. They are **not** rewritten. This file names the specific claims that no longer hold, so a reader of those documents is not misled.

## What changed, and why

003 planned to author the Neo4j integration inside a fork worktree of `PlakarKorp/integrations` (orphan branch `integration/neo4j`, subdirectory `neo4j/`, module `github.com/PlakarKorp/integration-neo4j`), consume it here via `go.mod replace`, and contribute it upstream by opening a pull request against their monorepo.

Plakar have since advised that this is unnecessary. Their integrations monorepo is the home for integrations **they** maintain; community integrations stay wherever their maintainer prefers, and become installable by adding a recipe to `PlakarKorp/hub` under `community/<kloset-compat-version>/<name>/recipe.yaml`. Their words: the integration can live on any git host — GitHub is not even required.

## Superseded claims

| Where | Claim in 003 | Now |
|---|---|---|
| `tasks.md` T040 | "Open the upstream PR for `integration-neo4j` against base branch `integration/neo4j` in `PlakarKorp/integrations`" | **Will not be done.** Replaced by authoring a hub recipe pointing at the OpsMill repository. |
| `tasks.md` L17, T005, L82 | Integration lives in a fork worktree of `PlakarKorp/integrations`, subdir `neo4j/`; consumed via fork build | Lives in its own repository `opsmill/plakar-integration-neo4j`, module at the repository **root**, consumed as a tagged external dependency |
| `plan.md` L13, L17, L85-86, L98 | Module `github.com/PlakarKorp/integration-neo4j`, "developed in a fork worktree", "contributed upstream" | Module `github.com/opsmill/plakar-integration-neo4j`, OpsMill-owned, distributed through the hub |
| `research.md` L131 | Consume from the fork via `replace` "until upstream merges/publishes the standalone mirror"; open the upstream PR in parallel | There is no upstream merge to wait for; the OpsMill repository *is* the home. No `replace` in any merged state. |
| `quickstart.md` contributor section | Clone your fork of `PlakarKorp/integrations`, work in `neo4j/` | Clone `opsmill/plakar-integration-neo4j` directly; see 004's quickstart for the extraction procedure |
| `contracts/neo4j-integration.md` | Module `github.com/PlakarKorp/integration-neo4j`, mirrors the monorepo's `mysql/` layout | Superseded wholesale by `specs/004-plakar-encryption/contracts/neo4j-integration-repo.md` |
| `deliverable-b-plan.md` L71 | "Remaining A: … upstream PR" | Remaining work is tracked as 004 workstream B; the upstream PR is not part of it |

## What still holds

- The integration's **technical** design — the `importer`/`exporter`/`manifest`/`neo4jconn` layout, both URI schemes (`neo4j://` Enterprise online, `neo4j+offline://` Community offline), the SDK plugin entrypoints, and the in-process registration model — is unchanged. 004 relocates it; it does not redesign it.
- The reusability rationale for making the integration generic (no Infrahub-specific assumptions) is unchanged, and is in fact what makes the hub recipe worth submitting.
- Everything in 003 concerning the co-located runner, the Postgres integration, and the tool-side rework is untouched by 004 workstream B.

## Two defects 004 found in the 003 output

Recorded here because they are findings about 003's deliverable, not about 004:

1. **`contrib/integration-neo4j/manifest.yaml` declares four connector executables that nothing builds** — `neo4jImporter`, `neo4jOfflineImporter`, `neo4jExporter`, `neo4jOfflineExporter`. The vendored tree has no `plugin/` directory at all. In-process registration happens in `init()`, so the manifest is dead metadata here and the defect could only surface on first packaging. The fork's two-connector form is correct.
2. **`contrib/` is a reduced copy of the fork subtree** — the Go sources are byte-identical, but `contrib/` lacks the plugin entrypoints, the testcontainers suite, `LICENSE`, a populated `Makefile`, and CI. 003's checkpoint "consumable via fork build" was true; "builds and its tests pass standalone" was only ever true of the fork, never of `contrib/`.
