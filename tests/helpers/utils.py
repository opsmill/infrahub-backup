import os
import subprocess
import uuid
from pathlib import Path

import httpx


async def wait_for_http(
    url: str,
    timeout: float = 120.0,
    interval: float = 2.0,
    expected_status: int = 200,
) -> None:
    """Poll an HTTP endpoint until it returns the expected status code."""
    import asyncio
    import time

    start = time.time()
    async with httpx.AsyncClient() as client:
        while time.time() - start < timeout:
            try:
                resp = await client.get(url, timeout=5)
                if resp.status_code == expected_status:
                    return
            except httpx.HTTPError:
                pass
            await asyncio.sleep(interval)
    msg = f"{url} did not return {expected_status} after {timeout}s"
    raise TimeoutError(msg)


async def seed_infrahub_data(infrahub_url: str, token: str) -> dict:
    """Create a BuiltinTag in Infrahub for backup/restore verification.

    Returns a dict with the tag_name that can be used for verification.
    """
    from infrahub_sdk import Config as InfrahubConfig
    from infrahub_sdk import InfrahubClient

    config = InfrahubConfig(address=infrahub_url, api_token=token)
    client = InfrahubClient(config=config)

    tag_name = f"e2e-backup-test-{uuid.uuid4().hex[:8]}"
    tag = await client.create(kind="BuiltinTag", name=tag_name)
    await tag.save()

    return {"tag_name": tag_name}


async def verify_infrahub_data(infrahub_url: str, token: str, expected: dict) -> None:
    """Verify the seeded tag exists after restore."""
    from infrahub_sdk import Config as InfrahubConfig
    from infrahub_sdk import InfrahubClient

    config = InfrahubConfig(address=infrahub_url, api_token=token)
    client = InfrahubClient(config=config)

    tag = await client.get(kind="BuiltinTag", name__value=expected["tag_name"])
    assert tag.name.value == expected["tag_name"], (
        f"Expected tag '{expected['tag_name']}' but got '{tag.name.value}'"
    )


async def modify_infrahub_data(infrahub_url: str, token: str, data: dict) -> None:
    """Delete the seeded tag so restore can prove it reverted."""
    from infrahub_sdk import Config as InfrahubConfig
    from infrahub_sdk import InfrahubClient

    config = InfrahubConfig(address=infrahub_url, api_token=token)
    client = InfrahubClient(config=config)

    tag = await client.get(kind="BuiltinTag", name__value=data["tag_name"])
    await tag.delete()

    # Verify deletion
    tags = await client.all(kind="BuiltinTag")
    assert all(t.name.value != data["tag_name"] for t in tags), (
        f"Tag '{data['tag_name']}' still exists after deletion"
    )


def run_backup(
    binary: str,
    extra_args: list[str],
    env: dict[str, str] | None = None,
) -> subprocess.CompletedProcess:
    """Run infrahub-backup create with the given arguments."""
    cmd = [binary] + extra_args
    run_env = {**os.environ, **(env or {})}
    result = subprocess.run(cmd, capture_output=True, text=True, env=run_env)
    if result.returncode != 0:
        raise RuntimeError(
            f"Backup failed (exit {result.returncode}):\n"
            f"stdout: {result.stdout}\n"
            f"stderr: {result.stderr}"
        )
    return result


def run_restore(
    binary: str,
    extra_args: list[str],
    env: dict[str, str] | None = None,
) -> subprocess.CompletedProcess:
    """Run infrahub-backup restore with the given arguments."""
    cmd = [binary] + extra_args
    run_env = {**os.environ, **(env or {})}
    result = subprocess.run(cmd, capture_output=True, text=True, env=run_env)
    if result.returncode != 0:
        raise RuntimeError(
            f"Restore failed (exit {result.returncode}):\n"
            f"stdout: {result.stdout}\n"
            f"stderr: {result.stderr}"
        )
    return result


def find_latest_backup(backup_dir: str | Path) -> Path:
    """Find the most recent infrahub_backup_*.tar.gz in a directory."""
    backup_dir = Path(backup_dir)
    backups = sorted(backup_dir.glob("infrahub_backup_*.tar.gz"))
    if not backups:
        raise FileNotFoundError(f"No backup files found in {backup_dir}")
    return backups[-1]


def list_s3_backups(
    bucket: str,
    prefix: str,
    endpoint: str,
    access_key: str,
    secret_key: str,
) -> list[str]:
    """List backup objects in an S3 bucket."""
    import boto3

    s3 = boto3.client(
        "s3",
        endpoint_url=endpoint,
        aws_access_key_id=access_key,
        aws_secret_access_key=secret_key,
        region_name="us-east-1",
    )
    response = s3.list_objects_v2(Bucket=bucket, Prefix=prefix)
    return [obj["Key"] for obj in response.get("Contents", [])]


def get_s3_backup_key(
    bucket: str,
    prefix: str,
    endpoint: str,
    access_key: str,
    secret_key: str,
) -> str:
    """Get the S3 key of the latest backup file."""
    keys = list_s3_backups(bucket, prefix, endpoint, access_key, secret_key)
    backup_keys = [k for k in keys if k.endswith(".tar.gz")]
    if not backup_keys:
        raise FileNotFoundError(f"No backup files found in s3://{bucket}/{prefix}")
    return backup_keys[-1]
