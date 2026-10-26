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
| `--encrypt-key` | **tarball-only** (legacy ECIES). With `--backend plakar` it is **rejected with an error** redirecting to `--encrypt` + `INFRAHUB_BACKUP_PASSPHRASE` (never silently ignored). |

Resolution order for the passphrase: `--passphrase-file` → `INFRAHUB_BACKUP_PASSPHRASE`. Wired through `create`, `restore`, and `snapshots list` (all currently drop encryption on the plakar path).

**Validation**: on `create --encrypt`, reject a passphrase shorter than **12 characters** before creating the repository (FR-013).

## Runner key injection (secure)

The `__run-connector` worker gains `--credentials-stdin`:
```
infrahub-backup __run-connector backup|restore <repo> <uri> [snap] --credentials-stdin [--s3-insecure] [--opt …] [--tag …]
```
- The orchestrator launches the runner with `docker run -i …` and **writes one JSON object to the container's stdin**, then closes it: `{"passphrase":…,"db_password":…,"s3_access_key":…,"s3_secret_key":…}`. JSON rather than `key=value` lines so a value containing a newline or an `=` cannot be misread.
- The worker reads it (only when `--credentials-stdin` is set) and uses the passphrase to open the repo, the database password as the connector's standalone `password` option, and the S3 pair as the storage credentials.
- **MUST NOT** pass any of them as a CLI arg or `-e` env var (both are visible in `docker inspect` / the process list). This covers the database password — which used to travel inside the connector URI on argv — and the object-store credentials, which used to travel both in an `s3://key:secret@…` argv URI and as `-e AWS_SECRET_ACCESS_KEY`. Only non-secrets stay on the command line: the credential-free URI, `--s3-insecure` (which records that the URI *had* carried credentials, so stripping them does not silently flip a plain-HTTP store to TLS), and `INFRAHUB_S3_ENDPOINT` as `-e`.
- Host-side ops (repo create, metadata snapshot, `snapshots list`) use the credentials directly in-process.

## Guarantees

- **G1**: encrypted repo bytes reveal no plaintext database content without the passphrase (SC-001).
- **G2**: wrong/absent passphrase ⇒ canary-verified fail-fast, zero changes (SC-003, SC-006).
- **G3**: no secret — passphrase, database password, or object-store credentials — ever in logs / argv / `-e` / `docker inspect` (SC-004).
- **G4**: plaintext repos and the no-`--encrypt` path are unchanged (SC-005).
