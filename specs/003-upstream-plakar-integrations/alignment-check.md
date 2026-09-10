# Spec / Ask Alignment Check

**Feature**: `003-upstream-plakar-integrations` · **Date**: 2026-06-30

## Source

Inline PRD: the multi-paragraph feature description passed to `/speckit.specify` (Deliverable A + Deliverable B, runtime/DB-access model, consumption model, key risks, non-goals, testing). >2,500 chars, structured — qualifies as a substantive PRD, so the check runs. No external URLs.

## Verdict

✅ **ALIGNED**

The spec faithfully covers every PRD element. Three clarifications were applied during `/speckit.clarify`; all are *necessary clarifications* (allowed), not scope additions — each resolves a genuine ambiguity and traces to PRD intent.

## Findings

| Severity | Category | PRD reference | Spec reference | Description |
|---|---|---|---|---|
| ✅ | covered | Deliverable A (neo4j integration, monorepo, two URI schemes, manifest, no lifecycle mgmt, fork build) | FR-017–FR-021, Assumptions, contracts/neo4j-integration.md | Fully represented. |
| ✅ | covered | Deliverable B (postgres as-is, neo4j new, clean break, delete StreamingImporter/dump/restore, keep orchestration) | FR-001–FR-003, FR-007–FR-016 | Fully represented. |
| ✅ | covered | Runtime: inside container/pod, zero host deps, runner image | FR-004–FR-006, Assumptions, contracts/runner-interface.md | Fully represented. |
| ✅ | covered | Consumption: spike-gated in-process vs plakar-CLI | FR-022, research.md R1 | Fully represented. |
| ✅ | covered | Key risks (backup port 6362, kloset drift, K8s repo reachability, upstream acceptance) | Assumptions & Dependencies, plan Risks | Fully represented. |
| ✅ | covered | Non-goals (tar.gz untouched, not Infrahub-specific, no new encryption, no legacy layout) | Out of Scope | Fully represented. |
| ✅ | covered | Testing (testcontainers both editions + postgres E2E) | FR-020, SC-001/003/005, quickstart | Fully represented. |
| ⚠️ clarification | added (necessary) | PRD names credential discovery in context/risks | FR-023 | Hybrid auto-discover + override — resolves "how do creds reach the runner". Necessary clarification, not new scope. |
| ⚠️ clarification | added (necessary) | PRD did not specify restore granularity | FR-024 | Full-group + selective per-component restore — resolves an ambiguity; matches prior backend capability. Necessary clarification. |
| ⚠️ clarification | added (necessary) | PRD says "any Compose or K8s deploy"; risk 3 flags K8s | FR-025 | Compose + K8s at full parity, neither deferred — sharpens an ambiguous scope point. Necessary clarification. |

No requirements were **dropped**, **softened**, **semantically changed**, or **contradicted**; no **off-scope** additions beyond necessary clarifications.

## Action

Proceed. No remediation required (no significant drift; 0 remediation passes used).
