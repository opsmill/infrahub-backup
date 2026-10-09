# Runner image for the co-located Plakar backup/restore (Deliverable B).
#
# Decided by the consumption spike (in-process) + the live E2E (2026-06-30):
# the runner is OUR OWN binary (it embeds kloset + integration-fs/-s3 + the
# postgres/neo4j connectors) plus the DB client tools, run as a one-shot
# container co-located with the target database.
#
# Why a one-shot container (not exec-into-the-existing-DB-container):
#   - it can MOUNT the host fs:// backup dir (-v) so the kloset repo is reachable
#     (a running container cannot gain new mounts) — resolves repo reachability;
#   - it carries neo4j-admin + pg client tools without modifying the DB images.
#
# neo4j-admin is version-sensitive for restore, so the neo4j base MUST match the
# deployment's Neo4j version (parameterised below; the tool detects it and selects
# / builds the matching tag).
ARG NEO4J_VERSION=2025.10.1
FROM neo4j:${NEO4J_VERSION}-enterprise

# PostgreSQL client tools (pg_dump/pg_dumpall/pg_restore/psql) for the postgres
# connector. The neo4j image is Debian-based.
RUN apt-get update \
    && apt-get install -y --no-install-recommends postgresql-client \
    && rm -rf /var/lib/apt/lists/*

# Our binary provides the dependency-light `__run-connector` worker that opens the
# kloset repo and runs ONE connector backup/restore op (no docker/kubectl deps).
# Build with: GOOS=linux GOARCH=<arch> CGO_ENABLED=0 go build -o bin/infrahub-backup-linux ./src/cmd/infrahub-backup
COPY bin/infrahub-backup-linux /usr/local/bin/infrahub-backup

# The container is invoked as:
#   infrahub-backup __run-connector backup  <repo> <source-uri> [opt=val ...] [--tag k=v ...]
#   infrahub-backup __run-connector restore <repo> <dest-uri>   <snapshot> [opt=val ...]
ENTRYPOINT ["/usr/local/bin/infrahub-backup", "__run-connector"]
