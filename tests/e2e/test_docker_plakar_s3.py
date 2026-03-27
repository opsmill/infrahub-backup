"""E2E tests: Docker Compose + plakar S3 backup/restore."""

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
async def test_backup_restore_plakar_s3(infrahub_docker, backup_binary, minio_docker):
    """Create a plakar backup to S3, restore, and verify."""
    url = infrahub_docker["url"]
    token = infrahub_docker["token"]
    project = infrahub_docker["project"]
    minio = minio_docker

    # Build the S3 repo URI using the MinIO endpoint
    # Extract host:port from the endpoint URL for the plakar S3 URI
    endpoint_no_scheme = minio["endpoint"].replace("http://", "")
    repo_uri = f"s3://{minio['access_key']}:{minio['secret_key']}@{endpoint_no_scheme}/{minio['bucket']}/plakar-e2e"

    s3_env = {
        "AWS_ACCESS_KEY_ID": minio["access_key"],
        "AWS_SECRET_ACCESS_KEY": minio["secret_key"],
    }

    common_args = [
        "--project", project,
        "--backend", "plakar",
        "--repo", repo_uri,
    ]

    # 1. Seed test data
    seed = await seed_infrahub_data(url, token)

    # 2. Create plakar backup to S3
    run_backup(backup_binary, common_args + ["create", "--force"], env=s3_env)

    # 3. Verify snapshot list works with S3 repo
    result = subprocess.run(
        [backup_binary] + common_args + ["--log-format", "json", "snapshots", "list"],
        capture_output=True,
        text=True,
        env={**__import__("os").environ, **s3_env},
    )
    assert result.returncode == 0, f"snapshots list failed: {result.stderr}"

    # 4. Modify data (delete the tag)
    await modify_infrahub_data(url, token, seed)

    # 5. Restore from plakar S3
    run_restore(backup_binary, common_args + ["restore"], env=s3_env)

    # 6. Wait for Infrahub to recover
    await wait_for_http(f"{url}/api/config", timeout=180.0, interval=5.0)

    # 7. Verify the tag is back
    await verify_infrahub_data(url, token, seed)
