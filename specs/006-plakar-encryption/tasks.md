# Tasks: Encrypt the Plakar backend at rest, and extract the Neo4j integration

**Feature**: `006-plakar-encryption` | **Branch**: `004-plakar-encryption`
**Input**: plan.md, spec.md, research.md, data-model.md, contracts/encryption-and-keys.md, contracts/neo4j-integration-repo.md, quickstart.md

Two independent workstreams on one branch:

- **A — encryption** (T001–T024, US1–US3): **complete and E2E-validated.**
- **B — integration extraction** (T025–T056, US4–US5): **not started.**

**Tech (A)**: Go 1.25.0; `github.com/PlakarKorp/kloset` v1.1.0 (`encryption`, `connectors/storage`, `repository`). No new module dependencies → no vendor-hash change. In-place extension of the 003 plakar backend + co-located runner.

**Tech (B)**: Go 1.25.0; new external module `github.com/opsmill/plakar-integration-neo4j` v0.1.0 replacing the `replace`-directed `github.com/PlakarKorp/integration-neo4j`. **Does** change `go.mod` → vendor hash must be regenerated. The new module's `go-kloset-sdk` + `testcontainers-go` deps must not enter this repo's build graph.

**Tests**: requested. A mandates an encrypted round-trip plus negative/no-leak checks (SC-001…SC-006). B's checks are verification gates plus the new repository's own testcontainers suite (SC-007…SC-012); several require a working, uncontended Docker host.

## Conventions

- `[P]` = parallelizable (distinct file, no dependency on an incomplete task in the same phase).
- `[USn]` maps to the spec's user stories. Setup/Foundational/Polish carry no story label.
- Unprefixed paths are repo-relative (workstream A lives under `src/`).
- **`NEW:` prefixes a path in the new integration repository's working tree** (`opsmill/plakar-integration-neo4j`), which is outside this repo.

---

**Workstream A — encryption (Phases 1–6, T001–T024). Complete.**

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

- [X] T020 [P] No-leak verification (SC-004, G3): grep the tool's logs/stderr from an encrypted backup for the passphrase → absent; `docker inspect` the runner container's `Args` + `Env` during a backup → passphrase absent. Record the recipe in `specs/006-plakar-encryption/quickstart.md` (extend the validation checklist).
- [X] T021 E2E encrypted round-trip against a throwaway Infrahub (per quickstart "Test recipe"): cross-compile `GOOS=linux GOARCH=arm64 CGO_ENABLED=0`; encrypted backup → wipe → restore with the passphrase for Neo4j **Enterprise online**, Neo4j **Community offline**, and **Postgres**; assert node/row counts match source (SC-002). Then a wrong-passphrase restore → fail, no changes (SC-003/SC-006). **Backend scope**: encryption lives in the repo CONFIG and is storage-agnostic, so the `fs://` round-trip validates the encryption path for `s3://` too; if an S3 endpoint (e.g. MinIO) is readily available, add a create+list smoke check against an encrypted `s3://` repo to confirm the backend wiring carries the encrypted CONFIG (otherwise note s3 as covered-by-design, not E2E-exercised — C1).
- [X] T022 [P] Plaintext regression (SC-005, G4): a no-`--encrypt` backup→restore still round-trips unchanged, and opening an existing 003 plaintext repo (no passphrase) is unaffected; add/extend a test in `src/internal/app/plakar_test.go`. **Also assert VR-3** (C2): opening a **plaintext** repo **with** a passphrase supplied warns and continues (`secret=nil`, no error, repo usable) — the warn-and-ignore path, not an error.
- [X] T023 [P] Update `README.md` + `src/cmd/infrahub-backup/main.go` flag help: document `--encrypt`, `INFRAHUB_BACKUP_PASSPHRASE`, `--passphrase-file`, the 12-char minimum, the lost-passphrase warning (no escrow), and that `--encrypt-key` is tarball-only.
- [X] T024 Final gate: `make build`, `go vet ./...`, `make test`; confirm no `go.mod`/`go.sum` change (so flake.nix vendorHash is untouched, per CLAUDE.md).

---

**Workstream B — integration extraction (Phases 7–10, T025–T056). Not started.**

## Phase 7: Foundational — workstream B (blocking — required by US4, US5)

**Goal**: a correct local working tree for the new integration repository. Nothing about packaging, publishing, or consuming can start until the module path, provenance, license, and CI are right.

- [X] T025 Seed a fresh working tree for the new repository from the fork's subtree: `git clone --branch integration/neo4j --single-branch https://github.com/BeArchiTek/integrations.git`, then copy its `neo4j/` directory to the new repo root. Use the **fork**, not `contrib/integration-neo4j` — only the fork carries `plugin/`, `tests/`, `LICENSE`, a populated `Makefile`, and CI (R6). **Done** — 19 files seeded to `~/automation/opsmill/plakar-integration-neo4j`, no `.git` carried over.
- [X] T026 Rewrite the module path in `NEW:go.mod`: `module github.com/PlakarKorp/integration-neo4j` → `module github.com/opsmill/plakar-integration-neo4j` (FR-014).
- [X] T027 Retarget **every** reference to the old module path — not just the plugin entrypoints. Six occurrences across five files: `NEW:plugin/neo4j-importer/neo4j-importer.go`, `NEW:plugin/neo4j-exporter/neo4j-exporter.go`, `NEW:importer/importer.go` (imports `manifest` + `neo4jconn`), `NEW:exporter/exporter.go` (`neo4jconn`), `NEW:manifest/manifest.go` (`neo4jconn`), `NEW:tests/logical/offline_test.go` (`tests/testhelpers`). **Not `[P]`** — the internal cross-package imports must all move together or the module will not resolve (FR-014).
- [X] T028 [P] Correct provenance in `NEW:manifest.yaml`: keep the fork's **two-connector** form (each declaring `protocols: [neo4j, neo4j+offline]`); set `tier: third-party`, `homepage: https://github.com/opsmill/plakar-integration-neo4j`, `contact: mailto:support@opsmill.com`. Do **not** carry `contrib/`'s four-executable form — those executables are phantom (R10, FR-016, FR-018, VR-8, VR-9).
- [X] T029 [P] Add the OpsMill 2026 copyright line alongside PlakarKorp's 2025 line in `NEW:LICENSE`, keeping the ISC terms, with a sentence naming the derived scaffolding (FR-019, VR-13).
- [X] T030 [P] Verify CI in `NEW:.github/workflows/test.yml` targets a root-level module. **No change was needed** — the workflow is already root-relative (`go-version-file: go.mod`, `go build ./...`, `make build`, `go vet ./...`, `make test`). Note that in the monorepo it lived at `neo4j/.github/workflows/`, where GitHub never ran it; at the new repo root it becomes active for the first time.
- [X] T031 [P] Merge the READMEs into `NEW:README.md` — the fork's as base, folding in the operational detail from `contrib/integration-neo4j/README.md`; retarget every URL, module path, and install instruction to the OpsMill repository. Dropped the stale DRAFT banner and the "TODO before upstream contribution" list; kept the validated-manually evidence and added the two operational gotchas (restore takes the artifact *file*; the dump output path must be `neo4j`-writable).
- [X] T032 Set `VERSION ?= v0.1.0` in `NEW:Makefile` and confirm the `build`/`package`/`install`/`uninstall`/`test`/`clean` targets reference `./plugin/neo4j-importer` and `./plugin/neo4j-exporter` (FR-023).
- [X] T033 Run `go mod tidy` in the new working tree, then confirm `go build ./...` and `go vet ./...` are clean (SC-007). **Done** — both clean, zero residual references to the old module path.

**Checkpoint**: the new working tree builds and vets clean under the OpsMill module path, with corrected provenance. Nothing published yet.

---

## Phase 8: User Story 4 — Install the Neo4j integration standalone (P2)

**Goal**: a Plakar user with no OpsMill software can build, install, and use the integration for both protocols.

**Independent test**: from a clean checkout of the new repository (no publish required), build the package, install it into Plakar, and confirm both URI schemes back up and restore against a throwaway Neo4j.

- [X] T034 [US4] Build both plugin binaries via `make build` in the new repository → `neo4jImporter` and `neo4jExporter` produced (FR-016, SC-007). **Done** — both arm64 binaries built, ~21 MB each.
- [X] T035 [US4] Verify manifest↔entrypoint agreement — the exact defect `contrib/` carried: every `executable:` in `NEW:manifest.yaml` has a corresponding `NEW:plugin/` entrypoint and vice versa. Diff the declared set against the built set; it must be empty (VR-8, SC-007, US4 scenario 1). **Done** — declared `{neo4jExporter, neo4jImporter}` == built set, diff empty.
- [X] T036 [P] [US4] Confirm both schemes register: `NEW:importer/importer.go` registers `neo4j` **and** `neo4j+offline` against `NewImporter`, `NEW:exporter/exporter.go` likewise against `NewExporter`, and each plugin entrypoint passes the requested protocol through for dispatch (FR-021, US4 scenario 2). **Done** — importer lines 31–32, exporter lines 26–27, both entrypoints pass the constructor to the SDK.
- [X] T037 [US4] Run the new repository's own suite: `make test` (testcontainers — needs a working, uncontended Docker host) (SC-007). **PASS after T037a + T037b** — `TestOfflineDumpLoadRoundTrip` green in 49.5 s: seed 25 nodes → offline dump (36 files, 257.9 MiB) → wipe → load → restart → count restored.
- [X] T037a [US4] Testcontainers could not find the Docker socket on this host: the active Docker context is **OrbStack** (`~/.orbstack/run/docker.sock`) and `/var/run/docker.sock` does not exist, so auto-discovery failed with "rootless Docker not found". Documented the required env (`DOCKER_HOST`, `TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE`) in `NEW:README.md`. Not a code defect.
- [X] T037b [US4] **Real harness defect fixed** in `NEW:tests/testhelpers/neo4j.go`. Root cause was *not* the file ownership the error message pointed at — running the lifecycle as `neo4j` removed that warning and `stop` still timed out. The cause is that `Entrypoint: ["sleep","infinity"]` makes PID 1 a process that **never reaps children**; the image's real PID 1 is `tini`. So the JVM exits and lingers as a `<defunct>` zombie, `neo4j stop` polls the pid for liveness, still sees it, and fails after its internal 120 s timeout even though the server is already down. Fix: `Entrypoint: ["tini","-g","--","sleep","infinity"]` — same stop now returns in ~11 s, no zombie. Verified as a single-variable change (exec user left as root). Documented in the helper so it is not "simplified" back.
- [X] T038 [US4] Package it: `plakar pkg create ./manifest.yaml v0.1.0` produces the `.ptar` (FR-016, SC-007). **Done** — required installing the `plakar` CLI (`go install github.com/PlakarKorp/plakar@v1.1.3`); produced `neo4j_v0.1.0_darwin_arm64.ptar` (27 MB) containing both binaries, `manifest.yaml`, and both schemas.
- [X] T039 [US4] Install and smoke-test out-of-process: `plakar pkg add` succeeded, and **both protocols dispatch to the out-of-process plugin** — probes produced `rpc error` (i.e. they crossed the gRPC plugin boundary) with correctly-constructed command lines: `neo4j-admin database backup --to-path=… --compress=false --from=…` and `neo4j-admin database dump --to-path=…`; the offline path even wrote `/manifest.json` into the snapshot before failing. Both fail *only* because `neo4j-admin` is not on this host's PATH — the connector shells out to it by design. **A complete host-side CLI data round-trip is not achievable here** (it needs Neo4j installed on the host, or the in-container plakar harness the README lists as not automated); the data round-trip is instead covered by T037 and by 003's live Enterprise/Community validations.
- [X] T040 [P] [US4] Review published provenance in `NEW:manifest.yaml` and `NEW:LICENSE`: third-party tier, OpsMill homepage and contact, no Plakar-official claim, dual copyright (US4 scenario 3, SC-012). **Verified.**

**Checkpoint**: US4 fully verified from a local clean checkout — deliberately provable before anything is published or `contrib/` is deleted.

---

## Phase 9: User Story 5 — Consume the integration as a versioned dependency (P2)

**Goal**: this repository resolves the integration as an external tagged module, with no `replace` and no in-tree copy.

**Independent test**: from a clean checkout, build and test this repository with no vendored integration tree and no `replace` directive present.

- [X] T041 [US5] Publish with fresh history: single commit (no fork lineage, no PlakarKorp monorepo history), then create the **public** repository `opsmill/plakar-integration-neo4j` and push (FR-014). **Done** — commit `bfbcff9`, 19 files / 1566 insertions, build artifacts correctly excluded by `.gitignore`; repo public, `isFork: false`, default branch `main`.
- [X] T042 [US5] Tag and push `v0.1.0`; confirm `go get …@v0.1.0` resolves from outside the working tree (FR-023, VR-14). **Done** — note `tag.gpgsign true` is set globally, so a lightweight `git tag` fails with "no tag message?"; an annotated tag is required. Verified end-to-end in a scratch module: `go get` resolved v0.1.0 through the proxy and a consumer importing only `importer`/`exporter` builds.
- [X] T043 [US5] Delete the vendored tree: `git rm -r contrib/integration-neo4j` (10 tracked files) (FR-015, VR-10). **Done** — `contrib/` is gone entirely.
- [X] T044 [US5] In `go.mod`: drop the `replace` and the old pseudo-version `require`; add `github.com/opsmill/plakar-integration-neo4j v0.1.0` (FR-015, VR-10). **Done** via `go mod edit` — zero `replace` directives remain.
- [X] T045 [US5] Retarget the two blank imports and the scheme-mapping comment in `src/internal/app/connectors.go` (FR-017). **Done** — in-process `init()` registration unchanged, comment now records why the plugin binaries are irrelevant here.
- [X] T046 [US5] `go mod tidy` **done**, and the `flake.nix` `vendorHash` is **regenerated and verified** (`sha256-LGJU/…` → `sha256-X5Jeefld13F8gPyL9SSi0sgeGYAznGiKqYTV0kCCcTs=`). `./scripts/update-vendor-hash.sh` could not run here — it needs `nix` (absent) and uses GNU `grep -oP` / `sed -i`, neither on macOS — so the hash was obtained **from nix itself** in a `nixos/nix` container (sentinel hash planted, `got:` read from the fixed-output mismatch — the script's own mechanism), never hand-computed (VR-15). `vendorHash` covers the fetched Go modules, so it is platform-independent. Verified by building: `nix build .#default` completes and the joined output contains both `infrahub-backup` and `infrahub-taskmanager`. **Follow-up:** make the script itself macOS-capable (Docker fallback + portable grep/sed).
- [X] T047 [US5] Verify the dependency boundary (SC-008, SC-010, VR-11). **Done, with a correction to the criterion**: `go mod graph` *does* show one edge each for `go-kloset-sdk` and `testcontainers-go`, because the integration's own `go.mod` declares them — that is the module graph, not the build list, so grepping `go mod graph` is the wrong test and would report a false failure. The meaningful checks both pass: `go list -deps ./src/...` contains **0** packages from either, and `go mod why` reports "main module does not need package" for both. Also verified: 0 `replace` directives, `contrib/` absent, integration resolves at `v0.1.0`.
- [X] T048 [US5] `make build` clean (both CLI binaries + cross-compiled watchdog); `go vet ./src/...` clean; `make test` — all `src/internal/app` tests pass, including the full encryption suite. **`make test` exits non-zero only on `tools/neo4jwatchdog [build failed]`, which fails identically at HEAD** (verified in a detached worktree): it uses Linux-only inotify APIs with no `//go:build linux` constraint, so it never built on macOS. Pre-existing, unrelated to this work. `make lint` not run (golangci-lint not installed here).
- [X] T049 [US5] Re-run both Neo4j round-trips in-process through the external module — Enterprise online (`neo4j://`) and Community offline (`neo4j+offline://`) — and confirm they match the results recorded for 003 (FR-021, SC-009). **PASS, all three runs.** Built a throwaway Infrahub-shaped Compose stack (only `database` and `task-manager-db` real; `infrahub-server`/`task-manager`/`task-worker` are placeholders carrying the `INFRAHUB_DB_*` / `PREFECT_*` env the tool reads for credential discovery), seeded 25 Neo4j nodes + 12 Postgres rows, backed up, wiped, restored, verified:
  - **Community** (`neo4j+offline://`, stop → dump → restart): 25 + 12 restored.
  - **Enterprise** (`neo4j://` online, no stop on the backup side — 5 s vs Community's 13 s, confirming the online path): 25 + 12 restored.
  - **Community + `--encrypt`** (workstreams A and B together, encrypted repo written by the external integration): 25 + 12 restored; listing without the passphrase fails with `repository is encrypted; a passphrase is required`, and with it succeeds.

  Harness at `scratchpad/e2e/` (`run-e2e.sh` + per-edition compose files); cross-built runner via `GOOS=linux GOARCH=arm64` and `INFRAHUB_RUNNER_BINARY`.

  **Two flaws in my first pass, both fixed — worth recording because either would have produced a false PASS:**
  1. *The wipe was never verified.* Neo4j is stopped/restarted around the offline backup and around every restore, so a count taken before Bolt returns silently yields empty. The first Community run printed `neo4j=` and the first encrypted run printed `neo4j=25` after the "wipe" — meaning the delete had not taken effect and the restore was being validated against data that was never removed. Added a `wait_bolt` poll plus a **hard gate** that aborts unless both stores actually read 0 before restoring.
  2. *A bogus leak check.* I grepped the encrypted repo for the 3-byte string `E2E` and it "matched". A 3-byte sequence occurs by chance roughly every 16 MB of high-entropy data — control strings `Q7X` and `J8Q`, which never existed anywhere, matched identically. There is no leak, and the rigorous check already exists as `TestEncryptedRepoHidesPlaintext` (random 1024-byte marker **with** a plaintext control), which passes in `make test`.

  Incidental: `postgres:18` moved `PGDATA` to `/var/lib/postgresql/18/docker` and requires the volume mounted at `/var/lib/postgresql`, not `.../data` — the old mount makes the container exit 1. Only affects my throwaway compose.

**Checkpoint**: exactly one source of truth for the integration; both schemes still round-trip; Nix build reproducible.

---

## Phase 10: Polish & Cross-Cutting — workstream B

- [X] T050 [P] Record the supersession of 003's upstreaming plan: new `specs/003-upstream-plakar-integrations/SUPERSEDED-BY-006.md`, banners on the six affected 003 documents, and 003's own T040 struck through as DROPPED (FR-022, SC-011). **Done 2026-08-07.**
- [X] T051 [P] Update `CLAUDE.md`: drop the stale `integration-neo4j (new, fork build)` entry, remove the duplicated storage line, and add the "being extracted" section covering the wrong vendored manifest and the no-upstreaming guidance (FR-022). **Done 2026-08-07.**
- [X] T052 [P] Correct `specs/006-plakar-encryption/checklists/requirements.md`, which still recorded the key model as keypair/asymmetric — contradicting the spec's own passphrase decision — and extend it with workstream-B coverage. **Done 2026-08-07.**
- [X] T053 [P] Final stale-reference sweep, only meaningful **after** T043–T045: confirm no `PlakarKorp/integration-neo4j` or fork reference survives in code, specs, or docs outside deliberately-recorded history (FR-022, SC-011). **Done** — `git grep` finds zero occurrences outside `specs/` (where they are deliberate history). The sweep caught one live staleness: the `CLAUDE.md` section written mid-migration still described the vendored tree as "the state on disk today"; rewritten to describe the integration's real home, the in-process consumption model, the engine-coupling rule, and two pre-existing host gotchas.
- [X] T054 [P] Author the hub recipe at `community/v1.1.0/neo4j/recipe.yaml` — three fields: `name: neo4j`, `version: v0.1.0`, `repository: https://github.com/opsmill/plakar-integration-neo4j`. **Author only; do not open the PR** (FR-020). **Done** — content staged in the session scratchpad at `hub-recipe/community/v1.1.0/neo4j/recipe.yaml`; no fork created and no PR opened, pending your go-ahead.
- [ ] T055 Gated submission: open the `PlakarKorp/hub` PR carrying `community/v1.1.0/neo4j/recipe.yaml` (from T054) **only after** T038 (package verified), T041 (repo public) and T042 (tag resolvable) all pass. This PR is where it gets confirmed whether the builder accepts an out-of-organisation `repository` — no existing recipe has one; the fallback is the proxmox arrangement (R8, FR-020).
- [X] T056 Final gate B. **Passing**: `make build`, `go vet ./src/...`, all `src/internal/app` tests, zero `replace` directives, no `contrib/` tree, integration resolved at `v0.1.0`, plugin/test deps absent from the build closure, `flake.nix` vendorHash regenerated and `nix build .#default` green (T046), and all three E2E round-trips (T049). `make lint` now verified via the pinned `golangci-lint v2.7.2` image — **0 issues** (T059). `make test` still exits non-zero only on the pre-existing macOS-only `tools/neo4jwatchdog` build failure. The `main` merge that used to block CI entirely is done — see Phase 11.

---

## Phase 11: Integration with `main` — merge, renumber, and what CI then found

The branch had diverged from `main` by 108 commits, which left PR #163 `CONFLICTING`. GitHub cannot build a merge ref for a conflicting PR, so **no CI had ever run on this work**. Resolving that was what surfaced T060–T063: none of these are merge artefacts, they are defects the merge made visible.

- [X] T057 Merge `origin/main` (11 conflicts). Each side's behaviour preserved rather than one side winning: `main.go` keeps main's restore refactor *and* the plakar passphrase load; `CLAUDE.md` became an `@AGENTS.md` pointer on main, so this feature's Neo4j-integration section and Active Technologies entries were ported into `AGENTS.md` rather than dropped with the file; `go.mod` unions both require sets; `flake.nix` keeps main's `infrahub-collect` package with the vendorHash regenerated for the merged module set (neither side's hash was valid). `main`'s new blanket `build/` ignore was scoped to `build/*` + `!build/runner/`, because a bare `build/` stops git descending into the tracked runner Dockerfile context.
- [X] T058 Renumber this feature `004-` → `006-`. `main` brought `004-backup-retention-policy` and `005-restore-latest-backup`, so both lower numbers were taken. The git branch keeps its `004-plakar-encryption` name — PRs are open against it — so the front matter now records branch and directory separately instead of implying they match. `FR-`/`SC-`/`T0` identifiers were preserved (the substitution required a non-word, non-hyphen boundary).
- [X] T059 Delete the streaming helpers the runner rework orphaned (254 lines across `backup_neo4j.go`, `backup_taskmanager.go`). `golangci-lint` reported all eight as unused; `main` lints clean and this branch already carried them pre-merge. This was not cosmetic: `test` needs `go-lint` and both e2e suites need `test`, so the whole Go test chain was skipping and the merge was unverified beyond `nix-build` and the linters.
- [X] T060 Fix `fs:///path` being classified as remote (FR-002 regression, operator-facing). Three call sites tested only for `"://"`, which `fs://` contains, so the **documented** spelling got no bind-mount and the host path was handed to the in-container worker. All four sites now share `parseRepoLocation`. Every local test recipe used the bare `--repo /tmp/...` spelling, which is why this branch's own E2E passed while `--repo fs:///backups/infra` — the form in `README.md` and the quickstart — was broken throughout.
- [X] T061 Fix the runner's effective user, and preserve data-directory ownership across a restore. `--user root` never took effect for Neo4j: `docker-entrypoint.sh` drops to uid 7474 even when started as root, so the worker could not read a repository directory owned by whoever ran the tool (`open /repo/CONFIG: permission denied`). Postgres honours root, so Neo4j was the only image deviating from what the runner already asked for; the entrypoint is now bypassed, since the runner borrows the image for its tools rather than to start a database. Restores therefore write as real root, so `--preserve-owner` restores the data directory's original ownership — previously correct only by accident, via the privilege drop being removed. This closes the "neo4j-user ownership" item 003 recorded as the tool's job. Also rewrites loopback S3 endpoints to `host.docker.internal` (+`--add-host`), because the runner sits on the database's compose network where `localhost` is the runner itself.
- [ ] T062 **Open — regression vs `main`, blocks merging #163.** Design prepared in
  [k8s-runner-plan.md](./k8s-runner-plan.md), which establishes why the 003 runner is structurally
  Docker-only (the connector runs `neo4j-admin` itself so it must sit with the data directory,
  while `test_k8s_plakar.py` puts the repository on the tool's local disk where no pod can reach
  it), why `main` avoided it (streaming through backend-agnostic `ExecStreamPipe`, snapshot written
  in-process), and recommends one transport per backend. **Decided**: align the layouts — the streaming path adopts the connector output. Without it a Docker-taken enterprise backup cannot restore on Kubernetes at all (a tar where a `.backup` artifact is expected), and a Community one silently depends on the database being named `neo4j`. `plakar_backup.go:39` and `plakar_restore.go:34` refuse Kubernetes outright ("supports Docker Compose only; Kubernetes support is pending"), but `main` ships `tests/e2e/test_k8s_plakar.py` and `test_k8s_plakar_s3.py` and **both k8s e2e suites pass on `main`**. So the 003/004 runner rework moved plakar on Kubernetes from working to unsupported. Merging as-is would put that regression on `main`. Two routes: keep a non-runner path for the Kubernetes backend, or implement pod-based execution in the runner. Decision deferred deliberately — it is feature work, not conflict resolution.
- [ ] T064 **Open — the remaining `main` e2e failure.** With T060/T061 in, the plakar backup and `snapshots list` now succeed in CI; all three `test_docker_plakar.py` cases instead fail because Infrahub cannot reach Neo4j afterwards (`/api/config` answers, `/api/schema` returns 503, server-side `SessionExpired: defunct connection … ('database', 7687)`). The three tests share a class-scoped compose stack, so the first one's backup poisons it and the S3 case fails at *seeding* — meaning T061's S3 rewrite is still unexercised. Both editions suspend Neo4j with SIGSTOP and resume with `kill -CONT`, on `main` too, so the DB is not restarted; the candidate difference is how long it stays suspended, since the runner adds container startup per component. Not reproducible in `test/e2e/`, whose `infrahub-server` is an `alpine sleep infinity` placeholder — needs a live instance (`invoke demo.start` in a sibling `infrahub` checkout). Worth capturing the database container's logs in the CI failure dump too: only `infrahub-server` was captured, which is what left the cause unobservable.
- [ ] T063 Confirm or dismiss the enterprise Docker e2e leg. It failed differently across consecutive runs (94% then 11%, the latter on a `collect` test) with a `503` from the Infrahub server, which reads as contention across four parallel e2e jobs on the shared runners rather than a defect. Judge it once the community leg is green.

---

## Phase 12: Deferred by the 2026-08-11 code review — blocked on a dependency release

Each of these was verified against the pinned connector sources during the review of
`test/e2e-plakar-round-trip`. None can be fixed inside this repository: they need a release of
another module, so they are recorded rather than attempted. Everything else the review found was
fixed on that branch.

- [ ] T065 **Restore `--expand-commands` to every neo4j-admin invocation.** Blocked on a new
  `opsmill/plakar-integration-neo4j` release. `main` passed `--expand-commands` at all five
  neo4j-admin call sites (`backup_neo4j.go` lines 43, 220, 422, 431, 483). The connector's
  `adminArgs` emits `["database","backup","--to-path=…","--compress=false"]` /
  `["database","dump",…]` with no such flag, and `ParseConnConfig` accepts only
  location/host/port/username/password/database/data_dir/neo4j_admin_path/neo4j_bin_dir/
  include_metadata/overwrite — there is no key to set it through. Impact: a deployment whose
  `neo4j.conf` uses command expansion (e.g. `server.memory.heap.max_size=$(…)`, common with
  secret-injection sidecars) fails both backup and restore with "the config file contains command
  expansion … use --expand-commands". Needs an `expand_commands` option in the integration, then a
  version bump plus `scripts/update-vendor-hash.sh`.
- [ ] T066 **Make the runner's view of the store's on-disk layout derived rather than assumed.**
  Partly blocked on the same integration. `composeRunnerArgs` copies none of the database
  container's `NEO4J_*` environment, and `dbDataDir` is hardcoded to `/data`. A deployment that
  relocates the store via `NEO4J_server_directories_data` gets a runner whose neo4j-admin resolves
  `server.directories.data` from image defaults — so `database dump` archives an absent store and
  `database load --overwrite-destination` writes where the live server does not read, while
  `--preserve-owner` chowns the wrong tree. The connector parses `data_dir`
  (`neo4jconn/conn.go:131-134`) but never passes it to neo4j-admin, so `neo4j+offline:///data` is
  decorative and the image's own config is the only thing deciding. Fixing this properly needs the
  integration to honour `data_dir`, plus this repo reading the deployment's actual data directory
  instead of assuming `/data`.
- [ ] T067 **STS / temporary S3 credentials on an `s3://` repository.** Blocked on
  `PlakarKorp/integration-s3`. The review asked for `AWS_SESSION_TOKEN` and `AWS_REGION` to be
  forwarded into the runner "so STS credentials work"; they would not. Pinned
  integration-s3 v1.1.0-beta.5 builds its client with
  `credentials.NewStaticV4(accessKey, secretAccessKey, "")` — the session token is hardcoded empty
  — and sets no `Region` on `minio.Options` (`storage/storage.go:134-138`). So a session token has
  nowhere to go, on the host as much as in the runner: this is a uniform limitation, not a
  host/runner asymmetry. Forwarding the variables today would be dead code. Needs a
  `session_token` option upstream (or a different credentials provider), after which both
  `storeConfig` and the runner credentials channel can carry it.

---

## Dependencies & Execution Order

```text
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

Workstream B (T025–T056), independent of A:

```text
Foundational B (T025 → T026 → T027,T028,T029,T030,T031 → T032 → T033)   [BLOCKS US4, US5]
  └─ US4  (T034 → T035,T036 → T037 → T038 → T039 → T040)
       │      verifiable entirely from a LOCAL checkout — no publish, contrib/ still intact
       └─ US5  (T041 → T042 → T043 → T044 → T045 → T046 → T047 → T048 → T049)
                  T041/T042 publish+tag;  T043 is the point of no return for contrib/
            └─ Polish B (T050–T052 done · T053 after T045 · T054 → T055 gated on T038+T041+T042 · T056 last)
```

- **Order is deliberate, not incidental**: US4 proves build, manifest agreement, tests, packaging and install *before* US5's T043 deletes `contrib/`. Every check that is cheaper to run while the vendored tree still exists is front-loaded.
- **T041/T042 gate US5**: `go get` needs a resolvable tag, so publish and tag precede the dependency swap.
- **T046 is not optional**: any `go.mod` change requires the vendor-hash regeneration, or the Nix build breaks.
- **T055 is externally gated**: it depends on Plakar accepting the recipe, which no existing recipe's `repository` field precedents (all are in-org). Not a blocker for T025–T054 — the fallback preserves every other outcome.
- **A and B do not interact**: B touches `go.mod`, `connectors.go`, `contrib/`, and docs; A touched the encryption path in `src/internal/app` + `main.go`. They can be worked in either order.

## Parallel Opportunities

- Foundational: T003 + T004 together (`[P]`).
- After Foundational: US2 and US3 implementation can run concurrently (disjoint files: `plakar_restore.go`+`snapshots.go`, distinct `main.go` blocks — coordinate the single `main.go` edits).
- Test tasks T013, T016, T019 are `[P]` (distinct `_test.go` files).
- Polish T020, T022, T023 are `[P]`; T021 then T024 are sequential and last.

Workstream B:

- Foundational B: T027, T028, T029, T030, T031 all `[P]` — five distinct files in the new tree (plugin imports, manifest, license, CI, README), none dependent on another.
- US4: T035 + T036 `[P]` (one inspects manifest-vs-plugin sets, the other the registration calls); T040 `[P]` with anything in the phase.
- US5: strictly sequential. Each step invalidates the previous state — publish, tag, delete, swap, tidy, hash, verify — so there is nothing safe to parallelise here.
- Polish B: T053 and T054 `[P]`; T055 gated externally; T056 last.

## Implementation Strategy

- **MVP = Phase 1 + Phase 2 + Phase 3 (US1)**: an encrypted repo that demonstrably hides plaintext (SC-001). Ship/validate this first.
- **Increment 2 = US2**: prove the round-trip restores both editions + Postgres (SC-002) and that a wrong key fails safe (SC-003/SC-006).
- **Increment 3 = US3 + Polish**: listing parity, the no-leak scan (SC-004), and the plaintext-unchanged regression (SC-005).

Workstream B:

- **Increment B1 = Phase 7 + US4**: a correct, buildable, packageable, installable integration repository — still local, `contrib/` untouched, nothing published. This is the reversible increment: if anything is wrong, discard the tree and retry at no cost to this repo.
- **Increment B2 = US5**: publish, tag, then swap this repo onto the external module. T043 (deleting `contrib/`) is the irreversible step, and by then build, manifest agreement, tests, packaging and install have all passed.
- **Increment B3 = Polish B**: stale-reference sweep, recipe authoring, and the externally-gated hub PR.
- **Order between workstreams**: A is already done, so B can start immediately. If both were pending, either order works — they share no files.

## Independent Test Criteria (per story)

- **US1**: encrypted repo bytes reveal no DB plaintext; `--encrypt` without a (≥12-char) passphrase refuses before create.
- **US2**: encrypted backup→restore round-trips Neo4j (enterprise+community) + Postgres with matching counts; wrong/absent key fails fast, zero changes.
- **US3**: list shows groups with the key, returns a clear key-required error without it.
- **US4**: from a clean local checkout — `make build` yields a plugin executable for every declared connector with none declared-but-missing, `make test` green, `plakar pkg create` produces the `.ptar`, and the installed package backs up and restores over both `neo4j://` and `neo4j+offline://`. Manifest claims third-party tier with OpsMill contact.
- **US5**: from a clean checkout of this repo — builds, tests, lints, vets clean with no `replace` directive and no `contrib/` tree; `go mod graph` free of `go-kloset-sdk` and `testcontainers`; both Neo4j schemes still round-trip in-process.
