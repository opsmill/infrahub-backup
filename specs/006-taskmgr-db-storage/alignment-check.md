# Spec/Ask Alignment Check: Storage Figures for the Task Manager Database

**Date**: 2026-10-26 | **Feature**: `specs/006-taskmgr-db-storage`

## Source

- The PRD comment on [opsmill/infrahub-backup#181](https://github.com/opsmill/infrahub-backup/issues/181): <https://github.com/opsmill/infrahub-backup/issues/181#issuecomment-6422690269>, fetched with `gh` on 2026-10-26.
- The scoping comment it references, which puts external PostgreSQL servers out of scope: <https://github.com/opsmill/infrahub-backup/issues/181#issuecomment-6420264056>.
- The issue body, for the problem statement and context.

## Verdict

MINOR DRIFT (proceeding)

## Findings

Every PRD element was traced into `spec.md`:

- User stories 1 to 12: covered by the two journeys and FR-001 to FR-016.
- Journeys P1 and P2 and their acceptance criteria: present, including the 5-million-row `events` scenario and the `not requested` outcome, and both still ship in the first release.
- FR-001 to FR-014 of the PRD: present with the same numbers and tests, except the differences in the table below.
- The six edge cases: present.
- SC-001 to SC-005: present without change, including the note on which can be tested on every change.
- The assumptions and the out-of-scope list: present. The documentation and changelog decisions are FR-015 and an assumption.
- The three open questions: resolved from sources in the spec's Clarifications.

| Severity | Category | PRD reference | Spec reference | Description |
|---|---|---|---|---|
| Minor | changed | FR-009, P2 acceptance, user story 11 (`timed out after 5m0s`) | FR-009, FR-016, P2 scenario 4, Clarifications | The reason text is `timed out after 300s`, the format every collector already writes (`formatCommandTimeout`, ADR 0002). The limit, the enforcement by PostgreSQL and the outcome do not change. |
| Minor | changed | FR-005 ("`flow_run` by flow name and by state type") | FR-005, Clarifications | Interpreted as one grouping by both keys. The figures for each key alone are sums of these groups, so nothing the PRD asks for is lost, and support gets the combination it needs to estimate a flush. |
| Minor | changed | FR-014 test ("the existing primary-pod selection ... already has tests") | FR-014 | On `main` (commit `d4e4b34`), no test covers `findPrimaryPod`. The spec states this and the tasks add the test (T012). The requirement does not change. |
| Minor | changed | Testing decisions ("A Kubernetes run checks the P1 file and the primary-pod selection") | plan, research R14, tasks T022, T012, T046 | The CI Kubernetes cluster has no CloudNativePG, so the Kubernetes end-to-end run checks the P1 file, and primary-pod selection is covered by a unit test and an optional manual check on kind. |
| Minor | added | not in PRD | FR-002 (statistics reset time) | Added after critique P2: without it, support cannot tell whether "0 dead rows, never vacuumed" is a fact or statistics reset minutes ago. It serves user story 3 and adds one field. |
| Minor | added | not in PRD | FR-017 | No password in the bundle, the manifest or the progress output. On Kubernetes, the failure diagnostics that collectors already write would otherwise include `env PGPASSWORD=...` (research R10). It applies the existing masking rule (ADR 0004) to the new collectors. |
| Minor | added | "reuses the database owner lookup ... that the backup tool already uses" | Assumptions, research R8 | The lookup is the backup tool's, with one more step before the defaults: the `task-manager-db` container's own environment, used when the `task-manager` container cannot be reached (critique X1). The primary path does not change. |
| Minor | added | not in PRD | Edge cases | Two edge cases make implicit behavior explicit: records without a parent flow run fall into a group with no flow name, and a schema change can make the always-collected query reach its 60-second limit (verified on PostgreSQL 18). |
| Info | added | Implementation decisions, Documentation | FR-015, FR-016 | Documentation and the per-table outcome were in the PRD's implementation decisions and user story 11; the spec makes them requirements with tests. Not new scope. |

No PRD requirement is missing, dropped or contradicted. Downstream artifacts were checked for scope creep: `plan.md` and `tasks.md` add an undocumented test-only variable that can only lower the time limits (needed for the PRD's own FR-009 test), ADR 0008 (suggested by the PRD's further notes), and planner settings that avoid temporary files (in support of SC-005). None adds user-facing scope.

## Action

Proceed. No remediation pass was used.
