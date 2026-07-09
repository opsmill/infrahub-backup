# CLI Contract: infrahub-collect

**Status**: normative — already published in `docs/docs/reference/commands.mdx`, `docs/docs/guides/collect-troubleshooting-bundle.mdx`, and `docs/docs/guides/install-collect.mdx`. The implementation must match this surface exactly.

## Command structure

```text
infrahub-collect [global-flags] <command> [flags]

Commands:
  create                Collect a troubleshooting bundle
  environment detect    Detect the deployment environment (shared, identical to infrahub-backup)
  environment list      List detected Docker projects / Kubernetes namespaces (shared)
  version               Print the build version (shared pattern: "Version: <BuildRevision>")
```

## Global flags (persistent on root)

| Flag | Default | Env var | Notes |
|------|---------|---------|-------|
| `--project <name>` | auto-detect | `INFRAHUB_PROJECT` | shared via `ConfigureRootCommand` |
| `--k8s-namespace <name>` | auto-detect | `INFRAHUB_K8S_NAMESPACE` | shared via `ConfigureRootCommand` |
| `--output-dir <path>` | `./infrahub_bundles` | `INFRAHUB_OUTPUT_DIR` | collect-specific persistent flag |
| `--log-format <text\|json>` | `text` | `INFRAHUB_LOG_FORMAT` | shared via `ConfigureRootCommand` |
| `--help, -h` | — | — | cobra built-in |

## `create` flags

| Flag | Default | Env var |
|------|---------|---------|
| `--log-lines <n>` | `100000` | `INFRAHUB_LOG_LINES` |
| `--include-backup` | `false` | `INFRAHUB_INCLUDE_BACKUP` |
| `--include-queries` | `false` | `INFRAHUB_INCLUDE_QUERIES` |
| `--benchmark` | `false` | `INFRAHUB_BENCHMARK` |

Precedence: flag > environment variable > default (viper, `INFRAHUB_` prefix, `-`→`_` replacement — existing shared behavior).

## Behavioral contract for `create`

1. Detects the environment (Docker Compose first unless `--k8s-namespace` is set), honoring `--project`/`--k8s-namespace`; multiple candidates without a selector → same selection behavior/error as `infrahub-backup` (FR-001).
2. Never stops, restarts, or scales any container/pod/workload (FR-010). The only exception is a transient benchmark container/pod the tool itself creates under `--benchmark`.
3. Runs all collectors even when some fail; each failure logs a `WARN` line and is recorded in the manifest (FR-009).
4. Every collector subprocess is time-bounded (60s for exec/status dumps, 5 min for log/copy transfers); a timeout is a per-collector failure (`timed out after <duration>` in the manifest), never a hang.
5. Streams one `INFO` progress line per collector as it runs (Principle V); the docs' Step-3 transcript is the reference format.
6. Writes `support_bundle_<YYYYMMDD_HHMMSS>.tar.gz` into the output directory and prints its path **and size** on success. Write failures (unwritable/full output directory) are hard errors (exit 1) after staging cleanup.
6. Performs no image pulls and no network egress beyond the deployment unless `--benchmark` is set (FR-012/FR-013).

## Exit codes

| Code | Condition |
|------|-----------|
| 0 | Archive produced — including partial bundles with failed collectors (FR-009) |
| 1 | Hard failure: no usable environment (neither docker nor kubectl usable → existing `ErrCLIUnavailable`/`ErrEnvironmentNotFound` messages), no deployment found, output dir unwritable, or archiving itself failed |

## Interrupt behavior

SIGINT/SIGTERM: no workload state to restore (read-only); staging directory is removed; no partial archive is left behind with a final name (edge case "Interrupted collection").
