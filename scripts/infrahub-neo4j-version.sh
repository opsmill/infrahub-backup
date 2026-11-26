#!/usr/bin/env bash
# Print the Neo4j version that the Infrahub Helm chart deploys.
#
# The end-to-end tests start their own Neo4j servers (the external server in CI, the Neo4j
# cluster) at this version, so they always run against the Neo4j that Infrahub ships and never
# against an older one. This script is the one place that answers "which version is that".
# The version is read from the `neo4j` dependency of the Infrahub chart. The enterprise chart
# has no such dependency: it depends on the `infrahub` chart, which is read instead.
#
# Usage: scripts/infrahub-neo4j-version.sh [--image-tag | --check-dockerfile]
#   (no option)         the version as the Neo4j Helm chart writes it, for example 2026.5.0
#   --image-tag         the version as the `neo4j` image tags it, for example 2026.05.0
#   --check-dockerfile  fail when the Neo4j tools in the Dockerfile (ARG NEO4J_VERSION) are
#                       older than the version Infrahub deploys. infrahub-backup refuses to
#                       back up a server that is newer than its tools.
# Environment:
#   INFRAHUB_HELM_CHART  the chart to read (default: the published `infrahub` chart)
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHART="${INFRAHUB_HELM_CHART:-oci://registry.opsmill.io/opsmill/chart/infrahub}"

# dependency_field <dependency name> <field>: read one field of one chart dependency from the
# chart metadata on stdin. The fields of a dependency come in any order.
dependency_field() {
    awk -v want="$1" -v field="$2" '
        function clean(text) { gsub(/^[ \t"\047]+|[ \t"\047]+$/, "", text); return text }
        function flush() { if (name == want && value != "" && !done) { print value; done = 1 } name = ""; value = "" }
        /^dependencies:/ { inside = 1; next }
        inside && /^[^ \t-]/ { flush(); inside = 0 }
        !inside { next }
        /^[ \t]*-[ \t]/ { flush(); sub(/^[ \t]*-[ \t]/, "") }
        {
            line = $0
            sub(/^[ \t]+/, "", line)
            split(line, parts, ":")
            key = parts[1]
            sub(/^[^:]*:/, "", line)
            if (key == "name") name = clean(line)
            if (key == field) value = clean(line)
        }
        END { flush() }
    '
}

show_chart() {
    helm show chart "$@" 2>/dev/null
}

metadata="$(show_chart "${CHART}")" || {
    echo "could not read the chart ${CHART}" >&2
    exit 1
}
version="$(dependency_field neo4j version <<<"${metadata}")"

if [[ -z "${version}" ]]; then
    parent_version="$(dependency_field infrahub version <<<"${metadata}")"
    parent_repository="$(dependency_field infrahub repository <<<"${metadata}")"
    if [[ -z "${parent_version}" ]]; then
        echo "the chart ${CHART} has no neo4j dependency and no infrahub dependency" >&2
        exit 1
    fi
    if [[ "${parent_repository}" == file://* ]]; then
        metadata="$(show_chart "${CHART}/${parent_repository#file://}")"
    else
        metadata="$(show_chart "${parent_repository%/}/infrahub" --version "${parent_version}")"
    fi
    version="$(dependency_field neo4j version <<<"${metadata}")"
fi

if [[ ! "${version}" =~ ^([0-9]+)\.([0-9]+)\.([0-9]+)$ ]]; then
    echo "could not read the Neo4j version from the chart ${CHART}: got '${version}'" >&2
    exit 1
fi
year="${BASH_REMATCH[1]}"
month="${BASH_REMATCH[2]}"
patch="${BASH_REMATCH[3]}"
# The image tags write a calendar version with a two-digit month. A 5.x version is unchanged.
if ((year >= 2025)); then
    image_tag="$(printf '%d.%02d.%d' "${year}" "$((10#${month}))" "${patch}")"
else
    image_tag="${version}"
fi

case "${1:-}" in
"")
    echo "${version}"
    ;;
--image-tag)
    echo "${image_tag}"
    ;;
--check-dockerfile)
    tools="$(sed -n 's/^ARG NEO4J_VERSION=//p' "${REPO_ROOT}/Dockerfile" | head -n 1)"
    if [[ ! "${tools}" =~ ^([0-9]+)\.([0-9]+)\.([0-9]+)$ ]]; then
        echo "could not read ARG NEO4J_VERSION from the Dockerfile: got '${tools}'" >&2
        exit 1
    fi
    tools_key="$(printf '%05d%05d%05d' "$((10#${BASH_REMATCH[1]}))" "$((10#${BASH_REMATCH[2]}))" "$((10#${BASH_REMATCH[3]}))")"
    server_key="$(printf '%05d%05d%05d' "$((10#${year}))" "$((10#${month}))" "$((10#${patch}))")"
    if [[ "${tools_key}" < "${server_key}" ]]; then
        echo "FAIL: the Dockerfile takes the Neo4j tools from ${tools}, but Infrahub deploys Neo4j ${image_tag} (${CHART})." >&2
        echo "infrahub-backup refuses to back up a server that is newer than its tools." >&2
        echo "Set ARG NEO4J_VERSION in the Dockerfile to ${image_tag} or newer." >&2
        exit 1
    fi
    echo "ok: the Neo4j tools (${tools}) are not older than the Neo4j that Infrahub deploys (${image_tag})"
    ;;
*)
    echo "unknown option: $1" >&2
    exit 2
    ;;
esac
