"""E2E tests: Kubernetes (vcluster) + the traces collector of infrahub-collect (INFP-744).

T023: installs the infrahub-observability chart (Tempo only) next to
Infrahub, sends a trace to its Tempo, and checks that the collector finds the
service by its label app.kubernetes.io/name=tempo, reads it through a
port-forward to port 3100, and writes the trace. A second case runs the
collector with a service account that cannot create pods/portforward and
expects a skipped entry that names the permission.

The Infrahub pods do not send traces in this test: turning tracing on would
restart them, and the session shares one Infrahub deployment.
"""

import os
import re
import subprocess
from collections.abc import Iterator
from contextlib import contextmanager
from pathlib import Path

import pytest
import yaml

from tests.helpers.bundle import find_bundle, read_bundle_archive, validate_manifest_schema
from tests.helpers.traces import (
    bundle_trace_files,
    post_trace,
    synthetic_trace,
    trace_id_of,
    traces_entry,
    wait_for_traces,
)
from tests.helpers.utils import run_collect

FIXTURES_DIR = Path(__file__).parent.resolve() / "fixtures"
OBSERVABILITY_CHART = "oci://registry.opsmill.io/opsmill/chart/infrahub-observability"
OBSERVABILITY_RELEASE = "obs"
TEMPO_SERVICE = f"{OBSERVABILITY_RELEASE}-tempo"
FORWARDING_RE = re.compile(r"Forwarding from 127\.0\.0\.1:(\d+)")


def _kubectl(kubeconfig: str, *args: str, input_text: str | None = None) -> str:
    result = subprocess.run(
        ["kubectl", "--kubeconfig", kubeconfig, *args],
        capture_output=True,
        text=True,
        input=input_text,
    )
    if result.returncode != 0:
        raise RuntimeError(f"kubectl {' '.join(args)} failed (exit {result.returncode}): {result.stderr}")
    return result.stdout


@contextmanager
def _port_forward(kubeconfig: str, namespace: str, service: str, port: int) -> Iterator[str]:
    """Forward a local port to a service port and yield the local URL."""
    process = subprocess.Popen(
        ["kubectl", "--kubeconfig", kubeconfig, "port-forward", "-n", namespace, f"service/{service}", f":{port}"],
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
    )
    try:
        assert process.stdout is not None
        for line in process.stdout:
            match = FORWARDING_RE.search(line)
            if match:
                yield f"http://127.0.0.1:{match.group(1)}"
                return
        raise RuntimeError(f"kubectl port-forward to {service}:{port} ended without a local port")
    finally:
        process.terminate()
        process.wait(timeout=10)


@pytest.fixture(scope="module")
def observability_tempo(infrahub_k8s) -> Iterator[dict]:
    """Install the infrahub-observability chart with only Tempo, in the Infrahub namespace."""
    kubeconfig = infrahub_k8s["kubeconfig_path"]
    namespace = infrahub_k8s["namespace"]
    subprocess.run(
        [
            "helm",
            "upgrade",
            "--install",
            OBSERVABILITY_RELEASE,
            OBSERVABILITY_CHART,
            "-n",
            namespace,
            "-f",
            str(FIXTURES_DIR / "helm" / "observability-values.yaml"),
            "--kubeconfig",
            kubeconfig,
            "--wait",
            "--timeout",
            "5m",
        ],
        check=True,
    )
    try:
        yield {"kubeconfig": kubeconfig, "namespace": namespace}
    finally:
        subprocess.run(
            ["helm", "uninstall", OBSERVABILITY_RELEASE, "-n", namespace, "--kubeconfig", kubeconfig, "--wait"],
            check=False,
        )


@pytest.fixture(scope="module")
def slow_trace_id(observability_tempo) -> str:
    """Send one 3-second trace to the Tempo of the chart, and wait until it is searchable."""
    kubeconfig = observability_tempo["kubeconfig"]
    namespace = observability_tempo["namespace"]
    trace_id, document = synthetic_trace("k8s-traces-test", 3, span_count=3)
    with _port_forward(kubeconfig, namespace, TEMPO_SERVICE, 4318) as otlp_url:
        response = post_trace(otlp_url, document)
        assert response.status_code == 200, response.text
    with _port_forward(kubeconfig, namespace, TEMPO_SERVICE, 3100) as tempo_url:
        wait_for_traces(tempo_url, lambda traces: any(t["traceID"].rjust(32, "0") == trace_id for t in traces))
    return trace_id


@pytest.mark.e2e
@pytest.mark.k8s
async def test_collect_traces_k8s(observability_tempo, slow_trace_id, collect_binary, tmp_path):
    """T023: the collector finds the chart's Tempo by label and reads it through a port-forward."""
    namespace = observability_tempo["namespace"]
    output_dir = tmp_path / "bundles"
    run_collect(
        collect_binary,
        ["--k8s-namespace", namespace, "--output-dir", str(output_dir), "create", "--include-traces"],
        env={"KUBECONFIG": observability_tempo["kubeconfig"]},
    )

    bundle_path = find_bundle(output_dir)
    _, manifest = read_bundle_archive(bundle_path)
    validate_manifest_schema(manifest)
    entry = traces_entry(manifest)
    assert entry["status"] == "success", entry
    written = {trace_id_of(name) for name in bundle_trace_files(bundle_path)}
    assert slow_trace_id in written, f"trace {slow_trace_id} not in the bundle: {written}"


# A role that reads the namespace and runs commands in pods, but cannot
# create pods/portforward.
NO_PORT_FORWARD_ROLE = """
apiVersion: v1
kind: ServiceAccount
metadata:
  name: traces-no-port-forward
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: traces-no-port-forward
rules:
  - apiGroups: [""]
    resources: [pods, pods/log, services, endpoints, configmaps, events, persistentvolumeclaims]
    verbs: [get, list, watch]
  - apiGroups: [""]
    resources: [pods/exec]
    verbs: [create, get]
  - apiGroups: [apps]
    resources: [deployments, statefulsets, replicasets, daemonsets]
    verbs: [get, list, watch]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: traces-no-port-forward
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: traces-no-port-forward
subjects:
  - kind: ServiceAccount
    name: traces-no-port-forward
"""


@pytest.fixture()
def kubeconfig_without_port_forward(observability_tempo, tmp_path) -> Iterator[str]:
    """Return a kubeconfig that authenticates as the service account of NO_PORT_FORWARD_ROLE."""
    kubeconfig = observability_tempo["kubeconfig"]
    namespace = observability_tempo["namespace"]
    _kubectl(kubeconfig, "apply", "-n", namespace, "-f", "-", input_text=NO_PORT_FORWARD_ROLE)
    try:
        token = _kubectl(kubeconfig, "create", "token", "traces-no-port-forward", "-n", namespace).strip()
        config = yaml.safe_load(Path(kubeconfig).read_text())
        context = next(c for c in config["contexts"] if c["name"] == config["current-context"])
        context["context"]["user"] = "traces-no-port-forward"
        context["context"]["namespace"] = namespace
        config["users"] = [{"name": "traces-no-port-forward", "user": {"token": token}}]
        restricted = tmp_path / "kubeconfig-no-port-forward"
        restricted.write_text(yaml.safe_dump(config))
        os.chmod(restricted, 0o600)
        yield str(restricted)
    finally:
        subprocess.run(
            ["kubectl", "--kubeconfig", kubeconfig, "delete", "-n", namespace, "-f", "-", "--ignore-not-found"],
            input=NO_PORT_FORWARD_ROLE,
            capture_output=True,
            text=True,
            check=False,
        )


@pytest.mark.e2e
@pytest.mark.k8s
async def test_collect_traces_k8s_without_port_forward_permission(
    observability_tempo, kubeconfig_without_port_forward, collect_binary, tmp_path
):
    """T023: without create on pods/portforward, the traces entry is skipped and names the permission."""
    namespace = observability_tempo["namespace"]
    output_dir = tmp_path / "bundles"
    run_collect(
        collect_binary,
        ["--k8s-namespace", namespace, "--output-dir", str(output_dir), "create", "--include-traces"],
        env={"KUBECONFIG": kubeconfig_without_port_forward},
    )

    _, manifest = read_bundle_archive(find_bundle(output_dir))
    entry = traces_entry(manifest)
    assert entry["status"] == "skipped", entry
    assert "pods/portforward" in entry["reason"], entry
