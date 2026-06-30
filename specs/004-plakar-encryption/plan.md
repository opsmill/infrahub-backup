# Implementation Plan: Encrypt the Plakar backend at rest

**Branch**: `004-plakar-encryption` | **Date**: 2026-06-30 | **Spec**: [spec.md](./spec.md)
**Input**: Feature specification from `specs/004-plakar-encryption/spec.md`

## Summary

Make the reworked Plakar backend (003) able to write **encrypted** repositories using kloset's **native symmetric** encryption, keyed by a **passphrase** (the spike ruled out asymmetric — kloset repo encryption is symmetric; its keypair is ed25519 *signing*; see [research.md](./research.md)). When `--encrypt` is set, the repo is created with an `encryption.Configuration` (Argon2id KDF + AES256-GCM-SIV) and a canary; the passphrase-derived secret is supplied on every open (create, backup, restore, list), including inside the 003 co-located runner — passed to the runner via **stdin** so it never lands on the command line, env dump, or logs. Wrong/absent passphrase fails fast via the canary. Plaintext repos and the no-`--encrypt` path are unchanged.

## Technical Context

**Language/Version**: Go 1.25.0
**Primary Dependencies**: `github.com/PlakarKorp/kloset` v1.1.0 — `encryption` (symmetric: `NewDefaultConfiguration`, `DeriveKey`, `DeriveCanary`, `VerifyCanary`), `connectors/storage` (`Configuration.Encryption`, `NewConfigurationFromBytes`), `repository.New(secret, …)`; builds on the 003 runner (`runner.go`, `run_connector.go`)
**Storage**: kloset repository (`fs://` / `s3://`), now optionally symmetric-encrypted (KDF + cipher params + canary in the repo CONFIG)
**Testing**: `go test`; E2E backup→restore round-trip with a passphrase against a throwaway Infrahub (both Neo4j editions + Postgres), plus a wrong/absent-passphrase failure check
**Target Platform**: tool binaries Linux/Darwin/Windows; runner image Linux; Docker Compose deployments (K8s deferred, per 003)
**Project Type**: single Go CLI project (extends 003)
**Performance Goals**: KDF cost is per-open (Argon2id default); negligible vs. dump time
**Constraints**: passphrase never logged / never on the runner's argv or `-e` env (use stdin); encryption fixed at repo-create time; no in-place re-encryption
**Scale/Scope**: one deployment, two databases; one repo per backup target

## Constitution Check

`.specify/memory/constitution.md` is the unpopulated template — no ratified principles, so no gates to evaluate. The plan follows `CLAUDE.md` conventions (Cobra commands; shared `src/internal/app`; `fmt.Errorf` wrapping; `scripts/update-vendor-hash.sh` on any `go.mod` change — **no new module deps here**, so the hash is unaffected). **Initial gate**: PASS. **Post-design**: PASS — no new dependencies, no new build artifacts; changes are confined to the existing plakar backend + runner.

## Project Structure

### Documentation (this feature)

```text
specs/004-plakar-encryption/
├── plan.md · research.md · data-model.md · quickstart.md
├── contracts/encryption-and-keys.md
├── checklists/requirements.md
└── tasks.md            # (/speckit.tasks)
```

### Source Code (changes, all in this repo)

```text
src/internal/app/
├── plakar.go            # openOrCreateRepo/openRepo: create with Encryption+canary; open derives secret + VerifyCanary
├── app.go               # Configuration/PlakarConfig: add Passphrase (source: env/file) + Encrypt flag plumbing
├── backup.go            # CreateBackup: pass --encrypt + passphrase to CreatePlakarBackup (currently dropped)
├── plakar_backup.go     # CreatePlakarBackup: accept encrypt+passphrase; encrypted ensurePlakarRepo + metadata; pass passphrase to runner
├── plakar_restore.go    # RestorePlakarBackup: accept passphrase; open encrypted repo; pass passphrase to runner
├── snapshots.go         # ListSnapshots/openRepo path: supply passphrase for an encrypted repo
├── runner.go            # LaunchComposeBackup/Restore: inject passphrase via STDIN (docker run -i), not argv/-e
└── run_connector.go     # __run-connector: add --passphrase-stdin; derive secret; open repo encrypted
src/cmd/infrahub-backup/main.go   # wire --encrypt (plakar) + INFRAHUB_BACKUP_PASSPHRASE / --passphrase-file into create/restore/snapshots;
                                  # enforce 12-char min passphrase (FR-013); reject --encrypt-key with --backend plakar (clarify)
```

**Structure Decision**: in-place extension of the 003 plakar backend + runner; no new packages, modules, or build artifacts. Encryption is a property of the repository (set at create, supplied at open), so it composes with the existing per-component runner flow without touching the connectors.

## Risks & Mitigations

| Risk | Mitigation |
|---|---|
| Passphrase leakage (logs, `docker inspect` argv/env, process list) | Pass to the runner via **stdin** only (R3); never log it; redact in errors; host-side ops hold it in memory only. SC-004 scans for leaks. |
| Wrong passphrase → corrupt/partial output | `VerifyCanary` **before** any read/write (FR-012); fail fast with a clear message. |
| Lost passphrase ⇒ unrecoverable backups | Document prominently; encryption is opt-in; no key escrow in scope. |
| Runner stdin plumbing (docker run -i + writing the secret to the worker) | Small, testable; covered by the E2E (encrypted round-trip both editions + postgres). |
| Mixing encrypted/plaintext repos | Detect via `cfg.Encryption` on open; clear errors for passphrase-without-encryption and encryption-without-passphrase. |

## Complexity Tracking

No constitutional violations; no new dependencies or artifacts. Nothing to justify.
