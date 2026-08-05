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
        await wait_for_http(f"{url}/api/config", timeout=180.0, interval=1.0)

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
        await wait_for_http(f"{url}/api/config", timeout=180.0, interval=1.0)

        # 6. Verify the tag is back
        await verify_infrahub_data(url, ADMIN_TOKEN, seed)

    async def test_restore_after_a_second_backup_loads_the_named_archive(
        self, infrahub_compose, infrahub_port, backup_binary, tmp_path
    ):
        """A restore that follows two real `create` runs loads the archive it was named.

        The regression test for the wrong-dump defect. `create` used to leave its Neo4j dump
        inside the container, in the very directory a restore copies an archive's dump into,
        and the loader read that leftover instead of the archive it was given — silently
        restoring the *other* backup's data, with exit 0 and "Neo4j dump restored
        successfully" in the log.

        Two real `create` runs are what makes the substitution observable: with one, the
        leftover dump *is* the archive being restored, so the wrong file and the right file
        are the same bytes. The archive is named explicitly, so nothing about `--latest` is
        involved, and the assertion is on the restored data rather than on the exit code,
        because the broken run exited 0.
        """
        url = f"http://localhost:{infrahub_port}"
        project = infrahub_compose.project_name
        backup_dir = tmp_path / "two-backups"

        # 1. Seed the tag that only the first archive holds.
        seed = await seed_infrahub_data(url, ADMIN_TOKEN)

        # 2. The archive to restore later, taken while the tag exists.
        run_backup(backup_binary, ["--project", project, "--backup-dir", str(backup_dir), "create", "--force"])
        first = _archives(backup_dir)
        assert len(first) == 1, f"expected exactly one archive after the first backup, found {first}"
        wanted = first[0]

        await wait_for_http(f"{url}/api/config", timeout=180.0, interval=1.0)

        # 3. Delete the tag, then take a second real backup without it. This is the run whose
        #    leftover dump used to be what a later restore loaded.
        await modify_infrahub_data(url, ADMIN_TOKEN, seed)
        run_backup(backup_binary, ["--project", project, "--backup-dir", str(backup_dir), "create", "--force"])
        both = _archives(backup_dir)
        assert len(both) == 2, f"expected two archives after the second backup, found {both}"
        assert both[0] == wanted, f"{wanted} is not the older of {both}"

        await wait_for_http(f"{url}/api/config", timeout=180.0, interval=1.0)

        # 4. Restore the first archive by name. A non-zero exit raises.
        run_restore(
            backup_binary,
            ["--project", project, "--backup-dir", str(backup_dir), "restore", str(backup_dir / wanted)],
        )

        # 5. The tag is back, so the data loaded came from the archive that was named and not
        #    from the second backup's leftovers.
        await wait_for_http(f"{url}/api/config", timeout=180.0, interval=1.0)
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

        await wait_for_http(f"{url}/api/config", timeout=180.0, interval=1.0)

    async def test_restore_latest_from_local_pool(self, infrahub_compose, infrahub_port, backup_binary, tmp_path):
        """`restore --latest` restores the newest archive in the local backup directory.

        Quickstart Scenario 1: no filename is passed, the pool holds more than one entry, and
        the newest is the archive that gets restored — named, together with the pool it came
        out of, before the restore starts (FR-001, FR-005, FR-009).

        The older entry is fabricated rather than backed up. Selection ranks names and never
        contents, so a name-only decoy is a full member of the pool, and one holding no
        archive at all is the stronger control: had it been ranked first, the restore would
        have failed outright instead of restoring the archive the audit line names. What a
        second real `create` adds — that the restore reads the selected archive rather than
        the previous backup's leftovers — is covered by
        test_restore_after_a_second_backup_loads_the_named_archive without paying for a
        second backup here.
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

        await wait_for_http(f"{url}/api/config", timeout=180.0, interval=1.0)

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
        await wait_for_http(f"{url}/api/config", timeout=180.0, interval=1.0)
        await verify_infrahub_data(url, ADMIN_TOKEN, seed)

    async def test_restore_latest_encrypted_newest(self, infrahub_compose, infrahub_port, backup_binary, tmp_path):
        """An encrypted newest archive is refused without a key and restored with one.

        Quickstart Scenario 5 in one test, because both halves have to be asserted against
        the *same* archive: that the run which lacked the key stopped before doing anything,
        and that the archive it refused is nonetheless perfectly restorable once the key is
        supplied. Splitting them would leave the refusal indistinguishable from a broken
        archive.

        Two regressions ride along on the same pool, because the state they need is the state
        this test already builds. A plain archive of the encrypted one's timestamp sits beside
        it: decryption used to write the plaintext to exactly that name and then delete it, so
        two archives went in and one came out. And the key is offered once through
        INFRAHUB_DECRYPT_KEY, which was bound to viper but never read, leaving a restore
        configured entirely through the environment unable to decrypt anything.
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

        # 4b. The plain member of a same-timestamp pair: the exact name the decryption used to
        #     write its plaintext to, and then delete. Its contents are not an archive, so any
        #     run that reads it fails loudly instead of quietly succeeding — but nothing may
        #     read it at all, because the tiebreak deliberately ranks the .enc member first.
        twin = backup_dir / selected.removesuffix(".enc")
        twin_body = "the plain archive that shares the encrypted one's timestamp"
        twin.write_text(twin_body)

        await wait_for_http(f"{url}/api/config", timeout=180.0, interval=1.0)

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

        # 6b. The key offered through INFRAHUB_DECRYPT_KEY reaches the restore. The variable
        #     points at a path that does not exist, so the run still fails — but on loading the
        #     key rather than on the "no key was passed" gate, which is only possible if the
        #     variable was read. A restore that got as far as a container would be a far more
        #     expensive way to assert the same thing.
        from_env = run_cli(
            backup_binary,
            ["--project", project, "--backup-dir", str(backup_dir), "restore", "--latest"],
            env={"INFRAHUB_DECRYPT_KEY": str(tmp_path / "absent.key")},
        )
        assert from_env.returncode != 0, f"expected a non-zero exit:\n{from_env.stdout}\n{from_env.stderr}"
        output = from_env.stdout + from_env.stderr
        assert "failed to load decryption key" in output, f"INFRAHUB_DECRYPT_KEY did not reach the restore:\n{output}"
        assert "is encrypted" not in output, f"the key from the environment was not seen at all:\n{output}"
        assert compose_container_runtimes(project) == before, (
            "the deployment was touched by a run whose key could not be loaded"
        )

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

        # 8. The pool is as it was: every archive still there, and no plaintext left behind
        #    for the next --latest to rank.
        assert _archives(backup_dir) == sorted([selected, older, twin.name]), (
            f"unexpected backup directory contents: {sorted(path.name for path in backup_dir.iterdir())}"
        )
        assert twin.read_text() == twin_body, (
            f"{twin.name} was overwritten by the decryption of {selected}, which shares its timestamp"
        )
        leftovers = [path.name for path in backup_dir.iterdir() if path.name.startswith("restore-")]
        assert not leftovers, f"temporary restore files were left behind: {leftovers}"

        # 9. The encrypted archive's data is back: it really was restored.
        await wait_for_http(f"{url}/api/config", timeout=180.0, interval=1.0)
        await verify_infrahub_data(url, ADMIN_TOKEN, seed)
