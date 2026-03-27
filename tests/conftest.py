import os
import subprocess
import uuid
from pathlib import Path
from typing import AsyncGenerator

import pytest
from kubernetes_asyncio import client as kubeclient
from kubernetes_asyncio import config as kubeconfig
from pytest_asyncio import is_async_test


def pytest_collection_modifyitems(items):
    """Force all async tests to use session-scoped event loop."""
    pytest_asyncio_tests = (item for item in items if is_async_test(item))
    session_scope_marker = pytest.mark.asyncio(loop_scope="session")
    for async_test in pytest_asyncio_tests:
        async_test.add_marker(session_scope_marker, append=False)


@pytest.fixture(scope="session")
async def vcluster(
    request: pytest.FixtureRequest,
    tmp_path_factory,
) -> AsyncGenerator[dict, None]:
    """Create a vCluster and yield connection details."""
    kubeconfig_path = str(tmp_path_factory.mktemp("vcluster") / "kubeconfig")
    cluster_name = f"pytest-{uuid.uuid4()}"

    vcluster_dir = os.environ.get("VCLUSTER_DIR")

    def teardown():
        delete_cmd = ["vcluster", "delete", cluster_name, "--driver=docker"]
        if vcluster_dir:
            delete_cmd.extend(["--config", vcluster_dir])
        subprocess.run(delete_cmd, check=True)

    request.addfinalizer(teardown)

    create_cmd = [
        "vcluster",
        "create",
        cluster_name,
        "--connect=false",
        "--driver=docker",
    ]
    if vcluster_dir:
        create_cmd.extend(["--config", vcluster_dir])
    subprocess.run(create_cmd, check=True)

    # Export kubeconfig
    connect_cmd = ["vcluster", "connect", cluster_name, "--print", "--driver=docker"]
    if vcluster_dir:
        connect_cmd.extend(["--config", vcluster_dir])
    result = subprocess.run(connect_cmd, capture_output=True, text=True, check=True)
    Path(kubeconfig_path).write_text(result.stdout)

    await kubeconfig.load_kube_config(config_file=kubeconfig_path)
    async with kubeclient.ApiClient() as api:
        yield {
            "api": api,
            "cluster_name": cluster_name,
            "kubeconfig_path": kubeconfig_path,
        }


# ---------------------------------------------------------------------------
# Hooks for log dumping on test failure
# ---------------------------------------------------------------------------
@pytest.hookimpl(tryfirst=True, hookwrapper=True)
def pytest_runtest_makereport(item, call):
    outcome = yield
    report = outcome.get_result()
    setattr(item, f"rep_{report.when}", report)


def _dump_docker_compose_logs(project: str) -> None:
    """Print docker compose logs for a project."""
    print(f"\n{'=' * 60}")
    print(f"Docker Compose logs for project '{project}'")
    print(f"{'=' * 60}")
    result = subprocess.run(
        ["docker", "compose", "-p", project, "logs", "--tail=200"],
        capture_output=True,
        text=True,
    )
    print(result.stdout or "(no output)")
    if result.stderr:
        print(f"(stderr) {result.stderr}")


def _dump_namespace_logs(kubeconfig: str, namespace: str) -> None:
    """Print pod statuses and container logs for a Kubernetes namespace."""
    result = subprocess.run(
        ["kubectl", "--kubeconfig", kubeconfig, "get", "pods", "-n", namespace, "-o", "wide"],
        capture_output=True,
        text=True,
    )
    print(f"\n{'=' * 60}")
    print(f"Pods in namespace '{namespace}'")
    print(f"{'=' * 60}")
    print(result.stdout or "(no output)")

    for resource in ["services", "statefulsets", "deployments"]:
        res_result = subprocess.run(
            ["kubectl", "--kubeconfig", kubeconfig, "get", resource, "-n", namespace, "-o", "wide"],
            capture_output=True,
            text=True,
        )
        print(f"\n{'=' * 60}")
        print(f"{resource} in namespace '{namespace}'")
        print(f"{'=' * 60}")
        print(res_result.stdout or "(no output)")

    # Get pod names and dump logs
    pod_result = subprocess.run(
        ["kubectl", "--kubeconfig", kubeconfig, "get", "pods", "-n", namespace, "-o", "name"],
        capture_output=True,
        text=True,
    )
    pods = [line.strip() for line in pod_result.stdout.strip().splitlines() if line.strip()]

    for pod in pods:
        print(f"\n{'=' * 60}")
        print(f"Logs: {namespace}/{pod}")
        print(f"{'=' * 60}")
        log_result = subprocess.run(
            [
                "kubectl", "--kubeconfig", kubeconfig,
                "logs", pod, "-n", namespace,
                "--all-containers", "--tail=200",
            ],
            capture_output=True,
            text=True,
        )
        print(log_result.stdout or "(no logs)")
        if log_result.stderr:
            print(f"(stderr) {log_result.stderr}")
