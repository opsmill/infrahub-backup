# Quickstart: encrypted Plakar backups

**Feature**: `004-plakar-encryption` (builds on 003)

## Operator — encrypted backup & restore

```bash
# Encrypted backup: --encrypt + a passphrase (non-interactive via env or file)
export INFRAHUB_BACKUP_PASSPHRASE='correct horse battery staple'
infrahub-backup create --backend plakar --repo fs:///backups/infra --encrypt
#   → creates an ENCRYPTED kloset repo (Argon2id + AES256-GCM-SIV); neo4j + postgres + metadata

# List requires the passphrase for an encrypted repo
INFRAHUB_BACKUP_PASSPHRASE='…' infrahub-backup --backend plakar --repo fs:///backups/infra snapshots list

# Restore requires the passphrase
INFRAHUB_BACKUP_PASSPHRASE='…' infrahub-backup restore --backend plakar --repo fs:///backups/infra <backup-id>

# Passphrase from a file instead of env
infrahub-backup create --backend plakar --repo fs:///backups/infra --encrypt --passphrase-file /run/secrets/backup-pass
```

Notes: the passphrase is required for every operation on an encrypted repo and is **never** logged or placed on a container command line. `--encrypt-key` stays tarball-only. A plaintext repo is unaffected (no `--encrypt`, no passphrase needed).

## Validation checklist (maps to Success Criteria)

- [ ] **SC-001**: inspect the encrypted repo's bytes without the passphrase → no recoverable DB content.
- [ ] **SC-002**: encrypted backup→restore round-trip restores neo4j (enterprise + community) and postgres with data intact.
- [ ] **SC-003 / SC-006**: restore/list/append with a **wrong or absent** passphrase → clear canary-verified failure, no changes.
- [ ] **SC-004**: scan tool logs + `docker inspect` of the runner → passphrase not present.
- [ ] **SC-005**: a 003 plaintext repo + the no-`--encrypt` path still work unchanged.

## Test recipe (throwaway Infrahub, like 003)

```bash
# 1. encrypted backup of the throwaway
INFRAHUB_BACKUP_PASSPHRASE=testpass infrahub-backup create --backend plakar --repo /tmp/encrepo --project restoretest --force
# 2. negative: list without the passphrase → must fail clearly
infrahub-backup --backend plakar --repo /tmp/encrepo snapshots list      # expect: "repository is encrypted; passphrase required"
# 3. wipe + restore with the passphrase → data back
INFRAHUB_BACKUP_PASSPHRASE=testpass infrahub-backup restore --backend plakar --repo /tmp/encrepo --project restoretest --force
# 4. negative: restore with a wrong passphrase → canary failure, no changes
INFRAHUB_BACKUP_PASSPHRASE=wrong infrahub-backup restore --backend plakar --repo /tmp/encrepo --project restoretest --force
```
