"""E2E tests: Kubernetes (vcluster) + troubleshooting bundle collection.

Covers spec 003-collect-tool US1 (quickstart scenario 5): archive integrity,
manifest schema conformance, one log file per replica (task-worker scaled to
2), previous-container logs for an induced restart, bundle layout per
contracts/bundle-layout.md, and read-only collection (SC-003: zero pod
restarts or scale events caused by the run).
"""

import json
import re
import subprocess
import tarfile
import time
from datetime import datetime
from pathlib import Path

import pytest

from tests.helpers.utils import run_collect

# Full planned collector set (SC-005: the manifest accounts for every planned
# collector, present or not) — logs per canonical service plus the parity
# diagnostics and metrics collectors.
LOG_SERVICES = [
    "infrahub-server",
    "task-worker",
    "database",
    "message-queue",
    "cache",
    "task-manager",
    "task-manager-db",
    "task-manager-background-svc",
]
EXPECTED_COLLECTORS = {f"logs/{service}" for service in LOG_SERVICES} | {
    "database-logs",
    "message-queue-status",
    "cache-status",
    "task-worker-state",
    "task-manager-state",
    "server-info",
    "metrics",
}

# Bundle directory owned by each non-log collector (contracts/bundle-layout.md).
COLLECTOR_DIRS = {
    "database-logs": "database",
    "message-queue-status": "message-queue",
    "cache-status": "cache",
    "task-worker-state": "task-worker",
    "task-manager-state": "task-manager",
    "server-info": "server",
    "metrics": "metrics",
}

ALLOWED_TOP_LEVEL = {"bundle_information.json", "logs", "benchmark"} | set(COLLECTOR_DIRS.values())

COLLECT_ID_RE = re.compile(r"^[0-9]{8}_[0-9]{6}$")


# ---------------------------------------------------------------------------
# kubectl helpers
# ---------------------------------------------------------------------------
def _kubectl(kubeconfig: str, *args: str) -> str:
    """Run kubectl against the test cluster and return stdout."""
    result = subprocess.run(
        ["kubectl", "--kubeconfig", kubeconfig, *args],
        capture_output=True,
        text=True,
    )
    if result.returncode != 0:
        raise RuntimeError(f"kubectl {' '.join(args)} failed (exit {result.returncode}): {result.stderr}")
    return result.stdout


def _service_pods(kubeconfig: str, namespace: str, service: str) -> list[dict]:
    """Return the pod objects backing an Infrahub service.

    Mirrors the collect backend's resolution order: the infrahub/service label
    first, then a pod-name substring match (the Helm chart labels some pods
    with a different value than the canonical service name).
    """
    output = _kubectl(
        kubeconfig,
        "get",
        "pods",
        "-n",
        namespace,
        "-l",
        f"infrahub/service={service}",
        "-o",
        "json",
    )
    items = json.loads(output)["items"]
    if items:
        return items
    all_pods = json.loads(_kubectl(kubeconfig, "get", "pods", "-n", namespace, "-o", "json"))["items"]
    return [pod for pod in all_pods if service in pod["metadata"]["name"]]


def _find_task_worker_deployment(kubeconfig: str, namespace: str) -> str:
    """Resolve the task-worker deployment name (label selector, then name match)."""
    output = _kubectl(
        kubeconfig,
        "get",
        "deployments",
        "-n",
        namespace,
        "-l",
        "infrahub/service=task-worker",
        "-o",
        "jsonpath={.items[*].metadata.name}",
    )
    names = output.split()
    if not names:
        output = _kubectl(kubeconfig, "get", "deployments", "-n", namespace, "-o", "jsonpath={.items[*].metadata.name}")
        names = [name for name in output.split() if "task-worker" in name]
    assert names, "No task-worker deployment found"
    return names[0]


def _scale_deployment(kubeconfig: str, namespace: str, deployment: str, replicas: int) -> None:
    """Scale a deployment and wait for the rollout to settle."""
    _kubectl(kubeconfig, "scale", "deployment", deployment, "-n", namespace, f"--replicas={replicas}")
    _kubectl(kubeconfig, "rollout", "status", f"deployment/{deployment}", "-n", namespace, "--timeout=300s")


def _container_restart_count(kubeconfig: str, namespace: str, pod: str, container: str) -> int:
    output = _kubectl(kubeconfig, "get", "pod", pod, "-n", namespace, "-o", "json")
    for status in json.loads(output)["status"].get("containerStatuses", []):
        if status["name"] == container:
            return status["restartCount"]
    raise RuntimeError(f"Container {container} not found in pod {pod}")


def _induce_container_restart(kubeconfig: str, namespace: str, pod: str, container: str) -> None:
    """Restart a container in place (same pod) so previous logs become available.

    Signals PID 1 inside the container; the pod is NOT deleted, so the
    container's restartCount increments and `kubectl logs --previous` works.
    """
    baseline = _container_restart_count(kubeconfig, namespace, pod, container)
    for signal_name in ("TERM", "INT", "KILL"):
        subprocess.run(
            [
                "kubectl",
                "--kubeconfig",
                kubeconfig,
                "exec",
                "-n",
                namespace,
                pod,
                "-c",
                container,
                "--",
                "sh",
                "-c",
                f"kill -{signal_name} 1",
            ],
            capture_output=True,
            text=True,
        )
        deadline = time.time() + 90
        while time.time() < deadline:
            if _container_restart_count(kubeconfig, namespace, pod, container) > baseline:
                return
            time.sleep(3)
    pytest.fail(f"Could not induce a restart of container {container} in pod {pod}")


def _wait_pods_ready(kubeconfig: str, namespace: str, service: str, timeout: int = 300) -> None:
    """Wait until every pod of a service is Ready."""
    _kubectl(
        kubeconfig,
        "wait",
        "--for=condition=Ready",
        "pod",
        "-n",
        namespace,
        "-l",
        f"infrahub/service={service}",
        f"--timeout={timeout}s",
    )


def _cluster_state(kubeconfig: str, namespace: str) -> dict:
    """Snapshot per-container restart counts, pod identities, and workload replicas (SC-003)."""
    pods = json.loads(_kubectl(kubeconfig, "get", "pods", "-n", namespace, "-o", "json"))["items"]
    restarts = {}
    pod_uids = set()
    for pod in pods:
        name = pod["metadata"]["name"]
        pod_uids.add(pod["metadata"]["uid"])
        for status in pod.get("status", {}).get("containerStatuses", []):
            restarts[f"{name}/{status['name']}"] = status["restartCount"]
    replicas = {}
    for kind in ("deployment", "statefulset"):
        items = json.loads(_kubectl(kubeconfig, "get", kind, "-n", namespace, "-o", "json"))["items"]
        for item in items:
            replicas[f"{kind}/{item['metadata']['name']}"] = item["spec"].get("replicas")
    return {"restarts": restarts, "pod_uids": pod_uids, "replicas": replicas}


# ---------------------------------------------------------------------------
# Bundle helpers
# ---------------------------------------------------------------------------
def _find_bundle(output_dir: Path) -> Path:
    bundles = sorted(output_dir.glob("support_bundle_*.tar.gz"))
    assert bundles, f"No support bundle found in {output_dir}"
    return bundles[-1]


def _expected_log_files(pods: list[dict]) -> set[str]:
    """Derive expected per-replica log filenames (data-model.md naming rules)."""
    expected: set[str] = set()
    for pod in pods:
        name = pod["metadata"]["name"]
        containers = [status["name"] for status in pod["status"].get("containerStatuses", [])]
        if len(containers) > 1:
            expected.update(f"{name}_{container}.log" for container in containers)
        else:
            expected.add(f"{name}.log")
    return expected


def _previous_log_name(pod: dict, container: str) -> str:
    """Derive the .previous.log filename for one restarted container."""
    name = pod["metadata"]["name"]
    containers = pod["status"].get("containerStatuses", [])
    base = f"{name}_{container}" if len(containers) > 1 else name
    return f"{base}.previous.log"


def _validate_manifest_schema(manifest: dict) -> None:
    """Assert manifest conformance with contracts/manifest.schema.json.

    jsonschema is not a test dependency; the schema's constraints are asserted
    manually (required fields, types, collect_id pattern, created_at format,
    environment/status enums, reason required on failed/skipped).
    """
    required = {
        "manifest_version": int,
        "collect_id": str,
        "created_at": str,
        "tool_version": str,
        "infrahub_version": str,
        "environment": str,
        "log_lines": int,
        "collectors": list,
    }
    for field, expected_type in required.items():
        assert field in manifest, f"Manifest missing required field {field!r}"
        value = manifest[field]
        assert isinstance(value, expected_type) and not isinstance(value, bool), (
            f"Manifest field {field!r} must be {expected_type.__name__}, got {type(value).__name__}"
        )

    assert COLLECT_ID_RE.match(manifest["collect_id"]), f"collect_id {manifest['collect_id']!r} violates pattern"
    datetime.fromisoformat(manifest["created_at"])  # RFC3339 date-time
    assert manifest["environment"] in ("docker", "kubernetes"), f"Invalid environment {manifest['environment']!r}"
    assert manifest["log_lines"] >= 1, "log_lines must be >= 1"
    assert manifest["collectors"], "collectors must have at least one entry"

    for entry in manifest["collectors"]:
        assert isinstance(entry, dict), f"Collector entry must be an object: {entry!r}"
        assert isinstance(entry.get("name"), str) and entry["name"], f"Collector entry missing name: {entry!r}"
        assert entry.get("status") in ("success", "failed", "skipped"), (
            f"Collector {entry.get('name')!r} has invalid status {entry.get('status')!r}"
        )
        if entry["status"] in ("failed", "skipped"):
            assert isinstance(entry.get("reason"), str) and entry["reason"], (
                f"Collector {entry['name']!r} is {entry['status']} but has no reason"
            )


# ---------------------------------------------------------------------------
# Test
# ---------------------------------------------------------------------------
@pytest.mark.e2e
@pytest.mark.k8s
async def test_collect_k8s_bundle(infrahub_k8s, collect_binary, tmp_path):
    """K8s: full collect — archive, manifest, per-replica + previous logs, SC-003."""
    namespace = infrahub_k8s["namespace"]
    kubeconfig = infrahub_k8s["kubeconfig_path"]
    output_dir = tmp_path / "bundles"
    env = {"KUBECONFIG": kubeconfig}

    deployment = _find_task_worker_deployment(kubeconfig, namespace)
    original_replicas = int(
        _kubectl(kubeconfig, "get", "deployment", deployment, "-n", namespace, "-o", "jsonpath={.spec.replicas}")
    )

    try:
        # 1. Scale task-worker to >= 2 replicas so per-replica logs are observable
        if original_replicas < 2:
            _scale_deployment(kubeconfig, namespace, deployment, 2)
        _wait_pods_ready(kubeconfig, namespace, "task-worker")

        worker_pods = _service_pods(kubeconfig, namespace, "task-worker")
        assert len(worker_pods) >= 2, f"Expected >= 2 task-worker pods, found {len(worker_pods)}"

        # 2. Induce an in-place container restart so previous logs exist
        restart_pod = worker_pods[0]
        restart_container = restart_pod["status"]["containerStatuses"][0]["name"]
        _induce_container_restart(kubeconfig, namespace, restart_pod["metadata"]["name"], restart_container)
        _wait_pods_ready(kubeconfig, namespace, "task-worker")

        # Refresh pod view after the induced restart (same pods, bumped restartCount)
        worker_pods = _service_pods(kubeconfig, namespace, "task-worker")
        restart_pod = next(pod for pod in worker_pods if pod["metadata"]["name"] == restart_pod["metadata"]["name"])
        server_pods = _service_pods(kubeconfig, namespace, "infrahub-server")
        assert server_pods, "No infrahub-server pods found"

        # 3. SC-003 baseline: restart counts, pod identities, workload replicas
        before = _cluster_state(kubeconfig, namespace)

        # 4. Run the collect
        run_collect(
            collect_binary,
            ["--k8s-namespace", namespace, "--output-dir", str(output_dir), "create"],
            env=env,
        )

        # 5. SC-003: the run caused zero restarts, pod replacements, or scale events
        after = _cluster_state(kubeconfig, namespace)
        assert after["restarts"] == before["restarts"], "Collect run changed container restart counts"
        assert after["pod_uids"] == before["pod_uids"], "Collect run replaced pods"
        assert after["replicas"] == before["replicas"], "Collect run changed workload replica counts"

        # 6. Archive integrity: valid tar.gz, everything under bundle/
        bundle_path = _find_bundle(output_dir)
        with tarfile.open(bundle_path, "r:gz") as tar:
            members = tar.getmembers()
            for member in members:
                assert member.name == "bundle" or member.name.startswith("bundle/"), (
                    f"Archive member outside bundle/: {member.name}"
                )
                if member.isfile():
                    extracted = tar.extractfile(member)
                    assert extracted is not None
                    extracted.read()  # full decompression validates integrity
            file_names = {member.name for member in members if member.isfile()}
            manifest_file = tar.extractfile("bundle/bundle_information.json")
            assert manifest_file is not None, "bundle_information.json missing from archive"
            manifest = json.loads(manifest_file.read())

        # 7. Manifest conforms to contracts/manifest.schema.json
        _validate_manifest_schema(manifest)
        assert manifest["environment"] == "kubernetes"
        assert f"support_bundle_{manifest['collect_id']}.tar.gz" == bundle_path.name

        # SC-005: one manifest entry per planned collector, no duplicates
        names = [entry["name"] for entry in manifest["collectors"]]
        assert len(names) == len(set(names)), f"Duplicate collector entries: {names}"
        assert set(names) == EXPECTED_COLLECTORS, (
            f"Manifest collector set mismatch: missing {EXPECTED_COLLECTORS - set(names)}, "
            f"unexpected {set(names) - EXPECTED_COLLECTORS}"
        )
        statuses = {entry["name"]: entry["status"] for entry in manifest["collectors"]}
        assert statuses["logs/infrahub-server"] == "success"
        assert statuses["logs/task-worker"] == "success"

        # 8. Layout per contracts/bundle-layout.md
        top_level = {name.split("/")[1] for name in file_names if name.count("/") >= 1}
        assert top_level <= ALLOWED_TOP_LEVEL, f"Unexpected top-level bundle entries: {top_level - ALLOWED_TOP_LEVEL}"
        for collector, directory in COLLECTOR_DIRS.items():
            if statuses[collector] == "success":
                assert any(name.startswith(f"bundle/{directory}/") for name in file_names), (
                    f"Collector {collector} succeeded but bundle/{directory}/ is empty"
                )
        for service in LOG_SERVICES:
            if statuses[f"logs/{service}"] == "success":
                assert any(name.startswith(f"bundle/logs/{service}/") for name in file_names), (
                    f"logs/{service} succeeded but bundle/logs/{service}/ is empty"
                )

        # 9. One log file per replica for the multi-replica service (FR-002)
        worker_logs = {
            name.removeprefix("bundle/logs/task-worker/")
            for name in file_names
            if name.startswith("bundle/logs/task-worker/")
        }
        expected_worker_logs = _expected_log_files(worker_pods)
        assert expected_worker_logs <= worker_logs, (
            f"Missing per-replica task-worker logs: {expected_worker_logs - worker_logs}"
        )

        server_logs = {
            name.removeprefix("bundle/logs/infrahub-server/")
            for name in file_names
            if name.startswith("bundle/logs/infrahub-server/")
        }
        assert _expected_log_files(server_pods) <= server_logs, "Missing per-replica infrahub-server logs"

        # 10. Previous-container log for the induced restart (FR-003)
        previous_log = _previous_log_name(restart_pod, restart_container)
        assert previous_log in worker_logs, (
            f"Expected {previous_log} for the restarted container, found: {sorted(worker_logs)}"
        )
    finally:
        # Leave the shared session cluster as we found it
        _scale_deployment(kubeconfig, namespace, deployment, original_replicas)
