# Build stage
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder

ARG TARGETARCH
ARG TARGETOS=linux
ARG VERSION

RUN apk add --no-cache git make

WORKDIR /app

# Copy go mod files first for better caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build neo4j watchdog binaries for both architectures (they get embedded)
RUN CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "-s -w" \
    -o ./src/internal/app/embedded/neo4jwatchdog/neo4j_watchdog_linux_arm64 ./tools/neo4jwatchdog && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "-s -w" \
    -o ./src/internal/app/embedded/neo4jwatchdog/neo4j_watchdog_linux_amd64 ./tools/neo4jwatchdog

# Build main binary for target architecture
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -ldflags "-X main.version=${VERSION} -s -w" \
    -o /infrahub-backup ./src/cmd/infrahub-backup

# Neo4j tools stage (FR-030, ADR 0017).
# neo4j-admin and cypher-shell are POSIX sh launchers around Java jars, so only the launchers, the
# jar directory and the default configuration are taken. The Java runtime comes from Alpine in the
# runtime stage. The `products` directory (server plugins) and `data`, `logs`, `run` are not needed
# to run the tools and are left out.
FROM neo4j:2025.10.1-enterprise AS neo4j-tools

# Runtime stage
#
# alpine:3.23 rather than 3.21: Alpine 3.21 and 3.22 do not package the PostgreSQL 18 client tools
# (they stop at postgresql17-client). The Neo4j tools run on Alpine (musl) with Alpine's OpenJDK 21,
# so the base image stays Alpine (checked by scripts/check-image-tools.sh).
FROM alpine:3.23

# Every database tool version is pinned. Alpine removes a package revision from its mirrors when it
# publishes the next one, so a stale pin fails the build loudly instead of drifting silently; bump
# the pin to the version `apk search -x <package>` reports for this Alpine release.
#   - Java 21: the runtime neo4j:2025.10.1-enterprise ships (Temurin 21), here as Alpine's OpenJDK
#   - PostgreSQL 18 client: pg_dump, pg_restore and psql for task-manager-db (postgres 18)
ARG OPENJDK_VERSION=21.0.12_p8-r0
ARG POSTGRESQL_CLIENT_VERSION=18.6-r0

RUN apk add --no-cache ca-certificates curl kubectl \
    "openjdk21-jre-headless=${OPENJDK_VERSION}" \
    "postgresql18-client=${POSTGRESQL_CLIENT_VERSION}"

# The Neo4j tools live under NEO4J_HOME. The launchers find their jars relative to their own path,
# and neo4j-admin reads NEO4J_CONF and writes under NEO4J_LOGS. Both point into a directory the
# non-root user owns, so neo4j-admin never needs to write into a root-owned path.
ENV JAVA_HOME=/usr/lib/jvm/java-21-openjdk \
    NEO4J_HOME=/opt/neo4j \
    NEO4J_CONF=/opt/neo4j/conf \
    NEO4J_LOGS=/opt/neo4j/logs
ENV PATH="${NEO4J_HOME}/bin:${JAVA_HOME}/bin:${PATH}"

COPY --from=neo4j-tools /var/lib/neo4j/bin/cypher-shell /var/lib/neo4j/bin/neo4j-admin /opt/neo4j/bin/
COPY --from=neo4j-tools /var/lib/neo4j/lib /opt/neo4j/lib
COPY --from=neo4j-tools /var/lib/neo4j/conf /opt/neo4j/conf
COPY --from=neo4j-tools /var/lib/neo4j/LICENSE.txt /var/lib/neo4j/LICENSES.txt /var/lib/neo4j/NOTICE.txt /opt/neo4j/

COPY --from=builder /infrahub-backup /usr/local/bin/

# Run as non-root user. The user owns NEO4J_HOME itself (not its bin and lib) and the directories
# neo4j-admin writes to; otherwise neo4j-admin warns on every run that the owner of NEO4J_HOME
# differs from the user running it.
RUN adduser -D -u 1000 infrahub && \
    mkdir -p /opt/neo4j/logs /opt/neo4j/data /opt/neo4j/run && \
    chown infrahub:infrahub /opt/neo4j && \
    chown -R infrahub:infrahub /opt/neo4j/logs /opt/neo4j/data /opt/neo4j/run /opt/neo4j/conf
USER infrahub

ENTRYPOINT ["/usr/local/bin/infrahub-backup"]
