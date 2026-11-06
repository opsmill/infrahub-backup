"""E2E tests: Kubernetes with a three-member Neo4j cluster outside the Infrahub namespace.

The cluster runs in a namespace of its own, so the Infrahub namespace has no `database`
workload and the tool takes the external path; see the "Neo4j cluster topologies" block
in `tests/e2e/conftest.py`. The single-server external suites cannot show what this shows:

- the capture runs through the cluster's headless service, the address Infrahub uses;
- the restore seeds the database on every member from the object store, and reports
  success only once the database is online on all of them.

The cases skip unless the session sets INFRAHUB_TESTING_NEO4J_CLUSTER=external and
declares the external topology (INFRAHUB_TESTING_EXTERNAL_DB=database and the variables
that go with it).
"""

import pytest

from tests.e2e.conftest import (
    NEO4J_CLUSTER_SIZE,
    SERVICE_NEO4J,
    external_db_env,
    external_db_image_args,
    portforward_infrahub,
    requires_external_db,
    requires_neo4j_cluster,
    transient_objects_after_run,
)
from tests.e2e.test_k8s_neo4j_cluster import neo4j_database_status
from tests.helpers.utils import (
    find_latest_backup,
    modify_infrahub_data,
    read_backup_metadata,
    run_backup,
    run_restore,
    seed_infrahub_data,
    verify_infrahub_data,
)


def _seed_store_args(seed_store: dict) -> list[str]:
    """The `--s3-*` arguments naming the store a restore stages its seed into."""
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
    return {"AWS_ACCESS_KEY_ID": seed_store["access_key"], "AWS_SECRET_ACCESS_KEY": seed_store["secret_key"]}


@pytest.mark.e2e
@pytest.mark.k8s
@requires_neo4j_cluster("external")
@requires_external_db(SERVICE_NEO4J, edition="enterprise", seed_store=True)
async def test_capture_through_the_cluster_address_is_recorded(
    infrahub_k8s_external_neo4j_cluster, external_topology, backup_binary, tmp_path
):
    """The backup goes through the address Infrahub uses, and the metadata records it.

    Infrahub reaches the cluster through its headless service, as in the HA example, so
    that service is the one endpoint the run is given. The capture runs against it on the
    backup port, where the service resolves to a member pod. `source_roles` maps the
    endpoints the run was given to the role observed for each; the service name is not the
    address of any member, so its role is recorded as `unknown`, which is the documented
    value for an endpoint the run looked for and could not match.
    """
    deployment = infrahub_k8s_external_neo4j_cluster
    namespace = deployment["namespace"]
    kubeconfig = deployment["kubeconfig_path"]
    backup_dir = tmp_path / "backups"
    env = {"KUBECONFIG": kubeconfig, **external_db_env(external_topology)}
    host = external_topology["neo4j_address"].rsplit(":", 1)[0]
    capture_endpoint = f"{host}:6362"

    async with portforward_infrahub(kubeconfig, namespace) as url:
        await seed_infrahub_data(url, deployment["token"])

    run_backup(
        backup_binary,
        [
            "--k8s-namespace",
            namespace,
            *external_db_image_args(deployment),
            "--backup-dir",
            str(backup_dir),
            "create",
            "--force",
        ],
        env=env,
    )
    metadata = read_backup_metadata(find_latest_backup(backup_dir))

    assert metadata.get("external_location", {}).get(SERVICE_NEO4J) is True, (
        f"the cluster was not recorded as external: {metadata.get('external_location')}"
    )
    assert metadata.get("source_endpoints", {}).get(SERVICE_NEO4J) == [capture_endpoint], (
        f"expected the capture to run against {capture_endpoint}: {metadata.get('source_endpoints')}"
    )
    assert metadata.get("source_roles", {}).get(SERVICE_NEO4J) == {capture_endpoint: "unknown"}, (
        f"expected the service address recorded with an unknown role: {metadata.get('source_roles')}"
    )
    assert metadata.get("capture_complete") is not False, "a retained artifact must not be an incomplete capture"

    left_behind = await transient_objects_after_run(kubeconfig, namespace)
    assert left_behind == [], f"transient objects survived the run: {left_behind}"


@pytest.mark.e2e
@pytest.mark.k8s
@requires_neo4j_cluster("external")
@requires_external_db(SERVICE_NEO4J, edition="enterprise", seed_store=True)
async def test_full_round_trip_through_the_external_cluster(
    infrahub_k8s_external_neo4j_cluster, external_topology, external_seed_store, backup_binary, tmp_path
):
    """Capture, change the data, restore, and read the pre-backup data back from the cluster.

    The restore seeds every member from the object store, so success means the database is
    online on all of them, not only on the member the tool talked to.
    """
    deployment = infrahub_k8s_external_neo4j_cluster
    namespace = deployment["namespace"]
    kubeconfig = deployment["kubeconfig_path"]
    token = deployment["token"]
    image_args = external_db_image_args(deployment)
    backup_dir = tmp_path / "backups"
    env = {"KUBECONFIG": kubeconfig, **external_db_env(external_topology), **_seed_store_env(external_seed_store)}

    async with portforward_infrahub(kubeconfig, namespace) as url:
        seed = await seed_infrahub_data(url, token)

    run_backup(
        backup_binary,
        ["--k8s-namespace", namespace, *image_args, "--backup-dir", str(backup_dir), "create", "--force"],
        env=env,
    )
    archive = find_latest_backup(backup_dir)

    async with portforward_infrahub(kubeconfig, namespace) as url:
        await modify_infrahub_data(url, token, seed)

    run_restore(
        backup_binary,
        [
            "--k8s-namespace",
            namespace,
            *image_args,
            *_seed_store_args(external_seed_store),
            "restore",
            "--allow-external-restore",
            "--force",
            str(archive),
        ],
        env=env,
    )

    statuses = neo4j_database_status(kubeconfig, deployment["neo4j_namespace"])
    assert len(statuses) == NEO4J_CLUSTER_SIZE and all(status == "online" for status in statuses), (
        f"the restored database is not online on every member: {statuses}"
    )

    async with portforward_infrahub(kubeconfig, namespace) as url:
        await verify_infrahub_data(url, token, seed)

    left_behind = await transient_objects_after_run(kubeconfig, namespace)
    assert left_behind == [], f"transient objects survived the restore: {left_behind}"
