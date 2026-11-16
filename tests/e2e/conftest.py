import asyncio
import os
import subprocess
import time
import uuid
from contextlib import asynccontextmanager
from pathlib import Path
from typing import AsyncGenerator, Generator

import kr8s.asyncio
import pytest
import yaml
from kr8s.asyncio.objects import Pod as AsyncPod
from kr8s.asyncio.objects import Secret as AsyncSecret
from kr8s.asyncio.objects import Service as AsyncService
from testcontainers.core.container import DockerContainer

from tests.conftest import _dump_namespace_logs
from tests.helpers.utils import wait_for_database_query, wait_for_http

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
    if not vcluster:
        return

    # The deployment fixtures yield the same shape and live in different namespaces, and a
    # test uses only one of them, so dump whichever the test asked for. A deployment whose
    # Neo4j cluster runs in a namespace of its own names it as `neo4j_namespace`.
    for fixture in (
        "infrahub_k8s",
        "infrahub_k8s_external",
        "infrahub_k8s_neo4j_cluster",
        "infrahub_k8s_external_neo4j_cluster",
    ):
        deployment = request.getfixturevalue(fixture) if fixture in request.fixturenames else None
        if deployment:
            _dump_namespace_logs(vcluster["kubeconfig_path"], deployment["namespace"])
            if deployment.get("neo4j_namespace"):
                _dump_namespace_logs(vcluster["kubeconfig_path"], deployment["neo4j_namespace"])


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
        "region": "us-east-1",  # `s3_region` in garage.toml
        # For a fixture that attaches the container to another Docker network.
        "container_id": container.get_wrapped_container().id,
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
        # /api/schema answers before every server worker can reach the database again
        # after a restore or the database pod reset between tests, so also wait for a
        # query that reads the database.
        await wait_for_database_query(url, INFRAHUB_ADMIN_TOKEN)
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
# Helpers: Helm values and installation
# ---------------------------------------------------------------------------
def _deep_merge(base: dict, overlay: dict) -> dict:
    """Merge `overlay` onto `base`, recursing into nested mappings.

    Helm values are trees, so an overlay that sets one leaf must not replace the whole
    branch it lives on: the external-topology overlay adds keys beside the tuned probes
    and resource presets in the base values rather than instead of them.
    """
    merged = dict(base)
    for key, value in overlay.items():
        if isinstance(value, dict) and isinstance(merged.get(key), dict):
            merged[key] = _deep_merge(merged[key], value)
        else:
            merged[key] = value
    return merged


def _infrahub_values_path(tmp_path_factory, overlay: dict | None = None):
    """The Helm values file for a deployment, overlaid and wrapped for the edition.

    Enterprise wraps the community values under an `infrahub` key, so an overlay is
    applied to the community shape *before* wrapping: its keys name community values and
    stay the same across both editions.
    """
    base_path = FIXTURES_DIR / "helm" / "infrahub-values.yaml"
    if overlay is None and not _is_enterprise():
        return base_path

    with open(base_path) as f:
        values = yaml.safe_load(f)
    if overlay:
        values = _deep_merge(values, overlay)
    if _is_enterprise():
        global_values = values.pop("global", {})
        values = {"infrahub": values, "global": global_values}

    values_path = tmp_path_factory.mktemp("helm") / "infrahub-values.yaml"
    with open(values_path, "w") as f:
        yaml.dump(values, f)
    return values_path


def _helm_install_infrahub(kubeconfig_path: str, namespace: str, values_path) -> None:
    """Install Infrahub via Helm into a namespace and wait for it to be ready."""
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

    values_path = _infrahub_values_path(tmp_path_factory)
    _helm_install_infrahub(kubeconfig_path, namespace, values_path)

    # Verify Infrahub is healthy with an initial port-forward
    async with portforward_infrahub(kubeconfig_path, namespace):
        pass  # health check happens inside the context manager

    yield {
        "namespace": namespace,
        "kubeconfig_path": kubeconfig_path,
        "token": INFRAHUB_ADMIN_TOKEN,
    }


# ---------------------------------------------------------------------------
# External database topology (spec 007-external-database-backup)
# ---------------------------------------------------------------------------
# A database that lives *outside* the Infrahub namespace cannot be stood up by this
# suite. It needs a Neo4j Enterprise licence, a backup listener bound to a non-loopback
# address (it defaults to localhost, so this is a server configuration change), and — for
# restore — an object store the *database host itself* can read, because an external
# restore inverts the data path and the server fetches the artifact. None of that is
# available from a vcluster created here, so the topology is supplied by the environment
# and the external suites skip, by name, when it is absent. In CI, the enterprise leg of
# the `e2e-tests-k8s` job provides it (tasks.md T072).
#
# Declared by:
#
#   INFRAHUB_TESTING_EXTERNAL_DB
#       Comma-separated deployment service names hosted outside the namespace:
#       `database` (Neo4j), `task-manager-db` (PostgreSQL). External mode is decided per
#       service, so `database` alone is the mixed topology and both names is the
#       both-external one. Unset or empty means no external topology at all.
#   INFRAHUB_TESTING_EXTERNAL_NEO4J_ADDRESS
#       `host[:port][,host[:port]...]`, the shape Infrahub's own INFRAHUB_DB_ADDRESS
#       carries. The port is Bolt (7687) — *not* the backup listener (6362), which the
#       tool derives itself.
#   INFRAHUB_TESTING_EXTERNAL_NEO4J_USERNAME / _PASSWORD
#       Credentials for that server. Optional: the tool discovers them from the
#       deployment's own components, and these only pin them for the test.
#   INFRAHUB_TESTING_EXTERNAL_NEO4J_EDITION
#       `enterprise` or `community`. Capture from an external Community server is refused
#       by design (FR-008), so the round-trip cases need `enterprise` and the refusal case
#       needs `community`; neither may silently run against the other.
#   INFRAHUB_TESTING_EXTERNAL_POSTGRES_ADDRESS / _POSTGRES_USERNAME / _POSTGRES_PASSWORD
#       The same, for the task manager's PostgreSQL.
#   INFRAHUB_TESTING_EXTERNAL_SEED_BUCKET / _SEED_ENDPOINT / _SEED_ACCESS_KEY /
#   _SEED_SECRET_KEY / _SEED_REGION / _SEED_PREFIX
#       An object store the database server can read, for the `seedURI` staging a restore
#       writes. The session's `minio_docker` container does NOT qualify: the tool can
#       reach it and the database cannot, which is exactly the distinction that makes this
#       topology hard to provide.
#   INFRAHUB_TESTING_EXTERNAL_HELM_VALUES
#       Optional path to a Helm values overlay pointing the chart at those endpoints,
#       replacing the generated overlay below when the chart's keys differ from it.

#   INFRAHUB_TESTING_EXTERNAL_DB_IMAGE
#       The infrahub-backup image built from this checkout, as a local Docker tag that is
#       not `latest` (the CI job tags it with the commit). The external path runs its
#       database tools from this image (FR-031, ADR 0017), and a development build has no
#       published image to default to, so every external case passes it with
#       `--external-db-image`. The `external_db_image` fixture loads it into the session's
#       vcluster; the cluster is created by pytest, so it cannot be loaded beforehand.
#   INFRAHUB_TESTING_EXTERNAL_INSECURE_TLS
#       `true` when the servers present self-signed certificates. Every external case
#       then passes `--external-db-insecure-tls`: the tool verifies certificates by
#       default (FR-029) and always encrypts PostgreSQL, so a server without TLS cannot
#       serve these suites at all.
#   INFRAHUB_TESTING_EXTERNAL_SCRATCH_SIZE
#       A storage quantity such as `2Gi`, passed as `--external-db-scratch-size` on every
#       external case. Needed until T158: Neo4j 2025.x reports no store size, so a capture
#       without one refuses to start.

EXTERNAL_DB_ENV_VAR = "INFRAHUB_TESTING_EXTERNAL_DB"
EXTERNAL_HELM_VALUES_ENV_VAR = "INFRAHUB_TESTING_EXTERNAL_HELM_VALUES"
EXTERNAL_DB_IMAGE_ENV_VAR = "INFRAHUB_TESTING_EXTERNAL_DB_IMAGE"
EXTERNAL_INSECURE_TLS_ENV_VAR = "INFRAHUB_TESTING_EXTERNAL_INSECURE_TLS"
EXTERNAL_SCRATCH_SIZE_ENV_VAR = "INFRAHUB_TESTING_EXTERNAL_SCRATCH_SIZE"

# The deployment service names the tool resolves per service, as `docker compose` and the
# Helm chart name them.
SERVICE_NEO4J = "database"
SERVICE_TASK_MANAGER_DB = "task-manager-db"

# The endpoint variable each external service needs before its suite can run.
EXTERNAL_ADDRESS_ENV_VARS = {
    SERVICE_NEO4J: "INFRAHUB_TESTING_EXTERNAL_NEO4J_ADDRESS",
    SERVICE_TASK_MANAGER_DB: "INFRAHUB_TESTING_EXTERNAL_POSTGRES_ADDRESS",
}

EXTERNAL_SEED_ENV_VARS = (
    "INFRAHUB_TESTING_EXTERNAL_SEED_BUCKET",
    "INFRAHUB_TESTING_EXTERNAL_SEED_ENDPOINT",
    "INFRAHUB_TESTING_EXTERNAL_SEED_ACCESS_KEY",
    "INFRAHUB_TESTING_EXTERNAL_SEED_SECRET_KEY",
)

# A host:port that resolves to nothing, for the "one endpoint of several unreachable"
# case. `.invalid` is reserved by RFC 2606 precisely so it can never resolve.
UNREACHABLE_ENDPOINT = "neo4j-does-not-exist.invalid:7687"


def external_db_services() -> set[str]:
    """The deployment services the environment declares as hosted outside the namespace."""
    raw = os.environ.get(EXTERNAL_DB_ENV_VAR, "")
    return {name.strip() for name in raw.split(",") if name.strip()}


def external_neo4j_edition() -> str:
    """The edition of the external Neo4j server, lowercased, or "" if undeclared."""
    return os.environ.get("INFRAHUB_TESTING_EXTERNAL_NEO4J_EDITION", "").strip().lower()


def _external_skip_reason(services: tuple[str, ...], edition: str = "", seed_store: bool = False) -> str | None:
    """Why the external topology cannot serve these services, or None if it can.

    Returns a reason naming both the missing piece of topology and the variable that
    supplies it, because a skip whose reason does not say what is missing is
    indistinguishable from a test that passed.
    """
    declared = external_db_services()
    missing = [service for service in services if service not in declared]
    if missing:
        return (
            f"needs {', '.join(missing)} hosted outside the Infrahub namespace: set "
            f"{EXTERNAL_DB_ENV_VAR}={','.join(services)}. In CI, only the enterprise leg of e2e-tests-k8s "
            "provides one (specs/archive/007-external-database-backup/tasks.md T072)"
        )

    unset = [
        EXTERNAL_ADDRESS_ENV_VARS[service]
        for service in services
        if service in EXTERNAL_ADDRESS_ENV_VARS and not os.environ.get(EXTERNAL_ADDRESS_ENV_VARS[service])
    ]
    if unset:
        return (
            f"{EXTERNAL_DB_ENV_VAR} declares {', '.join(services)} external but does not say where: "
            f"set {', '.join(unset)}"
        )

    if edition and external_neo4j_edition() != edition:
        return (
            f"needs the external Neo4j server to be {edition} (capture from an external Community "
            f"server is refused by design, FR-008): set "
            f"INFRAHUB_TESTING_EXTERNAL_NEO4J_EDITION={edition}"
        )

    if seed_store:
        unset_seed = [name for name in EXTERNAL_SEED_ENV_VARS if not os.environ.get(name)]
        if unset_seed:
            return (
                "needs an object store the external database server itself can read, for the "
                f"seedURI staging a restore writes: set {', '.join(unset_seed)}. The suite's own "
                "MinIO container does not qualify — the tool reaches it and the database does not"
            )

    return None


def requires_external_db(*services: str, edition: str = "", seed_store: bool = False):
    """Skip unless the environment provides these services outside the namespace.

    `edition` pins the external Neo4j edition the case is written for, and `seed_store`
    additionally requires the object store a restore stages its seed into.
    """
    reason = _external_skip_reason(services, edition=edition, seed_store=seed_store)
    return pytest.mark.skipif(reason is not None, reason=reason or "")


@pytest.fixture(scope="session")
def external_topology() -> dict:
    """The external database endpoints and credentials the environment declares.

    Skips as well as the marker on each case does, so that a case which forgets the marker
    cannot pass by finding no external database and quietly testing the internal path.
    """
    services = tuple(sorted(external_db_services()))
    reason = _external_skip_reason(services or (SERVICE_NEO4J,))
    if reason is not None:
        pytest.skip(reason)

    return {
        "services": set(services),
        "neo4j_address": os.environ.get("INFRAHUB_TESTING_EXTERNAL_NEO4J_ADDRESS", ""),
        "neo4j_username": os.environ.get("INFRAHUB_TESTING_EXTERNAL_NEO4J_USERNAME", ""),
        "neo4j_password": os.environ.get("INFRAHUB_TESTING_EXTERNAL_NEO4J_PASSWORD", ""),
        "neo4j_edition": external_neo4j_edition(),
        "postgres_address": os.environ.get("INFRAHUB_TESTING_EXTERNAL_POSTGRES_ADDRESS", ""),
        "postgres_username": os.environ.get("INFRAHUB_TESTING_EXTERNAL_POSTGRES_USERNAME", ""),
        "postgres_password": os.environ.get("INFRAHUB_TESTING_EXTERNAL_POSTGRES_PASSWORD", ""),
    }


@pytest.fixture(scope="session")
def external_seed_store(external_topology: dict) -> dict:
    """The object store a restore stages its seed into, which the *database* must reach."""
    unset = [name for name in EXTERNAL_SEED_ENV_VARS if not os.environ.get(name)]
    if unset:
        pytest.skip(
            "needs an object store the external database server itself can read, for the seedURI "
            f"staging a restore writes: set {', '.join(unset)}"
        )

    return {
        "bucket": os.environ["INFRAHUB_TESTING_EXTERNAL_SEED_BUCKET"],
        "endpoint": os.environ["INFRAHUB_TESTING_EXTERNAL_SEED_ENDPOINT"],
        "access_key": os.environ["INFRAHUB_TESTING_EXTERNAL_SEED_ACCESS_KEY"],
        "secret_key": os.environ["INFRAHUB_TESTING_EXTERNAL_SEED_SECRET_KEY"],
        "region": os.environ.get("INFRAHUB_TESTING_EXTERNAL_SEED_REGION", "us-east-1"),
        "prefix": os.environ.get("INFRAHUB_TESTING_EXTERNAL_SEED_PREFIX", ""),
    }


def _external_values_overlay(topology: dict) -> dict:
    """Helm values pointing the chart at the external endpoints instead of its own.

    The chart keys were confirmed against the published chart on 2026-09-03 --
    `oci://registry.opsmill.io/opsmill/chart/infrahub:4.33.2`, appVersion 1.11.2, which
    pulls anonymously -- so this no longer needs the
    INFRAHUB_TESTING_EXTERNAL_HELM_VALUES override for the keys' sake. That override
    remains for a chart whose keys have since moved.

    What that confirmation found:

    - `neo4j.enabled` is real, and `neo4j.nameOverride` is `database` -- which is also
      why the tool's service name matches. Disabling the subchart is what leaves no
      `database` workload in the namespace, which is the condition that puts the tool
      into external mode in the first place.
    - `prefect-server.postgresql.enabled` is real.
    - **`global.infrahubEnv` does not exist.** The chart's `global` block carries only
      `kubernetesClusterDomain`, `infrahubRepository`, `infrahubImageFlavor`,
      `imagePullPolicy`, `imagePullSecrets` and `commonLabels`. Environment is per
      component, and the two that read the database nest it under the component's own
      name again: `infrahubServer.infrahubServer.env` and
      `infrahubTaskWorker.infrahubTaskWorker.env` (each ships `INFRAHUB_DB_TYPE: neo4j`
      by default). Helm does not reject an unknown values key, so the previous shape was
      accepted in silence and left every component pointed at the Neo4j the overlay had
      just disabled -- a failure that would have read as a broken topology rather than
      as a wrong key. The singly-nested `<component>.env` is wrong the same silent way;
      only the doubly-nested path reaches the pod.

    Verified by rendering, not by reading: `helm template` with the shape below emits
    `INFRAHUB_DB_ADDRESS` into both pod specs, and disabling the subchart leaves
    `postgresql`, `message-queue` and `cache-master` as the only StatefulSets with no
    workload or service named `database` anywhere in the output -- which is the
    condition that puts the tool into external mode.
    """
    env: dict[str, str] = {}
    overlay: dict = {}

    if SERVICE_NEO4J in topology["services"]:
        # Stop the chart deploying its own Neo4j, and point every component at the
        # external one the way Infrahub itself reads the endpoint.
        overlay["neo4j"] = {"enabled": False}
        env["INFRAHUB_DB_ADDRESS"] = topology["neo4j_address"]
        if topology["neo4j_username"]:
            env["INFRAHUB_DB_USERNAME"] = topology["neo4j_username"]
        if topology["neo4j_password"]:
            env["INFRAHUB_DB_PASSWORD"] = topology["neo4j_password"]

    if SERVICE_TASK_MANAGER_DB in topology["services"]:
        user = topology["postgres_username"] or "postgres"
        password = topology["postgres_password"] or "prefect"
        address = topology["postgres_address"]
        host, _, port = address.rpartition(":") if ":" in address else (address, "", "5432")
        # With its own PostgreSQL disabled, the prefect-server subchart builds the task
        # manager's connection string from `secret`, and refuses to render without it.
        overlay["prefect-server"] = {
            "postgresql": {"enabled": False},
            "secret": {
                "create": True,
                "username": user,
                "password": password,
                "host": host,
                "port": port,
                "database": "prefect",
            },
        }
        env["PREFECT_API_DATABASE_CONNECTION_URL"] = f"postgresql+asyncpg://{user}:{password}@{address}/prefect"

    # Both components that read the database get the same environment, each under its own
    # name twice. The chart has no single place to say this once; see the docstring.
    if env:
        for component in ("infrahubServer", "infrahubTaskWorker"):
            overlay[component] = {component: {"env": dict(env)}}

    return overlay


def load_image_into_vcluster(image: str, cluster_name: str) -> None:
    """Import a local Docker image into every node of a docker-driver vcluster.

    The `vcluster` fixture creates the cluster with `--driver=docker`, which runs each node
    as a Docker container (`vcluster.cp.<name>`, and `vcluster.node.<name>...` for any
    worker) with its own containerd. Those nodes do not see the host's Docker images, so
    the image is streamed into each node's `k8s.io` containerd namespace, which is the one
    the kubelet reads. A pod then starts from it with `imagePullPolicy: IfNotPresent`,
    which the transient Job sets; a `latest` tag would default to `Always` and be pulled.
    """
    names = subprocess.run(
        ["docker", "ps", "--format", "{{.Names}}"], check=True, capture_output=True, text=True
    ).stdout.split()
    nodes = [
        name
        for name in names
        if (name.startswith("vcluster.cp.") or name.startswith("vcluster.node.")) and cluster_name in name
    ]
    if not nodes:
        raise RuntimeError(f"found no node container for vcluster {cluster_name} to load {image} into")

    for node in nodes:
        save = subprocess.Popen(["docker", "save", image], stdout=subprocess.PIPE)
        try:
            subprocess.run(
                ["docker", "exec", "-i", node, "ctr", "-n", "k8s.io", "images", "import", "--all-platforms", "-"],
                stdin=save.stdout,
                check=True,
                capture_output=True,
            )
        finally:
            if save.stdout:
                save.stdout.close()
            if save.wait() != 0:
                raise RuntimeError(f"docker save {image} failed; build it first")


def _require_pinned_tag(image: str) -> None:
    """Refuse an image reference that a pod would pull rather than take from the node."""
    last = image.rsplit("/", 1)[-1]
    if ":" not in last or last.endswith(":latest") or "@" in last:
        pytest.fail(
            f"{EXTERNAL_DB_IMAGE_ENV_VAR}={image} must be a local tag other than `latest`, such as "
            "`infrahub-backup:e2e-local`: the image is loaded into the cluster, not pulled"
        )


@pytest.fixture(scope="session")
def external_db_image(vcluster: dict, external_topology: dict) -> str:
    """The infrahub-backup image under test, loaded into the session's vcluster.

    Depends on `external_topology` so that a session with no external database skips
    before anything is built or loaded. With the topology declared and no image, the case
    fails rather than skips: a development build refuses the external path without
    `--external-db-image`, so a skip here would hide every external case.
    """
    image = os.environ.get(EXTERNAL_DB_IMAGE_ENV_VAR, "").strip()
    if not image:
        pytest.fail(
            f"the external topology is declared but {EXTERNAL_DB_IMAGE_ENV_VAR} is not: build the "
            "image from this checkout (`docker build -t infrahub-backup:e2e-local .`) and set "
            f"{EXTERNAL_DB_IMAGE_ENV_VAR}=infrahub-backup:e2e-local"
        )
    _require_pinned_tag(image)
    load_image_into_vcluster(image, vcluster["cluster_name"])
    return image


def external_topology_args() -> list[str]:
    """The flags the topology needs on every external run, whatever the case asserts.

    `--external-db-insecure-tls` when the servers present a certificate nothing can verify:
    the tool verifies certificates by default (FR-029) and always encrypts the PostgreSQL
    connection, so a test topology with self-signed certificates has to opt out, as an
    operator would. The channel stays encrypted.

    `--external-db-scratch-size` when the topology declares one: Neo4j 2025.x reports no
    store size, so a capture refuses to guess one (tasks.md T158). A case that passes its
    own size later on the command line overrides this one, because the last value wins.
    """
    args: list[str] = []
    if os.environ.get(EXTERNAL_INSECURE_TLS_ENV_VAR, "").strip().lower() in ("1", "true", "yes"):
        args.append("--external-db-insecure-tls")
    scratch = os.environ.get(EXTERNAL_SCRATCH_SIZE_ENV_VAR, "").strip()
    if scratch:
        args += ["--external-db-scratch-size", scratch]
    return args


def external_db_image_args(deployment: dict) -> list[str]:
    """The root flags that select how the external path runs, for a CLI invocation.

    A deployment that builds its own topology carries its flags as `topology_args`; the
    others take them from the environment.
    """
    topology_args = deployment.get("topology_args")
    if topology_args is None:
        topology_args = external_topology_args()
    return ["--external-db-image", deployment["external_db_image"], *topology_args]


# A Dockerfile, built with no context, that derives an image from the one under test whose
# `neo4j-admin --version` reports a chosen version and which otherwise runs the real tool.
# The FR-006 gate compares what `neo4j-admin --version` reports in the workload with what
# the live server reports, so this provokes either direction of the gate against the real
# server, whatever its version, without a second Neo4j distribution in the test.
_TOOLING_VERSION_DOCKERFILE = """\
FROM {base}
USER root
RUN set -eu; \\
    real="$(command -v neo4j-admin)"; \\
    mv "$real" "$real.real"; \\
    printf '#!/bin/sh\\nif [ "$1" = "--version" ]; then echo "{version}"; exit 0; fi\\nexec "%s.real" "$@"\\n' \\
      "$real" > "$real"; \\
    chmod 0755 "$real"
USER infrahub
"""


def build_tooling_image_reporting(base: str, version: str) -> str:
    """Build an image from `base` whose neo4j-admin reports `version`, and return its tag."""
    repository, _, tag = base.rpartition(":")
    derived = f"{repository}:{tag}-neo4j-admin-{version}"
    subprocess.run(
        ["docker", "build", "-t", derived, "-"],
        input=_TOOLING_VERSION_DOCKERFILE.format(base=base, version=version),
        text=True,
        check=True,
    )
    return derived


@pytest.fixture(scope="session")
def tooling_image_reporting(vcluster: dict, external_db_image: str):
    """A factory for images whose neo4j-admin reports a given version, loaded into the vcluster."""

    def build(version: str) -> str:
        derived = build_tooling_image_reporting(external_db_image, version)
        load_image_into_vcluster(derived, vcluster["cluster_name"])
        return derived

    return build


@pytest.fixture(scope="session")
async def infrahub_k8s_external(
    vcluster: dict,
    external_topology: dict,
    external_db_image: str,
    tmp_path_factory,
) -> AsyncGenerator[dict, None]:
    """Deploy Infrahub into vcluster pointed at the external database(s) by env.

    Deliberately a second namespace rather than a reconfiguration of `infrahub_k8s`: the
    internal deployment is what the FR-015 no-regression cases run against, and a session
    that runs both topologies must not have the external one perturb it.
    """
    kubeconfig_path = vcluster["kubeconfig_path"]
    namespace = "infrahub-external"

    override = os.environ.get(EXTERNAL_HELM_VALUES_ENV_VAR)
    if override:
        with open(override) as f:
            overlay = yaml.safe_load(f) or {}
    else:
        overlay = _external_values_overlay(external_topology)

    values_path = _infrahub_values_path(tmp_path_factory, overlay=overlay)
    _helm_install_infrahub(kubeconfig_path, namespace, values_path)

    async with portforward_infrahub(kubeconfig_path, namespace):
        pass  # health check happens inside the context manager

    yield {
        "namespace": namespace,
        "kubeconfig_path": kubeconfig_path,
        "token": INFRAHUB_ADMIN_TOKEN,
        "external_services": set(external_topology["services"]),
        "external_db_image": external_db_image,
    }


def external_db_env(topology: dict) -> dict[str, str]:
    """Environment for a CLI invocation against the external topology.

    The tool discovers credentials from the deployment's own components; passing them here
    as well pins what the test ran against, so a discovery regression shows up as a
    discovery test failing rather than as every case in these suites going red.
    """
    env: dict[str, str] = {}
    if topology.get("neo4j_username"):
        env["INFRAHUB_DB_USERNAME"] = topology["neo4j_username"]
    if topology.get("neo4j_password"):
        env["INFRAHUB_DB_PASSWORD"] = topology["neo4j_password"]
    return env


# The label every transient object the external paths create carries, and the selector the
# tool's own stray reaper uses. Kept in step with `transientLabelMarker` in
# src/internal/app/transient_workload.go.
TRANSIENT_LABEL_MARKER = "infrahub-backup/transient"

# The name prefix those objects carry, deliberately containing no deployment service name
# as a substring. Kept in step with `transientObjectPrefix` in the same file.
TRANSIENT_OBJECT_PREFIX = "infrahub-backup-xdb"


@asynccontextmanager
async def restricted_kubeconfig(kubeconfig_path: str, namespace: str, rules: list[dict], name: str):
    """A kubeconfig for an identity holding exactly `rules` in `namespace`, and nothing else.

    The diagnostic scenarios for FR-002 and FR-019 are about what the tool does when the
    cluster refuses it, and the only faithful way to provoke a refusal is to run as an
    identity that does not hold the verb. Editing the tool's own behaviour to simulate one
    would test the simulation; revoking a verb on the ambient kubeconfig would revoke it
    for the whole session.

    So a ServiceAccount, a Role carrying the supplied rules, and a RoleBinding are created
    for the duration of the case, and the token is written into a kubeconfig pointed at the
    same API server. Everything is removed on the way out, including on a failure, so the
    next case runs against the session's own identity.
    """
    with open(kubeconfig_path) as f:
        ambient = yaml.safe_load(f)
    cluster = ambient["clusters"][0]

    def kubectl(*args: str, capture: bool = False) -> str:
        result = subprocess.run(
            ["kubectl", "--kubeconfig", kubeconfig_path, "-n", namespace, *args],
            check=True,
            capture_output=capture,
            text=True,
        )
        return result.stdout if capture else ""

    manifests = yaml.dump_all(
        [
            {"apiVersion": "v1", "kind": "ServiceAccount", "metadata": {"name": name}},
            {
                "apiVersion": "rbac.authorization.k8s.io/v1",
                "kind": "Role",
                "metadata": {"name": name},
                "rules": rules,
            },
            {
                "apiVersion": "rbac.authorization.k8s.io/v1",
                "kind": "RoleBinding",
                "metadata": {"name": name},
                "roleRef": {"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": name},
                "subjects": [{"kind": "ServiceAccount", "name": name, "namespace": namespace}],
            },
        ]
    )

    restricted_path = Path(kubeconfig_path).with_name(f"kubeconfig-{name}.yaml")
    try:
        subprocess.run(
            ["kubectl", "--kubeconfig", kubeconfig_path, "-n", namespace, "apply", "-f", "-"],
            input=manifests,
            check=True,
            text=True,
        )
        token = kubectl("create", "token", name, "--duration", "30m", capture=True).strip()

        with open(restricted_path, "w") as f:
            yaml.dump(
                {
                    "apiVersion": "v1",
                    "kind": "Config",
                    "clusters": [cluster],
                    "users": [{"name": name, "user": {"token": token}}],
                    "contexts": [
                        {
                            "name": name,
                            "context": {
                                "cluster": cluster["name"],
                                "user": name,
                                "namespace": namespace,
                            },
                        }
                    ],
                    "current-context": name,
                },
                f,
            )

        yield str(restricted_path)
    finally:
        restricted_path.unlink(missing_ok=True)
        subprocess.run(
            ["kubectl", "--kubeconfig", kubeconfig_path, "-n", namespace, "delete", "-f", "-", "--ignore-not-found"],
            input=manifests,
            check=False,
            text=True,
        )


# The RBAC an external-database run needs, split at the two points the diagnostic
# scenarios revoke. Kept in step with `transientWorkloadRBAC` in
# src/internal/app/transient_workload.go, which is what the tool's own refusal names.
POD_READ_RULE = {"apiGroups": [""], "resources": ["pods"], "verbs": ["get", "list"]}
# Where a database runs is settled from the namespace's workloads as well as its pods, so an
# identity that is to reach the point of creating a workload needs to read both.
WORKLOAD_READ_RULE = {"apiGroups": ["apps"], "resources": ["deployments", "statefulsets"], "verbs": ["get", "list"]}
# The database address is read from the environment of a running Infrahub component.
POD_EXEC_RULE = {"apiGroups": [""], "resources": ["pods/exec"], "verbs": ["create"]}
TRANSIENT_WRITE_RULES = [
    {"apiGroups": ["batch"], "resources": ["jobs"], "verbs": ["create", "get", "delete"]},
    {"apiGroups": [""], "resources": ["pods"], "verbs": ["get", "list", "watch", "delete"]},
    {"apiGroups": [""], "resources": ["pods/exec"], "verbs": ["create"]},
    {"apiGroups": [""], "resources": ["secrets"], "verbs": ["create", "list", "delete"]},
]

# What a run inside the namespace needs when it takes direct mode (FR-032): the permissions
# of a backup whose databases run inside the deployment, and nothing on jobs or secrets.
# Kept in step with "Permissions in this mode" in docs/docs/backup/external-databases.mdx.
# `kubectl scale` patches the `scale` subresource, which `patch` on the workload itself
# does not cover, and workload resolution lists deployments and statefulsets by label.
DIRECT_MODE_RULES = [
    {"apiGroups": [""], "resources": ["pods"], "verbs": ["get", "list"]},
    {"apiGroups": [""], "resources": ["pods/exec"], "verbs": ["create"]},
    {"apiGroups": [""], "resources": ["pods/log"], "verbs": ["get"]},
    {"apiGroups": ["apps"], "resources": ["deployments", "statefulsets"], "verbs": ["get", "list", "patch"]},
    {"apiGroups": ["apps"], "resources": ["deployments/scale", "statefulsets/scale"], "verbs": ["get", "patch"]},
]

# The line a run logs when it takes direct mode, from `decideDirectMode` in
# src/internal/app/external_local.go, the one it logs when it takes the transient Job path
# instead, and the one it logs when it creates a transient workload.
DIRECT_MODE_LINE = "Running the external database tools in this process, with no transient Job"
JOB_PATH_LINE = "Running the external database tools in a transient Job"
TRANSIENT_WORKLOAD_LINE = "Creating a transient"


async def transient_jobs(kubeconfig_path: str, namespace: str) -> list[str]:
    """Every Job in the namespace carrying the transient marker, as `job/name`."""
    result = subprocess.run(
        [
            "kubectl",
            "--kubeconfig",
            kubeconfig_path,
            "-n",
            namespace,
            "get",
            "jobs",
            "-l",
            TRANSIENT_LABEL_MARKER,
            "-o",
            "name",
        ],
        check=True,
        capture_output=True,
        text=True,
    )
    return [line.replace("job.batch/", "job/") for line in result.stdout.split() if line]


async def transient_objects_after_run(kubeconfig_path: str, namespace: str, timeout: float = 60) -> list[str]:
    """`transient_objects` once the cluster has had `timeout` seconds to finish removing them.

    The tool deletes the Job without waiting (ADR 0018), and the garbage collector then
    deletes the Job's pod and Secret. Those can still be listed for a few seconds after the
    run returns, which is the cleanup working, not an object surviving it. What FR-011 and
    FR-023 forbid is an object the cluster does not remove, so this waits for the removal
    and returns what is still there at the deadline.
    """
    deadline = time.monotonic() + timeout
    found = await transient_objects(kubeconfig_path, namespace)
    while found and time.monotonic() < deadline:
        await asyncio.sleep(2)
        found = await transient_objects(kubeconfig_path, namespace)
    return found


async def transient_objects(kubeconfig_path: str, namespace: str) -> list[str]:
    """Every transient external-database object in the namespace, as `kind/name`.

    The suites assert this list is empty after a run: a surviving Job, pod or Secret is
    what FR-011 and FR-023 forbid, and the marker label is how the tool's own reaper finds
    them, so selecting on it is selecting exactly what the tool claims to clean up. The
    workload is a Job that owns its pod and its Secret (ADR 0018), so the Job is listed too.
    """
    api = await kr8s.asyncio.api(kubeconfig=kubeconfig_path)
    found: list[str] = await transient_jobs(kubeconfig_path, namespace)
    # kr8s listings are typed as able to yield raw dicts as well as objects; they only do so
    # when asked for raw output, which these are not, so the dict arm is skipped rather than
    # handled twice.
    async for pod in AsyncPod.list(namespace=namespace, label_selector=TRANSIENT_LABEL_MARKER, api=api):
        if not isinstance(pod, dict):
            found.append(f"pod/{pod.name}")
    async for secret in AsyncSecret.list(namespace=namespace, label_selector=TRANSIENT_LABEL_MARKER, api=api):
        if not isinstance(secret, dict):
            found.append(f"secret/{secret.name}")
    return found


# ---------------------------------------------------------------------------
# Neo4j cluster topologies (stacked on spec 007-external-database-backup)
# ---------------------------------------------------------------------------
# Every other Kubernetes case runs against one Neo4j server. These two topologies run
# Infrahub against a three-server Neo4j cluster instead, built the way Infrahub's own HA
# example builds it (`development/k8s/neo4j-values.yaml` and `development/deploy.tf` in
# opsmill/infrahub): three releases of the Neo4j Helm chart that discover each other
# because they share `neo4j.name`, and a `neo4j-headless-service` release in front of
# them, which Infrahub connects to with the `neo4j` protocol so that the driver routes
# writes to the current leader.
#
#   internal  The cluster runs in the Infrahub namespace and its pods carry
#             `infrahub/service: database`, as the HA example labels them. The tool
#             finds three `database` pods and restores through the cluster flow
#             (restoreNeo4jCluster), which seeds the database on every server.
#   external  The cluster runs in a namespace of its own, so the Infrahub namespace has no
#             `database` workload and the tool takes the external path: it captures with
#             `neo4j-admin database backup --from=<host>:6362` for each host in
#             INFRAHUB_DB_ADDRESS, which here is the headless service, and restores by
#             seeding every member from an object store.
#
# A session declares one topology with INFRAHUB_TESTING_NEO4J_CLUSTER, set to `internal`
# or `external`, and needs nothing else: the fixtures below build the whole topology.
#
# - The `vcluster` fixture (tests/conftest.py) creates three nodes for such a session,
#   and the members keep the chart's anti-affinity, so each runs on a node of its own.
#   `install_neo4j_cluster` checks that they do.
# - The external topology stages restores in the session's Garage container
#   (`s3_server`). The fixture attaches that container to the vcluster's Docker network,
#   so the Neo4j servers reach it at its address on that network while the tool, on the
#   host, reaches it at its published port.
# - The external topology runs the database tools from an infrahub-backup image built
#   from this checkout, unless INFRAHUB_TESTING_EXTERNAL_DB_IMAGE names one already built.

NEO4J_CLUSTER_ENV_VAR = "INFRAHUB_TESTING_NEO4J_CLUSTER"

# The Neo4j version of the cluster. It is the version the infrahub-backup image carries
# its Neo4j tools from (`FROM neo4j:...` in the Dockerfile): the external path refuses
# tools older than the server (FR-006), so a newer cluster would test that refusal instead.
NEO4J_CLUSTER_CHART_VERSION = "2025.10.1"
NEO4J_CLUSTER_SIZE = 3
# `neo4j.name` in every release: the members discover each other by it, and the headless
# service is named after it.
NEO4J_CLUSTER_NAME = "infrahub"
NEO4J_CLUSTER_USERNAME = "neo4j"
NEO4J_CLUSTER_PASSWORD = "admin12345"
# The Secret every release reads the password from. One Secret shared by all three, as in
# the HA example: a password set in the values makes each release create the same
# `<name>-auth` Secret, and the three releases then fight over it.
NEO4J_CLUSTER_AUTH_SECRET = "neo4j-user"
# The namespace the external topology's cluster runs in.
NEO4J_EXTERNAL_CLUSTER_NAMESPACE = "neo4j-cluster"
# Neo4j 2025.x reports no store size, so an external capture needs one (tasks.md T158).
NEO4J_CLUSTER_SCRATCH_SIZE = "2Gi"
# Where in the Garage bucket the external restores stage their seed.
NEO4J_CLUSTER_SEED_PREFIX = "neo4j-cluster-seed"


def neo4j_cluster_topology() -> str:
    """The cluster topology this session declares: `internal`, `external` or ""."""
    return os.environ.get(NEO4J_CLUSTER_ENV_VAR, "").strip().lower()


def requires_neo4j_cluster(topology: str):
    """Skip unless the session declares this Neo4j cluster topology.

    The cluster cases deploy a cluster of their own, which no other session has, so they
    skip by name in every session that does not declare them, including the default
    `pytest -m k8s tests/e2e` run.
    """
    declared = neo4j_cluster_topology()
    reason = (
        f"needs a {topology} Neo4j cluster: set {NEO4J_CLUSTER_ENV_VAR}={topology}. In CI, the "
        "e2e-tests-k8s-neo4j-cluster job runs one session per topology"
    )
    return pytest.mark.skipif(declared != topology, reason=reason)


def neo4j_cluster_headless_address(namespace: str) -> str:
    """The address Infrahub connects to: the headless service in front of every member."""
    return f"{NEO4J_CLUSTER_NAME}-headless.{namespace}.svc.cluster.local:7687"


def neo4j_member_pods() -> list[str]:
    """The pod of each member: release `database-<n>` runs StatefulSet pod `database-<n>-0`."""
    return [f"database-{index}-0" for index in range(NEO4J_CLUSTER_SIZE)]


def neo4j_cypher(kubeconfig_path: str, namespace: str, pod: str, query: str, member_only: bool = False) -> list[str]:
    """Run a Cypher query with cypher-shell in a member pod and return the result rows.

    By default the query goes through the cluster's routing, like Infrahub's own queries.
    With `member_only`, it is a read on that member alone, which shows what that member
    holds rather than what the cluster answers.
    """
    shell = ["cypher-shell", "-u", NEO4J_CLUSTER_USERNAME, "-p", NEO4J_CLUSTER_PASSWORD, "--format", "plain"]
    if member_only:
        shell += ["-a", "bolt://localhost:7687", "--access-mode", "read"]
    result = subprocess.run(
        ["kubectl", "--kubeconfig", kubeconfig_path, "-n", namespace, "exec", pod, "--", *shell, query],
        capture_output=True,
        text=True,
    )
    if result.returncode != 0:
        raise RuntimeError(f"cypher-shell in {namespace}/{pod} failed: {result.stdout}{result.stderr}")
    return [line.strip().strip('"') for line in result.stdout.splitlines()[1:] if line.strip()]


def neo4j_database_status(kubeconfig_path: str, namespace: str) -> list[str]:
    """The status of the default database on every member, as the first member reports it."""
    return neo4j_cypher(
        kubeconfig_path,
        namespace,
        neo4j_member_pods()[0],
        "SHOW DATABASE neo4j YIELD currentStatus RETURN currentStatus",
    )


def wait_for_neo4j_cluster_online(kubeconfig_path: str, namespace: str, timeout: float = 300) -> None:
    """Wait until the default database is online on every member."""
    deadline = time.monotonic() + timeout
    statuses: list[str] | str = []
    while time.monotonic() < deadline:
        try:
            statuses = neo4j_database_status(kubeconfig_path, namespace)
        except RuntimeError as exc:
            statuses = str(exc)
        if len(statuses) == NEO4J_CLUSTER_SIZE and all(status == "online" for status in statuses):
            return
        time.sleep(5)
    raise RuntimeError(f"the Neo4j cluster in {namespace} did not bring the database online: {statuses}")


def neo4j_member_nodes(kubeconfig_path: str, namespace: str) -> dict[str, str]:
    """The Kubernetes node each member pod runs on."""
    nodes = {}
    for pod in neo4j_member_pods():
        result = subprocess.run(
            [
                "kubectl",
                "--kubeconfig",
                kubeconfig_path,
                "-n",
                namespace,
                "get",
                "pod",
                pod,
                "-o",
                "jsonpath={.spec.nodeName}",
            ],
            capture_output=True,
            text=True,
            check=True,
        )
        nodes[pod] = result.stdout.strip()
    return nodes


def neo4j_member_graph_sizes(kubeconfig_path: str, namespace: str) -> dict[str, str]:
    """The number of nodes and relationships each member holds, read on that member alone."""

    def count(pod: str, query: str) -> str:
        return ", ".join(neo4j_cypher(kubeconfig_path, namespace, pod, query, member_only=True))

    sizes = {}
    for pod in neo4j_member_pods():
        nodes = count(pod, "MATCH (n) RETURN count(n)")
        relationships = count(pod, "MATCH ()-[r]->() RETURN count(r)")
        sizes[pod] = f"{nodes} nodes, {relationships} relationships"
    return sizes


def assert_members_hold_the_same_graph(kubeconfig_path: str, namespace: str, timeout: float = 120) -> None:
    """Check that every member holds the same graph after a restore.

    Infrahub reads through one member at a time, so a member left with the data from
    before the restore would not show in a check made through Infrahub. Each member is
    read on its own instead. The members can lag the leader for a moment, so the check
    waits for them to agree.
    """
    deadline = time.monotonic() + timeout
    sizes = neo4j_member_graph_sizes(kubeconfig_path, namespace)
    while len(set(sizes.values())) != 1 and time.monotonic() < deadline:
        time.sleep(5)
        sizes = neo4j_member_graph_sizes(kubeconfig_path, namespace)
    assert len(set(sizes.values())) == 1, f"the members hold different graphs after the restore: {sizes}"


def _neo4j_cluster_values(labels: dict[str, str], env: dict[str, str]) -> dict:
    """Helm values for one member of the cluster.

    The resource figures keep three members and an Infrahub deployment inside one CI
    runner. The chart's anti-affinity stays on, as in production: the session's vcluster
    has three nodes, and each member must run on a node of its own. The backup listener is
    bound to every interface so that a client outside the pod can take an online backup,
    which is what the external path does: it runs `neo4j-admin database backup
    --from=<host>:6362` for each host in INFRAHUB_DB_ADDRESS, and the headless service
    resolves straight to the member pods.
    """
    config = {
        "server.memory.heap.initial_size": "512m",
        "server.memory.heap.max_size": "512m",
        "server.memory.pagecache.size": "256m",
        "server.backup.enabled": "true",
        "server.backup.listen_address": "0.0.0.0:6362",
        "dbms.security.auth_minimum_password_length": "4",
    }

    return {
        "neo4j": {
            "name": NEO4J_CLUSTER_NAME,
            "minimumClusterSize": NEO4J_CLUSTER_SIZE,
            "edition": "enterprise",
            "acceptLicenseAgreement": "yes",
            "passwordFromSecret": NEO4J_CLUSTER_AUTH_SECRET,
            "labels": labels,
            "resources": {"cpu": "500m", "memory": "2Gi"},
        },
        "config": config,
        "env": env,
        "podSpec": {"podAntiAffinity": True},
        "logInitialPassword": False,
        "volumes": {
            "data": {
                "mode": "defaultStorageClass",
                "defaultStorageClass": {"requests": {"storage": "2Gi"}},
            }
        },
        # The chart's own load balancer service is not part of the HA example.
        "services": {"neo4j": {"enabled": False}},
    }


def install_neo4j_cluster(
    kubeconfig_path: str,
    namespace: str,
    tmp_path_factory,
    labels: dict[str, str] | None = None,
    env: dict[str, str] | None = None,
) -> None:
    """Install a three-member Neo4j Enterprise cluster and wait until it serves requests.

    The members are installed at the same time, without `helm --wait`: a member becomes
    ready only once the cluster has formed, so waiting on the first install alone would
    wait for members that were never started.
    """

    def kubectl(*args: str, stdin: str | None = None) -> None:
        subprocess.run(
            ["kubectl", "--kubeconfig", kubeconfig_path, "-n", namespace, *args], input=stdin, text=True, check=True
        )

    subprocess.run(
        ["kubectl", "--kubeconfig", kubeconfig_path, "create", "namespace", namespace],
        check=False,
        capture_output=True,
    )
    kubectl(
        "create",
        "secret",
        "generic",
        NEO4J_CLUSTER_AUTH_SECRET,
        f"--from-literal=NEO4J_AUTH={NEO4J_CLUSTER_USERNAME}/{NEO4J_CLUSTER_PASSWORD}",
    )

    values_dir = tmp_path_factory.mktemp(f"neo4j-cluster-{namespace}")
    installs: list[tuple[str, subprocess.Popen]] = []
    for index in range(NEO4J_CLUSTER_SIZE):
        release = f"database-{index}"
        values_path = values_dir / f"{release}.yaml"
        values = _neo4j_cluster_values(labels or {}, env or {})
        values_path.write_text(yaml.dump(values))
        installs.append(
            (
                release,
                subprocess.Popen(
                    [
                        "helm",
                        "upgrade",
                        "--install",
                        release,
                        "neo4j",
                        "--repo",
                        "https://helm.neo4j.com/neo4j",
                        "--version",
                        NEO4J_CLUSTER_CHART_VERSION,
                        "-n",
                        namespace,
                        "-f",
                        str(values_path),
                        "--kubeconfig",
                        kubeconfig_path,
                    ]
                ),
            )
        )
    failed = [release for release, install in installs if install.wait() != 0]
    if failed:
        raise RuntimeError(f"helm could not install the Neo4j cluster members {failed}")

    subprocess.run(
        [
            "helm",
            "upgrade",
            "--install",
            "database-service",
            "neo4j-headless-service",
            "--repo",
            "https://helm.neo4j.com/neo4j",
            "--version",
            NEO4J_CLUSTER_CHART_VERSION,
            "-n",
            namespace,
            "--set",
            f"neo4j.name={NEO4J_CLUSTER_NAME}",
            "--kubeconfig",
            kubeconfig_path,
        ],
        check=True,
    )

    for index in range(NEO4J_CLUSTER_SIZE):
        kubectl("rollout", "status", f"statefulset/database-{index}", "--timeout=10m")

    # Ready means each member answers Bolt, not that the default database is online on all
    # three, which is what Infrahub's first write needs.
    wait_for_neo4j_cluster_online(kubeconfig_path, namespace)

    # A cluster whose members share a node is not the topology these cases claim to test.
    placement = neo4j_member_nodes(kubeconfig_path, namespace)
    if len(set(placement.values())) != NEO4J_CLUSTER_SIZE:
        raise RuntimeError(f"the Neo4j members in {namespace} do not run on distinct nodes: {placement}")


def _neo4j_cluster_infrahub_overlay(address: str) -> dict:
    """Helm values that point Infrahub at a Neo4j cluster instead of the chart's own Neo4j.

    The same keys as the external overlay (see `_external_values_overlay`), plus the
    `neo4j` protocol, which the HA example sets so that the driver routes each query to
    the member that can serve it.
    """
    env = {
        "INFRAHUB_DB_ADDRESS": address.rsplit(":", 1)[0],
        "INFRAHUB_DB_PORT": address.rsplit(":", 1)[1],
        "INFRAHUB_DB_PROTOCOL": "neo4j",
        "INFRAHUB_DB_USERNAME": NEO4J_CLUSTER_USERNAME,
        "INFRAHUB_DB_PASSWORD": NEO4J_CLUSTER_PASSWORD,
    }
    overlay: dict = {"neo4j": {"enabled": False}}
    for component in ("infrahubServer", "infrahubTaskWorker"):
        overlay[component] = {component: {"env": dict(env)}}
    return overlay


@pytest.fixture(scope="session")
async def infrahub_k8s_neo4j_cluster(vcluster: dict, tmp_path_factory) -> AsyncGenerator[dict, None]:
    """Infrahub with a three-member Neo4j cluster in its own namespace (internal topology)."""
    if neo4j_cluster_topology() != "internal":
        pytest.skip(f"needs {NEO4J_CLUSTER_ENV_VAR}=internal")

    kubeconfig_path = vcluster["kubeconfig_path"]
    namespace = "infrahub-cluster"

    install_neo4j_cluster(
        kubeconfig_path,
        namespace,
        tmp_path_factory,
        labels={"infrahub/service": "database"},
    )
    overlay = _neo4j_cluster_infrahub_overlay(neo4j_cluster_headless_address(namespace))
    _helm_install_infrahub(kubeconfig_path, namespace, _infrahub_values_path(tmp_path_factory, overlay=overlay))

    async with portforward_infrahub(kubeconfig_path, namespace):
        pass  # health check happens inside the context manager

    yield {
        "namespace": namespace,
        "kubeconfig_path": kubeconfig_path,
        "token": INFRAHUB_ADMIN_TOKEN,
    }


@pytest.fixture(scope="session")
def neo4j_cluster_seed_store(vcluster: dict, s3_server: dict) -> Generator[dict, None, None]:
    """The session's Garage container, reachable from the Neo4j servers and from the tool.

    The container is attached to the vcluster's Docker network for the session, and
    detached again before the vcluster is deleted, which cannot remove a network that a
    container is still attached to.
    """
    network = f"vcluster.{vcluster['cluster_name']}"
    container = s3_server["container_id"]
    subprocess.run(["docker", "network", "connect", network, container], check=True)
    try:
        address = subprocess.run(
            [
                "docker",
                "inspect",
                container,
                "--format",
                f'{{{{(index .NetworkSettings.Networks "{network}").IPAddress}}}}',
            ],
            capture_output=True,
            text=True,
            check=True,
        ).stdout.strip()
        yield {
            "bucket": s3_server["bucket"],
            "prefix": NEO4J_CLUSTER_SEED_PREFIX,
            "region": s3_server["region"],
            "access_key": s3_server["access_key"],
            "secret_key": s3_server["secret_key"],
            # The tool runs on the host and uploads through the published port.
            "endpoint": s3_server["endpoint"],
            # The Neo4j servers fetch the seed through the vcluster's network.
            "server_endpoint": f"http://{address}:3900",
        }
    finally:
        subprocess.run(["docker", "network", "disconnect", network, container], check=False)


@pytest.fixture(scope="session")
def neo4j_cluster_backup_image(vcluster: dict) -> Generator[str, None, None]:
    """The infrahub-backup image the external path runs its tools from, loaded into the vcluster.

    A session that already built the image names it in INFRAHUB_TESTING_EXTERNAL_DB_IMAGE.
    Otherwise the image is built from this checkout and removed at the end of the session.
    """
    image = os.environ.get(EXTERNAL_DB_IMAGE_ENV_VAR, "").strip()
    built = not image
    if built:
        image = f"infrahub-backup:e2e-{uuid.uuid4().hex[:12]}"
        subprocess.run(["docker", "build", "-t", image, str(PROJECT_ROOT)], check=True)
    _require_pinned_tag(image)
    try:
        load_image_into_vcluster(image, vcluster["cluster_name"])
        yield image
    finally:
        if built:
            subprocess.run(["docker", "image", "rm", "-f", image], check=False, capture_output=True)


@pytest.fixture(scope="session")
async def infrahub_k8s_external_neo4j_cluster(
    vcluster: dict,
    neo4j_cluster_seed_store: dict,
    neo4j_cluster_backup_image: str,
    tmp_path_factory,
) -> AsyncGenerator[dict, None]:
    """Infrahub pointed at a three-member Neo4j cluster outside its namespace.

    The cluster's servers read a restore's seed from the object store themselves, so they
    carry the store's credentials and endpoint.
    """
    if neo4j_cluster_topology() != "external":
        pytest.skip(f"needs {NEO4J_CLUSTER_ENV_VAR}=external")

    kubeconfig_path = vcluster["kubeconfig_path"]
    namespace = "infrahub-external-cluster"
    seed_store = neo4j_cluster_seed_store
    address = neo4j_cluster_headless_address(NEO4J_EXTERNAL_CLUSTER_NAMESPACE)
    topology = {
        "services": {SERVICE_NEO4J},
        "neo4j_address": address,
        "neo4j_username": NEO4J_CLUSTER_USERNAME,
        "neo4j_password": NEO4J_CLUSTER_PASSWORD,
        "neo4j_edition": "enterprise",
    }

    install_neo4j_cluster(
        kubeconfig_path,
        NEO4J_EXTERNAL_CLUSTER_NAMESPACE,
        tmp_path_factory,
        env={
            "AWS_ACCESS_KEY_ID": seed_store["access_key"],
            "AWS_SECRET_ACCESS_KEY": seed_store["secret_key"],
            "AWS_REGION": seed_store["region"],
            "AWS_ENDPOINT_URL_S3": seed_store["server_endpoint"],
        },
    )
    overlay = _neo4j_cluster_infrahub_overlay(address)
    _helm_install_infrahub(kubeconfig_path, namespace, _infrahub_values_path(tmp_path_factory, overlay=overlay))

    async with portforward_infrahub(kubeconfig_path, namespace):
        pass  # health check happens inside the context manager

    yield {
        "namespace": namespace,
        "kubeconfig_path": kubeconfig_path,
        "token": INFRAHUB_ADMIN_TOKEN,
        "external_services": {SERVICE_NEO4J},
        "external_db_image": neo4j_cluster_backup_image,
        # The cluster serves no TLS, which the tool accepts only when told to (FR-029).
        "topology_args": ["--external-db-insecure-tls", "--external-db-scratch-size", NEO4J_CLUSTER_SCRATCH_SIZE],
        "topology": topology,
        "seed_store": seed_store,
        "neo4j_namespace": NEO4J_EXTERNAL_CLUSTER_NAMESPACE,
    }


# The test module of each topology. A session that declares a topology and passes none of
# its cases has tested nothing, and would otherwise report success.
NEO4J_CLUSTER_TEST_MODULES = {
    "internal": "test_k8s_neo4j_cluster.py",
    "external": "test_k8s_external_neo4j_cluster.py",
}
_passed_test_files: set[str] = set()


def pytest_runtest_logreport(report: pytest.TestReport) -> None:
    if report.when == "call" and report.passed:
        _passed_test_files.add(Path(report.fspath).name)


def pytest_sessionfinish(session: pytest.Session, exitstatus: int) -> None:
    topology = neo4j_cluster_topology()
    module = NEO4J_CLUSTER_TEST_MODULES.get(topology)
    if not topology or exitstatus != 0:
        return
    if module is None:
        print(
            f"\n{NEO4J_CLUSTER_ENV_VAR}={topology} names no topology: use one of {sorted(NEO4J_CLUSTER_TEST_MODULES)}"
        )
        session.exitstatus = pytest.ExitCode.USAGE_ERROR
    elif module not in _passed_test_files:
        print(f"\n{NEO4J_CLUSTER_ENV_VAR}={topology} is set, but no case in {module} passed")
        session.exitstatus = pytest.ExitCode.TESTS_FAILED
