# Spec/Ask Alignment Check: Backup Retention Policy

**Date**: 2026-08-04 | **Feature**: `specs/004-backup-retention-policy`

## Source

The confirmed idea brief for opsmill/infrahub-backup#151, produced in an interactive
grilling session and posted at
<https://github.com/opsmill/infrahub-backup/issues/151#issuecomment-5175364825>
(identical local copy used verbatim as the comparison source). The brief carried
three journeys (P1 scheduled create, P2 standalone prune, P3 Plakar), FR-001–FR-010,
SC-001–SC-005, constitution alignment, governance gates, assumptions, out-of-scope,
and two open questions explicitly deferred to spec stage.

## Verdict

**✅ ALIGNED**

## Findings

| Severity | Category | PRD reference | Spec reference | Description |
|---|---|---|---|---|
| Info | added (necessary clarification) | Open Question 1 | FR-007, US2 scenario 5 | Brief deferred how the standalone `prune` S3 leg activates; spec resolves it as an explicit `--s3` request, with config presence alone never triggering deletion — the conservative reading of Principle II, within the brief's delegation |
| Info | added (necessary clarification) | Open Question 2 | FR-010, US3 scenario 3 | Brief deferred incomplete Plakar group handling; spec resolves: excluded from count rule, age-prunable, newest group overall never removed |
| Info | added (necessary clarification) | — (gap surfaced by critique X1) | FR-012, edge case "Plakar backend selected before its slice ships" | Brief split P1/P2 vs P3 but never defined the v1 boundary behavior; spec adds warn-and-skip on `create`, hard error on `prune`. Boundary definition, not scope expansion |
| Info | changed (tightened) | FR-008 | FR-008 | Critique E1 added within-leg best-effort semantics (attempt all candidates, aggregate errors) — a strict refinement of the brief's "MUST still attempt the other leg" |
| Info | added (consistent) | Users and Value ("zero external cleanup machinery") | SC-006 | Spec adds SC-006 (no new services/sidecars/wrappers), restating the brief's value proposition as a measurable criterion |

No brief requirement, journey, success criterion, assumption, or out-of-scope item is
missing, dropped, softened, or contradicted. All five deltas are additive
clarifications inside the decision space the brief explicitly delegated to spec/plan
stage; none change semantics of a stated requirement.

## Action

Proceed. No remediation pass needed (0 of 2 budget used).
