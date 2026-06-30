# Contract: Plakar repository encryption + key handling

## Repository encryption (kloset symmetric)

**Create (encrypted)** — `openOrCreateRepo` when `--encrypt`:
```go
enc := encryption.NewDefaultConfiguration()                  // Argon2id + AES256-GCM-SIV
secret, err := encryption.DeriveKey(enc.KDFParams, []byte(passphrase))
enc.Canary, err = encryption.DeriveCanary(enc, secret)
storageConfig.Encryption = enc                               // was nil (plaintext) in 003
// …serialize + storage.Create as in 003…
repo, err := repository.New(kctx, secret, store, configBytes) // was secret = nil
```

**Open** — `openRepo`/`openOrCreateRepo`:
```go
store, configBytes, _ := storage.Open(kctx, sc)
cfg := /* storage.NewConfigurationFromBytes(version, unwrapped) */
switch {
case cfg.Encryption == nil && passphrase == "":  repository.New(kctx, nil, store, configBytes)
case cfg.Encryption == nil && passphrase != "":   warn("repo not encrypted; passphrase ignored"); repository.New(kctx, nil, …)
case cfg.Encryption != nil && passphrase == "":   return errEncryptedRepoNeedsPassphrase
case cfg.Encryption != nil:
    secret, _ := encryption.DeriveKey(cfg.Encryption.KDFParams, []byte(passphrase))
    if !encryption.VerifyCanary(cfg.Encryption, secret) { return errWrongPassphrase }  // FR-012
    repository.New(kctx, secret, store, configBytes)
}
```
Both error returns are clear, actionable, and make **no** repository changes (FR-005).

## CLI / config surface

| Input | Meaning |
|---|---|
| `--encrypt` (create) | create the plakar repo encrypted (no-op if the repo already exists with its own setting) |
| `INFRAHUB_BACKUP_PASSPHRASE` (env) | the passphrase (create/backup/restore/list) |
| `--passphrase-file <path>` | read the passphrase from a file (first line) |
| `--encrypt-key` | **tarball-only** (legacy ECIES); NOT used by the plakar backend — documented |

Resolution order for the passphrase: `--passphrase-file` → `INFRAHUB_BACKUP_PASSPHRASE`. Wired through `create`, `restore`, and `snapshots list` (all currently drop encryption on the plakar path).

## Runner key injection (secure)

The `__run-connector` worker gains `--passphrase-stdin`:
```
infrahub-backup __run-connector backup|restore <repo> <uri> [snap] --passphrase-stdin [--opt …] [--tag …]
```
- The orchestrator launches the runner with `docker run -i …` and **writes the passphrase to the container's stdin** (one line), then closes it.
- The worker reads the passphrase from stdin (only when `--passphrase-stdin` is set) and uses it to open the repo.
- **MUST NOT** pass the passphrase as a CLI arg or `-e` env var (both are visible in `docker inspect` / the process list). Host-side ops (repo create, metadata snapshot, `snapshots list`) use the passphrase directly in-process.

## Guarantees

- **G1**: encrypted repo bytes reveal no plaintext database content without the passphrase (SC-001).
- **G2**: wrong/absent passphrase ⇒ canary-verified fail-fast, zero changes (SC-003, SC-006).
- **G3**: passphrase never in logs / argv / `-e` / `docker inspect` (SC-004).
- **G4**: plaintext repos and the no-`--encrypt` path are unchanged (SC-005).
