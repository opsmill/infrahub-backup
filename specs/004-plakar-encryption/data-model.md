# Data Model: Encrypt the Plakar backend at rest

**Feature**: `004-plakar-encryption` · **Date**: 2026-06-30

No application schema is introduced. The entities are the repository's encryption configuration and the runtime key material.

## Entities

### Encrypted repository
A kloset repository whose stored contents are symmetric-encrypted.
- **Attributes**: location (`fs://`/`s3://`); `encrypted` (bool); the engine **Encryption configuration** (below) persisted in the repo CONFIG at create.
- **State**: created plaintext **or** encrypted — fixed at create; not toggleable in place (FR-008).
- **Relationships**: holds the same component snapshots (neo4j/postgres/metadata) as a plaintext repo; encryption is orthogonal to the connectors.

### Encryption configuration (in the repo CONFIG)
The engine's symmetric encryption parameters, written when the repo is created.
- **Attributes**: `KDFParams` (default Argon2id), `SubKeyAlgorithm` (AES256-KW), `DataAlgorithm` (AES256-GCM-SIV), `Canary` (a value encrypted with the derived key, used to verify a supplied passphrase).
- **Source**: `encryption.NewDefaultConfiguration()`; `Canary = DeriveCanary(config, secret)` at create.
- **Identity**: the KDF salt/params make each repo's derived key unique even for the same passphrase.

### Passphrase (key material)
The operator secret.
- **Attributes**: the passphrase string (never persisted by the tool, never logged).
- **Derivation**: `secret = DeriveKey(KDFParams, passphrase)` → the symmetric repository key.
- **Sources**: env `INFRAHUB_BACKUP_PASSPHRASE` or `--passphrase-file <path>`; required when `--encrypt` is set and when opening an encrypted repo.
- **Lifecycle**: held in memory by the host tool; passed to the runner via stdin for the duration of one op; never written to argv/env/logs.

### Runner (from 003)
- **Receives**: the passphrase on **stdin** (`--passphrase-stdin`) for any op against an encrypted repo; derives the secret + verifies the canary inside the container.

## Validation rules

- **VR-1**: `--encrypt` without an available passphrase ⇒ refuse before creating anything (FR-006).
- **VR-2**: opening an encrypted repo without a passphrase, or with one that fails `VerifyCanary`, ⇒ fail fast, no read/write (FR-005, FR-012).
- **VR-3**: a passphrase supplied for a **plaintext** repo ⇒ warn + ignore (don't error the whole op).
- **VR-4**: the passphrase MUST NOT appear in logs, error text, the runner command line, `-e` env, or `docker inspect` output (FR-007, FR-011).
- **VR-5**: encryption applies to every component (neo4j enterprise/community, postgres, metadata) once the repo is encrypted, and to create/backup/restore/list alike (FR-002, FR-003).

## State transitions (encrypted backup/restore)

```
create:  --encrypt + passphrase -> NewDefaultConfiguration() + DeriveKey + DeriveCanary -> storage.Create(encrypted)
open:    storage.Open -> cfg.Encryption? -> DeriveKey(KDFParams, passphrase) -> VerifyCanary -> repository.New(secret)
backup:  host derives nothing for the runner; passphrase -> runner stdin -> worker derives secret -> writes encrypted snapshot
restore: passphrase -> runner stdin -> worker opens encrypted repo (canary-verified) -> exporter restores
```
