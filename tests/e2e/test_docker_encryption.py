"""E2E tests: Docker Compose + encrypted tarball backup/restore.

Covers the encrypted half of `restore --latest` (spec 005-restore-latest-backup,
quickstart Scenario 5, FR-007): the newest archive in the pool is encrypted, and the
command either refuses before it has done anything at all or, given the key, restores
that same archive. Refusing is the whole point — quietly restoring the newest archive
that happens to be readable would leave a staging deployment holding stale data, which
is worse than a job that visibly failed.
"""

import time
from datetime import datetime, timedelta
from pathlib import Path

import pytest
from infrahub_sdk.testing.docker import TestInfrahubDockerClient

from tests.helpers.utils import (
    compose_container_runtimes,
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
    """Every backup archive in the directory, oldest name first."""
    return sorted(path.name for path in backup_dir.iterdir() if path.name.startswith("infrahub_backup_"))


def _older_archive_name(name: str, behind: timedelta = timedelta(hours=1)) -> str:
    """The unencrypted name an archive created `behind` before the given one would carry.

    Derived from the name rather than the clock so the decoy is unambiguously the *older*
    entry in the pool whatever the run's timing.
    """
    stamp = name.removeprefix("infrahub_backup_").removesuffix(".enc").removesuffix(".tar.gz")
    older = datetime.strptime(stamp, "%Y%m%d_%H%M%S") - behind
    return f"infrahub_backup_{older.strftime('%Y%m%d_%H%M%S')}.tar.gz"


@pytest.mark.e2e
@pytest.mark.docker
class TestDockerEncryption(TestInfrahubDockerClient):
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
