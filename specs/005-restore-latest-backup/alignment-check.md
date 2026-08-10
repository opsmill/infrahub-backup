# Spec/Ask Alignment Check: Restore the Latest Backup

**Date**: 2026-08-04 | **Feature**: `specs/005-restore-latest-backup`

## Source

The maintainer-confirmed idea brief for opsmill/infrahub-backup#152, provided
inline as the feature description and canonically published at
<https://github.com/opsmill/infrahub-backup/issues/152#issuecomment-5179350853>.
The inline copy is verbatim identical to the published comment (same authoring
session), so no re-fetch was required.

## Verdict

**✅ ALIGNED**

## Findings

Every PRD element was traced into `spec.md`:

- **FR-001 … FR-009**: all nine present with semantics preserved (wording expanded
  into testable form; no requirement softened, dropped, or re-scoped).
- **User journeys P1/P2**: both preserved, including the Given/When/Then
  acceptance scenarios and the P1-over-P2 rationale.
- **Edge cases**: all five from the PRD present (encrypted-newest, empty pool,
  non-backup files, timestamp ties, prune race).
- **Success criteria SC-001 … SC-003**: preserved in meaning and measurability.
- **Assumptions and Out of Scope (v1)**: all items carried over.

| Severity | Category | PRD reference | Spec reference | Description |
|---|---|---|---|---|
| Info (not drift) | added | — | Assumptions, bare-`restore` edge case | Spec restates FR-003 as an edge-case bullet and adds two assumptions: (a) `--latest` satisfies constitution Principle II's explicit-request rule with no new confirmation prompt — a necessary clarification, since a prompt would contradict the PRD's unattended P1 journey; (b) existing restore safety machinery applies unchanged. Both are expansions consistent with the PRD, not new scope. |
| Info (not drift) | added | "local by default with --s3 for the bucket" | contracts/cli.md invocation matrix | The plakar + `--latest --s3` combination (unaddressed by the PRD) is resolved as a hard error in the design artifacts — a necessary completion of the invocation matrix, consistent with the PRD's "pools are never merged" stance. |

No missing, changed, dropped, or contradicted requirements. Downstream artifacts
were spot-checked for creep: the critique-driven design change (S3 temp-path
download) alters *mechanics* the PRD never specified, and strengthens the PRD's
own safety posture; tasks.md covers all nine FRs and adds nothing outside the
PRD's scope beyond the critique remediations (docs, follow-up issue).

## Action

Proceed. No remediation passes used.
