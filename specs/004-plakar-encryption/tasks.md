# Tasks: Encrypt the Plakar backend at rest

**Feature**: `004-plakar-encryption` | **Branch**: `004-plakar-encryption`
**Input**: plan.md, spec.md, research.md, data-model.md, contracts/encryption-and-keys.md, quickstart.md

**Tech**: Go 1.25.0; `github.com/PlakarKorp/kloset` v1.1.0 (`encryption`, `connectors/storage`, `repository`). No new module dependencies → no vendor-hash change. In-place extension of the 003 plakar backend + co-located runner.

**Tests**: requested — the spec mandates an encrypted round-trip plus negative/no-leak checks (SC-001…SC-006). Test tasks are included per story.

## Conventions

- `[P]` = parallelizable (distinct file, no dependency on an incomplete task in the same phase).
- `[USn]` maps to the spec's user stories. Setup/Foundational/Polish carry no story label.
- All paths are repo-relative under `src/`.

---

## Phase 1: Setup

- [X] T001 Add encryption error sentinels and a redaction helper in `src/internal/app/plakar.go` (`errEncryptedRepoNeedsPassphrase`, `errWrongPassphrase`, `errEncryptWithoutPassphrase`, `errEncryptKeyOnPlakar`) — clear, actionable text; never embed the passphrase (FR-005, FR-007).

---

## Phase 2: Foundational (blocking — required by US1, US2, US3)

**Goal**: the passphrase pipeline + encrypted repo create/open + secure runner injection that every story opens an encrypted repo through. No story can pass until these land.

- [X] T002 Add `Passphrase string` and `Encrypt bool` to `PlakarConfig` (and thread into `Configuration`) in `src/internal/app/app.go`; default empty/false so the no-encrypt path is unchanged (FR-008, FR-009).
- [X] T003 [P] Add `resolvePassphrase()` in `src/internal/app/utils.go`: resolution order `--passphrase-file` (first line, trimmed) → `INFRAHUB_BACKUP_PASSPHRASE`; return empty when neither set; never log the value (FR-011, contract "Resolution order").
- [X] T004 [P] Add `validatePassphrase()` in `src/internal/app/utils.go`: reject `< 12` chars with a clear message, used only on `create --encrypt` before any repo work (FR-013, VR-6).
- [X] T005 Implement encrypted **create** in `openOrCreateRepo` (`src/internal/app/plakar.go`): when `Encrypt` is set, `enc := encryption.NewDefaultConfiguration()`; `secret, _ := encryption.DeriveKey(enc.KDFParams, []byte(passphrase))`; `enc.Canary, _ = encryption.DeriveCanary(enc, secret)`; `storageConfig.Encryption = enc`; `repository.New(kctx, secret, store, configBytes)` (contract "Create (encrypted)", FR-001).
- [X] T006 Implement encrypted **open** in `openRepo`/`openOrCreateRepo` (`src/internal/app/plakar.go`): read `cfg.Encryption` via `storage.NewConfigurationFromBytes`; the four-way switch — plaintext+no-pass → `secret=nil`; plaintext+pass → warn+ignore (VR-3); encrypted+no-pass → `errEncryptedRepoNeedsPassphrase`; encrypted+pass → `DeriveKey` then `VerifyCanary` (else `errWrongPassphrase`) then `repository.New(secret)`. No read/write before the canary check (FR-005, FR-012, VR-2, contract "Open").
- [X] T007 Add `--passphrase-stdin` to the hidden `__run-connector` worker in `src/internal/app/run_connector.go`: when set, read one line from stdin into the passphrase, pass it to the repo-open path (T006); never accept the passphrase via flag value or env (FR-002, FR-007, contract "Runner key injection").
- [X] T008 Inject the passphrase into the runner via **stdin** in `src/internal/app/runner.go`: add a `passphrase` parameter to `LaunchComposeBackup`/`LaunchComposeRestore`; when non-empty, run `docker ... -i`, append `--passphrase-stdin` to the worker args, write the passphrase + newline to the container's stdin and close it; MUST NOT add it to argv or `-e` env (FR-007, FR-011, VR-4, contract).

**Checkpoint**: `go build ./...` and `go vet ./...` green; encrypted repo can be created and re-opened in-process (host-side), wrong passphrase rejected by the canary.

---

## Phase 3: User Story 1 — Create an encrypted backup (P1) 🎯 MVP

**Goal**: `create --backend plakar --encrypt` + a passphrase produces an encrypted repo; the stored bytes reveal no plaintext DB content; a second backup appends under the same encryption.

**Independent test**: encrypted backup into a fresh repo → scan the repo bytes for known DB content → none recoverable; backup group recorded complete.

- [X] T009 [US1] In `src/cmd/infrahub-backup/main.go`: on `create`, when `--backend plakar` and `--encrypt-key` is set → return `errEncryptKeyOnPlakar` (redirect to `--encrypt` + passphrase), never silent-ignore (VR-7, clarify); keep `--encrypt-key` working for the tarball backend.
- [X] T010 [US1] In `src/cmd/infrahub-backup/main.go`: wire `--encrypt` + resolved passphrase into the plakar `create` path; when `--encrypt` is set, call `validatePassphrase()` and refuse before any repo work if absent/too short (FR-006, FR-013, VR-1, VR-6).
- [X] T011 [US1] In `src/internal/app/backup.go`: pass `Encrypt` + passphrase from `CreateBackup` into `CreatePlakarBackup` (currently dropped on the plakar path) (FR-004).
- [X] T012 [US1] In `src/internal/app/plakar_backup.go`: accept encrypt+passphrase; `ensurePlakarRepo` creates the repo encrypted (T005); `writeMetadataSnapshot` opens with the passphrase; pass the passphrase to `LaunchComposeBackup` for **every** component (neo4j enterprise/community, postgres) so the whole repo is encrypted (FR-002, FR-003, VR-5).
- [X] T013 [P] [US1] Test in `src/internal/app/plakar_backup_test.go`: create an encrypted repo in-process, write a snapshot with known marker bytes, then assert the repo's on-disk bytes do not contain the marker (SC-001, G1). Also assert `--encrypt` without a passphrase errors before create (VR-1) and a `<12`-char passphrase is rejected (VR-6).

**Checkpoint**: encrypted backup works standalone; SC-001 verified by the byte-scan test.

---

## Phase 4: User Story 2 — Restore from an encrypted backup (P1)

**Goal**: restore from an encrypted repo with the passphrase restores both Neo4j editions + Postgres intact; a wrong/absent passphrase fails fast with no changes.

**Independent test**: encrypted backup → wipe → restore with the passphrase → DB contents match source; repeat with a wrong passphrase → clear failure, nothing changed.

- [X] T014 [US2] In `src/cmd/infrahub-backup/main.go`: wire the resolved passphrase into the plakar `restore` path (FR-004).
- [X] T015 [US2] In `src/internal/app/plakar_restore.go`: add a passphrase parameter to `RestorePlakarBackup`/`restoreComponentViaRunner`; open the encrypted repo (T006) — fail fast on missing/wrong passphrase before touching any DB; pass the passphrase to `LaunchComposeRestore` via stdin for each component (FR-002, FR-005, FR-012, VR-2).
- [X] T016 [P] [US2] Test in `src/internal/app/plakar_restore_test.go`: open an encrypted repo with a wrong passphrase → `errWrongPassphrase`, and with no passphrase → `errEncryptedRepoNeedsPassphrase`; assert no exporter/restore call is reached (SC-003, SC-006, G2).

**Checkpoint**: encrypted restore wired; negative-key behavior unit-tested. Full E2E round-trip is exercised in Phase 6 (T021).

---

## Phase 5: User Story 3 — List/inspect an encrypted repository (P2)

**Goal**: `snapshots list` on an encrypted repo shows groups with the passphrase, fails clearly without it.

**Independent test**: list an encrypted repo with the key (groups shown) and without (clear key-required error).

- [X] T017 [US3] In `src/cmd/infrahub-backup/main.go`: wire the resolved passphrase into the `snapshots list` path (FR-002).
- [X] T018 [US3] In `src/internal/app/snapshots.go`: pass the passphrase to the `openRepo` call on the listing path (T006) so an encrypted repo lists with the key and returns `errEncryptedRepoNeedsPassphrase` without it (FR-005, VR-2).
- [X] T019 [P] [US3] Test in `src/internal/app/snapshots_test.go`: list an encrypted repo with the correct passphrase → groups returned; without → clear key-required error, no partial output (SC-003).

**Checkpoint**: all three stories independently testable; in-process unit tests green.

---

## Phase 6: Polish & Cross-Cutting

- [X] T020 [P] No-leak verification (SC-004, G3): grep the tool's logs/stderr from an encrypted backup for the passphrase → absent; `docker inspect` the runner container's `Args` + `Env` during a backup → passphrase absent. Record the recipe in `specs/004-plakar-encryption/quickstart.md` (extend the validation checklist).
- [X] T021 E2E encrypted round-trip against a throwaway Infrahub (per quickstart "Test recipe"): cross-compile `GOOS=linux GOARCH=arm64 CGO_ENABLED=0`; encrypted backup → wipe → restore with the passphrase for Neo4j **Enterprise online**, Neo4j **Community offline**, and **Postgres**; assert node/row counts match source (SC-002). Then a wrong-passphrase restore → fail, no changes (SC-003/SC-006). **Backend scope**: encryption lives in the repo CONFIG and is storage-agnostic, so the `fs://` round-trip validates the encryption path for `s3://` too; if an S3 endpoint (e.g. MinIO) is readily available, add a create+list smoke check against an encrypted `s3://` repo to confirm the backend wiring carries the encrypted CONFIG (otherwise note s3 as covered-by-design, not E2E-exercised — C1).
- [X] T022 [P] Plaintext regression (SC-005, G4): a no-`--encrypt` backup→restore still round-trips unchanged, and opening an existing 003 plaintext repo (no passphrase) is unaffected; add/extend a test in `src/internal/app/plakar_test.go`. **Also assert VR-3** (C2): opening a **plaintext** repo **with** a passphrase supplied warns and continues (`secret=nil`, no error, repo usable) — the warn-and-ignore path, not an error.
- [X] T023 [P] Update `README.md` + `src/cmd/infrahub-backup/main.go` flag help: document `--encrypt`, `INFRAHUB_BACKUP_PASSPHRASE`, `--passphrase-file`, the 12-char minimum, the lost-passphrase warning (no escrow), and that `--encrypt-key` is tarball-only.
- [X] T024 Final gate: `make build`, `go vet ./...`, `make test`; confirm no `go.mod`/`go.sum` change (so flake.nix vendorHash is untouched, per CLAUDE.md).

---

## Dependencies & Execution Order

```
Setup (T001)
  └─ Foundational (T002 → T003,T004 → T005 → T006 → T007 → T008)   [BLOCKS all stories]
       ├─ US1 (T009,T010 → T011 → T012 → T013)        ← MVP
       ├─ US2 (T014 → T015 → T016)                    (needs encrypted repo from US1 for E2E, but unit-testable alone)
       └─ US3 (T017 → T018 → T019)
            └─ Polish (T020,T021,T022,T023 → T024)
```

- **Foundational is a hard barrier**: T005/T006 (create/open) and T007/T008 (runner stdin) gate every story.
- **Within Foundational**: T003/T004 are `[P]` (both new funcs in utils.go but independent — if a contributor splits them, fine; else do sequentially). T005 before T006 (open reuses the create-side config understanding); T007 before T008 (runner must know the worker flag exists).
- **Story order**: US1 (P1, MVP) → US2 (P1) → US3 (P2). US2/US3 only touch their own command path + one app file each, so they can proceed in parallel once Foundational is done; the E2E (T021) needs an encrypted repo, so run it after US1+US2.

## Parallel Opportunities

- Foundational: T003 + T004 together (`[P]`).
- After Foundational: US2 and US3 implementation can run concurrently (disjoint files: `plakar_restore.go`+`snapshots.go`, distinct `main.go` blocks — coordinate the single `main.go` edits).
- Test tasks T013, T016, T019 are `[P]` (distinct `_test.go` files).
- Polish T020, T022, T023 are `[P]`; T021 then T024 are sequential and last.

## Implementation Strategy

- **MVP = Phase 1 + Phase 2 + Phase 3 (US1)**: an encrypted repo that demonstrably hides plaintext (SC-001). Ship/validate this first.
- **Increment 2 = US2**: prove the round-trip restores both editions + Postgres (SC-002) and that a wrong key fails safe (SC-003/SC-006).
- **Increment 3 = US3 + Polish**: listing parity, the no-leak scan (SC-004), and the plaintext-unchanged regression (SC-005).

## Independent Test Criteria (per story)

- **US1**: encrypted repo bytes reveal no DB plaintext; `--encrypt` without a (≥12-char) passphrase refuses before create.
- **US2**: encrypted backup→restore round-trips Neo4j (enterprise+community) + Postgres with matching counts; wrong/absent key fails fast, zero changes.
- **US3**: list shows groups with the key, returns a clear key-required error without it.
