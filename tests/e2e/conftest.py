import os
import subprocess
import time
from contextlib import asynccontextmanager
from pathlib import Path
from typing import AsyncGenerator

import kr8s.asyncio
import pytest
import yaml
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

    vcluster = request.getfixturevalue("vcluster") if "vcluster" in request.fixturenames else None
    infrahub_k8s = request.getfixturevalue("infrahub_k8s") if "infrahub_k8s" in request.fixturenames else None
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
# Fixture: collect_binary
# ---------------------------------------------------------------------------
@pytest.fixture(scope="session")
def collect_binary() -> str:
    """Build infrahub-collect and return the binary path."""
    subprocess.run(["make", "build"], cwd=str(PROJECT_ROOT), check=True)
    binary = PROJECT_ROOT / "bin" / "infrahub-collect"
    assert binary.exists(), f"Binary not found at {binary}"
    return str(binary)


# ---------------------------------------------------------------------------
# Fixture: s3_server (Garage, testcontainers)
# ---------------------------------------------------------------------------
GARAGE_IMAGE = "dxflrs/garage:v2.4.1"
GARAGE_ADMIN_TOKEN = "infrahub-backup-e2e-admin-token"
GARAGE_RPC_SECRET = "a0b1c2d3e4f5061728394a5b6c7d8e9fa0b1c2d3e4f5061728394a5b6c7d8e9f"
# Garage only imports keys shaped like the ones it generates: "GK" and 24 hex
# characters, with a 64-character hex secret. Fixed test values, not secrets.
S3_ACCESS_KEY = "GKdeadbeefdeadbeefdeadbeef"
S3_SECRET_KEY = "deadbeef" * 8
S3_BUCKET = "backups"
# Mounted from the checkout rather than written to a pytest temporary directory:
# in the Kubernetes CI job, Garage did not start with its config mounted from /tmp.
GARAGE_CONFIG_PATH = FIXTURES_DIR / "garage.toml"


def _bootstrap_garage(admin_url: str) -> None:
    """Make a fresh Garage node serve the test bucket.

    A new node stores nothing until a layout is assigned, and the access key and
    bucket have to be created through the admin API. The key may also create
    buckets, which the retention tests do for each case.
    """
    import requests

    def api(method: str, endpoint: str, payload: dict | None = None) -> dict:
        resp = requests.request(
            method,
            f"{admin_url}/v2/{endpoint}",
            headers={"Authorization": f"Bearer {GARAGE_ADMIN_TOKEN}"},
            json=payload,
            timeout=5,
        )
        resp.raise_for_status()
        return resp.json() if resp.content else {}

    deadline = time.time() + 30
    while True:
        try:
            status = api("GET", "GetClusterStatus")
            break
        except requests.RequestException:
            if time.time() > deadline:
                raise
            time.sleep(1)

    node_id = status["nodes"][0]["id"]
    api("POST", "UpdateClusterLayout", {"roles": [{"id": node_id, "zone": "e2e", "capacity": 10**9, "tags": []}]})
    api("POST", "ApplyClusterLayout", {"version": status["layoutVersion"] + 1})

    api("POST", "ImportKey", {"accessKeyId": S3_ACCESS_KEY, "secretAccessKey": S3_SECRET_KEY, "name": "e2e"})
    api("POST", f"UpdateKey?id={S3_ACCESS_KEY}", {"allow": {"createBucket": True}})
    bucket = api("POST", "CreateBucket", {"globalAlias": S3_BUCKET})
    api(
        "POST",
        "AllowBucketKey",
        {
            "bucketId": bucket["id"],
            "accessKeyId": S3_ACCESS_KEY,
            "permissions": {"read": True, "write": True, "owner": True},
        },
    )

    # The admin API reports healthy only once the layout is applied.
    while requests.get(f"{admin_url}/health", timeout=5).status_code != 200:
        if time.time() > deadline:
            raise TimeoutError(f"Garage at {admin_url} did not become healthy")
        time.sleep(1)


def _container_report(container: DockerContainer) -> str:
    """Describe a container that did not start serving: its state, its mounts and its logs."""
    wrapped = container.get_wrapped_container()
    wrapped.reload()
    state = wrapped.attrs.get("State", {})
    mounts = ", ".join(
        f"{mount.get('Source')} -> {mount.get('Destination')}" for mount in wrapped.attrs.get("Mounts", [])
    )
    stdout, stderr = container.get_logs()
    logs = (stdout + stderr).decode(errors="replace").strip() or "<no output>"
    return (
        f"container {wrapped.short_id} is {state.get('Status')} (exit code {state.get('ExitCode')}), "
        f"mounts: {mounts or '<none>'}\n{logs}"
    )


@pytest.fixture(scope="session")
def s3_server(request: pytest.FixtureRequest) -> dict:
    """Start a single-node Garage container that serves an S3 bucket for the tests."""
    container = (
        DockerContainer(GARAGE_IMAGE)
        .with_exposed_ports(3900, 3903)
        .with_env("GARAGE_RPC_SECRET", GARAGE_RPC_SECRET)
        .with_env("GARAGE_ADMIN_TOKEN", GARAGE_ADMIN_TOKEN)
        .with_volume_mapping(str(GARAGE_CONFIG_PATH), "/etc/garage.toml", "ro")
    )

    container.start()
    request.addfinalizer(container.stop)

    host = container.get_container_host_ip()
    try:
        _bootstrap_garage(f"http://{host}:{container.get_exposed_port(3903)}")
    except Exception as exc:
        raise RuntimeError(f"Garage did not start serving S3: {_container_report(container)}") from exc

    return {
        "endpoint": f"http://{host}:{container.get_exposed_port(3900)}",
        "access_key": S3_ACCESS_KEY,
        "secret_key": S3_SECRET_KEY,
        "bucket": S3_BUCKET,
    }


# ---------------------------------------------------------------------------
# Helper: portforward_infrahub — fresh port-forward for each use
# ---------------------------------------------------------------------------
@asynccontextmanager
async def portforward_infrahub(kubeconfig_path: str, namespace: str):
    """Open a fresh port-forward to infrahub-server and yield the local URL.

    Pods may be restarted by infrahub-backup during restore, which kills any
    long-lived port-forward.  Call this each time you need to talk to Infrahub.

    Waits until Infrahub can query Neo4j, not only until it answers HTTP.
    /api/config and anonymous requests answer without Neo4j, but Infrahub
    looks an API key up in Neo4j, so the wait sends the admin token.
    """
    kr8s_api = await kr8s.asyncio.api(kubeconfig=kubeconfig_path)
    service = await AsyncService.get("infrahub-infrahub-server", namespace=namespace, api=kr8s_api)
    async with service.portforward(remote_port=8000, local_port="auto") as local_port:
        url = f"http://localhost:{local_port}"
        await wait_for_http(
            f"{url}/api/schema?branch=main",
            timeout=300.0,
            interval=5.0,
            headers={"X-INFRAHUB-KEY": INFRAHUB_ADMIN_TOKEN},
        )
        yield url


# ---------------------------------------------------------------------------
# Helper: delete database pod to reset CrashLoopBackOff
# ---------------------------------------------------------------------------
async def _delete_and_wait_for_database_pod(kubeconfig_path: str, namespace: str, timeout: float = 300.0) -> None:
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
