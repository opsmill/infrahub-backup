"""E2E tests: Kubernetes with a three-member Neo4j cluster outside the Infrahub namespace.

The cluster runs in a namespace of its own, so the Infrahub namespace has no `database`
workload and the tool takes the external path; see the "Neo4j cluster topologies" block
in `tests/e2e/conftest.py`. The single-server external suites cannot show what this shows:

- the capture runs through the cluster's headless service, the address Infrahub uses;
- the restore seeds the database on every member from the object store, and reports
  success only once the database is online on all of them;
- a run inside the Infrahub namespace, the way the infrahub-backup Helm chart runs the
  tool, captures and restores the cluster with the tools of its own image: it needs no
  `--external-db-image` and creates no transient Job (direct mode, ADR 0017).

The cluster runs on a vcluster with three nodes, one member on each. The fixtures build
the whole topology, including the object store the restore seeds from and the
infrahub-backup image the external path runs its tools from.

The cases skip unless the session sets INFRAHUB_TESTING_NEO4J_CLUSTER=external.
"""

import subprocess
from collections.abc import Iterator
from contextlib import contextmanager

import pytest
import yaml

from tests.e2e.conftest import (
    DIRECT_MODE_LINE,
    DIRECT_MODE_RULES,
    JOB_PATH_LINE,
    NEO4J_CLUSTER_SIZE,
    SERVICE_NEO4J,
    TRANSIENT_WORKLOAD_LINE,
    assert_members_hold_the_same_graph,
    external_db_env,
    external_db_image_args,
    neo4j_database_status,
    portforward_infrahub,
    requires_neo4j_cluster,
    transient_objects_after_run,
)
from tests.helpers.utils import (
    find_latest_backup,
    modify_infrahub_data,
    read_backup_metadata,
    run_backup,
    run_restore,
    seed_infrahub_data,
    verify_infrahub_data,
)

# The pod a direct-mode run executes in, its service account and its Role share this name.
# It contains no deployment service name as a substring, so the tool's name-based fallbacks
# cannot take this pod for a replica of a service it backs up or stops.
RUNNER_NAME = "ibk-cluster-runner"
RUNNER_BACKUP_DIR = "/tmp/backups"
# The pod's temporary directory, where a direct-mode run writes the capture and stages the
# restore. It is an emptyDir so the free space does not depend on the image's file system.
RUNNER_SCRATCH_LIMIT = "6Gi"
RUNNER_READY_TIMEOUT = "300s"


def _seed_store_args(seed_store: dict, endpoint: str | None = None) -> list[str]:
    """The `--s3-*` arguments naming the store a restore stages its seed into.

    `endpoint` replaces the store's host endpoint for a run inside the vcluster.
    """
    args = [
        "--s3-bucket",
        seed_store["bucket"],
        "--s3-endpoint",
        endpoint or seed_store["endpoint"],
        "--s3-region",
        seed_store["region"],
    ]
    if seed_store["prefix"]:
        args += ["--s3-prefix", seed_store["prefix"]]
    return args


def _seed_store_env(seed_store: dict) -> dict[str, str]:
    return {"AWS_ACCESS_KEY_ID": seed_store["access_key"], "AWS_SECRET_ACCESS_KEY": seed_store["secret_key"]}


def _runner_manifests(image: str, env: dict[str, str]) -> str:
    """A pod running the infrahub-backup image idle, under a service account that holds only
    the direct-mode permissions.

    The service account holds nothing on jobs or secrets, so a run that took the transient
    Job path would be refused by the API server as well as caught by the log assertions.
    """
    return yaml.dump_all(
        [
            {"apiVersion": "v1", "kind": "ServiceAccount", "metadata": {"name": RUNNER_NAME}},
            {
                "apiVersion": "rbac.authorization.k8s.io/v1",
                "kind": "Role",
                "metadata": {"name": RUNNER_NAME},
                "rules": DIRECT_MODE_RULES,
            },
            {
                "apiVersion": "rbac.authorization.k8s.io/v1",
                "kind": "RoleBinding",
                "metadata": {"name": RUNNER_NAME},
                "roleRef": {"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": RUNNER_NAME},
                "subjects": [{"kind": "ServiceAccount", "name": RUNNER_NAME}],
            },
            {
                "apiVersion": "v1",
                "kind": "Pod",
                "metadata": {"name": RUNNER_NAME},
                "spec": {
                    "serviceAccountName": RUNNER_NAME,
                    "restartPolicy": "Never",
                    "containers": [
                        {
                            "name": "backup",
                            "image": image,
                            # Loaded into the nodes, never pulled: see load_image_into_vcluster.
                            "imagePullPolicy": "IfNotPresent",
                            "command": ["sleep", "infinity"],
                            "env": [{"name": key, "value": value} for key, value in env.items()],
                            "volumeMounts": [{"name": "tmp", "mountPath": "/tmp"}],
                        }
                    ],
                    "volumes": [{"name": "tmp", "emptyDir": {"sizeLimit": RUNNER_SCRATCH_LIMIT}}],
                },
            },
        ]
    )


@contextmanager
def _runner_pod(kubeconfig: str, namespace: str, image: str, env: dict[str, str]) -> Iterator[list[str]]:
    """Start the runner pod, and yield the command prefix that runs a command inside it."""
    kubectl = ["kubectl", "--kubeconfig", kubeconfig, "-n", namespace]
    manifests = _runner_manifests(image, env)
    subprocess.run([*kubectl, "apply", "-f", "-"], input=manifests, text=True, capture_output=True, check=True)
    try:
        subprocess.run(
            [*kubectl, "wait", f"pod/{RUNNER_NAME}", "--for=condition=Ready", f"--timeout={RUNNER_READY_TIMEOUT}"],
            capture_output=True,
            text=True,
            check=True,
        )
        yield [*kubectl, "exec", RUNNER_NAME, "--"]
    finally:
        subprocess.run(
            [*kubectl, "delete", "-f", "-", "--ignore-not-found", "--wait=false"],
            input=manifests,
            text=True,
            capture_output=True,
            check=False,
        )


def _assert_direct_mode(log: str, step: str) -> None:
    assert DIRECT_MODE_LINE in log, f"the {step} did not report taking direct mode:\n{log}"
    assert JOB_PATH_LINE not in log, f"the {step} took the transient Job path:\n{log}"
    assert TRANSIENT_WORKLOAD_LINE not in log, f"the {step} created a transient workload:\n{log}"


@pytest.mark.e2e
@pytest.mark.k8s
@requires_neo4j_cluster("external")
async def test_capture_through_the_cluster_address_is_recorded(
    infrahub_k8s_external_neo4j_cluster, backup_binary, tmp_path
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
    env = {"KUBECONFIG": kubeconfig, **external_db_env(deployment["topology"])}
    host = deployment["topology"]["neo4j_address"].rsplit(":", 1)[0]
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
async def test_full_round_trip_through_the_external_cluster(
    infrahub_k8s_external_neo4j_cluster, backup_binary, tmp_path
):
    """Capture, change the data, restore, and read the pre-backup data back from the cluster.

    The restore seeds every member from the object store, so success means the database is
    online on all of them, not only on the member the tool talked to, and that every
    member holds the same graph.
    """
    deployment = infrahub_k8s_external_neo4j_cluster
    namespace = deployment["namespace"]
    kubeconfig = deployment["kubeconfig_path"]
    token = deployment["token"]
    seed_store = deployment["seed_store"]
    image_args = external_db_image_args(deployment)
    backup_dir = tmp_path / "backups"
    env = {"KUBECONFIG": kubeconfig, **external_db_env(deployment["topology"]), **_seed_store_env(seed_store)}

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
            *_seed_store_args(seed_store),
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
    assert_members_hold_the_same_graph(kubeconfig, deployment["neo4j_namespace"])

    async with portforward_infrahub(kubeconfig, namespace) as url:
        await verify_infrahub_data(url, token, seed)

    left_behind = await transient_objects_after_run(kubeconfig, namespace)
    assert left_behind == [], f"transient objects survived the restore: {left_behind}"


@pytest.mark.e2e
@pytest.mark.k8s
@requires_neo4j_cluster("external")
async def test_round_trip_from_inside_the_namespace_needs_no_external_db_image(infrahub_k8s_external_neo4j_cluster):
    """FR-032: a run inside the namespace captures and restores the cluster in its own process.

    This is how the infrahub-backup Helm chart runs the tool. The run gets no
    `--external-db-image`, finds `neo4j-admin` and `cypher-shell` on the PATH of its own
    image, and creates no Job, pod or Secret. Its service account holds only the
    direct-mode permissions. The restore reports success only when the database is online
    on every member and every member holds the same graph.
    """
    deployment = infrahub_k8s_external_neo4j_cluster
    namespace = deployment["namespace"]
    kubeconfig = deployment["kubeconfig_path"]
    token = deployment["token"]
    seed_store = deployment["seed_store"]
    env = {**external_db_env(deployment["topology"]), **_seed_store_env(seed_store)}
    root_args = ["--k8s-namespace", namespace, *deployment["topology_args"]]

    async with portforward_infrahub(kubeconfig, namespace) as url:
        seed = await seed_infrahub_data(url, token)

    with _runner_pod(kubeconfig, namespace, deployment["external_db_image"], env) as exec_in_runner:
        backup = run_backup(
            exec_in_runner[0],
            [
                *exec_in_runner[1:],
                "infrahub-backup",
                *root_args,
                "--backup-dir",
                RUNNER_BACKUP_DIR,
                "create",
                "--force",
            ],
        )
        _assert_direct_mode(backup.stdout + backup.stderr, "backup")

        listing = subprocess.run(
            [*exec_in_runner, "ls", "-1", RUNNER_BACKUP_DIR], capture_output=True, text=True, check=True
        ).stdout
        archives = [name for name in listing.split() if name.startswith("infrahub_backup_")]
        assert len(archives) == 1, f"expected one artifact in {RUNNER_BACKUP_DIR}, got {archives}"

        async with portforward_infrahub(kubeconfig, namespace) as url:
            await modify_infrahub_data(url, token, seed)

        restore = run_restore(
            exec_in_runner[0],
            [
                *exec_in_runner[1:],
                "infrahub-backup",
                *root_args,
                # The run is inside the vcluster, so it uploads the seed through the
                # address the Neo4j servers fetch it from.
                *_seed_store_args(seed_store, endpoint=seed_store["server_endpoint"]),
                "restore",
                "--allow-external-restore",
                "--force",
                f"{RUNNER_BACKUP_DIR}/{archives[0]}",
            ],
        )
        _assert_direct_mode(restore.stdout + restore.stderr, "restore")

    statuses = neo4j_database_status(kubeconfig, deployment["neo4j_namespace"])
    assert len(statuses) == NEO4J_CLUSTER_SIZE and all(status == "online" for status in statuses), (
        f"the restored database is not online on every member: {statuses}"
    )
    assert_members_hold_the_same_graph(kubeconfig, deployment["neo4j_namespace"])

    async with portforward_infrahub(kubeconfig, namespace) as url:
        await verify_infrahub_data(url, token, seed)

    left_behind = await transient_objects_after_run(kubeconfig, namespace)
    assert left_behind == [], f"a direct-mode run left transient objects in the namespace: {left_behind}"
