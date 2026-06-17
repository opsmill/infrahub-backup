# Phase 0 Research: Self-Update Command

This document resolves the open technical questions for the self-update feature.
The single NEEDS CLARIFICATION carried from the spec — the download source — is
resolved here along with the supporting library and pipeline decisions.

## Decision 1 — Update source: GitHub Releases (not the private S3 bucket)

**Decision**: Self-update discovers and downloads from the public GitHub Releases
of `opsmill/infrahub-backup` via the GitHub REST API
(`GET /repos/opsmill/infrahub-backup/releases/latest` and
`/releases/tags/{tag}`). Release assets are downloaded over HTTPS from the
`browser_download_url` of each asset.

**Rationale**:
- The repository is public (`github.com/opsmill/infrahub-backup`) and the
  release pipeline already fires on `release: published`, so a GitHub Release
  for every version already exists — only its *assets* are missing today.
- The current binary upload target is a **private** S3 bucket whose endpoint and
  credentials are GitHub secrets; its public-read status is unknown and relying
  on it would couple a user-facing feature to private infra. GitHub Releases are
  unambiguously public and need no credentials.
- The unauthenticated GitHub API allows 60 requests/hour per IP — far above what
  a once-in-a-while update command needs. No token handling required.
- Tags follow `vX.Y.Z` (e.g. `v1.7.3`), and `releases/latest` returns the most
  recent non-prerelease release, which directly satisfies "latest stable".

**Required pipeline change**: `.github/workflows/release.yml` currently uploads
binaries only to S3. Add a step to (a) generate a `SHA256SUMS` file over the
`bin/` artifacts and (b) attach all per-platform binaries + `SHA256SUMS` to the
GitHub Release (e.g. via `gh release upload ${tag} bin/* SHA256SUMS`). The S3
and Docker/Homebrew jobs are unchanged.

**Prior art (uv / axoupdater)**: Astral's `uv self update` is powered by
[axoupdater](https://github.com/axodotdev/axoupdater) + cargo-dist and also reads
from **GitHub Releases**, confirming this as the standard source. axoupdater
supports an optional GitHub token (`set_github_token`) to avoid the
unauthenticated rate limit (60 req/hr) in CI and on shared IPs — we adopt the
same idea (see "Token support" below).

**Token support**: The GitHub client SHOULD honor a token from `GITHUB_TOKEN`
(or `GH_TOKEN`) when present, sending it as a `Bearer` auth header on API
requests. Unauthenticated access remains the default and is sufficient for
interactive use; the token only matters for high-frequency/CI use and for the
`--yes` automation path. No token is ever required for a public release.

**Alternatives considered**:
- *Public S3 bucket*: would avoid the GitHub API but requires confirming/forcing
  public-read ACLs and hard-coding a bucket URL; rejected as fragile and
  dependent on private infra configuration.
- *Homebrew/Docker as the only channels*: these already exist but cover only two
  install methods and don't serve the standalone-binary user the feature targets.
  They remain the recommended path for those install types (FR-010).

## Decision 2 — Binary replacement: `github.com/minio/selfupdate`

**Decision**: Use `github.com/minio/selfupdate` to apply the downloaded binary
over the running executable.

**Rationale**:
- It performs the hard, platform-specific parts correctly: write to a temp file
  in the **same directory** as the target, `fsync`, then atomic `rename`, with
  automatic **rollback** of the previous binary if the swap fails — directly
  satisfying FR-005 and SC-003 (never leave a broken binary), including the
  Windows "can't delete a running exe" case.
- Built-in `Checksum` (SHA-256) and optional signature verification options map
  onto FR-004 with no custom crypto.
- It is the maintained successor to `inconshreveable/go-update`, used in
  production by MinIO; pairs naturally with `minio-go/v7`, already a dependency,
  keeping the dependency surface coherent.

**Why not uv's "re-run the installer" model**: uv/axoupdater downloads the
release *installer* and re-executes it, because cargo-dist apps can be multi-file
and the installer also manages `PATH`/shell profiles. That indirection brings the
PATH-modification friction uv users repeatedly hit (e.g. astral-sh/uv#7319, and
the `INSTALLER_NO_MODIFY_PATH` escape hatch). Our distribution is a single
self-contained binary with no PATH management, so a direct in-place atomic swap
is both simpler and avoids that entire class of problem. We deliberately do **not**
adopt the installer-rerun approach.

**Alternatives considered**:
- *Hand-rolled temp-file + `os.Rename`*: feasible but re-implements rollback,
  permission preservation, and Windows handling that selfupdate already solves;
  rejected to avoid subtle "bricked binary" bugs.
- *`rhysd/go-selfupdate`*: bundles GitHub discovery and replacement together but
  pulls in `go-github` + `oauth2` and assumes archive layouts; heavier than
  needed since we control the asset naming. Rejected in favor of stdlib
  discovery + minio/selfupdate apply.

## Decision 3 — Version comparison: `golang.org/x/mod/semver`

**Decision**: Compare installed vs. available versions with
`golang.org/x/mod/semver` (`semver.Compare`, `semver.IsValid`).

**Rationale**: Tags are already in `vX.Y.Z` form, which is exactly what
`x/mod/semver` expects (it requires the leading `v`). It is a tiny, std-adjacent,
zero-transitive-dependency module. Handles "installed is newer than latest"
(downgrade detection) and prerelease ordering correctly.

**Alternatives considered**: `Masterminds/semver` / `hashicorp/go-version` —
more features (constraints, ranges) than needed and a larger footprint. Plain
string compare — incorrect across multi-digit components (e.g. `v1.10.0` vs
`v1.9.0`). Both rejected.

## Decision 4 — Asset naming contract

**Decision**: Assets are named `<binary>-<os>-<arch>` with a `.exe` suffix on
Windows, exactly as `make build-all` emits today (e.g.
`infrahub-backup-darwin-arm64`, `infrahub-taskmanager-windows-amd64.exe`). The
updater computes the expected asset name from `runtime.GOOS`/`runtime.GOARCH`
and the invoked binary's base name, then selects that asset from the release.

**Rationale**: Reuses the existing build naming with no pipeline churn beyond
attaching the files; deterministic mapping avoids guessing. The full contract is
captured in `contracts/release-artifacts.md`.

## Decision 5 — Install-method detection (when NOT to self-update)

**Decision**: Before replacing, classify the install method and refuse for
managed/non-durable cases (FR-010, FR-013):

- **Dev/unversioned build**: `version` (ldflags) is empty / not a valid semver →
  refuse with "self-update is only available for released builds".
- **Homebrew**: resolved executable path is under a Homebrew prefix
  (`/opt/homebrew/`, `/usr/local/Cellar/`, or `$(brew --prefix)`), or path
  contains `/Cellar/` → refuse, direct user to `brew upgrade infrahub-backup`.
- **Container image**: presence of `/.dockerenv` or container hints in
  `/proc/1/cgroup` → warn that the image binary won't persist and recommend
  pulling a newer image tag; allow override only if explicitly forced (out of
  scope for v1 — refuse by default).
- **Permission**: target binary or its directory not writable by the current
  user → report the path and that elevated privileges are required; do not
  attempt escalation (FR-011).

**Rationale**: Each method has a native, correct upgrade path; self-replacing
under a package manager corrupts its bookkeeping, and replacing an image-baked
binary is lost on restart. Detection is best-effort and conservative — when in
doubt about Homebrew/container, prefer refusing with guidance over a surprising
replacement.

**Alternatives considered**:
- *Always attempt replacement and let it fail* — rejected because it produces
  confusing partial states and breaks package managers silently.
- *Install receipt (uv/axoupdater approach)* — the most robust option:
  uv/cargo-dist writes a `*-receipt.json` at install time recording the install
  method, version, and prefix, and `uv self update` reads it to decide
  eligibility instead of guessing at runtime. We reject this **for now** only
  because it has a hard prerequisite: a first-party installer (e.g. a
  `curl | sh` script) that writes the receipt. We currently ship via direct S3
  binary, Homebrew, and Docker — none of which writes a receipt — so runtime
  heuristics are the pragmatic choice. If a first-party installer is added
  later, switching detection to a receipt would be a clean, strictly more
  reliable upgrade and is the recommended future direction.

## Decision 6 — Confirmation & non-interactive behavior

**Decision**: Default interactive runs print `current → target` and prompt
`[y/N]`. `--yes`/`-y` skips the prompt. When stdin is not a TTY and `--yes` was
not given, refuse rather than block, instructing the user to pass `--yes` for
unattended use.

**Rationale**: Safe-by-default for humans (FR-008, US3) while giving automation a
clean, non-hanging path. TTY detection via `golang.org/x/term.IsTerminal` (the
`golang.org/x/sys`/`x/term` line is already in the dependency tree) or stat of
stdin.

## Summary of new dependencies

| Module | Purpose | Notes |
|--------|---------|-------|
| `github.com/minio/selfupdate` | atomic binary apply + rollback + checksum verify | aligns with existing minio-go |
| `golang.org/x/mod` (`/semver`) | semver comparison | tiny, std-adjacent |

After adding these, run `scripts/update-vendor-hash.sh` to refresh `flake.nix`
`vendorHash` (per CLAUDE.md).
