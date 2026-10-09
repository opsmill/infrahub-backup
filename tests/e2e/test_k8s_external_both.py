"""E2E tests: Kubernetes with both databases outside the namespace.

Covers quickstart Scenarios 2, 3 and 5 of spec 007-external-database-backup — both
databases captured, the full round trip, and the restore refused without its
authorisation — plus the two restore-side rows of the diagnostic scenario table (US2):
an artifact URI the database server cannot read, and one it can read but cannot load.

The cases run in the order they are written, which matters on a shared deployment: the
capture-only case and the refusal case must both observe a deployment nothing has restored
into, so they precede the round trip that destroys and rebuilds its data, and the
diagnostic rows come last because the second of them stages a deliberately corrupt
artifact.

Restoring into an external Neo4j inverts the data path — the server fetches the artifact
itself via `seedURI` — so the round trip needs an object store the *database host* can
read, not merely one this suite can reach. That, plus a Neo4j Enterprise licence outside
the cluster, is why these cases skip by name until the environment declares the topology;
see the external-topology block in `tests/e2e/conftest.py`. Provisioning it in CI is T072
in `specs/archive/007-external-database-backup/tasks.md`, still open, so skips here are skips and
prove nothing.
"""

import re
from pathlib import Path

import pytest

from tests.e2e.conftest import (
    SERVICE_NEO4J,
    SERVICE_TASK_MANAGER_DB,
    external_db_env,
    portforward_infrahub,
    requires_external_db,
    transient_objects,
)
from tests.helpers.utils import (
    find_latest_backup,
    modify_infrahub_data,
    read_backup_metadata,
    run_backup,
    run_cli,
    run_restore,
    seed_infrahub_data,
    verify_infrahub_data,
)

BOTH_EXTERNAL = (SERVICE_NEO4J, SERVICE_TASK_MANAGER_DB)

# From src/internal/app/external_backup_gate.go: the refusal states, in the same sentence,
# that nothing was touched. Scenario 5 asserts the claim and then verifies it independently.
UNTOUCHED_CLAIM = "no Infrahub service was stopped and no data was changed"


def _seed_store_args(seed_store: dict) -> list[str]:
    """The `--s3-*` arguments naming the store a restore stages its seed into.

    Deliberately the existing S3 surface rather than a flag of its own: the artefact store
    a restore stages into is the bucket the operator already configures
    (contracts/artifact-and-metadata.md, "Restore-side staging").
    """
    args = [
        "--s3-bucket",
        seed_store["bucket"],
        "--s3-endpoint",
        seed_store["endpoint"],
        "--s3-region",
        seed_store["region"],
    ]
    if seed_store["prefix"]:
        args += ["--s3-prefix", seed_store["prefix"]]
    return args


def _seed_store_env(seed_store: dict) -> dict[str, str]:
    return {
        "AWS_ACCESS_KEY_ID": seed_store["access_key"],
        "AWS_SECRET_ACCESS_KEY": seed_store["secret_key"],
    }


async def _workload_replicas(kubeconfig: str, namespace: str) -> dict[str, int | None]:
    """The declared replica count of every Deployment and StatefulSet, keyed by `kind/name`.

    A restore quiesces the deployment and must return it to the scale it found, so the
    before-and-after comparison is the assertion; reading `spec.replicas` rather than
    counting pods is what makes it insensitive to a pod still terminating.
    """
    import kr8s.asyncio
    from kr8s.asyncio.objects import Deployment as AsyncDeployment
    from kr8s.asyncio.objects import StatefulSet as AsyncStatefulSet

    api = await kr8s.asyncio.api(kubeconfig=kubeconfig)
    replicas: dict[str, int | None] = {}
    async for deployment in AsyncDeployment.list(namespace=namespace, api=api):
        if not isinstance(deployment, dict):
            replicas[f"deployment/{deployment.name}"] = deployment.raw["spec"].get("replicas")
    async for statefulset in AsyncStatefulSet.list(namespace=namespace, api=api):
        if not isinstance(statefulset, dict):
            replicas[f"statefulset/{statefulset.name}"] = statefulset.raw["spec"].get("replicas")
    return replicas


def _archive_whose_neo4j_artifact_cannot_be_loaded(archive: Path, destination: Path) -> Path:
    """Rebuild `archive` at `destination` with the Neo4j artifact replaced by unloadable
    bytes, and every check that reads the artifact still satisfied.

    This is what FR-021 needs and what a truncated archive cannot give it. Truncating the
    `.tar.gz` breaks gzip decompression, so the run fails while *extracting* — upstream of
    the seed staging and of the load on the database server, which is the stage FR-021 is
    about. Replacing bytes inside the archive without repairing the metadata does not get
    much further: the restore validates the checksums it recorded at capture time, so it
    fails there instead, still before anything is staged.

    So the artifact has to be internally consistent and substantively wrong: same member
    names, same sizes, checksums that match the bytes that are actually there. Every check
    this host can make then passes, the artifact stages and reads back from the object
    store, and the first thing that can notice is `neo4j-admin database restore` on the
    database server itself — which is exactly the boundary between FR-007 (this run could
    not read the artifact) and FR-021 (the database did not come online after a seed).

    The members to overwrite are taken from the metadata's own checksum map rather than
    from a path in the archive, so this stays correct if the capture's layout moves.
    """
    import hashlib
    import io
    import json
    import tarfile

    metadata_name = "backup/backup_information.json"

    with tarfile.open(archive, "r:gz") as source:
        members = source.getmembers()
        payloads: dict[str, bytes] = {}
        for member in members:
            if not member.isfile():
                continue
            handle = source.extractfile(member)
            assert handle is not None, f"{member.name} is a file member with no data"
            payloads[member.name] = handle.read()

    assert metadata_name in payloads, f"{archive.name} is missing {metadata_name}"
    metadata = json.loads(payloads[metadata_name])
    checksums = dict(metadata.get("checksums") or {})
    # prefect.dump is the task manager's; the rest of the map is the Neo4j artifact, whose
    # load is the step under test.
    artifacts = sorted(name for name in checksums if name != "prefect.dump")
    assert artifacts, f"{archive.name} records no Neo4j artifact checksums: {sorted(checksums)}"

    for relative in artifacts:
        name = f"backup/{relative}"
        assert name in payloads, f"{archive.name} records a checksum for {relative} but has no such member"
        payloads[name] = b"\x00" * len(payloads[name])
        checksums[relative] = hashlib.sha256(payloads[name]).hexdigest()

    metadata["checksums"] = checksums
    payloads[metadata_name] = json.dumps(metadata, indent=4).encode()

    destination.parent.mkdir(parents=True, exist_ok=True)
    with tarfile.open(destination, "w:gz") as rebuilt:
        for member in members:
            if not member.isfile():
                rebuilt.addfile(member)
                continue
            body = payloads[member.name]
            member.size = len(body)
            rebuilt.addfile(member, io.BytesIO(body))

    return destination


@pytest.mark.e2e
@pytest.mark.k8s
@requires_external_db(*BOTH_EXTERNAL, edition="enterprise")
async def test_both_databases_external_are_both_captured(
    infrahub_k8s_external, external_topology, backup_binary, tmp_path
):
    """Quickstart Scenario 2: both captured, recorded per database, nothing left behind.

    Proves FR-001, FR-011 and SC-006. Recording the endpoints and roles *per database* is
    what makes the artifact readable by a restore that has to dispatch on where each
    database lives, rather than on a single verdict for the deployment.
    """
    namespace = infrahub_k8s_external["namespace"]
    kubeconfig = infrahub_k8s_external["kubeconfig_path"]
    backup_dir = tmp_path / "backups"
    env = {"KUBECONFIG": kubeconfig, **external_db_env(external_topology)}

    run_backup(
        backup_binary,
        ["--k8s-namespace", namespace, "--backup-dir", str(backup_dir), "create", "--force"],
        env=env,
    )
    metadata = read_backup_metadata(find_latest_backup(backup_dir))

    external_location = metadata.get("external_location", {})
    source_endpoints = metadata.get("source_endpoints", {})
    source_roles = metadata.get("source_roles", {})
    for service in BOTH_EXTERNAL:
        assert external_location.get(service) is True, (
            f"external_location does not mark {service} external: {external_location}"
        )
        assert source_endpoints.get(service), f"source_endpoints records no endpoint for {service}: {source_endpoints}"
        assert source_roles.get(service), f"source_roles records no observed roles for {service}: {source_roles}"

    assert set(metadata.get("components", [])) >= {"database", "task-manager"}, (
        f"an artifact of both databases must list both components: {metadata.get('components')}"
    )
    assert metadata.get("capture_complete") is not False, "a retained artifact must not be an incomplete capture"

    left_behind = await transient_objects(kubeconfig, namespace)
    assert left_behind == [], f"transient objects survived the run: {left_behind}"


@pytest.mark.e2e
@pytest.mark.k8s
@requires_external_db(*BOTH_EXTERNAL, edition="enterprise", seed_store=True)
async def test_restore_is_refused_without_the_authorisation(
    infrahub_k8s_external, external_topology, external_seed_store, backup_binary, tmp_path
):
    """Quickstart Scenario 5: refused, and the databases are demonstrably untouched.

    Proves FR-009, FR-010 and SC-005. The second half is the part worth having: `--force`
    and an unattended `--latest` restore are the two things an operator would expect to
    confer the authorisation, and neither does.
    """
    namespace = infrahub_k8s_external["namespace"]
    kubeconfig = infrahub_k8s_external["kubeconfig_path"]
    backup_dir = tmp_path / "backups"
    env = {"KUBECONFIG": kubeconfig, **external_db_env(external_topology), **_seed_store_env(external_seed_store)}
    # The store is configured even though a refused restore stages nothing into it: with it
    # present, the only thing missing from the invocation is the authorisation, so the
    # refusal under test cannot be a refusal about an unconfigured artefact store (FR-020).
    store_args = _seed_store_args(external_seed_store)

    async with portforward_infrahub(kubeconfig, namespace) as url:
        seed = await seed_infrahub_data(url, infrahub_k8s_external["token"])

    run_backup(
        backup_binary,
        ["--k8s-namespace", namespace, "--backup-dir", str(backup_dir), "create", "--force"],
        env=env,
    )
    archive = find_latest_backup(backup_dir)

    # Delete the tag, so a restore that ran anyway would put it back — which is how the
    # "databases untouched" claim is checked rather than believed.
    async with portforward_infrahub(kubeconfig, namespace) as url:
        await modify_infrahub_data(url, infrahub_k8s_external["token"], seed)

    before = await _workload_replicas(kubeconfig, namespace)

    # --force does not confer it.
    forced = run_cli(
        backup_binary,
        ["--k8s-namespace", namespace, *store_args, "restore", "--force", str(archive)],
        env=env,
    )
    # An unattended `restore --latest`, the shape a scheduled restore takes, does not either.
    scheduled = run_cli(
        backup_binary,
        ["--k8s-namespace", namespace, "--backup-dir", str(backup_dir), *store_args, "restore", "--force", "--latest"],
        env=env,
    )

    for label, result in (("--force", forced), ("--latest", scheduled)):
        combined = result.stdout + result.stderr
        assert result.returncode != 0, f"{label} must not authorise an external restore:\n{combined}"
        assert "--allow-external-restore" in combined, (
            f"the refusal of {label} does not name the flag that authorises it:\n{combined}"
        )
        assert "INFRAHUB_ALLOW_EXTERNAL_RESTORE" in combined, (
            f"the refusal of {label} does not name the configuration key:\n{combined}"
        )
        assert UNTOUCHED_CLAIM in combined, (
            f"the refusal of {label} does not state that nothing was touched:\n{combined}"
        )

    # The claim, verified: the deployment was neither quiesced nor rescaled, and the data is
    # still the post-deletion state rather than the artifact's.
    assert await _workload_replicas(kubeconfig, namespace) == before, "a refused restore rescaled the deployment"
    async with portforward_infrahub(kubeconfig, namespace) as url:
        from infrahub_sdk import Config as InfrahubConfig
        from infrahub_sdk import InfrahubClient

        client = InfrahubClient(config=InfrahubConfig(address=url, api_token=infrahub_k8s_external["token"]))
        tags = await client.all(kind="BuiltinTag")
        assert all(tag.name.value != seed["tag_name"] for tag in tags), (
            f"a refused restore put {seed['tag_name']} back, so it was not refused before the destructive step"
        )


@pytest.mark.e2e
@pytest.mark.k8s
@requires_external_db(*BOTH_EXTERNAL, edition="enterprise", seed_store=True)
async def test_full_round_trip_through_both_external_databases(
    infrahub_k8s_external, external_topology, external_seed_store, backup_binary, tmp_path
):
    """Quickstart Scenario 3: capture, destroy, restore, and serve the pre-backup data again.

    Proves FR-013, FR-021 and SC-001 — the criterion the whole feature exists to satisfy.
    The run must not report success until the restored database reports a usable state, so
    a zero exit followed by data that is not back is the failure this asserts against.
    """
    namespace = infrahub_k8s_external["namespace"]
    kubeconfig = infrahub_k8s_external["kubeconfig_path"]
    token = infrahub_k8s_external["token"]
    backup_dir = tmp_path / "backups"
    env = {"KUBECONFIG": kubeconfig, **external_db_env(external_topology), **_seed_store_env(external_seed_store)}
    store_args = _seed_store_args(external_seed_store)

    async with portforward_infrahub(kubeconfig, namespace) as url:
        seed = await seed_infrahub_data(url, token)

    run_backup(
        backup_binary,
        ["--k8s-namespace", namespace, "--backup-dir", str(backup_dir), "create", "--force"],
        env=env,
    )
    archive = find_latest_backup(backup_dir)

    before = await _workload_replicas(kubeconfig, namespace)

    async with portforward_infrahub(kubeconfig, namespace) as url:
        await modify_infrahub_data(url, token, seed)

    run_restore(
        backup_binary,
        [
            "--k8s-namespace",
            namespace,
            *store_args,
            "restore",
            "--allow-external-restore",
            "--force",
            str(archive),
        ],
        env=env,
    )

    # FR-013: back to the scale the restore found, not to whatever the chart declares.
    assert await _workload_replicas(kubeconfig, namespace) == before, (
        "the restore did not return the workloads to their prior scale"
    )

    # SC-001: the deployment serves the pre-backup data.
    async with portforward_infrahub(kubeconfig, namespace) as url:
        await verify_infrahub_data(url, token, seed)

    # FR-011 / FR-023: and the restore left nothing of its own behind either.
    left_behind = await transient_objects(kubeconfig, namespace)
    assert left_behind == [], f"transient objects survived the restore: {left_behind}"


# ---------------------------------------------------------------------------
# The diagnostic scenario table, restore-side rows (US2, quickstart)
# ---------------------------------------------------------------------------
#
# The other four rows are in `test_k8s_external_neo4j.py`, which needs only one external
# database. These two need a restore, so they need both — and, like the round trip above,
# an object store the database host can read.
#
# They run last in this file on purpose. The first destroys nothing, and the second is the
# only case here that deliberately leaves a corrupt artifact behind; putting either before
# the round trip would have it observe a deployment these had already interfered with.


@pytest.mark.e2e
@pytest.mark.k8s
@requires_external_db(*BOTH_EXTERNAL, edition="enterprise", seed_store=True)
async def test_an_unreadable_artifact_uri_is_refused_before_anything_destructive(
    infrahub_k8s_external, external_topology, external_seed_store, backup_binary, tmp_path
):
    """Diagnostic row 5: refused before any destructive step, database untouched.

    Proves FR-007 and FR-020. The Neo4j server fetches the artifact itself, so a URI this
    run cannot read is one the server certainly cannot — and discovering that *after*
    Infrahub has been quiesced and the database overwritten is the failure mode the
    pre-flight exists to prevent.

    Provoked by staging into a bucket that does not exist, which is the shape the real
    failure takes: a bucket named in configuration that the credentials cannot reach.
    """
    namespace = infrahub_k8s_external["namespace"]
    kubeconfig = infrahub_k8s_external["kubeconfig_path"]
    token = infrahub_k8s_external["token"]
    backup_dir = tmp_path / "backups"
    env = {"KUBECONFIG": kubeconfig, **external_db_env(external_topology), **_seed_store_env(external_seed_store)}

    async with portforward_infrahub(kubeconfig, namespace) as url:
        seed = await seed_infrahub_data(url, token)

    run_backup(
        backup_binary,
        ["--k8s-namespace", namespace, "--backup-dir", str(backup_dir), "create", "--force"],
        env=env,
    )
    archive = find_latest_backup(backup_dir)

    before = await _workload_replicas(kubeconfig, namespace)
    unreachable_bucket = f"{external_seed_store['bucket']}-does-not-exist"

    result = run_cli(
        backup_binary,
        [
            "--k8s-namespace",
            namespace,
            "--s3-bucket",
            unreachable_bucket,
            "--s3-endpoint",
            external_seed_store["endpoint"],
            "--s3-region",
            external_seed_store["region"],
            "restore",
            "--allow-external-restore",
            "--force",
            str(archive),
        ],
        env=env,
    )
    combined = result.stdout + result.stderr

    assert result.returncode != 0, f"a restore whose artifact the server cannot read must fail:\n{combined}"

    # Condition and resource: the URI it attempted, named.
    assert unreachable_bucket in combined, f"the refusal does not name the location it tried to stage into:\n{combined}"
    # Operator action: the server-side prerequisite, which is what separates this from an
    # ordinary object-store error — the remedy is on the database host, not here.
    assert re.search(r"servers? (fetch|can read)|from the database host", combined, re.IGNORECASE), (
        f"the refusal does not name the server-side prerequisite, so an operator cannot tell it "
        f"from a bucket typo on this host:\n{combined}"
    )
    assert UNTOUCHED_CLAIM in combined, f"the refusal does not state that nothing was touched:\n{combined}"

    # The claim, verified independently: FR-007 requires the refusal to come before the
    # destructive step, and the deployment still serving the *current* data is the proof.
    assert await _workload_replicas(kubeconfig, namespace) == before, "a refused restore rescaled the deployment"
    async with portforward_infrahub(kubeconfig, namespace) as url:
        await verify_infrahub_data(url, token, seed)

    left_behind = await transient_objects(kubeconfig, namespace)
    assert left_behind == [], f"transient objects survived the refused restore: {left_behind}"


@pytest.mark.e2e
@pytest.mark.k8s
@requires_external_db(*BOTH_EXTERNAL, edition="enterprise", seed_store=True)
async def test_a_readable_but_corrupt_artifact_fails_and_never_reports_success(
    infrahub_k8s_external, external_topology, external_seed_store, backup_binary, tmp_path
):
    """Diagnostic row 6: the restore fails, and success is never reported.

    Proves FR-021. This is the row the pre-flight cannot cover and the reason FR-021 exists
    separately from FR-007: the artifact is readable, so every check that runs before the
    destructive step passes, and the seed is validated only as the database starts. What
    must not happen is a zero exit — an operator who is told a restore succeeded stops
    looking, and finds out at the next outage.
    """
    namespace = infrahub_k8s_external["namespace"]
    kubeconfig = infrahub_k8s_external["kubeconfig_path"]
    backup_dir = tmp_path / "backups"
    env = {"KUBECONFIG": kubeconfig, **external_db_env(external_topology), **_seed_store_env(external_seed_store)}
    store_args = _seed_store_args(external_seed_store)

    async with portforward_infrahub(kubeconfig, namespace) as url:
        await seed_infrahub_data(url, infrahub_k8s_external["token"])

    run_backup(
        backup_binary,
        ["--k8s-namespace", namespace, "--backup-dir", str(backup_dir), "create", "--force"],
        env=env,
    )
    archive = find_latest_backup(backup_dir)

    # An artifact this host can read, stage and verify in full, and that the database
    # server cannot load. Every earlier stage has to pass for this row to be the row it
    # claims to be — see _archive_whose_neo4j_artifact_cannot_be_loaded, which is also
    # where the two cheaper corruptions are ruled out and why.
    corrupt = _archive_whose_neo4j_artifact_cannot_be_loaded(archive, tmp_path / "corrupt" / archive.name)

    result = run_cli(
        backup_binary,
        [
            "--k8s-namespace",
            namespace,
            *store_args,
            "restore",
            "--allow-external-restore",
            "--force",
            str(corrupt),
        ],
        env=env,
    )
    combined = result.stdout + result.stderr

    # The whole of this row: never a zero exit.
    assert result.returncode != 0, (
        f"a corrupt artifact produced a successful restore, so an operator is told their data "
        f"is back when it is not (FR-021):\n{combined}"
    )
    assert not re.search(r"restore completed successfully|Restore complete", combined, re.IGNORECASE), (
        f"the run reported success alongside a non-zero exit:\n{combined}"
    )
    # Condition, resource and action. Whichever stage catches it — the archive read, the
    # seed, or the database failing to come online — the message has to say which database
    # and where to look, because on this path the cause is in the server's own logs.
    assert re.search(r"corrupt|invalid|unexpected|cannot|failed", combined, re.IGNORECASE), (
        f"the failure does not name a condition:\n{combined}"
    )

    # FR-011: a restore that failed still gives back everything it created.
    left_behind = await transient_objects(kubeconfig, namespace)
    assert left_behind == [], f"transient objects survived the failed restore: {left_behind}"
