# Contract: Release Artifacts

Defines what a GitHub Release of `opsmill/infrahub-backup` MUST contain for
self-update to work. This is the contract between the release pipeline
(`.github/workflows/release.yml`) and the `update` command.

## Tag format

- Release tags MUST be valid semver with a leading `v`: `vMAJOR.MINOR.PATCH`
  (e.g. `v1.7.3`). This matches existing tags and `golang.org/x/mod/semver`.
- `releases/latest` (non-draft, non-prerelease) defines "latest stable".

## Required assets

Each release MUST attach one binary per supported platform, named exactly as
`make build-all` produces:

```text
infrahub-backup-linux-amd64
infrahub-backup-linux-arm64
infrahub-backup-darwin-amd64
infrahub-backup-darwin-arm64
infrahub-backup-windows-amd64.exe
infrahub-backup-windows-arm64.exe
infrahub-taskmanager-linux-amd64
infrahub-taskmanager-linux-arm64
infrahub-taskmanager-darwin-amd64
infrahub-taskmanager-darwin-arm64
infrahub-taskmanager-windows-amd64.exe
infrahub-taskmanager-windows-arm64.exe
```

Naming rule: `<binary>-<GOOS>-<GOARCH>` plus `.exe` when `GOOS == windows`.

## Checksums asset

Each release MUST attach a single `SHA256SUMS` file in standard `sha256sum`
format, covering every binary asset:

```text
<64-hex-sha256>␠␠infrahub-backup-linux-amd64
<64-hex-sha256>␠␠infrahub-backup-darwin-arm64
...
```

- Lowercase hex, two spaces before the filename, one entry per binary.
- Filenames in `SHA256SUMS` MUST match the asset names exactly.

## Pipeline change required

`release.yml` today uploads `bin/` only to S3. Add, after `make build-all`:

1. Generate checksums: `(cd bin && sha256sum * > ../SHA256SUMS)` (or `shasum -a 256`).
2. Attach to the GitHub Release, e.g.:
   `gh release upload "${GITHUB_REF_NAME}" bin/* SHA256SUMS --clobber`
   (requires `GITHUB_TOKEN` with `contents: write`).

The existing S3 upload, Docker build, and Homebrew bump jobs are unchanged.

## Consumer expectations (the `update` command)

- Discovery: `GET https://api.github.com/repos/opsmill/infrahub-backup/releases/latest`
  or `/releases/tags/{tag}` for `--version`. Sends a `Bearer` token from
  `GITHUB_TOKEN`/`GH_TOKEN` when present (optional; for rate-limit/CI).
- Select the asset whose name equals the computed `<binary>-<os>-<arch>[.exe]`.
- Download `SHA256SUMS`, look up the digest for that asset name, and pass it to
  the verifying apply step. Missing platform asset or missing `SHA256SUMS` entry
  is a hard failure (refuse, no write).
