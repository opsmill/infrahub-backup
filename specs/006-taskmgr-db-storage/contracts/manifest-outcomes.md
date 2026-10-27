# Contract: Bundle Layout and Manifest Outcomes

**Feature**: [spec.md](../spec.md) (FR-004, FR-010, FR-011, FR-012, FR-016)

## Bundle layout

A new directory sits next to `task-manager/`. The layout is the same on Docker and Kubernetes.

```text
bundle/
├── ...
├── task-manager/                       # unchanged
├── task-manager-db/
│   ├── storage.json                    # every run (contracts/storage.schema.json)
│   ├── storage.err.txt                 # instead of storage.json, when its query fails
│   ├── storage-breakdown.json          # with --include-task-manager-storage (contracts/storage-breakdown.schema.json)
│   └── storage-breakdown.err.txt       # instead of storage-breakdown.json, when the catalog query fails
└── ...
```

A `.err.txt` file holds the command (with the password masked), the error and both output streams, as for every other failed dump in the bundle.

## Manifest entries

Both entries use the existing `{name, status, reason?}` shape. The manifest version does not change.

### `task-manager-db-storage`

| Case | Status | Reason |
|---|---|---|
| The query succeeds | `success` | none |
| `task-manager-db` has no running replica | `skipped` | `service not deployed; the task manager may use an external PostgreSQL server` |
| The query reaches the 60-second limit | `failed` | `timed out after 60s` |
| `psql` is missing in the container | `failed` | `psql missing: <cause>` |
| PostgreSQL rejects the login | `failed` | `login rejected: <cause>` |
| The container or server cannot be reached | `failed` | `not reachable: <cause>` |
| Any other query error | `failed` | `query failed: <cause>` |

### `task-manager-db-storage-breakdown`

| Case | Status | Reason |
|---|---|---|
| The flag and the environment variable are not set | `skipped` | `not requested` |
| `task-manager-db` has no running replica | `skipped` | `service not deployed; the task manager may use an external PostgreSQL server` |
| The catalog query fails | `failed` | `<kind>: <cause>`, as for the storage collector; no table is attempted |
| Every table on the list is collected or absent | `success` | none |
| One or more tables fail | `failed` | `partial storage breakdown: <table>: <reason>; <table>: <reason>` |

`<cause>` is the line of the error output that names the problem, with the password masked. The per-table reason in `storage-breakdown.json` uses the same text, for example `timed out after 300s`.

## Progress output

Breakdown lines, at info level unless stated (FR-013):

```text
Collecting storage breakdown of table events (time limit 300s)
Table events: 5000000 rows in 42 groups (1m12s)
Collecting storage breakdown of table log (time limit 300s)
WARN Table log: timed out after 300s
Table artifact: absent
```
