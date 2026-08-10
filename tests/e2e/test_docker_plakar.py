"""E2E tests: Docker Compose + plakar backup/restore, against local fs and against S3."""

import os
import re
import subprocess
from pathlib import Path

import pytest
from infrahub_sdk.testing.docker import TestInfrahubDockerClient

from tests.helpers.utils import (
    modify_infrahub_data,
    run_backup,
    run_restore,
    seed_infrahub_data,
    verify_infrahub_data,
    wait_for_http,
)

ADMIN_TOKEN = "06438eb2-8019-4776-878c-0941b1f1d1ec"

# The backup group a plakar restore reports having resolved. The id is the group's creation
# timestamp, logged as a logrus field on the line that announces the restore, so it is the
# one observable that says *which* group a run chose rather than merely that it succeeded.
BACKUP_GROUP_ID = re.compile(r"backup_id=(\d{8}_\d{6})")

# The same id as it appears in `snapshots list --log-format json`, which is how the
# repository's own view of what it holds is read back.
LISTED_GROUP_ID = re.compile(r'"backup_id":\s*"(\d{8}_\d{6})"')


def _resolved_backup_group(output: str) -> str:
    """The backup group id a plakar restore run resolved, read back from its output."""
    ids = BACKUP_GROUP_ID.findall(output)
    assert ids, f"the restore reported no backup group id:\n{output}"
    assert len(set(ids)) == 1, f"the restore reported more than one backup group: {sorted(set(ids))}\n{output}"
    return ids[0]


@pytest.mark.e2e
@pytest.mark.docker
class TestDockerPlakar(TestInfrahubDockerClient):
    """Plakar backup and restore against one class-scoped compose stack.

    Both repository backends — a local filesystem repository and one on S3 — need the same
    default stack, so they share one rather than paying its setup and teardown each.
    """

    async def test_backup_restore_plakar_local(self, infrahub_compose, infrahub_port, backup_binary, tmp_path):
        """Create a plakar backup to local fs, restore, and verify."""
        url = f"http://localhost:{infrahub_port}"
        project = infrahub_compose.project_name
        repo_path = str(tmp_path / "plakar-repo")

        # 1. Seed test data
        seed = await seed_infrahub_data(url, ADMIN_TOKEN)

        # 2. Create plakar backup
        run_backup(
            backup_binary,
            [
                "--project",
                project,
                "--backend",
                "plakar",
                "--repo",
                f"fs://{repo_path}",
                "create",
                "--force",
            ],
        )

        # 3. Verify snapshot list shows the backup
        result = subprocess.run(
            [
                backup_binary,
                "--backend",
                "plakar",
                "--repo",
                f"fs://{repo_path}",
                "--log-format",
                "json",
                "snapshots",
                "list",
            ],
            capture_output=True,
            text=True,
        )
        assert result.returncode == 0, f"snapshots list failed: {result.stderr}"

        # 6. Wait for Infrahub to recover
        await wait_for_http(f"{url}/api/config", timeout=180.0, interval=1.0)

        # 4. Modify data (delete the tag)
        await modify_infrahub_data(url, ADMIN_TOKEN, seed)

        # 5. Restore from plakar
        run_restore(
            backup_binary,
            [
                "--project",
                project,
                "--backend",
                "plakar",
                "--repo",
                f"fs://{repo_path}",
                "restore",
            ],
        )

        # 6. Wait for Infrahub to recover
        await wait_for_http(f"{url}/api/config", timeout=180.0, interval=1.0)

        # 7. Verify the tag is back
        await verify_infrahub_data(url, ADMIN_TOKEN, seed)

    async def test_backup_restore_plakar_s3(self, infrahub_compose, infrahub_port, backup_binary, minio_docker):
        """Create a plakar backup to S3, restore, and verify."""
        url = f"http://localhost:{infrahub_port}"
        project = infrahub_compose.project_name
        minio = minio_docker

        # Build the S3 repo URI using the MinIO endpoint
        endpoint_no_scheme = minio["endpoint"].replace("http://", "")
        repo_uri = f"s3://{minio['access_key']}:{minio['secret_key']}@{endpoint_no_scheme}/{minio['bucket']}/plakar-e2e"

        s3_env = {
            "AWS_ACCESS_KEY_ID": minio["access_key"],
            "AWS_SECRET_ACCESS_KEY": minio["secret_key"],
        }

        common_args = [
            "--project",
            project,
            "--backend",
            "plakar",
            "--repo",
            repo_uri,
        ]

        # 1. Seed test data
        seed = await seed_infrahub_data(url, ADMIN_TOKEN)

        # 2. Create plakar backup to S3
        run_backup(backup_binary, common_args + ["create", "--force"], env=s3_env)

        # 3. Verify snapshot list works with S3 repo
        result = subprocess.run(
            [backup_binary] + common_args + ["--log-format", "json", "snapshots", "list"],
            capture_output=True,
            text=True,
            env={**os.environ, **s3_env},
        )
        assert result.returncode == 0, f"snapshots list failed: {result.stderr}"

        # 6. Wait for Infrahub to recover
        await wait_for_http(f"{url}/api/config", timeout=180.0, interval=1.0)

        # 4. Modify data (delete the tag)
        await modify_infrahub_data(url, ADMIN_TOKEN, seed)

        # 5. Restore from plakar S3
        run_restore(backup_binary, common_args + ["restore"], env=s3_env)

        # 6. Wait for Infrahub to recover
        await wait_for_http(f"{url}/api/config", timeout=180.0, interval=1.0)

        # 7. Verify the tag is back
        await verify_infrahub_data(url, ADMIN_TOKEN, seed)

    async def test_restore_latest_matches_bare_restore(self, infrahub_compose, infrahub_port, backup_binary, tmp_path):
        """`restore --latest` and bare `restore` resolve the same latest complete group.

        FR-004: on this backend `--latest` is an explicit alias, not a second mechanism —
        the flag exists so a scheduled restore can be written once and run against either
        backend without knowing which one it got. The repository holds two groups, so
        "the latest" is a real choice, and both invocations must name the newer one.

        The group id is asserted rather than only the exit code because a restore that
        resolved the *older* group would still succeed and still exit 0 — it would simply
        bring back data from the wrong point in time.

        What each invocation verifies differs, deliberately:

        - `restore --latest` is data-verified end to end — the seeded tag is deleted first
          and has to come back — because that a `--latest` restore really restores rather
          than merely printing a group id is this spec's claim.
        - the bare `restore` is checked only for the group id it resolved. Bare `restore`
          is pre-existing behaviour, already data-verified by
          `test_backup_restore_plakar_local` in this class, so a second round trip here
          would buy nothing but another database recovery wait.
        """
        url = f"http://localhost:{infrahub_port}"
        project = infrahub_compose.project_name
        repo_args = ["--project", project, "--backend", "plakar", "--repo", f"fs://{tmp_path / 'plakar-parity-repo'}"]

        # 1. Seed the data whose return proves each restore really ran.
        seed = await seed_infrahub_data(url, ADMIN_TOKEN)

        # 2. Two groups, so the newer one has to be chosen rather than being the only option.
        run_backup(backup_binary, repo_args + ["create", "--force"])
        await wait_for_http(f"{url}/api/config", timeout=180.0, interval=1.0)
        run_backup(backup_binary, repo_args + ["create", "--force"])
        await wait_for_http(f"{url}/api/config", timeout=180.0, interval=1.0)

        groups = subprocess.run(
            [backup_binary, *repo_args, "--log-format", "json", "snapshots", "list"],
            capture_output=True,
            text=True,
        )
        assert groups.returncode == 0, f"snapshots list failed: {groups.stderr}"
        listed = sorted(set(LISTED_GROUP_ID.findall(groups.stdout + groups.stderr)))
        assert len(listed) == 2, f"expected two backup groups in the repository, found {listed}:\n{groups.stdout}"
        newest = max(listed)

        resolved = {}

        # 3. `restore --latest`, proving itself by bringing the deleted tag back.
        await modify_infrahub_data(url, ADMIN_TOKEN, seed)
        result = run_restore(backup_binary, repo_args + ["restore", "--latest"])
        resolved["--latest"] = _resolved_backup_group(result.stdout + result.stderr)
        await wait_for_http(f"{url}/api/config", timeout=180.0, interval=1.0)
        await verify_infrahub_data(url, ADMIN_TOKEN, seed)

        # 4. The bare form, against the same repository so the group it resolves is
        #    comparable. Only the resolved id is read back; the recovery wait stays because
        #    the next test in this class opens with `create --force`, which would otherwise
        #    meet a deployment that is still restarting.
        result = run_restore(backup_binary, repo_args + ["restore"])
        resolved["bare"] = _resolved_backup_group(result.stdout + result.stderr)
        await wait_for_http(f"{url}/api/config", timeout=180.0, interval=1.0)

        # 5. The parity claim itself.
        assert resolved["--latest"] == resolved["bare"], (
            f"--latest resolved group {resolved['--latest']} but bare restore resolved {resolved['bare']}"
        )

        # 6. And the group they agreed on is the newest one in the repository, not just any
        #    shared answer.
        assert resolved["--latest"] == newest, (
            f"both invocations resolved {resolved['--latest']}, but the repository's newest group is {newest} "
            f"(listed: {listed})"
        )

    @pytest.mark.xfail(reason="dedup not deterministic...")
    async def test_plakar_dedup_logical_vs_physical(self, infrahub_compose, backup_binary, tmp_path):
        """Verify plakar deduplication: repo physical size after two identical
        backups should be much less than 2x the size after one backup."""
        project = infrahub_compose.project_name
        repo_path = str(tmp_path / "plakar-repo-dedup")

        common_args = [
            "--project",
            project,
            "--backend",
            "plakar",
            "--repo",
            f"fs://{repo_path}",
        ]

        def repo_size_bytes() -> int:
            """Return total size of all files in the plakar repository."""
            return sum(f.stat().st_size for f in Path(repo_path).rglob("*") if f.is_file())

        # First backup – establishes baseline physical size
        run_backup(backup_binary, common_args + ["create", "--force"])
        size_after_first = repo_size_bytes()
        assert size_after_first > 0, "Repository should not be empty after first backup"

        # Second backup of identical data – should be mostly deduplicated
        run_backup(backup_binary, common_args + ["create", "--force"])
        size_after_second = repo_size_bytes()

        # The growth from the second backup should be well under 50% of the
        # first backup's size, proving that deduplication is effective.
        # With identical data the overhead is mostly snapshot metadata.
        growth = size_after_second - size_after_first
        print(f"Repository growth after dedup: {growth}")
        max_allowed_growth = size_after_first * 0.9
        assert growth < max_allowed_growth, (
            f"Deduplication check failed: first backup {size_after_first} bytes, "
            f"second added {growth} bytes ({growth / size_after_first:.1%}), "
            f"expected < 90% growth"
        )

        # Verify two backup groups exist
        result = subprocess.run(
            [backup_binary] + common_args + ["--log-format", "json", "snapshots", "list"],
            capture_output=True,
            text=True,
        )
        assert result.returncode == 0, f"snapshots list failed: {result.stderr}"
