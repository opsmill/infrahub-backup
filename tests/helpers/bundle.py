"""Shared assertions for troubleshooting-bundle e2e tests.

Used by both test_docker_collect.py and test_k8s_collect.py so the
cross-environment parity promised by spec 003-collect-tool (FR-006 / SC-004)
is asserted with the same code on both sides.
"""

import json
import re
import tarfile
from datetime import datetime
from pathlib import Path

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
    "task-manager-db-storage",
    "server-info",
    "telemetry",
    "metrics",
    "task-manager-db-storage-breakdown",
    "benchmark",
    "backup",
}

# Opt-in extras: always present in the manifest, skipped ("not requested")
# unless their flag is set.
OPT_IN_COLLECTORS = {"task-manager-db-storage-breakdown", "backup", "benchmark"}

# Bundle directory owned by each non-log collector (contracts/bundle-layout.md;
# task-manager-db/ is specs/006-taskmgr-db-storage/contracts/manifest-outcomes.md).
# Each directory is also an allowed top-level bundle entry (ALLOWED_TOP_LEVEL).
COLLECTOR_DIRS = {
    "database-logs": "database",
    "message-queue-status": "message-queue",
    "cache-status": "cache",
    "task-worker-state": "task-worker",
    "task-manager-state": "task-manager",
    "task-manager-db-storage": "task-manager-db",
    "server-info": "server",
    "telemetry": "telemetry",
    "metrics": "metrics",
}

ALLOWED_TOP_LEVEL = {"bundle_information.json", "logs", "benchmark"} | set(COLLECTOR_DIRS.values())

COLLECT_ID_RE = re.compile(r"^[0-9]{8}_[0-9]{6}$")

# Storage figures of the task manager database (spec 006-taskmgr-db-storage).
STORAGE_REPORT_MEMBER = "bundle/task-manager-db/storage.json"
STORAGE_ERROR_MEMBER = "bundle/task-manager-db/storage.err.txt"
STORAGE_BREAKDOWN_MEMBER = "bundle/task-manager-db/storage-breakdown.json"
STORAGE_BREAKDOWN_ERROR_MEMBER = "bundle/task-manager-db/storage-breakdown.err.txt"

# Field sets and patterns of specs/006-taskmgr-db-storage/contracts/storage.schema.json.
STORAGE_REPORT_FIELDS = {"collected_at", "database", "database_size", "statistics_reset_at", "tables"}
STORAGE_TABLE_FIELDS = {
    "schema",
    "name",
    "total_size",
    "data_size",
    "index_size",
    "toast_size",
    "live_rows",
    "dead_rows",
    "last_vacuum",
    "last_autovacuum",
}
STORAGE_SIZE_FIELDS = {"bytes", "readable"}
STORAGE_READABLE_RE = re.compile(r"^[0-9]+(\.[0-9])? (B|KB|MB|GB|TB|PB|EB)$")
# RFC 3339 date-time (JSON Schema "format": "date-time").
RFC3339_RE = re.compile(
    r"^[0-9]{4}-[0-9]{2}-[0-9]{2}[Tt][0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?([Zz]|[+-][0-9]{2}:[0-9]{2})$"
)

# Field sets, enums and patterns of
# specs/006-taskmgr-db-storage/contracts/storage-breakdown.schema.json.
BREAKDOWN_FIELDS = {"collected_at", "database", "time_limit_per_table", "tables"}
BREAKDOWN_TABLE_REQUIRED_FIELDS = {"name", "status", "grouped_by", "timestamp_column"}
BREAKDOWN_TABLE_OPTIONAL_FIELDS = {"reason", "rows", "groups"}
BREAKDOWN_GROUP_REQUIRED_FIELDS = {"group", "rows", "oldest", "newest"}
BREAKDOWN_GROUP_OPTIONAL_FIELDS = {"rows_without_start_time"}
BREAKDOWN_TABLE_STATUSES = {"collected", "absent", "failed"}
BREAKDOWN_KEY_NAMES = {"event_name", "flow_name", "state_type", "log_level", "artifact_type"}
BREAKDOWN_TIMESTAMP_COLUMNS = {"occurred", "start_time", "timestamp", "created"}
BREAKDOWN_TIME_LIMIT_RE = re.compile(r"^[0-9].*s$")
# The breakdown list in collection order, with the group keys and timestamp
# column of each table (data-model.md, research R7). The schema's name enum
# holds the same seven tables.
BREAKDOWN_TABLES = {
    "events": (["event_name"], "occurred"),
    "flow_run": (["flow_name", "state_type"], "start_time"),
    "task_run": (["flow_name", "state_type"], "start_time"),
    "flow_run_state": (["state_type"], "timestamp"),
    "task_run_state": (["state_type"], "timestamp"),
    "log": (["flow_name", "log_level"], "timestamp"),
    "artifact": (["artifact_type"], "created"),
}
# Only the run tables count the rows without start_time (FR-007).
BREAKDOWN_RUN_TABLES = {"flow_run", "task_run"}


def find_bundle(output_dir: Path) -> Path:
    bundles = sorted(output_dir.glob("support_bundle_*.tar.gz"))
    assert bundles, f"No support bundle found in {output_dir}"
    return bundles[-1]


def read_bundle_archive(bundle_path: Path) -> tuple[set[str], dict]:
    """Validate archive integrity and return (file member names, manifest).

    Asserts every member lives under bundle/ and fully decompresses each file
    so a truncated or corrupt archive fails here.
    """
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
    return file_names, manifest


def read_bundle_member(bundle_path: Path, member: str) -> str:
    """Return the text content of one file inside the bundle archive."""
    with tarfile.open(bundle_path, "r:gz") as tar:
        extracted = tar.extractfile(member)
        assert extracted is not None, f"{member} missing from archive"
        return extracted.read().decode()


def validate_manifest_schema(manifest: dict) -> None:
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
        if "artifact" in entry:
            assert isinstance(entry["artifact"], str) and entry["artifact"], (
                f"Collector {entry['name']!r} has a non-string or empty artifact: {entry['artifact']!r}"
            )
            assert entry["status"] == "success", (
                f"Collector {entry['name']!r} carries an artifact but is {entry['status']}"
            )


def assert_collector_outcomes(manifest: dict) -> dict[str, str]:
    """SC-005: one manifest entry per planned collector, no duplicates.

    Returns the {collector name: status} mapping for further assertions.
    """
    names = [entry["name"] for entry in manifest["collectors"]]
    assert len(names) == len(set(names)), f"Duplicate collector entries: {names}"
    assert set(names) == EXPECTED_COLLECTORS, (
        f"Manifest collector set mismatch: missing {EXPECTED_COLLECTORS - set(names)}, "
        f"unexpected {set(names) - EXPECTED_COLLECTORS}"
    )
    return {entry["name"]: entry["status"] for entry in manifest["collectors"]}


def assert_bundle_layout(file_names: set[str], statuses: dict[str, str]) -> None:
    """Layout per contracts/bundle-layout.md: successful collectors own files."""
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


def _assert_exact_fields(obj: object, fields: set[str], where: str) -> None:
    """Assert an object has every required field and no other (additionalProperties: false)."""
    assert isinstance(obj, dict), f"{where} must be an object, got {type(obj).__name__}"
    missing = fields - obj.keys()
    unexpected = obj.keys() - fields
    assert not missing, f"{where} is missing required fields {sorted(missing)}"
    assert not unexpected, f"{where} has fields the schema does not allow: {sorted(unexpected)}"


def _assert_non_negative_int(value: object, where: str) -> None:
    assert isinstance(value, int) and not isinstance(value, bool), (
        f"{where} must be an integer, got {type(value).__name__}"
    )
    assert value >= 0, f"{where} must be >= 0, got {value}"


def _assert_non_empty_str(value: object, where: str) -> None:
    assert isinstance(value, str) and value, f"{where} must be a non-empty string, got {value!r}"


def _assert_timestamp(value: object, where: str, nullable: bool = False) -> None:
    """Assert an RFC 3339 date-time, or null where the schema allows it."""
    if nullable and value is None:
        return
    assert isinstance(value, str) and RFC3339_RE.match(value), f"{where} is not an RFC 3339 date-time: {value!r}"
    try:
        datetime.fromisoformat(value.upper())
    except ValueError as exc:  # an impossible date or time, such as 2026-02-30
        raise AssertionError(f"{where} is not a valid date-time: {value!r} ({exc})") from exc


def _assert_size(size: object, where: str) -> int:
    """Assert one {bytes, readable} size and return its bytes."""
    _assert_exact_fields(size, STORAGE_SIZE_FIELDS, where)
    assert isinstance(size, dict)  # narrowed for type checkers; asserted above
    _assert_non_negative_int(size["bytes"], f"{where}.bytes")
    readable = size["readable"]
    assert isinstance(readable, str) and STORAGE_READABLE_RE.match(readable), (
        f"{where}.readable {readable!r} does not match {STORAGE_READABLE_RE.pattern}"
    )
    return size["bytes"]


def assert_storage_report(doc: dict) -> None:
    """Assert storage.json conformance with specs/006-taskmgr-db-storage/contracts/storage.schema.json.

    jsonschema is not a test dependency; the schema's constraints are asserted
    manually, as validate_manifest_schema does for the manifest: required
    fields and no others, types and minimums, the readable-size pattern, RFC
    3339 or null timestamps. It also asserts what the schema cannot express
    (research R5, data-model.md): for every table, data + index + TOAST sizes
    equal the total, and the tables are ordered by total size, largest first,
    then by schema and name.
    """
    _assert_exact_fields(doc, STORAGE_REPORT_FIELDS, "storage.json")
    _assert_timestamp(doc["collected_at"], "collected_at")
    _assert_non_empty_str(doc["database"], "database")
    _assert_size(doc["database_size"], "database_size")
    _assert_timestamp(doc["statistics_reset_at"], "statistics_reset_at", nullable=True)

    tables = doc["tables"]
    assert isinstance(tables, list), f"tables must be an array, got {type(tables).__name__}"
    order_keys = []
    for index, table in enumerate(tables):
        where = f"tables[{index}]"
        _assert_exact_fields(table, STORAGE_TABLE_FIELDS, where)
        _assert_non_empty_str(table["schema"], f"{where}.schema")
        _assert_non_empty_str(table["name"], f"{where}.name")
        where = f"table {table['schema']}.{table['name']}"
        total = _assert_size(table["total_size"], f"{where} total_size")
        data = _assert_size(table["data_size"], f"{where} data_size")
        indexes = _assert_size(table["index_size"], f"{where} index_size")
        toast = _assert_size(table["toast_size"], f"{where} toast_size")
        assert data + indexes + toast == total, (
            f"{where}: data {data} + index {indexes} + TOAST {toast} = {data + indexes + toast}, total is {total}"
        )
        _assert_non_negative_int(table["live_rows"], f"{where} live_rows")
        _assert_non_negative_int(table["dead_rows"], f"{where} dead_rows")
        _assert_timestamp(table["last_vacuum"], f"{where} last_vacuum", nullable=True)
        _assert_timestamp(table["last_autovacuum"], f"{where} last_autovacuum", nullable=True)
        order_keys.append((-total, table["schema"], table["name"]))
    assert order_keys == sorted(order_keys), (
        "tables are not ordered by total size (largest first), then schema and name: "
        f"{[(schema, name, -negative_total) for negative_total, schema, name in order_keys]}"
    )


def assert_storage_collected(bundle_path: Path, file_names: set[str], manifest: dict) -> dict:
    """Assert the task-manager-db-storage collector succeeded and its storage.json is valid.

    Shared by the Docker and Kubernetes tests so both check the same contract.
    On failure the message carries the manifest reason and storage.err.txt,
    which hold the cause. Returns the parsed storage.json.
    """
    entry = next(entry for entry in manifest["collectors"] if entry["name"] == "task-manager-db-storage")
    if entry["status"] != "success":
        diagnostics = (
            read_bundle_member(bundle_path, STORAGE_ERROR_MEMBER)
            if STORAGE_ERROR_MEMBER in file_names
            else "<no storage.err.txt>"
        )
        raise AssertionError(f"task-manager-db-storage = {entry}, want success; storage.err.txt:\n{diagnostics}")
    assert STORAGE_REPORT_MEMBER in file_names, f"{STORAGE_REPORT_MEMBER} missing from a successful run"
    assert STORAGE_ERROR_MEMBER not in file_names, f"{STORAGE_ERROR_MEMBER} present in a successful run"
    doc = json.loads(read_bundle_member(bundle_path, STORAGE_REPORT_MEMBER))
    assert_storage_report(doc)
    return doc


def _assert_fields(obj: object, required: set[str], optional: set[str], where: str) -> None:
    """Assert an object has every required field and nothing outside required and optional fields."""
    assert isinstance(obj, dict), f"{where} must be an object, got {type(obj).__name__}"
    missing = required - obj.keys()
    unexpected = obj.keys() - required - optional
    assert not missing, f"{where} is missing required fields {sorted(missing)}"
    assert not unexpected, f"{where} has fields the schema does not allow: {sorted(unexpected)}"


def _assert_breakdown_group(group: object, grouped_by: list[str], run_table: bool, where: str) -> int:
    """Assert one group of a collected breakdown table and return its rows."""
    _assert_fields(group, BREAKDOWN_GROUP_REQUIRED_FIELDS, BREAKDOWN_GROUP_OPTIONAL_FIELDS, where)
    assert isinstance(group, dict)  # narrowed for type checkers; asserted above
    keys = group["group"]
    assert isinstance(keys, dict), f"{where}.group must be an object, got {type(keys).__name__}"
    for name, value in keys.items():
        assert value is None or isinstance(value, str), f"{where}.group.{name} must be a string or null: {value!r}"
    # data-model.md: one field per name in grouped_by.
    assert sorted(keys) == sorted(grouped_by), f"{where}.group has keys {sorted(keys)}, grouped_by is {grouped_by}"

    rows = group["rows"]
    _assert_non_negative_int(rows, f"{where}.rows")
    assert rows >= 1, f"{where}.rows must be >= 1, got {rows}"
    # data-model.md: rows_without_start_time is only for flow_run and task_run.
    if run_table:
        assert "rows_without_start_time" in group, f"{where} of a run table has no rows_without_start_time"
        without_start = group["rows_without_start_time"]
        _assert_non_negative_int(without_start, f"{where}.rows_without_start_time")
        assert without_start <= rows, f"{where}: {without_start} rows without start_time, more than its {rows} rows"
    else:
        assert "rows_without_start_time" not in group, f"{where} has rows_without_start_time outside a run table"
    _assert_timestamp(group["oldest"], f"{where}.oldest", nullable=True)
    _assert_timestamp(group["newest"], f"{where}.newest", nullable=True)
    return rows


def _breakdown_group_order_key(group: dict) -> tuple:
    """Order of the groups in a table (data-model.md): rows, largest first, then
    the key values in key-name order with null after every string."""
    values = tuple((group["group"][name] is None, group["group"][name] or "") for name in sorted(group["group"]))
    return (-group["rows"], values)


def assert_storage_breakdown(doc: dict) -> None:
    """Assert storage-breakdown.json conformance with
    specs/006-taskmgr-db-storage/contracts/storage-breakdown.schema.json.

    jsonschema is not a test dependency; the schema's constraints are asserted
    manually, as validate_manifest_schema does for the manifest: the required
    fields and no others at the top level, the required fields and no fields
    other than the optional ones in each table and group, the enums, the
    minimums, RFC 3339 or null timestamps, and, for each table, rows and
    groups present only when the table is collected and reason only when it
    is not. It also asserts what the schema cannot express (data-model.md,
    research R7): the seven tables in list order with their documented group
    keys and timestamp column, the group keys equal to grouped_by,
    rows_without_start_time only for the run tables, a table's rows equal to
    the sum of its groups' rows, and the order of the groups.
    """
    _assert_exact_fields(doc, BREAKDOWN_FIELDS, "storage-breakdown.json")
    _assert_timestamp(doc["collected_at"], "collected_at")
    _assert_non_empty_str(doc["database"], "database")
    time_limit = doc["time_limit_per_table"]
    assert isinstance(time_limit, str) and BREAKDOWN_TIME_LIMIT_RE.match(time_limit), (
        f"time_limit_per_table {time_limit!r} does not match {BREAKDOWN_TIME_LIMIT_RE.pattern}"
    )

    tables = doc["tables"]
    assert isinstance(tables, list), f"tables must be an array, got {type(tables).__name__}"
    for index, table in enumerate(tables):
        where = f"tables[{index}]"
        _assert_fields(table, BREAKDOWN_TABLE_REQUIRED_FIELDS, BREAKDOWN_TABLE_OPTIONAL_FIELDS, where)
        name = table["name"]
        assert name in BREAKDOWN_TABLES, f"{where}.name {name!r} is not one of {sorted(BREAKDOWN_TABLES)}"
        where = f"table {name}"
        status = table["status"]
        assert status in BREAKDOWN_TABLE_STATUSES, f"{where} has invalid status {status!r}"

        grouped_by = table["grouped_by"]
        assert isinstance(grouped_by, list) and grouped_by, f"{where}.grouped_by must be a non-empty array"
        for key in grouped_by:
            assert key in BREAKDOWN_KEY_NAMES, (
                f"{where}.grouped_by has {key!r}, not one of {sorted(BREAKDOWN_KEY_NAMES)}"
            )
        column = table["timestamp_column"]
        assert column in BREAKDOWN_TIMESTAMP_COLUMNS, (
            f"{where}.timestamp_column {column!r} is not one of {sorted(BREAKDOWN_TIMESTAMP_COLUMNS)}"
        )
        expected_keys, expected_column = BREAKDOWN_TABLES[name]
        assert grouped_by == expected_keys, f"{where} is grouped by {grouped_by}, want {expected_keys}"
        assert column == expected_column, f"{where} takes its times from {column!r}, want {expected_column!r}"

        if status != "collected":
            assert "rows" not in table and "groups" not in table, f"{where} is {status} but has rows or groups"
            _assert_non_empty_str(table.get("reason"), f"{where}.reason")
            continue
        assert "reason" not in table, f"{where} is collected but has a reason: {table['reason']!r}"
        assert "rows" in table and "groups" in table, f"{where} is collected but has no rows or no groups"
        _assert_non_negative_int(table["rows"], f"{where}.rows")
        groups = table["groups"]
        assert isinstance(groups, list), f"{where}.groups must be an array, got {type(groups).__name__}"
        group_rows = [
            _assert_breakdown_group(group, grouped_by, name in BREAKDOWN_RUN_TABLES, f"{where} groups[{i}]")
            for i, group in enumerate(groups)
        ]
        assert sum(group_rows) == table["rows"], (
            f"{where}: groups add up to {sum(group_rows)} rows, rows is {table['rows']}"
        )
        order_keys = [_breakdown_group_order_key(group) for group in groups]
        assert order_keys == sorted(order_keys), (
            f"{where}: groups are not ordered by rows (largest first), then key values: "
            f"{[(group['rows'], group['group']) for group in groups]}"
        )

    names = [table["name"] for table in tables]
    assert names == list(BREAKDOWN_TABLES), f"tables are {names}, want the breakdown list {list(BREAKDOWN_TABLES)}"
