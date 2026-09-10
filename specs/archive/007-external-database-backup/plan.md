# Implementation Plan: External Database Backup and Restore

**Branch**: `007-external-database-backup` | **Date**: 2026-09-02 | **Spec**: [spec.md](./spec.md)

**Input**: Feature specification from `/specs/007-external-database-backup/spec.md`

## Summary

Infrahub deployments whose Neo4j Enterprise or PostgreSQL databases run outside the Kubernetes
namespace cannot be backed up, because every backup and restore path executes the vendor tooling
*inside* the database container and no container exists. The fix is to supply one: create a
transient pod in the namespace that can reach the external endpoint, register it under the
canonical service name, and let every existing execution, streaming and copy primitive work
unchanged.

Phase 0 established that this is the vendor's own documented topology — a *backup client*, a
machine on the database's network but not in its cluster, with the administration tooling
installed. It also confirmed remote restore is available by seeding a database from an object-store
URI over the client protocol, and produced six findings that changed the spec (see
[research.md](./research.md#spec-impact)), of which the most consequential is that seeding
validates only when the database starts — so an accepted restore is not a completed one.

## Technical Context

**Language/Version**: Go, version pinned in `go.mod` (currently 1.25.0)

**Primary Dependencies**: cobra, viper, logrus, kloset (Plakar core), integration-fs. **No new Go
modules** — deliberately. Kubernetes and the databases are reached by shelling out to `kubectl`
through the existing `CommandExecutor`, per ADR-0001, and the server version is probed with the
client shipped in the transient pod's image rather than a vendored driver.

**Storage**: Plakar repository (filesystem or S3) as today, unchanged. No new persistent store; the
new entities are run-scoped configuration plus one transient cluster object.

**Testing**: `make test` for table-driven unit tests (endpoint resolution, version comparison
across both numbering schemes, metadata compatibility, completeness derivation); the Python
end-to-end suite under `tests/e2e/` for flows that touch containers, extended with
external-topology fixtures alongside the existing `test_k8s_*.py` cases.

**Target Platform**: Kubernetes for this feature. The binaries themselves continue to
cross-compile for linux/darwin/windows on amd64/arm64.

**Project Type**: CLI toolset — three thin cobra entry points over a shared core. This feature
touches `infrahub-backup` only.

**Performance Goals**: No latency target. The relevant budget is resource-shaped: the backup
command is documented as CPU- and memory-intensive and needs local scratch space at least the size
of the database, which is why the transient pod's resources are explicit rather than defaulted
(FR-022).

**Constraints**: No new inbound network exposure at the database (SC-002). Nothing destructive
without explicit authorisation (FR-009). No artifact may be presented as complete when it is not
(FR-012). Artifact layout and metadata additive only (FR-016). No new assumed deployment service
name (FR-017).

**Scale/Scope**: Two logical databases, resolved independently; one to several endpoints each. The
change is concentrated in environment detection, configuration resolution and the restore path —
the ~93 call sites that name the two database services are expected to remain untouched, and that
being true is the design's main success indicator.

## Constitution Check

*GATE: evaluated against `.specify/memory/constitution.md` v1.1.0.*

### Pre-Phase 0

| Principle | Assessment |
|---|---|
| **I. Split-Binary, Shared-Core** | PASS. All logic lands in `src/internal/app`; `src/cmd/infrahub-backup` gains flag registration only. No new binary, so no amendment needed. No helper executable added. |
| **II. Operational Safety & Data Integrity (NON-NEGOTIABLE)** | PASS with obligations, and this feature is squarely inside this principle. Backup stays non-destructive to the running instance. Restore's quiescing already restarts what it stopped; the transient workload joins that discipline (FR-011, FR-013). Metadata and checksums are additive with prior-version artifacts still restorable (FR-016). The destructive restore requires explicit authorisation — and this feature goes beyond the principle's "confirmation **or** a bypass flag" by requiring an authorisation that neither `--force` nor scheduled restore confers (FR-009), because the target is infrastructure the tool does not manage. |
| **III. Self-Contained, Cross-Platform Binaries** | PASS, and this principle decided the design. It permits depending on runtimes already present in the target environment while requiring actionable detection of their absence. The alternative of running the vendor tooling on the operator's host would require a JVM on machines the principle explicitly describes as having "no toolchain or package manager" — so it is deferred to User Story 3. `CGO_ENABLED=0` and cross-compilation are unaffected; no new embedded assets. |
| **IV. Quality Gates** | PASS. Behaviour change is accompanied by unit tests for the pure logic and end-to-end coverage for container-touching flows, per [quickstart.md](./quickstart.md). |
| **V. Explicit Errors & Streaming Feedback** | PASS with an obligation. Errors wrapped with `%w`, no `os.Exit` in the core. Streaming is preserved because the existing stream primitives are reused rather than replaced. The obligation is specific: a vendor exit status that conflates failure with partial success must not be surfaced as-is (research R6), and the wording contract in [contracts/cli-surface.md](./contracts/cli-surface.md) exists to satisfy SC-008. |

**Operational constraints**:

- *Deployment contract*: the documented service names are a public contract, and adding an assumed
  service is a breaking change. The transient workload registers under an existing name, so no new
  name is introduced (FR-017). **Compliant.**
- *Backup engine*: snapshot layout and metadata changes must preserve restorability of
  previously released artifacts. Additive only, absence of the new fields meaning exactly what a
  pre-feature artifact is. **Compliant, no migration path required.**
- *Nix packaging*: no `go.mod`/`go.sum` change is anticipated, so no vendor hash refresh. If a
  dependency does become necessary, `scripts/update-vendor-hash.sh` must be run — never
  hand-edited.
- *Documentation*: user-facing docs under `docs/docs/backup/` follow Diataxis with Vale and rumdl
  passing. The customer-side prerequisites are the load-bearing content.

**Gate result: PASS.** No violations, so Complexity Tracking stays empty.

### Post-Phase 1 re-check

Re-evaluated after the design artifacts. Still PASS, with two things the design added that the
constitution demanded and the spec had not yet stated:

- Principle II drove **FR-021** (confirm the restored database reached a usable state). Without it
  the tool would report a successful restore on an artifact that silently failed to load — the
  precise failure this principle rules out, and it is invisible to the pre-flight in FR-007 because
  it happens after the destructive step.
- Principle II also drove **FR-022** (explicit scratch sizing). A backup that fills a node's disk
  damages the running instance, which the principle's first clause forbids.

No new violation was introduced by Phase 1. One requirement correction is outstanding and needs the
feature owner's agreement: FR-006 asserts a symmetric version-matching rule that the vendor
documentation does not support (research R3, spec-impact item 6). It is recorded, not silently
applied.

## Project Structure

### Documentation (this feature)

```text
specs/007-external-database-backup/
├── plan.md                             # This file
├── spec.md                             # Feature specification
├── research.md                         # Phase 0 output
├── data-model.md                       # Phase 1 output
├── quickstart.md                       # Phase 1 output
├── checklists/
│   └── requirements.md                 # Spec quality checklist
├── contracts/
│   ├── cli-surface.md                  # Flags, config, exit behaviour, failure wording
│   └── artifact-and-metadata.md        # Artifact layout and metadata compatibility
└── tasks.md                            # Phase 2 output (/speckit-tasks — NOT created here)
```

### Source Code (repository root)

```text
src/
├── cmd/
│   └── infrahub-backup/
│       └── main.go                     # Flag registration only (Principle I)
└── internal/
    └── app/
        ├── cli.go                      # New flags, viper binding
        ├── app_config.go               # Read the address/port/protocol/TLS settings
        │                               #   currently discarded
        ├── environment_kubernetes.go    # errNoPodsMatched discrimination on the
        │                               #   singular pod resolver; transient-pod
        │                               #   create/adopt/reap; podCache registration
        ├── external_endpoint.go         # NEW — endpoint resolution, version compare
        ├── transient_workload.go        # NEW — manifest, lifecycle, reaping, secret
        ├── backup_neo4j.go              # Remote source arguments; completeness from
        │                               #   reported output, not exit status
        ├── backup_taskmanager.go        # Remote host instead of localhost
        ├── backup_metadata.go           # Additive metadata fields
        ├── backup.go                    # Restore ordering, authorisation, online check
        └── retention.go                 # Skip artifacts marked incomplete

tests/
└── e2e/
    ├── conftest.py                     # External-topology fixtures
    ├── test_k8s_external_neo4j.py      # NEW
    └── test_k8s_external_both.py       # NEW

docs/
└── docs/
    └── backup/                          # Prerequisites, flags, restore authorisation
```

**Structure Decision**: The existing single-project layout is kept unchanged. Two new files in
`src/internal/app` carry the genuinely new concepts — endpoint resolution and transient-workload
lifecycle — while every other change is an edit to the file that already owns that concern. This
follows Principle I (all logic in the shared core, entry points thin) and keeps the feature's
footprint aligned with its shape: the new behaviour is concentrated in resolution and lifecycle,
and the execution paths above them are deliberately left alone.

## Complexity Tracking

No constitution violations, so no justifications are required.

| Violation | Why Needed | Simpler Alternative Rejected Because |
|-----------|------------|-------------------------------------|
| *(none)* | | |

### T071: the one edited call site naming a database service

The design's success indicator is that the existing call sites naming `database` and
`task-manager-db` are **not** edited — an edit means the stand-in-pod approach leaked into the
in-deployment path instead of dispatching ahead of it. T071 audited that against the branch base
`8186fcf`. One edit exists, and this is it.

**Criterion used.** A *call site naming the service* is a line in non-test Go under `src/` where
`"database"` or `"task-manager-db"` sits in the **service-name argument position** — the first
argument to `Exec`, `ExecWithOptions`, `ExecStream*`, `CopyFrom`, `IsRunning` and the like. The
same word elsewhere is not a call site: `neo4j-admin database backup` is a subcommand, `database`
is also a directory inside the artifact and a metadata key, and a comment mentioning the word
names nothing. Both the narrow and the wide readings are reported below so the number cannot be
the criterion's doing.

**Counts.**

| Measure | Base | Edited |
|---|---|---|
| Non-test `src/` lines containing either literal | 93 | 3 |
| Of those, lines with the literal in the service-argument position | 48 | 1 |
| Service-argument lines whose **service name** changed | 48 | 0 |
| Call sites deleted, rerouted, or switched to the new constants | 48 | 0 |
| Enclosing functions of the 93 sites whose body was edited | 33 | 15 |

**The one edit.** In `backupNeo4jEnterpriseStream` and `backupNeo4jEnterprise`
(`src/internal/app/backup_neo4j.go`), the argv handed to `iops.Exec("database", …)` was replaced
by `neo4jCaptureCommand(neo4jCaptureRequest{…})`. The service argument `"database"` is
byte-identical; only the second argument changed. It was necessary because the external capture
runs *the same* `neo4j-admin database backup` invocation from the transient workload, and FR-015
plus `contracts/artifact-and-metadata.md` require an artifact taken from an external database to
be restorable into an internal deployment and back. Leaving the argv inline would have made that
a coincidence between two literal lists rather than a shared fact; the alternative — a second
copy of the flag list in the external path — is the drift the shared constructor exists to
prevent. Two further removed lines are the `neo4j-admin` subcommand word `database` moving into
that same constructor, and are not service names.

**The wider reading, stated plainly.** 15 of the 33 enclosing functions were edited, including
`stopAppContainers`' callers, `redactDatabase`, `isNeo4jCluster`, `RestoreBackup` and the Plakar
restore paths. Every one of those edits *inserts* something ahead of the existing calls — an
`externalDatabaseFor` gate that returns early, FR-013 restart handling, FR-012's incomplete
capture refusal — or changes a signature to carry an error. None rewrites how a service is
named or which service is addressed. That is the shape the indicator was asking for, and the
single argv edit above is the one place it is not literally true.
