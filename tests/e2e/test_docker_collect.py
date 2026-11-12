"""E2E tests: Docker Compose + troubleshooting bundle collection.

Covers spec 003-collect-tool US2 (quickstart scenarios 3, 4, and 6): full
bundle with parity content and a schema-validated manifest, project selection
with and without --project, the degraded case (stopped cache container →
exit 0 + failed manifest entry, FR-009/SC-005), masked env output (FR-008),
--log-lines / INFRAHUB_LOG_LINES precedence (FR-011), and US3
(--include-backup: standard backup next to the bundle, referenced in the
manifest, FR-014). Also covers spec 006-taskmgr-db-storage User Story 1
(quickstart scenario 2): the storage figures of the task manager database in
every default bundle; and User Story 2 (quickstart scenarios 3 and 4): the
storage breakdown on request, and the time limit that PostgreSQL enforces.

The tests share one class-scoped compose stack; the disruptive scenarios run
last so the stack stays healthy for the earlier ones. The time-limit test locks
the artifact table and releases the lock before it ends. The degraded-cache test
stops/starts the cache container, and the include-backup test — the very last
— stops and restarts application containers through the inherited backup
behavior.
"""

import asyncio
import json
import os
import re
import subprocess
import tarfile
import time
import uuid
from datetime import datetime, timedelta
from pathlib import Path

import pytest
from infrahub_sdk.testing.docker import TestInfrahubDockerClient
from infrahub_testcontainers.container import InfrahubDockerCompose

from tests.helpers.bundle import (
    BREAKDOWN_TABLES,
    EXPECTED_COLLECTORS,
    OPT_IN_COLLECTORS,
    STORAGE_BREAKDOWN_ERROR_MEMBER,
    STORAGE_BREAKDOWN_MEMBER,
    STORAGE_ERROR_MEMBER,
    STORAGE_REPORT_MEMBER,
    assert_bundle_layout,
    assert_collector_outcomes,
    assert_storage_breakdown,
    assert_storage_collected,
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

# FR-001 (spec 006-taskmgr-db-storage): the bundle's database size may differ
# from a later pg_database_size by what the task manager writes in between.
STORAGE_SIZE_TOLERANCE_BYTES = 1024 * 1024
# SC-002: the storage figures add no more than 10 seconds to a run.
STORAGE_COLLECTION_BUDGET = timedelta(seconds=10)

# Spec 006 User Story 2: the flow whose runs the breakdown test creates through
# the Prefect API, and how long to wait for their events to be stored.
STORAGE_SEED_FLOW = "e2e-storage-seed"
STORAGE_SEED_COMPLETED_RUNS = 3
STORAGE_SEED_EVENTS_TIMEOUT = 60.0
# Runs inside a task-worker container, which has the Prefect client and
# PREFECT_API_URL, as `python - <flow name> <completed runs>`. It prints the
# seeded flow run IDs as JSON on its last line.
STORAGE_SEED_SCRIPT = '''
import asyncio
import json
import sys

from prefect import flow, get_client
from prefect.states import Cancelled, Completed, Running, Scheduled

FLOW_NAME = sys.argv[1]
COMPLETED_RUNS = int(sys.argv[2])


@flow(name=FLOW_NAME)
def e2e_storage_seed() -> None:
    """Never runs: the seed creates the flow runs through the API only."""


async def set_state(client, flow_run_id, state, force=False):
    result = await client.set_flow_run_state(flow_run_id, state, force=force)
    if result.status.value != "ACCEPT":
        raise SystemExit(f"flow run {flow_run_id}: {state.type.value} not accepted: {result.status.value}")


async def main():
    async with get_client() as client:
        completed = []
        for index in range(COMPLETED_RUNS):
            run = await client.create_flow_run(e2e_storage_seed, name=f"{FLOW_NAME}-completed-{index}")
            await set_state(client, run.id, Running())
            await set_state(client, run.id, Completed())
            completed.append(str(run.id))
        # Cancelled while still scheduled, so the run never gets a start_time.
        run = await client.create_flow_run(e2e_storage_seed, name=f"{FLOW_NAME}-cancelled", state=Scheduled())
        await set_state(client, run.id, Cancelled(), force=True)
        print(json.dumps({"completed": completed, "cancelled": str(run.id)}))


asyncio.run(main())
'''

# Spec 006 FR-009: the test-only variable that lowers both query limits, the
# limit it sets, and the application name of the session that holds the lock
# on the artifact table, so the test can find and stop that session.
DB_QUERY_TIMEOUT_ENV = "INFRAHUB_TASK_MANAGER_DB_QUERY_TIMEOUT"
LOWERED_DB_QUERY_LIMIT = "5s"
ARTIFACT_LOCK_APPLICATION = "infrahub-e2e-artifact-lock"
# The lock must outlast the whole run, not only the storage queries: the
# lock also blocks `prefect flow-run ls`, so task-manager-state waits for its
# own 60-second limit first, and a run takes about 85 seconds locally. The
# quickstart's 120 seconds leaves too little margin on a slower machine; the
# test stops the session as soon as the run ends.
ARTIFACT_LOCK_SECONDS = 300
ARTIFACT_LOCK_SQL = (
    f"BEGIN; LOCK TABLE artifact IN ACCESS EXCLUSIVE MODE; SELECT pg_sleep({ARTIFACT_LOCK_SECONDS}); COMMIT;"
)
# The sessions of the lock application, and the ACCESS EXCLUSIVE locks they
# hold on artifact. to_regclass does not wait for the lock (research R5).
ARTIFACT_LOCK_STATE_SQL = (
    f"SELECT (SELECT count(*) FROM pg_stat_activity WHERE application_name = '{ARTIFACT_LOCK_APPLICATION}'), "
    "(SELECT count(*) FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid "
    f"WHERE a.application_name = '{ARTIFACT_LOCK_APPLICATION}' AND l.locktype = 'relation' "
    "AND l.relation = to_regclass('artifact') AND l.mode = 'AccessExclusiveLock' AND l.granted)"
)
ARTIFACT_LOCK_TERMINATE_SQL = (
    f"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name = '{ARTIFACT_LOCK_APPLICATION}'"
)
LOCK_WAIT_TIMEOUT = 30.0


# ---------------------------------------------------------------------------
# docker compose helpers
# ---------------------------------------------------------------------------
def _compose(project: str, *args: str, check: bool = True, input: str | None = None) -> subprocess.CompletedProcess:
    result = subprocess.run(
        ["docker", "compose", "-p", project, *args],
        capture_output=True,
        text=True,
        input=input,
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


def _container_env(project: str, service: str, name: str) -> str | None:
    """Value of one environment variable in a running service container, or None when unset."""
    result = _compose(project, "exec", "-T", service, "printenv", name, check=False)
    return result.stdout.strip() if result.returncode == 0 else None


def _task_manager_db_owner_and_database(project: str) -> tuple[str, str]:
    """Owner and database of task-manager-db, read from its container's environment.

    Uses the official PostgreSQL image's defaults when a variable is unset: the
    owner is postgres, and the database is named after the owner.
    """
    owner = _container_env(project, "task-manager-db", "POSTGRES_USER") or "postgres"
    database = _container_env(project, "task-manager-db", "POSTGRES_DB") or owner
    return owner, database


def _task_manager_db_query(project: str, owner: str, database: str, sql: str) -> str:
    """Run one SQL statement in task-manager-db with psql and return its unaligned output."""
    return _compose(project, "exec", "-T", "task-manager-db", "psql", "-U", owner, "-d", database, "-Atc", sql).stdout


def _collector_duration(json_log: str, collector: str) -> timedelta:
    """Time from a collector's `Collecting <name>` line to the next collector's line.

    Reads the output of `--log-format json`. The next collector's line is the
    run loop's `Collecting <name>` or `Skipping <name>: <reason>` for another
    planned collector. logrus writes the time with one-second precision.
    """
    records = []
    for line in json_log.splitlines():
        try:
            record = json.loads(line)
        except json.JSONDecodeError:
            continue
        if isinstance(record, dict) and "msg" in record and "time" in record:
            records.append(record)

    def starts_collector(message: str, names: set[str]) -> bool:
        return any(message == f"Collecting {name}" or message.startswith(f"Skipping {name}: ") for name in names)

    start = next((i for i, r in enumerate(records) if r["msg"] == f"Collecting {collector}"), None)
    assert start is not None, f"No 'Collecting {collector}' line in the JSON log:\n{json_log}"
    others = EXPECTED_COLLECTORS - {collector}
    end = next((r for r in records[start + 1 :] if starts_collector(r["msg"], others)), None)
    assert end is not None, f"No collector line after 'Collecting {collector}' in the JSON log:\n{json_log}"
    return datetime.fromisoformat(end["time"]) - datetime.fromisoformat(records[start]["time"])


async def _seed_storage_breakdown(project: str, owner: str, database: str) -> None:
    """Create flow runs of STORAGE_SEED_FLOW through the Prefect client in a task-worker container.

    Spec 006 User Story 2: STORAGE_SEED_COMPLETED_RUNS flow runs moved through
    Running to Completed, and one flow run cancelled with force while still
    Scheduled, so its start_time stays null (FR-007). The task manager stores
    events in batches, so this waits until the event of the final state of
    every seeded flow run is in the events table.
    """
    result = _compose(
        project,
        "exec",
        "-T",
        "task-worker",
        "python",
        "-",
        STORAGE_SEED_FLOW,
        str(STORAGE_SEED_COMPLETED_RUNS),
        input=STORAGE_SEED_SCRIPT,
    )
    lines = result.stdout.strip().splitlines()
    assert lines, f"The seed script printed no flow run IDs; stderr:\n{result.stderr}"
    seeded = json.loads(lines[-1])
    # uuid.UUID validates each ID before it goes into the SQL text.
    final_events = {uuid.UUID(run_id): "prefect.flow-run.Completed" for run_id in seeded["completed"]}
    final_events[uuid.UUID(seeded["cancelled"])] = "prefect.flow-run.Cancelled"
    expected = {f"prefect.flow-run.{run_id}|{event}" for run_id, event in final_events.items()}
    resource_ids = ", ".join(f"'prefect.flow-run.{run_id}'" for run_id in final_events)
    sql = f"SELECT resource_id, event FROM events WHERE resource_id IN ({resource_ids})"

    deadline = time.monotonic() + STORAGE_SEED_EVENTS_TIMEOUT
    while True:
        stored = set(_task_manager_db_query(project, owner, database, sql).splitlines())
        if expected <= stored:
            return
        if time.monotonic() > deadline:
            raise AssertionError(
                f"Events of the seeded flow runs not stored after {STORAGE_SEED_EVENTS_TIMEOUT}s: "
                f"missing {sorted(expected - stored)}"
            )
        await asyncio.sleep(1)


def _read_storage_breakdown(output_dir: Path) -> tuple[set[str], dict[str, dict], dict]:
    """Read a bundle collected with the breakdown: (file names, manifest entries by name, storage-breakdown.json).

    Validates the manifest and the breakdown file against their contracts. On a
    missing file, the message carries the manifest entry and
    storage-breakdown.err.txt, which hold the cause.
    """
    bundle_path = find_bundle(output_dir)
    file_names, manifest = read_bundle_archive(bundle_path)
    validate_manifest_schema(manifest)
    assert_collector_outcomes(manifest)
    entries = {entry["name"]: entry for entry in manifest["collectors"]}
    if STORAGE_BREAKDOWN_MEMBER not in file_names:
        diagnostics = (
            read_bundle_member(bundle_path, STORAGE_BREAKDOWN_ERROR_MEMBER)
            if STORAGE_BREAKDOWN_ERROR_MEMBER in file_names
            else "<no storage-breakdown.err.txt>"
        )
        raise AssertionError(
            f"{STORAGE_BREAKDOWN_MEMBER} missing; task-manager-db-storage-breakdown = "
            f"{entries['task-manager-db-storage-breakdown']}; storage-breakdown.err.txt:\n{diagnostics}"
        )
    assert STORAGE_BREAKDOWN_ERROR_MEMBER not in file_names, f"{STORAGE_BREAKDOWN_ERROR_MEMBER} next to the breakdown"
    breakdown = json.loads(read_bundle_member(bundle_path, STORAGE_BREAKDOWN_MEMBER))
    assert_storage_breakdown(breakdown)
    return file_names, entries, breakdown


def _assert_breakdown_progress(output: str, time_limit: str) -> None:
    """FR-013: one `Collecting storage breakdown of table <table>` line per table, in list order."""
    positions = []
    for table in BREAKDOWN_TABLES:
        line = f"Collecting storage breakdown of table {table} (time limit {time_limit})"
        assert line in output, f"No {line!r} in the output:\n{output}"
        positions.append(output.index(line))
    assert positions == sorted(positions), f"Breakdown progress lines are not in list order:\n{output}"


def _breakdown_group(table: dict, **keys: str) -> dict:
    """The group of a collected breakdown table whose key values are keys."""
    group = next((group for group in table["groups"] if group["group"] == keys), None)
    assert group is not None, f"No group {keys} in table {table['name']}: {[g['group'] for g in table['groups']]}"
    return group


def _assert_seeded_breakdown(output_dir: Path, output: str, database: str) -> None:
    """Spec 006 quickstart scenario 3: the breakdown holds the flow runs and events the test seeded."""
    _, entries, breakdown = _read_storage_breakdown(output_dir)
    assert entries["task-manager-db-storage-breakdown"]["status"] == "success", (
        f"task-manager-db-storage-breakdown = {entries['task-manager-db-storage-breakdown']}, want success"
    )
    assert breakdown["database"] == database, (
        f"storage-breakdown.json names {breakdown['database']!r}, want {database!r}"
    )
    # FR-009: each table is bounded by the 5-minute limit.
    assert breakdown["time_limit_per_table"] == "300s", breakdown["time_limit_per_table"]
    _assert_breakdown_progress(output, "300s")

    tables = {table["name"]: table for table in breakdown["tables"]}
    not_collected = {name: table for name, table in tables.items() if table["status"] != "collected"}
    assert not not_collected, f"Tables not collected on a healthy stack: {not_collected}"

    # FR-007: the run cancelled before it started is counted as a row without start_time.
    cancelled = _breakdown_group(tables["flow_run"], flow_name=STORAGE_SEED_FLOW, state_type="CANCELLED")
    assert cancelled["rows"] == 1, cancelled
    assert cancelled["rows_without_start_time"] == 1, cancelled
    completed = _breakdown_group(tables["flow_run"], flow_name=STORAGE_SEED_FLOW, state_type="COMPLETED")
    assert completed["rows"] == STORAGE_SEED_COMPLETED_RUNS, completed
    assert completed["rows_without_start_time"] == 0, completed
    assert completed["oldest"] is not None and completed["newest"] is not None, completed

    event_names = {group["group"]["event_name"] for group in tables["events"]["groups"]}
    assert "prefect.flow-run.Completed" in event_names, f"No prefect.flow-run.Completed group in {sorted(event_names)}"


def _artifact_lock_state(project: str, owner: str, database: str) -> tuple[int, int]:
    """(sessions of the lock application, ACCESS EXCLUSIVE locks they hold on artifact)."""
    sessions, locks = _task_manager_db_query(project, owner, database, ARTIFACT_LOCK_STATE_SQL).strip().split("|")
    return int(sessions), int(locks)


def _start_artifact_lock(project: str, owner: str, database: str) -> subprocess.Popen:
    """Start a psql session that holds an ACCESS EXCLUSIVE lock on artifact for up to ARTIFACT_LOCK_SECONDS."""
    return subprocess.Popen(
        [
            "docker",
            "compose",
            "-p",
            project,
            "exec",
            "-T",
            "-e",
            f"PGAPPNAME={ARTIFACT_LOCK_APPLICATION}",
            "task-manager-db",
            "psql",
            "-U",
            owner,
            "-d",
            database,
            "-c",
            ARTIFACT_LOCK_SQL,
        ],
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )


async def _wait_for_artifact_lock(project: str, owner: str, database: str, lock: subprocess.Popen) -> None:
    """Wait until pg_locks shows the lock session's ACCESS EXCLUSIVE lock on artifact."""
    deadline = time.monotonic() + LOCK_WAIT_TIMEOUT
    while _artifact_lock_state(project, owner, database)[1] < 1:
        if lock.poll() is not None:
            stdout, stderr = lock.communicate()
            raise RuntimeError(
                f"The lock session exited ({lock.returncode}) before it took the lock:\n{stdout}{stderr}"
            )
        if time.monotonic() > deadline:
            raise TimeoutError(f"The lock session did not lock artifact within {LOCK_WAIT_TIMEOUT}s")
        await asyncio.sleep(0.5)


async def _release_artifact_lock(project: str, owner: str, database: str, lock: subprocess.Popen) -> None:
    """Stop the lock session and wait until PostgreSQL no longer shows it, so later tests run unlocked.

    Stopping the docker compose client alone can leave psql running in the
    container, so the session is terminated in PostgreSQL first.
    """
    _task_manager_db_query(project, owner, database, ARTIFACT_LOCK_TERMINATE_SQL)
    try:
        lock.communicate(timeout=LOCK_WAIT_TIMEOUT)
    except subprocess.TimeoutExpired:
        lock.kill()
        lock.communicate()
    deadline = time.monotonic() + LOCK_WAIT_TIMEOUT
    while (state := _artifact_lock_state(project, owner, database)) != (0, 0):
        if time.monotonic() > deadline:
            raise TimeoutError(f"The lock session is still in PostgreSQL (sessions, locks) = {state}")
        await asyncio.sleep(0.5)


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
        """US2 scenario 1: parity bundle with schema-validated manifest (FR-006/FR-008).

        Also spec 006-taskmgr-db-storage User Story 1 (quickstart scenario 2):
        every default bundle holds the storage figures of the task manager
        database (FR-001 to FR-003), collected in under 10 seconds (SC-002).
        """
        project = infrahub_compose.project_name
        output_dir = tmp_path / "bundles"

        result = run_collect(
            collect_binary,
            ["--project", project, "--output-dir", str(output_dir), "--log-format", "json", "create"],
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

        # Spec 006 FR-001 to FR-003: storage.json is collected and matches
        # contracts/storage.schema.json. Checked before the parity check
        # below, so a failure reports its reason and storage.err.txt.
        storage = assert_storage_collected(bundle_path, file_names, manifest)
        # Spec 006 FR-004: without the flag or its environment variable, the
        # breakdown is skipped as not requested and writes no file.
        breakdown_entry = next(e for e in manifest["collectors"] if e["name"] == "task-manager-db-storage-breakdown")
        assert (breakdown_entry["status"], breakdown_entry.get("reason")) == ("skipped", "not requested"), (
            f"task-manager-db-storage-breakdown = {breakdown_entry}, want skipped with 'not requested'"
        )
        assert STORAGE_BREAKDOWN_MEMBER not in file_names, f"{STORAGE_BREAKDOWN_MEMBER} in a default bundle"

        # Parity content (US2 scenario 1): every collector succeeds on a
        # healthy stack except the optional service that is scaled to zero
        # and the opt-in extras, which are skipped when not requested.
        assert statuses["logs/task-manager-background-svc"] == "skipped"
        failed = {name: status for name, status in statuses.items() if status != "success"}
        failed.pop("logs/task-manager-background-svc")
        for extra in OPT_IN_COLLECTORS:
            assert statuses[extra] == "skipped", f"Opt-in collector {extra} must be skipped when not requested"
            failed.pop(extra)
        # Telemetry export depends on runtime-accumulated snapshots (and the
        # deployment's Infrahub version); a freshly started e2e instance has
        # none, so the collector degrades to skipped rather than failed.
        assert statuses["telemetry"] in ("success", "skipped"), (
            f"telemetry collector = {statuses['telemetry']}, want success or skipped"
        )
        failed.pop("telemetry", None)
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

        # The InfrahubStatus GraphQL result is captured against the real server:
        # this verifies the /graphql endpoint and best-effort token auth end to
        # end, not just the collector's exit status — a GraphQL-level error comes
        # back as HTTP 200 with an "errors" array, so assert on the payload.
        status_doc = json.loads(read_bundle_member(bundle_path, "bundle/server/infrahub_status.json"))
        assert "errors" not in status_doc, f"InfrahubStatus query returned errors: {status_doc.get('errors')}"
        assert "schema_hash_synced" in status_doc["data"]["InfrahubStatus"]["summary"], (
            f"InfrahubStatus result missing summary.schema_hash_synced: {status_doc}"
        )

        # Spec 006 FR-001: the database total is the size PostgreSQL reports, give or
        # take what the task manager wrote since the collection.
        owner, database = _task_manager_db_owner_and_database(project)
        assert storage["database"] == database, f"storage.json names {storage['database']!r}, want {database!r}"
        live_bytes = int(
            _task_manager_db_query(project, owner, database, "SELECT pg_database_size(current_database())").strip()
        )
        reported_bytes = storage["database_size"]["bytes"]
        assert abs(reported_bytes - live_bytes) <= STORAGE_SIZE_TOLERANCE_BYTES, (
            f"storage.json database size {reported_bytes} differs from pg_database_size {live_bytes} "
            f"by more than {STORAGE_SIZE_TOLERANCE_BYTES} bytes"
        )

        # SC-002: the storage collector adds under 10 seconds to the run.
        storage_duration = _collector_duration(result.stderr, "task-manager-db-storage")
        print(f"task-manager-db-storage collection time: {storage_duration}")
        assert storage_duration < STORAGE_COLLECTION_BUDGET, (
            f"task-manager-db-storage took {storage_duration}, want under {STORAGE_COLLECTION_BUDGET}"
        )

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

    async def test_collect_task_manager_storage_breakdown(
        self, infrahub_compose, infrahub_port, collect_binary, tmp_path
    ):
        """Spec 006 User Story 2 (quickstart scenario 3): the storage breakdown on request.

        Seeds completed flow runs and one flow run cancelled before it starts,
        then collects the breakdown with --include-task-manager-storage and,
        in a second run, with INFRAHUB_INCLUDE_TASK_MANAGER_STORAGE and no
        flag; both give the same outcome (FR-004 to FR-007, FR-013).
        """
        project = infrahub_compose.project_name
        owner, database = _task_manager_db_owner_and_database(project)
        await _seed_storage_breakdown(project, owner, database)

        flag_dir = tmp_path / "flag"
        result = run_collect(
            collect_binary,
            ["--project", project, "--output-dir", str(flag_dir), "create", "--include-task-manager-storage"],
        )
        _assert_seeded_breakdown(flag_dir, result.stdout + result.stderr, database)

        env_dir = tmp_path / "env"
        result = run_collect(
            collect_binary,
            ["--project", project, "--output-dir", str(env_dir), "create"],
            env={"INFRAHUB_INCLUDE_TASK_MANAGER_STORAGE": "true"},
        )
        _assert_seeded_breakdown(env_dir, result.stdout + result.stderr, database)

    async def test_collect_task_manager_storage_time_limit(
        self, infrahub_compose, infrahub_port, collect_binary, tmp_path
    ):
        """Spec 006 quickstart scenario 4: PostgreSQL stops a query at its time limit (FR-009, FR-016).

        A separate session holds an ACCESS EXCLUSIVE lock on artifact while
        the tool runs with both limits lowered to 5 seconds. Measuring the
        size of artifact waits for the same lock (research R5), so the storage
        figures fail too. The lock session is stopped before the next test.
        """
        project = infrahub_compose.project_name
        output_dir = tmp_path / "bundles"
        owner, database = _task_manager_db_owner_and_database(project)
        timed_out = f"timed out after {LOWERED_DB_QUERY_LIMIT}"

        lock = _start_artifact_lock(project, owner, database)
        try:
            await _wait_for_artifact_lock(project, owner, database, lock)
            # run_collect raises on a non-zero exit, so returning proves exit 0
            result = run_collect(
                collect_binary,
                ["--project", project, "--output-dir", str(output_dir), "create", "--include-task-manager-storage"],
                env={DB_QUERY_TIMEOUT_ENV: LOWERED_DB_QUERY_LIMIT},
            )
            # Read after the tool exited and while the lock is still held: a
            # query that PostgreSQL had not stopped would still wait for it.
            lock_state = _artifact_lock_state(project, owner, database)
            tool_sessions = _task_manager_db_query(
                project,
                owner,
                database,
                "SELECT count(*) FROM pg_stat_activity WHERE application_name = 'infrahub-collect'",
            ).strip()
        finally:
            await _release_artifact_lock(project, owner, database, lock)

        assert lock_state == (1, 1), f"The lock on artifact ended before the run did: (sessions, locks) = {lock_state}"
        assert tool_sessions == "0", f"{tool_sessions} infrahub-collect sessions left in PostgreSQL after the run"

        file_names, entries, breakdown = _read_storage_breakdown(output_dir)
        output = result.stdout + result.stderr

        # The always-collected figures reach their limit too, and leave their
        # diagnostics instead of storage.json (FR-011).
        storage_entry = entries["task-manager-db-storage"]
        assert (storage_entry["status"], storage_entry.get("reason")) == ("failed", timed_out), (
            f"task-manager-db-storage = {storage_entry}, want failed with {timed_out!r}"
        )
        assert STORAGE_ERROR_MEMBER in file_names, f"{STORAGE_ERROR_MEMBER} missing after a timed-out query"
        assert STORAGE_REPORT_MEMBER not in file_names, f"{STORAGE_REPORT_MEMBER} written by a timed-out query"

        # FR-016: the locked table fails, the other tables are still collected.
        assert breakdown["time_limit_per_table"] == LOWERED_DB_QUERY_LIMIT, breakdown["time_limit_per_table"]
        tables = {table["name"]: table for table in breakdown["tables"]}
        assert (tables["artifact"]["status"], tables["artifact"].get("reason")) == ("failed", timed_out), (
            f"artifact = {tables['artifact']}, want failed with {timed_out!r}"
        )
        not_collected = {name: t for name, t in tables.items() if name != "artifact" and t["status"] != "collected"}
        assert not not_collected, f"Tables other than artifact not collected: {not_collected}"
        breakdown_entry = entries["task-manager-db-storage-breakdown"]
        want_reason = f"partial storage breakdown: artifact: {timed_out}"
        assert (breakdown_entry["status"], breakdown_entry.get("reason")) == ("failed", want_reason), (
            f"task-manager-db-storage-breakdown = {breakdown_entry}, want failed with {want_reason!r}"
        )

        # FR-013: the progress output names the lowered limit and the failed table.
        _assert_breakdown_progress(output, LOWERED_DB_QUERY_LIMIT)
        assert f"Table artifact: {timed_out}" in output, f"No 'Table artifact: {timed_out}' warning:\n{output}"

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
            await wait_for_http(f"http://localhost:{infrahub_port}/api/config", timeout=180.0, interval=1.0)
