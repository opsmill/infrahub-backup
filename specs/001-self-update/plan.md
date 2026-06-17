# Implementation Plan: Self-Update Command

**Branch**: `001-self-update` | **Date**: 2026-06-17 | **Spec**: [spec.md](./spec.md)
**Input**: Feature specification from `/specs/001-self-update/spec.md`

## Summary

Add an `update` command to both shipped binaries (`infrahub-backup`,
`infrahub-taskmanager`) that discovers the latest GitHub Release of
`opsmill/infrahub-backup`, downloads the artifact matching the running OS/arch,
verifies its SHA-256 checksum, and atomically replaces the running binary with
rollback on failure. A `--check` mode reports availability without writing, a
`--version` flag targets a specific release (pin/downgrade), and a `--yes` flag
skips the interactive confirmation. Installs managed by Homebrew or baked into a
container image are detected and refused with a pointer to the correct upgrade
path; unversioned dev builds are likewise refused.

Technical approach: discovery via the public GitHub Releases API using stdlib
`net/http` (with optional `GITHUB_TOKEN`/`GH_TOKEN` to lift the rate limit in
CI); semver comparison via `golang.org/x/mod/semver`; atomic apply-with-rollback
and checksum verification via `github.com/minio/selfupdate` (a natural fit given
`minio-go` is already a dependency). The release pipeline
(`.github/workflows/release.yml`) is extended to attach the cross-compiled
binaries plus a `SHA256SUMS` file as GitHub Release assets, which is the source
self-update reads from.

This mirrors the proven shape of Astral's `uv self update` (GitHub Releases as
the source, refusing self-update for package-manager installs) — see
[research.md](./research.md) — while deliberately using a single-binary atomic
swap rather than uv's "re-run the installer" model, since this tool ships one
self-contained binary and does no `PATH` management.

## Technical Context

**Language/Version**: Go 1.25.0
**Primary Dependencies**: cobra (CLI), logrus (logging), `github.com/minio/selfupdate` (NEW — atomic binary replace + checksum verify), `golang.org/x/mod/semver` (NEW — version comparison), stdlib `net/http`/`encoding/json` (GitHub Releases API)
**Storage**: N/A (operates on the on-disk executable; uses a temp file beside the target for atomic rename)
**Testing**: Go `testing`, table-driven tests alongside code in `src/internal/app/*_test.go`; HTTP interactions tested with `httptest.Server`
**Target Platform**: linux/amd64, linux/arm64, darwin/amd64, darwin/arm64, windows/amd64, windows/arm64 (the matrix `make build-all` already produces)
**Project Type**: single (Go module `infrahub-ops`, two binary entry points sharing `src/internal/app`)
**Performance Goals**: full update (discover → download → verify → replace) under 60s on broadband (SC-002); read-only check under 10s (SC-004)
**Constraints**: an interrupted update MUST always leave a runnable binary (SC-003, FR-005); no privilege escalation — detect and report permission failures (FR-011); no new heavy dependencies
**Scale/Scope**: small surface — one shared `updater` package, one `update` subcommand registered on each binary, plus a release-workflow change

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

The project constitution (`.specify/memory/constitution.md`) is an unpopulated
template with no ratified principles, so there are no formal gates to enforce.
The plan instead honors the established repo conventions documented in
`CLAUDE.md`:

- **CLI command-pattern with cobra** — `update` is added as a cobra subcommand on each binary, matching `version`, `environment`, etc. ✅
- **Shared internal logic** — update logic lives in a new shared package under `src/internal/app` (or a sibling `src/internal/updater`), used by both binaries. ✅
- **Explicit error wrapping with `fmt.Errorf`** and errors returned up to cobra handlers. ✅
- **Nix vendor hash discipline** — adding modules to `go.mod`/`go.sum` requires running `scripts/update-vendor-hash.sh`. ✅ (tracked as a task)
- **`make fmt` / `make lint` / `make test`** must pass (errcheck is disabled per `.golangci.yaml`). ✅

No violations. Complexity Tracking is empty.

## Project Structure

### Documentation (this feature)

```text
specs/001-self-update/
├── plan.md              # This file
├── research.md          # Phase 0 output
├── data-model.md        # Phase 1 output
├── quickstart.md        # Phase 1 output
├── contracts/           # Phase 1 output
│   ├── update-cli.md            # CLI command contract (flags, exit codes, output)
│   └── release-artifacts.md     # Release asset naming + checksum file contract
└── tasks.md             # Phase 2 output (/speckit.tasks — NOT created here)
```

### Source Code (repository root)

```text
src/
├── cmd/
│   ├── infrahub-backup/main.go          # register update subcommand (small change)
│   └── infrahub-taskmanager/main.go     # register update subcommand (small change)
└── internal/
    ├── app/
    │   ├── cli.go                       # AttachUpdateCommand(rootCmd, binaryName) helper
    │   └── utils.go                     # BuildRevision()/version already here
    └── updater/                         # NEW shared package
        ├── updater.go                   # orchestration: check → download → verify → apply
        ├── github.go                    # GitHub Releases API client (latest + by-tag)
        ├── platform.go                  # OS/arch → asset name; install-method detection
        ├── apply.go                     # wraps minio/selfupdate apply + rollback
        ├── updater_test.go
        ├── github_test.go
        └── platform_test.go

.github/workflows/release.yml            # add: generate SHA256SUMS + upload GH release assets
```

**Structure Decision**: Single Go module with two cobra entry points sharing
`src/internal`. New self-update logic is isolated in `src/internal/updater` so
both binaries register the same command via a thin `AttachUpdateCommand` helper
added to the existing `src/internal/app/cli.go`. This mirrors the existing
`ConfigureRootCommand` / `AttachEnvironmentCommands` pattern.

## Complexity Tracking

> No constitution violations. Section intentionally empty.
