import asyncio
import os
import subprocess
import time
import uuid
from pathlib import Path
from typing import AsyncGenerator

import boto3
import httpx
import kr8s.asyncio
import pytest
import yaml
from kr8s.asyncio.objects import Service as AsyncService
from kubernetes_asyncio import client as kubeclient
from testcontainers.core.container import DockerContainer

from tests.conftest import _dump_docker_compose_logs, _dump_namespace_logs
from tests.helpers.utils import wait_for_http

PROJECT_ROOT = Path(__file__).parent.resolve().parents[1]
FIXTURES_DIR = Path(__file__).parent.resolve() / "fixtures"
INFRAHUB_HELM_CHART = Path("/home/ubuntu/opsmill/git/infrahub-helm/charts/infrahub")

INFRAHUB_ADMIN_TOKEN = "06438eb2-8019-4776-878c-0941b1f1d1ec"


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
    """Dump relevant logs when a test fails."""
    yield
    rep_call = getattr(request.node, "rep_call", None)
    if rep_call is None or not rep_call.failed:
        return

    # Docker Compose logs
    infrahub_docker = (
        request.getfixturevalue("infrahub_docker")
        if "infrahub_docker" in request.fixturenames
        else None
    )
    if infrahub_docker:
        _dump_docker_compose_logs(infrahub_docker["project"])

    # Kubernetes logs
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
# Wait helpers
# ---------------------------------------------------------------------------
async def _wait_for_service_ready(
    core_api: kubeclient.CoreV1Api,
    *,
    name: str,
    namespace: str = "default",
    label_selector: str,
    pod_timeout: float = 120.0,
    endpoint_timeout: float = 60.0,
) -> None:
    """Wait for pods to be ready and service to have endpoints."""
    deadline = time.time() + pod_timeout
    while time.time() < deadline:
        pods = await core_api.list_namespaced_pod(
            namespace=namespace, label_selector=label_selector
        )
        if pods.items and all(
            c.ready
            for pod in pods.items
            if pod.status and pod.status.container_statuses
            for c in pod.status.container_statuses
        ):
            break
        await asyncio.sleep(2)
    else:
        msg = f"pods matching '{label_selector}' in {namespace} did not become ready"
        raise TimeoutError(msg)

    deadline = time.time() + endpoint_timeout
    while time.time() < deadline:
        endpoints = await core_api.read_namespaced_endpoints(
            name=name, namespace=namespace
        )
        if endpoints.subsets and any(subset.addresses for subset in endpoints.subsets):
            break
        await asyncio.sleep(2)
    else:
        msg = f"service {name} in {namespace} has no ready endpoints"
        raise TimeoutError(msg)


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
        .with_command("server /data")
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

    # Create the bucket
    s3 = boto3.client(
        "s3",
        endpoint_url=endpoint,
        aws_access_key_id="minioadmin",
        aws_secret_access_key="minioadmin",
        region_name="us-east-1",
    )
    s3.create_bucket(Bucket="infrahub-backups")

    return {
        "endpoint": endpoint,
        "access_key": "minioadmin",
        "secret_key": "minioadmin",
        "bucket": "infrahub-backups",
    }


# ---------------------------------------------------------------------------
# Fixture: infrahub_docker
# ---------------------------------------------------------------------------
@pytest.fixture(scope="session")
async def infrahub_docker(request: pytest.FixtureRequest) -> AsyncGenerator[dict, None]:
    """Start Infrahub via Docker Compose with a random project name."""
    project = f"e2e-infrahub-{uuid.uuid4().hex[:8]}"
    compose_file = str(FIXTURES_DIR / "docker-compose" / "docker-compose.yml")

    def teardown():
        subprocess.run(
            [
                "docker",
                "compose",
                "-f",
                compose_file,
                "-p",
                project,
                "down",
                "-v",
                "--remove-orphans",
            ],
            capture_output=True,
        )

    request.addfinalizer(teardown)

    subprocess.run(
        ["docker", "compose", "-f", compose_file, "-p", project, "up", "-d", "--wait"],
        check=True,
    )

    # Wait for Infrahub server to be healthy
    url = "http://localhost:8000"
    await wait_for_http(f"{url}/api/config", timeout=300.0, interval=5.0)

    yield {
        "project": project,
        "url": url,
        "token": INFRAHUB_ADMIN_TOKEN,
    }


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

    # Port-forward infrahub-server
    kr8s_api = await kr8s.asyncio.api(kubeconfig=kubeconfig_path)
    service = await AsyncService.get(
        "infrahub-infrahub-server", namespace=namespace, api=kr8s_api
    )
    async with service.portforward(remote_port=8000, local_port="auto") as local_port:
        url = f"http://localhost:{local_port}"
        await wait_for_http(f"{url}/api/config", timeout=300.0, interval=5.0)

        yield {
            "namespace": namespace,
            "kubeconfig_path": kubeconfig_path,
            "url": url,
            "token": INFRAHUB_ADMIN_TOKEN,
        }


# ---------------------------------------------------------------------------
# Fixture: minio_k8s
# ---------------------------------------------------------------------------
@pytest.fixture(scope="session")
async def minio_k8s(
    vcluster: dict,
    infrahub_k8s: dict,
) -> AsyncGenerator[dict, None]:
    """Deploy MinIO into vcluster for S3 testing."""
    kubeconfig_path = vcluster["kubeconfig_path"]
    api: kubeclient.ApiClient = vcluster["api"]
    namespace = infrahub_k8s["namespace"]

    # Deploy MinIO from manifest
    manifest_path = FIXTURES_DIR / "minio" / "deployment.yaml"
    with open(manifest_path) as f:
        docs = list(yaml.safe_load_all(f))

    apps_api = kubeclient.AppsV1Api(api)
    core_api = kubeclient.CoreV1Api(api)

    for doc in docs:
        if doc["kind"] == "Deployment":
            await apps_api.create_namespaced_deployment(namespace=namespace, body=doc)
        elif doc["kind"] == "Service":
            await core_api.create_namespaced_service(namespace=namespace, body=doc)

    await _wait_for_service_ready(
        core_api,
        name="minio",
        namespace=namespace,
        label_selector="app=minio",
        pod_timeout=120.0,
    )

    # Port-forward to create bucket
    kr8s_api = await kr8s.asyncio.api(kubeconfig=kubeconfig_path)
    service = await AsyncService.get("minio", namespace=namespace, api=kr8s_api)
    async with service.portforward(remote_port=9000, local_port="auto") as local_port:
        local_endpoint = f"http://localhost:{local_port}"
        await wait_for_http(f"{local_endpoint}/minio/health/ready", timeout=30.0)

        s3 = boto3.client(
            "s3",
            endpoint_url=local_endpoint,
            aws_access_key_id="minioadmin",
            aws_secret_access_key="minioadmin",
            region_name="us-east-1",
        )
        s3.create_bucket(Bucket="infrahub-backups")

        yield {
            "cluster_endpoint": f"http://minio.{namespace}.svc:9000",
            "local_endpoint": local_endpoint,
            "access_key": "minioadmin",
            "secret_key": "minioadmin",
            "bucket": "infrahub-backups",
        }
