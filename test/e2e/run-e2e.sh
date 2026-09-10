#!/usr/bin/env bash
#
# End-to-end backup -> wipe -> restore round-trip for the Plakar backend, run
# against a throwaway Infrahub-shaped Compose stack.
#
#   ./test/e2e/run-e2e.sh community          # neo4j+offline:// (offline dump/load)
#   ./test/e2e/run-e2e.sh enterprise         # neo4j:// (online backup/restore)
#   ENCRYPT=1 ./test/e2e/run-e2e.sh community   # same, against an encrypted repo
#
# What it proves: that a backup taken through the Plakar backend can be restored
# into an emptied deployment. The wipe is ASSERTED EMPTY before restoring — see
# "Why the wipe is gated" below; without that the test passes even when restore
# does nothing at all.
#
# Requirements: docker + docker compose, and a Linux tool binary for the runner
# (see INFRAHUB_RUNNER_BINARY below). Neo4j Enterprise additionally needs the
# enterprise image, which carries a licence acceptance flag.
set -uo pipefail

EDITION="${1:-community}"
case "$EDITION" in
  community|enterprise) ;;
  *) echo "usage: $0 <community|enterprise>   (got '$EDITION')" >&2; exit 2 ;;
esac

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(git -C "$HERE" rev-parse --show-toplevel)"
COMPOSE="$HERE/docker-compose.${EDITION}.yml"
BACKUP="${INFRAHUB_BACKUP_BIN:-$REPO_ROOT/bin/infrahub-backup}"

# The co-located runner container executes the tool binary, so on a non-Linux
# host it must be given a cross-built one:
#   GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
#     go build -o bin/infrahub-backup-linux ./src/cmd/infrahub-backup
if [ -z "${INFRAHUB_RUNNER_BINARY:-}" ] && [ -x "$REPO_ROOT/bin/infrahub-backup-linux" ]; then
  export INFRAHUB_RUNNER_BINARY="$REPO_ROOT/bin/infrahub-backup-linux"
fi

# Credentials the tool would otherwise discover by exec'ing into the deployment.
# They must match the compose file's values.
export INFRAHUB_DB_DATABASE=neo4j
export INFRAHUB_DB_USERNAME=neo4j
export INFRAHUB_DB_PASSWORD=e2epassword
export PREFECT_SERVER_DATABASE_CONNECTION_URL="postgresql+asyncpg://prefect:prefectpass@task-manager-db:5432/prefect"

ENCRYPT="${ENCRYPT:-0}"
PROJ="e2e-${EDITION}"
KREPO="${TMPDIR:-/tmp}/e2e-${EDITION}-repo"
CREATE_EXTRA=()
if [ "$ENCRYPT" = "1" ]; then
  export INFRAHUB_BACKUP_PASSPHRASE="${INFRAHUB_BACKUP_PASSPHRASE:-e2e-correct-horse-staple}"
  CREATE_EXTRA=(--encrypt)
  PROJ="${PROJ}-enc"
  KREPO="${TMPDIR:-/tmp}/e2e-${EDITION}-enc-repo"
fi

# REPO_SPELLING=uri runs the same round-trip against `fs://<path>` instead of a bare
# path. Both are documented forms and they took different code paths in the runner:
# the URI spelling was classified as remote, so the repository directory was never
# bind-mounted and the worker was handed a host path that does not exist in the
# container. Every recipe here used the bare form, which is exactly why that went
# unnoticed — so the URI form is worth running deliberately.
case "${REPO_SPELLING:-path}" in
  path) REPO_ARG="$KREPO" ;;
  uri)  REPO_ARG="fs://$KREPO" ;;
  *) echo "REPO_SPELLING must be 'path' or 'uri' (got '${REPO_SPELLING}')" >&2; exit 2 ;;
esac

DC=(docker compose -p "$PROJ" -f "$COMPOSE")

say() { printf '\n=== %s ===\n' "$*"; }
cy()  { "${DC[@]}" exec -T database \
          cypher-shell -u neo4j -p "$INFRAHUB_DB_PASSWORD" --format plain "$1" 2>/dev/null; }
pg()  { "${DC[@]}" exec -T task-manager-db psql -U prefect -d prefect -tAc "$1" 2>/dev/null; }
ncount() { cy 'MATCH (n:E2E) RETURN count(n);' | tail -1 | tr -dc '0-9'; }
pcount() { pg 'SELECT count(*) FROM e2e_marker;' | tr -dc '0-9'; }

# Neo4j is stopped and restarted around the offline backup and around every
# restore. A count taken before Bolt is answering silently returns empty, so
# without waiting here the wipe below can fail unnoticed and the round-trip
# "passes" against data that was never deleted.
wait_bolt() {
  for _ in $(seq 1 60); do
    [ -n "$(ncount)" ] && return 0
    sleep 3
  done
  echo "  ERROR: Bolt did not come back" >&2
  return 1
}

cleanup() { say "teardown $PROJ"; "${DC[@]}" down -v --remove-orphans >/dev/null 2>&1; }
trap cleanup EXIT

[ -x "$BACKUP" ] || { echo "tool binary not found at $BACKUP — run 'make build'" >&2; exit 1; }

say "start throwaway stack ($EDITION, encrypt=$ENCRYPT)"
rm -rf "$KREPO"
"${DC[@]}" up -d --wait --wait-timeout 300 >/dev/null 2>&1 || {
  echo "stack failed to become healthy:" >&2; "${DC[@]}" ps; exit 1; }
"${DC[@]}" ps --format '  {{.Service}}	{{.Status}}'

say "seed"
cy "UNWIND range(1,25) AS i CREATE (:E2E {tag:'marker', n:i});" >/dev/null
pg "CREATE TABLE IF NOT EXISTS e2e_marker(id int primary key, note text);" >/dev/null
pg "INSERT INTO e2e_marker SELECT g, 'row-'||g FROM generate_series(1,12) g ON CONFLICT DO NOTHING;" >/dev/null
N0="$(ncount)"; P0="$(pcount)"
echo "  neo4j E2E nodes=$N0  postgres e2e_marker rows=$P0"
[ "$N0" = "25" ] && [ "$P0" = "12" ] || { echo "  seed failed" >&2; exit 1; }

say "backup"
"$BACKUP" create --backend plakar --repo "$REPO_ARG" --project "$PROJ" \
  "${CREATE_EXTRA[@]+"${CREATE_EXTRA[@]}"}" --force 2>&1 | tail -20
BRC="${PIPESTATUS[0]}"
[ "$BRC" = "0" ] || { echo "  backup failed (exit $BRC)" >&2; exit 1; }

say "snapshots"
"$BACKUP" --backend plakar --repo "$REPO_ARG" snapshots list 2>&1 | tail -6

if [ "$ENCRYPT" = "1" ]; then
  say "negative: listing an encrypted repo without the passphrase must fail"
  ( unset INFRAHUB_BACKUP_PASSPHRASE
    "$BACKUP" --backend plakar --repo "$REPO_ARG" snapshots list 2>&1 | tail -1 )
fi

say "wipe"
wait_bolt || exit 1
cy "MATCH (n:E2E) DETACH DELETE n;" >/dev/null
pg "DELETE FROM e2e_marker;" >/dev/null
NW="$(ncount)"; PW="$(pcount)"
echo "  neo4j=${NW:-<unreachable>} postgres=${PW:-<unreachable>}"
# Hard gate: if the wipe did not take, a restore proves nothing. Fail loudly
# rather than report a false PASS.
if [ "$NW" != "0" ] || [ "$PW" != "0" ]; then
  echo "  ABORT: wipe did not take effect — the restore would be unverifiable" >&2
  exit 1
fi

say "restore"
"$BACKUP" restore --backend plakar --repo "$REPO_ARG" --project "$PROJ" --force 2>&1 | tail -20
RRC="${PIPESTATUS[0]}"
[ "$RRC" = "0" ] || { echo "  restore failed (exit $RRC)" >&2; exit 1; }

say "verify"
wait_bolt || exit 1
N1="$(ncount)"; P1="$(pcount)"
echo "  neo4j nodes  : before=$N0 after=${N1:-<unreachable>}"
echo "  postgres rows: before=$P0 after=${P1:-<unreachable>}"
if [ "$N1" = "$N0" ] && [ "$P1" = "$P0" ]; then
  echo ""; echo "  RESULT: PASS ($EDITION, encrypt=$ENCRYPT)"; exit 0
fi
echo ""; echo "  RESULT: FAIL ($EDITION, encrypt=$ENCRYPT)"; exit 1
