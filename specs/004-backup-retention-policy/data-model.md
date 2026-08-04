# Data Model: Backup Retention Policy

**Feature**: `specs/004-backup-retention-policy` | **Date**: 2026-08-04

No persisted state is introduced. All entities below are in-memory, per-invocation
values in the `src/internal/app` package.

## RetentionPolicy

The pair of optional rules supplied per invocation (flags / `INFRAHUB_*` env /
config file). Ephemeral — never written to disk (spec: Key Entities).

| Field | Type | Meaning | Validation |
|---|---|---|---|
| `Days` | `int` | Keep backups whose age (now − embedded timestamp) is strictly less than `Days` days. `0` = rule inactive. | If set: ≥ 1, else validation error (FR-001) |
| `Count` | `int` | Keep the `Count` most recent backups at the location. `0` = rule inactive. | If set: ≥ 1, else validation error (FR-001) |

**Derived predicate** `Active()`: true iff `Days ≥ 1 || Count ≥ 1`. `create` skips
retention entirely when inactive (FR-004); `prune` refuses to run when inactive
(US2 scenario 4).

**Keep rule (union, FR-002)**: ref kept ⇔ `(Days active ∧ age(ref) < Days·24h)` ∨
`(Count active ∧ recencyRank(ref) ≤ Count)` ∨ `ref = newest at location` (floor,
FR-003).

## backupRef

One discovered backup candidate at a storage location.

| Field | Type | Meaning |
|---|---|---|
| `Name` | `string` | Base name (`infrahub_backup_<ts>.tar.gz[.enc]`) — display + deletion key |
| `CreatedAt` | `time.Time` | Parsed from the embedded `20060102_150405` timestamp (R1) |

**Construction rule**: `parseBackupName(name) (backupRef, bool)` — returns `false`
(candidate invisible to retention) for any name not matching the pattern or whose
timestamp fails to parse (FR-006). This is the *only* way refs enter the system, so
non-backup files are structurally undeletable.

**Ordering**: refs sort by `CreatedAt` descending; ties broken by `Name` descending
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
- `s3Location{client *S3Client}` — prefix-scoped paginated list, keys filtered through `parseBackupName` on the base name; delete via `RemoveObject` (R4).

**Implementation (P3, deferred)**: `plakarLocation` — refs are complete snapshot
groups; incomplete groups excluded from count ranking; delete removes the group's
component snapshots + maintenance (R9).

## pruneOutcome

Per-location result used for reporting and error aggregation (FR-008, FR-009).

| Field | Type | Meaning |
|---|---|---|
| `Location` | `string` | `storageLocation.Name()` |
| `Kept` | `[]backupRef` | Refs retained (policy or floor) |
| `Pruned` | `[]backupRef` | Refs actually deleted (or that *would* be, in dry-run) |
| `Err` | `error` | First/joined error for this leg; nil on success |

**Aggregation rule**: all legs are attempted; if any `Err != nil` after a successful
backup, the run returns `"backup succeeded (<name>); retention failed: <joined errs>"`
and exits non-zero. Standalone `prune` returns the joined errors directly.

## State transitions

The only state machine is per-candidate and terminal:

```text
discovered ──parseBackupName──▶ ref ──selection──▶ kept
                │                        └───────▶ prunable ──delete──▶ removed
                └─(no match)─▶ invisible                └─(dry-run)──▶ reported only
```

No transitions persist; every invocation rediscovers from the location's live
contents.
