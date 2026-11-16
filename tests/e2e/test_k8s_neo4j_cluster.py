"""E2E tests: Kubernetes with a three-member Neo4j cluster in the Infrahub namespace.

The cluster is built the way Infrahub's HA example builds it; see the "Neo4j cluster
topologies" block in `tests/e2e/conftest.py`. Every other Kubernetes case runs against a
single Neo4j server, so this is the only place the cluster restore flow
(`restoreNeo4jCluster` in src/internal/app/backup_neo4j.go) runs against a real cluster:
it restores on one server and seeds the database on the others from it.

The cluster runs on a vcluster with three nodes, one member on each.

The cases skip unless the session sets INFRAHUB_TESTING_NEO4J_CLUSTER=internal.
"""

import pytest

from tests.e2e.conftest import (
    NEO4J_CLUSTER_SIZE,
    assert_members_hold_the_same_graph,
    neo4j_database_status,
    portforward_infrahub,
    requires_neo4j_cluster,
)
from tests.helpers.utils import (
    find_latest_backup,
    modify_infrahub_data,
    run_backup,
    run_restore,
    seed_infrahub_data,
    verify_infrahub_data,
)

# From restoreNeo4jCluster in src/internal/app/backup_neo4j.go.
CLUSTER_RESTORE_LINE = "Using Neo4j cluster restore flow"
CLUSTER_RESTORE_DONE_LINE = "Neo4j cluster restore completed successfully"


@pytest.mark.e2e
@pytest.mark.k8s
@requires_neo4j_cluster("internal")
async def test_backup_restore_round_trip_on_a_neo4j_cluster(infrahub_k8s_neo4j_cluster, backup_binary, tmp_path):
    """Back up, change the data, restore, and read the pre-backup data back from the cluster.

    The restore must take the cluster flow, report success only once the database is
    online on every member, and leave all three members serving the same graph.
    """
    namespace = infrahub_k8s_neo4j_cluster["namespace"]
    kubeconfig = infrahub_k8s_neo4j_cluster["kubeconfig_path"]
    token = infrahub_k8s_neo4j_cluster["token"]
    backup_dir = str(tmp_path / "backups")
    env = {"KUBECONFIG": kubeconfig}

    async with portforward_infrahub(kubeconfig, namespace) as url:
        seed = await seed_infrahub_data(url, token)

    run_backup(
        backup_binary,
        ["--k8s-namespace", namespace, "--backup-dir", backup_dir, "create", "--force"],
        env=env,
    )
    backup_file = find_latest_backup(backup_dir)

    async with portforward_infrahub(kubeconfig, namespace) as url:
        await modify_infrahub_data(url, token, seed)

    result = run_restore(
        backup_binary,
        ["--k8s-namespace", namespace, "restore", str(backup_file)],
        env=env,
    )
    log = result.stdout + result.stderr

    assert CLUSTER_RESTORE_LINE in log, f"the restore did not take the cluster flow:\n{log}"
    assert CLUSTER_RESTORE_DONE_LINE in log, f"the cluster restore did not report completion:\n{log}"

    statuses = neo4j_database_status(kubeconfig, namespace)
    assert len(statuses) == NEO4J_CLUSTER_SIZE and all(status == "online" for status in statuses), (
        f"the restored database is not online on every member: {statuses}"
    )
    assert_members_hold_the_same_graph(kubeconfig, namespace)

    async with portforward_infrahub(kubeconfig, namespace) as url:
        await verify_infrahub_data(url, token, seed)
