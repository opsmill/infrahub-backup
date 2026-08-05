"""E2E tests: Docker Compose + S3 tarball backup/restore."""

import hashlib
import uuid
from datetime import datetime, timedelta
from pathlib import Path

import pytest
from infrahub_sdk.testing.docker import TestInfrahubDockerClient

from tests.helpers.utils import (
    get_s3_backup_key,
    modify_infrahub_data,
    run_backup,
    run_restore,
    seed_infrahub_data,
    verify_infrahub_data,
    wait_for_http,
)

ADMIN_TOKEN = "06438eb2-8019-4776-878c-0941b1f1d1ec"


def _archives(backup_dir: Path) -> list[str]:
    """Every backup archive in the directory, oldest name first."""
    return sorted(path.name for path in backup_dir.iterdir() if path.name.startswith("infrahub_backup_"))


def _digest(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def _newer_archive_name(name: str, ahead: timedelta = timedelta(hours=1)) -> str:
    """The name an archive created `ahead` after the given one would carry.

    Derived from the name rather than the clock so the decoy is unambiguously the newest
    entry in the local pool, whatever the run's timing.
    """
    stamp = name.removeprefix("infrahub_backup_").removesuffix(".tar.gz")
    newer = datetime.strptime(stamp, "%Y%m%d_%H%M%S") + ahead
    return f"infrahub_backup_{newer.strftime('%Y%m%d_%H%M%S')}.tar.gz"


@pytest.mark.e2e
@pytest.mark.docker
class TestDockerS3(TestInfrahubDockerClient):
    async def test_backup_restore_s3_tarball(
        self, infrahub_compose, infrahub_port, backup_binary, minio_docker, tmp_path
    ):
        """Create a tarball backup uploaded to S3, restore from S3, and verify.

        The upload keeps its local copy, so the restore below runs against the state
        `create --s3-upload --s3-keep-local` leaves on every host: the same archive name in
        the bucket and in the backup directory. A positional-URI restore used to download onto
        that name and then delete it, consuming the local copy an operator reaches for when
        the bucket is unreachable — which is why the local copy is checked afterwards.
        """
        url = f"http://localhost:{infrahub_port}"
        project = infrahub_compose.project_name
        minio = minio_docker

        s3_env = {
            "AWS_ACCESS_KEY_ID": minio["access_key"],
            "AWS_SECRET_ACCESS_KEY": minio["secret_key"],
        }

        # 1. Seed test data
        seed = await seed_infrahub_data(url, ADMIN_TOKEN)

        # 2. Create backup with S3 upload, keeping the local copy
        run_backup(
            backup_binary,
            [
                "--project",
                project,
                "--backup-dir",
                str(tmp_path),
                "--s3-bucket",
                minio["bucket"],
                "--s3-endpoint",
                minio["endpoint"],
                "--s3-region",
                "us-east-1",
                "create",
                "--force",
                "--s3-upload",
                "--s3-keep-local",
            ],
            env=s3_env,
        )
        uploaded = _archives(tmp_path)
        assert len(uploaded) == 1, f"expected exactly one local archive after the upload, found {uploaded}"
        local_copy = tmp_path / uploaded[0]
        local_digest = _digest(local_copy)

        # 3. Find the backup key in S3
        s3_key = get_s3_backup_key(
            bucket=minio["bucket"],
            prefix="",
            endpoint=minio["endpoint"],
            access_key=minio["access_key"],
            secret_key=minio["secret_key"],
        )
        s3_uri = f"s3://{minio['bucket']}/{s3_key}"

        # 6. Wait for Infrahub to recover
        await wait_for_http(f"{url}/api/config", timeout=180.0, interval=1.0)

        # 4. Modify data (delete the tag)
        await modify_infrahub_data(url, ADMIN_TOKEN, seed)

        # 5. Restore from S3
        run_restore(
            backup_binary,
            [
                "--project",
                project,
                "--backup-dir",
                str(tmp_path),
                "--s3-bucket",
                minio["bucket"],
                "--s3-endpoint",
                minio["endpoint"],
                "--s3-region",
                "us-east-1",
                "restore",
                s3_uri,
            ],
            env=s3_env,
        )

        # 6. The local archive of the same name is still there, byte for byte, and the
        #    download the restore made is not.
        assert local_copy.exists(), f"{local_copy.name} was deleted from the backup directory"
        assert _digest(local_copy) == local_digest, f"{local_copy.name} was overwritten by the download"
        leftovers = [path.name for path in tmp_path.iterdir() if path.name.startswith("restore-")]
        assert not leftovers, f"temporary downloads were left behind: {leftovers}"

        # 7. Wait for Infrahub to recover
        await wait_for_http(f"{url}/api/config", timeout=180.0, interval=1.0)

        # 8. Verify the tag is back
        await verify_infrahub_data(url, ADMIN_TOKEN, seed)

    async def test_restore_latest_from_s3(self, infrahub_compose, infrahub_port, backup_binary, minio_docker, tmp_path):
        """`restore --latest --s3` restores the newest object and spares the local copy.

        Quickstart Scenario 2: the S3 pool is the only pool consulted even when the local
        directory holds a newer archive (FR-006), and the local archive that shares the
        selected object's name — what `create --s3-upload --s3-keep-local` leaves behind —
        is still there, byte for byte, afterwards (critique E1/X1).

        The newer local-only archive is fabricated rather than backed up: selection reads
        names, not contents, and a decoy holding no real archive proves the local pool was
        never consulted twice over — had it been ranked, the restore would have failed
        outright instead of restoring the object named in the log.
        """
        url = f"http://localhost:{infrahub_port}"
        project = infrahub_compose.project_name
        minio = minio_docker
        backup_dir = tmp_path / "backups"

        # A prefix of this test's own, so the pool `--latest --s3` ranks holds nothing but
        # the object uploaded below.
        prefix = f"restore-latest-{uuid.uuid4().hex[:8]}"

        s3_env = {
            "AWS_ACCESS_KEY_ID": minio["access_key"],
            "AWS_SECRET_ACCESS_KEY": minio["secret_key"],
        }
        s3_flags = [
            "--s3-bucket",
            minio["bucket"],
            "--s3-prefix",
            prefix,
            "--s3-endpoint",
            minio["endpoint"],
            "--s3-region",
            "us-east-1",
        ]

        # 1. Seed the data whose survival proves which archive was restored.
        seed = await seed_infrahub_data(url, ADMIN_TOKEN)

        # 2. Upload an archive while keeping its local copy: the collision this flow makes
        #    the expected state.
        run_backup(
            backup_binary,
            [
                "--project",
                project,
                "--backup-dir",
                str(backup_dir),
                *s3_flags,
                "create",
                "--force",
                "--s3-upload",
                "--s3-keep-local",
            ],
            env=s3_env,
        )
        uploaded = _archives(backup_dir)
        assert len(uploaded) == 1, f"expected exactly one archive after the upload, found {uploaded}"
        selected = uploaded[0]
        local_copy = backup_dir / selected
        local_digest = _digest(local_copy)

        assert (
            get_s3_backup_key(
                bucket=minio["bucket"],
                prefix=prefix,
                endpoint=minio["endpoint"],
                access_key=minio["access_key"],
                secret_key=minio["secret_key"],
            )
            == f"{prefix}/{selected}"
        ), "the archive was not uploaded where the configured prefix says it should be"

        await wait_for_http(f"{url}/api/config", timeout=180.0, interval=1.0)

        # 3. Delete the tag whose return proves the restore happened.
        await modify_infrahub_data(url, ADMIN_TOKEN, seed)

        # 4. Put a newer archive in the local pool only. `--latest --s3` must not see it.
        newer_local = _newer_archive_name(selected)
        (backup_dir / newer_local).write_text("not a real archive: the local pool must never be ranked")
        assert newer_local > selected, f"{newer_local} must be newer than the uploaded {selected}"

        # 5. Restore the latest object in the bucket. A non-zero exit raises.
        result = run_restore(
            backup_binary,
            ["--project", project, "--backup-dir", str(backup_dir), *s3_flags, "restore", "--latest", "--s3"],
            env=s3_env,
        )

        # 6. The run says which archive it chose and out of which pool (FR-009).
        output = result.stdout + result.stderr
        expected_line = f"Restoring latest backup {selected} from s3://{minio['bucket']}/{prefix}"
        assert expected_line in output, f"selection was not reported as {expected_line!r}:\n{output}"
        assert newer_local not in output, f"the newer local archive was consulted:\n{output}"

        # 7. The local copy is untouched and no download was left behind.
        assert local_copy.exists(), f"{selected} was deleted from the backup directory"
        assert _digest(local_copy) == local_digest, f"{selected} was overwritten by the download"
        assert _archives(backup_dir) == sorted([selected, newer_local]), (
            f"unexpected backup directory contents: {sorted(path.name for path in backup_dir.iterdir())}"
        )
        leftovers = [path.name for path in backup_dir.iterdir() if path.name.startswith("restore-latest-")]
        assert not leftovers, f"temporary downloads were left behind: {leftovers}"

        # 8. The uploaded archive's data is back: the object was really restored.
        await wait_for_http(f"{url}/api/config", timeout=180.0, interval=1.0)
        await verify_infrahub_data(url, ADMIN_TOKEN, seed)
