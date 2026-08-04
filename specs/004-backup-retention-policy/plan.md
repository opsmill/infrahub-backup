# Implementation Plan: Backup Retention Policy

**Branch**: `fac/backup-retention-policy-fouxa` | **Date**: 2026-08-04 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `/specs/004-backup-retention-policy/spec.md`

## Summary

Add a retention policy to `infrahub-backup`: two optional rules (max age in days, max kept count) with union keep-semantics and an unconditional keep-newest floor, applied automatically by `create` after a fully successful backup and available as a standalone `prune` command with dry-run, confirmation prompt, and `--force` bypass. v1 (P1+P2) prunes tarball archives in the local `BackupDir` and, when active, backup objects under the configured S3 bucket/prefix — each location evaluated independently. The Plakar backend (P3) reuses the same policy semantics in a separate slice.

Technical approach: a pure, table-testable selection function over parsed backup references (`name → timestamp`), fed by two thin "storage location" adapters (local directory via `os.ReadDir`, S3 via the existing minio-based `S3Client` extended with list/delete). Orchestration lives in `src/internal/app/retention.go`; the binaries gain only Cobra wiring (retention flags on `create`, a new `prune` command).

## Technical Context

**Language/Version**: Go 1.25.0 (pinned in `go.mod`), `CGO_ENABLED=0`

**Primary Dependencies**: cobra (CLI), viper (config/env binding, prefix `INFRAHUB_`), logrus (logging), `minio-go/v7` (S3 — already a direct dependency); kloset (Plakar core) for the deferred P3 slice. **No new dependencies.**

**Storage**: local filesystem (`Configuration.BackupDir`, default `./infrahub_backups`); S3-compatible object storage via existing `S3Client` (`src/internal/app/s3.go`); Plakar repository (P3 only)

**Testing**: `go test` (`make test`) — table-driven unit tests co-located in `src/internal/app/*_test.go`; Python/pytest end-to-end suite under `tests/e2e/`; CI via `.github/workflows/ci.yml`

**Target Platform**: linux/darwin/windows on amd64/arm64 (`make build-all`); runs on operator hosts and inside containers (K8s CronJob)

**Project Type**: CLI toolset — split-binary, shared-core (`src/cmd/infrahub-backup` + `src/internal/app`)

**Performance Goals**: prune adds negligible wall-clock to a backup run — selection over 10,000 candidates completes in well under 1 second; S3 listing is paginated and bounded by object count under the prefix, not bucket size

**Constraints**: no `os.Exit`/direct printing in `src/internal/app` (errors wrapped with `%w`, returned to Cobra); real-time logging of each deletion; prune must never touch non-matching files/objects; new backup never rolled back on prune failure; S3 leg needs list+delete permissions (documented); retention S3 list/delete calls bounded by a context timeout (mirroring `S3Client.Upload`'s 30-minute bound, sized smaller for list/delete); interactive confirmation implemented behind an injectable seam (mirroring `updater.Proceed`) so decline/accept/non-TTY paths are unit-testable; Plakar backend + retention in v1 warns-and-skips on `create`, errors on `prune` (FR-012)

**Scale/Scope**: backup directories with O(10³) archives; S3 prefixes with O(10⁴) objects; two commands touched, one new file plus focused edits; docs page updates under `docs/docs/backup/`

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| Principle | Assessment | Status |
|---|---|---|
| I. Split-Binary, Shared-Core | All retention logic (policy, selection, location adapters, orchestration) in `src/internal/app/retention.go` + `s3.go`; `src/cmd/infrahub-backup/main.go` gains only flag registration and a thin `prune` Cobra command delegating to `InfrahubOps.Prune` | ✅ PASS |
| II. Operational Safety & Data Integrity | Retention flags are explicit consent for the unattended path; standalone `prune` prompts and requires `--force` to bypass (mirrors `update`'s `--yes` prompt convention); unconditional keep-newest floor with no override; selection restricted to the backup naming pattern; the just-created backup is never rolled back; backup failure ⇒ no pruning | ✅ PASS |
| III. Self-Contained, Cross-Platform | Pure Go, no new dependencies, no runtime assets, no embed changes; builds unaffected | ✅ PASS |
| IV. Quality Gates | Selection/validation/parsing are pure functions with table-driven tests; S3 leg covered by e2e (`tests/e2e/`) against the existing minio-based fixtures if present, else unit-tested via interface seam; `make test|vet|lint|fmt` all apply | ✅ PASS |
| V. Explicit Errors & Streaming Feedback | Each deletion logged via logrus as it happens; leg errors aggregated with `errors.Join` and wrapped as "backup succeeded; retention failed: %w"; no `os.Exit` in app package | ✅ PASS |
| Backup engine constraint (Plakar) | v1 does not touch snapshot layout or metadata; P3 design constraint recorded (deletion + space reclamation must keep prior-version snapshots restorable) | ✅ PASS (P3 gate re-checked in its own slice) |

**Post-Phase-1 re-check**: design artifacts introduce no new violations — the `storageLocation` seam is an in-package interface (not a new project/layer), contracts are CLI-surface documentation only. ✅ PASS

## Project Structure

### Documentation (this feature)

```text
specs/004-backup-retention-policy/
├── plan.md              # This file
├── research.md          # Phase 0 output
├── data-model.md        # Phase 1 output
├── quickstart.md        # Phase 1 output
├── contracts/
│   └── cli.md           # CLI contract: create retention flags + prune command
├── checklists/
│   └── requirements.md  # Spec quality checklist (from /speckit-specify)
└── tasks.md             # Phase 2 output (/speckit-tasks — NOT created by /speckit-plan)
```

### Source Code (repository root)

```text
src/
├── cmd/
│   └── infrahub-backup/
│       └── main.go            # EDIT: --retention-days/--retention-count on create;
│                              #       new `prune` command (--dry-run, --force, --s3)
└── internal/
    └── app/
        ├── retention.go       # NEW: RetentionPolicy, filename parsing, selection
        │                      #      function, location adapters, Prune orchestrator
        ├── retention_test.go  # NEW: table-driven tests (selection, parsing, floor,
        │                      #      validation, dry-run equality)
        ├── s3.go              # EDIT: add List (paginated, prefix-scoped) and Delete
        ├── s3_test.go         # EDIT/NEW: list/delete key-filtering tests
        ├── backup.go          # EDIT: invoke retention after successful create/upload
        └── app.go             # EDIT: RetentionConfig on Configuration

tests/
└── e2e/                       # EDIT: extend backup e2e to cover create-with-retention
                               #       and prune (local leg; S3 leg if minio fixture exists)

docs/
└── docs/
    └── backup/                # EDIT: retention how-to section + S3 permission note
```

**Structure Decision**: single-project layout as mandated by Constitution I — all logic in the shared `src/internal/app` package, thin Cobra wiring in `src/cmd/infrahub-backup`. No new packages, binaries, or directories beyond one new source file (+tests) in the existing app package.

## Complexity Tracking

No constitution violations — table intentionally empty.
