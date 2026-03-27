"""E2E tests: Docker Compose + local tarball backup/restore."""

import pytest

from tests.helpers.utils import (
    find_latest_backup,
    modify_infrahub_data,
    run_backup,
    run_restore,
    seed_infrahub_data,
    verify_infrahub_data,
    wait_for_http,
)


@pytest.mark.e2e
@pytest.mark.docker
async def test_backup_restore_local_tarball(infrahub_docker, backup_binary, tmp_path):
    """Create a local tarball backup, modify data, restore, and verify."""
    url = infrahub_docker["url"]
    token = infrahub_docker["token"]
    project = infrahub_docker["project"]
    backup_dir = str(tmp_path / "backups")

    # 1. Seed test data
    seed = await seed_infrahub_data(url, token)

    # 2. Create backup
    run_backup(backup_binary, [
        "--project", project,
        "--backup-dir", backup_dir,
        "create", "--force",
    ])
    backup_file = find_latest_backup(backup_dir)

    # 3. Modify data (delete the tag)
    await modify_infrahub_data(url, token, seed)

    # 4. Restore from backup
    run_restore(backup_binary, [
        "--project", project,
        "restore", str(backup_file),
    ])

    # 5. Wait for Infrahub to recover after restore
    await wait_for_http(f"{url}/api/config", timeout=180.0, interval=5.0)

    # 6. Verify the tag is back
    await verify_infrahub_data(url, token, seed)
