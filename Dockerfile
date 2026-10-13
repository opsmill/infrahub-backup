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

# Jars neo4j-admin and cypher-shell never load: the server's browser, the S3, Azure and GCS client SDKs
# (the tool only writes backups to a local directory), Cypher planner and runtimes,
# HTTP server and metrics, Parquet/Arrow import, and native libraries for macOS and Windows. Checked
# by loading classes during a remote backup, inspect, restore, consistency check and cypher-shell
# over plain and verified TLS Bolt.
FROM alpine:3.23 AS neo4j-lib
COPY --from=neo4j-tools /var/lib/neo4j/lib /opt/neo4j/lib
RUN cd /opt/neo4j/lib && rm -f \
      neo4j-browser-*.jar \
      *-osx-*.jar *-windows-*.jar netty-transport-classes-kqueue-*.jar netty-resolver-dns-classes-macos-*.jar \
      neo4j-ast-*.jar neo4j-codegen-*.jar neo4j-cypher-[0-9]*.jar neo4j-cypher-cache-*.jar \
      neo4j-cypher-compiled-expressions-*.jar neo4j-cypher-config-*.jar neo4j-cypher-expression-evaluator-*.jar \
      neo4j-cypher-interpreted-runtime-*.jar neo4j-cypher-ir-*.jar neo4j-cypher-logical-plans-*.jar \
      neo4j-cypher-macros-*.jar neo4j-cypher-physical-planning-*.jar neo4j-cypher-pipelined-runtime-*.jar \
      neo4j-cypher-planner-*.jar neo4j-cypher-rendering-*.jar neo4j-cypher-runtime-util-*.jar \
      neo4j-cypher-slotted-runtime-*.jar neo4j-expressions-*.jar neo4j-front-end-*.jar neo4j-rewriting-*.jar scala-*.jar \
      aircompressor-*.jar arrow-*.jar flatbuffers-java-*.jar flight-core-*.jar parquet-*.jar snappy-java-*.jar \
      hk2-*.jar jakarta.*.jar javax.annotation-api-*.jar jaxb-*.jar jersey-*.jar \
      jetty-alpn-*.jar jetty-ee*.jar jetty-http*.jar jetty-io-*.jar jetty-security-*.jar jetty-server-*.jar \
      jetty-session-*.jar jetty-util-*.jar jetty-xml-*.jar jts-core-*.jar prometheus-metrics-*.jar \
      annotations-2.*.jar apache-client-2.*.jar arns-2.*.jar auth-2.*.jar aws-*.jar checksums-*.jar \
      crt-core-*.jar endpoints-spi-*.jar http-auth-*.jar http-client-spi-*.jar identity-spi-*.jar \
      json-utils-2.*.jar metrics-spi-*.jar netty-nio-client-*.jar profiles-2.*.jar protocol-core-*.jar \
      regions-*.jar retries-*.jar s3-2.*.jar s3-transfer-manager-*.jar sdk-core-*.jar \
      third-party-jackson-core-*.jar utils-2.*.jar \
      azure-core-[0-9]*.jar azure-core-http-netty-*.jar azure-identity-*.jar azure-json-*.jar \
      azure-storage-*.jar azure-xml-*.jar msal4j-*.jar accessors-smart-*.jar content-type-*.jar \
      json-smart-*.jar lang-tag-*.jar nimbus-jose-jwt-*.jar oauth2-oidc-sdk-*.jar jna-platform-*.jar \
      reactor-core-*.jar reactor-netty-*.jar \
      api-common-*.jar gax-*.jar google-api-*.jar google-auth-*.jar google-cloud-core-*.jar \
      google-cloud-monitoring-*.jar google-http-client-*.jar google-oauth-client-*.jar grpc-*.jar \
      proto-google-*.jar protobuf-java-*.jar opencensus-*.jar perfmark-api-*.jar threetenbp-*.jar \
      guava-*.jar failureaccess-*.jar j2objc-annotations-*.jar conscrypt-openjdk-uber-*.jar

# Java runtime stage: a jlink runtime with the modules the Neo4j tool jars reference (jdeps) plus
# modules loaded by reflection or service lookup. binutils provides the objcopy that --strip-debug runs.
# No --compress: the image layer compresses an uncompressed lib/modules better than jlink does.
FROM alpine:3.23 AS java-runtime
ARG OPENJDK_VERSION=21.0.12_p8-r0
RUN apk add --no-cache "openjdk21-jdk=${OPENJDK_VERSION}" "openjdk21-jmods=${OPENJDK_VERSION}" binutils && \
    /usr/lib/jvm/java-21-openjdk/bin/jlink \
      --add-modules java.base,java.compiler,java.datatransfer,java.desktop,java.instrument,java.logging,java.management,java.naming,java.net.http,java.prefs,java.rmi,java.scripting,java.security.jgss,java.security.sasl,java.sql,java.transaction.xa,java.xml,jdk.attach,jdk.charsets,jdk.crypto.cryptoki,jdk.crypto.ec,jdk.httpserver,jdk.incubator.vector,jdk.jdi,jdk.jfr,jdk.management,jdk.management.jfr,jdk.naming.dns,jdk.net,jdk.security.auth,jdk.unsupported,jdk.zipfs \
      --strip-debug --no-man-pages --no-header-files \
      --output /opt/java

# Runtime stage
#
# alpine:3.23 rather than 3.21: Alpine 3.21 and 3.22 do not package the PostgreSQL 18 client tools
# (they stop at postgresql17-client). The Neo4j tools run on Alpine (musl) with Alpine's OpenJDK 21,
# so the base image stays Alpine (checked by scripts/check-image-tools.sh).
FROM alpine:3.23

# Every database tool version is pinned. Alpine removes a package revision from its mirrors when it
# publishes the next one, so a stale pin fails the build loudly instead of drifting silently; bump
# the pin to the version `apk search -x <package>` reports for this Alpine release.
#   - Java 21: the runtime neo4j:2025.10.1-enterprise ships (Temurin 21), here as a jlink runtime built from Alpine's OpenJDK (java-runtime stage)
#   - PostgreSQL 18 client: pg_dump, pg_restore and psql for task-manager-db (postgres 18)
ARG POSTGRESQL_CLIENT_VERSION=18.6-r0

RUN apk add --no-cache ca-certificates curl kubectl \
    "postgresql18-client=${POSTGRESQL_CLIENT_VERSION}"

# The Neo4j tools live under NEO4J_HOME. The launchers find their jars relative to their own path,
# and neo4j-admin reads NEO4J_CONF and writes under NEO4J_LOGS. Both point into a directory the
# non-root user owns, so neo4j-admin never needs to write into a root-owned path.
ENV JAVA_HOME=/opt/java \
    NEO4J_HOME=/opt/neo4j \
    NEO4J_CONF=/opt/neo4j/conf \
    NEO4J_LOGS=/opt/neo4j/logs
ENV PATH="${NEO4J_HOME}/bin:${JAVA_HOME}/bin:${PATH}"

COPY --from=neo4j-tools /var/lib/neo4j/bin/cypher-shell /var/lib/neo4j/bin/neo4j-admin /opt/neo4j/bin/
COPY --from=java-runtime /opt/java /opt/java
COPY --from=neo4j-lib /opt/neo4j/lib /opt/neo4j/lib
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
