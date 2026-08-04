# Implementation Plan: Restore the Latest Backup Without Naming an Archive

**Branch**: `fac/restore-latest-backup-xmj2m` | **Date**: 2026-08-04 | **Spec**: [spec.md](spec.md)

**Input**: Feature specification from `/specs/005-restore-latest-backup/spec.md`

## Summary

Add a `--latest` flag to `infrahub-backup restore` so the newest backup can be restored without naming an archive — the primitive the scheduled prod → staging sync (opsmill/infrahub-helm#80) needs. On the tarball backend, `--latest` lists the configured pool (local backup directory by default, the configured S3 bucket/prefix with `--s3`), ranks archives exactly as retention does (filename timestamp, name-descending tiebreak), fails fast on an encrypted newest archive without a key, logs the resolved archive, and delegates to the existing restore path. On the plakar backend, `--latest` is an explicit alias for the already-shipped no-argument behavior. All selection logic lives in `src/internal/app`; the binary entry point stays thin Cobra wiring.

## Technical Context

**Language/Version**: Go 1.25.0 (pinned in `go.mod`)

**Primary Dependencies**: cobra (CLI), viper (config), logrus (logging), minio-go v7 (S3) — all existing; **no new dependencies** (so no `vendorHash` update needed)

**Storage**: Local backup directory (`Configuration.BackupDir`) and S3 bucket/prefix via the existing `S3Client`; plakar repository untouched

**Testing**: `make test` (Go table-driven unit tests, existing fakes for `storageLocation` in `retention_test.go`); pytest e2e suite under `tests/e2e/` (docker/k8s × tarball/s3 variants)

**Target Platform**: linux/darwin/windows, amd64/arm64, `CGO_ENABLED=0` (constitution III)

**Project Type**: CLI — split-binary, shared-core (`src/cmd/infrahub-backup` + `src/internal/app`)

**Performance Goals**: Negligible — one `List()` of a directory or S3 prefix per invocation, then the existing restore path

**Constraints**: Fail-fast ordering (spec FR-007): encrypted-without-key must be rejected before the `--sleep` wait, any S3 download, and any container action; selection must be order-identical to retention's ranking (FR-005)

**Scale/Scope**: Pools of at most a few hundred archives (bounded by retention); single new flag pair on one command; ~1 new app file + CLI wiring + tests

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| Principle | Assessment | Status |
|---|---|---|
| I. Split-Binary, Shared-Core | Selection/orchestration logic lands in `src/internal/app` (new `restore_latest.go`); `src/cmd/infrahub-backup/main.go` gains only flag registration, argument validation, and delegation. No new binary. | ✅ PASS |
| II. Operational Safety & Data Integrity | `--latest` changes only *how the archive is chosen*, never how it is restored: metadata validation, checksum verification, version compatibility, and container stop/restart guarantees are the existing `RestoreBackup` path, unchanged. `--latest` is the explicit destructive-operation request (same contract as naming an archive today — restore has never prompted interactively); the resolved archive is logged before anything runs (FR-009), and every ambiguous state (flag+arg conflict, empty pool, encrypted newest without key) is a hard error, never a guess or fallback. | ✅ PASS |
| III. Self-Contained, Cross-Platform Binaries | No new runtime assets, no embedding changes, no CGO, no new external runtime expectations. | ✅ PASS |
| IV. Quality Gates: Test, Lint, Vet | Behavior change ships with table-driven unit tests (selection ordering reusing retention fixtures, fail-fast checks, CLI argument validation) plus e2e scenarios in the existing pytest suite; `make test`/`make vet`/`make lint` before merge. | ✅ PASS |
| V. Explicit Errors & Streaming Feedback | All new errors wrapped with `fmt.Errorf`/`%w` and returned to the Cobra handler; no `os.Exit`/direct printing in `src/internal/app`; the restore itself keeps its existing streaming output. New pre-restore log line uses logrus. | ✅ PASS |

**Post-Phase 1 re-check**: design introduces one new app file, two new flags, zero new entities, zero new dependencies — no violations. Complexity Tracking stays empty.

## Project Structure

### Documentation (this feature)

```text
specs/005-restore-latest-backup/
├── plan.md              # This file
├── research.md          # Phase 0 output
├── data-model.md        # Phase 1 output
├── quickstart.md        # Phase 1 output
├── contracts/
│   └── cli.md           # Phase 1 output — restore command surface contract
└── tasks.md             # Phase 2 output (/speckit-tasks — NOT created by /speckit-plan)
```

### Source Code (repository root)

```text
src/
├── cmd/infrahub-backup/
│   ├── main.go              # restoreCmd: add --latest / --s3 flags, Args validation,
│   │                        #   delegation to app.RestoreLatestBackup
│   └── main_test.go         # CLI validation tests (flag conflicts, bare-restore error text)
└── internal/app/
    ├── restore_latest.go        # NEW: resolveLatestBackup (pure selection) +
    │                            #   (iops) RestoreLatestBackup (orchestration)
    ├── restore_latest_test.go   # NEW: table-driven selection/fail-fast tests
    ├── backup.go                # RestoreBackup — unchanged consumer (delegation target)
    ├── retention.go             # reused: backupRef, parseBackupName,
    │                            #   sortBackupRefsNewestFirst, localLocation, s3Location
    └── s3.go                    # reused: S3Client.List, buildS3Key, config validation

tests/e2e/
├── test_docker_tarball.py   # extend: restore --latest (local pool)
└── test_docker_s3.py        # extend: restore --latest --s3 (bucket pool)
```

**Structure Decision**: Single-project layout, exactly as the repository stands. One new pair of files in the shared core (`src/internal/app/restore_latest.go` + test); wiring edits confined to `src/cmd/infrahub-backup/main.go` and its test; e2e additions extend the two existing suites that already exercise the tarball and S3 restore paths.

## Complexity Tracking

> No Constitution Check violations — table intentionally empty.
