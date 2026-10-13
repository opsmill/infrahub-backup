# 11. Asymmetric client/server version compatibility across two version schemes

**Status**: Accepted
**Date**: 2026-09-07
**Source**: specs/archive/007-external-database-backup/research.md (R3), spec FR-006, Clarifications (session 2026-09-02)

## Context

The backup utility for an external database runs in the transient pod's image, and its version is
chosen by the operator (a pinned default, overridable by flag). The server's version is whatever
the customer runs. The spec originally assumed a strict major.minor match was required, which the
vendor documentation does not support: for the backup command no client/server matching rule is
documented at all, and for restore the documented rule is forward tolerance (an artifact restores
into the same or a later version). Neo4j has also moved from `5.x.y` to calendar versions
(`YYYY.MM.p`), so a naive comparison silently misorders the two schemes.

For PostgreSQL the rule is documented: `pg_dump` reads servers older than itself and refuses
servers newer than itself, loudly.

## Decision

- **Probe the live server version before any data is read or written**, from the transient pod,
  and compare it with the utility version the same pod carries.
- **Asymmetric enforcement**: abort, naming both versions, when the utility is *older* than the
  server; warn and continue when it is *newer*. Only the direction with a known failure mode is an
  abort condition.
- **One database-version comparator that understands both schemes**, ordering every calendar
  version after every `5.x.y` one. It lives beside endpoint resolution and is deliberately not the
  comparator the self-updater uses for this tool's own semver release tags; the two must not be
  merged.
- **A version the server did not report is a failure to determine, not a pass.** The same holds for
  the edition: a probe that did not answer never selects a branch.
- **The exact compatibility envelope is measured, not asserted**: the end-to-end suite establishes
  it against real server versions, and the rule is not tightened on the page.

## Consequences

- The tool does not refuse combinations that work, which a symmetric rule would have done on
  every minor-version skew.
- The one direction that can produce a corrupt or refused artifact is caught before a byte moves,
  and the message names which version must change.
- Two version comparators exist in the codebase on purpose, with a note in `AGENTS.md` saying why.
- The remaining uncertainty is recorded as a known unknown rather than encoded as an abort.

## Alternatives Considered

- **Strict major.minor matching** — asserted a rule the vendor does not document, and would abort
  on combinations that work.
- **Two-phase discovery** (start a default image, probe, then start a version-matched one) — two
  pod lifecycles per run and a second failure surface, to derive something the operator knows.
- **A single semver comparator for everything** — misorders `2025.10.1` against `5.26.0`, and the
  updater's strict semver would reject calendar versions outright.
