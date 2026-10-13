"""E2E tests: Docker Compose + the artifact compatibility FR-015 and FR-016 require.

T058 in `specs/archive/007-external-database-backup/tasks.md` asks for two checks the ordinary
suites do not make: that an artifact written by the **previous released version** still
restores, and that an artifact whose metadata says its database was external restores into
an ordinary **internal** deployment.

Both are about the artifact contract rather than about the external path. FR-015 requires
deployments whose databases are all internal to be unaffected by this feature, and FR-016
requires the metadata additions to be additive, so that a reader which has never heard of
them ignores them rather than deciding incorrectly on the fields it cannot see.

The external-origin case synthesises its artifact by adding the external fields to a real
one, rather than capturing from a real external database — which needs a topology that does
not exist yet (T072). That is deliberate, and it is the stronger test of FR-016: what has to
be proved is that an internal restore dispatches on where the database *is*, not on what the
artifact says about where it came from.
"""

import json
import os
import platform
import shutil
import stat
import subprocess
import tarfile
from pathlib import Path

import pytest
from infrahub_sdk.testing.docker import TestInfrahubDockerClient

from tests.helpers.utils import (
    find_latest_backup,
    modify_infrahub_data,
    read_backup_metadata,
    run_backup,
    run_restore,
    seed_infrahub_data,
    verify_infrahub_data,
    wait_for_http,
)

ADMIN_TOKEN = "06438eb2-8019-4776-878c-0941b1f1d1ec"

# The release whose artifact the current binary must still read. Pinning it by name keeps
# the case reproducible; leaving it unset resolves the newest published release, which is
# what "the previous released version" means on a branch that has not been released yet.
PREVIOUS_RELEASE_ENV_VAR = "INFRAHUB_TESTING_PREVIOUS_RELEASE"


def _asset_name() -> str:
    """The release asset for this host, named the way the release workflow names them."""
    goos = {"Darwin": "darwin", "Linux": "linux"}.get(platform.system(), platform.system().lower())
    goarch = {"x86_64": "amd64", "AMD64": "amd64", "arm64": "arm64", "aarch64": "arm64"}.get(
        platform.machine(), platform.machine()
    )
    return f"infrahub-backup-{goos}-{goarch}"


@pytest.fixture(scope="session")
def previous_release_binary(tmp_path_factory) -> str:
    """The previous released `infrahub-backup`, downloaded from its GitHub release.

    Skips rather than fails when the release cannot be fetched: the check is worth having
    wherever the network and `gh` allow it, and a host without them must not turn a
    compatibility question into a red build about credentials.
    """
    if shutil.which("gh") is None:
        pytest.skip("needs the `gh` CLI to fetch the previous release's binary")

    tag = os.environ.get(PREVIOUS_RELEASE_ENV_VAR, "")
    if not tag:
        listed = subprocess.run(
            ["gh", "release", "list", "--limit", "1", "--json", "tagName", "--jq", ".[0].tagName"],
            capture_output=True,
            text=True,
        )
        if listed.returncode != 0 or not listed.stdout.strip():
            pytest.skip(
                "could not resolve the previous release; set "
                f"{PREVIOUS_RELEASE_ENV_VAR} to a tag, e.g. v2.3.0: {listed.stderr.strip()}"
            )
        tag = listed.stdout.strip()

    target_dir = tmp_path_factory.mktemp("previous-release")
    asset = _asset_name()
    fetched = subprocess.run(
        ["gh", "release", "download", tag, "--pattern", asset, "--dir", str(target_dir)],
        capture_output=True,
        text=True,
    )
    binary = target_dir / asset
    if fetched.returncode != 0 or not binary.exists():
        pytest.skip(f"could not download {asset} from release {tag}: {fetched.stderr.strip()}")

    binary.chmod(binary.stat().st_mode | stat.S_IXUSR)
    return str(binary)


def _rewrite_metadata(archive: Path, destination: Path, changes: dict) -> Path:
    """Copy an artifact with `changes` merged into its `backup_information.json`.

    The component files and their checksums are carried over untouched, so the copy differs
    from the original in exactly the metadata fields under test — which is what makes a
    restore of it evidence about those fields rather than about repacking.
    """
    destination.mkdir(parents=True, exist_ok=True)
    work = destination / "unpacked"
    with tarfile.open(archive, "r:gz") as tar:
        tar.extractall(work)  # noqa: S202 - our own artifact, produced by the run above

    metadata_path = work / "backup" / "backup_information.json"
    metadata = json.loads(metadata_path.read_text())
    metadata.update(changes)
    metadata_path.write_text(json.dumps(metadata, indent=2))

    rewritten = destination / archive.name
    with tarfile.open(rewritten, "w:gz") as tar:
        tar.add(work / "backup", arcname="backup")
    return rewritten


@pytest.mark.e2e
@pytest.mark.docker
class TestDockerExternalArtifactCompatibility(TestInfrahubDockerClient):
    """One internal Docker stack, two artifacts the current binary must be able to restore."""

    async def test_restore_an_artifact_from_the_previous_release(
        self, infrahub_compose, infrahub_port, backup_binary, previous_release_binary, tmp_path
    ):
        """An artifact the previous release wrote restores with the current binary.

        Proves FR-015 and FR-016 in the direction that actually breaks: a metadata addition
        that a new reader requires rather than merely tolerates would make every artifact in
        an operator's existing pool unrestorable, and they would find out during an incident.
        """
        url = f"http://localhost:{infrahub_port}"
        project = infrahub_compose.project_name
        backup_dir = tmp_path / "backups"

        await wait_for_http(f"{url}/api/config", timeout=180.0, interval=1.0)
        seed = await seed_infrahub_data(url, ADMIN_TOKEN)

        # Written by the previous release, deliberately knowing nothing of this feature.
        run_backup(
            previous_release_binary,
            ["--project", project, "--backup-dir", str(backup_dir), "create", "--force"],
        )
        archive = find_latest_backup(backup_dir)
        metadata = read_backup_metadata(archive)
        assert "external_location" not in metadata, (
            "the previous release wrote this feature's fields, so the case no longer tests "
            f"reading an older artifact: {metadata}"
        )

        await wait_for_http(f"{url}/api/config", timeout=180.0, interval=1.0)
        await modify_infrahub_data(url, ADMIN_TOKEN, seed)

        run_restore(backup_binary, ["--project", project, "restore", str(archive)])

        # The restore stops and starts the application containers.
        await wait_for_http(f"{url}/api/config", timeout=180.0, interval=1.0)
        await verify_infrahub_data(url, ADMIN_TOKEN, seed)

    async def test_restore_an_external_origin_artifact_into_an_internal_deployment(
        self, infrahub_compose, infrahub_port, backup_binary, tmp_path
    ):
        """An artifact captured from an external database restores into an internal deployment.

        Proves FR-015 and FR-016 in the other direction: the restore path dispatches on where
        the database being written *is*, not on where the artifact came from, so metadata
        saying the source was external must not send an internal restore down the external
        path — which would look for an object store the deployment has no reason to configure.
        """
        url = f"http://localhost:{infrahub_port}"
        project = infrahub_compose.project_name
        backup_dir = tmp_path / "backups"

        await wait_for_http(f"{url}/api/config", timeout=180.0, interval=1.0)
        seed = await seed_infrahub_data(url, ADMIN_TOKEN)

        run_backup(backup_binary, ["--project", project, "--backup-dir", str(backup_dir), "create", "--force"])
        archive = find_latest_backup(backup_dir)

        # Exactly the fields an external capture adds, and nothing else.
        external_origin = _rewrite_metadata(
            archive,
            tmp_path / "external-origin",
            {
                "external_location": {"database": True, "task-manager-db": True},
                "source_endpoints": {
                    "database": ["neo4j-a.example.internal:6362", "neo4j-b.example.internal:6362"],
                    "task-manager-db": ["pg.example.internal:5432"],
                },
                "source_roles": {
                    "database": {"neo4j-a.example.internal": "primary", "neo4j-b.example.internal": "secondary"},
                    "task-manager-db": {"pg.example.internal": "primary"},
                },
                "capture_complete": True,
            },
        )

        await wait_for_http(f"{url}/api/config", timeout=180.0, interval=1.0)
        await modify_infrahub_data(url, ADMIN_TOKEN, seed)

        # No --allow-external-restore and no --s3-*: this deployment's databases are internal,
        # so neither the authorisation nor the seed staging applies to it.
        run_restore(backup_binary, ["--project", project, "restore", str(external_origin)])

        # The restore stops and starts the application containers.
        await wait_for_http(f"{url}/api/config", timeout=180.0, interval=1.0)
        await verify_infrahub_data(url, ADMIN_TOKEN, seed)
