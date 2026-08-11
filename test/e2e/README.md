# Plakar backend E2E round-trip

A backup → wipe → restore round-trip for the Plakar backend, run against a
throwaway Infrahub-shaped Compose stack. This is the harness behind the results
recorded in `specs/006-plakar-encryption/quickstart.md`; before it existed, that
document's test recipe referred to a `--project restoretest` deployment that had
no compose file in the repo, so the recipe was not reproducible.

## Run it

```bash
make build                                    # produces bin/infrahub-backup

# The runner container executes the tool binary, so on macOS/Windows also build
# a Linux one (the script picks it up automatically):
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
  go build -o bin/infrahub-backup-linux ./src/cmd/infrahub-backup

./test/e2e/run-e2e.sh community               # neo4j+offline:// offline dump/load
./test/e2e/run-e2e.sh enterprise              # neo4j:// online backup/restore
ENCRYPT=1 ./test/e2e/run-e2e.sh community     # same, against an encrypted repository
```

Each run creates its own Compose project and kloset repository and tears the
stack down (including volumes) on exit, so runs neither collide with each other
nor with a real local deployment.

| Variable | Purpose |
|---|---|
| `INFRAHUB_BACKUP_BIN` | Tool binary to drive (default `bin/infrahub-backup`) |
| `INFRAHUB_RUNNER_BINARY` | Linux binary mounted into the runner (auto-set to `bin/infrahub-backup-linux` when present) |
| `ENCRYPT=1` | Create an encrypted repository and exercise the passphrase paths |
| `INFRAHUB_BACKUP_PASSPHRASE` | Override the passphrase used when `ENCRYPT=1` |

If `docker compose` cannot reach your daemon — OrbStack, Colima, rootless Docker,
or Docker Desktop without the default socket — export `DOCKER_HOST` first
(`docker context inspect` shows the endpoint).

## What the stack contains

Only the two services the tool actually backs up are real:

| Service | Image | Role |
|---|---|---|
| `database` | `neo4j:2025.10.1-community` / `-enterprise` | backup/restore target |
| `task-manager-db` | `postgres:18-alpine` | task-manager DB target |
| `infrahub-server` | `alpine:3` | placeholder; carries `INFRAHUB_DB_*` for credential discovery |
| `task-manager` | `alpine:3` | placeholder; carries `PREFECT_SERVER_DATABASE_CONNECTION_URL` |
| `task-worker` | `alpine:3` | placeholder; stopped/restarted by the tool |
| `cache` | `alpine:3` | placeholder; transient state wiped and restarted by a restore |
| `message-queue` | `alpine:3` | placeholder; transient state wiped and restarted by a restore |

The placeholders exist because the tool stops and restarts those services and may
`exec env` into them to discover credentials. They are not Infrahub, so the tool
logs `Could not detect Infrahub version: exit status 127` and records the version
as `unknown` — expected, and harmless to the round-trip.

`cache` and `message-queue` are needed rather than optional: a restore wipes their
transient state (which describes the database being replaced) and then restarts
them, and treats a failure to restart them as fatal — as it does on a real
deployment. The wipe itself execs `find` into paths an `alpine:3` image does not
have, which the tool logs and continues past.

The Enterprise compose additionally enables the backup service on port 6362,
which is what the online `neo4j://` path connects to.

## Why the wipe is gated

The script asserts both databases actually read **zero** after the wipe, and
aborts if not. This is not defensive padding — it is the control that makes the
test mean anything.

Neo4j is stopped and restarted around the offline backup and around every
restore. A count taken before Bolt is answering silently returns empty, and the
delete that preceded it may have failed just as silently. Both happened while
this harness was being written: one run reported an empty count after the wipe
and another reported 25, and in both cases the subsequent "restored 25" was
simply data that had never been deleted. The round-trip would have passed with
restore completely broken.

Hence `wait_bolt` before the wipe, and a hard gate on `0`/`0` before restoring.

## On checking for plaintext in an encrypted repository

Don't grep an encrypted repository for a short marker string. A 3-byte sequence
occurs by chance roughly every 16 MB of high-entropy data, so `grep -r 'E2E'`
"finds" a leak in a perfectly good repository — control strings that were never
written match just as often.

The meaningful version of that check already exists as
`TestEncryptedRepoHidesPlaintext` in `src/internal/app/plakar_encryption_test.go`:
a random 1024-byte marker, plus a **control** asserting a plaintext repository
*does* contain it — otherwise the test cannot prove encryption is what hid it.

## Coverage

| Run | Path exercised |
|---|---|
| `community` | Neo4j offline dump/load (`neo4j+offline://`) + Postgres |
| `enterprise` | Neo4j online backup/restore (`neo4j://`) + Postgres |
| `ENCRYPT=1 community` | Encrypted repository end to end, plus the negative case that listing without the passphrase fails |

Not covered: Kubernetes (the K8s runner is still pending), `s3://` repositories
(encryption lives in the repository CONFIG and is storage-agnostic, so `fs://`
covers the encryption path), and restore-to-a-different-Neo4j-version.
