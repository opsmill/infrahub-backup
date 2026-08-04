"""E2E tests: Docker Compose + local tarball backup/restore, plaintext and encrypted.

The encrypted cases cover the encrypted half of `restore --latest` (spec
005-restore-latest-backup, quickstart Scenario 5, FR-007): when the newest archive in the
pool is encrypted, the command either refuses before it has done anything at all or,
given the key, restores that same archive. Refusing is the whole point — quietly
restoring the newest archive that happens to be readable would leave a staging
deployment holding stale data, which is worse than a job that visibly failed.
"""

import time
from datetime import datetime, timedelta
from pathlib import Path

import pytest
from infrahub_sdk.testing.docker import TestInfrahubDockerClient

from tests.helpers.utils import (
    compose_container_runtimes,
    find_latest_backup,
    modify_infrahub_data,
    run_backup,
    run_cli,
    run_restore,
    seed_infrahub_data,
    verify_infrahub_data,
    wait_for_http,
)

ADMIN_TOKEN = "06438eb2-8019-4776-878c-0941b1f1d1ec"

# The wait the refused run must be observed *not* to have taken. `--sleep 5m` is passed to
# the refusal below precisely so the fail-fast ordering is measurable: the gate runs before
# the wait, so the invocation returns in well under a second in practice. The budget is
# generous enough that a loaded CI host cannot fail it, and still two orders of magnitude
# below the sleep it must not have entered.
FAIL_FAST_SLEEP = "5m"
FAIL_FAST_BUDGET_SECONDS = 30.0


def _archives(backup_dir: Path) -> list[str]:
    """Every backup archive in the directory, oldest name first.

    The timestamp format sorts lexicographically in chronological order, so a plain sort
    is the same ranking `--latest` applies.
    """
    return sorted(path.name for path in backup_dir.iterdir() if path.name.startswith("infrahub_backup_"))


def _older_archive_name(name: str, behind: timedelta = timedelta(hours=1)) -> str:
    """The unencrypted name an archive created `behind` before the given one would carry.

    Derived from the name rather than the clock so the decoy is unambiguously the older
    entry in the pool whatever the run's timing. `.enc` is stripped as well, so an
    encrypted archive's name yields a plaintext decoy of the right age.
    """
    stamp = name.removeprefix("infrahub_backup_").removesuffix(".enc").removesuffix(".tar.gz")
    older = datetime.strptime(stamp, "%Y%m%d_%H%M%S") - behind
    return f"infrahub_backup_{older.strftime('%Y%m%d_%H%M%S')}.tar.gz"


@pytest.mark.e2e
@pytest.mark.docker
class TestDockerTarball(TestInfrahubDockerClient):
    """Local tarball backup and restore, plaintext and encrypted, against one stack.

    Every case here needs the same default class-scoped compose stack, so they share one
    rather than paying its setup and teardown each. The restoring tests stop and start
    application containers, so the empty-pool case — which must leave the deployment
    alone — runs before them.
    """

    async def test_backup_restore_local_tarball(self, infrahub_compose, infrahub_port, backup_binary, tmp_path):
        """Create a local tarball backup, modify data, restore, and verify."""
        url = f"http://localhost:{infrahub_port}"
        project = infrahub_compose.project_name
        backup_dir = str(tmp_path / "backups")

        # 1. Seed test data
        seed = await seed_infrahub_data(url, ADMIN_TOKEN)

        # 2. Create backup
        run_backup(
            backup_binary,
            [
                "--project",
                project,
                "--backup-dir",
                backup_dir,
                "create",
                "--force",
            ],
        )
        backup_file = find_latest_backup(backup_dir)

        # 6. Wait for Infrahub to recover
        await wait_for_http(f"{url}/api/config", timeout=180.0, interval=5.0)

        # 3. Modify data (delete the tag)
        await modify_infrahub_data(url, ADMIN_TOKEN, seed)

        # 4. Restore from backup
        run_restore(
            backup_binary,
            [
                "--project",
                project,
                "restore",
                str(backup_file),
            ],
        )

        # 5. Wait for Infrahub to recover after restore
        await wait_for_http(f"{url}/api/config", timeout=180.0, interval=5.0)

        # 6. Verify the tag is back
        await verify_infrahub_data(url, ADMIN_TOKEN, seed)

    async def test_restore_latest_empty_pool_is_an_error(
        self, infrahub_compose, infrahub_port, backup_binary, tmp_path
    ):
        """`restore --latest` over a backup directory holding no archives fails cleanly.

        Quickstart Scenario 4 / FR-008: an empty pool is the expected state before the
        first backup ever runs, and the one state a scheduled restore must never read as a
        successful no-op. The error names the directory that was looked in, and the
        deployment is left exactly as it was found.
        """
        url = f"http://localhost:{infrahub_port}"
        project = infrahub_compose.project_name
        empty_pool = tmp_path / "empty-pool"
        empty_pool.mkdir()

        before = compose_container_runtimes(project)

        # BACKUP_DIR rather than --backup-dir: the environment-variable form a scheduled
        # job configures, which is how quickstart Scenario 4 invokes it.
        result = run_cli(
            backup_binary,
            ["--project", project, "restore", "--latest"],
            env={"BACKUP_DIR": str(empty_pool)},
        )

        # 1. Non-zero exit with the pool named (FR-008).
        assert result.returncode != 0, f"expected a non-zero exit:\n{result.stdout}\n{result.stderr}"
        output = result.stdout + result.stderr
        assert f"no backups found at local:{empty_pool}" in output, (
            f"the empty pool was not reported clearly:\n{output}"
        )

        # 2. Nothing was restored and nothing was touched.
        assert "Starting backup restore" not in output, f"a restore was attempted over an empty pool:\n{output}"
        assert compose_container_runtimes(project) == before, "the deployment was touched despite the empty pool"
        assert not list(empty_pool.iterdir()), f"the rejected pool was written into: {list(empty_pool.iterdir())}"

        await wait_for_http(f"{url}/api/config", timeout=180.0, interval=5.0)

    async def test_restore_latest_from_local_pool(self, infrahub_compose, infrahub_port, backup_binary, tmp_path):
        """`restore --latest` restores the newest archive in the local backup directory.

        Quickstart Scenario 1: no filename is passed, the pool holds more than one entry, and
        the newest is the archive that gets restored — named, together with the pool it came
        out of, before the restore starts (FR-001, FR-005, FR-009).

        The older entry is fabricated rather than backed up. Selection ranks names and never
        contents, so a name-only decoy is a full member of the pool, and one holding no
        archive at all is the stronger control: had it been ranked first, the restore would
        have failed outright instead of restoring the archive the audit line names. A second
        real `create` is also not available here — it leaves its dump in the same container
        directory a later restore copies into, which breaks the shared Neo4j restore path
        however the archive was chosen.
        """
        url = f"http://localhost:{infrahub_port}"
        project = infrahub_compose.project_name
        backup_dir = tmp_path / "latest-pool"

        # 1. Seed the data whose return proves the restore completed.
        seed = await seed_infrahub_data(url, ADMIN_TOKEN)

        # 2. The archive that must win.
        run_backup(backup_binary, ["--project", project, "--backup-dir", str(backup_dir), "create", "--force"])
        created = _archives(backup_dir)
        assert len(created) == 1, f"expected exactly one archive after the backup, found {created}"
        newest = created[0]

        # 3. An older entry, so "the newest" is a real choice rather than the only option.
        older = _older_archive_name(newest)
        (backup_dir / older).write_text("an older archive --latest must never rank first")
        assert older < newest, f"{older} must be older than {newest}"

        await wait_for_http(f"{url}/api/config", timeout=180.0, interval=5.0)

        # 4. Delete the tag whose return proves the restore happened.
        await modify_infrahub_data(url, ADMIN_TOKEN, seed)

        # 5. Restore without naming an archive. A non-zero exit raises (FR-001).
        result = run_restore(
            backup_binary,
            ["--project", project, "--backup-dir", str(backup_dir), "restore", "--latest"],
        )

        # 6. The run says which archive it chose and out of which pool (FR-009).
        output = result.stdout + result.stderr
        expected_line = f"Restoring latest backup {newest} from local:{backup_dir}"
        assert expected_line in output, f"selection was not reported as {expected_line!r}:\n{output}"

        # 7. The newest archive is the one the restore path was actually given (FR-005).
        assert str(backup_dir / newest) in output, f"{newest} was not the archive restored:\n{output}"
        assert older not in output, f"the older archive was ranked first:\n{output}"

        # 8. The restore ran to completion and the data is back.
        await wait_for_http(f"{url}/api/config", timeout=180.0, interval=5.0)
        await verify_infrahub_data(url, ADMIN_TOKEN, seed)

    async def test_restore_latest_encrypted_newest(self, infrahub_compose, infrahub_port, backup_binary, tmp_path):
        """An encrypted newest archive is refused without a key and restored with one.

        Quickstart Scenario 5 in one test, because both halves have to be asserted against
        the *same* archive: that the run which lacked the key stopped before doing anything,
        and that the archive it refused is nonetheless perfectly restorable once the key is
        supplied. Splitting them would leave the refusal indistinguishable from a broken
        archive.
        """
        url = f"http://localhost:{infrahub_port}"
        project = infrahub_compose.project_name
        backup_dir = tmp_path / "backups"
        private_key = tmp_path / "backup.key"
        public_key = Path(f"{private_key}.pub")

        # 1. A keypair of this test's own, so the archive cannot be read by the built-in key.
        run_backup(backup_binary, ["keygen", "-o", str(private_key)])
        assert private_key.exists(), f"keygen wrote no private key at {private_key}"
        assert public_key.exists(), f"keygen wrote no public key at {public_key}"

        # 2. Seed the data whose return proves the keyed restore really happened.
        seed = await seed_infrahub_data(url, ADMIN_TOKEN)

        # 3. The newest archive in the pool is encrypted: --encrypt-key implies --encrypt.
        run_backup(
            backup_binary,
            [
                "--project",
                project,
                "--backup-dir",
                str(backup_dir),
                "create",
                "--force",
                "--encrypt-key",
                str(public_key),
            ],
        )
        created = _archives(backup_dir)
        assert len(created) == 1, f"expected exactly one archive after the backup, found {created}"
        selected = created[0]
        assert selected.endswith(".tar.gz.enc"), f"the archive was not encrypted: {selected}"

        # 4. An older, unencrypted archive that a fallback would find readable. Nothing may
        #    ever choose it: FR-007 forbids falling back, and the refusal below has to be
        #    seen naming the .enc archive with this one sitting right there.
        older = _older_archive_name(selected)
        (backup_dir / older).write_text("an older archive --latest must never fall back to")
        assert older < selected, f"{older} must be older than the encrypted {selected}"

        await wait_for_http(f"{url}/api/config", timeout=180.0, interval=5.0)

        # 5. Delete the tag whose return proves the keyed restore happened.
        await modify_infrahub_data(url, ADMIN_TOKEN, seed)

        # 6. Refuse without a key — and be seen refusing before the sleep, before any
        #    container action, and without falling back (FR-007).
        before = compose_container_runtimes(project)
        started = time.monotonic()
        refused = run_cli(
            backup_binary,
            [
                "--project",
                project,
                "--backup-dir",
                str(backup_dir),
                "restore",
                "--latest",
                "--sleep",
                FAIL_FAST_SLEEP,
            ],
        )
        elapsed = time.monotonic() - started

        assert refused.returncode != 0, f"expected a non-zero exit:\n{refused.stdout}\n{refused.stderr}"

        # The duration *is* the ordering assertion: the gate cannot have run after a wait it
        # never entered. Reported as well as asserted, so a captured run shows the margin
        # rather than only that some threshold held.
        print(f"refused `restore --latest --sleep {FAIL_FAST_SLEEP}` in {elapsed:.2f}s")
        assert elapsed < FAIL_FAST_BUDGET_SECONDS, (
            f"the run took {elapsed:.1f}s, so the --sleep {FAIL_FAST_SLEEP} wait was entered before the gate"
        )

        output = refused.stdout + refused.stderr
        assert f"latest backup {selected} at local:{backup_dir} is encrypted" in output, (
            f"the refusal did not name the encrypted archive and its pool:\n{output}"
        )
        assert "--decrypt-key" in output, f"the refusal did not point at --decrypt-key:\n{output}"
        assert "Sleeping for" not in output, f"the sleep was entered before the gate:\n{output}"
        assert "Starting backup restore" not in output, f"a restore was attempted without a key:\n{output}"
        assert older not in output, f"--latest fell back to the older unencrypted archive:\n{output}"
        assert compose_container_runtimes(project) == before, "the deployment was touched despite the refusal"

        # 7. The same archive, with the key. A non-zero exit raises (FR-007).
        result = run_restore(
            backup_binary,
            [
                "--project",
                project,
                "--backup-dir",
                str(backup_dir),
                "restore",
                "--latest",
                "--decrypt-key",
                str(private_key),
            ],
        )
        output = result.stdout + result.stderr
        expected_line = f"Restoring latest backup {selected} from local:{backup_dir}"
        assert expected_line in output, f"selection was not reported as {expected_line!r}:\n{output}"

        # 8. The pool is as it was: the plaintext copy the decryption made is not left behind
        #    for the next --latest to rank.
        assert _archives(backup_dir) == sorted([selected, older]), (
            f"unexpected backup directory contents: {sorted(path.name for path in backup_dir.iterdir())}"
        )

        # 9. The encrypted archive's data is back: it really was restored.
        await wait_for_http(f"{url}/api/config", timeout=180.0, interval=5.0)
        await verify_infrahub_data(url, ADMIN_TOKEN, seed)
