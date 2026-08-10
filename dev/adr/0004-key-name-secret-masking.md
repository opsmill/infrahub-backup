# 4. Key-name secret masking through a single choke-point

**Status**: Accepted
**Date**: 2026-07-09
**Source**: specs/archive/003-collect-tool/research.md (R5)

## Context

A troubleshooting bundle captures environment variables and configuration dumps (server env, server API config, Redis `CONFIG GET`, RabbitMQ `environment`/`status`). These contain secrets. Support engineers receive these bundles, so plaintext secrets must not be written (spec FR-008). The prior Python tool masked by key name, and parity was required.

## Decision

A pure function masks values whose **key name** contains any of `pass`, `secret`, `token`, `key` (case-insensitive substring), replacing the value with `********`. Every dump that can carry secrets is routed through this single choke-point (`masking.go`), and the set of masked outputs is enumerated explicitly:

1. `infrahub-server` environment (`maskEnvOutput`)
2. server API configuration dump (`maskJSON`, recurses through nested JSON)
3. Redis `CONFIG GET '*'` (`maskConfigPairs`)
4. RabbitMQ `rabbitmqctl environment` and `status` (`maskErlangConfig`)

The token list is a documented **minimum**; `pass` was added over the spec's `password` to also catch `requirepass`/`default_pass`. Raw service logs are written as-is (parity with the prior tool; the docs warn users to review before sharing).

## Consequences

- A single, table-tested function governs masking; new sensitive dumps must be added to the enumerated list and routed through it, so masking cannot be silently skipped.
- Over-masking (a non-secret key that happens to contain `key`) is accepted as safe; under-masking is not.
- Known limitation: credentials embedded in a value under a non-matching key (e.g. a connection string `..._URL=postgres://user:pw@host`) are not masked. This is a documented parity limitation, not a regression.

## Alternatives Considered

- **Value-pattern scrubbing inside logs** — out of scope for v1 (documented); logs are copied verbatim.
- **Per-collector ad-hoc masking** — rejected: a single choke-point with an enumerated call-site list is enforceable; scattered masking rots.
- **Allowlist of safe keys** — rejected: inverts the prior tool's behaviour and breaks parity.
