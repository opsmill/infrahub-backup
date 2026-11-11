"""E2E tests: Docker Compose + the traces collector of infrahub-collect (INFP-744).

The class starts its own Infrahub stack with a Tempo service named
infrahub-tempo, as the observability Compose file of Infrahub does, and with
tracing on for infrahub-server and task-worker. It covers:

- T022: a bundle with --include-traces has the traces and a success entry; a
  bundle without it has no traces/ folder; the API token is in no file;
- T027: every attribute key that Infrahub writes is in a list of
  traces_attributes.json (FR-007);
- T028: no Cypher text, client IP address or exception text in the bundle;
- T031: the size limit keeps the slowest traces;
- T032: a trace file of the bundle loads into a second, empty Tempo;
- T037: --traces-tempo-url reads the traces of another Tempo;
- T033: the traces are collected while infrahub-server and task-worker are
  stopped. This test runs last, because it stops services.

The tests do not depend on the Neo4j edition, so they run in the community
leg of CI only.
"""

import json
import subprocess
import time
from datetime import datetime
from pathlib import Path

import pytest
import requests
from infrahub_sdk.testing.docker import TestInfrahubDockerClient
from infrahub_testcontainers.container import InfrahubDockerCompose
from testcontainers.core.container import DockerContainer

from tests.e2e.conftest import _is_enterprise
from tests.helpers.bundle import (
    assert_bundle_layout,
    assert_collector_outcomes,
    find_bundle,
    read_bundle_archive,
    validate_manifest_schema,
)
from tests.helpers.traces import (
    TEMPO_CONFIG_PATH,
    TEMPO_IMAGE,
    attribute_keys,
    attribute_values,
    bundle_file_contents,
    bundle_trace_files,
    fetch_trace,
    known_attribute_keys,
    load_allow_list,
    post_trace,
    search_traces,
    synthetic_trace,
    trace_id_of,
    traces_entry,
    wait_for_trace,
    wait_for_traces,
)
from tests.helpers.utils import run_collect, wait_for_http

TRACING_ENVIRONMENT = {
    "INFRAHUB_TRACE_ENABLE": "true",
    "INFRAHUB_TRACE_EXPORTER_TYPE": "otlp",
    "INFRAHUB_TRACE_EXPORTER_PROTOCOL": "grpc",
    "INFRAHUB_TRACE_EXPORTER_ENDPOINT": "http://infrahub-tempo:4317",
    "INFRAHUB_TRACE_INSECURE": "true",
    # Read by Infrahub 1.12 and later; earlier versions trace every request.
    "INFRAHUB_TRACE_SAMPLE_RATIO": "1",
}


def _compose_override() -> dict:
    """Return the Compose override that adds Tempo and turns tracing on."""
    return {
        "services": {
            "infrahub-tempo": {
                "image": TEMPO_IMAGE,
                "command": ["-config.file=/etc/tempo.yaml"],
                "volumes": [f"{TEMPO_CONFIG_PATH}:/etc/tempo.yaml:ro"],
                # Published on random host ports: the collector reads 3200
                # with `docker compose port`; the tests post traces to 4318.
                "ports": ["3200", "4318"],
            },
            "infrahub-server": {"environment": TRACING_ENVIRONMENT},
            "task-worker": {"environment": TRACING_ENVIRONMENT},
        }
    }


def _compose(project: str, *args: str) -> str:
    result = subprocess.run(["docker", "compose", "-p", project, *args], capture_output=True, text=True)
    if result.returncode != 0:
        raise RuntimeError(f"docker compose {' '.join(args)} failed (exit {result.returncode}): {result.stderr}")
    return result.stdout.strip()


def _published_url(project: str, service: str, port: int) -> str:
    """Return the host URL of a published port of a Compose service."""
    address = _compose(project, "port", service, str(port))
    host_port = address.rsplit(":", 1)[1]
    return f"http://127.0.0.1:{host_port}"


def _graphql(base_url: str, token: str, query: str) -> dict:
    response = requests.post(
        f"{base_url}/graphql",
        json={"query": query},
        headers={"X-INFRAHUB-KEY": token},
        timeout=60,
    )
    response.raise_for_status()
    body = response.json()
    assert "errors" not in body, f"GraphQL errors: {body['errors']}"
    return body["data"]


def _create_bundle(collect_binary: str, project: str, output_dir: Path, *args: str, env: dict | None = None):
    """Run infrahub-collect create and return the bundle path and manifest."""
    result = run_collect(
        collect_binary,
        ["--project", project, "--output-dir", str(output_dir), "create", *args],
        env=env,
    )
    bundle_path = find_bundle(output_dir)
    file_names, manifest = read_bundle_archive(bundle_path)
    validate_manifest_schema(manifest)
    statuses = assert_collector_outcomes(manifest)
    assert_bundle_layout(file_names, statuses)
    return bundle_path, manifest, result


@pytest.mark.e2e
@pytest.mark.docker
@pytest.mark.skipif(_is_enterprise(), reason="the traces collector does not depend on the edition")
class TestDockerCollectTraces(TestInfrahubDockerClient):
    @pytest.fixture(scope="class")
    def infrahub_compose(
        self,
        tmp_directory: Path,
        remote_repos_dir: Path,
        remote_backups_dir: Path,
        infrahub_version: str,
        deployment_type: str | None,
    ) -> InfrahubDockerCompose:
        """Compose project with Tempo and tracing on.

        Docker Compose reads docker-compose.override.yml next to
        docker-compose.yml, so the generated stack gets the Tempo service
        without a change to the testcontainers plugin.
        """
        compose = InfrahubDockerCompose.init(
            directory=tmp_directory,
            version=infrahub_version,
            deployment_type=deployment_type,
        )
        (tmp_directory / "docker-compose.override.yml").write_text(json.dumps(_compose_override(), indent=2))
        return compose

    @pytest.fixture(scope="class")
    def project(self, infrahub_compose: InfrahubDockerCompose) -> str:
        return infrahub_compose.project_name

    @pytest.fixture(scope="class")
    def admin_token(self, infrahub_compose: InfrahubDockerCompose) -> str:
        return infrahub_compose.get_env_var("INFRAHUB_TESTING_INITIAL_ADMIN_TOKEN")

    @pytest.fixture(scope="class")
    def tempo_url(self, infrahub_port: int, project: str) -> str:
        return _published_url(project, "infrahub-tempo", 3200)

    @pytest.fixture(scope="class")
    def tempo_otlp_url(self, infrahub_port: int, project: str) -> str:
        return _published_url(project, "infrahub-tempo", 4318)

    @pytest.fixture(scope="class")
    def infrahub_traces(self, infrahub_port: int, admin_token: str, tempo_url: str) -> list[dict]:
        """Send requests that create HTTP, GraphQL, database and Prefect spans.

        A branch creation runs a Prefect flow on task-worker. Returns the
        traces that Tempo then holds.
        """
        base_url = f"http://localhost:{infrahub_port}"
        for index in range(3):
            _graphql(
                base_url,
                admin_token,
                f'mutation {{ BuiltinTagCreate(data: {{name: {{value: "tag-{index}"}}}}) {{ ok }} }}',
            )
        _graphql(base_url, admin_token, "query { BuiltinTag { edges { node { id name { value } } } } }")
        _graphql(
            base_url,
            admin_token,
            'mutation { BranchCreate(data: {name: "traces-e2e"}, wait_until_completion: true) { ok } }',
        )

        # With Infrahub 1.8, every root service in Tempo was infrahub-server
        # in a local run, so the wait counts traces only.
        return wait_for_traces(tempo_url, lambda traces: len(traces) >= 5)

    def test_traces_not_requested(self, project, infrahub_traces, collect_binary, tmp_path):
        """T022 (b), SC-001: without --include-traces, no traces/ folder and a skipped entry."""
        bundle_path, manifest, _ = _create_bundle(collect_binary, project, tmp_path / "bundles")

        entry = traces_entry(manifest)
        assert entry["status"] == "skipped"
        assert "not requested" in entry["reason"]
        assert not bundle_trace_files(bundle_path)

    def test_traces_collected(self, project, infrahub_traces, admin_token, tempo_url, collect_binary, tmp_path):
        """T022 (a, c) and T028: the traces of the window, filtered, slowest first."""
        bundle_path, manifest, result = _create_bundle(
            collect_binary,
            project,
            tmp_path / "bundles",
            "--include-traces",
            "--traces-min-duration",
            "1ms",
            env={"INFRAHUB_TRACES_API_TOKEN": admin_token},
        )

        entry = traces_entry(manifest)
        assert entry["status"] == "success", entry
        details = entry["details"]
        assert details["min_duration"] == "1ms"
        assert details["max_size_mb"] == 100
        assert details["traces_written"] > 0
        assert details["traces_written"] + details["traces_left_out"] == details["traces_found"]

        window = details["window"]
        span = datetime.fromisoformat(window["end"]) - datetime.fromisoformat(window["start"])
        assert span.total_seconds() == 2 * 3600, f"default window is not 2h: {window}"

        files = bundle_trace_files(bundle_path)
        names = list(files)
        assert names[0].startswith("0001-"), names
        assert len(names) == details["traces_written"]
        assert sum(len(content) for content in files.values()) == details["bytes_written"]

        # Slowest first: the file order follows the durations that Tempo reports.
        durations = {
            trace["traceID"].lower().rjust(32, "0"): trace.get("durationMs", 0) for trace in search_traces(tempo_url)
        }
        ordered = [durations[trace_id_of(name)] for name in names if trace_id_of(name) in durations]
        assert ordered == sorted(ordered, reverse=True), f"traces are not slowest first: {ordered}"

        # FR-014: the API token is in no file of the bundle and not in the output.
        for path, content in bundle_file_contents(bundle_path).items():
            assert admin_token.encode() not in content, f"the API token is in {path}"
        assert admin_token not in result.stdout + result.stderr

        # Infrahub 1.12 and later report the sampling ratio; earlier versions
        # have no such API, and the entry then has no sample_ratio.
        if "sample_ratio" in details:
            assert details["sample_ratio"]["ratio_in_force"] == 1

        # T028, quickstart scenario 6: no Cypher text, client IP address,
        # exception text or removed attribute key in the trace files.
        allow = load_allow_list()
        client_addresses: set[str] = set()
        for name in names[:20]:
            raw = fetch_trace(tempo_url, trace_id_of(name))
            client_addresses |= attribute_values(raw, "net.peer.ip")
        for name, content in files.items():
            text = content.decode()
            assert "MATCH" not in text, f"{name} has Cypher text"
            assert "exception" not in text.lower(), f"{name} has exception text"
            for address in client_addresses:
                assert address not in text, f"{name} has the client IP address {address}"
            resource_keys, span_keys = attribute_keys(json.loads(text))
            assert not (resource_keys | span_keys) & set(allow["removed"]), f"{name} has a removed key"

        # Defaults: without --traces-min-duration, the minimum is 1s.
        _, manifest, _ = _create_bundle(collect_binary, project, tmp_path / "defaults", "--include-traces")
        entry = traces_entry(manifest)
        assert entry["status"] == "success", entry
        assert entry["details"]["min_duration"] == "1s"

    def test_attribute_keys_are_listed(self, infrahub_traces, tempo_url):
        """T027, FR-007: every attribute key that Infrahub writes is kept or removed on purpose."""
        known = known_attribute_keys(load_allow_list())
        unknown: dict[str, set[str]] = {}
        for summary in search_traces(tempo_url):
            trace = fetch_trace(tempo_url, summary["traceID"])
            resource_keys, span_keys = attribute_keys(trace)
            service = summary.get("rootServiceName", "?")
            for key in (resource_keys | span_keys) - known:
                unknown.setdefault(key, set()).add(service)
        assert not unknown, (
            "Attribute keys in neither list of src/internal/app/traces_attributes.json "
            f"(key: services): {dict(sorted(unknown.items()))}"
        )

    def test_size_limit_keeps_slowest(
        self, project, infrahub_traces, tempo_url, tempo_otlp_url, collect_binary, tmp_path
    ):
        """T031, SC-003: with --traces-max-size 1, the slowest traces fill 1 MB."""
        # Eight traces of about 200 KB each, from 9s down to 2s, so they are
        # slower than every Infrahub trace of the test and pass 1 MB together.
        large = {}
        for seconds in range(9, 1, -1):
            trace_id, document = synthetic_trace("size-limit-test", seconds, span_count=200, attribute_size=900)
            assert post_trace(tempo_otlp_url, document).status_code == 200
            large[trace_id] = seconds
        wait_for_traces(
            tempo_url,
            lambda traces: {t["traceID"].rjust(32, "0") for t in traces} >= set(large),
            query='{ resource.service.name = "size-limit-test" }',
        )

        bundle_path, manifest, _ = _create_bundle(
            collect_binary,
            project,
            tmp_path / "bundles",
            "--include-traces",
            "--traces-max-size",
            "1",
            "--traces-min-duration",
            "1500ms",
        )
        details = traces_entry(manifest)["details"]
        files = bundle_trace_files(bundle_path)
        assert sum(len(content) for content in files.values()) <= 1024 * 1024
        assert details["bytes_written"] <= 1024 * 1024
        assert details["traces_left_out"] > 0
        assert details["traces_written"] + details["traces_left_out"] == details["traces_found"]

        written = [trace_id_of(name) for name in files]
        written_large = [trace_id for trace_id in written if trace_id in large]
        slowest = sorted(large, key=large.get, reverse=True)[: len(written_large)]
        assert written_large == slowest, "a left-out trace is slower than a kept one"

    @pytest.fixture(scope="class")
    def second_tempo(self):
        """A second, empty Tempo, outside the Compose project."""
        container = (
            DockerContainer(TEMPO_IMAGE)
            .with_command("-config.file=/etc/tempo.yaml")
            .with_volume_mapping(str(TEMPO_CONFIG_PATH), "/etc/tempo.yaml", "ro")
            .with_exposed_ports(3200, 4318)
        )
        container.start()
        try:
            host = container.get_container_host_ip()
            urls = {
                "api": f"http://{host}:{container.get_exposed_port(3200)}",
                "otlp": f"http://{host}:{container.get_exposed_port(4318)}",
            }
            wait_for_http(f"{urls['api']}/ready", timeout=120.0, interval=2.0)
            yield urls
        finally:
            container.stop()

    def test_replay_into_second_tempo(self, project, infrahub_traces, second_tempo, collect_binary, tmp_path):
        """T032, FR-004: a trace file of the bundle loads into an empty Tempo."""
        bundle_path, _, _ = _create_bundle(
            collect_binary, project, tmp_path / "bundles", "--include-traces", "--traces-min-duration", "1ms"
        )
        files = bundle_trace_files(bundle_path)
        # Use the first trace that Infrahub wrote, not a synthetic one.
        name, content = next((name, content) for name, content in files.items() if b"size-limit-test" not in content)

        response = post_trace(second_tempo["otlp"], content)
        assert response.status_code == 200, response.text

        replayed = wait_for_trace(second_tempo["api"], trace_id_of(name))
        resource_keys, span_keys = attribute_keys(replayed)
        assert "service.name" in resource_keys
        if b'"query_name"' in content:
            assert "query_name" in span_keys

    def test_custom_tempo_url(self, project, second_tempo, collect_binary, tmp_path):
        """T037: --traces-tempo-url reads the traces of another Tempo."""
        trace_id, document = synthetic_trace("custom-backend-test", 3)
        assert post_trace(second_tempo["otlp"], document).status_code == 200
        wait_for_traces(
            second_tempo["api"],
            lambda traces: any(t["traceID"].rjust(32, "0") == trace_id for t in traces),
        )

        bundle_path, manifest, _ = _create_bundle(
            collect_binary,
            project,
            tmp_path / "bundles",
            "--include-traces",
            "--traces-tempo-url",
            second_tempo["api"],
        )
        entry = traces_entry(manifest)
        assert entry["status"] == "success", entry
        assert entry["details"]["backend"] == second_tempo["api"]
        written = {trace_id_of(name) for name in bundle_trace_files(bundle_path)}
        assert trace_id in written

    def test_traces_with_services_stopped(self, project, infrahub_port, infrahub_traces, collect_binary, tmp_path):
        """T033, SC-004: the traces are collected while Infrahub is down.

        Runs last: it stops infrahub-server and task-worker, and starts them
        again at the end.
        """
        _compose(project, "stop", "infrahub-server", "task-worker")
        try:
            bundle_path, manifest, _ = _create_bundle(
                collect_binary, project, tmp_path / "bundles", "--include-traces", "--traces-min-duration", "1ms"
            )
        finally:
            _compose(project, "start", "infrahub-server", "task-worker")
            time.sleep(2)
            wait_for_http(f"http://localhost:{infrahub_port}/api/config", timeout=180.0, interval=2.0)

        entry = traces_entry(manifest)
        assert entry["status"] == "success", entry
        assert entry["details"]["traces_written"] > 0
        assert bundle_trace_files(bundle_path)
