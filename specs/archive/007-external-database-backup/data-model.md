# Phase 1 Data Model: External Database Backup and Restore

**Feature**: 007-external-database-backup | **Date**: 2026-09-02

This feature adds no persistent storage. The entities below are configuration resolved at run
time, one transient cluster object, and additive fields on the existing backup metadata.

---

## `DatabaseEndpoint` (new, in-memory)

Where a logical database actually lives, resolved once per run per database and thereafter the
single source of truth for both the backup and restore paths.

| Field | Type | Source | Validation |
|---|---|---|---|
| `Service` | string | Caller | One of the documented deployment service names (`database`, `task-manager-db`). Never a new name (FR-017). |
| `Location` | enum `internal` \| `external` | Resolved | `external` only on a positively-empty container match; a query failure is an error, never `external` (FR-002). |
| `Hosts` | `[]HostPort` | `INFRAHUB_DB_ADDRESS`, or the Prefect connection URL, or flag override | At least one entry when `Location` is `external`. Order is significant — it is the order the backup command will try (FR-005). |
| `ClientPort` | int | `INFRAHUB_DB_PORT` (Bolt, default 7687) / connection URL | Used for version probing and for restore. |
| `BackupPort` | int | Defaulted to 6362; flag override | Not discoverable — Infrahub does not record it. |
| `Protocol` | string | `INFRAHUB_DB_PROTOCOL` | `bolt` or `neo4j`; the routed form is required for multi-member deployments. |
| `TLS` | `TLSSettings` | `INFRAHUB_DB_TLS_*` | Applies to the client connection, independently of the backup channel's own policy. |
| `Database` | string | `INFRAHUB_DB_DATABASE` / connection URL | Names the artifact entry, so it must be carried through rather than assumed (a cross-name restore trap already fixed once in this codebase). |
| `Credentials` | reference | Existing credential resolution | Never rendered into a command line or into workload configuration (FR-014). |

`HostPort` is `{Host string; Port int}`, where a member given without an explicit port inherits
`ClientPort` — mirroring how Infrahub itself interprets its address list.

**Lifecycle**: resolved during environment detection, immutable for the rest of the run, and
reported before any connection is opened (FR-003).

---

## `TransientWorkload` (new, one cluster object per run per external database)

The execution context that stands in for the absent database container. It exists so that every
existing execution, streaming and copy primitive continues to work unchanged.

| Field | Type | Constraint |
|---|---|---|
| `RunID` | string | Unique per run. Labelled onto the workload so a later run can identify strays (FR-011). |
| `Service` | string | The canonical service name it stands in for, so it resolves under the existing name (FR-017). |
| `Image` | string | Pinned default, overridable by explicit flag only — never from ambient environment (FR-004, FR-027). |
| `SecurityContext` | fixed | Must satisfy a restricted pod-security policy, reconciled with the vendor's requirement that the backup tooling run as the database's own user (FR-027). |
| `Deadline` | duration | Set on the object itself, so the cluster reclaims it if the tool never returns (FR-011). |
| `ScratchSize` | size | At least the size of the database being captured (FR-022). |
| `CredentialRef` | reference | A secret **owned by** the workload — its lifetime is bound to the workload's, so the cluster's garbage collector reclaims it when the workload goes (FR-023). Same `RunID` label. |
| `State` | enum | `pending` → `ready` → `released`. Only `ready` may be used as an execution target. |

**Invariants**:

- Never outlives its run on any exit path, including abnormal termination (FR-011), and neither
  does the credential object it owns (FR-023) — deleting the workload is sufficient to remove
  both, so no cleanup step depends on the tool still being alive.
- Adopted by exactly one run — a concurrent run must not use another's workload.
- Registered under the canonical service name so that execution resolves to it transparently — but
  **not** discoverable as a replica of that service. Service enumeration, replica counts and
  diagnostic collection must not return it (FR-028); the shared resolvers fall back to substring
  matching on names, so this exclusion has to be explicit rather than assumed.

---

## `BackupMetadata` (existing — additive fields only)

Additive by requirement: artifacts from previously released versions must remain restorable
without operator action (FR-016), and the constitution's backup-engine clause makes any
non-additive change a documented migration.

| Field | Type | Purpose |
|---|---|---|
| `source_endpoints` | ordered list, per database | The endpoints supplied, in the order they would be tried (FR-005, SC-004). Not a claim about which one served the capture — that is not knowable. |
| `source_roles` | map endpoint → role, per database | Each member's role as observed at capture time: leader, follower, or unknown. `unknown` is a real value, distinct from absent. |
| `capture_complete` | bool | False when the operation reported problems even though it produced an artifact. Such an artifact is removed rather than retained (FR-012), so this field is defence in depth for the case where removal itself fails — not an input to retention, which never reads metadata. |
| `external_location` | bool, per database | Whether this database was captured externally. Diagnostic, and it explains an artifact whose source endpoint is outside the deployment. |

**Compatibility rule**: a reader encountering none of these fields treats the artifact as an
internal, complete capture of unknown endpoint — which is exactly what a pre-feature artifact is.

---

## `RestoreAuthorization` (new, configuration)

| Field | Type | Constraint |
|---|---|---|
| `ExternalRestoreAllowed` | bool | Distinct from the existing restore override and from the scheduled-restore toggle; neither confers it (FR-009). |
| `Source` | enum | `flag` for interactive, `config` for unattended. Unattended restore requires the configuration form (FR-010). |

**State transition**: absent → refuse and name the authorisation required. Present → proceed to
pre-flight (FR-006, FR-007), then to the destructive step, then to confirmation (FR-021). There is
no path from absent to a destructive step.

---

## Entity relationships

```text
Run
 ├── DatabaseEndpoint (one per database; internal or external)
 │     └── TransientWorkload (only when external; 1:1, same RunID)
 │           └── CredentialRef (1:1, same RunID)
 ├── RestoreAuthorization (restore only)
 └── BackupMetadata (one per artifact; carries per-database source facts)
```

`TransientWorkload` exists only for `external` endpoints. That is the whole shape of the feature:
an internal endpoint already has a container, and an external one is given a stand-in so the
layers above cannot tell the difference.
