# Tasks: Restore the Latest Backup Without Naming an Archive

**Input**: Design documents from `/specs/005-restore-latest-backup/`

**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/cli.md, quickstart.md

**Tests**: Included — the spec's FR verifications name concrete tests and constitution IV requires behavior changes to ship with them.

**Organization**: Tasks are grouped by user story. The selection core, fail-fast gate, and CLI surface are shared by both stories, so they are Foundational; US1 adds the S3 leg (the P1 automation journey), US2 verifies the local journey end-to-end.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel (different files, no dependencies)
- **[Story]**: Which user story this task belongs to (US1, US2)
- Include exact file paths in descriptions

## Path Conventions

Single project: `src/`, `tests/` at repository root, per plan.md.

---

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: Baseline before any change — this feature adds no dependencies and no scaffolding.

- [X] T001 Record a green baseline on the branch: `make build && make test && make vet && make lint` all pass before any feature change (no `go.mod` change is expected at any point in this feature, so `scripts/update-vendor-hash.sh` must NOT be needed; treat a dirty `flake.nix` as a review error)

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: The selection core, orchestrator (local leg, fail-fast gate, audit log), and the full CLI surface. Both user stories exercise this machinery; no story work can begin before it exists.

**⚠️ CRITICAL**: No user story work can begin until this phase is complete.

- [X] T002 Implement `resolveLatestBackup(ctx context.Context, loc storageLocation) (backupRef, error)` in `src/internal/app/restore_latest.go`: `loc.List(ctx)` → `sortBackupRefsNewestFirst` → first ref; empty listing returns an error naming `loc.Name()` (FR-005, FR-008); listing errors wrapped with `%w` (constitution V)
- [X] T003 Add table-driven unit tests for `resolveLatestBackup` in `src/internal/app/restore_latest_test.go` using a fake `storageLocation` (pattern as in `retention_test.go`): newest-by-timestamp wins; timestamp tie broken by name descending; empty pool errors and names the location; `List` error propagates wrapped (FR-005, FR-008)
- [X] T004 Implement `(iops *InfrahubOps) RestoreLatestBackup(s3 bool, excludeTaskManager bool, restoreMigrateFormat bool, sleepDuration time.Duration, decryptKey string, force bool, resetDeploymentID bool) error` in `src/internal/app/restore_latest.go`: build the local pool `newLocalLocation(iops.config.BackupDir)` (structure the pool switch on the `s3` parameter now; its S3 arm is implemented in T008–T009 and may return a clear not-implemented error until then); resolve via `resolveLatestBackup`; fail fast when the resolved name has the `.enc` suffix and `decryptKey == ""` — before any sleep, download, or container action (FR-007); log `Restoring latest backup <name> from <location>` at info level before delegating (FR-009); local leg delegates to `iops.RestoreBackup(filepath.Join(iops.config.BackupDir, name), …)` passing all parameters through unchanged
- [X] T005 Add table-driven unit tests for the orchestrator's fail-fast matrix in `src/internal/app/restore_latest_test.go`: encrypted newest + no key → error naming the archive and `--decrypt-key`, and no delegation happened; encrypted newest + key → delegation proceeds; plain newest → delegation proceeds; assert the FR-009 log line content via a logrus test hook (FR-007, FR-009)
- [X] T006 Wire the CLI in `src/cmd/infrahub-backup/main.go`: register `--latest` and `--s3` bool flags on `restoreCmd` (no viper bindings — per-invocation switches, research D6); rework `Args`/`RunE` validation to enforce: `--latest` + positional arg → mutual-exclusion error (FR-002); bare `restore` on tarball → existing error extended to mention `--latest` (FR-003); `--s3` without `--latest` → error pointing at the positional `s3://` URI form; plakar + `--latest` → route to the existing no-argument path exactly as bare `restore` does (FR-004); plakar + `--latest --s3` → error (contracts/cli.md invocation matrix); tarball + `--latest` → delegate to `app.RestoreLatestBackup`
- [X] T007 Add CLI validation tests in `src/cmd/infrahub-backup/main_test.go` covering the full invocation matrix from `contracts/cli.md`: flag+arg conflict, `--s3` without `--latest`, bare tarball `restore` error text contains `--latest` (golden output, FR-003), plakar `--latest` accepted, plakar `--latest --s3` rejected (FR-002, FR-003, FR-004)

**Checkpoint**: `restore --latest` works against the local pool with all conflicts validated — US2's journey is functional; US1 (S3) still errors on the unimplemented arm.

---

## Phase 3: User Story 1 - Scheduled staging sync restores the newest prod backup unattended (Priority: P1) 🎯 MVP

**Goal**: `restore --latest --s3` selects the newest archive in the configured S3 bucket/prefix, downloads it via a collision-proof temp path, and restores it — the primitive opsmill/infrahub-helm#80 needs.

**Independent Test**: Seed an S3 bucket/prefix with several archives (plus a newer local-only one), run `restore --latest --s3`, verify the newest S3 archive is restored, the log names it and the `s3://` location, exit 0, and a same-named local archive survives untouched (quickstart Scenario 2).

### Implementation for User Story 1

- [X] T008 [US1] Implement the S3 pool arm in `RestoreLatestBackup` in `src/internal/app/restore_latest.go`: `iops.config.S3.ValidateConfig()` → `NewS3Client(iops.config.S3)` → `newS3Location(client)`, mirroring `retentionLegsForPrune` (FR-006); the same client instance is retained for the download in T009
- [X] T009 [US1] Implement the S3 download-and-delegate leg in `src/internal/app/restore_latest.go`: after the fail-fast gate, download the resolved object (`client.Download` with the key from `buildS3Key(name)`) to `os.CreateTemp(iops.config.BackupDir, "restore-latest-*.download")` — a name that must never match `backupNamePattern` — then delegate to `iops.RestoreBackup(<tempPath>, …)` and remove the temp file afterwards (deferred, also on failure); never write to `BackupDir/<name>` (critique E1/X1; contracts/cli.md "Local-copy safety")
- [X] T010 [P] [US1] Add unit tests for the S3 leg in `src/internal/app/restore_latest_test.go`: the temp download path never matches `backupNamePattern` (critique E3); a same-named file in `BackupDir` is byte-identical after the leg runs against a fake client; download failure propagates wrapped and leaves no temp file behind (FR-006, FR-007)
- [X] T011 [US1] Extend `tests/e2e/test_docker_s3.py`: scenario per quickstart Scenario 2 — `create --s3-upload` (keeping the local copy), then a newer local-only `create`, then `restore --latest --s3`; assert the S3 archive was selected (FR-006), the FR-009 log line names the archive and the `s3://` location, exit 0, and the same-named local archive still exists unmodified

**Checkpoint**: US1 fully functional and independently testable — MVP complete.

---

## Phase 4: User Story 2 - Operator restores the newest local backup interactively (Priority: P2)

**Goal**: `restore --latest` restores the newest archive from the configured local backup directory with zero lookup steps.

**Independent Test**: Create two backups, run `restore --latest`, verify the newer one is restored with the log line naming it (quickstart Scenario 1); empty-directory and conflict invocations fail cleanly (Scenarios 3–4).

### Implementation for User Story 2

- [X] T012 [US2] Extend `tests/e2e/test_docker_tarball.py`: create two backups, run `restore --latest`, assert the newer archive is restored, the FR-009 log line names it and `local:<dir>`, and exit 0 (FR-001, FR-005, FR-009); add the empty-pool case with a fresh `BACKUP_DIR` asserting a clear error and non-zero exit (FR-008)
- [X] T013 [P] [US2] Extend the encrypted-backup coverage: e2e or integration-level check per quickstart Scenario 5 — with the newest archive encrypted and no `--decrypt-key`, `restore --latest --sleep 5m` exits non-zero immediately (no sleep occurred, proving the fail-fast ordering) and no container was touched; with `--decrypt-key` the same archive restores (FR-007) — place alongside the existing encryption e2e coverage in `tests/e2e/`

**Checkpoint**: Both user stories independently functional and covered end-to-end.

---

## Phase 5: Polish & Cross-Cutting Concerns

**Purpose**: Discoverability, backend parity, and final gates.

- [X] T014 [P] Document `restore --latest` and `--s3` in `README.md`: flag reference on the restore section, the selection rule (newest by filename timestamp, same ordering as retention), the fail-fast behaviors, and a short scheduled-restore example for the nightly-sync use case (critique P1)
- [X] T015 [P] Extend `tests/e2e/test_docker_plakar.py` with a parity smoke test: `restore --latest` and bare `restore` resolve and restore the same latest complete backup group (FR-004)
- [X] T016 Open a follow-up GitHub issue on opsmill/infrahub-backup documenting the pre-existing positional `s3://` URI restore footgun (download to `BackupDir/<basename>` truncates a same-named local archive and `defer os.Remove` deletes it afterwards) — out of scope for this feature per research D3, must not be silently dropped (critique E1 follow-up). **Drafted, not filed**: three ready-to-file drafts live in `specs/005-restore-latest-backup/followups/` (the `s3://` footgun, the Neo4j stale-leftover-dump defect, the dead `restore` viper bindings); filing them on GitHub is an outward-facing action pending user approval
- [X] T017 Run the full gates and validation: `make fmt`, `make test`, `make vet`, `make lint` all green (constitution IV); walk `specs/005-restore-latest-backup/quickstart.md` Scenarios 1–6 against a live docker deployment and record outcomes; confirm every SC-002 failure path has a test asserting non-zero exit with the deployment untouched — results in `specs/005-restore-latest-backup/quickstart-results.md` (Scenarios 1–6 all pass; Scenario 1 carries a caveat traced to follow-up issue 2, a pre-existing Neo4j restore defect)

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (Phase 1)**: No dependencies — run first.
- **Foundational (Phase 2)**: Depends on T001. Blocks both user stories. Internal order: T002 → T003; T002 → T004 → T005; T004 → T006 → T007 (T003/T005 can run parallel to T006 once their prerequisites exist).
- **User Story 1 (Phase 3)**: Depends on Phase 2. Internal order: T008 → T009 → {T010, T011}.
- **User Story 2 (Phase 4)**: Depends on Phase 2 only — independent of US1; T012 and T013 are parallel.
- **Polish (Phase 5)**: T014/T015/T016 depend only on Phase 2 (can start once the CLI surface is final); T017 depends on everything else.

### User Story Dependencies

- **US1 (P1)**: Foundational only. No dependency on US2.
- **US2 (P2)**: Foundational only. No dependency on US1 — its journey is already functional at the Phase 2 checkpoint; this phase is its end-to-end verification.

### Parallel Opportunities

```text
Phase 2:  T003 ∥ T006   (after T002/T004)      different files
          T005 ∥ T007   (after T004/T006)      different files
Phase 3:  T010 ∥ T011   (after T009)           unit vs e2e files
Phase 4:  T012 ∥ T013                          different e2e files
Phase 5:  T014 ∥ T015 ∥ T016                   docs vs e2e vs GitHub
US1 ∥ US2: Phases 3 and 4 are fully independent after Phase 2
```

---

## Implementation Strategy

### MVP First

1. Phase 1 (baseline) → Phase 2 (Foundational — the bulk of the feature).
2. Phase 3 (US1, S3 leg) → **MVP**: the helm#80 CronJob can be built against it.
3. Note: the Phase 2 checkpoint already makes US2's journey work locally; Phase 4 is deliberately thin (verification, not construction).

### Incremental Delivery

Each checkpoint is shippable: Phase 2 alone delivers local `--latest` (US2 value); Phase 3 adds the automation primitive (US1 value); Phase 5 hardens discoverability and parity. Constitution IV gates (`make test`/`vet`/`lint`) must be green at every commit, not only at T017.

---

## Notes

- No `go.mod`/`go.sum` change is expected — if one appears, run `scripts/update-vendor-hash.sh` and treat it as a design deviation worth flagging.
- All new errors: wrap with `fmt.Errorf`/`%w`, return to Cobra handlers; no `os.Exit`/printing in `src/internal/app` (constitution V).
- The temp download name (`restore-latest-*.download`) is a contract, not an implementation detail: T010 pins it against `backupNamePattern`.
- Commit after each task or logical group.
