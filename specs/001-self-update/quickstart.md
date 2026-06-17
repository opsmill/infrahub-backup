# Quickstart: Self-Update

## For users

Check whether a newer version exists (read-only, changes nothing):

```bash
infrahub-backup update --check
# → update available: v1.7.3 → v1.8.0  (https://github.com/opsmill/infrahub-backup/releases/tag/v1.8.0)
# or
# → already up to date (v1.8.0)
```

Update to the latest release:

```bash
infrahub-backup update
# shows: v1.7.3 → v1.8.0, prompts [y/N], then:
# → updated v1.7.3 → v1.8.0
```

Update unattended (CI / scripts):

```bash
infrahub-backup update --yes
```

Pin to / downgrade to a specific version:

```bash
infrahub-backup update --version v1.7.2 --yes
```

The same commands work for `infrahub-taskmanager` and update that binary.

### When self-update is declined

| You see | Do this instead |
|---------|-----------------|
| "managed by Homebrew" | `brew upgrade infrahub-backup` |
| "running in a container" | pull a newer image tag |
| "development build" | build/install a released version |
| "permission denied: `<path>`" | re-run with `sudo` or as a user who can write the binary |

## For maintainers — verifying the release pipeline

After cutting a release, confirm the GitHub Release has the required assets:

```bash
gh release view vX.Y.Z --repo opsmill/infrahub-backup --json assets \
  --jq '.assets[].name'
# expect: 12 platform binaries + SHA256SUMS
```

## Developer validation checklist (maps to acceptance scenarios)

1. **US1 happy path** — build at an older `VERSION`, run `update --yes` against a
   real/newer release, confirm `version` reports the new tag and the binary runs.
2. **US1 no-op** — run `update` on the latest; expect "already up to date", no
   file change (compare mtime/checksum before/after).
3. **US1 rollback** — point the updater at a corrupt asset (checksum mismatch);
   expect abort and the original binary still runnable.
4. **US2 check** — `update --check` on an older build reports availability and
   leaves the binary byte-identical.
5. **US3 confirm** — interactive run prompts; `--yes` skips; non-interactive
   without `--yes` refuses with guidance.
6. **Refusals** — simulate Homebrew path, `/.dockerenv`, empty version, and a
   read-only binary dir; each refuses with the correct message and exits 1.

Unit tests use `httptest.Server` to stand in for the GitHub API and a temp-dir
fake binary for apply/rollback, so no network or real release is needed in CI.
