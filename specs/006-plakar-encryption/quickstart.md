# Quickstart: encrypted Plakar backups, and the extracted Neo4j integration

**Feature**: `006-plakar-encryption` (builds on 003)

Workstream A (encryption) is operator-facing and **done**. Workstream B (integration extraction) is maintainer-facing and **planned** — its commands below are the intended sequence, not a record of completed runs.

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

### Re-verified 2026-08-10, after the `main` merge and the runner fixes

The runner changed materially since the June run (T060/T061: `fs://` classification, and the
image entrypoint that was silently dropping `--user root`), so the encrypted round-trip was
re-run rather than assumed to still hold:

```bash
ENCRYPT=1 REPO_SPELLING=uri ./test/e2e/run-e2e.sh community      # → RESULT: PASS
```

- **SC-002 re-confirmed** against a real Neo4j 2025.10.1 Community and a real PostgreSQL 18:
  encrypted repository created, three component snapshots (neo4j, postgres, metadata), wipe
  **gated at 0/0**, restore recovered 25 neo4j nodes and 12 postgres rows.
- **SC-003 re-confirmed**: listing without the passphrase → `repository is encrypted; a
  passphrase is required`.
- Run over the **`fs://` spelling** on purpose. Every earlier recipe used a bare path, and
  that is exactly why T060 went unnoticed — the URI form the README recommends was broken.

**What this run does not cover**: the harness replaces `infrahub-server`, `task-manager` and
`task-worker` with `alpine sleep infinity`, so it exercises the data plane only. It cannot
show whether the *Infrahub server* recovers its Bolt connections after the offline backup
suspends Neo4j — which is the open failure in `main`'s own e2e suite (T064).

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

> **Automated.** This recipe is implemented by [`test/e2e/run-e2e.sh`](../../test/e2e/run-e2e.sh),
> which brings up its own throwaway stack, seeds it, and asserts the wipe emptied
> both databases before restoring. Prefer it over the manual steps below:
>
> ```bash
> ./test/e2e/run-e2e.sh community          # neo4j+offline://
> ./test/e2e/run-e2e.sh enterprise         # neo4j://
> ENCRYPT=1 ./test/e2e/run-e2e.sh community
> ```
>
> The manual commands that follow assume a deployment you already have; the
> `restoretest` project name is a placeholder for whatever yours is called.

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

---

## Maintainer — extract the Neo4j integration (workstream B, planned)

### 1. Seed the new repository from the fork's subtree

```bash
# The fork's neo4j/ subtree is the complete source — contrib/ lacks plugin/, tests/, LICENSE, CI
git clone --branch integration/neo4j --single-branch \
    https://github.com/BeArchiTek/integrations.git /tmp/fork
mkdir /tmp/plakar-integration-neo4j
cp -R /tmp/fork/neo4j/. /tmp/plakar-integration-neo4j/
cd /tmp/plakar-integration-neo4j

# Module path: github.com/PlakarKorp/integration-neo4j -> github.com/opsmill/plakar-integration-neo4j
#   go.mod module line + the two plugin/ entrypoint imports
# manifest.yaml: tier official->third-party, homepage/contact -> OpsMill  (see contracts/neo4j-integration-repo.md)
# LICENSE: add the OpsMill 2026 copyright line alongside PlakarKorp 2025
# .github/workflows/test.yml: drop the monorepo `neo4j/` working-directory assumption
```

### 2. Verify before publishing (cheaper than after)

```bash
go build ./... && go vet ./...
make build                      # must yield neo4jImporter AND neo4jExporter
make test                       # testcontainers; needs a quiet Docker host
plakar pkg create ./manifest.yaml v0.1.0

# Manifest/entrypoint agreement — the defect contrib/ carried (VR-8).
# Every declared executable must have a plugin dir, and vice versa:
grep -E '^\s+executable:' manifest.yaml | awk '{print $2}' | sort > /tmp/declared
ls plugin/ | sed 's/neo4j-importer/neo4jImporter/; s/neo4j-exporter/neo4jExporter/' | sort > /tmp/built
diff /tmp/declared /tmp/built   # must be empty
```

### 3. Publish with fresh history

```bash
rm -rf .git && git init && git add -A
git commit -m "feat: Neo4j integration for Plakar (Enterprise online + Community offline)"
gh repo create opsmill/plakar-integration-neo4j --public --source=. --push
git tag v0.1.0 && git push origin v0.1.0
```

### 4. Rewire this repository

```bash
cd ~/automation/opsmill/infrahub-backup
git rm -r contrib/integration-neo4j

# go.mod: drop `replace github.com/PlakarKorp/integration-neo4j => ./contrib/integration-neo4j`
go get github.com/opsmill/plakar-integration-neo4j@v0.1.0
# src/internal/app/connectors.go: retarget the two blank imports + the scheme-mapping comment
go mod tidy
./scripts/update-vendor-hash.sh        # mandatory on any go.mod change (CLAUDE.md)

make build && make test && make lint && make vet
```

### 5. Verify the dependency boundary

```bash
# SDK and testcontainers serve only plugin/ and tests/ — they must not enter this build (SC-010)
go mod graph | grep -E 'go-kloset-sdk|testcontainers'   # expect: no match
grep -n 'replace' go.mod                                 # expect: no integration replace
go list -m github.com/opsmill/plakar-integration-neo4j   # expect: v0.1.0
```

### 6. Re-run both Neo4j round-trips through the external module

```bash
# Both editions, plus the encrypted variant. Each run builds its own throwaway
# stack and asserts the wipe emptied the databases before restoring.
./test/e2e/run-e2e.sh community            # neo4j+offline:// (offline dump/load)
./test/e2e/run-e2e.sh enterprise           # neo4j:// (online backup/restore)
ENCRYPT=1 ./test/e2e/run-e2e.sh community  # encrypted repository
```

### 7. Author the hub recipe (submit only after steps 2–3 pass)

```bash
# community/v1.1.0/neo4j/recipe.yaml in a fork of PlakarKorp/hub
cat <<'YAML'
name: neo4j
version: v0.1.0
repository: https://github.com/opsmill/plakar-integration-neo4j
YAML
```

### Validation checklist — workstream B (maps to Success Criteria)

Not yet run; each maps to a gate in `plan.md`.

- [ ] **SC-007**: clean checkout builds, vets, tests green; a plugin executable exists for every declared connector, none declared but missing.
- [ ] **SC-008**: this repo builds/tests/lints/vets clean; no `replace`; no in-tree copy.
- [ ] **SC-009**: `neo4j://` and `neo4j+offline://` both round-trip, matching the 003 results.
- [ ] **SC-010**: `go mod graph` free of `go-kloset-sdk` and `testcontainers`.
- [ ] **SC-011**: no stray `PlakarKorp/integration-neo4j` or fork references outside deliberate history.
- [ ] **SC-012**: manifest states third-party tier + OpsMill contact; `LICENSE` credits both parties.
