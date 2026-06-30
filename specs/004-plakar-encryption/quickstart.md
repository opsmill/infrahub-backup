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

Verified 2026-06-30 — unit tests (`plakar_encryption_test.go`) + live/throwaway E2E:

- [x] **SC-001**: encrypted backup of the live enterprise instance → repo bytes contain none of the known plaintext tokens (`backup_information`, `infrahub_backup_`, `prefect`, `flow_run`, `CoreNode`, …); packfiles opaque. Unit test scans a known marker.
- [x] **SC-002**: throwaway **community** round-trip restored 25 neo4j nodes + 12 postgres rows with content intact; in-process encrypted round-trip recovers exact bytes via the real exporter; **enterprise** backup path validated against the live instance.
- [x] **SC-003 / SC-006**: `snapshots list` without the passphrase → `repository is encrypted; a passphrase is required`; restore with a **wrong** passphrase → `cannot open encrypted repository: incorrect passphrase`, failing at repo-open **before** neo4j was stopped (counts intact). Unit tests assert both error sentinels.
- [x] **SC-004**: passphrase absent from the create log and from the repo bytes; it is never on the runner's argv/`-e` env (only `--passphrase-stdin`), so `docker inspect` of the runner cannot reveal it.
- [x] **SC-005**: plaintext repos open unchanged; a passphrase supplied for a plaintext repo warns and continues (unit test).

### Leak-scan recipe (SC-004)

```bash
# 1. tool logs/stderr must not contain the passphrase
grep -F "$INFRAHUB_BACKUP_PASSPHRASE" create.log            # → no match
# 2. the repo bytes must not contain the passphrase
grep -rF "$INFRAHUB_BACKUP_PASSPHRASE" /tmp/encrepo          # → no match
# 3. while a runner is alive (long backup), its argv/env must not contain it
docker inspect <runner-cid> --format '{{json .Args}} {{json .Config.Env}}'   # → passphrase absent; only --passphrase-stdin
```

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
