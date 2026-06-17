---
description: "Task list for Self-Update Command"
---

# Tasks: Self-Update Command

**Input**: Design documents from `/specs/001-self-update/`
**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/

**Tests**: Included. The repo follows a `src/internal/app/*_test.go` convention and
SC-003 (interrupted/failed updates must always leave a runnable binary) requires
verification, so each story carries targeted unit tests using `httptest.Server`
and a temp-dir fake binary — no network or real release needed in CI.

**Organization**: Tasks are grouped by user story (US1=P1, US2=P2, US3=P3) so each
can be implemented and tested independently.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel (different files, no dependencies)
- **[Story]**: US1 / US2 / US3
- All paths are relative to repo root `/Users/alex/dev/opsmill/infrahub-backup`

## Path Conventions

Single Go module `infrahub-ops`. New shared logic lives in `src/internal/updater/`;
command wiring lives in `src/internal/app/cli.go` and the two `src/cmd/*/main.go`
entry points.

---

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: Add dependencies and create the package scaffold.

- [X] T001 Add `github.com/minio/selfupdate` and `golang.org/x/mod` to `go.mod` and run `go mod tidy` to populate `go.sum`
- [X] T002 Run `scripts/update-vendor-hash.sh` to refresh `vendorHash` in `flake.nix` (required after go.mod/go.sum change per CLAUDE.md)
- [X] T003 [P] Create the `src/internal/updater/` package directory with a `doc.go` package comment describing the discover → verify → apply pipeline

**Checkpoint**: Dependencies resolved, package exists, `make build` still passes.

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: Shared building blocks that BOTH US1 (update) and US2 (check) depend on — release discovery, platform mapping, version comparison, install-method detection, and the command surface.

**⚠️ CRITICAL**: No user story work can begin until this phase is complete.

- [X] T004 Define core types (`Release`, `Asset`, `PlatformTarget`, `InstalledBinary`, `Checksums`, `UpdateResult`, `InstallMethod` enum) in `src/internal/updater/types.go` per data-model.md
- [X] T005 [P] Implement the GitHub Releases API client (`LatestRelease()` via `GET /repos/opsmill/infrahub-backup/releases/latest` and `ReleaseByTag(tag)` via `/releases/tags/{tag}`, stdlib `net/http`, JSON decode, prerelease/draft filtering) in `src/internal/updater/github.go`; send a `Bearer` auth header when `GITHUB_TOKEN`/`GH_TOKEN` is set (optional, for rate-limit/CI), and detect+report HTTP 403 rate-limit responses clearly (per research.md "Token support")
- [X] T006 [P] Implement platform asset-name mapping (`PlatformTarget` from `runtime.GOOS`/`runtime.GOARCH` + invoked binary base name → `<binary>-<os>-<arch>[.exe]`; select matching `Asset` from a `Release`) in `src/internal/updater/platform.go`
- [X] T007 [P] Implement version comparison helpers (`IsValid`, `Compare`, "is target newer than installed", downgrade detection) wrapping `golang.org/x/mod/semver` in `src/internal/updater/version.go`
- [X] T008 [P] Implement install-method + writability detection (`detectInstallMethod` → dev/homebrew/container/direct via empty-version, Homebrew prefix/`/Cellar/`, `/.dockerenv`+`/proc/1/cgroup`; `os.Executable`+`EvalSymlinks`; writability of binary and its dir) in `src/internal/updater/install.go`
- [X] T009 Add `AttachUpdateCommand(rootCmd *cobra.Command, binaryName string)` to `src/internal/app/cli.go` defining the `update` cobra command with `--check`, `--version`, and `--yes`/`-y` flags, dispatching to `updater` package functions (behavior filled by stories); mark `--check`/`--yes` mutually exclusive
- [X] T010 Register the update command by calling `app.AttachUpdateCommand(rootCmd, "infrahub-backup")` in `src/cmd/infrahub-backup/main.go` and `app.AttachUpdateCommand(rootCmd, "infrahub-taskmanager")` in `src/cmd/infrahub-taskmanager/main.go`
- [X] T011 [P] Unit tests for foundational pieces: `github_test.go` (httptest server returns latest/by-tag JSON, asserts `Bearer` header sent when token env set, handles 403 rate-limit), `platform_test.go` (asset-name matrix incl. windows `.exe`), `version_test.go` (multi-digit ordering, downgrade), `install_test.go` (homebrew path, `/.dockerenv`, empty version, read-only dir) under `src/internal/updater/`

**Checkpoint**: `update --help` works on both binaries; discovery, mapping, version compare, and install detection are tested and green.

---

## Phase 3: User Story 1 - Update to the latest version (Priority: P1) 🎯 MVP

**Goal**: One command downloads the latest release for the running platform, verifies its checksum, and atomically replaces the binary with rollback on failure.

**Independent Test**: Build at an older `VERSION`, run `update --yes` against a newer release, confirm `version` reports the new tag and the binary still runs; corrupt the asset and confirm the original binary survives.

### Tests for User Story 1

- [X] T012 [P] [US1] Test SHA256SUMS parsing (valid lines, lookup by asset name, missing entry) in `src/internal/updater/checksums_test.go`
- [X] T013 [P] [US1] Test apply + rollback: applying a good payload replaces a temp fake binary; a checksum mismatch aborts and leaves the original byte-identical, in `src/internal/updater/apply_test.go`
- [X] T014 [P] [US1] Integration test of the update orchestration happy path and "already up to date" no-op using `httptest.Server` (release JSON + asset + SHA256SUMS) and a temp binary, in `src/internal/updater/updater_test.go`

### Implementation for User Story 1

- [X] T015 [US1] Implement `SHA256SUMS` download + parse into `Checksums` (filename → hex digest) in `src/internal/updater/checksums.go`
- [X] T016 [US1] Implement the verifying apply wrapper around `github.com/minio/selfupdate` (download asset to a reader, pass expected SHA-256 as `selfupdate.Options.Checksum`, apply to `os.Executable()` with rollback) in `src/internal/updater/apply.go`
- [X] T017 [US1] Implement `Update(opts)` orchestration in `src/internal/updater/updater.go`: build `InstalledBinary`, run eligibility state machine (dev/homebrew/container/permission → refuse; installed ≥ target → no-op), resolve latest `Release`, select platform asset, fetch+parse checksums, call apply, return `UpdateResult{Action: updated|already-current|refused, From/To, DetailsURL}`
- [X] T018 [US1] Wire the default (non-`--check`) path in `AttachUpdateCommand` (`src/internal/app/cli.go`) to call `updater.Update` and print "updated `<from>` → `<to>`" / "already up to date" / refusal reason; map refusals & operational failures to exit code 1, success/no-op to 0 (per contracts/update-cli.md). For US1 the default proceeds without prompting (confirmation added in US3)
- [X] T019 [US1] Add `fmt.Errorf`-wrapped, actionable error messages for network-unreachable, no-matching-asset, missing-checksum-entry, and write-permission failures along the `Update` path

**Checkpoint**: `infrahub-backup update --yes` performs a real end-to-end upgrade; failed/corrupt updates leave a working binary. MVP complete.

---

## Phase 4: User Story 2 - Check for updates without installing (Priority: P2)

**Goal**: A read-only `--check` reports current version, latest version, and whether an update is available, with zero disk changes.

**Independent Test**: Run `update --check` on an older build; it reports availability + details URL and the binary is byte-identical afterward.

### Tests for User Story 2

- [X] T020 [P] [US2] Test `Check` returns `available` / `already-current` correctly and performs no writes (assert temp binary unchanged) using `httptest.Server`, in `src/internal/updater/updater_test.go`

### Implementation for User Story 2

- [X] T021 [US2] Implement `Check(opts)` in `src/internal/updater/updater.go`: resolve latest `Release`, compare versions, return `UpdateResult{Action: available|already-current, From/To, DetailsURL}` without downloading assets or touching disk
- [X] T022 [US2] Wire the `--check` path in `AttachUpdateCommand` (`src/internal/app/cli.go`) to call `updater.Check` and print "update available: `<cur>` → `<target>` (`<url>`)" or "already up to date (`<cur>`)"; exit 0 on completion, non-zero only on source-unreachable errors (per contracts/update-cli.md)
- [X] T023 [US2] Ensure `--check` still reports dev/homebrew/container states informatively but makes no disk changes (reuse install detection from T008)

**Checkpoint**: US1 and US2 both work independently; `--check` is provably read-only.

---

## Phase 5: User Story 3 - Confirm or skip confirmation, and target a version (Priority: P3)

**Goal**: Interactive runs confirm before replacing; `--yes` skips; non-interactive without `--yes` refuses cleanly; `--version` targets a specific release (pin/downgrade).

**Independent Test**: Interactive run prompts `[y/N]`; `--yes` skips the prompt; piped stdin without `--yes` refuses with guidance; `update --version v1.7.2 --yes` installs that exact tag.

### Tests for User Story 3

- [X] T024 [P] [US3] Test confirmation gating: `--yes` proceeds, declining the prompt aborts with no write, and non-interactive (non-TTY) without `--yes` refuses; plus `--version` resolves a specific tag and permits downgrade, in `src/internal/updater/updater_test.go`

### Implementation for User Story 3

- [X] T025 [US3] Implement TTY detection and a `[y/N]` confirmation prompt (showing `current → target`) in `src/internal/updater/prompt.go` (TTY check via `golang.org/x/term` or stat of stdin)
- [X] T026 [US3] Update the default path in `AttachUpdateCommand` (`src/internal/app/cli.go`) to prompt before applying when interactive and `--yes` not set; skip when `--yes`; when non-interactive and no `--yes`, refuse with "re-run with --yes to update non-interactively" (exit 1)
- [X] T027 [US3] Implement `--version` targeting in `updater.Update`/`Check`: when set, resolve via `ReleaseByTag` (allowing prereleases and downgrades), validate the tag is valid semver, and surface a clear error for an unknown tag

**Checkpoint**: All three stories are independently functional; default is safe-by-default, automation has a clean path, and pinning/downgrade works.

---

## Phase 6: Polish & Cross-Cutting Concerns

**Purpose**: Make the feature work end-to-end in production and meet project quality bars.

- [X] T028 **(required for production)** Extend `.github/workflows/release.yml`: after `make build-all`, generate `SHA256SUMS` over `bin/*` and attach all platform binaries + `SHA256SUMS` to the GitHub Release via `gh release upload "${GITHUB_REF_NAME}" bin/* SHA256SUMS --clobber` (add `permissions: contents: write` and `GITHUB_TOKEN`); leave S3, Docker, and Homebrew jobs unchanged (per contracts/release-artifacts.md)
- [X] T029 [P] Document the `update` command in the Infrahub Ops docs (and README install section) covering `--check`, `--yes`, `--version`, the optional `GITHUB_TOKEN`/`GH_TOKEN` for CI/rate limits, the Windows-Defender quarantine caveat for freshly-downloaded binaries, and the Homebrew/container/dev refusal paths, following the Diataxis how-to structure
- [X] T030 [P] Add a context-aware HTTP client (timeouts; respect a `--timeout`-free sane default) to the GitHub calls in `src/internal/updater/github.go` so a slow/unreachable source fails fast rather than hanging (supports SC-002/SC-004)
- [X] T031 Run `make fmt`, `make vet`, `make lint`, and `make test`; fix any findings (errcheck is disabled per `.golangci.yaml`)
- [X] T032 Execute the quickstart.md developer validation checklist end-to-end against a test release and confirm acceptance scenarios for US1/US2/US3 and all refusal cases

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (Phase 1)**: No dependencies — start immediately.
- **Foundational (Phase 2)**: Depends on Setup — BLOCKS all user stories.
- **User Stories (Phase 3–5)**: All depend on Foundational. US1, US2, US3 are independent of each other and can proceed in parallel once Foundational is done (US3 refines the US1 command path, so if both are staffed, coordinate on `cli.go`).
- **Polish (Phase 6)**: Depends on the desired stories being complete. T028 (release pipeline) is independent of the Go code and can be done any time after the asset-naming contract is agreed.

### User Story Dependencies

- **US1 (P1)**: After Foundational. No dependency on US2/US3.
- **US2 (P2)**: After Foundational. Independent; shares discovery/version helpers (already foundational).
- **US3 (P3)**: After Foundational. Refines the US1/US2 command wiring in `cli.go` — independently testable, but touches the same file as T018/T022 (sequence those if worked concurrently).

### Within Each User Story

- Tests are written first and should FAIL before implementation.
- types/helpers → checksums/apply → orchestration → command wiring → error messages.

### Parallel Opportunities

- Setup: T003 in parallel with dependency work once T001/T002 land.
- Foundational: T005, T006, T007, T008 are different files — fully parallel; T011 tests parallel with each other.
- US1 tests T012/T013/T014 parallel; US1 impl T015/T016 parallel (different files) before T017.
- Polish: T029, T030 parallel.
- Cross-team: US1, US2, US3 can each be owned by a different developer after Foundational.

---

## Parallel Example: Foundational Phase

```bash
# After T004 (types) lands, launch the independent building blocks together:
Task: "Implement GitHub Releases API client in src/internal/updater/github.go"      # T005
Task: "Implement platform asset-name mapping in src/internal/updater/platform.go"   # T006
Task: "Implement semver comparison helpers in src/internal/updater/version.go"      # T007
Task: "Implement install-method + writability detection in src/internal/updater/install.go"  # T008
```

## Parallel Example: User Story 1 Tests

```bash
Task: "Test SHA256SUMS parsing in src/internal/updater/checksums_test.go"           # T012
Task: "Test apply + rollback in src/internal/updater/apply_test.go"                 # T013
Task: "Integration test update orchestration in src/internal/updater/updater_test.go" # T014
```

---

## Implementation Strategy

### MVP First (User Story 1 Only)

1. Phase 1: Setup (T001–T003).
2. Phase 2: Foundational (T004–T011) — CRITICAL, blocks all stories.
3. Phase 3: User Story 1 (T012–T019).
4. Phase 6: T028 (release pipeline) so a real release carries assets+checksums.
5. **STOP and VALIDATE**: `update --yes` upgrades end-to-end; corrupt-asset run leaves a working binary.

### Incremental Delivery

1. Setup + Foundational → foundation ready.
2. US1 → test independently → MVP (`update --yes`).
3. US2 → adds read-only `--check`.
4. US3 → adds safe-by-default confirmation, non-interactive guard, and `--version` pin/downgrade.
5. Polish → docs, lint/test, quickstart validation.

### Parallel Team Strategy

After Foundational: Dev A → US1, Dev B → US2, Dev C → US3 (coordinating on `cli.go`). T028 (CI) and T029 (docs) can run alongside.

---

## Notes

- [P] = different files, no dependencies on incomplete tasks.
- T028 is infra-only but is required for the feature to function against real releases — don't ship US1 to users without it.
- Commit after each task or logical group; run `make test` before each checkpoint.
- After any go.mod change beyond T001, re-run `scripts/update-vendor-hash.sh` (T002 pattern).
- **Prior art**: the design mirrors uv/axoupdater (GitHub Releases source, refuse managed installs) but deliberately uses a single-binary atomic swap instead of uv's "re-run the installer" model — see research.md. **Future direction**: if a first-party `curl | sh` installer is ever added, replace the heuristic install-method detection (T008) with an install-receipt file, which is strictly more reliable.
