"""E2E tests: Docker Compose + troubleshooting bundle collection.

Covers spec 003-collect-tool US2 (quickstart scenarios 3, 4, and 6): full
bundle with parity content and a schema-validated manifest, project selection
with and without --project, the degraded case (stopped cache container →
exit 0 + failed manifest entry, FR-009/SC-005), masked env output (FR-008),
--log-lines / INFRAHUB_LOG_LINES precedence (FR-011), and US3
(--include-backup: standard backup next to the bundle, referenced in the
manifest, FR-014).

The tests share one class-scoped compose stack; the disruptive scenarios run
last so the stack stays healthy for the earlier ones: the degraded-cache test
stops/starts the cache container, and the include-backup test — the very last
— stops and restarts application containers through the inherited backup
behavior.
"""

import json
import os
import re
import subprocess
import tarfile
from pathlib import Path

import pytest
from infrahub_sdk.testing.docker import TestInfrahubDockerClient
from infrahub_testcontainers.container import InfrahubDockerCompose

from tests.helpers.bundle import (
    OPT_IN_COLLECTORS,
    assert_bundle_layout,
    assert_collector_outcomes,
    find_bundle,
    read_bundle_archive,
    read_bundle_member,
    validate_manifest_schema,
)
from tests.helpers.utils import find_latest_backup, run_collect, wait_for_http

# The compose stack scales task-worker to 2 so per-replica collection on a
# scaled compose service is observable (FR-002).
TASK_WORKER_COUNT = 2

# Mirrors the tool's masking contract (research R5): keys matching these
# substrings must have masked values in env dumps.
SENSITIVE_KEY_RE = re.compile(r"pass|secret|token|key", re.IGNORECASE)
ENV_VAR_NAME_RE = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*$")
MASKED_VALUE = "********"


# ---------------------------------------------------------------------------
# docker compose helpers
# ---------------------------------------------------------------------------
def _compose(project: str, *args: str, check: bool = True) -> subprocess.CompletedProcess:
    result = subprocess.run(
        ["docker", "compose", "-p", project, *args],
        capture_output=True,
        text=True,
    )
    if check and result.returncode != 0:
        raise RuntimeError(f"docker compose {' '.join(args)} failed (exit {result.returncode}): {result.stderr}")
    return result


def _service_container_names(project: str, service: str) -> list[str]:
    """Container names backing one compose service (running or stopped)."""
    output = _compose(project, "ps", "-a", "--format", "json", service).stdout
    names = []
    for line in output.splitlines():
        line = line.strip()
        if not line:
            continue
        entry = json.loads(line)
        if entry.get("Service") == service and entry.get("Name"):
            names.append(entry["Name"])
    return sorted(names)


def _list_infrahub_compose_projects() -> list[str]:
    """Enumerate Infrahub compose projects the way ListDockerProjects does."""
    output = subprocess.run(["docker", "compose", "ls"], capture_output=True, text=True, check=True).stdout
    projects = []
    for line in output.splitlines():
        line = line.strip()
        if not line or line.upper().startswith("NAME "):
            continue
        name = line.split()[0]
        ps = subprocess.run(["docker", "compose", "-p", name, "ps", "-a"], capture_output=True, text=True)
        if ps.returncode == 0 and "infrahub" in ps.stdout.lower():
            projects.append(name)
    return sorted(projects)


def _assert_env_dump_masked(env_text: str, plaintext_secrets: list[str]) -> None:
    """FR-008: sensitive keys masked, known secret values absent."""
    masked = 0
    for line in env_text.splitlines():
        if "=" not in line:
            continue
        key, value = line.split("=", 1)
        if not ENV_VAR_NAME_RE.match(key):
            continue  # continuation line of a multi-line value
        if SENSITIVE_KEY_RE.search(key):
            assert value == MASKED_VALUE, f"Sensitive env var {key} not masked: {value!r}"
            masked += 1
    assert masked > 0, "env dump contains no masked entries; masking cannot be verified"
    for secret in plaintext_secrets:
        assert secret not in env_text, f"Plaintext secret {secret!r} leaked into the env dump"


# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------
@pytest.mark.e2e
@pytest.mark.docker
class TestDockerCollect(TestInfrahubDockerClient):
    @pytest.fixture(scope="class")
    def infrahub_compose(
        self,
        request: pytest.FixtureRequest,
        tmp_directory: Path,
        remote_repos_dir: Path,
        remote_backups_dir: Path,
        infrahub_version: str,
        deployment_type: str | None,
    ) -> InfrahubDockerCompose:
        """Compose project with task-worker scaled to TASK_WORKER_COUNT.

        Overrides the session default of 1 worker (tests/e2e/conftest.py) so
        per-replica collection on a scaled compose service is observable.
        Compose interpolation reads the process environment when `up` runs
        (it overrides the generated .env), so the variable must stay set for
        the whole class and is restored at class teardown.
        """
        original = os.environ.get("INFRAHUB_TESTING_TASK_WORKER_COUNT")

        def restore() -> None:
            if original is None:
                os.environ.pop("INFRAHUB_TESTING_TASK_WORKER_COUNT", None)
            else:
                os.environ["INFRAHUB_TESTING_TASK_WORKER_COUNT"] = original

        request.addfinalizer(restore)
        os.environ["INFRAHUB_TESTING_TASK_WORKER_COUNT"] = str(TASK_WORKER_COUNT)
        return InfrahubDockerCompose.init(
            directory=tmp_directory,
            version=infrahub_version,
            deployment_type=deployment_type,
        )

    async def test_collect_full_bundle(self, infrahub_compose, infrahub_port, collect_binary, tmp_path):
        """US2 scenario 1: parity bundle with schema-validated manifest (FR-006/FR-008)."""
        project = infrahub_compose.project_name
        output_dir = tmp_path / "bundles"

        run_collect(
            collect_binary,
            ["--project", project, "--output-dir", str(output_dir), "create"],
        )

        # Archive integrity: valid tar.gz, everything under bundle/
        bundle_path = find_bundle(output_dir)
        file_names, manifest = read_bundle_archive(bundle_path)

        # Manifest conforms to contracts/manifest.schema.json
        validate_manifest_schema(manifest)
        assert manifest["environment"] == "docker"
        assert manifest["log_lines"] == 100000  # documented default
        assert f"support_bundle_{manifest['collect_id']}.tar.gz" == bundle_path.name

        # SC-005: one manifest entry per planned collector, no duplicates
        statuses = assert_collector_outcomes(manifest)

        # Parity content (US2 scenario 1): every collector succeeds on a
        # healthy stack except the optional service that is scaled to zero
        # and the opt-in extras, which are skipped when not requested.
        assert statuses["logs/task-manager-background-svc"] == "skipped"
        failed = {name: status for name, status in statuses.items() if status != "success"}
        failed.pop("logs/task-manager-background-svc")
        for extra in OPT_IN_COLLECTORS:
            assert statuses[extra] == "skipped", f"Opt-in collector {extra} must be skipped when not requested"
            failed.pop(extra)
        assert not failed, f"Collectors did not succeed on a healthy stack: {failed}"

        # Layout per contracts/bundle-layout.md — identical to Kubernetes (SC-004)
        assert_bundle_layout(file_names, statuses)

        # FR-002: one log file per replica of the scaled task-worker service,
        # named after the container
        worker_containers = _service_container_names(project, "task-worker")
        assert len(worker_containers) >= TASK_WORKER_COUNT, (
            f"Expected >= {TASK_WORKER_COUNT} task-worker containers, found {worker_containers}"
        )
        worker_logs = {
            name.removeprefix("bundle/logs/task-worker/")
            for name in file_names
            if name.startswith("bundle/logs/task-worker/")
        }
        expected_worker_logs = {f"{name}.log" for name in worker_containers}
        assert expected_worker_logs <= worker_logs, (
            f"Missing per-replica task-worker logs: {expected_worker_logs - worker_logs}"
        )

        # Previous-container logs do not exist on Docker
        previous_logs = [name for name in file_names if name.endswith(".previous.log")]
        assert not previous_logs, f"Unexpected previous logs in a Docker bundle: {previous_logs}"

        # Per-replica task-worker state directories, named after the container
        for container in worker_containers:
            assert any(name.startswith(f"bundle/task-worker/{container}/") for name in file_names), (
                f"Missing per-replica task-worker state for {container}"
            )

        # FR-008: masked env output contains no plaintext secrets
        env_text = read_bundle_member(bundle_path, "bundle/server/environment.txt")
        secrets = [
            infrahub_compose.get_env_var("INFRAHUB_TESTING_INITIAL_ADMIN_TOKEN"),
            infrahub_compose.get_env_var("INFRAHUB_TESTING_INITIAL_AGENT_TOKEN"),
            infrahub_compose.get_env_var("INFRAHUB_TESTING_SECURITY_SECRET_KEY"),
        ]
        _assert_env_dump_masked(env_text, secrets)

    async def test_collect_project_selection_without_flag(
        self, infrahub_compose, infrahub_port, collect_binary, tmp_path
    ):
        """US2 scenario 2: without --project, selection behaves like the backup tool."""
        project = infrahub_compose.project_name
        output_dir = tmp_path / "bundles"

        infrahub_projects = _list_infrahub_compose_projects()
        assert project in infrahub_projects, f"Test project {project} not discoverable: {infrahub_projects}"

        if len(infrahub_projects) == 1:
            # Exactly one Infrahub project on this host: auto-detection selects it.
            run_collect(collect_binary, ["--output-dir", str(output_dir), "create"])
            _, manifest = read_bundle_archive(find_bundle(output_dir))
            assert manifest["environment"] == "docker"
        else:
            # Shared host with several Infrahub projects: the tool must refuse
            # and demand --project, exactly like infrahub-backup.
            result = subprocess.run(
                [collect_binary, "--output-dir", str(output_dir), "create"],
                capture_output=True,
                text=True,
            )
            assert result.returncode != 0, "create without --project succeeded despite multiple projects"
            combined = result.stdout + result.stderr
            assert "multiple docker compose projects found" in combined, (
                f"Expected project-selection error, got:\n{combined}"
            )

    async def test_collect_log_lines_flag_and_env(self, infrahub_compose, infrahub_port, collect_binary, tmp_path):
        """FR-011 / quickstart scenario 6: --log-lines wins over INFRAHUB_LOG_LINES."""
        project = infrahub_compose.project_name

        flag_dir = tmp_path / "flag"
        run_collect(
            collect_binary,
            ["--project", project, "--output-dir", str(flag_dir), "create", "--log-lines", "500"],
            env={"INFRAHUB_LOG_LINES": "250"},
        )
        _, manifest = read_bundle_archive(find_bundle(flag_dir))
        assert manifest["log_lines"] == 500, "flag must win over the environment variable"

        env_dir = tmp_path / "env"
        run_collect(
            collect_binary,
            ["--project", project, "--output-dir", str(env_dir), "create"],
            env={"INFRAHUB_LOG_LINES": "250"},
        )
        _, manifest = read_bundle_archive(find_bundle(env_dir))
        assert manifest["log_lines"] == 250, "environment variable must apply when the flag is absent"

    async def test_collect_degraded_cache(self, infrahub_compose, infrahub_port, collect_binary, tmp_path):
        """FR-009/SC-005 (quickstart scenario 4): stopped cache → exit 0 + failed entry.

        Runs after the healthy-stack scenarios: it degrades the shared stack
        and restores it afterwards.
        """
        project = infrahub_compose.project_name
        output_dir = tmp_path / "bundles"

        _compose(project, "stop", "cache")
        try:
            # run_collect raises on a non-zero exit, so returning proves exit 0
            run_collect(
                collect_binary,
                ["--project", project, "--output-dir", str(output_dir), "create"],
            )
        finally:
            _compose(project, "start", "cache")

        bundle_path = find_bundle(output_dir)
        file_names, manifest = read_bundle_archive(bundle_path)
        validate_manifest_schema(manifest)

        # Every planned collector is still accounted for (SC-005)
        statuses = assert_collector_outcomes(manifest)

        entries = {entry["name"]: entry for entry in manifest["collectors"]}
        assert entries["cache-status"]["status"] == "failed", (
            f"cache-status = {entries['cache-status']}, want failed while cache is stopped"
        )
        assert entries["cache-status"]["reason"], "failed cache-status entry must carry a reason"

        # A deployed-but-stopped container still yields its logs (FR-009)
        assert statuses["logs/cache"] == "success", "logs of the stopped cache container must still be collected"
        assert any(name.startswith("bundle/logs/cache/") for name in file_names)

    async def test_collect_include_backup(self, infrahub_compose, infrahub_port, collect_binary, tmp_path):
        """US3 scenario 1 (FR-014): --include-backup creates a standard backup
        next to the bundle and records it in the manifest.

        Runs last in the class: the delegated backup follows the standard
        backup behavior, which stops and restarts application containers.
        """
        project = infrahub_compose.project_name
        output_dir = tmp_path / "bundles"
        backup_dir = tmp_path / "backups"

        # run_collect raises on a non-zero exit, so returning proves exit 0
        run_collect(
            collect_binary,
            [
                "--project",
                project,
                "--output-dir",
                str(output_dir),
                "--backup-dir",
                str(backup_dir),
                "create",
                "--include-backup",
            ],
        )

        try:
            # The backup artifact is a standard backup archive next to the
            # bundle (not embedded in it), carrying the backup metadata.
            backup_file = find_latest_backup(backup_dir)
            with tarfile.open(backup_file, "r:gz") as tar:
                metadata_file = tar.extractfile("backup/backup_information.json")
                assert metadata_file is not None, "backup archive is missing backup_information.json"
                backup_metadata = json.loads(metadata_file.read())
            assert backup_metadata["backup_id"] == backup_file.name.removesuffix(".tar.gz"), (
                f"Backup metadata id {backup_metadata['backup_id']!r} does not match {backup_file.name}"
            )

            # The manifest references the backup; the rest of the plan still ran
            bundle_path = find_bundle(output_dir)
            file_names, manifest = read_bundle_archive(bundle_path)
            validate_manifest_schema(manifest)
            statuses = assert_collector_outcomes(manifest)
            assert statuses["backup"] == "success", f"backup collector = {statuses['backup']}, want success"

            entries = {entry["name"]: entry for entry in manifest["collectors"]}
            assert entries["backup"].get("artifact") == str(backup_file), (
                f"Manifest backup artifact {entries['backup'].get('artifact')!r} "
                f"does not reference the produced backup {backup_file}"
            )

            # The backup stays a sibling artifact, never embedded in the bundle
            embedded_archives = [name for name in file_names if name.endswith(".tar.gz")]
            assert not embedded_archives, f"Backup archive embedded in the bundle: {embedded_archives}"
        finally:
            # The inherited backup behavior stopped/restarted app containers;
            # wait for recovery so class teardown sees a healthy deployment.
            await wait_for_http(f"http://localhost:{infrahub_port}/api/config", timeout=180.0, interval=5.0)
