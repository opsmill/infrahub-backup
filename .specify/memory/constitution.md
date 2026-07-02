<!--
Sync Impact Report
==================
Version change: 1.0.0 → 1.1.0
Rationale: MINOR bump. Principle I materially expanded: the architecture is
redefined from a fixed dual-binary set to a split-binary, shared-core pattern
with an enumerated binary set now including `infrahub-collect` (troubleshooting
bundle tool, spec 003-collect-tool / Jira INFP-415). The amendment mechanism
(new binaries require a constitution amendment) is retained unchanged.

Modified principles:
- I. Dual-Binary, Shared-Core Architecture → I. Split-Binary, Shared-Core
  Architecture (adds `src/cmd/infrahub-collect` to the enumerated entry points)
Added sections: none
Removed sections: none

Templates status:
- ✅ .specify/templates/plan-template.md — Constitution Check gate resolves
  against this file at plan time; no edits required.
- ✅ .specify/templates/spec-template.md — compatible as-is.
- ✅ .specify/templates/tasks-template.md — compatible as-is.
- ⚠ AGENTS.md / CLAUDE.md — still describe "two specialized CLI binaries";
  MUST be updated to the three-binary set when infrahub-collect is implemented
  (tracked in spec 003-collect-tool).

Follow-up TODOs:
- Update AGENTS.md project overview and architecture sections alongside the
  003-collect-tool implementation.
-->

# Infrahub Ops CLI Constitution

## Core Principles

### I. Split-Binary, Shared-Core Architecture

All application logic MUST live in the shared package `src/internal/app`. The
binary entry points (`src/cmd/infrahub-backup`, `src/cmd/infrahub-taskmanager`,
`src/cmd/infrahub-collect`) MUST remain thin Cobra wiring: command definitions,
flag registration, and delegation to the shared core. New capabilities MUST be
exposed through one of the binaries enumerated above; introducing an additional
binary requires a constitution amendment. Helper executables (e.g.,
`tools/neo4jwatchdog`, `tools/s3-uploader`) are permitted only when they are
embedded into, and distributed through, the main binaries.

*Rationale: the tools intentionally share environment detection, command
execution, and configuration; duplicating logic in entry points causes the
binaries to drift apart.*

### II. Operational Safety & Data Integrity (NON-NEGOTIABLE)

This toolset operates on production Infrahub data. Therefore:

- Backup operations MUST be non-destructive to the running instance beyond the
  documented stop/start of containers, and MUST restart what they stopped even
  on failure paths.
- Every backup artifact MUST carry metadata (versions, timestamps) and
  checksums; restore MUST validate both, and MUST refuse incompatible versions
  unless the user explicitly overrides.
- Destructive operations (restore over existing data, flushing flow runs,
  cancelling stale runs) MUST require explicit confirmation or an explicit
  bypass flag.
- Encryption support MUST never silently produce unencrypted output when
  encryption was requested.

*Rationale: a backup tool that can corrupt or silently lose data is worse than
no tool; recoverability is the product.*

### III. Self-Contained, Cross-Platform Binaries

Binaries MUST build with `CGO_ENABLED=0` and MUST cross-compile for
linux/darwin/windows on amd64 and arm64 (`make build-all`). All runtime assets
— Python scripts for Prefect operations, watchdog binaries, templates — MUST be
embedded via `go:embed`; a deployed binary MUST NOT depend on files shipped
alongside it. Features MAY depend on external runtimes already present in the
target environment (Docker Compose, kubectl contexts) but MUST detect their
absence and fail with an actionable error.

*Rationale: the tools are copied onto customer hosts and into containers where
no toolchain or package manager is available.*

### IV. Quality Gates: Test, Lint, Vet

`make test`, `make vet`, and `make lint` (golangci-lint v2; errcheck is
intentionally disabled) MUST pass before merge; CI enforces this and is the
source of truth. Behavior changes MUST be accompanied by tests — table-driven
unit tests for pure logic (metadata, checksums, encryption, pagination,
configuration merging) and CI-level end-to-end backup/restore jobs for flows
that touch containers. Code MUST be formatted with `go fmt` (`make fmt`).

*Rationale: regressions in a maintenance tool surface during incidents, the
worst possible time to discover them.*

### V. Explicit Errors & Streaming Feedback

Errors MUST be wrapped with `fmt.Errorf` (using `%w`) to add operation context
and returned up to the Cobra command handlers, which own user-facing display;
library code in `src/internal/app` MUST NOT call `os.Exit` or print errors
directly. Long-running operations (backups, restores, flushes) MUST stream
subprocess output in real time rather than buffering until completion. Logging
uses logrus with levels controlled by shared CLI flags.

*Rationale: operators run these commands interactively against slow,
large-data operations; silent hangs and contextless failures erode trust.*

## Operational Constraints & Tooling

- **Language/toolchain**: Go (version pinned in `go.mod`); dependencies managed
  with `make deps` / `make deps-update`.
- **Nix packaging**: whenever `go.mod` or `go.sum` changes, run
  `scripts/update-vendor-hash.sh` to refresh `vendorHash` in `flake.nix`; never
  hand-edit the hash.
- **Deployment contract**: Docker Compose service names (`database`,
  `task-manager-db`, `infrahub-server`, `task-worker`, `task-manager`,
  `task-manager-background-svc`, `cache`, `message-queue`) and their Kubernetes
  equivalents are a public contract; renaming or adding assumed services is a
  breaking change and MUST be called out in specs and release notes.
- **Backup engine**: Plakar (kloset) integration backs snapshot storage
  (filesystem or S3); changes to snapshot layout or metadata MUST preserve the
  ability to restore backups created by prior released versions, or document a
  migration path.
- **Documentation**: user-facing docs follow the Diataxis framework and the
  style rules in AGENTS.md; Vale and rumdl checks MUST pass for docs changes.

## Development Workflow

- Feature work follows the spec-kit flow: `/speckit-specify` →
  (`/speckit-clarify`) → `/speckit-plan` → `/speckit-tasks` →
  `/speckit-implement`, with artifacts under `specs/###-feature-name/`.
- Every plan MUST pass the Constitution Check gate in
  `.specify/templates/plan-template.md` against this document before Phase 0
  research, and again after Phase 1 design; violations require an entry in the
  plan's Complexity Tracking table.
- Work lands via pull requests against `main`; CI (lint, tests, backup
  end-to-end jobs) MUST be green before merge.
- Reviewers MUST check changes touching backup/restore paths against
  Principle II explicitly.

## Governance

This constitution supersedes ad-hoc practice for the areas it covers. It is
amended by pull request that (a) edits this file, (b) updates the Sync Impact
Report comment, and (c) bumps the version below according to semantic
versioning: MAJOR for removed or redefined principles, MINOR for new or
materially expanded principles/sections, PATCH for clarifications and wording.
`RATIFICATION_DATE` never changes; `LAST_AMENDED_DATE` is updated on every
amendment. Compliance is reviewed at two points: the plan-stage Constitution
Check and PR review. Runtime development guidance for agents lives in
AGENTS.md (routed via CLAUDE.md) and MUST stay consistent with this document.

**Version**: 1.1.0 | **Ratified**: 2026-07-02 | **Last Amended**: 2026-07-02
