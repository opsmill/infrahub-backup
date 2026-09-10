"""E2E tests: Kubernetes + an external Neo4j, with the task manager's PostgreSQL in-namespace.

Covers quickstart Scenarios 1 and 4 of spec 007-external-database-backup — the mixed
topology, and the client/server version gate in both directions — plus the robustness
cases from the same document and four of the six rows of its diagnostic scenario table
(US2): the external Community refusal, an undiscoverable endpoint, a revoked pod listing,
and an absent pod-creation permission. Those live here rather than in
`test_k8s_external_both.py` because they need only one external database: a case gated on
the weaker topology runs sooner, and none of them is about PostgreSQL. The two
restore-side diagnostic rows are in the both-external suite, which is where a restore is.

The Community refusal is the one case in this file gated on `community` rather than
`enterprise`, so it and the round-trip cases are mutually exclusive on any one topology —
which is the point of pinning the edition rather than inferring it from what answers.

Every case here needs a database outside the namespace, which this suite cannot create:
it needs a Neo4j Enterprise licence and a backup listener bound to a non-loopback address.
The topology is declared by environment variables documented in `tests/e2e/conftest.py`,
and each case skips *by name* until they are set. Provisioning it in CI is T072 in
`specs/archive/007-external-database-backup/tasks.md`, which is still open — so a green run of
this file that reports skips has proved nothing, and says so.
"""

import asyncio
import os
import re

import kr8s.asyncio
import pytest
from kr8s.asyncio.objects import Deployment as AsyncDeployment

from tests.e2e.conftest import (
    POD_READ_RULE,
    SERVICE_NEO4J,
    SERVICE_TASK_MANAGER_DB,
    UNREACHABLE_ENDPOINT,
    external_db_env,
    portforward_infrahub,
    requires_external_db,
    restricted_kubeconfig,
    transient_objects,
)
from tests.helpers.utils import (
    find_latest_backup,
    read_backup_metadata,
    run_backup,
    run_cli,
    seed_infrahub_data,
    start_cli,
    wait_for_cli_output,
)

# The log line the resolved endpoint is reported on, and the first line that follows a
# connection to it. Scenario 1 requires the endpoint to be reported *before* anything is
# opened, so their order in the log is the assertion, not merely their presence.
RESOLVED_ENDPOINT_LINE = f"Resolved {SERVICE_NEO4J} endpoint:"
TRANSIENT_WORKLOAD_LINE = "Creating a transient"

# Images whose Neo4j tooling sits either side of the server, for the two directions of the
# version gate. The defaults assume a calendar-versioned server (`2025.x`), the scheme the
# quickstart's compatibility-envelope section says must be measured rather than assumed; a
# `5.x` server needs both variables set, because `5.26` is *newer* than nothing in the
# calendar scheme and the gate would then be provoked in one direction only.
OLDER_TOOLING_IMAGE_VAR = "INFRAHUB_TESTING_EXTERNAL_NEO4J_OLDER_IMAGE"
NEWER_TOOLING_IMAGE_VAR = "INFRAHUB_TESTING_EXTERNAL_NEO4J_NEWER_IMAGE"
DEFAULT_OLDER_TOOLING_IMAGE = "neo4j:5.26.0-enterprise"
DEFAULT_NEWER_TOOLING_IMAGE = "neo4j:2025.10.1-enterprise"

# The gate's two messages, from src/internal/app/external_endpoint.go. The abort names both
# versions and the flag that changes the tooling; the reverse direction only warns.
OLDER_THAN_SERVER = re.compile(r"utility \(([^)]+)\) is older than the server \(([^)]+)\)")
NEWER_THAN_SERVER = re.compile(r"utility \(([^)]+)\) is newer than the server \(([^)]+)\)")


def _log(result) -> str:
    """The tool's log stream.

    logrus is left on its default output, so every line the assertions below look for is on
    stderr; stdout carries command output only. Reading one stream is what makes the
    ordering assertion in Scenario 1 meaningful — interleaving two would not preserve it.
    """
    return result.stderr


def _archives(backup_dir) -> list[str]:
    """Every backup archive in the directory, or [] if the run never created it."""
    if not backup_dir.exists():
        return []
    return sorted(path.name for path in backup_dir.iterdir() if path.name.startswith("infrahub_backup_"))


@pytest.mark.e2e
@pytest.mark.k8s
@requires_external_db(SERVICE_NEO4J, edition="enterprise")
async def test_mixed_topology_reports_and_records_the_external_neo4j(
    infrahub_k8s_external, external_topology, backup_binary, tmp_path
):
    """Quickstart Scenario 1: one artifact, and metadata that says which database was external.

    Proves FR-001 (external mode resolved per service, so PostgreSQL staying internal is
    not incidental but asserted), FR-003 (the endpoint is discovered and reported before it
    is used) and SC-004.
    """
    namespace = infrahub_k8s_external["namespace"]
    kubeconfig = infrahub_k8s_external["kubeconfig_path"]
    backup_dir = tmp_path / "backups"
    env = {"KUBECONFIG": kubeconfig, **external_db_env(external_topology)}

    async with portforward_infrahub(kubeconfig, namespace) as url:
        await seed_infrahub_data(url, infrahub_k8s_external["token"])

    result = run_backup(
        backup_binary,
        ["--k8s-namespace", namespace, "--backup-dir", str(backup_dir), "create", "--force"],
        env=env,
    )
    log = _log(result)

    # FR-003: reported before anything is opened against it. An endpoint an operator only
    # learns about from a failure is not a discovered endpoint.
    assert RESOLVED_ENDPOINT_LINE in log, f"the resolved endpoint was never reported:\n{log}"
    assert TRANSIENT_WORKLOAD_LINE in log, f"no transient workload was created:\n{log}"
    assert log.index(RESOLVED_ENDPOINT_LINE) < log.index(TRANSIENT_WORKLOAD_LINE), (
        "the endpoint must be reported before a connection is opened to it"
    )
    for host in external_topology["neo4j_address"].split(","):
        hostname = host.strip().rsplit(":", 1)[0]
        assert hostname in log, f"the resolved endpoint does not name {hostname}:\n{log}"

    # One artifact covering both databases, not one per database.
    archives = _archives(backup_dir)
    assert len(archives) == 1, f"expected exactly one artifact, got {archives}"
    metadata = read_backup_metadata(find_latest_backup(backup_dir))

    # FR-001: external is a per-service verdict. Neo4j external, PostgreSQL not.
    external_location = metadata.get("external_location", {})
    assert external_location.get(SERVICE_NEO4J) is True, (
        f"external_location does not mark {SERVICE_NEO4J} external: {external_location}"
    )
    assert external_location.get(SERVICE_TASK_MANAGER_DB) is not True, (
        f"the in-namespace {SERVICE_TASK_MANAGER_DB} was recorded as external: {external_location}"
    )

    # FR-003 / SC-004: what was captured, and from where.
    assert metadata.get("source_endpoints", {}).get(SERVICE_NEO4J), (
        f"source_endpoints records no endpoint for {SERVICE_NEO4J}: {metadata.get('source_endpoints')}"
    )
    assert metadata.get("source_roles", {}).get(SERVICE_NEO4J), (
        f"source_roles records no observed roles for {SERVICE_NEO4J}: {metadata.get('source_roles')}"
    )
    assert metadata.get("capture_complete") is not False, "a retained artifact must not be an incomplete capture"

    # FR-011 / FR-023: nothing of the run's own survives it.
    left_behind = await transient_objects(kubeconfig, namespace)
    assert left_behind == [], f"transient objects survived the run: {left_behind}"


@pytest.mark.e2e
@pytest.mark.k8s
@requires_external_db(SERVICE_NEO4J, edition="enterprise")
async def test_tooling_older_than_the_server_aborts_before_writing_anything(
    infrahub_k8s_external, external_topology, backup_binary, tmp_path
):
    """Quickstart Scenario 4, first direction: abort, both versions named, zero bytes written.

    Proves FR-006 and SC-003. The zero-bytes half is the point: a version gate that fires
    after the capture has begun leaves a partial artifact behind to be mistaken for a good
    one.
    """
    namespace = infrahub_k8s_external["namespace"]
    kubeconfig = infrahub_k8s_external["kubeconfig_path"]
    backup_dir = tmp_path / "backups"
    older_image = os.environ.get(OLDER_TOOLING_IMAGE_VAR, DEFAULT_OLDER_TOOLING_IMAGE)
    env = {"KUBECONFIG": kubeconfig, **external_db_env(external_topology)}

    result = run_cli(
        backup_binary,
        [
            "--k8s-namespace",
            namespace,
            "--backup-dir",
            str(backup_dir),
            "--external-db-image-neo4j",
            older_image,
            "create",
            "--force",
        ],
        env=env,
    )

    assert result.returncode != 0, f"an older client than the server must abort:\n{_log(result)}"
    match = OLDER_THAN_SERVER.search(result.stdout + result.stderr)
    assert match, f"the abort does not name both versions:\n{result.stdout}\n{result.stderr}"
    utility, server = match.group(1), match.group(2)
    assert utility and server and utility != server, f"both versions must be named and differ: {utility!r} {server!r}"
    assert "--external-db-image-neo4j" in result.stdout + result.stderr, (
        "the abort must name the flag that changes the tooling"
    )
    assert _archives(backup_dir) == [], "an aborted run must write no artifact at all"


@pytest.mark.e2e
@pytest.mark.k8s
@requires_external_db(SERVICE_NEO4J, edition="enterprise")
async def test_tooling_newer_than_the_server_warns_and_continues(
    infrahub_k8s_external, external_topology, backup_binary, tmp_path
):
    """Quickstart Scenario 4, second direction: a warning, not an abort.

    The asymmetry established in research R3: no documented rule forbids newer tooling, so
    refusing it would make the tool decline work it could have done (FR-006).
    """
    namespace = infrahub_k8s_external["namespace"]
    kubeconfig = infrahub_k8s_external["kubeconfig_path"]
    backup_dir = tmp_path / "backups"
    newer_image = os.environ.get(NEWER_TOOLING_IMAGE_VAR, DEFAULT_NEWER_TOOLING_IMAGE)
    env = {"KUBECONFIG": kubeconfig, **external_db_env(external_topology)}

    result = run_backup(
        backup_binary,
        [
            "--k8s-namespace",
            namespace,
            "--backup-dir",
            str(backup_dir),
            "--external-db-image-neo4j",
            newer_image,
            "create",
            "--force",
        ],
        env=env,
    )

    assert NEWER_THAN_SERVER.search(_log(result)), f"the newer direction must be reported as a warning:\n{_log(result)}"
    assert len(_archives(backup_dir)) == 1, "a warning must not stop the capture"


# ---------------------------------------------------------------------------
# Robustness scenarios (quickstart, "Robustness scenarios")
# ---------------------------------------------------------------------------
@pytest.mark.e2e
@pytest.mark.k8s
@requires_external_db(SERVICE_NEO4J, edition="enterprise")
async def test_killing_the_tool_mid_capture_leaves_nothing_behind(
    infrahub_k8s_external, external_topology, backup_binary, tmp_path
):
    """Killed mid-capture: no surviving pod or Secret, and the deployment is not left quiesced.

    Proves FR-011, FR-013 and SC-006. The kill is pinned to the moment the transient
    workload reports ready, so the run is interrupted while it holds both a pod and the
    Secret carrying the database credentials — the state a sleep-based kill would miss.
    """
    namespace = infrahub_k8s_external["namespace"]
    kubeconfig = infrahub_k8s_external["kubeconfig_path"]
    backup_dir = tmp_path / "backups"
    env = {"KUBECONFIG": kubeconfig, **external_db_env(external_topology)}

    proc, log_path = start_cli(
        backup_binary,
        ["--k8s-namespace", namespace, "--backup-dir", str(backup_dir), "create", "--force"],
        env=env,
        log_path=tmp_path / "killed-mid-capture.log",
    )
    try:
        wait_for_cli_output(log_path, "is ready to run", timeout=600.0)
        proc.kill()
    finally:
        proc.wait(timeout=120)

    # A second run reaps the stray rather than adopting it, so the assertion is made
    # against the namespace as the kill left it, before anything else runs.
    left_behind = await transient_objects(kubeconfig, namespace)
    assert left_behind == [], (
        f"a killed run left transient objects behind: {left_behind}. "
        "The cluster reclaims them at their deadline, but FR-011 requires the objects to be "
        "owned so that reclaiming is not the only thing that removes them"
    )

    # FR-013: Infrahub still serves. A killed backup must not leave a quiesced deployment.
    async with portforward_infrahub(kubeconfig, namespace):
        pass


@pytest.mark.e2e
@pytest.mark.k8s
@requires_external_db(SERVICE_NEO4J, edition="enterprise")
async def test_credentials_are_reclaimed_with_the_workload_without_the_tool(
    infrahub_k8s_external, external_topology, backup_binary, tmp_path
):
    """Killed between credential creation and workload deletion: the Secret goes with the pod.

    Proves FR-023. Deleting the pod by hand — with no tool involvement at all — must be
    enough to remove the credentials, which is only true if the Secret is owned by the pod
    rather than cleaned up by a tool that may have been killed.
    """
    import kr8s.asyncio
    from kr8s.asyncio.objects import Pod as AsyncPod
    from kr8s.asyncio.objects import Secret as AsyncSecret

    from tests.e2e.conftest import TRANSIENT_LABEL_MARKER, TRANSIENT_OBJECT_PREFIX

    namespace = infrahub_k8s_external["namespace"]
    kubeconfig = infrahub_k8s_external["kubeconfig_path"]
    env = {"KUBECONFIG": kubeconfig, **external_db_env(external_topology)}

    proc, log_path = start_cli(
        backup_binary,
        ["--k8s-namespace", namespace, "--backup-dir", str(tmp_path / "backups"), "create", "--force"],
        env=env,
        log_path=tmp_path / "killed-after-credentials.log",
    )
    try:
        wait_for_cli_output(log_path, "is ready to run", timeout=600.0)
        proc.kill()
    finally:
        proc.wait(timeout=120)

    api = await kr8s.asyncio.api(kubeconfig=kubeconfig)
    pods = [pod async for pod in AsyncPod.list(namespace=namespace, label_selector=TRANSIENT_LABEL_MARKER, api=api)]
    secrets = [
        secret async for secret in AsyncSecret.list(namespace=namespace, label_selector=TRANSIENT_LABEL_MARKER, api=api)
    ]

    if not pods and not secrets:
        pytest.skip(
            "the run cleaned up before the kill landed, so there was nothing for the cluster to "
            "reclaim; FR-023 is about the case where the tool does not get to clean up"
        )

    # FR-028: the objects are named so that they cannot be mistaken for a replica of the
    # service they stand in for — the pod resolvers fall back to a substring match on the
    # name, so a name containing a service name would surface in replica enumeration
    # however carefully its labels were chosen. This is the one case in these suites where
    # transient objects exist to be inspected, so it is where that is asserted.
    for obj in [*pods, *secrets]:
        assert obj.name.startswith(TRANSIENT_OBJECT_PREFIX), (
            f"{obj.name} does not carry the documented transient prefix {TRANSIENT_OBJECT_PREFIX}"
        )
        for service in (SERVICE_NEO4J, SERVICE_TASK_MANAGER_DB):
            assert service not in obj.name, (
                f"{obj.name} contains the service name {service}, so a substring match will "
                "find it during replica enumeration"
            )

    # Every credential object must be owned by a pod, which is what makes reclaiming the
    # pod sufficient. An unowned Secret survives until something remembers to delete it.
    for secret in secrets:
        owners = secret.raw["metadata"].get("ownerReferences", [])
        assert any(owner.get("kind") == "Pod" for owner in owners), (
            f"secret {secret.name} is not owned by a pod, so nothing reclaims it with the workload"
        )

    for pod in pods:
        await pod.async_delete()

    for _ in range(60):
        remaining = [
            secret.name
            async for secret in AsyncSecret.list(namespace=namespace, label_selector=TRANSIENT_LABEL_MARKER, api=api)
        ]
        if not remaining:
            break
        await asyncio.sleep(5)
    else:
        raise AssertionError(f"credentials survived the workload being reclaimed: {remaining}")


@pytest.mark.e2e
@pytest.mark.k8s
@requires_external_db(SERVICE_NEO4J, edition="enterprise")
async def test_one_unreachable_endpoint_of_several_is_not_a_retained_artifact(
    infrahub_k8s_external, external_topology, backup_binary, tmp_path
):
    """One endpoint of several unreachable: nothing is retained that looks complete.

    Proves FR-012, and it is the case the quickstart says to write first: a partially
    successful capture and an outright failure are indistinguishable by exit status, so an
    artifact retained after one is the most likely source of a silently unusable backup.
    """
    namespace = infrahub_k8s_external["namespace"]
    kubeconfig = infrahub_k8s_external["kubeconfig_path"]
    backup_dir = tmp_path / "backups"
    addresses = f"{external_topology['neo4j_address']},{UNREACHABLE_ENDPOINT}"
    env = {"KUBECONFIG": kubeconfig, **external_db_env(external_topology)}

    result = run_cli(
        backup_binary,
        [
            "--k8s-namespace",
            namespace,
            "--backup-dir",
            str(backup_dir),
            "--neo4j-address",
            addresses,
            "create",
            "--force",
        ],
        env=env,
    )

    combined = result.stdout + result.stderr
    assert result.returncode != 0, f"a capture that reached only some endpoints must not report success:\n{combined}"
    assert "incomplete" in combined, f"the failure does not say the capture was incomplete:\n{combined}"
    assert _archives(backup_dir) == [], (
        "an incomplete capture must not be retained: retention and latest-backup selection "
        "would both treat a retained archive as restorable"
    )
    left_behind = await transient_objects(kubeconfig, namespace)
    assert left_behind == [], f"transient objects survived the failed run: {left_behind}"


@pytest.mark.e2e
@pytest.mark.k8s
@requires_external_db(SERVICE_NEO4J, edition="enterprise")
async def test_scratch_space_too_small_fails_cleanly(infrahub_k8s_external, external_topology, backup_binary, tmp_path):
    """Database larger than the scratch space: succeeds, or fails without exhausting the host.

    Proves FR-022. The size is pinned absurdly low rather than relying on a large database,
    so the case exercises the same code path on any topology: what must not happen is the
    capture filling shared node storage before anything notices.
    """
    namespace = infrahub_k8s_external["namespace"]
    kubeconfig = infrahub_k8s_external["kubeconfig_path"]
    backup_dir = tmp_path / "backups"
    env = {"KUBECONFIG": kubeconfig, **external_db_env(external_topology)}

    result = run_cli(
        backup_binary,
        [
            "--k8s-namespace",
            namespace,
            "--backup-dir",
            str(backup_dir),
            "--external-db-scratch-size",
            "1Mi",
            "create",
            "--force",
        ],
        env=env,
    )

    combined = result.stdout + result.stderr
    if result.returncode == 0:
        # Allowed by FR-022, but only if what it produced is a whole artifact.
        assert len(_archives(backup_dir)) == 1, f"a successful run must leave one artifact:\n{combined}"
        metadata = read_backup_metadata(find_latest_backup(backup_dir))
        assert metadata.get("capture_complete") is not False, "a retained artifact must be a complete capture"
        return

    assert _archives(backup_dir) == [], f"a failed capture must retain nothing:\n{combined}"
    assert re.search(r"scratch|space|storage|ephemeral", combined, re.IGNORECASE), (
        f"the failure must name the scratch space as the condition, not just fail:\n{combined}"
    )
    left_behind = await transient_objects(kubeconfig, namespace)
    assert left_behind == [], f"transient objects survived the failed run: {left_behind}"


# ---------------------------------------------------------------------------
# The diagnostic scenario table (US2, quickstart "Diagnostic scenarios")
# ---------------------------------------------------------------------------
#
# Four of the table's six rows are here, because each needs only one external database;
# the two restore-side rows are in `test_k8s_external_both.py`. Every case asserts the
# three elements contracts/cli-surface.md requires of the message — condition, resource,
# operator action (SC-008) — and the two rows the contract states negatively assert the
# absence as well. A message that carries all three elements and still says the wrong one
# of those is the defect this story exists to remove.
#
# The wording is deliberately not matched verbatim. What the contract fixes is the three
# elements, not the sentence, and a case that pinned the sentence would fail on an
# improvement and pass on a message that dropped an element while keeping the phrasing.
# The unit assertions in `src/internal/app/external_endpoint_test.go`
# (TestFailureMessageContract) hold the constructors to the same three elements; these
# prove the messages reach an operator from a real deployment.


@pytest.mark.e2e
@pytest.mark.k8s
@requires_external_db(SERVICE_NEO4J, edition="community")
async def test_external_community_is_refused_for_its_mechanism_not_a_missing_service(
    infrahub_k8s_external, external_topology, backup_binary, tmp_path
):
    """Diagnostic row 1: the refusal names the storage-access limitation, not "service not found".

    Proves FR-008. This is the row with a customer behind it: the pre-feature message named
    a missing service, and the customer concluded their deployment was broken and went
    looking for a container that was never meant to be there.

    It is the one case in this file that needs `community` rather than `enterprise`, so it
    skips on every topology the round-trip cases run against and vice versa — which is the
    point of pinning the edition rather than inferring it.
    """
    namespace = infrahub_k8s_external["namespace"]
    kubeconfig = infrahub_k8s_external["kubeconfig_path"]
    backup_dir = tmp_path / "backups"
    env = {"KUBECONFIG": kubeconfig, **external_db_env(external_topology)}

    result = run_cli(
        backup_binary,
        ["--k8s-namespace", namespace, "--backup-dir", str(backup_dir), "create", "--force"],
        env=env,
    )
    combined = result.stdout + result.stderr

    assert result.returncode != 0, f"an external Community capture must be refused:\n{combined}"

    # Condition: why the mechanism cannot serve this database.
    assert "Community" in combined and "storage" in combined, (
        f"the refusal does not name the storage-access limitation of the Community mechanism:\n{combined}"
    )
    # Resource: which database, at which endpoint.
    for host in external_topology["neo4j_address"].split(","):
        hostname = host.strip().rsplit(":", 1)[0]
        assert hostname in combined, f"the refusal does not name the database it refused:\n{combined}"
    # Operator action: the edition that does support it.
    assert "Enterprise" in combined, f"the refusal does not name what would support it:\n{combined}"
    # And explicitly not the reading that sent a customer the wrong way.
    assert not re.search(r"service not found|no such service", combined, re.IGNORECASE), (
        f"the refusal still reads as a missing service, which is the defect FR-008 exists to fix:\n{combined}"
    )

    assert _archives(backup_dir) == [], f"a refused capture must retain nothing:\n{combined}"
    left_behind = await transient_objects(kubeconfig, namespace)
    assert left_behind == [], f"transient objects survived the refusal: {left_behind}"


@pytest.mark.e2e
@pytest.mark.k8s
@requires_external_db(SERVICE_NEO4J, edition="enterprise")
async def test_an_undiscoverable_endpoint_names_where_discovery_looked(
    infrahub_k8s_external, external_topology, backup_binary, tmp_path
):
    """Diagnostic row 2: discovery was attempted, where it looked, and the override flag.

    Proves FR-003 and FR-004. Provoked by scaling `infrahub-server` to zero, which is where
    the tool reads INFRAHUB_DB_ADDRESS from: with no pod to exec into, discovery has nothing
    to read and the operator has supplied no override, which is precisely the deployment
    FR-004 exists for — one whose configuration does not expose the endpoint.

    It runs last in this file and restores the scale on the way out, because the deployment
    is shared with every case above it.
    """
    namespace = infrahub_k8s_external["namespace"]
    kubeconfig = infrahub_k8s_external["kubeconfig_path"]
    backup_dir = tmp_path / "backups"
    # Deliberately *without* external_db_env: the credentials it pins are irrelevant here,
    # and this case is about the run having no address from any channel at all.
    env = {"KUBECONFIG": kubeconfig}

    api = await kr8s.asyncio.api(kubeconfig=kubeconfig)
    server = await AsyncDeployment.get("infrahub-infrahub-server", namespace=namespace, api=api)
    prior = server.raw["spec"].get("replicas", 1)

    try:
        await server.scale(0)

        result = run_cli(
            backup_binary,
            ["--k8s-namespace", namespace, "--backup-dir", str(backup_dir), "create", "--force"],
            env=env,
        )
        combined = result.stdout + result.stderr

        assert result.returncode != 0, f"a run with no discoverable endpoint must fail:\n{combined}"

        # Condition: the database is outside the deployment and discovery came back empty —
        # not that the cluster could not be queried, which is a different row with a
        # different remedy.
        assert "does not run in this deployment" in combined, (
            f"the failure does not state the condition it hit:\n{combined}"
        )
        # Resource: the settings that were read, and the component they were read out of.
        assert "INFRAHUB_DB_ADDRESS" in combined, (
            f"the failure does not say where discovery looked, so an operator who *has* set it "
            f"cannot tell an unset setting from a component this run could not read:\n{combined}"
        )
        assert "infrahub-server" in combined, f"the failure does not name the component discovery read:\n{combined}"
        # Operator action: the override, in both the channels it has.
        assert "--neo4j-address" in combined, f"the failure does not name the override flag:\n{combined}"
        assert "INFRAHUB_NEO4J_ADDRESS" in combined, (
            f"the failure does not name the configuration key an unattended run needs:\n{combined}"
        )

        assert _archives(backup_dir) == [], f"a run that never resolved an endpoint must retain nothing:\n{combined}"

        # The positive control, and the half that makes the assertion above mean something:
        # supplying the override is what the message says to do, so it must get the run past
        # this condition. Without it, a message naming a flag that changes nothing would pass.
        overridden = run_cli(
            backup_binary,
            [
                "--k8s-namespace",
                namespace,
                "--backup-dir",
                str(backup_dir),
                "--neo4j-address",
                external_topology["neo4j_address"],
                "create",
                "--force",
            ],
            env={"KUBECONFIG": kubeconfig, **external_db_env(external_topology)},
        )
        past = overridden.stdout + overridden.stderr
        assert "--neo4j-address" not in past or "INFRAHUB_DB_ADDRESS" not in past, (
            f"the override did not get the run past the undiscoverable-endpoint refusal, so the "
            f"message names a flag that does not change the outcome:\n{past}"
        )
        assert RESOLVED_ENDPOINT_LINE in past, f"the run with the override never reported a resolved endpoint:\n{past}"
    finally:
        await server.scale(prior)
        async with portforward_infrahub(kubeconfig, namespace):
            pass  # wait for the deployment to be serving again before the next case


@pytest.mark.e2e
@pytest.mark.k8s
@requires_external_db(SERVICE_NEO4J, edition="enterprise")
async def test_a_revoked_pod_listing_fails_rather_than_switching_to_external_mode(
    infrahub_k8s_external, external_topology, backup_binary, tmp_path
):
    """Diagnostic row 3: a failure, not a switch to external mode.

    Proves FR-002, and it is the most dangerous of the six to get wrong in the other
    direction: a deployment whose databases are *internal* and whose pod listing is refused
    would, on a tool that read the refusal as an empty match, be taken off the path it works
    on today and sent looking for a database on the network.

    Run as an identity holding no verb on pods at all, so the refusal is the cluster's own
    rather than anything this test asserted into being.
    """
    namespace = infrahub_k8s_external["namespace"]
    kubeconfig = infrahub_k8s_external["kubeconfig_path"]
    backup_dir = tmp_path / "backups"

    async with restricted_kubeconfig(kubeconfig, namespace, rules=[], name="xdb-nolist") as restricted:
        result = run_cli(
            backup_binary,
            ["--k8s-namespace", namespace, "--backup-dir", str(backup_dir), "create", "--force"],
            env={"KUBECONFIG": restricted, **external_db_env(external_topology)},
        )
        combined = result.stdout + result.stderr

    assert result.returncode != 0, f"a deployment query that was refused must fail the run:\n{combined}"

    # Condition: the query failed. Resource: which service, in which namespace.
    assert re.search(r"forbidden|could not be queried|failed to determine", combined, re.IGNORECASE), (
        f"the failure does not say that the query failed:\n{combined}"
    )
    assert namespace in combined, f"the failure does not name the namespace it queried:\n{combined}"
    # Operator action: the permission, or the namespace.
    assert re.search(r"pods|--k8s-namespace", combined), (
        f"the failure names neither the permission nor the namespace flag:\n{combined}"
    )
    # And explicitly not the reading FR-002 forbids. A run that switched to external mode
    # would have gone on to build a transient workload.
    assert TRANSIENT_WORKLOAD_LINE not in combined, (
        f"a refused pod listing was read as evidence the database is external, and the run went "
        f"on to build a workload for it (FR-002):\n{combined}"
    )
    assert _archives(backup_dir) == [], f"a run that never established a location must retain nothing:\n{combined}"


@pytest.mark.e2e
@pytest.mark.k8s
@requires_external_db(SERVICE_NEO4J, edition="enterprise")
async def test_absent_pod_creation_permission_names_the_verbs_and_resource(
    infrahub_k8s_external, external_topology, backup_binary, tmp_path
):
    """Diagnostic row 4: the failure names the missing verbs and resource.

    Proves FR-019. Run as an identity that *can* list pods — so the location is established
    positively and the run genuinely reaches the point of creating a workload — but cannot
    create one. That split is what separates this row from row 3: the same cluster, one verb
    apart, two different messages, and a tool that collapsed them would send an operator to
    fix the wrong thing.
    """
    namespace = infrahub_k8s_external["namespace"]
    kubeconfig = infrahub_k8s_external["kubeconfig_path"]
    backup_dir = tmp_path / "backups"

    async with restricted_kubeconfig(kubeconfig, namespace, rules=[POD_READ_RULE], name="xdb-nocreate") as restricted:
        result = run_cli(
            backup_binary,
            ["--k8s-namespace", namespace, "--backup-dir", str(backup_dir), "create", "--force"],
            env={"KUBECONFIG": restricted, **external_db_env(external_topology)},
        )
        combined = result.stdout + result.stderr

    assert result.returncode != 0, f"a run that cannot create its workload must fail:\n{combined}"

    # Condition: the API server refused the workload — not that the endpoint was
    # undiscoverable, which this identity could and did resolve.
    assert re.search(r"forbidden|refused", combined, re.IGNORECASE), (
        f"the failure does not say the request was refused:\n{combined}"
    )
    assert "INFRAHUB_DB_ADDRESS" not in combined, (
        f"a missing permission was reported as an undiscoverable endpoint, which is the exact "
        f"conflation FR-019 and FR-004 are separate requirements to prevent:\n{combined}"
    )
    # Resource and verbs, which is what this row is entirely about.
    assert re.search(r"\bcreate\b", combined), f"the failure does not name the verb required:\n{combined}"
    assert re.search(r"\bpods?\b", combined), f"the failure does not name the resource required:\n{combined}"
    assert re.search(r"secrets?", combined), (
        f"the failure names pods but not secrets, so an operator granting only what it named "
        f"gets one step further and reads a second refusal:\n{combined}"
    )
    assert namespace in combined, f"the failure does not name the namespace the Role belongs in:\n{combined}"
    # Operator action: FR-019 forbids a workaround, so the remedy is the grant or User Story 3.
    assert re.search(r"grant|Role", combined), f"the failure does not name the remedy:\n{combined}"

    assert _archives(backup_dir) == [], f"a run that could not create its workload must retain nothing:\n{combined}"
