"""E2E tests: Docker Compose + retention policy applied by `create`."""

from datetime import datetime, timedelta
from pathlib import Path

import pytest
from infrahub_sdk.testing.docker import TestInfrahubDockerClient

from tests.helpers.utils import run_backup, wait_for_http

ADMIN_TOKEN = "06438eb2-8019-4776-878c-0941b1f1d1ec"

# Files that share the backup directory but are not backups: retention must never
# touch them, including the name that looks like a backup but carries no timestamp.
DECOYS = ["notes.txt", "infrahub_backup_garbage.tar.gz", "somebackup.tar.gz"]

# Ages in days of the fabricated archives, relative to the moment the test starts.
OUT_OF_POLICY_AGES = [20, 25, 30]
IN_POLICY_AGES = [1, 2]
IN_POLICY_ENCRYPTED_AGE = 3


def _archive_name(age_days: int, encrypted: bool = False) -> str:
    """Render the archive name a backup created age_days ago would carry."""
    timestamp = (datetime.now() - timedelta(days=age_days)).strftime("%Y%m%d_%H%M%S")
    return f"infrahub_backup_{timestamp}.tar.gz" + (".enc" if encrypted else "")


def _seed_fixtures(backup_dir: Path) -> tuple[list[str], list[str]]:
    """Fill backup_dir with archives spanning the policy boundary, plus decoys.

    Returns the (out_of_policy, survivors) name lists, where survivors covers both
    the in-policy archives and the decoys.
    """
    backup_dir.mkdir(parents=True, exist_ok=True)

    out_of_policy = [_archive_name(age) for age in OUT_OF_POLICY_AGES]
    in_policy = [_archive_name(age) for age in IN_POLICY_AGES]
    in_policy.append(_archive_name(IN_POLICY_ENCRYPTED_AGE, encrypted=True))

    for name in out_of_policy + in_policy + DECOYS:
        (backup_dir / name).write_text("retention fixture")

    return out_of_policy, in_policy + DECOYS


def _names(backup_dir: Path) -> set[str]:
    return {path.name for path in backup_dir.iterdir()}


def _real_backups(backup_dir: Path, fixtures: set[str]) -> set[str]:
    """Return the archives `create` actually produced, excluding the fixtures."""
    return {name for name in _names(backup_dir) if name.startswith("infrahub_backup_")} - fixtures


@pytest.mark.e2e
@pytest.mark.docker
class TestDockerRetention(TestInfrahubDockerClient):
    async def test_create_applies_retention(self, infrahub_compose, infrahub_port, backup_binary, tmp_path):
        """`create` prunes out-of-policy archives only when retention is configured."""
        url = f"http://localhost:{infrahub_port}"
        project = infrahub_compose.project_name
        backup_dir = tmp_path / "backups"

        out_of_policy, survivors = _seed_fixtures(backup_dir)
        fixtures = set(out_of_policy) | set(survivors)

        # 1. Back up with a retention policy. The count rule is 3 rather than the
        #    14 of a realistic schedule so that this small fixture set actually
        #    crosses the boundary: union semantics mean a count larger than the
        #    number of archives present would claim every one of them.
        result = run_backup(
            backup_binary,
            [
                "--project",
                project,
                "--backup-dir",
                str(backup_dir),
                "create",
                "--force",
                "--retention-days",
                "7",
                "--retention-count",
                "3",
            ],
        )

        # 2. The new archive plus every in-policy archive and every decoy remain,
        #    and exactly the out-of-policy archives are gone (US1 scenario 1).
        pruned_run_backups = _real_backups(backup_dir, fixtures)
        assert len(pruned_run_backups) == 1, f"expected exactly one new archive, found {pruned_run_backups}"
        assert _names(backup_dir) == set(survivors) | pruned_run_backups, (
            f"unexpected backup directory contents: {sorted(_names(backup_dir))}"
        )

        # 3. Every deletion is reported (FR-009).
        output = result.stdout + result.stderr
        for name in out_of_policy:
            assert f"Pruned backup {name}" in output, f"deletion of {name} was not reported:\n{output}"

        # 4. Wait for Infrahub to recover before the second backup.
        await wait_for_http(f"{url}/api/config", timeout=180.0, interval=5.0)

        # 5. Without retention options nothing is pruned (US1 scenario 3).
        for name in out_of_policy:
            (backup_dir / name).write_text("retention fixture")
        before = _names(backup_dir)

        result = run_backup(
            backup_binary,
            [
                "--project",
                project,
                "--backup-dir",
                str(backup_dir),
                "create",
                "--force",
            ],
        )

        plain_run_backups = _real_backups(backup_dir, fixtures) - pruned_run_backups
        assert len(plain_run_backups) == 1, f"expected exactly one new archive, found {plain_run_backups}"
        assert _names(backup_dir) == before | plain_run_backups, (
            f"a run without retention options changed the directory: {sorted(_names(backup_dir))}"
        )
        assert "Pruned backup" not in (result.stdout + result.stderr), "retention ran without being configured"

        await wait_for_http(f"{url}/api/config", timeout=180.0, interval=5.0)
