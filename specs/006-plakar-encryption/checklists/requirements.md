# Specification Quality Checklist: Encrypt the Plakar backend at rest, and extract the Neo4j integration

**Purpose**: Validate specification completeness and quality before proceeding to planning
**Created**: 2026-06-30 · **Revised**: 2026-08-07 (corrected the key-model entry; added workstream B)
**Feature**: [spec.md](../spec.md)

## Content Quality

- [x] No implementation details (languages, frameworks, APIs)
- [x] Focused on user value and business needs
- [x] Written for non-technical stakeholders
- [x] All mandatory sections completed

## Requirement Completeness

- [x] No [NEEDS CLARIFICATION] markers remain
- [x] Requirements are testable and unambiguous
- [x] Success criteria are measurable
- [x] Success criteria are technology-agnostic (no implementation details)
- [x] All acceptance scenarios are defined
- [x] Edge cases are identified
- [x] Scope is clearly bounded
- [x] Dependencies and assumptions identified

## Feature Readiness

- [x] All functional requirements have clear acceptance criteria
- [x] User scenarios cover primary flows
- [x] Feature meets measurable outcomes defined in Success Criteria
- [x] No implementation details leak into specification

## Workstream coverage

- [x] **A — encryption**: FR-001…FR-013, SC-001…SC-006, US1–US3. Implemented and E2E-validated (see [quickstart.md](../quickstart.md)).
- [x] **B — integration extraction**: FR-014…FR-024, SC-007…SC-012, US4–US5. Specified and planned; not yet implemented.
- [x] The two workstreams are independent and separately verifiable — neither's success criteria depend on the other's.
- [x] Terminology is consistent with Plakar's own vocabulary (integration ⊃ connector → plugin), defined in the spec's Overview.

## Notes

- **Correction (2026-08-07).** This checklist previously recorded the key-management model as "keypair (asymmetric) — backup needs only the public key; restore needs the private key." That was the *initial* answer and it is wrong. The plan-phase spike found kloset's repository encryption is **symmetric-only** (`encryption/keypair` is ed25519 for snapshot *signing*, not encryption), so the model is a **passphrase** deriving a symmetric key, required for every operation. The spec's Clarifications and FR-010 carry the revised decision; only this checklist had been left behind.
- The observation that the exact engine mechanism was a plan-phase research item rather than a spec-level decision was sound — and the spike is what corrected the spec.
- **Known residual inconsistency, deliberate**: the spec's title and `Status: Draft` header now understate its contents, since workstream A is implemented and workstream B is a second concern folded onto the same branch. Left as-is pending a decision on renaming.
- Workstream B carries one risk that no amount of specification can close: no existing hub recipe points at a repository outside the Plakar organisation. The basis for expecting it to work is Plakar's explicit statement, and the recipe submission is the confirmation — see research R8, which also records the fallback.
