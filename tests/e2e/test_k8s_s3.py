"""E2E tests: Kubernetes (vcluster) + S3 tarball backup/restore."""

import pytest

from tests.e2e.conftest import portforward_infrahub
from tests.helpers.utils import (
    get_s3_backup_key,
    modify_infrahub_data,
    run_backup,
    run_restore,
    seed_infrahub_data,
    verify_infrahub_data,
)


@pytest.mark.e2e
@pytest.mark.k8s
async def test_backup_restore_k8s_s3_tarball(infrahub_k8s, backup_binary, minio_k8s, tmp_path):
    """K8s: Create a tarball backup uploaded to S3, restore from S3, and verify."""
    token = infrahub_k8s["token"]
    namespace = infrahub_k8s["namespace"]
    kubeconfig = infrahub_k8s["kubeconfig_path"]
    minio = minio_k8s

    env = {
        "KUBECONFIG": kubeconfig,
        "AWS_ACCESS_KEY_ID": minio["access_key"],
        "AWS_SECRET_ACCESS_KEY": minio["secret_key"],
    }

    # 1. Seed test data (fresh port-forward)
    async with portforward_infrahub(kubeconfig, namespace) as url:
        seed = await seed_infrahub_data(url, token)

    # 2. Create backup with S3 upload
    run_backup(
        backup_binary,
        [
            "--k8s-namespace", namespace,
            "--backup-dir", str(tmp_path),
            "--s3-bucket", minio["bucket"],
            "--s3-endpoint", minio["local_endpoint"],
            "--s3-region", "us-east-1",
            "create", "--force", "--s3-upload",
        ],
        env=env,
    )

    # 3. Find the backup key in S3
    s3_key = get_s3_backup_key(
        bucket=minio["bucket"],
        prefix="",
        endpoint=minio["local_endpoint"],
        access_key=minio["access_key"],
        secret_key=minio["secret_key"],
    )
    s3_uri = f"s3://{minio['bucket']}/{s3_key}"

    # 4. Modify data (fresh port-forward)
    async with portforward_infrahub(kubeconfig, namespace) as url:
        await modify_infrahub_data(url, token, seed)

    # 5. Restore from S3 (may restart pods)
    run_restore(
        backup_binary,
        [
            "--k8s-namespace", namespace,
            "--s3-bucket", minio["bucket"],
            "--s3-endpoint", minio["local_endpoint"],
            "--s3-region", "us-east-1",
            "restore", s3_uri,
        ],
        env=env,
    )

    # 6. Verify the tag is back (fresh port-forward after pod restart)
    async with portforward_infrahub(kubeconfig, namespace) as url:
        await verify_infrahub_data(url, token, seed)
