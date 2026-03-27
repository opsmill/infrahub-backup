"""E2E tests: Kubernetes (vcluster) + local tarball backup/restore."""

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
@pytest.mark.k8s
async def test_backup_restore_k8s_local_tarball(infrahub_k8s, backup_binary, tmp_path):
    """K8s: Create a local tarball backup, modify data, restore, and verify."""
    url = infrahub_k8s["url"]
    token = infrahub_k8s["token"]
    namespace = infrahub_k8s["namespace"]
    kubeconfig = infrahub_k8s["kubeconfig_path"]
    backup_dir = str(tmp_path / "backups")

    env = {"KUBECONFIG": kubeconfig}

    # 1. Seed test data
    seed = await seed_infrahub_data(url, token)

    # 2. Create backup
    run_backup(
        backup_binary,
        [
            "--k8s-namespace", namespace,
            "--backup-dir", backup_dir,
            "create", "--force",
        ],
        env=env,
    )
    backup_file = find_latest_backup(backup_dir)

    # 3. Modify data (delete the tag)
    await modify_infrahub_data(url, token, seed)

    # 4. Restore from backup
    run_restore(
        backup_binary,
        [
            "--k8s-namespace", namespace,
            "restore", str(backup_file),
        ],
        env=env,
    )

    # 5. Wait for Infrahub to recover
    await wait_for_http(f"{url}/api/config", timeout=180.0, interval=5.0)

    # 6. Verify the tag is back
    await verify_infrahub_data(url, token, seed)
