# Tasks: Backup Retention Policy

**Input**: Design documents from `/specs/004-backup-retention-policy/`

**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/cli.md, quickstart.md

**Tests**: INCLUDED — Constitution Principle IV requires behavior changes to be accompanied by tests (table-driven unit tests for pure logic; e2e for flows).

**Organization**: Tasks are grouped by user story. US1 (scheduled `create` with retention) is the MVP; US2 (standalone `prune`) ships with it in the same release; US3 (Plakar) is deferred to its own slice by explicit plan decision (research.md R9) — only its v1 guard rails are built here (FR-012).

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel (different files, no dependencies)
- **[Story]**: Which user story this task belongs to (US1, US2)

## Phase 1: Setup

**Purpose**: Confirm a green baseline so regressions are attributable.

- [X] T001 Verify baseline: `make build`, `make test`, `make vet`, `make lint` all pass on the unmodified branch (record any pre-existing failures in the PR description rather than fixing them here)

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: The retention core — policy, parsing, selection, location adapters, orchestrator — shared by both shipped stories. All logic lands in `src/internal/app` (Constitution I).

**⚠️ CRITICAL**: No user story work can begin until this phase is complete.

- [X] T002 Add `Retention RetentionConfig{Days, Count int}` to `Configuration` in src/internal/app/app.go (0 = rule inactive; no default activation)
- [X] T003 Create src/internal/app/retention.go with `RetentionPolicy` (`Days`, `Count`), `Active()`, and `Validate()` (each ≥ 1 when set; error messages per contracts/cli.md; FR-001)
- [X] T004 Add `backupRef{Name, CreatedAt}`, `parseBackupName()` (anchored regex `^infrahub_backup_(\d{8}_\d{6})\.tar\.gz(\.enc)?$`, timestamp layout `20060102_150405`, non-matching/unparseable → not a backup), and deterministic ordering (CreatedAt desc, Name desc tiebreak) in src/internal/app/retention.go (FR-006, data-model.md)
- [X] T005 Implement `selectPrunable(refs, policy, now) (keep, prune)` in src/internal/app/retention.go — union keep-semantics, unconditional keep-newest floor **inside** the function (FR-002, FR-003, research.md R2)
- [X] T006 Implement `storageLocation` interface + `localLocation` adapter (`os.ReadDir` on BackupDir, filter via `parseBackupName`, delete via `os.Remove`) in src/internal/app/retention.go
- [X] T007 [P] Add `List(ctx)` (prefix-scoped, paginated, base-name filtered) and `Delete(ctx, key)` to `S3Client` in src/internal/app/s3.go, each bounded by a context timeout (research.md R4, critique E3)
- [X] T008 Implement `s3Location` adapter over the new `S3Client` methods in src/internal/app/retention.go (depends on T007)
- [X] T009 Implement `applyRetention(ctx, locations, policy, dryRun)` orchestrator in src/internal/app/retention.go — per-location evaluation, floor per location, best-effort within each leg (attempt every candidate, collect per-deletion errors), all legs attempted, `errors.Join` aggregation, per-deletion logrus logging, dry-run reports without deleting (FR-007, FR-008, FR-009, critique E1)
- [X] T010 Table-driven unit tests for T003–T005 in src/internal/app/retention_test.go: validation table; parse table incl. round-trip with `generateBackupFilename` (critique E2), `.enc` variant, decoys, unparseable timestamps, path-separator names never match (critique E5); ordering ties; selection union/floor/all-expired/empty/single-backup (SC-003 invariant)
- [X] T011 Unit tests for `applyRetention` with fake `storageLocation` implementations in src/internal/app/retention_test.go: best-effort within leg, all legs attempted after a leg error, dry-run set equals real-run set (SC-004), empty location no-op, error aggregation text
- [X] T012 [P] Unit tests for S3 key filtering and delete-key construction in src/internal/app/s3_test.go (prefixed keys, non-matching objects invisible, FR-006 at the S3 location)

**Checkpoint**: Retention core complete and fully unit-tested — user stories can now be wired.

---

## Phase 3: User Story 1 — Scheduled backup with automatic retention (Priority: P1) 🎯 MVP

**Goal**: `create` applies the policy automatically after a fully successful backup — local leg always, S3 leg exactly when this run uploaded — making scheduled backups self-bounding.

**Independent Test**: quickstart.md Scenario 5 (and 6 for S3): seed BackupDir with old-timestamp fixtures, run `create --retention-days 7 --retention-count 14` against a live deployment; out-of-policy fixtures gone, new archive + in-policy fixtures remain, exit 0; without flags, behavior unchanged.

### Implementation for User Story 1

- [X] T013 [US1] Register `--retention-days` / `--retention-count` on the create command with viper bindings (`retention-days`, `retention-count` → `INFRAHUB_RETENTION_*`) in src/cmd/infrahub-backup/main.go (FR-011, contracts/cli.md)
- [X] T014 [US1] Extract a testable gating helper (e.g. `retentionLegsForCreate(cfg, s3UploadedThisRun) []storageLocation`) and invoke `applyRetention` at the end of `CreateBackup` — strictly after archive write, checksum, and (when requested) successful S3 upload; on leg errors return `backup succeeded (<name>); retention failed: <joined>` in src/internal/app/backup.go (FR-004, FR-007, FR-008, research.md R7)
- [X] T015 [US1] FR-012 guard on the Plakar early-return path in src/internal/app/backup.go: when `Backend == plakar` and the policy is active, log an explicit "retention not yet supported for the plakar backend; skipping" warning, perform no pruning, do not fail the backup
- [X] T016 [US1] Unit tests for the gating helper and FR-012 warn-path in src/internal/app/retention_test.go: no active policy → no legs; policy active → local leg; policy + uploaded-this-run → local+S3 legs; plakar + policy → warn and skip
- [X] T017 [US1] End-to-end test in tests/e2e/: create-with-retention prunes seeded old archives after a successful backup, leaves decoys untouched, exits 0; create without flags prunes nothing (US1 acceptance scenarios 1, 3)

**Checkpoint**: US1 fully functional — scheduled backups are self-bounding. MVP shippable.

---

## Phase 4: User Story 2 — Manual prune with preview (Priority: P2)

**Goal**: Standalone `prune` command sharing the same policy evaluation, with `--dry-run`, interactive confirmation, `--force` bypass, and explicit `--s3` opt-in.

**Independent Test**: quickstart.md Scenarios 1–4: dry-run lists the exact candidate set and deletes nothing; interactive decline aborts cleanly; `--force` deletes exactly the previewed set; validation errors on missing rules / bad values / `--dry-run --force`.

### Implementation for User Story 2

- [ ] T018 [US2] Injectable confirmation seam in src/internal/app/retention.go — `confirmFunc func(candidates) (bool, error)` defaulting to a y/N stdin prompt; non-TTY stdin without `--force` → actionable refusal error (mirrors `updater.Proceed`; research.md R5, critique E4)
- [ ] T019 [US2] Implement `InfrahubOps.Prune(policy, opts{DryRun, Force, S3})` in src/internal/app/retention.go — validation order: policy active (else "at least one retention rule required"), `Backend != plakar` (else FR-012 error), `!(DryRun && Force)` (contradictory); legs: local always, S3 only when `opts.S3`; preview → confirm (unless Force/DryRun) → `applyRetention`
- [ ] T020 [US2] Register the `prune` command (`--retention-days`, `--retention-count`, `--dry-run`, `--force`, `--s3`; `--dry-run`/`--force`/`--s3` deliberately not viper-bound) in src/cmd/infrahub-backup/main.go (contracts/cli.md)
- [ ] T021 [US2] Unit tests for `Prune` in src/internal/app/retention_test.go: validation table (no rule, plakar backend, dry-run×force), confirm accept/decline/non-TTY paths via injected seam, dry-run never calls Delete, S3 leg only with opts.S3
- [ ] T022 [US2] End-to-end test in tests/e2e/: prune `--dry-run` (nothing deleted, exact list), prune `--force` (exact set deleted, decoys + newest survive — floor scenario with all-expired fixtures), validation-error exit codes (US2 acceptance scenarios 1, 3, 4; SC-003, SC-004, SC-005)

**Checkpoint**: US1 and US2 both independently functional — the v1 release surface is complete.

---

## Phase 5: User Story 3 — Plakar backend retention (Priority: P3) — DEFERRED

**No implementation tasks in this feature.** By explicit plan decision (plan.md, research.md R9) the Plakar slice ships separately, reusing `RetentionPolicy` and the `storageLocation` seam with a `plakarLocation` implementation. Its v1 guard rails — warn-and-skip on `create` (T015) and hard error on `prune` (T019) — are delivered above under FR-012. Recorded constraints for the future slice: incomplete groups never count toward the count rule; newest group overall and newest complete group are never removed; prior-version snapshots stay restorable.

---

## Phase 6: Polish & Cross-Cutting Concerns

- [ ] T023 [P] Documentation in docs/docs/backup/: retention how-to (flags, env vars, union semantics, keep-newest floor, per-location evaluation), S3 requirements with a least-privilege prefix-scoped policy example (list+delete), note that archives restored from S3 into BackupDir become retention candidates, Plakar not-yet-supported note, S3 lifecycle rules as a complement (critique P1); Vale + rumdl pass on changed docs
- [ ] T024 [P] Execute quickstart.md Scenarios 1–4 against the built binary and fix any contract deviations (Scenarios 5–6 covered by tests/e2e/)
- [ ] T025 Final gates: `make fmt`, `make vet`, `make lint`, `make test` all green; confirm no changes to go.mod/go.sum (otherwise run scripts/update-vendor-hash.sh per constitution)

---

## Dependencies & Execution Order

### Phase Dependencies

- **Phase 1 (Setup)**: none — start immediately
- **Phase 2 (Foundational)**: after T001 — BLOCKS both stories. Internal order: T002 → T003 → T004 → T005 → T006 → T009 sequential (same file retention.go); T007 [P] anytime, T008 after T007; tests T010 → T011 sequential (same file), T012 [P] after T007
- **Phase 3 (US1)**: after Phase 2. T013 [P-able vs T014/T015 (different files)]; T014 → T015 sequential (both backup.go); T016 after T014–T015; T017 after T013–T015
- **Phase 4 (US2)**: after Phase 2; independent of Phase 3 (different concerns; T020 and T013 both touch main.go — coordinate if parallel). T018 → T019 sequential (retention.go), T020 after T019, T021 after T019, T022 after T020
- **Phase 6 (Polish)**: after Phases 3 and 4

### Parallel Opportunities

- T007 + T012 (s3.go / s3_test.go) run parallel to the retention.go chain T003–T006
- Once Phase 2 completes, US1 (Phase 3) and US2 (Phase 4) can proceed in parallel — only shared file is src/cmd/infrahub-backup/main.go (T013 vs T020)
- T023 + T024 run in parallel within Polish

### Parallel Example: after Phase 2 checkpoint

```bash
# Developer A (US1):  T013 (main.go) → T014, T015 (backup.go) → T016, T017
# Developer B (US2):  T018, T019 (retention.go) → T020 (main.go, after A's T013 merges) → T021, T022
```

---

## Implementation Strategy

**MVP first**: Phases 1 → 2 → 3 deliver the P1 promise (self-bounding scheduled backups). Stop, run quickstart Scenario 5, ship if needed.

**Incremental**: Phase 4 adds the operator-facing prune/preview surface; Phase 6 documents and validates. US3 intentionally leaves this feature as a follow-up slice with its guard rails already in place.

**Task count**: 25 total — Setup 1, Foundational 11, US1 5, US2 5, US3 0 (deferred), Polish 3.
