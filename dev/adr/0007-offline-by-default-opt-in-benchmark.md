# 7. Offline by default; image pulls and the benchmark are strictly opt-in

**Status**: Accepted
**Date**: 2026-07-09
**Source**: specs/archive/003-collect-tool/research.md (R11, R12), spec FR-012/FR-013

## Context

Troubleshooting bundles are often collected in air-gapped or egress-restricted environments. The tool must work with only the operator's existing Docker/kubectl access, and any feature that needs the network must fail gracefully rather than block the whole collection.

## Decision

By default, collection performs **no** container image pulls and **no** network access beyond the target deployment itself. The only feature that reaches out is the opt-in `--benchmark`, which pulls and runs the OpsMill benchmark image (`registry.opsmill.io/opsmill/bench`, overridable via `INFRAHUB_BENCHMARK_IMAGE`). When the image cannot be pulled or run — e.g. air-gapped — the benchmark collector records `skipped` with a warning and the rest of the collection proceeds normally (it is a skip, not a failure).

## Consequences

- The default run is safe in air-gapped environments; nothing silently reaches the internet.
- Opting into the benchmark is an explicit, documented choice, and its failure degrades gracefully.
- The override env var keeps air-gapped-with-private-registry workflows possible.

## Alternatives Considered

- **Benchmark on by default (as the prior Python tool)** — rejected: offline-by-default is the correct posture for an ops tool that runs in restricted networks.
- **A Go-native benchmark to avoid the image pull** — rejected: duplicates the maintained benchmark suite.
- **Treat a benchmark pull failure as `failed`** — rejected: an unavailable image in an air-gapped run is expected, not an error; `skipped` keeps the manifest honest without alarming the operator.
