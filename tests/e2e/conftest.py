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
from kr8s.asyncio.objects import Secret as AsyncSecret
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
    if not vcluster:
        return

    # Both deployment fixtures yield the same shape and live in different namespaces, and
    # an external-topology test uses only the second, so dump whichever the test asked for.
    for fixture in ("infrahub_k8s", "infrahub_k8s_external"):
        deployment = request.getfixturevalue(fixture) if fixture in request.fixturenames else None
        if deployment:
            _dump_namespace_logs(vcluster["kubeconfig_path"], deployment["namespace"])


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
        .with_command("-c 'mkdir -p /data/backups && minio server /data --console-address :9001'")
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
    service = await AsyncService.get("infrahub-infrahub-server", namespace=namespace, api=kr8s_api)
    async with service.portforward(remote_port=8000, local_port="auto") as local_port:
        url = f"http://localhost:{local_port}"
        await wait_for_http(f"{url}/api/config", timeout=300.0, interval=5.0)
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
# and the external suites skip, by name, when it is absent. Provisioning it in CI is
# tasks.md T072, which is still open.
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

EXTERNAL_DB_ENV_VAR = "INFRAHUB_TESTING_EXTERNAL_DB"
EXTERNAL_HELM_VALUES_ENV_VAR = "INFRAHUB_TESTING_EXTERNAL_HELM_VALUES"

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
            f"{EXTERNAL_DB_ENV_VAR}={','.join(services)}. No CI topology provides one yet "
            "(specs/archive/007-external-database-backup/tasks.md T072)"
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
        overlay["prefect-server"] = {"postgresql": {"enabled": False}}
        user = topology["postgres_username"] or "postgres"
        password = topology["postgres_password"] or "prefect"
        address = topology["postgres_address"]
        env["PREFECT_API_DATABASE_CONNECTION_URL"] = f"postgresql+asyncpg://{user}:{password}@{address}/prefect"

    # Both components that read the database get the same environment, each under its own
    # name twice. The chart has no single place to say this once; see the docstring.
    if env:
        for component in ("infrahubServer", "infrahubTaskWorker"):
            overlay[component] = {component: {"env": dict(env)}}

    return overlay


@pytest.fixture(scope="session")
async def infrahub_k8s_external(
    vcluster: dict,
    external_topology: dict,
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
TRANSIENT_WRITE_RULES = [
    {"apiGroups": [""], "resources": ["pods", "secrets"], "verbs": ["create", "get", "list", "delete"]},
    {"apiGroups": [""], "resources": ["pods/exec"], "verbs": ["create"]},
    {"apiGroups": [""], "resources": ["pods/log"], "verbs": ["get"]},
]


async def transient_objects(kubeconfig_path: str, namespace: str) -> list[str]:
    """Every transient external-database object in the namespace, as `kind/name`.

    The suites assert this list is empty after a run: a surviving pod or Secret is what
    FR-011 and FR-023 forbid, and the marker label is how the tool's own reaper finds
    them, so selecting on it is selecting exactly what the tool claims to clean up.
    """
    api = await kr8s.asyncio.api(kubeconfig=kubeconfig_path)
    found: list[str] = []
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
