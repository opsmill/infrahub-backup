#!/usr/bin/env bash
# Build the infrahub-backup image and check that it carries the database tools (FR-030, ADR 0017).
#
# Each tool runs inside the image as the image's own user (no --user override), so the check fails
# if a tool is missing, cannot start, or needs root. The image must not run as root.
#
# Usage: scripts/check-image-tools.sh [image-tag]
#   image-tag  tag to build and check (default: infrahub-backup:check-tools)
# Environment:
#   SKIP_BUILD=1  check an existing image instead of building it
set -euo pipefail

IMAGE="${1:-infrahub-backup:check-tools}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if [[ "${SKIP_BUILD:-0}" != "1" ]]; then
    echo "Building ${IMAGE} from ${REPO_ROOT}..."
    docker build --build-arg VERSION="${VERSION:-}" -t "${IMAGE}" "${REPO_ROOT}"
fi

uid="$(docker run --rm --entrypoint id "${IMAGE}" -u)"
user="$(docker image inspect "${IMAGE}" --format '{{.Config.User}}')"
echo "Image user: ${user} (uid ${uid})"
if [[ "${uid}" == "0" ]]; then
    echo "FAIL: the image runs as root" >&2
    exit 1
fi

failed=0
for tool in neo4j-admin cypher-shell pg_dump pg_restore psql; do
    if output="$(docker run --rm --entrypoint "${tool}" "${IMAGE}" --version 2>&1)"; then
        echo "ok   ${tool}: ${output}"
    else
        echo "FAIL ${tool}: ${output}" >&2
        failed=1
    fi
done

size="$(docker image inspect "${IMAGE}" --format '{{.Size}}')"
echo "Image size: ${size} bytes ($((size / 1024 / 1024)) MiB)"

if [[ "${failed}" != "0" ]]; then
    echo "FAIL: at least one database tool is missing or does not run" >&2
    exit 1
fi
echo "All database tools present in ${IMAGE}"
