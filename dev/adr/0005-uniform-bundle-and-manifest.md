# 5. Single `.tar.gz` bundle with a manifest, uniform across environments

**Status**: Accepted
**Date**: 2026-07-09
**Source**: specs/archive/003-collect-tool/research.md (R6, R7)

## Context

Support tooling and habits must transfer between Docker Compose and Kubernetes deployments without per-environment instructions (spec FR-006, SC-004). Support also needs a machine-readable record of what a bundle contains and which collectors failed, so a partial bundle from a degraded instance is still trustworthy (FR-007).

## Decision

A collection run produces one `support_bundle_<YYYYMMDD_HHMMSS>.tar.gz` whose internal layout is **identical** across Docker and Kubernetes, with all members under a top-level `bundle/` directory. A `bundle_information.json` manifest at the bundle root is the collect-side sibling of `BackupMetadata`:

- Fields: `manifest_version` (date-serial constant, bumped on breaking change), `collect_id`, `created_at` (RFC3339 UTC), `tool_version`, `infrahub_version`, `environment` (`docker`|`kubernetes`), `log_lines`, and `collectors[]` with `{name, status, reason?, artifact?}`.
- `status` is a typed enum (`success`|`failed`|`skipped`); `reason` is required whenever status is not `success`; `artifact` appears only on success (e.g. the `--include-backup` path).
- Written last, after every collector has reported, so it reflects final outcomes.

Files are staged in a temp directory created **inside** the output directory, packaged with the existing `createTarball`, and the staging dir is removed on success, failure, and interrupt (SIGINT/SIGTERM) so no temp files leak.

## Consequences

- One layout and one manifest schema to learn; the manifest is authoritative for what was attempted vs. produced.
- The manifest mirrors the proven `BackupMetadata` conventions (date-serial version constant, `MarshalIndent`, `os.WriteFile`).
- Staging inside the output directory keeps all writes on one filesystem and confined to the user-chosen location; a write failure (e.g. disk full) is a hard error after cleanup.

## Alternatives Considered

- **Streaming tar writer without a staging directory** — rejected: the manifest must be finalized *after* all collectors report, which a streaming writer cannot retro-fit.
- **Staging in `/tmp`** — rejected: cross-device writes and files leaking outside the user-designated directory.
- **Extending `BackupMetadata` rather than a sibling type** — rejected: different lifecycle and fields; the spec defines the manifest as a sibling.
