# Session Retrospective: 007-external-database-backup

**Date**: 2026-09-02
**Scope**: full speckit pipeline run — specify → plan → critique → tasks → 8 implementation chunks
→ `/code-review max` → partial reset → first fix chunk
**Branch**: `007-external-database-backup`
**Approved dispositions**: `fix-now` only. `open-pr`, `github-issue` and `local-only` were
explicitly declined by the user; their findings are retained below so they are not lost.

---

## Findings

| ID | Category | Evidence | Improvement | Disposition |
|----|----------|----------|-------------|-------------|
| R1 | Instructions / Config | Chunk 2a: "`speckit-checkpoint-commit` is not installed here; committed inline." Same for `critique-template.md` and the git extension. `.specify/extensions.yml` lists all three extensions as `installed:` | Record in `AGENTS.md` which extension assets exist and what to substitute | **fix-now — applied** |
| R2 | Instructions / Config | `speckit-opsmill-auto` invoked twice with a non-description argument (empty, then `"and /speckit-opsmill-retrospect"`). A literal Phase A would have opened `008-` twice while 007 was mid-remediation | Precondition: if `.specify/feature.json` names a spec dir with an incomplete `tasks.md`, resume rather than specify | declined (`open-pr`) |
| R3 | Instructions / Config | `update-agent-context.sh` appended its own sections to `CLAUDE.md` — a deliberate one-line router — truncating at the first `**` and emitting `grep: repetition-operator operand invalid` | Make the script respect a router-style agent file, or target `AGENTS.md` | declined (`open-pr`); hazard documented in `AGENTS.md` under fix-now |
| R4 | Instructions / Config | Orchestrator lint check failed with `golangci-lint: command not found` while subagents reported it running; it lives in `$(go env GOPATH)/bin` via `make dev-setup` | Note it, plus `GO_TAGS` and the darwin `neo4jwatchdog` break, in `AGENTS.md` | **fix-now — applied** |
| R5 | Documentation | `AGENTS.md` claimed `utils.go` does "version detection and comparison". It has only `SetVersion`/`BuildRevision`. Chunk 2c spent a turn hunting for code to extend. Fixed in `5d63a9c` | A drift check failing when `AGENTS.md` names a capability no symbol provides | declined (`github-issue`) |
| R6 | Documentation | F29: `postgres:18-alpine` and `neo4j:2025.10.1-enterprise` both report `Config.User` empty and drop privileges inside their entrypoints, which `Command: ["sh"]` bypasses. Chunk 2d asserted the opposite and shipped a pod the kubelet always rejects | `dev/knowledge/` page recording the measured behaviour | **fix-now — applied** |
| R7 | Documentation | T065 never ran. ADR-0007 scopes offline-by-default to `infrahub-collect`; this feature makes an image pull mandatory and load-bearing, unable to degrade to "skipped" | ADR recording the posture change | declined (`github-issue`) |
| R8 | Architectural | `podCache` — a discovery cache — used as durable execution state. Reset wholesale at `environment_kubernetes.go:192` and `kubernetes_workloads.go:116`; restore calls `StartServices` by design. Forced exclusion filters onto four enumerators | ADR for an explicit exec target | declined (`github-issue`); remains queued as remediation step 2 |
| R9 | Architectural | `NewNeo4jEditionInfo` defaults to Community on **any** detection error, and the Community branch calls `stopAppContainers()`. A transient `cypher-shell` failure against an *internal* Enterprise database stops Neo4j and dumps it cold — **a live bug for current customers, independent of 007** | Own fix, own PR | declined (`github-issue`) |
| R10 | Architectural | Every destructive decision in this tool rests on substring evidence with errors swallowed: location (fixed in `de5e022`), *what gets scaled* (`findWorkloadResource:40,54` plus `if err != nil \|\| output == "" { continue }` at `:23`), and edition (R9) | One matching rule with explicit evidence requirements | declined (`github-issue`) |
| R11 | Mistakes | FR-002, FR-011 and FR-025 were certified done from subagent unit tests. Five requirements were inert — ~470 lines, zero non-test callers, doc comments asserting calls that don't exist | Chunk contract: grep for non-test callers of every newly exported symbol, **per chunk** | declined (`open-pr`); adopted informally from the first fix chunk onward |
| R12 | Mistakes | Chunk 2d's "the official images run as uid 7474/70" was accepted without checking. `docker image inspect` falsified it in ten seconds | Chunk contract: any factual premise about an external artifact must be verified by a named command, not asserted | declined (`open-pr`) |
| R13 | Mistakes | `/code-review` was scoped to `e9008b4..HEAD` and read the **working tree**, reviewing chunk 3a's uncommitted code | Don't run review while an implementation subagent holds the tree | declined (`local-only`); hazard documented in `AGENTS.md` under fix-now |

No separate action: the T074 mischaracterisation ("latent rather than live" — the exposure is *any*
pod matching, not only the transient pod) is subsumed by R10.

## Applied under fix-now

1. `AGENTS.md` — new "Spec-kit extensions: registered but only partly vendored" section: a table of
   what a skill names versus what exists, plus the `update-agent-context.sh` (R3) and
   `/code-review` working-tree (R13) hazards.
2. `AGENTS.md` — notes under Testing and Quality on `golangci-lint`'s install location, the
   required `GO_TAGS`, and the expected darwin `neo4jwatchdog` failure.
3. `dev/knowledge/container-image-user-semantics.md` — the measured image-user behaviour, why
   `runAsNonRoot` without `runAsUser` fails closed, and why overriding `Command` removes the
   privilege drop those images depend on.

## The one lesson worth carrying

Eight chunks each self-reported 100% success with accurate, verbatim, honest test evidence. A later
review found ~31 defects, including five requirements that were completely inert.

Unit tests written by the agent that wrote the code prove that it compiles and that the author's
assertions are self-consistent. They cannot distinguish working code from unreachable code, correct
logic wired to the wrong thing, or a guarantee that holds per-chunk and fails in composition. Two
checks would have caught most of it, both nearly free, neither in the loop: **grep for callers of
each newly exported symbol**, and **verify any premise about an external artifact with a named
command**. Both were adopted for the first fix chunk, and both immediately paid — the caller check
confirmed the new gate is reachable from `backup.go:105` and `plakar_backup.go:37`, and the premise
check confirmed the chart labels the database canonically at `tests/e2e/conftest.py`.

## Still open, recorded here rather than actioned

- The restore path never gates on location: `backup.go:482` and `plakar_restore.go:210/364/429/458`
  call `stopAppContainers()` with no location established — the create path's fixed defect, on the
  destructive path.
- `findWorkloadResource` chooses which workload to scale by substring and swallows kubectl errors
  (R10).
- ~28 further review findings, listed in `opsmill-implement-report.md` §5.
- T072 (CI topology) and T073 (customer prerequisite validation) remain the real gates on the
  feature.
