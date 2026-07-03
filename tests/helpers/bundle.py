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
    "server-info",
    "metrics",
    "benchmark",
    "backup",
}

# Opt-in extras: always present in the manifest, skipped ("not requested")
# unless their flag is set.
OPT_IN_COLLECTORS = {"backup", "benchmark"}

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
