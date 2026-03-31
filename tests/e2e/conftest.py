import os
import subprocess
import time
from contextlib import asynccontextmanager
from pathlib import Path
from typing import AsyncGenerator

import yaml

import boto3
import httpx
import kr8s.asyncio
import pytest
from kr8s.asyncio.objects import Pod as AsyncPod
from kr8s.asyncio.objects import Service as AsyncService
from testcontainers.core.container import DockerContainer

from tests.conftest import _dump_namespace_logs
from tests.helpers.utils import wait_for_http

PROJECT_ROOT = Path(__file__).parent.resolve().parents[1]


def _is_enterprise() -> bool:
    """Return True if the current test run targets Infrahub Enterprise."""
    if os.environ.get("INFRAHUB_TESTING_ENTERPRISE"):
        return True
    return "enterprise" in os.environ.get("INFRAHUB_HELM_CHART", "")


FIXTURES_DIR = Path(__file__).parent.resolve() / "fixtures"

INFRAHUB_ADMIN_TOKEN = "06438eb2-8019-4776-878c-0941b1f1d1ec"

# Reduce resource usage for e2e tests
os.environ.setdefault("INFRAHUB_TESTING_API_SERVER_COUNT", "1")
os.environ.setdefault("INFRAHUB_TESTING_TASK_WORKER_COUNT", "1")


# ---------------------------------------------------------------------------
# Log dumping on failure
# ---------------------------------------------------------------------------
@pytest.hookimpl(tryfirst=True, hookwrapper=True)
def pytest_runtest_makereport(item, call):
    outcome = yield
    report = outcome.get_result()
    setattr(item, f"rep_{report.when}", report)


@pytest.fixture(autouse=True)
def _dump_logs_on_failure(request):
    """Dump Kubernetes logs when a test fails.

    Docker Compose log dumping is handled by TestInfrahubDocker base class.
    """
    yield
    rep_call = getattr(request.node, "rep_call", None)
    if rep_call is None or not rep_call.failed:
        return

    vcluster = (
        request.getfixturevalue("vcluster")
        if "vcluster" in request.fixturenames
        else None
    )
    infrahub_k8s = (
        request.getfixturevalue("infrahub_k8s")
        if "infrahub_k8s" in request.fixturenames
        else None
    )
    if vcluster and infrahub_k8s:
        _dump_namespace_logs(vcluster["kubeconfig_path"], infrahub_k8s["namespace"])


# ---------------------------------------------------------------------------
# Fixture: backup_binary
# ---------------------------------------------------------------------------
@pytest.fixture(scope="session")
def backup_binary() -> str:
    """Build infrahub-backup and return the binary path."""
    subprocess.run(["make", "build"], cwd=str(PROJECT_ROOT), check=True)
    binary = PROJECT_ROOT / "bin" / "infrahub-backup"
    assert binary.exists(), f"Binary not found at {binary}"
    return str(binary)


# ---------------------------------------------------------------------------
# Fixture: minio_docker (testcontainers)
# ---------------------------------------------------------------------------
@pytest.fixture(scope="session")
def minio_docker(request: pytest.FixtureRequest) -> dict:
    """Start a MinIO container using testcontainers for S3 testing."""
    container = (
        DockerContainer("minio/minio:RELEASE.2025-09-07T16-13-09Z")
        .with_exposed_ports(9000)
        .with_env("MINIO_ROOT_USER", "minioadmin")
        .with_env("MINIO_ROOT_PASSWORD", "minioadmin")
        .with_kwargs(entrypoint="sh")
        .with_command(
            "-c 'mkdir -p /data/backups && minio server /data --console-address :9001'"
        )
    )

    container.start()
    request.addfinalizer(container.stop)

    # Get the mapped host port
    host = container.get_container_host_ip()
    port = container.get_exposed_port(9000)
    endpoint = f"http://{host}:{port}"

    # Wait for MinIO to be ready
    import requests

    deadline = time.time() + 30
    while time.time() < deadline:
        try:
            resp = requests.get(f"{endpoint}/minio/health/ready", timeout=2)
            if resp.status_code == 200:
                break
        except Exception:
            pass
        time.sleep(1)
    else:
        raise TimeoutError(f"MinIO at {endpoint} did not become ready")

    return {
        "endpoint": endpoint,
        "access_key": "minioadmin",
        "secret_key": "minioadmin",
        "bucket": "backups",
    }


# ---------------------------------------------------------------------------
# Helper: portforward_infrahub — fresh port-forward for each use
# ---------------------------------------------------------------------------
@asynccontextmanager
async def portforward_infrahub(kubeconfig_path: str, namespace: str):
    """Open a fresh port-forward to infrahub-server and yield the local URL.

    Pods may be restarted by infrahub-backup during restore, which kills any
    long-lived port-forward.  Call this each time you need to talk to Infrahub.
    """
    kr8s_api = await kr8s.asyncio.api(kubeconfig=kubeconfig_path)
    service = await AsyncService.get(
        "infrahub-infrahub-server", namespace=namespace, api=kr8s_api
    )
    async with service.portforward(remote_port=8000, local_port="auto") as local_port:
        url = f"http://localhost:{local_port}"
        await wait_for_http(f"{url}/api/config", timeout=300.0, interval=5.0)
        yield url


# ---------------------------------------------------------------------------
# Helper: delete database pod to reset CrashLoopBackOff
# ---------------------------------------------------------------------------
async def _delete_and_wait_for_database_pod(
    kubeconfig_path: str, namespace: str, timeout: float = 300.0
) -> None:
    """Delete the database pod and wait for its replacement to be ready."""
    import asyncio

    api = await kr8s.asyncio.api(kubeconfig=kubeconfig_path)
    pods = [
        pod
        async for pod in AsyncPod.list(
            namespace=namespace,
            label_selector="infrahub/service=database",
            api=api,
        )
    ]
    old_uids = {pod.metadata.get("uid") for pod in pods}
    for pod in pods:
        await pod.async_delete()

    # Wait for a new ready pod (skip old pods that haven't terminated yet)
    start = asyncio.get_event_loop().time()
    while asyncio.get_event_loop().time() - start < timeout:
        async for pod in AsyncPod.list(
            namespace=namespace,
            label_selector="infrahub/service=database",
            api=api,
        ):
            if pod.metadata.get("uid") in old_uids:
                continue
            phase = pod.raw.get("status", {}).get("phase")
            containers = pod.raw.get("status", {}).get("containerStatuses", [])
            if phase == "Running" and all(c.get("ready") for c in containers):
                return
        await asyncio.sleep(5)
    raise TimeoutError(f"Database pod not ready after {timeout}s")


# ---------------------------------------------------------------------------
# Fixture: reset_database_pod
# ---------------------------------------------------------------------------
@pytest.fixture()
async def reset_database_pod(infrahub_k8s):
    """Delete the database pod after test to reset CrashLoopBackOff (community only)."""
    yield
    if _is_enterprise():
        return
    await _delete_and_wait_for_database_pod(
        infrahub_k8s["kubeconfig_path"],
        infrahub_k8s["namespace"],
    )


# ---------------------------------------------------------------------------
# Fixture: infrahub_k8s
# ---------------------------------------------------------------------------
@pytest.fixture(scope="session")
async def infrahub_k8s(
    vcluster: dict,
    tmp_path_factory,
) -> AsyncGenerator[dict, None]:
    """Deploy Infrahub via Helm into vcluster."""
    kubeconfig_path = vcluster["kubeconfig_path"]
    namespace = "infrahub"

    # Prepare Helm values — enterprise wraps community values under "infrahub" key
    values_path = FIXTURES_DIR / "helm" / "infrahub-values.yaml"
    if _is_enterprise():
        with open(values_path) as f:
            values = yaml.safe_load(f)
        global_values = values.pop("global", {})
        wrapped = {"infrahub": values, "global": global_values}
        values_path = tmp_path_factory.mktemp("helm") / "infrahub-values.yaml"
        with open(values_path, "w") as f:
            yaml.dump(wrapped, f)

    # Install Infrahub via Helm
    subprocess.run(
        [
            "helm",
            "upgrade",
            "--install",
            "infrahub",
            "--dependency-update",
            "--create-namespace",
            "-n",
            namespace,
            os.environ.get(
                "INFRAHUB_HELM_CHART",
                "oci://registry.opsmill.io/opsmill/chart/infrahub",
            ),
            "-f",
            str(values_path),
            "--kubeconfig",
            kubeconfig_path,
            "--wait",
            "--timeout",
            "10m",
        ],
        check=True,
    )

    # Verify Infrahub is healthy with an initial port-forward
    async with portforward_infrahub(kubeconfig_path, namespace):
        pass  # health check happens inside the context manager

    yield {
        "namespace": namespace,
        "kubeconfig_path": kubeconfig_path,
        "token": INFRAHUB_ADMIN_TOKEN,
    }
