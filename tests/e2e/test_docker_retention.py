"""E2E tests: retention policy applied by `create` and by the standalone `prune`."""

import os
import re
import subprocess
import uuid
from collections.abc import Iterator
from datetime import datetime, timedelta
from pathlib import Path

import boto3
import pytest
from infrahub_sdk.testing.docker import TestInfrahubDockerClient

from tests.helpers.utils import run_backup, wait_for_http

# Files that share the backup directory but are not backups: retention must never
# touch them, including the name that looks like a backup but carries no timestamp.
DECOYS = ["notes.txt", "infrahub_backup_garbage.tar.gz", "somebackup.tar.gz"]

# Ages in days of the fabricated archives, relative to the moment the test starts.
OUT_OF_POLICY_AGES = [20, 25, 30]
IN_POLICY_AGES = [1, 2]
IN_POLICY_ENCRYPTED_AGE = 3


def _archive_name(age_days: int, encrypted: bool = False) -> str:
    """Render the archive name a backup created age_days ago would carry."""
    timestamp = (datetime.now() - timedelta(days=age_days)).strftime("%Y%m%d_%H%M%S")
    return f"infrahub_backup_{timestamp}.tar.gz" + (".enc" if encrypted else "")


def _seed_fixtures(backup_dir: Path) -> tuple[list[str], list[str]]:
    """Fill backup_dir with archives spanning the policy boundary, plus decoys.

    Returns the (out_of_policy, survivors) name lists, where survivors covers both
    the in-policy archives and the decoys.
    """
    backup_dir.mkdir(parents=True, exist_ok=True)

    out_of_policy = [_archive_name(age) for age in OUT_OF_POLICY_AGES]
    in_policy = [_archive_name(age) for age in IN_POLICY_AGES]
    in_policy.append(_archive_name(IN_POLICY_ENCRYPTED_AGE, encrypted=True))

    for name in out_of_policy + in_policy + DECOYS:
        (backup_dir / name).write_text("retention fixture")

    return out_of_policy, in_policy + DECOYS


def _names(backup_dir: Path) -> set[str]:
    return {path.name for path in backup_dir.iterdir()}


def _real_backups(backup_dir: Path, fixtures: set[str]) -> set[str]:
    """Return the archives `create` actually produced, excluding the fixtures."""
    return {name for name in _names(backup_dir) if name.startswith("infrahub_backup_")} - fixtures


ALL_EXPIRED_AGES = [40, 50, 60]


def _run_prune(
    binary: str,
    args: list[str],
    stdin=subprocess.DEVNULL,
    env: dict[str, str] | None = None,
) -> subprocess.CompletedProcess:
    """Run `infrahub-backup prune` and return the result without raising on failure."""
    run_env = {**os.environ, **(env or {})}
    return subprocess.run([binary, *args], capture_output=True, text=True, stdin=stdin, env=run_env)


def _candidates(output: str) -> set[str]:
    """Return the archive names a dry run listed as candidates."""
    return set(re.findall(r"Would prune backup (\S+) from", output))


def _pruned(output: str) -> set[str]:
    """Return the archive names a real run reported as deleted."""
    return set(re.findall(r"Pruned backup (\S+) from", output))


# The location label is the last field of a logrus message, so the capture has to stop
# at the closing quote as well as at whitespace.
_LOCATION = r'([^"\s]+)'


def _candidates_by_location(output: str) -> set[tuple[str, str]]:
    """Return the (archive name, location label) pairs a dry run listed."""
    return set(re.findall(rf"Would prune backup (\S+) from {_LOCATION}", output))


def _pruned_by_location(output: str) -> set[tuple[str, str]]:
    """Return the (archive name, location label) pairs a real run reported as deleted."""
    return set(re.findall(rf"Pruned backup (\S+) from {_LOCATION}", output))


@pytest.mark.e2e
@pytest.mark.docker
class TestPruneRetention:
    """`prune` against a seeded backup directory.

    Retention on the local leg never talks to a deployment, so these cases need no
    Infrahub stack. They carry the `docker` marker because that is one of the two
    marks CI collects.
    """

    def test_dry_run_lists_candidates_and_deletes_nothing(self, backup_binary, tmp_path):
        """US2 scenario 1 / SC-004: the preview is exact and removes nothing."""
        backup_dir = tmp_path / "backups"
        out_of_policy, survivors = _seed_fixtures(backup_dir)
        before = _names(backup_dir)

        result = _run_prune(
            backup_binary,
            ["--backup-dir", str(backup_dir), "prune", "--retention-days", "7", "--dry-run"],
        )

        assert result.returncode == 0, f"dry run failed:\n{result.stdout}\n{result.stderr}"
        output = result.stdout + result.stderr
        assert _candidates(output) == set(out_of_policy), f"unexpected candidate set in:\n{output}"
        assert _pruned(output) == set(), f"a dry run reported deletions:\n{output}"
        assert _names(backup_dir) == before, f"a dry run changed the directory: {sorted(_names(backup_dir))}"
        assert set(survivors) <= _names(backup_dir)

    def test_force_deletes_exactly_the_previewed_set(self, backup_binary, tmp_path):
        """US2 scenario 3 / SC-004: `--force` deletes the previewed set and nothing else."""
        backup_dir = tmp_path / "backups"
        out_of_policy, survivors = _seed_fixtures(backup_dir)

        preview = _run_prune(
            backup_binary,
            ["--backup-dir", str(backup_dir), "prune", "--retention-days", "7", "--dry-run"],
        )
        assert preview.returncode == 0, f"dry run failed:\n{preview.stdout}\n{preview.stderr}"
        previewed = _candidates(preview.stdout + preview.stderr)

        result = _run_prune(
            backup_binary,
            ["--backup-dir", str(backup_dir), "prune", "--retention-days", "7", "--force"],
        )

        assert result.returncode == 0, f"forced prune failed:\n{result.stdout}\n{result.stderr}"
        output = result.stdout + result.stderr
        assert _pruned(output) == previewed == set(out_of_policy), f"deleted set does not match the preview:\n{output}"
        assert _names(backup_dir) == set(survivors), f"unexpected survivors: {sorted(_names(backup_dir))}"

    def test_force_keeps_the_newest_when_everything_is_out_of_policy(self, backup_binary, tmp_path):
        """FR-003 / SC-003: the floor has no override, even when every rule prunes."""
        backup_dir = tmp_path / "backups"
        backup_dir.mkdir(parents=True)
        expired = [_archive_name(age) for age in ALL_EXPIRED_AGES]
        for name in expired + DECOYS:
            (backup_dir / name).write_text("retention fixture")

        newest = _archive_name(min(ALL_EXPIRED_AGES))
        result = _run_prune(
            backup_binary,
            [
                "--backup-dir",
                str(backup_dir),
                "prune",
                "--retention-days",
                "7",
                "--retention-count",
                "1",
                "--force",
            ],
        )

        assert result.returncode == 0, f"forced prune failed:\n{result.stdout}\n{result.stderr}"
        output = result.stdout + result.stderr
        assert _pruned(output) == set(expired) - {newest}, f"unexpected deletions:\n{output}"
        assert _names(backup_dir) == {newest} | set(DECOYS), (
            f"the newest archive or a decoy was removed: {sorted(_names(backup_dir))}"
        )

    @pytest.mark.parametrize(
        ("args", "expected"),
        [
            ([], "at least one retention rule is required"),
            (["--retention-days", "0"], "--retention-days must be at least 1 when set"),
            (["--retention-count", "0"], "--retention-count must be at least 1 when set"),
            (["--retention-days", "7", "--dry-run", "--force"], "contradictory"),
            (["--retention-days", "7", "--backend", "plakar", "--repo", "/tmp/nope"], "not yet supported"),
            (["--retention-days", "7"], "re-run with --force"),
        ],
        ids=["no-rule", "zero-days", "zero-count", "dry-run-with-force", "plakar-backend", "non-interactive-stdin"],
    )
    def test_validation_errors_exit_non_zero(self, backup_binary, tmp_path, args, expected):
        """US2 scenario 4: refusals exit non-zero, explain themselves, and delete nothing."""
        backup_dir = tmp_path / "backups"
        _seed_fixtures(backup_dir)
        before = _names(backup_dir)

        result = _run_prune(backup_binary, ["--backup-dir", str(backup_dir), "prune", *args])

        assert result.returncode != 0, f"expected a non-zero exit:\n{result.stdout}\n{result.stderr}"
        output = result.stdout + result.stderr
        assert expected in output, f"expected {expected!r} in:\n{output}"
        assert _names(backup_dir) == before, f"a refused prune changed the directory: {sorted(_names(backup_dir))}"


# ---------------------------------------------------------------------------
# `prune --s3` against a real S3 endpoint
# ---------------------------------------------------------------------------

# The configured prefix carries no trailing slash, which is how an operator is most
# likely to write it and which exercises the prefix normalisation on both the listing
# and the deletion side.
S3_PREFIX = "backups"

# An out-of-policy encrypted archive, so the `.enc` name variant is exercised on the
# deletion side and not only among the survivors.
OUT_OF_POLICY_ENCRYPTED_AGE = 35

# Ages for the S3 leg of the two-leg case. They differ from ALL_EXPIRED_AGES so that
# each location's floor keeps a demonstrably different archive.
S3_ALL_EXPIRED_AGES = [45, 55, 65]

# Objects that live inside the configured prefix but are not backups (FR-006).
S3_DECOYS = [
    "notes.txt",
    # Backup-shaped, but carries no timestamp at all.
    "infrahub_backup_garbage.tar.gz",
    "somebackup.tar.gz",
    # A checksum sidecar, not an archive.
    "infrahub_backup_20240101_010101.tar.gz.sha256",
    # Right shape, impossible instant: month 13 and hour 99 never parse.
    "infrahub_backup_20241345_996699.tar.gz",
]


def _s3_client(minio: dict):
    """Return a boto3 client bound to the test MinIO endpoint."""
    return boto3.client(
        "s3",
        endpoint_url=minio["endpoint"],
        aws_access_key_id=minio["access_key"],
        aws_secret_access_key=minio["secret_key"],
        region_name="us-east-1",
    )


def _s3_keys(client, bucket: str) -> set[str]:
    """Return every key in the bucket, following pagination."""
    keys: set[str] = set()
    for page in client.get_paginator("list_objects_v2").paginate(Bucket=bucket):
        keys.update(obj["Key"] for obj in page.get("Contents", []))
    return keys


def _put_s3(client, bucket: str, key: str):
    """Seed one object. Retention reads age from the key's name, never from metadata."""
    client.put_object(Bucket=bucket, Key=key, Body=b"retention fixture")


def _s3_flags(minio: dict, bucket: str, prefix: str = S3_PREFIX) -> list[str]:
    """The persistent S3 flags that point the binary at the test endpoint."""
    return [
        "--s3-bucket",
        bucket,
        "--s3-endpoint",
        minio["endpoint"],
        "--s3-region",
        "us-east-1",
        "--s3-prefix",
        prefix,
    ]


def _s3_env(minio: dict) -> dict[str, str]:
    """Credentials for the binary, which resolves them from the standard AWS variables."""
    return {
        "AWS_ACCESS_KEY_ID": minio["access_key"],
        "AWS_SECRET_ACCESS_KEY": minio["secret_key"],
    }


def _s3_location(bucket: str, prefix: str = S3_PREFIX) -> str:
    """The label the S3 location gives itself in logs, for a prefix as an operator wrote it."""
    normalised = prefix.rstrip("/")
    return f"s3://{bucket}/{normalised}" if normalised else f"s3://{bucket}"


def _key_prefix(prefix: str = S3_PREFIX) -> str:
    """The key prefix uploads write under, for a prefix as an operator wrote it."""
    normalised = prefix.rstrip("/")
    return f"{normalised}/" if normalised else ""


def _empty_local_dir(tmp_path: Path) -> Path:
    """An existing but empty backup directory, so the local leg finds nothing to prune."""
    local_dir = tmp_path / "backups"
    local_dir.mkdir(parents=True, exist_ok=True)
    return local_dir


@pytest.fixture
def s3_bucket(minio_docker: dict) -> Iterator[str]:
    """Create an empty bucket for one test and remove it, with its contents, afterwards.

    A bucket per test keeps the session-scoped MinIO container shared while letting each
    case own the whole key space — including the bucket root, which the prefix-scoping
    case has to seed.
    """
    client = _s3_client(minio_docker)
    bucket = f"retention-{uuid.uuid4().hex[:12]}"
    client.create_bucket(Bucket=bucket)
    try:
        yield bucket
    finally:
        for key in _s3_keys(client, bucket):
            client.delete_object(Bucket=bucket, Key=key)
        client.delete_bucket(Bucket=bucket)


@pytest.mark.e2e
@pytest.mark.docker
class TestS3Retention:
    """`prune --s3` against a real S3 endpoint (MinIO).

    These cases close the one retention path that no test could reach through a fake:
    S3 answers a delete for a key that does not exist with success, so a wrongly built
    deletion key would report every prune while removing nothing, indefinitely. Every
    assertion here is therefore made against the bucket's exact surviving key set rather
    than against counts or log lines alone.

    `prune` only reads the backup directory and the configured prefix, so no Infrahub
    deployment is involved. The `docker` marker is what CI collects, and MinIO itself
    runs as a container.
    """

    @pytest.mark.parametrize("prefix", [S3_PREFIX, f"{S3_PREFIX}/", ""], ids=["prefix", "trailing-slash", "no-prefix"])
    def test_force_deletes_exactly_the_out_of_policy_objects(
        self, backup_binary, minio_docker, s3_bucket, tmp_path, prefix
    ):
        """FR-002/FR-005/SC-001: the objects retention prunes are really gone from the bucket.

        This is the case that would catch a wrong deletion key: the exact surviving key
        set distinguishes "deleted the right object" from "reported a deletion that
        removed nothing", which a count would not. The `no-prefix` variant cannot make
        that distinction — at the bucket root a base name and its key are the same
        string — but it does cover a bucket configured without a prefix at all.

        The three variants are the three ways an operator writes the prefix; all but the
        last must resolve to the same keys and the same location label.
        """
        client = _s3_client(minio_docker)
        local_dir = _empty_local_dir(tmp_path)
        key_prefix = _key_prefix(prefix)

        out_of_policy = [_archive_name(age) for age in OUT_OF_POLICY_AGES]
        out_of_policy.append(_archive_name(OUT_OF_POLICY_ENCRYPTED_AGE, encrypted=True))
        in_policy = [_archive_name(age) for age in IN_POLICY_AGES]
        in_policy.append(_archive_name(IN_POLICY_ENCRYPTED_AGE, encrypted=True))
        for name in out_of_policy + in_policy:
            _put_s3(client, s3_bucket, f"{key_prefix}{name}")

        result = _run_prune(
            backup_binary,
            [
                "--backup-dir",
                str(local_dir),
                *_s3_flags(minio_docker, s3_bucket, prefix),
                "prune",
                "--retention-days",
                "7",
                "--force",
                "--s3",
            ],
            env=_s3_env(minio_docker),
        )

        assert result.returncode == 0, f"forced prune failed:\n{result.stdout}\n{result.stderr}"
        output = result.stdout + result.stderr
        location = _s3_location(s3_bucket, prefix)
        assert _pruned_by_location(output) == {(name, location) for name in out_of_policy}, (
            f"unexpected reported deletions:\n{output}"
        )
        assert _s3_keys(client, s3_bucket) == {f"{key_prefix}{name}" for name in in_policy}, (
            f"the bucket does not hold exactly the in-policy objects: {sorted(_s3_keys(client, s3_bucket))}"
        )
        assert (
            f"Retention at {location}: {len(in_policy)} backup(s) kept, {len(out_of_policy)} candidate(s) to prune"
            in output
        ), f"missing or wrong per-location summary:\n{output}"

    def test_prune_is_scoped_to_the_configured_prefix(self, backup_binary, minio_docker, s3_bucket, tmp_path):
        """FR-005: only objects where uploads write them are candidates.

        Every outsider below is old enough that retention would delete it if it were
        visible, so their survival is the assertion — sibling prefixes, the bucket root,
        and anything nested below the configured prefix are all out of reach.
        """
        client = _s3_client(minio_docker)
        local_dir = _empty_local_dir(tmp_path)

        out_of_policy = [_archive_name(age) for age in OUT_OF_POLICY_AGES]
        in_policy = [_archive_name(age) for age in IN_POLICY_AGES]
        for name in out_of_policy + in_policy:
            _put_s3(client, s3_bucket, f"{S3_PREFIX}/{name}")

        outsiders = {
            # A sibling prefix whose name starts with the configured one.
            f"{S3_PREFIX}-old/{_archive_name(21)}",
            # An unrelated sibling prefix.
            f"other/{_archive_name(22)}",
            # Two at the bucket root, which a listing that lost its prefix would reach.
            # Two rather than one so that the keep-newest floor cannot mask the mistake:
            # a single stray object would survive as its location's newest.
            _archive_name(23),
            _archive_name(26),
            # Nested below the configured prefix: not where uploads write.
            f"{S3_PREFIX}/nested/{_archive_name(24)}",
        }
        for key in outsiders:
            _put_s3(client, s3_bucket, key)

        result = _run_prune(
            backup_binary,
            [
                "--backup-dir",
                str(local_dir),
                *_s3_flags(minio_docker, s3_bucket),
                "prune",
                "--retention-days",
                "7",
                "--force",
                "--s3",
            ],
            env=_s3_env(minio_docker),
        )

        assert result.returncode == 0, f"forced prune failed:\n{result.stdout}\n{result.stderr}"
        output = result.stdout + result.stderr
        assert _pruned_by_location(output) == {(name, _s3_location(s3_bucket)) for name in out_of_policy}, (
            f"an object outside the configured prefix was treated as a candidate:\n{output}"
        )
        assert _s3_keys(client, s3_bucket) == outsiders | {f"{S3_PREFIX}/{name}" for name in in_policy}, (
            f"unexpected surviving keys: {sorted(_s3_keys(client, s3_bucket))}"
        )

    def test_non_backup_objects_in_the_prefix_survive(self, backup_binary, minio_docker, s3_bucket, tmp_path):
        """FR-006 at the S3 location: anything that is not a backup archive is invisible.

        Includes a name of exactly the right shape whose timestamp is not a real
        instant, which must be as invisible as a name of the wrong shape.
        """
        client = _s3_client(minio_docker)
        local_dir = _empty_local_dir(tmp_path)

        out_of_policy = [_archive_name(age) for age in OUT_OF_POLICY_AGES]
        in_policy = [_archive_name(age) for age in IN_POLICY_AGES]
        for name in out_of_policy + in_policy + S3_DECOYS:
            _put_s3(client, s3_bucket, f"{S3_PREFIX}/{name}")

        result = _run_prune(
            backup_binary,
            [
                "--backup-dir",
                str(local_dir),
                *_s3_flags(minio_docker, s3_bucket),
                "prune",
                "--retention-days",
                "7",
                "--force",
                "--s3",
            ],
            env=_s3_env(minio_docker),
        )

        assert result.returncode == 0, f"forced prune failed:\n{result.stdout}\n{result.stderr}"
        output = result.stdout + result.stderr
        assert _pruned(output) == set(out_of_policy), f"a non-backup object was pruned:\n{output}"
        assert _s3_keys(client, s3_bucket) == {f"{S3_PREFIX}/{name}" for name in in_policy + S3_DECOYS}, (
            f"unexpected surviving keys: {sorted(_s3_keys(client, s3_bucket))}"
        )

    def test_keeps_the_newest_object_when_everything_is_out_of_policy(
        self, backup_binary, minio_docker, s3_bucket, tmp_path
    ):
        """FR-003/SC-003: the keep-newest floor holds at the S3 location too, with no override."""
        client = _s3_client(minio_docker)
        local_dir = _empty_local_dir(tmp_path)

        # ALL_EXPIRED_AGES is ascending, so the first name is the newest. Deriving it by
        # index rather than by rendering the age again keeps the two from disagreeing if
        # the clock ticks between calls.
        expired = [_archive_name(age) for age in ALL_EXPIRED_AGES]
        newest = expired[0]
        for name in expired:
            _put_s3(client, s3_bucket, f"{S3_PREFIX}/{name}")

        result = _run_prune(
            backup_binary,
            [
                "--backup-dir",
                str(local_dir),
                *_s3_flags(minio_docker, s3_bucket),
                "prune",
                "--retention-days",
                "7",
                "--retention-count",
                "1",
                "--force",
                "--s3",
            ],
            env=_s3_env(minio_docker),
        )

        assert result.returncode == 0, f"forced prune failed:\n{result.stdout}\n{result.stderr}"
        output = result.stdout + result.stderr
        assert _pruned(output) == set(expired) - {newest}, f"unexpected deletions:\n{output}"
        assert _s3_keys(client, s3_bucket) == {f"{S3_PREFIX}/{newest}"}, (
            f"the newest object did not survive: {sorted(_s3_keys(client, s3_bucket))}"
        )

    def test_s3_is_untouched_without_the_s3_flag(self, backup_binary, minio_docker, s3_bucket, tmp_path):
        """FR-007: a fully configured bucket is still never touched without `--s3`."""
        client = _s3_client(minio_docker)
        backup_dir = tmp_path / "backups"
        local_out_of_policy, local_survivors = _seed_fixtures(backup_dir)

        s3_out_of_policy = [_archive_name(age) for age in OUT_OF_POLICY_AGES]
        seeded = {f"{S3_PREFIX}/{name}" for name in s3_out_of_policy}
        for key in seeded:
            _put_s3(client, s3_bucket, key)

        result = _run_prune(
            backup_binary,
            [
                "--backup-dir",
                str(backup_dir),
                *_s3_flags(minio_docker, s3_bucket),
                "prune",
                "--retention-days",
                "7",
                "--force",
            ],
            env=_s3_env(minio_docker),
        )

        assert result.returncode == 0, f"forced prune failed:\n{result.stdout}\n{result.stderr}"
        output = result.stdout + result.stderr
        # The local leg did real work, so "nothing happened" cannot explain the survival
        # of the objects.
        assert _pruned(output) == set(local_out_of_policy), f"unexpected deletions:\n{output}"
        assert _names(backup_dir) == set(local_survivors), f"unexpected local survivors: {sorted(_names(backup_dir))}"
        assert _s3_keys(client, s3_bucket) == seeded, (
            f"a run without --s3 changed the bucket: {sorted(_s3_keys(client, s3_bucket))}"
        )
        assert _s3_location(s3_bucket) not in output, f"the S3 location was evaluated without --s3:\n{output}"

    def test_both_legs_are_pruned_each_with_its_own_floor(self, backup_binary, minio_docker, s3_bucket, tmp_path):
        """FR-005/SC-003: the policy is evaluated per location, floor included.

        Every archive at both locations is out of policy, and the S3 objects are all
        older than every local archive. A floor applied across the run rather than per
        location would keep one survivor in total; the contract is one per location.
        """
        client = _s3_client(minio_docker)
        backup_dir = tmp_path / "backups"
        backup_dir.mkdir(parents=True)

        local_expired = [_archive_name(age) for age in ALL_EXPIRED_AGES]
        for name in local_expired:
            (backup_dir / name).write_text("retention fixture")

        s3_expired = [_archive_name(age) for age in S3_ALL_EXPIRED_AGES]
        for name in s3_expired:
            _put_s3(client, s3_bucket, f"{S3_PREFIX}/{name}")

        result = _run_prune(
            backup_binary,
            [
                "--backup-dir",
                str(backup_dir),
                *_s3_flags(minio_docker, s3_bucket),
                "prune",
                "--retention-days",
                "7",
                "--retention-count",
                "1",
                "--force",
                "--s3",
            ],
            env=_s3_env(minio_docker),
        )

        assert result.returncode == 0, f"forced prune failed:\n{result.stdout}\n{result.stderr}"
        output = result.stdout + result.stderr
        local_location = f"local:{backup_dir}"
        expected_pruned = {(name, local_location) for name in local_expired[1:]}
        expected_pruned |= {(name, _s3_location(s3_bucket)) for name in s3_expired[1:]}
        assert _pruned_by_location(output) == expected_pruned, f"unexpected deletions:\n{output}"
        assert _names(backup_dir) == {local_expired[0]}, f"unexpected local survivors: {sorted(_names(backup_dir))}"
        assert _s3_keys(client, s3_bucket) == {f"{S3_PREFIX}/{s3_expired[0]}"}, (
            f"unexpected S3 survivors: {sorted(_s3_keys(client, s3_bucket))}"
        )

    def test_dry_run_lists_s3_candidates_and_deletes_nothing(self, backup_binary, minio_docker, s3_bucket, tmp_path):
        """SC-004 at the S3 location: the preview names the objects and removes none."""
        client = _s3_client(minio_docker)
        backup_dir = tmp_path / "backups"
        local_out_of_policy, _ = _seed_fixtures(backup_dir)
        local_before = _names(backup_dir)

        s3_out_of_policy = [_archive_name(age) for age in OUT_OF_POLICY_AGES]
        s3_in_policy = [_archive_name(age) for age in IN_POLICY_AGES]
        seeded = {f"{S3_PREFIX}/{name}" for name in s3_out_of_policy + s3_in_policy}
        for key in seeded:
            _put_s3(client, s3_bucket, key)

        result = _run_prune(
            backup_binary,
            [
                "--backup-dir",
                str(backup_dir),
                *_s3_flags(minio_docker, s3_bucket),
                "prune",
                "--retention-days",
                "7",
                "--dry-run",
                "--s3",
            ],
            env=_s3_env(minio_docker),
        )

        assert result.returncode == 0, f"dry run failed:\n{result.stdout}\n{result.stderr}"
        output = result.stdout + result.stderr
        expected = {(name, f"local:{backup_dir}") for name in local_out_of_policy}
        expected |= {(name, _s3_location(s3_bucket)) for name in s3_out_of_policy}
        assert _candidates_by_location(output) == expected, f"unexpected candidate set:\n{output}"
        assert _pruned(output) == set(), f"a dry run reported deletions:\n{output}"
        assert _s3_keys(client, s3_bucket) == seeded, (
            f"a dry run changed the bucket: {sorted(_s3_keys(client, s3_bucket))}"
        )
        assert _names(backup_dir) == local_before, f"a dry run changed the directory: {sorted(_names(backup_dir))}"


@pytest.mark.e2e
@pytest.mark.docker
@pytest.mark.enterprise
class TestDockerRetention(TestInfrahubDockerClient):
    """Retention as `create` applies it, against one class-scoped compose stack.

    These are the retention cases that need a live deployment, and they share one stack
    rather than paying its setup and teardown each. The local leg is covered first; the S3
    leg follows, and needs a deployment for the same reason — `create` prunes the bucket
    because this run uploaded to it, not because the bucket was configured.

    The `enterprise` mark places these in the Enterprise CI leg only. Nothing here depends
    on the Neo4j edition — retention selects archives by name and never reads one, so these
    backups only have to succeed — while on Community every one of them takes the offline
    dump path, stopping and restarting the whole deployment. Enterprise backs up online, and
    the Community path stays covered by the tarball, S3, and plakar cases, which restore the
    archives they take and therefore have to exercise it.
    """

    async def test_create_applies_retention(self, infrahub_compose, infrahub_port, backup_binary, tmp_path):
        """`create` prunes out-of-policy archives only when retention is configured."""
        url = f"http://localhost:{infrahub_port}"
        project = infrahub_compose.project_name
        backup_dir = tmp_path / "backups"

        out_of_policy, survivors = _seed_fixtures(backup_dir)
        fixtures = set(out_of_policy) | set(survivors)

        # 1. Back up with a retention policy. The count rule is 3 rather than the
        #    14 of a realistic schedule so that this small fixture set actually
        #    crosses the boundary: union semantics mean a count larger than the
        #    number of archives present would claim every one of them.
        result = run_backup(
            backup_binary,
            [
                "--project",
                project,
                "--backup-dir",
                str(backup_dir),
                "create",
                "--force",
                "--retention-days",
                "7",
                "--retention-count",
                "3",
            ],
        )

        # 2. The new archive plus every in-policy archive and every decoy remain,
        #    and exactly the out-of-policy archives are gone (US1 scenario 1).
        pruned_run_backups = _real_backups(backup_dir, fixtures)
        assert len(pruned_run_backups) == 1, f"expected exactly one new archive, found {pruned_run_backups}"
        assert _names(backup_dir) == set(survivors) | pruned_run_backups, (
            f"unexpected backup directory contents: {sorted(_names(backup_dir))}"
        )

        # 3. Every deletion is reported (FR-009).
        output = result.stdout + result.stderr
        for name in out_of_policy:
            assert f"Pruned backup {name}" in output, f"deletion of {name} was not reported:\n{output}"

        # 4. Wait for Infrahub to recover before the second backup.
        await wait_for_http(f"{url}/api/config", timeout=180.0, interval=1.0)

        # 5. Without retention options nothing is pruned (US1 scenario 3).
        for name in out_of_policy:
            (backup_dir / name).write_text("retention fixture")
        before = _names(backup_dir)

        result = run_backup(
            backup_binary,
            [
                "--project",
                project,
                "--backup-dir",
                str(backup_dir),
                "create",
                "--force",
            ],
        )

        plain_run_backups = _real_backups(backup_dir, fixtures) - pruned_run_backups
        assert len(plain_run_backups) == 1, f"expected exactly one new archive, found {plain_run_backups}"
        assert _names(backup_dir) == before | plain_run_backups, (
            f"a run without retention options changed the directory: {sorted(_names(backup_dir))}"
        )
        assert "Pruned backup" not in (result.stdout + result.stderr), "retention ran without being configured"

        await wait_for_http(f"{url}/api/config", timeout=180.0, interval=1.0)

    async def test_create_prunes_s3_only_when_the_run_uploaded(
        self, infrahub_compose, infrahub_port, backup_binary, minio_docker, s3_bucket, tmp_path
    ):
        """US1 scenario 2 / FR-007: the S3 leg is pruned exactly when the run uploaded there.

        Quickstart scenario 6: `create --s3-upload` prunes the bucket it just uploaded to.
        """
        url = f"http://localhost:{infrahub_port}"
        project = infrahub_compose.project_name
        client = _s3_client(minio_docker)

        # The local leg is deliberately quiet — a single in-policy archive, nothing to
        # prune — so every assertion below is about the S3 leg. `create` without
        # --s3-keep-local also removes the fresh archive locally after uploading it.
        backup_dir = tmp_path / "backups"
        backup_dir.mkdir(parents=True)
        (backup_dir / _archive_name(IN_POLICY_AGES[0])).write_text("retention fixture")

        out_of_policy = [_archive_name(age) for age in OUT_OF_POLICY_AGES]
        in_policy = [_archive_name(age) for age in IN_POLICY_AGES]
        in_policy.append(_archive_name(IN_POLICY_ENCRYPTED_AGE, encrypted=True))
        seeded = {f"{S3_PREFIX}/{name}" for name in out_of_policy + in_policy}
        for key in seeded:
            _put_s3(client, s3_bucket, key)

        common = [
            "--project",
            project,
            "--backup-dir",
            str(backup_dir),
            *_s3_flags(minio_docker, s3_bucket),
        ]

        # 1. A run that uploads prunes the bucket it uploaded to.
        result = run_backup(
            backup_binary,
            [*common, "create", "--force", "--s3-upload", "--retention-days", "7"],
            env=_s3_env(minio_docker),
        )

        surviving = _s3_keys(client, s3_bucket)
        uploaded = surviving - seeded
        assert len(uploaded) == 1, f"expected exactly one uploaded object, found {sorted(uploaded)}"
        assert surviving == uploaded | {f"{S3_PREFIX}/{name}" for name in in_policy}, (
            f"the bucket does not hold the fresh upload plus exactly the in-policy objects: {sorted(surviving)}"
        )
        output = result.stdout + result.stderr
        assert _pruned_by_location(output) == {(name, _s3_location(s3_bucket)) for name in out_of_policy}, (
            f"unexpected reported deletions:\n{output}"
        )

        await wait_for_http(f"{url}/api/config", timeout=180.0, interval=1.0)

        # 2. A run that does not upload leaves the bucket alone, however complete the S3
        #    configuration is: the leg follows the upload, not the configuration.
        for key in {f"{S3_PREFIX}/{name}" for name in out_of_policy}:
            _put_s3(client, s3_bucket, key)
        before = _s3_keys(client, s3_bucket)

        result = run_backup(
            backup_binary,
            [*common, "create", "--force", "--retention-days", "7"],
            env=_s3_env(minio_docker),
        )

        assert _s3_keys(client, s3_bucket) == before, (
            f"a run without --s3-upload changed the bucket: {sorted(_s3_keys(client, s3_bucket))}"
        )
        output = result.stdout + result.stderr
        assert _s3_location(s3_bucket) not in output, f"the S3 leg ran without an upload:\n{output}"

        await wait_for_http(f"{url}/api/config", timeout=180.0, interval=1.0)
