"""Helpers for the traces end-to-end tests (INFP-744).

They talk to a Tempo over its HTTP API: post OTLP JSON traces, search them,
and read them back. They also read the traces written in a bundle.
"""

import json
import os
import secrets
import tarfile
import time
from pathlib import Path

import requests

PROJECT_ROOT = Path(__file__).resolve().parents[2]
ALLOW_LIST_PATH = PROJECT_ROOT / "src" / "internal" / "app" / "traces_attributes.json"
TEMPO_IMAGE = "grafana/tempo:2.10.3"
TEMPO_CONFIG_PATH = PROJECT_ROOT / "tests" / "e2e" / "fixtures" / "tempo.yaml"


def load_allow_list() -> dict:
    """Return the allow-list file of the traces collector."""
    return json.loads(ALLOW_LIST_PATH.read_text())


def known_attribute_keys(allow: dict) -> set[str]:
    """Return every key the allow-list file names, kept or removed.

    A removed entry that ends with ".*" names a prefix, such as
    prefect.run.parameter.* for the parameters of each Prefect flow; it is
    returned as is and matched by is_known_key.
    """
    keys = set(allow["resource"]) | set(allow["removed"])
    for section in ("by_span_name", "by_span_kind"):
        for values in allow[section].values():
            keys |= set(values)
    return keys


def is_known_key(key: str, known: set[str]) -> bool:
    """Return whether the allow-list file names key, directly or by a prefix entry."""
    if key in known:
        return True
    return any(entry.endswith(".*") and key.startswith(entry[:-1]) for entry in known)


def synthetic_trace(
    service: str,
    duration_s: float,
    span_count: int = 1,
    attribute_size: int = 0,
    end_ns: int | None = None,
) -> tuple[str, dict]:
    """Build an OTLP JSON trace that ends now and lasts duration_s.

    The first span is the root; the others are its children, named
    execute_db_query_with_metadata so that their query_name attribute stays
    in the bundle. attribute_size pads query_name to make large traces.
    Returns the hex trace ID and the OTLP document.
    """
    trace_id = secrets.token_hex(16)
    end = end_ns or time.time_ns()
    start = end - int(duration_s * 1_000_000_000)
    root_id = secrets.token_hex(8)
    spans = [
        {
            "traceId": trace_id,
            "spanId": root_id,
            "name": "POST /graphql",
            "kind": 2,
            "startTimeUnixNano": str(start),
            "endTimeUnixNano": str(end),
            "attributes": [
                {"key": "http.method", "value": {"stringValue": "POST"}},
                {"key": "http.route", "value": {"stringValue": "/graphql"}},
                {"key": "http.status_code", "value": {"intValue": "200"}},
            ],
        }
    ]
    for index in range(span_count - 1):
        spans.append(
            {
                "traceId": trace_id,
                "spanId": secrets.token_hex(8),
                "parentSpanId": root_id,
                "name": "execute_db_query_with_metadata",
                "kind": 1,
                "startTimeUnixNano": str(start + index),
                "endTimeUnixNano": str(start + index + 1000),
                "attributes": [
                    {"key": "query_name", "value": {"stringValue": f"query-{index}-" + "x" * attribute_size}},
                    {"key": "rows", "value": {"intValue": "1"}},
                ],
            }
        )
    document = {
        "resourceSpans": [
            {
                "resource": {"attributes": [{"key": "service.name", "value": {"stringValue": service}}]},
                "scopeSpans": [{"spans": spans}],
            }
        ]
    }
    return trace_id, document


def post_trace(otlp_http_url: str, document: dict | bytes) -> requests.Response:
    """Post one OTLP JSON document to the /v1/traces endpoint of a Tempo."""
    body = document if isinstance(document, bytes) else json.dumps(document).encode()
    response = requests.post(
        f"{otlp_http_url}/v1/traces",
        data=body,
        headers={"Content-Type": "application/json"},
        timeout=30,
    )
    return response


def search_traces(tempo_url: str, query: str = "{}", since_s: int = 7200, limit: int = 1000) -> list[dict]:
    """Return the trace summaries that Tempo finds for a TraceQL query."""
    now = int(time.time())
    response = requests.get(
        f"{tempo_url}/api/search",
        params={"q": query, "start": now - since_s, "end": now + 60, "limit": limit},
        timeout=30,
    )
    response.raise_for_status()
    return response.json().get("traces") or []


def collector_search(tempo_url: str, min_duration: str, since_s: int = 7200) -> list[dict]:
    """Return the trace summaries of the search that infrahub-collect sends.

    It uses the query, the limit and the window end of searchSlowTraces in
    src/internal/app/collect_traces.go (first round, tracesSearchLimit). The
    query `{}` can report another duration for the same trace, so a check of
    the order of the bundle files reads the durations of this search.
    """
    now = int(time.time())
    response = requests.get(
        f"{tempo_url}/api/search",
        params={
            "q": f"{{ traceDuration > {min_duration} }}",
            "start": now - since_s,
            "end": now,
            "limit": 10000,
        },
        timeout=30,
    )
    response.raise_for_status()
    return response.json().get("traces") or []


def wait_for_traces(tempo_url: str, predicate, query: str = "{}", timeout: float = 180.0) -> list[dict]:
    """Search Tempo until predicate(traces) is true, and return the traces."""
    deadline = time.time() + timeout
    traces: list[dict] = []
    while time.time() < deadline:
        try:
            traces = search_traces(tempo_url, query)
            if predicate(traces):
                return traces
        except requests.RequestException:
            pass
        time.sleep(3)
    raise TimeoutError(f"Tempo at {tempo_url} did not return the expected traces in {timeout}s; last: {traces}")


def fetch_trace(tempo_url: str, trace_id: str) -> dict:
    """Return the OTLP JSON document of one trace, as Tempo stores it."""
    response = requests.get(
        f"{tempo_url}/api/v2/traces/{trace_id}",
        headers={"Accept": "application/json"},
        timeout=30,
    )
    response.raise_for_status()
    return response.json()["trace"]


def wait_for_trace(tempo_url: str, trace_id: str, timeout: float = 120.0) -> dict:
    """Fetch one trace by ID until Tempo has it."""
    deadline = time.time() + timeout
    last_error: Exception | None = None
    while time.time() < deadline:
        try:
            return fetch_trace(tempo_url, trace_id)
        except requests.RequestException as exc:
            last_error = exc
        time.sleep(3)
    raise TimeoutError(f"Tempo at {tempo_url} has no trace {trace_id} after {timeout}s: {last_error}")


def attribute_keys(trace: dict) -> tuple[set[str], set[str]]:
    """Return the resource attribute keys and the span attribute keys of a trace."""
    resource_keys: set[str] = set()
    span_keys: set[str] = set()
    for resource_spans in trace.get("resourceSpans", []):
        for attribute in (resource_spans.get("resource") or {}).get("attributes", []):
            resource_keys.add(attribute["key"])
        for scope in resource_spans.get("scopeSpans", []):
            for span in scope.get("spans", []):
                for attribute in span.get("attributes", []):
                    span_keys.add(attribute["key"])
    return resource_keys, span_keys


def attribute_values(trace: dict, key: str) -> set[str]:
    """Return the string values of one span attribute key in a trace."""
    values: set[str] = set()
    for resource_spans in trace.get("resourceSpans", []):
        for scope in resource_spans.get("scopeSpans", []):
            for span in scope.get("spans", []):
                for attribute in span.get("attributes", []):
                    if attribute["key"] == key:
                        value = attribute.get("value", {})
                        values.update(str(v) for v in value.values())
    return values


def bundle_trace_files(bundle_path: Path) -> dict[str, bytes]:
    """Return the trace files of a bundle, by file name, in name order."""
    files: dict[str, bytes] = {}
    with tarfile.open(bundle_path, "r:gz") as tar:
        for member in tar.getmembers():
            if member.isfile() and member.name.startswith("bundle/traces/"):
                extracted = tar.extractfile(member)
                assert extracted is not None
                files[os.path.basename(member.name)] = extracted.read()
    return dict(sorted(files.items()))


def bundle_file_contents(bundle_path: Path) -> dict[str, bytes]:
    """Return every file of a bundle, by path."""
    contents: dict[str, bytes] = {}
    with tarfile.open(bundle_path, "r:gz") as tar:
        for member in tar.getmembers():
            if member.isfile():
                extracted = tar.extractfile(member)
                assert extracted is not None
                contents[member.name] = extracted.read()
    return contents


def trace_id_of(file_name: str) -> str:
    """Return the hex trace ID in a file name such as 0001-<id>.json."""
    return file_name.removesuffix(".json").split("-", 1)[1]


def traces_entry(manifest: dict) -> dict:
    """Return the traces entry of bundle_information.json."""
    return next(entry for entry in manifest["collectors"] if entry["name"] == "traces")
