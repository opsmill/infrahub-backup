# Phase 1 Data Model: Self-Update Command

This feature has no persistent storage. The "entities" are in-memory values that
flow through the update pipeline. They are described here as the contract between
the discovery, verification, and apply stages.

## Entity: Release

A published version of the tool, as returned by the GitHub Releases API.

| Field | Type | Source / Notes |
|-------|------|----------------|
| `TagName` | string | e.g. `v1.7.3`; MUST be valid semver (leading `v`) |
| `Prerelease` | bool | excluded when resolving "latest stable" |
| `Draft` | bool | never a self-update target |
| `Assets` | []Asset | downloadable files attached to the release |
| `HTMLURL` | string | shown to the user as the changelog/details link |

**Validation rules**:
- A Release is a valid update target only if `Draft == false`, `Prerelease == false`
  (unless a specific prerelease tag was requested via `--version`), `TagName`
  passes `semver.IsValid`, and it contains both the platform asset and a
  `SHA256SUMS` asset.

## Entity: Asset

A single downloadable file attached to a Release.

| Field | Type | Notes |
|-------|------|-------|
| `Name` | string | `<binary>-<os>-<arch>[.exe]` or `SHA256SUMS` |
| `BrowserDownloadURL` | string | HTTPS download URL |
| `Size` | int64 | used for progress/sanity |

## Entity: PlatformTarget (derived, not from API)

Describes which asset the running binary needs.

| Field | Type | Source |
|-------|------|--------|
| `BinaryName` | string | base name of the running executable (`infrahub-backup` / `infrahub-taskmanager`) |
| `OS` | string | `runtime.GOOS` |
| `Arch` | string | `runtime.GOARCH` |
| `AssetName` | string | computed: `BinaryName-OS-Arch` + `.exe` if `OS=="windows"` |

## Entity: InstalledBinary (derived, not from API)

The currently running executable and how it was installed.

| Field | Type | Source |
|-------|------|--------|
| `Version` | string | ldflags `main.version` via `app.BuildRevision()` |
| `Path` | string | `os.Executable()` resolved with `filepath.EvalSymlinks` |
| `Writable` | bool | can the current user replace the file/dir |
| `InstallMethod` | enum | `direct` \| `homebrew` \| `container` \| `dev` (see Decision 5) |

**State / eligibility transitions** (drives the command's control flow):

```
InstalledBinary.InstallMethod == dev        → REFUSE (not a released build)
InstalledBinary.InstallMethod == homebrew   → REFUSE (use `brew upgrade`)
InstalledBinary.InstallMethod == container  → REFUSE (pull newer image tag)
InstalledBinary.Writable == false           → REFUSE (permissions; show path)
semver.Compare(installed, target) >= 0      → NO-OP (already up to date)
otherwise                                   → PROCEED to download/verify/apply
```

## Entity: Checksums

Parsed from the release's `SHA256SUMS` asset.

| Field | Type | Notes |
|-------|------|-------|
| `ByFilename` | map[string]string | filename → lowercase hex SHA-256 |

Format: standard `sha256sum` output — `<hex>␠␠<filename>` per line. The updater
looks up `PlatformTarget.AssetName` and passes the expected digest to
`selfupdate.Apply` for verification before the swap.

## Entity: UpdateResult (returned to the command layer)

| Field | Type | Notes |
|-------|------|-------|
| `FromVersion` | string | installed version before the run |
| `ToVersion` | string | target version |
| `Action` | enum | `updated` \| `already-current` \| `available` (check mode) \| `refused` |
| `RefusedReason` | string | populated when `Action == refused` |
| `DetailsURL` | string | release HTML URL |
