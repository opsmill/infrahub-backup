# Extraction Record

**Extracted on**: 2026-11-12
**Extracted by**: speckit.opsmill.extract

Most of this spec was written into `dev/` by the pull request that implemented it ([#193](https://github.com/opsmill/infrahub-backup/pull/193)): ADR 0008 (R2, R3, R4, R10, R15), the storage section of `dev/knowledge/infrahub-collect.md` (R5, R6, R8, R12) and the timeout-wrapping exception in `dev/guidelines/collectors.md` (R11). This extraction added what was still missing.

## ADRs Created

- dev/adr/0009-postgresql-access-through-psql-in-task-manager-db.md (from R1, spec Assumptions, ADR 0001)

## Knowledge Updated

- dev/knowledge/infrahub-collect.md (Task manager database storage: new subsections "File contracts" from data-model.md and spec Clarifications, "Outcomes, reasons and progress lines" from contracts/manifest-outcomes.md and R9, R11, R13, and "Privileges and authentication" from spec Assumptions, R2, R8, FR-014 and t046-cnpg-check.md; Gotchas: the official image trusts localhost, from R8)

## Guidelines Updated

- dev/guidelines/collectors.md (Query the task manager database through `runTaskManagerDBQuery`: one JSON value per query and the breakdown list as data, from R1 and R7; Testing: database time limits end to end, JSON Schema assertions by hand, and password-checked stacks, from R8 and R14)

## Archive

Spec directory moved to `specs/archive/006-taskmgr-db-storage/` as a historical record. The references to the spec in `dev/adr/0008-database-enforced-query-limits.md`, `src/internal/app/collect_taskmanager_db.go`, `src/internal/app/environment_kubernetes_test.go` and `tests/helpers/bundle.py` now point at the archive path.
