"""E2E tests: Kubernetes (vcluster) + plakar local-fs backup/restore."""

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
@pytest.mark.k8s
async def test_backup_restore_k8s_plakar_local(infrahub_k8s, backup_binary, tmp_path):
    """K8s: Create a plakar backup to local fs, restore, and verify."""
    url = infrahub_k8s["url"]
    token = infrahub_k8s["token"]
    namespace = infrahub_k8s["namespace"]
    kubeconfig = infrahub_k8s["kubeconfig_path"]
    repo_path = str(tmp_path / "plakar-repo")

    env = {"KUBECONFIG": kubeconfig}

    common_args = [
        "--k8s-namespace", namespace,
        "--backend", "plakar",
        "--repo", f"fs://{repo_path}",
    ]

    # 1. Seed test data
    seed = await seed_infrahub_data(url, token)

    # 2. Create plakar backup
    run_backup(backup_binary, common_args + ["create", "--force"], env=env)

    # 3. Verify snapshot list
    result = subprocess.run(
        [backup_binary] + common_args + ["--log-format", "json", "snapshots", "list"],
        capture_output=True,
        text=True,
        env={**__import__("os").environ, **env},
    )
    assert result.returncode == 0, f"snapshots list failed: {result.stderr}"

    # 4. Modify data (delete the tag)
    await modify_infrahub_data(url, token, seed)

    # 5. Restore from plakar
    run_restore(backup_binary, common_args + ["restore"], env=env)

    # 6. Wait for Infrahub to recover
    await wait_for_http(f"{url}/api/config", timeout=180.0, interval=5.0)

    # 7. Verify the tag is back
    await verify_infrahub_data(url, token, seed)
