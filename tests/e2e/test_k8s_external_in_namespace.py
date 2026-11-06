"""E2E test: infrahub-backup running as a Job inside the namespace it backs up (FR-032).

When the process runs in a pod of the target namespace and finds the database tools on its
own PATH, it runs them in-process and creates no transient Job and no Secret (direct mode,
ADR 0017). This case runs the image built from this checkout as a Job in the external
topology's namespace, under a service account holding only the permissions the direct-mode
section of docs/docs/backup/external-databases.mdx lists, and asserts that the capture
completes and that no transient workload was created.

It needs the external topology (tasks.md T072) like the other external suites, and skips by
name until that is declared.
"""

import subprocess
import time

import pytest
import yaml

from tests.e2e.conftest import (
    DIRECT_MODE_RULES,
    SERVICE_NEO4J,
    external_db_env,
    external_topology_args,
    requires_external_db,
    transient_objects_after_run,
)

# The line the run logs when it takes direct mode, from src/internal/app/external_local.go,
# and the one it logs when it creates a transient workload instead.
DIRECT_MODE_LINE = "Running the external database tools in this process, with no transient Job"
TRANSIENT_WORKLOAD_LINE = "Creating a transient"
JOB_PATH_LINE = "Running the external database tools in a transient Job"

# The Job, its service account and its Role share this name. It contains no deployment
# service name as a substring, so the tool's name-based fallbacks cannot take this pod for
# a replica of a service it backs up.
RUNNER_NAME = "ibk-inns-runner"
BACKUP_DIR = "/tmp/backups"
ARCHIVE_MARKER = "ARCHIVES:"
JOB_TIMEOUT_SECONDS = 1800


def _kubectl(kubeconfig: str, namespace: str, *args: str, stdin: str | None = None, check: bool = True):
    return subprocess.run(
        ["kubectl", "--kubeconfig", kubeconfig, "-n", namespace, *args],
        input=stdin,
        capture_output=True,
        text=True,
        check=check,
    )


def _runner_manifests(namespace: str, image: str, env: dict[str, str]) -> str:
    flags = " ".join(external_topology_args())
    script = (
        f"infrahub-backup --k8s-namespace {namespace} {flags} --backup-dir {BACKUP_DIR} create --force "
        f"&& echo {ARCHIVE_MARKER} && ls -1 {BACKUP_DIR}"
    )
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
                "subjects": [{"kind": "ServiceAccount", "name": RUNNER_NAME, "namespace": namespace}],
            },
            {
                "apiVersion": "batch/v1",
                "kind": "Job",
                "metadata": {"name": RUNNER_NAME},
                "spec": {
                    "backoffLimit": 0,
                    "activeDeadlineSeconds": JOB_TIMEOUT_SECONDS,
                    "template": {
                        "spec": {
                            "serviceAccountName": RUNNER_NAME,
                            "restartPolicy": "Never",
                            "containers": [
                                {
                                    "name": "backup",
                                    "image": image,
                                    # Loaded into the node, never pulled: see load_image_into_vcluster.
                                    "imagePullPolicy": "IfNotPresent",
                                    "command": ["sh", "-c", script],
                                    "env": [{"name": key, "value": value} for key, value in env.items()],
                                }
                            ],
                        }
                    },
                },
            },
        ]
    )


def _wait_for_job(kubeconfig: str, namespace: str) -> str:
    """Wait until the runner Job completes or fails, and return which."""
    deadline = time.monotonic() + JOB_TIMEOUT_SECONDS + 60
    while time.monotonic() < deadline:
        result = _kubectl(
            kubeconfig,
            namespace,
            "get",
            "job",
            RUNNER_NAME,
            "-o",
            'jsonpath={range .status.conditions[?(@.status=="True")]}{.type}{"\\n"}{end}',
        )
        conditions = set(result.stdout.split())
        if "Complete" in conditions:
            return "Complete"
        if "Failed" in conditions:
            return "Failed"
        time.sleep(5)
    return "Timeout"


@pytest.mark.e2e
@pytest.mark.k8s
@requires_external_db(SERVICE_NEO4J, edition="enterprise")
async def test_a_run_inside_the_namespace_captures_with_no_transient_workload(infrahub_k8s_external, external_topology):
    """FR-032: in-namespace, with the tools on PATH, the capture runs in-process.

    The assertions are that the run took direct mode and said so, completed a capture, and
    left no Job, pod or Secret of the transient kind in the namespace, and that it never
    logged creating one. The service account holds no permission on jobs or secrets, so a
    run that tried the Job path would also have been refused by the API server.
    """
    namespace = infrahub_k8s_external["namespace"]
    kubeconfig = infrahub_k8s_external["kubeconfig_path"]
    image = infrahub_k8s_external["external_db_image"]
    manifests = _runner_manifests(namespace, image, external_db_env(external_topology))

    try:
        _kubectl(kubeconfig, namespace, "apply", "-f", "-", stdin=manifests)
        outcome = _wait_for_job(kubeconfig, namespace)
        log = _kubectl(kubeconfig, namespace, "logs", f"job/{RUNNER_NAME}", "--tail=-1", check=False)
        output = log.stdout + log.stderr

        assert outcome == "Complete", f"the in-namespace run did not complete ({outcome}):\n{output}"
        assert DIRECT_MODE_LINE in output, f"the run did not report taking direct mode:\n{output}"
        assert JOB_PATH_LINE not in output, f"the run took the transient Job path:\n{output}"
        assert TRANSIENT_WORKLOAD_LINE not in output, f"the run created a transient workload:\n{output}"

        archives = [
            line.strip()
            for line in output.split(ARCHIVE_MARKER, 1)[-1].splitlines()
            if line.strip().startswith("infrahub_backup_")
        ]
        assert ARCHIVE_MARKER in output and len(archives) == 1, (
            f"expected exactly one artifact in {BACKUP_DIR}, got {archives}:\n{output}"
        )

        left_behind = await transient_objects_after_run(kubeconfig, namespace)
        assert left_behind == [], f"a direct-mode run left transient objects in the namespace: {left_behind}"
    finally:
        _kubectl(
            kubeconfig,
            namespace,
            "delete",
            "-f",
            "-",
            "--ignore-not-found",
            "--cascade=background",
            stdin=manifests,
            check=False,
        )
