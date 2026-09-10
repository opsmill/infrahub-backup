# Extraction Record

**Extracted on**: 2026-09-07
**Extracted by**: speckit.opsmill.extract

## ADRs Created

- dev/adr/0009-transient-workload-lifecycle-and-credential-ownership.md (from R5, R7; FR-011/014/023/028; data-model `TransientWorkload`)
- dev/adr/0010-neo4j-restore-by-seed-uri.md (from R2; FR-007/020/021; artifact contract "Restore-side staging")
- dev/adr/0011-asymmetric-version-compatibility.md (from R3; FR-006)
- dev/adr/0012-location-from-positive-evidence.md (from R4; FR-001/002/003/004; data-model `DatabaseEndpoint`)
- dev/adr/0013-incomplete-captures-removed-metadata-additive.md (from R6; FR-005/012/016/024; artifact contract)
- dev/adr/0014-separate-external-restore-authorisation.md (from spec FR-009/010; cli-surface contract)
- dev/adr/0015-verify-by-default-external-tls.md (from FR-029; cli-surface contract, T100)
- dev/adr/0016-single-ownership-decider.md (from retrospective R10; FR-002/028; `kubernetes_ownership.go`)

Previously extracted from this spec, before this record: dev/adr/0008-transient-workload-for-external-databases.md (2026-09-03), dev/knowledge/container-image-user-semantics.md (2026-09-02).

## Knowledge Updated

- dev/knowledge/external-database-backup.md (new file)
- dev/knowledge/infrahub-collect.md (Gotchas)
- dev/adr/0008-transient-workload-for-external-databases.md (Source path moved to the archive)

## Guidelines Updated

- dev/guidelines/kubernetes-backend.md (new file)
- dev/guidelines/implementation-verification.md (new file)

## Not extracted

- plan.md, tasks.md, quickstart.md, checklists/, critiques/, opsmill-implement-report.md — execution artifacts; the quickstart scenarios live on as `tests/e2e/test_k8s_external_*.py`, and the report's §5 residuals are ticket material.
- research R1 — recorded as context in ADR 0008.
- research R8 — restates constitution Principle IV.
- retrospective R1–R5, R11–R13 — applied to AGENTS.md on 2026-09-02, or declined.

## Archive

Spec directory moved to `specs/archive/007-external-database-backup/` as a historical record.
