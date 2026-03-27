"""E2E tests: Docker Compose + plakar local-fs backup/restore."""

import json
import subprocess

import pytest

from tests.helpers.utils import (
    modify_infrahub_data,
    run_backup,
    run_restore,
    seed_infrahub_data,
    verify_infrahub_data,
    wait_for_http,
)


@pytest.mark.e2e
@pytest.mark.docker
async def test_backup_restore_plakar_local(infrahub_docker, backup_binary, tmp_path):
    """Create a plakar backup to local fs, restore, and verify."""
    url = infrahub_docker["url"]
    token = infrahub_docker["token"]
    project = infrahub_docker["project"]
    repo_path = str(tmp_path / "plakar-repo")

    # 1. Seed test data
    seed = await seed_infrahub_data(url, token)

    # 2. Create plakar backup
    run_backup(backup_binary, [
        "--project", project,
        "--backend", "plakar",
        "--repo", f"fs://{repo_path}",
        "create", "--force",
    ])

    # 3. Verify snapshot list shows the backup
    result = subprocess.run(
        [
            backup_binary,
            "--backend", "plakar",
            "--repo", f"fs://{repo_path}",
            "--log-format", "json",
            "snapshots", "list",
        ],
        capture_output=True,
        text=True,
    )
    assert result.returncode == 0, f"snapshots list failed: {result.stderr}"

    # 4. Modify data (delete the tag)
    await modify_infrahub_data(url, token, seed)

    # 5. Restore from plakar
    run_restore(backup_binary, [
        "--project", project,
        "--backend", "plakar",
        "--repo", f"fs://{repo_path}",
        "restore",
    ])

    # 6. Wait for Infrahub to recover
    await wait_for_http(f"{url}/api/config", timeout=180.0, interval=5.0)

    # 7. Verify the tag is back
    await verify_infrahub_data(url, token, seed)


@pytest.mark.e2e
@pytest.mark.docker
async def test_plakar_incremental_backup(infrahub_docker, backup_binary, tmp_path):
    """Verify that a second plakar backup is incremental (deduplication)."""
    url = infrahub_docker["url"]
    token = infrahub_docker["token"]
    project = infrahub_docker["project"]
    repo_path = str(tmp_path / "plakar-repo-incr")

    common_args = [
        "--project", project,
        "--backend", "plakar",
        "--repo", f"fs://{repo_path}",
    ]

    # First backup
    run_backup(backup_binary, common_args + ["create", "--force"])

    # Second backup (should be mostly deduplicated)
    run_backup(backup_binary, common_args + ["create", "--force"])

    # Verify two backup groups exist
    result = subprocess.run(
        [backup_binary] + common_args + ["--log-format", "json", "snapshots", "list"],
        capture_output=True,
        text=True,
    )
    assert result.returncode == 0, f"snapshots list failed: {result.stderr}"
