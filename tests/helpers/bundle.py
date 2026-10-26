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
    "benchmark",
    "backup",
}

# Opt-in extras: always present in the manifest, skipped ("not requested")
# unless their flag is set.
OPT_IN_COLLECTORS = {"backup", "benchmark"}

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
