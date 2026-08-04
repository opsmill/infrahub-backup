"""E2E tests: Docker Compose + local tarball backup/restore."""

from datetime import datetime, timedelta
from pathlib import Path

import pytest
from infrahub_sdk.testing.docker import TestInfrahubDockerClient

from tests.helpers.utils import (
    compose_container_runtimes,
    find_latest_backup,
    modify_infrahub_data,
    run_backup,
    run_cli,
    run_restore,
    seed_infrahub_data,
    verify_infrahub_data,
    wait_for_http,
)

ADMIN_TOKEN = "06438eb2-8019-4776-878c-0941b1f1d1ec"


def _archives(backup_dir: Path) -> list[str]:
    """Every backup archive in the directory, oldest name first.

    The timestamp format sorts lexicographically in chronological order, so a plain sort
    is the same ranking `--latest` applies.
    """
    return sorted(path.name for path in backup_dir.iterdir() if path.name.startswith("infrahub_backup_"))


def _older_archive_name(name: str, behind: timedelta = timedelta(hours=1)) -> str:
    """The name an archive created `behind` before the given one would carry.

    Derived from the name rather than the clock so the decoy is unambiguously the older
    entry in the pool whatever the run's timing.
    """
    stamp = name.removeprefix("infrahub_backup_").removesuffix(".tar.gz")
    older = datetime.strptime(stamp, "%Y%m%d_%H%M%S") - behind
    return f"infrahub_backup_{older.strftime('%Y%m%d_%H%M%S')}.tar.gz"


@pytest.mark.e2e
@pytest.mark.docker
class TestDockerTarball(TestInfrahubDockerClient):
    """Local tarball backup and restore against one class-scoped compose stack.

    The restoring tests stop and start application containers, so the empty-pool case —
    which must leave the deployment alone — runs before them.
    """

    async def test_backup_restore_local_tarball(self, infrahub_compose, infrahub_port, backup_binary, tmp_path):
        """Create a local tarball backup, modify data, restore, and verify."""
        url = f"http://localhost:{infrahub_port}"
        project = infrahub_compose.project_name
        backup_dir = str(tmp_path / "backups")

        # 1. Seed test data
        seed = await seed_infrahub_data(url, ADMIN_TOKEN)

        # 2. Create backup
        run_backup(
            backup_binary,
            [
                "--project",
                project,
                "--backup-dir",
                backup_dir,
                "create",
                "--force",
            ],
        )
        backup_file = find_latest_backup(backup_dir)

        # 6. Wait for Infrahub to recover
        await wait_for_http(f"{url}/api/config", timeout=180.0, interval=5.0)

        # 3. Modify data (delete the tag)
        await modify_infrahub_data(url, ADMIN_TOKEN, seed)

        # 4. Restore from backup
        run_restore(
            backup_binary,
            [
                "--project",
                project,
                "restore",
                str(backup_file),
            ],
        )

        # 5. Wait for Infrahub to recover after restore
        await wait_for_http(f"{url}/api/config", timeout=180.0, interval=5.0)

        # 6. Verify the tag is back
        await verify_infrahub_data(url, ADMIN_TOKEN, seed)

    async def test_restore_latest_empty_pool_is_an_error(
        self, infrahub_compose, infrahub_port, backup_binary, tmp_path
    ):
        """`restore --latest` over a backup directory holding no archives fails cleanly.

        Quickstart Scenario 4 / FR-008: an empty pool is the expected state before the
        first backup ever runs, and the one state a scheduled restore must never read as a
        successful no-op. The error names the directory that was looked in, and the
        deployment is left exactly as it was found.
        """
        url = f"http://localhost:{infrahub_port}"
        project = infrahub_compose.project_name
        empty_pool = tmp_path / "empty-pool"
        empty_pool.mkdir()

        before = compose_container_runtimes(project)

        # BACKUP_DIR rather than --backup-dir: the environment-variable form a scheduled
        # job configures, which is how quickstart Scenario 4 invokes it.
        result = run_cli(
            backup_binary,
            ["--project", project, "restore", "--latest"],
            env={"BACKUP_DIR": str(empty_pool)},
        )

        # 1. Non-zero exit with the pool named (FR-008).
        assert result.returncode != 0, f"expected a non-zero exit:\n{result.stdout}\n{result.stderr}"
        output = result.stdout + result.stderr
        assert f"no backups found at local:{empty_pool}" in output, (
            f"the empty pool was not reported clearly:\n{output}"
        )

        # 2. Nothing was restored and nothing was touched.
        assert "Starting backup restore" not in output, f"a restore was attempted over an empty pool:\n{output}"
        assert compose_container_runtimes(project) == before, "the deployment was touched despite the empty pool"
        assert not list(empty_pool.iterdir()), f"the rejected pool was written into: {list(empty_pool.iterdir())}"

        await wait_for_http(f"{url}/api/config", timeout=180.0, interval=5.0)

    async def test_restore_latest_from_local_pool(self, infrahub_compose, infrahub_port, backup_binary, tmp_path):
        """`restore --latest` restores the newest archive in the local backup directory.

        Quickstart Scenario 1: no filename is passed, the pool holds more than one entry, and
        the newest is the archive that gets restored — named, together with the pool it came
        out of, before the restore starts (FR-001, FR-005, FR-009).

        The older entry is fabricated rather than backed up. Selection ranks names and never
        contents, so a name-only decoy is a full member of the pool, and one holding no
        archive at all is the stronger control: had it been ranked first, the restore would
        have failed outright instead of restoring the archive the audit line names. A second
        real `create` is also not available here — it leaves its dump in the same container
        directory a later restore copies into, which breaks the shared Neo4j restore path
        however the archive was chosen.
        """
        url = f"http://localhost:{infrahub_port}"
        project = infrahub_compose.project_name
        backup_dir = tmp_path / "latest-pool"

        # 1. Seed the data whose return proves the restore completed.
        seed = await seed_infrahub_data(url, ADMIN_TOKEN)

        # 2. The archive that must win.
        run_backup(backup_binary, ["--project", project, "--backup-dir", str(backup_dir), "create", "--force"])
        created = _archives(backup_dir)
        assert len(created) == 1, f"expected exactly one archive after the backup, found {created}"
        newest = created[0]

        # 3. An older entry, so "the newest" is a real choice rather than the only option.
        older = _older_archive_name(newest)
        (backup_dir / older).write_text("an older archive --latest must never rank first")
        assert older < newest, f"{older} must be older than {newest}"

        await wait_for_http(f"{url}/api/config", timeout=180.0, interval=5.0)

        # 4. Delete the tag whose return proves the restore happened.
        await modify_infrahub_data(url, ADMIN_TOKEN, seed)

        # 5. Restore without naming an archive. A non-zero exit raises (FR-001).
        result = run_restore(
            backup_binary,
            ["--project", project, "--backup-dir", str(backup_dir), "restore", "--latest"],
        )

        # 6. The run says which archive it chose and out of which pool (FR-009).
        output = result.stdout + result.stderr
        expected_line = f"Restoring latest backup {newest} from local:{backup_dir}"
        assert expected_line in output, f"selection was not reported as {expected_line!r}:\n{output}"

        # 7. The newest archive is the one the restore path was actually given (FR-005).
        assert str(backup_dir / newest) in output, f"{newest} was not the archive restored:\n{output}"
        assert older not in output, f"the older archive was ranked first:\n{output}"

        # 8. The restore ran to completion and the data is back.
        await wait_for_http(f"{url}/api/config", timeout=180.0, interval=5.0)
        await verify_infrahub_data(url, ADMIN_TOKEN, seed)
