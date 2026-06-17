# Contract: `update` CLI Command

Applies to both `infrahub-backup` and `infrahub-taskmanager`. The command
updates the binary it was invoked from.

## Synopsis

```text
infrahub-backup update [flags]
infrahub-taskmanager update [flags]
```

## Flags

| Flag | Type | Default | Behavior |
|------|------|---------|----------|
| `--check` | bool | false | Report availability only; never write to disk (US2). Mutually exclusive with `--yes`. |
| `--version <vX.Y.Z>` | string | "" (latest) | Target a specific release tag instead of latest; enables pin/downgrade (FR-009). |
| `--yes`, `-y` | bool | false | Skip the confirmation prompt; required for non-interactive runs (FR-008). |

### Environment variables

| Variable | Effect |
|----------|--------|
| `GITHUB_TOKEN` / `GH_TOKEN` | Optional. When set, sent as a `Bearer` auth header on GitHub API calls to avoid the unauthenticated rate limit (60 req/hr). Never required for a public release; primarily for CI and shared-IP/SSH scenarios. |

## Behavior matrix

| Precondition | `--check` output | default/`--yes` action |
|--------------|------------------|------------------------|
| Newer release available | "update available: `<cur>` → `<target>`" + details URL | prompt (or proceed with `--yes`) → download, verify, replace; print "updated `<cur>` → `<target>`" |
| Already latest (or installed ≥ target) | "already up to date (`<cur>`)" | no-op, same message |
| Dev/unversioned build | "self-update unavailable: development build" | same — refuse |
| Homebrew install | "managed by Homebrew — run `brew upgrade infrahub-backup`" | same — refuse |
| Container image | "running in a container — pull a newer image tag" | same — refuse |
| Binary path not writable | "cannot update `<path>`: permission denied; re-run with elevated privileges" | same — refuse |
| Non-interactive stdin, no `--yes` | (n/a for check) | refuse: "re-run with --yes to update non-interactively" |
| Network/source unreachable | error message, no change | error message, no change |
| No matching asset for OS/arch | "no release artifact for `<os>/<arch>`" | same — refuse |
| Checksum verification fails | (n/a) | abort, discard download, original binary intact |

## Exit codes

| Code | Meaning |
|------|---------|
| 0 | Update applied, OR already up to date, OR `--check` completed (regardless of whether an update was available) |
| 1 | Refused (dev/homebrew/container/permission/non-interactive) or operational failure (network, no asset, checksum mismatch, write error) |

> Note: `--check` exits 0 even when an update is available, so it is safe in
> conditional shell logic; parse the stdout line (or a future `--json`) to gate
> on availability. A non-zero `--check` exit is reserved for *errors* (e.g.
> source unreachable), not for "an update exists".

## Output

- Human-readable lines to stdout for normal results; errors to stderr (matches
  existing logrus usage in the repo).
- Every successful or refused outcome names the **from** and **to** versions and
  includes the release details URL where applicable (FR-006).

## Invariants

- The running binary is replaced only after the download is fully written and
  its SHA-256 matches the value in `SHA256SUMS` (FR-004).
- Replacement is atomic with rollback; an interruption at any point leaves the
  prior binary runnable (FR-005, SC-003).
- `--check` and any refusal path make **zero** modifications on disk (FR-007).
