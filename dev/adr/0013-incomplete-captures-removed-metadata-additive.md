# 13. Incomplete captures are removed, never retained; completeness is read from output; metadata stays additive

**Status**: Accepted
**Date**: 2026-09-07
**Source**: specs/archive/007-external-database-backup/research.md (R6), spec FR-005/FR-012/FR-016/FR-024, contracts/artifact-and-metadata.md

## Context

The vendor's backup command exits `1` both when the backup failed and when it "succeeded but
encountered problems such as some servers being uncontactable". FR-005 encourages supplying several
endpoints, which makes the partial case more likely, and FR-012 forbids presenting a partial
capture as a complete restore point. Retention decides from artifact names and recency ranks and
never reads metadata, and an older release of the tool ignores metadata fields it does not know.
Had an incomplete artifact been retained, it would have occupied a keep slot, evicted a good
backup, and been restorable by an older tool without notice.

The artifact is also a cross-version contract: the constitution requires backups from prior
releases to remain restorable, or a documented migration.

## Decision

- **Completeness is derived from what the backup command reported, not from its exit status.** A
  non-zero exit is a failure by default; the command's own output identifies the "succeeded with
  uncontactable servers" case.
- **An incomplete capture is not retained.** Either every requested database is captured, or the
  run fails and the partial artifact is removed from every location it reached, including the
  object store. Retention and restore-point selection are never required to interpret metadata to
  honour this.
- **Metadata additions are additive and carry a documented meaning for absence**:
  `source_endpoints` (the list supplied, in try order), `source_roles` (each member's observed
  role, where `unknown` is a real value distinct from the field being absent), `capture_complete`,
  and `external_location` per database. A reader finding none of them treats the artifact as an
  internal, complete capture from an unrecorded endpoint — which is exactly what every previous
  release produced.
- **The metadata version does not change** (FR-024). Compatibility holds in both directions: a new
  tool reads an old artifact, and an old tool reads a new one.
- **No field claims which endpoint served the capture.** With several endpoints supplied, no
  documented interface reports it; the metadata records enough for an operator to see that a
  follower *could* have served it, which is the fact that bears on the recovery point.

## Consequences

- `capture_complete: false` should never be read: it is defence in depth for the case where
  removal itself fails, and a reader that meets it must not offer the artifact as a restore point.
- Retention code is untouched by the feature, and a partially failed multi-endpoint capture leaves
  the retention policy's decisions identical to a run that failed before writing anything.
- Artifact layout is byte-compatible between internal and external captures, so an artifact
  taken from an external database restores into an internal deployment and vice versa — a test
  obligation, not only a statement.
- Any future change that renames a field, changes its meaning or alters the layout is a breaking
  change needing a migration path and a release-note callout.

## Alternatives Considered

- **Back up from a single endpoint to avoid the ambiguity** — throws away the availability benefit
  that made multiple endpoints the vendor's recommendation.
- **Retain the artifact and mark it incomplete in metadata** — requires retention and restore to
  read metadata, and an older tool would still restore it.
- **Bump the metadata version** — makes every prior artifact a migration case for no reader that
  needs one.
- **Record the first endpoint as the source** — a guess presented as provenance.
