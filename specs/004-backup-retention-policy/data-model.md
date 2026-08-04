# Data Model: Backup Retention Policy

**Feature**: `specs/004-backup-retention-policy` | **Date**: 2026-08-04

No persisted state is introduced. All entities below are in-memory, per-invocation
values in the `src/internal/app` package.

## RetentionPolicy

The pair of optional rules supplied per invocation (flags / `INFRAHUB_*` env — the
tool's only two configuration channels). Ephemeral — never written to disk (spec: Key
Entities).

| Field | Type | Meaning | Validation |
|---|---|---|---|
| `Days` | `int` | Keep backups whose age (now − embedded timestamp) is strictly less than `Days` days. `0` = rule inactive. | If set: ≥ 1, else validation error (FR-001) |
| `Count` | `int` | Keep the `Count` most recent backups at the location. `0` = rule inactive. | If set: ≥ 1, else validation error (FR-001) |

**Derived predicate** `Active()`: true iff `Days ≥ 1 || Count ≥ 1`. `create` skips
retention entirely when inactive (FR-004); `prune` refuses to run when inactive
(US2 scenario 4).

**Keep rule (union, FR-002)**: ref kept ⇔ `(Days active ∧ age(ref) < Days·24h)` ∨
`(Count active ∧ recencyRank(ref) ≤ Count)` ∨ `ref = newest at location` (floor,
FR-003). A day is a fixed 24 hours, and `age(ref)` is measured against a filename
timestamp parsed in the local time of the host running the command, so the boundary
moves with that host's timezone offset.

## backupRef

One discovered backup candidate at a storage location.

| Field | Type | Meaning |
|---|---|---|
| `Name` | `string` | Base name (`infrahub_backup_<ts>.tar.gz[.enc]`) — display + deletion key |

**Derived accessor** `createdAt() time.Time`: the embedded `20060102_150405` timestamp
(R1), re-parsed from `Name` in `time.Local` on each call. The name is the ref's only
state: age and ordering are derived from it rather than stored beside it, so no ref —
however it was constructed — can carry a timestamp that disagrees with the archive a
deletion would remove. A name that is not a backup archive yields the zero instant.

**Construction rule**: `parseBackupName(name) (backupRef, bool)` — returns `false`
(candidate invisible to retention) for any name not matching the pattern or whose
timestamp fails to parse (FR-006). This is the *only* way refs enter the system, so
non-backup files are structurally undeletable — and after the derived-`createdAt()`
change the guarantee is stronger than when first written: it now also covers **ranking**,
because a ref cannot be ordered by a timestamp other than the one its own name carries,
and both `Delete` implementations re-check the name before removing anything.

**Ordering**: refs sort by `createdAt()` descending; ties broken by `Name` descending
(deterministic; `.enc` and plain with identical timestamps are distinct refs).

## storageLocation (interface seam)

A place where backups accumulate; the policy is evaluated independently per
location with the floor per location (FR-007).

| Method | Contract |
|---|---|
| `Name() string` | Human-readable label used in logs and error aggregation (e.g. `local:/backups`, `s3://bucket/prefix`) |
| `List(ctx) ([]backupRef, error)` | All matching refs currently at the location; empty slice for an empty location (edge case: successful no-op) |
| `Delete(ctx, ref backupRef) error` | Remove exactly that ref; error carries operation context (Principle V) |

**Implementations (v1)**:
- `localLocation{dir string}` — `os.ReadDir` + `parseBackupName` filter; delete via `os.Remove`.
- `s3Location{client s3Backend}` — prefix-scoped paginated list, keys filtered through `parseBackupName` on the base name; delete via `RemoveObject` on the key rebuilt by `buildS3Key` (R4). The narrow `s3Backend` interface (rather than `*S3Client`) exists so the base-name-to-key reconstruction is testable without a bucket: passing a base name where the full key belongs succeeds silently against real S3.

**Implementation (P3, deferred)**: `plakarLocation` — refs are complete snapshot
groups; incomplete groups excluded from count ranking; delete removes the group's
component snapshots + maintenance (R9).

## pruneOutcome

Per-location result used for reporting and error aggregation (FR-008, FR-009).

| Field | Type | Meaning |
|---|---|---|
| `Location` | `string` | `storageLocation.Name()` |
| `Kept` | `[]backupRef` | Refs retained (policy or floor) |
| `Candidates` | `[]backupRef` | Refs the policy did not keep — exactly what a real run deletes. Filled by planning; executing leaves it alone, so a preview and the run that follows report the same set (SC-004) |
| `Deleted` | `[]backupRef` | Refs that actually went away. Empty until a plan is executed (a dry run never fills it); a strict subset of `Candidates` when a deletion failed or a candidate had already vanished |
| `Err` | `error` | Joined error for this leg; nil on success |

The single `Pruned` field this table originally carried was split into `Candidates` and
`Deleted` during implementation: on a destructive path, one count meaning "would delete"
in dry-run mode and "did delete" in execute mode cannot be read correctly at a call site
that does not know which mode produced it.

**Aggregation rule**: on `create` and on `prune --force`, all legs are attempted; if any
`Err != nil` after a successful backup, the run returns
`"backup succeeded (<name>); retention failed: <joined errs>"` and exits non-zero.
Standalone `prune` returns the joined errors directly. On a non-forced `prune`, a leg
that fails to *list* aborts the run before the confirmation, so no leg deletes anything
(see `contracts/cli.md` exit semantics).

## State transitions

The only state machine is per-candidate and terminal:

```text
discovered ──parseBackupName──▶ ref ──selection──▶ kept
                │                        └───────▶ prunable ──delete──▶ removed
                └─(no match)─▶ invisible                └─(dry-run)──▶ reported only
```

No transitions persist; every invocation rediscovers from the location's live
contents.
