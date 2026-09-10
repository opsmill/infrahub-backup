# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

`infrahub-ops-cli` is a Go-based toolset for managing and maintaining Infrahub instances. The project provides three specialized CLI binaries:

- **infrahub-backup** - Backup/restore operations and environment detection
- **infrahub-taskmanager** - Task manager (Prefect) maintenance operations
- **infrahub-collect** - Troubleshooting-bundle collection (read-only diagnostics) for support

All three tools share common internal application logic but expose different commands through their respective main entry points.

## Common Development Commands

### Building and Running

- `make build` - Build all three binaries to `bin/infrahub-backup`, `bin/infrahub-taskmanager`, and `bin/infrahub-collect`
- `make build-all` - Cross-compile all three binaries for Linux, Darwin, and Windows (amd64/arm64)
- `make install` - Build and install all three binaries to `$GOPATH/bin`
- `make clean` - Remove build artifacts

### Testing and Quality

- `make test` - Run all tests
- `make test-coverage` - Generate coverage report (outputs coverage.html)
- `make lint` - Run golangci-lint (note: errcheck is disabled in .golangci.yaml)
- `make fmt` - Format code with go fmt
- `make vet` - Run go vet

Notes on running these directly rather than through `make`:

- `golangci-lint` is installed by `make dev-setup` into `$(go env GOPATH)/bin`, which is **not**
  necessarily on `PATH`. Invoking it as a bare command can fail with `command not found` while
  `make lint` works.
- The Makefile passes `GO_TAGS=-tags untested_go_version`. Use the same tag when running `go test`
  or `go vet` by hand, or results will not match CI. `golangci-lint` needs it too, spelled
  `--build-tags untested_go_version`: without it every package fails `typecheck` on undefined
  symbols in `cockroachdb/swiss`, which reads as a code error and is not one.
- The linter binary must be a **v2** (`.golangci.yaml` is `version: "2"`) built by a toolchain at
  least as new as the one in use. A binary built by an older Go cannot decode the compiler's
  export data and reports `export data version N is greater than maximum supported version M` for
  every stdlib import — or panics outright in `goanalysis`. `make dev-setup` reinstalls it from
  source at the pinned `GOLANGCI_VERSION`; re-run it after a Go upgrade.
- `make vet`, `make lint` and `go test ./...` fail on `tools/neo4jwatchdog` on darwin, because it
  uses `unix.Inotify*`. This is expected and unrelated to any change under `src/`; scope to
  `./src/...` when working on macOS — for the linter that is
  `golangci-lint run --build-tags untested_go_version ./src/...`. CI runs on Linux and lints the
  whole tree, so the targets are deliberately not narrowed.

### Development Setup

- `make dev-setup` - Install development dependencies including golangci-lint
- `make deps` - Download and tidy dependencies
- `make deps-update` - Update all dependencies

### Spec-kit extensions: registered but only partly vendored

`.specify/extensions.yml` lists `critique`, `opsmill` and `review` under `installed:`, but not all
of their assets are present in this repo. A skill step that names one of the missing ones needs a
substitute rather than a retry:

| Named by a skill | Present? | Substitute |
|---|---|---|
| `speckit-checkpoint-commit` | **no** | Commit inline, following the conventions in `git log` |
| `.specify/templates/critique-template.md` | **no** | Follow the structure the critique command describes |
| git extension (`before_specify` hook) | **no** | `/speckit-specify` creates **no branch** — create one by hand if the work should not sit on `main` |
| `reconcile` command | **no** | Do the substance inline |
| `/speckit-critique-run`, `/speckit-review-run` | yes | Wired as *optional* `after_plan` / `after_implement` hooks, so they are offered, not run |

Two further hazards when running the pipeline here:

- `.specify/scripts/bash/update-agent-context.sh` writes its own `## Active Technologies` and
  `## Recent Changes` sections into the agent file. `CLAUDE.md` in this repo is deliberately a
  one-line `@AGENTS.md` router, so the script corrupts it and truncates values at the first `**`.
  Restore `CLAUDE.md` afterwards and hand-write the entries into this file instead.
- `/code-review` reads the **working tree**, not the commit range passed to it. Do not run it while
  an implementation subagent holds uncommitted edits.

### Nix Vendor Hash

Whenever Go modules change (any modification to `go.mod` or `go.sum` — adding, removing, or updating dependencies), ALWAYS run `scripts/update-vendor-hash.sh` to update the `vendorHash` in `flake.nix`. Do not compute or edit the vendor hash manually.

## Architecture

The codebase follows a command-pattern architecture using Cobra for CLI structure, with three separate binary entry points sharing common internal logic:

### Core Components

1. **src/cmd/infrahub-backup/main.go** - Backup tool entry point
   - Defines root command with backup/restore subcommands
   - Commands: `create`, `restore`, `environment detect`, `environment list`, `version`
   - Uses shared application logic from `src/internal/app`

2. **src/cmd/infrahub-taskmanager/main.go** - Task manager tool entry point
   - Defines root command with task management subcommands
   - Commands: `flush flow-runs`, `flush stale-runs`, `environment detect`, `environment list`, `version`
   - Uses shared application logic from `src/internal/app`

3. **src/cmd/infrahub-collect/main.go** - Troubleshooting-bundle tool entry point
   - Defines root command with troubleshooting-bundle collection
   - Commands: `create`, `environment detect`, `environment list`, `version`
   - Uses shared application logic from `src/internal/app`

4. **src/internal/app/app.go** - Core application logic
   - `InfrahubOps` struct - Main application controller
   - `CommandExecutor` - Handles Docker Compose and system command execution
   - Environment detection (Docker vs Kubernetes)
   - Docker project discovery and validation
   - Shared by all three CLI tools

5. **src/internal/app/backup.go** - Backup and restore operations
   - Creates tar.gz backups with metadata JSON
   - Backs up Neo4j database, PostgreSQL (task-manager), and artifacts
   - Implements safe backup with container stopping/starting
   - Restore validates metadata and handles version compatibility

6. **src/internal/app/taskmanager.go** - Task management operations
   - PostgreSQL database connection management
   - Flow run cleanup operations (completed/failed/cancelled)
   - Stale run cancellation (stuck in running state)
   - Uses embedded Python scripts for Prefect API operations

7. **src/internal/app/collect*.go** - Troubleshooting-bundle collection
   - `collect.go` - Orchestrator (`CollectBundle`), collector run plan, staging/archive lifecycle
   - `collect_manifest.go` - `bundle_information.json` manifest and per-collector results
   - `collect_logs.go` - Per-replica service log collector (backend-agnostic)
   - `collect_diagnostics.go` - Database, message-queue, cache, task-worker, task-manager, and server collectors
   - `collect_metrics.go` - Container resource metrics collector
   - `collect_extras.go` - Opt-in `--include-backup` and `--benchmark` collectors
   - `masking.go` - Key-name secret masking for env and config dumps

8. **src/internal/app/utils.go** - Utility functions
   - File operations, checksum validation, tar/tarball handling
   - Environment variable handling
   - This tool's own build version (`SetVersion`, `BuildRevision`) from ldflags — **not** a
     version comparator. Comparators live elsewhere and are deliberately separate:
     `src/internal/updater/version.go` compares this tool's own release tags with strict semver,
     and `src/internal/app/external_endpoint.go` compares *database* versions, which must handle
     both `5.x.y` and calendar `YYYY.MM.p` schemes. Do not merge them.

9. **src/internal/app/cli.go** - Shared CLI configuration
   - `ConfigureRootCommand()` - Sets up common flags and configuration
   - `AttachEnvironmentCommands()` - Adds environment detection commands
   - Shared between all three binaries

10. **src/internal/app/external_*.go** - Databases that live outside the deployment (spec 007)
    - `external_backup_gate.go` - Where the location is settled *before* anything is stopped;
      `resolveDatabaseTargets` is the only way a *run* obtains `databaseTarget`s, and holding one
      is the evidence that the reachability question was answered. The constructor it calls,
      `databaseTargetWith`, is directly callable within the package (tests drive it), so the
      evidence is a `gated` field only that constructor sets and both preparers refuse a target
      without — not a promise that the type could not keep
    - `external_endpoint.go` - Endpoint resolution and discovery, the `databaseTarget` type, and
      the *database* version comparator (see the note on `utils.go`)
    - `external_capture.go` - The capture sequence: probe, edition/version gates, then the
      capture workload. `externalCaptureOps` is the only function a transient object can come
      from, so it is where a run that may not create one is refused (`infrahub-collect`, ADR-0003)
    - `external_restore.go` - The restore sequence, which is the capture's mirror and not its
      copy: one workload created at the gate and held for the run, the artifact staged where the
      *server* can fetch it (FR-007), the seed statement in the form the server version accepts,
      and the poll that decides whether the seed actually loaded (FR-021) — accepting the request
      proves nothing
    - `external_ca.go` - Carries the deployment's own certificate authority into the workload
      that has to verify against it, once per workload
    - `external_timeout.go` - Every bound and deadline the external path spends
    - `transient_workload.go` - The stand-in pod and its credential Secret: manifest
      construction, lifecycle, the stray reaper (FR-011) and the FR-028 exclusion. The
      exclusion is read from the pod's own `transientLabelMarker` before its name, in that
      order: a name test rests on `transientObjectPrefix`, and the fallbacks that reach
      `withoutTransientPods` go on to match a name against a service, so a prefix containing a
      service name would have them claim the pod rather than merely miss it

11. **src/internal/app/kubernetes_*.go** - The Kubernetes-side questions the above rests on
    - `kubernetes_ownership.go` - The single answer to "whose resource is this?": label
      evidence, release prefixes, `deploymentClaimsResource`, `unanchoredNamePolicyFor`, and
      `parseLabelledPods`, the one parser for both pod listings. Nothing outside this file may
      *decide* ownership from a name — the acceptance test is that
      `grep -rn 'nameMatchesService(' src/ | grep -v _test` returns only its definition in
      `kubernetes_pod_match.go` and its call sites here. `labelledPod.Labels` keeps the four
      service-label values the listing carried, because a label *selector* and a *declaration*
      are two readings of the same fields: `podsServiceSelectorsWouldMatch` reproduces the
      running check's four `<key>=<service>` queries out of them, in selector order, so those
      queries are not issued. Ownership is deliberately not that reproduction — it also claims
      a pod carrying a release prefix, and `IsRunning` answering true from one of those is a
      stop and a scale to zero on evidence no selector produced
    - `kubernetes_pod_match.go` - Pod phases and name shape: the field selectors, their
      client-side halves, and `nameMatchesService`, which answers what a name *looks like* and
      never what it belongs to
    - `kubernetes_workloads.go` - Workload (Deployment/StatefulSet) resolution and scaling
    - `environment_kubernetes.go` - The backend itself: exec/copy, pod resolution and its caches
      (`dropPodCaches` is what a scale clears), the running check, `locateService`. The running
      check costs one kubectl call: it reads one namespace listing, answers the selector tier
      from that listing's own label fields and falls back to ownership only where no selector
      would have matched. That one call failing *is* the undetermined status, and
      `stopAppContainers` fails the run on it

### Key Design Patterns

- **Split Binary Architecture**: Three specialized binaries sharing common internal logic for focused functionality
- **Embedded Scripts**: Python scripts are embedded using Go's embed package (src/internal/app/scripts directory)
- **Docker Compose Integration**: All operations work through Docker Compose commands
- **Project-based Operations**: Can target specific Docker Compose projects with `--project` flag
- **Streaming Output**: Commands stream output in real-time for user feedback
- **Shared Configuration**: All binaries use the same configuration system and environment variables

## Docker Compose Dependencies

All tools assume Infrahub is deployed using Docker Compose with these service names:

- `database` (Neo4j) - Used by infrahub-backup and infrahub-collect
- `task-manager-db` (PostgreSQL) - Used by infrahub-backup and infrahub-taskmanager
- `infrahub-server`, `task-worker`, `task-manager`, `task-manager-background-svc` - Application containers
- `cache`, `message-queue` - Infrastructure services

## Error Handling

The codebase uses explicit error wrapping with `fmt.Errorf` for context. All commands return errors up to the Cobra command handlers which handle display to users.

**Documentation Purpose:**

- Guide users through installing, configuring, and using Infrahub in real-world workflows
- Explain concepts and system architecture clearly, including new paradigms introduced by Infrahub
- Support troubleshooting and advanced use cases with actionable, well-organized content
- Enable adoption by offering approachable examples and hands-on guides that lower the learning curve

**Structure:** Follows [Diataxis framework](https://diataxis.fr/)

- **Tutorials** (learning-oriented)
- **How-to guides** (task-oriented)
- **Explanation** (understanding-oriented)
- **Reference** (information-oriented)

**Tone and Style:**

- Professional but approachable: Avoid jargon unless well defined. Use plain language with technical precision
- Concise and direct: Prefer short, active sentences. Reduce fluff
- Informative over promotional: Focus on explaining how and why, not on marketing
- Consistent and structured: Follow a predictable pattern across sections and documents

**For Guides:**

- Use conditional imperatives: "If you want X, do Y. To achieve W, do Z."
- Focus on practical tasks and problems, not the tools themselves
- Address the user directly using imperative verbs: "Configure...", "Create...", "Deploy..."
- Maintain focus on the specific goal without digressing into explanations
- Use clear titles that state exactly what the guide shows how to accomplish

**For Topics:**

- Use a more discursive, reflective tone that invites understanding
- Include context, background, and rationale behind design decisions
- Make connections between concepts and to users' existing knowledge
- Present alternative perspectives and approaches where appropriate
- Use illustrative analogies and examples to deepen understanding

**Terminology and Naming:**

- Always define new terms when first used. Use callouts or glossary links if possible
- Prefer domain-relevant language that reflects the user's perspective (e.g., playbooks, branches, schemas, commits)
- Be consistent: follow naming conventions established by Infrahub's data model and UI

**Reference Files:**

- Documentation guidelines: `docs/docs/development/docs.mdx`
- Vale styles: `.vale/styles/`
- Markdown linting: `[tool.rumdl]` in `pyproject.toml`

### Document Structure Patterns (Following Diataxis)

**How-to Guides Structure (Task-oriented, practical steps):**

```markdown
- Title and Metadata
    - Title should clearly state what problem is being solved (YAML frontmatter)
    - Begin with "How to..." to signal the guide's purpose
    - Optional: Imports for components (e.g., Tabs, TabItem, CodeBlock, VideoPlayer)
- Introduction
    - Brief statement of the specific problem or goal this guide addresses
    - Context or real-world use case that frames the guide
    - Clearly indicate what the user will achieve by following this guide
    - Optional: Links to related topics or more detailed documentation
- Prerequisites / Assumptions
    - What the user should have or know before starting
    - Environment setup or requirements
    - What prior knowledge is assumed
- Step-by-Step Instructions
    - Step 1: [Action/Goal]
        - Clear, actionable instructions focused on the task
        - Code snippets (YAML, GraphQL, shell commands, etc.)
        - Screenshots or images for visual guidance
        - Tabs for alternative methods (e.g., Web UI, GraphQL, Shell/cURL)
        - Notes, tips, or warnings as callouts
    - Step 2: [Action/Goal]
        - Repeat structure as above for each step
    - Step N: [Action/Goal]
        - Continue as needed
- Validation / Verification
    - How to check that the solution worked as expected
    - Example outputs or screenshots
    - Potential failure points and how to address them
- Advanced Usage / Variations
    - Optional: Alternative approaches for different circumstances
    - Optional: How to adapt the solution for related problems
    - Optional: Ways to extend or optimize the solution
- Related Resources
    - Links to related guides, reference materials, or explanation topics
    - Optional: Embedded videos or labs for further learning
```

**Topics Structure (Understanding-oriented, theoretical knowledge):**

```markdown
- Title and Metadata
    - Title should clearly indicate the topic being explained (YAML frontmatter)
    - Consider using "About..." or "Understanding..." in the title
    - Optional: Imports for components (e.g., Tabs, TabItem, CodeBlock, VideoPlayer)
- Introduction
    - Brief overview of what this explanation covers
    - Why this topic matters in the context of Infrahub
    - Questions this explanation will answer
- Main Content Sections
    - Concepts & Definitions
        - Clear explanations of key terms and concepts
        - How these concepts fit into the broader system
    - Background & Context
        - Historical context or evolution of the concept/feature
        - Design decisions and rationale behind implementations
        - Technical constraints or considerations
    - Architecture & Design (if applicable)
        - Diagrams, images, or explanations of structure
        - How components interact or relate to each other
    - Mental Models
        - Analogies and comparisons to help understanding
        - Different ways to think about the topic
    - Connection to Other Concepts
        - How this topic relates to other parts of Infrahub
        - Integration points and relationships
    - Alternative Approaches
        - Different perspectives or methodologies
        - Pros and cons of different approaches
- Further Reading
    - Links to related topics, guides, or reference materials
    - External resources for deeper understanding
```

### Quality and Clarity Checklist

**General Documentation:**

- Content is accurate and reflects the latest version of Infrahub
- Instructions are clear, with step-by-step guidance where needed
- Markdown formatting is correct and compliant with Infrahub's style
- Spelling and grammar are checked with Vale
- **Vale style checks pass**: Run `vale $(find ./docs -type f \( -name "*.mdx" -o -name "*.md" \) -not -path "./docs/node_modules/*")` and address all issues

**For Guides:**

- The guide addresses a specific, practical problem or task
- The title clearly indicates what will be accomplished
- Steps follow a logical sequence that maintains flow
- Each step focuses on actions, not explanations
- The guide omits unnecessary details that don't serve the goal
- Validation steps help users confirm their success
- The guide addresses real-world complexity rather than oversimplified scenarios

**For Topics:**

- The explanation is bounded to a specific topic area
- Content provides genuine understanding, not just facts
- Background and context are included to deepen understanding
- Connections are made to related concepts and the bigger picture
- Different perspectives or approaches are acknowledged where relevant
- The content remains focused on explanation without drifting into tutorial or reference material
- The explanation answers "why" questions, not just "what" or "how"

## Active Technologies

- Go 1.25.0 + kloset v1.0.13 (Plakar core), integration-fs (storage), cobra, logrus, viper (002-plakar-integration)
- Plakar repository (local filesystem or S3 via integration backends) (002-plakar-integration)
- Go 1.25.0 + cobra, viper, logrus; Docker/Kubernetes via `docker`/`kubectl` CLI shell-out through `CommandExecutor` — no client-go or Docker SDK (003-collect-tool)
- Local filesystem bundle output under `--output-dir` (default `./infrahub_bundles`); no network egress beyond the target deployment (003-collect-tool)
- Go 1.25.0 + no new modules; external databases reached from a transient in-namespace pod created via `kubectl` on stdin, so vendor tooling still runs in a container and the existing exec/stream/copy primitives are unchanged (007-external-database-backup)
- Neo4j endpoint read from `INFRAHUB_DB_ADDRESS` (comma-separated `host[:port]`), `_PORT` (Bolt 7687, *not* the 6362 backup port), `_PROTOCOL`, `_TLS_*` (007-external-database-backup)

## Recent Changes

- 007-external-database-backup: External Neo4j Enterprise / PostgreSQL backup and restore for Kubernetes — transient stand-in pod registered under the existing service names, endpoint discovery from in-namespace component environment, remote restore by seeding from an object-store URI
- 003-collect-tool: Added `infrahub-collect` troubleshooting-bundle binary (collector framework, log/metrics primitives on both backends, key-name secret masking, bundle manifest)
- 002-plakar-integration: Added kloset (Plakar core library), integration-fs (filesystem storage/exporter), cobra, logrus
