# Feature Specification: Infrahub Collect (Troubleshooting Bundle Tool)

**Feature Branch**: `fac/collect-tool-me2vx`

**Created**: 2026-07-02

**Status**: Extracted

**Input**: User description: "Add a new third CLI binary `infrahub-collect` (troubleshooting-bundle collection tool) to the infrahub-ops codebase, replacing the Python `invoke bundle collect` script from the main Infrahub repo and adding first-class Kubernetes support. Source: Jira INFP-415 and its Discovery Brief; upload is explicitly out of scope (deferred to INFP-581)." (Full grilled idea brief provided in conversation; confirmed decisions: third binary, parity minus benchmark with benchmark opt-in, key-name secret masking.)

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Collect a troubleshooting bundle from a Kubernetes deployment (Priority: P1)

A support engineer (or a customer guided by support) has access to a Kubernetes cluster running Infrahub. They run a single `infrahub-collect create` command from their workstation and receive one local archive containing logs from every Infrahub service (all replicas), diagnostic status dumps, server information, and a manifest describing what was collected. No repository checkout, Python installation, or container image download is required.

**Why this priority**: Kubernetes is the biggest gap today — no collection tool exists at all, so support and customers pull logs pod-by-pod with kubectl, Rancher, GKE, or Argo CD, inconsistently at every customer. This story alone delivers the core value of INFP-415.

**Independent Test**: Deploy Infrahub via the official Helm chart into a test cluster, run `infrahub-collect create` with only a kubeconfig available, and verify the produced archive contains per-service logs, status dumps, server info, and a manifest.

**Acceptance Scenarios**:

1. **Given** an Infrahub deployment in a namespace discoverable via the standard Helm chart labels, **When** the user runs `infrahub-collect create`, **Then** a `support_bundle_<timestamp>.tar.gz` file is written to the local output directory containing per-service logs, diagnostic status dumps, server info, and a manifest, and the command exits successfully printing the archive path.
2. **Given** multiple namespaces containing Infrahub deployments, **When** the user runs `infrahub-collect create --k8s-namespace <ns>`, **Then** only the named namespace is collected.
3. **Given** a service running multiple replicas (e.g. several task-worker pods), **When** collection runs, **Then** logs from every replica appear in the bundle in per-replica subdirectories.
4. **Given** a pod that has crashed and restarted (e.g. CrashLoopBackOff or OOMKilled), **When** collection runs, **Then** the bundle contains both the current container logs and the previous container's logs for that pod.

---

### User Story 2 - Collect from a Docker Compose deployment without the Infrahub repo (Priority: P2)

A support engineer or customer running Infrahub via Docker Compose runs the same `infrahub-collect create` command and receives a bundle with at least the content the existing `invoke bundle collect` script produces today — without cloning the Infrahub repository or installing Python.

**Why this priority**: Docker collection works today but requires a repo checkout and a Python environment; replacing it with the same prebuilt binary unifies the support workflow. It is P2 because a (cumbersome) alternative exists, unlike Kubernetes.

**Independent Test**: Start an Infrahub Docker Compose project, run `infrahub-collect create` from a host with only the binary and Docker installed, and compare bundle contents against the parity list.

**Acceptance Scenarios**:

1. **Given** a running Docker Compose project containing Infrahub, **When** the user runs `infrahub-collect create` (optionally with `--project <name>`), **Then** the bundle contains everything on the parity list (service logs, database logs, message-queue/cache/task-manager diagnostics, server info, container metrics, project metadata) and the internal bundle layout is identical to the Kubernetes bundle layout.
2. **Given** several Infrahub Compose projects on one host, **When** the user runs `infrahub-collect create` without `--project`, **Then** the tool offers the same project selection behavior as the existing backup tool.

---

### User Story 3 - Include a backup in the collection for issue reproduction (Priority: P3)

Support frequently asks for "logs plus a backup" so they can reproduce a customer issue locally. The user runs `infrahub-collect create --include-backup` and gets the troubleshooting bundle together with a backup produced by the existing backup logic, with the backup recorded in the manifest.

**Why this priority**: Valuable for reproduction workflows, but users can already run `infrahub-backup create` separately; this story removes a step rather than unlocking a new capability.

**Independent Test**: Run `infrahub-collect create --include-backup` against a test deployment and verify a backup artifact is produced alongside (or within) the bundle and referenced in the manifest.

**Acceptance Scenarios**:

1. **Given** a running Infrahub deployment, **When** the user runs `infrahub-collect create --include-backup`, **Then** a backup is created using the existing backup behavior, its location/identity is recorded in the bundle manifest, and both artifacts are available locally.
2. **Given** the backup step fails (e.g. database container unreachable), **When** collection completes, **Then** the log/diagnostic bundle is still produced and the manifest records the backup failure.

---

### Edge Cases

- **Degraded instance (the normal case for this tool)**: any single collector failing (service missing, pod down, exec refused) MUST NOT abort the run; the failure is recorded in the manifest and the remaining collectors continue.
- **Crashed/restarting pods**: log collection captures previous-container logs; exec-based collectors against a dead pod are skipped and recorded as failed.
- **Missing CLI prerequisites**: if neither Docker nor kubectl tooling is usable, the tool fails fast with the existing actionable "CLI unavailable" error behavior.
- **Very large logs**: the per-service log volume cap is configurable; the effective cap is recorded in the manifest so support can tell whether logs were truncated.
- **Optional services absent** (e.g. `task-manager-background-svc` not deployed): absence is recorded, not treated as an error.
- **HA database topologies** (e.g. CNPG-managed PostgreSQL with multiple instances): exec-based collectors target the primary using the existing primary-detection behavior.
- **Air-gapped / restricted environments**: default collection performs no image pulls and no network egress beyond the deployment itself; the opt-in benchmark degrades gracefully (skip + manifest warning) when its image cannot be pulled.
- **Interrupted collection** (Ctrl-C, killed): no workload is left stopped or scaled down, because collection never stops workloads in the first place; temporary working files should not be left in the output directory.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: System MUST automatically detect whether the target Infrahub deployment runs on Docker Compose or Kubernetes using the existing environment detection, and MUST honor the existing `--project` and `--k8s-namespace` selection flags and their environment-variable equivalents.
- **FR-002**: System MUST collect container logs for every Infrahub service present in the deployment (`infrahub-server`, `task-worker`, `database`, `message-queue`, `cache`, `task-manager`, `task-manager-db`, `task-manager-background-svc`), covering **all replicas** of multi-instance services, and on Kubernetes including previous-container logs for restarted pods.
- **FR-003**: System MUST collect the parity diagnostics that `invoke bundle collect` produces today: database (Neo4j) server logs, message-queue (RabbitMQ) status (queues, exchanges, bindings, connections, channels, status, environment), cache (Redis) status (info, client list, configuration, slow log, database size), task-worker state (Prefect worker CLI output), task-manager state (work pools, work queues, recent flow runs, events, automations), and server information (version, installed packages, API info/config/schema, environment variables).
- **FR-004**: System MUST collect database query logs only when the user opts in via a dedicated flag (matching the current tool's `--include-queries` behavior), because query logs may contain customer data.
- **FR-005**: System MUST collect container-level resource metrics using the mechanism appropriate to each environment (container stats on Docker, pod resource metrics on Kubernetes); host-level metrics from the collector's own machine are not collected.
- **FR-006**: System MUST package all collected data into a single local `.tar.gz` archive whose internal directory layout is identical across Docker and Kubernetes deployments.
- **FR-007**: System MUST include a manifest file in the bundle recording: a unique collection ID, creation timestamp, tool version, Infrahub version, environment type (docker or kubernetes), the set of collectors attempted, and per-collector success/failure/skip status with reasons.
- **FR-008**: System MUST mask secret values in collected environment variables and configuration output using key-name matching (at minimum keys containing `password`, `secret`, `token`, or `key`), matching the current tool's behavior.
- **FR-009**: System MUST treat individual collector failures as non-fatal: collection continues, the failure is recorded in the manifest, a warning is shown, and the command still exits successfully with a partial bundle.
- **FR-010**: System MUST NOT stop, restart, or scale any container, pod, or workload during collection; collection is strictly read-only with respect to the deployment's lifecycle.
- **FR-011**: Users MUST be able to configure the per-service log volume limit via a CLI flag and a corresponding `INFRAHUB_`-prefixed environment variable; the default replaces the current hardcoded 100,000-line limit and the effective value is recorded in the manifest.
- **FR-012**: System MUST be fully usable offline by default: no container image pulls and no network access beyond the target deployment itself.
- **FR-013**: Users MUST be able to opt into a benchmark run via a dedicated flag (default off); when the benchmark image cannot be pulled or run, the benchmark is skipped with a recorded warning and the rest of the collection proceeds.
- **FR-014**: Users MUST be able to include a backup via an `--include-backup` flag that reuses the existing backup behavior unmodified and records the backup in the manifest.
- **FR-015**: The collect capability MUST be exposed as a new `infrahub-collect` binary whose main command is `infrahub-collect create`, following the existing thin-entry-point pattern with all logic in the shared core, and reusing the shared environment commands (`environment detect`, `environment list`) and `version` command.

### Key Entities

- **Support bundle**: the single local `.tar.gz` archive produced by a collection run; contains per-service log directories, diagnostic dumps, server info, metrics, and the manifest. Identified by a timestamped filename.
- **Bundle manifest**: metadata document inside the bundle describing the collection run (ID, timestamps, versions, environment type, collector outcomes, effective limits); the collect-side sibling of the existing backup metadata document.
- **Collector**: a unit of collection (e.g. "service logs for task-worker", "cache status", "server info") with an individual outcome (success, failed, skipped) recorded in the manifest.
- **Deployment environment**: the detected target (Docker Compose project or Kubernetes namespace); reuses the existing environment abstraction and selection semantics.
- **Backup (existing)**: optionally produced via the existing backup capability when `--include-backup` is used; referenced by, not redefined by, this feature.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: A support engineer or customer obtains a complete troubleshooting bundle from a reference Kubernetes deployment with a single command, with no repository checkout, no Python environment, and no image download, in under 5 minutes.
- **SC-002**: For support cases where a bundle was provided, support needs zero follow-up requests for any artifact on the parity list (measured across support threads after rollout).
- **SC-003**: Collection causes no service interruption: zero container/pod stops, restarts, or scale changes attributable to the tool during collection.
- **SC-004**: The command surface and the bundle's internal layout are identical across Docker and Kubernetes deployments, so support tooling and habits transfer without per-environment instructions.
- **SC-005**: On a deployment with at least one unavailable service, collection still completes successfully and the bundle manifest accounts for 100% of attempted collectors with an explicit outcome.

## Assumptions

- Kubernetes deployments are installed via the official Infrahub Helm chart and carry its standard labels; deployments with custom labels may require explicit namespace selection.
- The machine running the tool has Docker or kubectl access with sufficient permissions to read logs and execute commands in containers (on Kubernetes: `pods/log` and `pods/exec`).
- The diagnostic CLIs used by exec-based collectors (Prefect, Redis, RabbitMQ clients) exist inside their respective official containers, as the current Python tool already assumes.
- Bundle upload/transfer is out of scope and deferred to INFP-581 (Auto-bundle Upload); the existing S3 upload code in this repo is a likely foundation for it.
- Pattern-based scrubbing of secrets inside *logs* is out of scope for v1; masking applies to environment/configuration output only (parity with today).
- Ansible/bare-metal deployment targets, all-in-one CLI consolidation, and log analysis/summarization are out of scope for v1.
- **Constitution impact**: Principle I of the project constitution currently limits the project to two binaries and states that "introducing a third binary requires a constitution amendment." The confirmed product decision (Jira INFP-415, grilled with the product owner) is a dedicated third binary. A constitution amendment (MINOR version bump extending Principle I to the split-binary pattern including `infrahub-collect`) MUST accompany or precede the implementation plan.
- Adding the new binary touches the build/release surface (Makefile targets, cross-compile matrix, release workflow, Dockerfile, Nix flake); if Go module dependencies change, `scripts/update-vendor-hash.sh` must be run per project policy.
