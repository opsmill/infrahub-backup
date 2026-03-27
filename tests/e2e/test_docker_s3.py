"""E2E tests: Docker Compose + S3 tarball backup/restore."""

import pytest

from tests.helpers.utils import (
    get_s3_backup_key,
    modify_infrahub_data,
    run_backup,
    run_restore,
    seed_infrahub_data,
    verify_infrahub_data,
    wait_for_http,
)


@pytest.mark.e2e
@pytest.mark.docker
async def test_backup_restore_s3_tarball(infrahub_docker, backup_binary, minio_docker, tmp_path):
    """Create a tarball backup uploaded to S3, restore from S3, and verify."""
    url = infrahub_docker["url"]
    token = infrahub_docker["token"]
    project = infrahub_docker["project"]
    minio = minio_docker

    s3_env = {
        "AWS_ACCESS_KEY_ID": minio["access_key"],
        "AWS_SECRET_ACCESS_KEY": minio["secret_key"],
    }

    # 1. Seed test data
    seed = await seed_infrahub_data(url, token)

    # 2. Create backup with S3 upload
    run_backup(
        backup_binary,
        [
            "--project", project,
            "--backup-dir", str(tmp_path),
            "--s3-bucket", minio["bucket"],
            "--s3-endpoint", minio["endpoint"],
            "--s3-region", "us-east-1",
            "create", "--force", "--s3-upload",
        ],
        env=s3_env,
    )

    # 3. Find the backup key in S3
    s3_key = get_s3_backup_key(
        bucket=minio["bucket"],
        prefix="",
        endpoint=minio["endpoint"],
        access_key=minio["access_key"],
        secret_key=minio["secret_key"],
    )
    s3_uri = f"s3://{minio['bucket']}/{s3_key}"

    # 4. Modify data (delete the tag)
    await modify_infrahub_data(url, token, seed)

    # 5. Restore from S3
    run_restore(
        backup_binary,
        [
            "--project", project,
            "--s3-bucket", minio["bucket"],
            "--s3-endpoint", minio["endpoint"],
            "--s3-region", "us-east-1",
            "restore", s3_uri,
        ],
        env=s3_env,
    )

    # 6. Wait for Infrahub to recover
    await wait_for_http(f"{url}/api/config", timeout=180.0, interval=5.0)

    # 7. Verify the tag is back
    await verify_infrahub_data(url, token, seed)
