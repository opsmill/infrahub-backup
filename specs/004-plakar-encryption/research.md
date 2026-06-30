# Phase 0 Research: Encrypt the Plakar backend at rest

**Feature**: `004-plakar-encryption` · **Date**: 2026-06-30

Resolves the spec's open items. The gating spike (key-management feasibility) is **done** and changed the design from keypair to passphrase.

---

## R1. Key-management model — SPIKE RESULT (decision: passphrase / symmetric)

**Decision**: Use kloset's **native symmetric** repository encryption, keyed by a **passphrase**.

**Spike evidence** (kloset v1.1.0 source):
- `connectors/storage.Configuration.Encryption` is `*encryption.Configuration` — kloset's **symmetric** config (`encryption/symmetric.go`): `{ SubKeyAlgorithm: AES256-KW, DataAlgorithm: AES256-GCM-SIV, KDFParams (default ARGON2ID), Canary }`.
- Repository decryption is symmetric: `repository.New(ctx, secret, store, config)` → `encryption.DecryptStream(config.Encryption, r.secret, …)`. `secret == nil` ⇒ plaintext (today).
- `encryption/keypair` is **ed25519** (`Generate/Sign/Verify`, `FromPublicKey/FromPrivateKey`) used for snapshot **signing/verification** (`snapshot/verify*`), **not** encryption.

**Consequence / rationale**: There is no asymmetric repo-encryption path. Writing an encrypted repo (and reading existing chunks for dedup) requires the symmetric secret, so an asymmetric "backup needs only the public key" model is impossible natively. Passphrase (symmetric) is the native, dedup-preserving option.

**Alternatives considered**:
- *Keypair (asymmetric)* — ruled out by the spike (not how kloset encrypts repos).
- *Custom ECIES key-wrapping* (wrap a random symmetric repo key under an ECIES public key, reuse `encryption.go`) — would give "public-key-only" backup **only for a fresh repo**; incremental backups + restore still need the symmetric secret, and it's bespoke crypto on top of kloset. Rejected: high risk, partial benefit.

---

## R2. Encrypted-repo create & open flow (kloset API)

**Create (encrypted)** — in `openOrCreateRepo`:
```
enc := encryption.NewDefaultConfiguration()        // Argon2id + AES256-GCM-SIV
secret, _ := encryption.DeriveKey(enc.KDFParams, []byte(passphrase))
enc.Canary, _ = encryption.DeriveCanary(enc, secret)
storageConfig.Encryption = enc                      // (today: = nil)
// serialize + storage.Create as today, then:
repository.New(kctx, secret, store, configBytes)    // (today: secret = nil)
```

**Open (existing)** — in `openRepo`/`openOrCreateRepo`:
```
store, configBytes, _ := storage.Open(kctx, sc)
cfg, _ := storage.NewConfigurationFromBytes(version, <unwrapped config>)
if cfg.Encryption != nil {                          // encrypted repo
    if passphrase == "" { return error("repository is encrypted; a passphrase is required") }
    secret, _ := encryption.DeriveKey(cfg.Encryption.KDFParams, []byte(passphrase))
    if !encryption.VerifyCanary(cfg.Encryption, secret) { return error("incorrect passphrase for encrypted repository") }  // FR-012, fail-fast
    repository.New(kctx, secret, store, configBytes)
} else {
    if passphrase != "" { warn("repository is not encrypted; passphrase ignored") }
    repository.New(kctx, nil, store, configBytes)
}
```

**Decision**: add a `secret []byte` (derived) path to `plakar.go`'s repo helpers; **Canary** gives the FR-012 fail-fast check. **Alternatives**: attempting `New(nil)` and catching a decrypt error — rejected; the canary is the intended, clean check and we must read `KDFParams` from the repo config to derive the key anyway.

---

## R3. Passphrase source & **secure injection into the runner**

**Decision**:
- The host tool obtains the passphrase from (in order) an explicit flag/file → env var (e.g. `INFRAHUB_BACKUP_PASSPHRASE`). Non-interactive (FR-011).
- **Host-side ops** (repo create, the in-process metadata snapshot, `snapshots list`) use the passphrase directly.
- **Runner ops**: pass the passphrase to the `__run-connector` worker via **stdin** (`docker run -i`, worker reads stdin), NOT via argv and NOT via `-e`. Rationale (FR-007/FR-011): `docker inspect` exposes both the command line and env, so argv/`-e` would leak the secret into persisted container metadata; stdin does not. The worker gets a `--passphrase-stdin` flag and reads one line.

**Alternatives**: `-e INFRAHUB_BACKUP_PASSPHRASE` (rejected — visible in `docker inspect`/env dump); a tmpfs-mounted secret file (viable but more moving parts than stdin); argv `--passphrase` (rejected — visible in `docker inspect` + process list).

---

## R4. Flag/CLI surface

**Decision**: reuse the existing `--encrypt` flag to mean "encrypt the plakar repo", and accept the passphrase via `INFRAHUB_BACKUP_PASSPHRASE` (env) or `--passphrase-file` (path); wire `--encrypt` + the passphrase through `CreateBackup → CreatePlakarBackup` and into `RestorePlakarBackup` + `snapshots list` (all currently drop them on the plakar path). `--encrypt-key` (the legacy ECIES public-key path) is **not** reused for the plakar backend (it's asymmetric/file-level; symmetric passphrase is the model). Document that `--encrypt-key` remains tarball-only.

**Rationale**: minimal new surface; consistent verb (`--encrypt`); non-interactive secret via env/file. **Alternatives**: a brand-new `--passphrase` inline flag (rejected — secrets on argv leak via the process list); interactive prompt only (rejected — breaks unattended/scheduled backups).

---

## R5. Scope confirmations

- **Create-time only**: encryption is fixed when the repo is created (kloset writes `Encryption` into the repo CONFIG). A plaintext repo stays plaintext; no in-place toggle (FR-008, non-goal).
- **Both editions + Postgres**: encryption is a property of the *repository*, orthogonal to which connector writes into it — so neo4j (enterprise/community) + postgres + metadata all inherit it once the repo is encrypted. The runner just needs the passphrase on every op.
- **Compose-first**: matches 003; K8s runner still pending (non-goal here).
- **No cache encryption beyond kloset's**: the local pebble dedup cache is out of scope (non-goal).

---

## Resolved unknowns

| Unknown | Resolution |
|---|---|
| Key model | Passphrase / symmetric (spike: kloset repo encryption is symmetric-only; keypair = ed25519 signing) — R1 |
| Create/open API | `NewDefaultConfiguration` + `DeriveKey` + `DeriveCanary`/`VerifyCanary`; `repository.New(secret)`; detect via `cfg.Encryption` — R2 |
| Wrong-key fail-fast | `VerifyCanary` before read/write (FR-012) — R2 |
| Secure runner injection | passphrase via **stdin** to the worker (not argv/`-e`) — R3 |
| CLI surface | `--encrypt` + `INFRAHUB_BACKUP_PASSPHRASE`/`--passphrase-file`; `--encrypt-key` stays tarball-only — R4 |

No `NEEDS CLARIFICATION` remain.
