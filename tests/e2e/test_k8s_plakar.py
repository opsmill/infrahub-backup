"""E2E tests: Kubernetes (vcluster) + plakar local-fs backup/restore."""

import os
import subprocess

import pytest

from tests.e2e.conftest import portforward_infrahub
from tests.helpers.utils import (
    modify_infrahub_data,
    run_backup,
    run_restore,
    seed_infrahub_data,
    verify_infrahub_data,
)


@pytest.mark.e2e
@pytest.mark.k8s
async def test_backup_restore_k8s_plakar_local(
    reset_database_pod, infrahub_k8s, backup_binary, tmp_path
):
    """K8s: Create a plakar backup to local fs, restore, and verify."""
    token = infrahub_k8s["token"]
    namespace = infrahub_k8s["namespace"]
    kubeconfig = infrahub_k8s["kubeconfig_path"]
    repo_path = str(tmp_path / "plakar-repo")

    env = {"KUBECONFIG": kubeconfig}

    common_args = [
        "--k8s-namespace",
        namespace,
        "--backend",
        "plakar",
        "--repo",
        f"fs://{repo_path}",
    ]

    # 1. Seed test data (fresh port-forward)
    async with portforward_infrahub(kubeconfig, namespace) as url:
        seed = await seed_infrahub_data(url, token)

    # 2. Create plakar backup
    run_backup(backup_binary, common_args + ["create", "--force"], env=env)

    # 3. Verify snapshot list
    result = subprocess.run(
        [backup_binary] + common_args + ["--log-format", "json", "snapshots", "list"],
        capture_output=True,
        text=True,
        env={**os.environ, **env},
    )
    assert result.returncode == 0, f"snapshots list failed: {result.stderr}"

    # 4. Modify data (fresh port-forward)
    async with portforward_infrahub(kubeconfig, namespace) as url:
        await modify_infrahub_data(url, token, seed)

    # 5. Restore from plakar (may restart pods)
    run_restore(backup_binary, common_args + ["restore"], env=env)

    # 6. Verify the tag is back (fresh port-forward after pod restart)
    async with portforward_infrahub(kubeconfig, namespace) as url:
        await verify_infrahub_data(url, token, seed)
