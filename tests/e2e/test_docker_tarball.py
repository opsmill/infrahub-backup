"""E2E tests: Docker Compose + local tarball backup/restore."""

import pytest
from infrahub_sdk.testing.docker import TestInfrahubDockerClient

from tests.helpers.utils import (
    find_latest_backup,
    modify_infrahub_data,
    run_backup,
    run_restore,
    seed_infrahub_data,
    verify_infrahub_data,
    wait_for_http,
)

ADMIN_TOKEN = "06438eb2-8019-4776-878c-0941b1f1d1ec"


@pytest.mark.e2e
@pytest.mark.docker
class TestDockerTarball(TestInfrahubDockerClient):
    async def test_backup_restore_local_tarball(
        self, infrahub_compose, infrahub_port, backup_binary, tmp_path
    ):
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
