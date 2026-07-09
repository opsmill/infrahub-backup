# Extraction Record

**Extracted on**: 2026-07-09
**Extracted by**: speckit.opsmill.extract

## ADRs Created

- dev/adr/0001-shell-out-to-docker-kubectl-clis.md (from R1)
- dev/adr/0002-non-fatal-timeout-bounded-collectors.md (from R2, critique E1)
- dev/adr/0003-read-only-collection.md (from spec FR-010 / SC-003)
- dev/adr/0004-key-name-secret-masking.md (from R5, critique E3)
- dev/adr/0005-uniform-bundle-and-manifest.md (from R6, R7)
- dev/adr/0006-include-backup-reuses-createbackup.md (from R10 + the force=true fix)
- dev/adr/0007-offline-by-default-opt-in-benchmark.md (from R11, R12, FR-012/FR-013)

## Knowledge Updated

- dev/knowledge/infrahub-collect.md (new: CLI surface, bundle layout, manifest, collect backend seam, gotchas)

## Guidelines Updated

- dev/guidelines/collectors.md (new: timeouts, non-fatal failures, masking choke-point, read-only, bounded shared-helper variants, testing)

## Archive

Spec directory moved to `specs/archive/003-collect-tool/` as a historical record.
