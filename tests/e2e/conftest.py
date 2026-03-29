import os
import subprocess
import time
from contextlib import asynccontextmanager
from pathlib import Path
from typing import AsyncGenerator

import boto3
import httpx
import kr8s.asyncio
import pytest
from kr8s.asyncio.objects import Service as AsyncService
from testcontainers.core.container import DockerContainer

from tests.conftest import _dump_namespace_logs
from tests.helpers.utils import wait_for_http

PROJECT_ROOT = Path(__file__).parent.resolve().parents[1]
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
# Fixture: infrahub_k8s
# ---------------------------------------------------------------------------
@pytest.fixture(scope="session")
async def infrahub_k8s(
    vcluster: dict,
) -> AsyncGenerator[dict, None]:
    """Deploy Infrahub via Helm into vcluster."""
    kubeconfig_path = vcluster["kubeconfig_path"]
    namespace = "infrahub"

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
            "oci://registry.opsmill.io/opsmill/chart/infrahub",
            "-f",
            str(FIXTURES_DIR / "helm" / "infrahub-values.yaml"),
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
