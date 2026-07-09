# 1. Shell out to `docker`/`kubectl` CLIs instead of client libraries

**Status**: Accepted
**Date**: 2026-07-09
**Source**: specs/archive/003-collect-tool/research.md (R1)

## Context

`infrahub-collect` (and the existing `infrahub-backup`/`infrahub-taskmanager`) must operate against both Docker Compose and Kubernetes deployments of Infrahub. The tools are copied onto customer hosts and CI runners where no Go toolchain or package manager is available (constitution Principle III), and they must cross-compile `CGO_ENABLED=0` for linux/darwin/windows on amd64/arm64.

## Decision

All Docker and Kubernetes interaction shells out to the `docker` and `kubectl` binaries through the shared `CommandExecutor`. No Kubernetes `client-go`, no Docker Engine SDK. New collectors reuse the existing backend abstraction (`EnvironmentBackend`) and executor rather than introducing a client dependency.

## Consequences

- No new Go module dependencies for collect; the vendor hash and cross-compilation stay trivial.
- `kubectl` handles kubeconfig, contexts, and auth plugins (OIDC, cloud IAM) for free — none of that has to be reimplemented.
- Remote Docker contexts keep working because the `docker` CLI resolves them.
- The cost is that behaviour depends on the CLIs being present and on parsing their text/JSON output; absence is detected and reported with an actionable error (`ErrCLIUnavailable`).

## Alternatives Considered

- **`k8s.io/client-go`** — rejected: large dependency tree, auth-plugin complexity, and inconsistent with the existing backends.
- **Docker Engine API over the socket** — rejected: breaks remote-context workflows the `docker` CLI handles transparently.
