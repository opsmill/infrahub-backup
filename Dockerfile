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

# Runtime stage
FROM alpine:3.21

RUN apk add --no-cache ca-certificates curl kubectl

COPY --from=builder /infrahub-backup /usr/local/bin/

# Run as non-root user
RUN adduser -D -u 1000 infrahub
USER infrahub

ENTRYPOINT ["/usr/local/bin/infrahub-backup"]
