"""E2E tests: retention policy applied by `create` and by the standalone `prune`."""

import re
import subprocess
from datetime import datetime, timedelta
from pathlib import Path

import pytest
from infrahub_sdk.testing.docker import TestInfrahubDockerClient

from tests.helpers.utils import run_backup, wait_for_http

ADMIN_TOKEN = "06438eb2-8019-4776-878c-0941b1f1d1ec"

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


@pytest.mark.e2e
@pytest.mark.docker
class TestDockerRetention(TestInfrahubDockerClient):
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
        await wait_for_http(f"{url}/api/config", timeout=180.0, interval=5.0)

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

        await wait_for_http(f"{url}/api/config", timeout=180.0, interval=5.0)


ALL_EXPIRED_AGES = [40, 50, 60]


def _run_prune(binary: str, args: list[str], stdin=subprocess.DEVNULL) -> subprocess.CompletedProcess:
    """Run `infrahub-backup prune` and return the result without raising on failure."""
    return subprocess.run([binary, *args], capture_output=True, text=True, stdin=stdin)


def _candidates(output: str) -> set[str]:
    """Return the archive names a dry run listed as candidates."""
    return set(re.findall(r"Would prune backup (\S+) from", output))


def _pruned(output: str) -> set[str]:
    """Return the archive names a real run reported as deleted."""
    return set(re.findall(r"Pruned backup (\S+) from", output))


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
