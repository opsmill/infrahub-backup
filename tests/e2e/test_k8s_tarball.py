"""E2E tests: Kubernetes (vcluster) + local tarball backup/restore."""

import pytest

from tests.e2e.conftest import portforward_infrahub
from tests.helpers.utils import (
    find_latest_backup,
    modify_infrahub_data,
    run_backup,
    run_restore,
    seed_infrahub_data,
    verify_infrahub_data,
)


@pytest.mark.e2e
@pytest.mark.k8s
async def test_backup_restore_k8s_local_tarball(infrahub_k8s, backup_binary, tmp_path):
    """K8s: Create a local tarball backup, modify data, restore, and verify."""
    token = infrahub_k8s["token"]
    namespace = infrahub_k8s["namespace"]
    kubeconfig = infrahub_k8s["kubeconfig_path"]
    backup_dir = str(tmp_path / "backups")

    env = {"KUBECONFIG": kubeconfig}

    # 1. Seed test data (fresh port-forward)
    async with portforward_infrahub(kubeconfig, namespace) as url:
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

    # 3. Modify data (fresh port-forward)
    async with portforward_infrahub(kubeconfig, namespace) as url:
        await modify_infrahub_data(url, token, seed)

    # 4. Restore from backup (may restart pods)
    run_restore(
        backup_binary,
        [
            "--k8s-namespace", namespace,
            "restore", str(backup_file),
        ],
        env=env,
    )

    # 5. Verify the tag is back (fresh port-forward after pod restart)
    async with portforward_infrahub(kubeconfig, namespace) as url:
        await verify_infrahub_data(url, token, seed)
