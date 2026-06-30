# Feature Specification: Encrypt the Plakar backup backend at rest

**Feature Branch**: `004-plakar-encryption`
**Created**: 2026-06-30
**Status**: Draft
**Input**: User description: "Add at-rest encryption to the Plakar (kloset) backup backend, which is currently plaintext."

## Overview

The reworked Plakar backend (feature 003) writes **plaintext** backup repositories — anyone who obtains the repository (a local directory or an object-store bucket) can read every backed-up database. Feature 003 deferred encryption as an explicit non-goal. The legacy tar.gz backend already encrypts its archive (public-key encryption with an optional custom key and a `keygen` helper); the equivalent flags exist on the backup command but are silently ignored on the Plakar path.

This feature makes Plakar backups **encrypted at rest**: a leaked or stolen repository is unreadable without the key. Encryption is established when the repository is first created and the key must be supplied for every operation against it (backup, restore, and listing/inspection), including when those run inside the co-located runner introduced in 003.

## Clarifications

### Session 2026-06-30

- Q: Which key-management model should the encrypted Plakar repository use? → A: **Keypair (asymmetric).** Backups require only the **public** key (so an unattended/scheduled backup runner never holds the decryption secret); **restore** (and listing/inspection of contents) requires the **private** key. This matches the legacy tarball's public-key UX and gives the strongest posture for unattended backups. Feasibility of the engine's native keypair support is a plan-phase spike; **passphrase (symmetric) is the documented fallback** if keypair is not workable.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Create an encrypted backup (Priority: P1)

An operator runs a backup with encryption enabled. The resulting repository is encrypted: its contents cannot be read or restored without the key. Subsequent backups to the same repository continue to use that encryption.

**Why this priority**: This is the core protection — without it a leaked repository exposes all backed-up data. It is the foundation every other scenario builds on.

**Independent Test**: Run an encrypted backup into a fresh repository; confirm the repository's stored bytes do not reveal database contents (no plaintext dump recoverable) and that the backup group is recorded as complete.

**Acceptance Scenarios**:

1. **Given** encryption is requested and a key is provided, **When** the operator backs up, **Then** an encrypted repository is created and the backup succeeds.
2. **Given** an existing encrypted repository and the correct key, **When** the operator backs up again, **Then** the new snapshots are added under the same encryption.
3. **Given** a stored encrypted repository, **When** it is inspected without the key, **Then** the database contents are not recoverable from the stored bytes.

---

### User Story 2 - Restore from an encrypted backup (Priority: P1)

An operator restores a deployment from an encrypted repository by supplying the key. The databases are restored exactly as a plaintext restore would, for both Neo4j editions and Postgres.

**Why this priority**: An encrypted backup is only valuable if it can be restored; backup and restore must round-trip.

**Independent Test**: Take an encrypted backup, restore it into a clean deployment with the key, and confirm the restored databases match the source.

**Acceptance Scenarios**:

1. **Given** an encrypted repository and the correct key, **When** the operator restores, **Then** Neo4j (Enterprise online and Community offline) and Postgres are restored with data intact.
2. **Given** an encrypted repository and a **missing or wrong** key, **When** the operator restores, **Then** the operation fails fast with a clear "cannot open encrypted repository — key required/incorrect" message and changes nothing.

---

### User Story 3 - List/inspect an encrypted repository (Priority: P2)

An operator lists the backup groups in an encrypted repository. With the key, the listing works as for a plaintext repository; without it, the listing fails clearly rather than showing partial or garbled output.

**Why this priority**: Operators routinely inspect repositories before restoring; this must behave consistently with backup/restore.

**Independent Test**: List an encrypted repository with the key (groups shown) and without it (clear failure).

**Acceptance Scenarios**:

1. **Given** an encrypted repository and the key, **When** the operator lists snapshots, **Then** the backup groups and their status are shown.
2. **Given** no/incorrect key, **When** the operator lists snapshots, **Then** a clear key-required error is returned.

---

### Edge Cases

- **Wrong key against an encrypted repo** — fail fast with a clear message; never partially read or corrupt the repository.
- **Key provided for a plaintext repo, or no key for an encrypted repo** — detect the mismatch and report it clearly (don't silently produce an unusable result).
- **Encryption requested but no key/passphrase available** — refuse to start rather than create a repository the operator can't reproduce the key for.
- **Key material exposure** — the key/passphrase must never appear in logs, error messages, or persisted process metadata, including where it is handed to the co-located runner.
- **Mixing**: targeting an existing encrypted repository for a new backup without the key — fail clearly (cannot append to an encrypted repo blindly).

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The tool MUST be able to create an **encrypted** Plakar repository when encryption is requested, establishing the encryption at repository-create time.
- **FR-002**: The tool MUST supply the key material for **every** operation against an encrypted repository — create, backup, restore, and list/inspection — including when the operation runs inside the co-located runner.
- **FR-003**: A full backup→restore round-trip against an encrypted repository MUST preserve data for **both** Neo4j editions (Enterprise online, Community offline) and Postgres.
- **FR-004**: The tool MUST wire encryption into the Plakar **create** and **restore** paths (the existing encryption flags are currently ignored on the Plakar path), plus a key/passphrase input suitable for unattended use (e.g., an environment variable or file, not only an interactive prompt).
- **FR-005**: Opening an encrypted repository **without** the correct key (for restore, listing, or appending a backup) MUST fail fast with a clear, actionable message and make no changes.
- **FR-006**: Requesting encryption **without** available key material MUST be refused before any repository is created.
- **FR-007**: The key/passphrase MUST NOT be written to logs, error output, or persisted process/command metadata — including when injected into the runner.
- **FR-008**: Plaintext repositories MUST continue to work unchanged; encryption is chosen at create time and is **not** applied retroactively to existing plaintext repositories.
- **FR-009**: Encryption MUST be optional — backups remain plaintext unless encryption is explicitly requested (no behavior change for existing 003 users who don't opt in).
- **FR-010**: The key-management model MUST be **asymmetric (keypair)**: encryption is established with a **public** key at create time; **backup** requires only the public key; **restore** and content listing/inspection require the **private** key. (If a plan-phase spike shows the engine's keypair support is unworkable, the documented fallback is a symmetric passphrase — recorded as a deviation.)
- **FR-011**: An unattended **backup** MUST be possible with only the **public** key present (the backup runner MUST NOT require the decryption/private key).
- **FR-012**: The tool MUST provide a way to obtain a keypair for backup encryption (generate one, and/or accept a provided public key), consistent with how the legacy backend's `keygen`/`--encrypt-key` works.

### Key Entities *(include if data involved)*

- **Encrypted repository**: a Plakar repository whose stored contents are encrypted; its encryption is fixed at create time and recorded in the repository's own configuration.
- **Key material**: an asymmetric **key pair** — the **public** key encrypts (used at create + every backup); the **private** key decrypts (used for restore + content inspection). The private key is the sensitive secret and is needed only at restore time.
- **Runner**: the co-located one-shot context (from 003) that performs backup/restore; it receives the **public** key for backup and the **private** key for restore, securely (never logged).

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: An encrypted repository's stored bytes contain **no recoverable plaintext** of the backed-up databases (verified by inspecting the repository without the key).
- **SC-002**: An encrypted backup→restore round-trip restores **both** Neo4j editions and Postgres with data matching the source in 100% of test runs.
- **SC-003**: Attempting to restore, list, or append-backup an encrypted repository **without** the correct key fails with a clear error and zero changes, in 100% of test runs.
- **SC-004**: The key/passphrase never appears in tool logs, error messages, or the runner's command line / persisted metadata (verified by scanning output + process/command inspection).
- **SC-005**: Existing plaintext (003) repositories and the no-encryption path continue to work unchanged (no regression for users who don't opt in).
- **SC-006**: An encrypted backup completes with **only the public key** available (the private/decryption key is never required to back up), verified by running a backup in an environment that has no access to the private key.

## Assumptions & Dependencies

- **Builds on 003**: depends on the reworked Plakar backend + the co-located runner (branch `003-upstream-plakar-integrations`). Encryption is wired through that same backup/restore/runner flow.
- **Native repository encryption**: uses the backup engine's built-in repository encryption (key material supplied when the repository is opened), rather than encrypting files after the fact. The engine offers symmetric (passphrase-derived) and asymmetric (keypair) modes; this feature targets the **keypair** mode, with a plan-phase spike to confirm it is usable (and passphrase as the fallback).
- **Encryption at create time**: a repository is either encrypted or plaintext from creation; switching requires creating a new repository.
- **Docker Compose first**: consistent with 003, which targets Compose; the Kubernetes runner remains pending and is out of scope here.
- **Unattended-friendly key input**: key material can be supplied non-interactively (env/file) so scheduled backups work.

## Out of Scope (Non-Goals)

- Changing the legacy tar.gz backend's existing encryption.
- In-place re-encryption or key rotation of an existing repository (create a new encrypted repository instead).
- Encrypting the local dedup/cache state beyond what the backup engine already does.
- Kubernetes support (the 003 K8s runner is still pending).
