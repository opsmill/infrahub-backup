---
branch: 001-k8s-helm-backup
date: 2026-01-26
spec: spec.md
---

# Implementation Plan: Kubernetes Helm Chart Support for Infrahub Backup

## Summary

Add Kubernetes Helm chart support for infrahub-backup to enable users without direct kubectl access (using ArgoCD/Flux) to perform backup/restore operations.

**Key Finding**: S3 storage integration is already implemented in PR #56 (https://github.com/opsmill/infrahub-backup/pull/56). This plan focuses on:
1. Dockerfile for multi-arch container image (amd64 + arm64) - in this repo
2. Helm subchart in the upstream infrahub-helm repository

## Repository Structure

This implementation spans **two repositories**:

| Repository | Changes |
|------------|---------|
| `opsmill/infrahub-ops-cli` (this repo) | Dockerfile, Makefile updates, GitHub Actions for Docker image |
| `opsmill/infrahub-helm` | New `infrahub-backup` subchart + integration into main `infrahub` chart |

## Technical Context

- **Language/Version**: Go 1.25.0
- **Primary Dependencies**: cobra, viper, pgx/v5, logrus, aws-sdk-go-v2 (from PR #56)
- **Storage**: S3-compatible (AWS S3, MinIO), local filesystem
- **Testing**: go test, helm lint
- **Target Platform**: Kubernetes (Linux containers - amd64 and arm64)
- **Helm Chart Location**: `opsmill/infrahub-helm` repository

## S3 Implementation Status (PR #56)

The S3 support is already implemented and includes:
- `src/internal/app/s3.go` - S3 client with upload/download using AWS SDK v2
- CLI flags: `--s3-bucket`, `--s3-prefix`, `--s3-endpoint`, `--s3-region`
- Create command flags: `--s3-upload`, `--s3-keep-local`
- Restore supports `s3://bucket/key` URI format
- AWS SDK dependencies in go.mod

**This PR should be merged before proceeding with Helm chart implementation.**

## Project Structure

### This Repository (infrahub-ops-cli)

```text
Dockerfile                  # NEW: Multi-arch container build
Makefile                    # MODIFY: Add docker build targets
.github/workflows/
└── release.yml             # MODIFY: Add Docker image build & push
```

### infrahub-helm Repository

```text
charts/
├── infrahub/
│   ├── Chart.yaml          # MODIFY: Add infrahub-backup as dependency
│   ├── values.yaml         # MODIFY: Add infrahub-backup section
│   └── templates/          # No changes needed - subchart handles templates
└── infrahub-backup/        # NEW: Standalone subchart
    ├── Chart.yaml
    ├── values.yaml
    └── templates/
        ├── _helpers.tpl
        ├── serviceaccount.yaml
        ├── role.yaml
        ├── rolebinding.yaml
        ├── job-backup.yaml
        ├── cronjob-backup.yaml
        └── job-restore.yaml
```

## Implementation Phases

### Phase 1: Multi-Arch Dockerfile (this repo: infrahub-ops-cli)

**1.1 Create Dockerfile** (supports both amd64 and arm64)

```dockerfile
# Build stage
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder

ARG TARGETARCH
ARG TARGETOS

RUN apk add --no-cache git make

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build neo4j watchdog binaries for target architecture
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build -ldflags "-s -w" \
    -o /neo4j_watchdog ./tools/neo4jwatchdog

# Build main binary for target architecture
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags "-s -w" \
    -o /infrahub-backup ./src/cmd/infrahub-backup

# Runtime stage
FROM alpine:3.19

ARG TARGETARCH

# Install kubectl for target architecture
RUN apk add --no-cache ca-certificates curl kubectl

COPY --from=builder /infrahub-backup /usr/local/bin/
COPY --from=builder /neo4j_watchdog /usr/local/bin/

ENTRYPOINT ["/usr/local/bin/infrahub-backup"]
```

**1.2 Update Makefile** - Add multi-arch build targets:

```makefile
docker-build: ## Build Docker image for current platform
	docker build -t opsmill/infrahub-backup:$(VERSION) -t opsmill/infrahub-backup:latest .

docker-build-multi: ## Build multi-arch Docker images (amd64 + arm64)
	docker buildx build --platform linux/amd64,linux/arm64 \
		-t opsmill/infrahub-backup:$(VERSION) \
		-t opsmill/infrahub-backup:latest .

docker-push: docker-build-multi ## Build and push multi-arch images
	docker buildx build --platform linux/amd64,linux/arm64 \
		-t opsmill/infrahub-backup:$(VERSION) \
		-t opsmill/infrahub-backup:latest --push .
```

**1.3 Update release workflow** (`.github/workflows/release.yml`)

Add Docker build and push step:

```yaml
docker:
  needs: publish
  runs-on: ubuntu-24.04
  steps:
    - uses: actions/checkout@v4
    - uses: docker/setup-qemu-action@v3
    - uses: docker/setup-buildx-action@v3
    - uses: docker/login-action@v3
      with:
        username: ${{ secrets.DOCKERHUB_USERNAME }}
        password: ${{ secrets.DOCKERHUB_TOKEN }}
    - uses: docker/build-push-action@v6
      with:
        push: true
        platforms: linux/amd64,linux/arm64
        tags: |
          opsmill/infrahub-backup:${{ github.event.release.tag_name }}
          opsmill/infrahub-backup:latest
```

### Phase 2: Helm Subchart (infrahub-helm repo)

The chart will be created in `opsmill/infrahub-helm` as a subchart that integrates with the main `infrahub` chart.

**2.1 Create infrahub-backup subchart** (`charts/infrahub-backup/`)

**Chart.yaml**:

```yaml
apiVersion: v2
name: infrahub-backup
description: Backup and restore Helm chart for Infrahub on Kubernetes
type: application
version: 0.1.0
appVersion: "1.0.0"
keywords:
  - infrahub
  - backup
  - restore
  - neo4j
  - postgresql
maintainers:
  - name: OpsMill
    url: https://github.com/opsmill
```

**values.yaml** (matching existing documentation):

```yaml
serviceAccount:
  create: true
  name: ""
  annotations: {}

rbac:
  create: true

backup:
  enabled: false
  mode: "job"  # "job" or "cronjob"
  schedule: "0 2 * * *"
  storage:
    type: "local"  # "s3" or "local"
    s3:
      bucket: ""
      prefix: ""
      endpoint: ""
      region: "us-east-1"
      secretName: ""  # Secret with AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY
    local:
      path: "/backups"
  options:
    force: false
    excludeTaskmanager: false
    neo4jMetadata: "all"
    keepLocal: false  # Keep local file after S3 upload

restore:
  enabled: false
  s3:
    bucket: ""
    key: ""  # Backup filename in bucket
    endpoint: ""
    region: "us-east-1"
    secretName: ""
  options:
    excludeTaskmanager: false
    migrateFormat: false

image:
  repository: opsmill/infrahub-backup
  tag: ""  # Defaults to chart appVersion
  pullPolicy: IfNotPresent

resources:
  requests:
    cpu: "100m"
    memory: "256Mi"
  limits:
    cpu: "500m"
    memory: "512Mi"

nodeSelector: {}
tolerations: []
affinity: {}
```

**2.2 RBAC templates** (from documentation):

```yaml
# templates/role.yaml
{{- if .Values.rbac.create }}
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: {{ include "infrahub-backup.fullname" . }}
  labels:
    {{- include "infrahub-backup.labels" . | nindent 4 }}
rules:
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["list", "get"]
  - apiGroups: [""]
    resources: ["pods/exec"]
    verbs: ["create"]
  - apiGroups: [""]
    resources: ["pods/log"]
    verbs: ["get"]
  - apiGroups: ["apps"]
    resources: ["deployments", "statefulsets"]
    verbs: ["get", "patch"]
{{- end }}
```

**2.3 CronJob template** (`templates/cronjob-backup.yaml`):

```yaml
{{- if and .Values.backup.enabled (eq .Values.backup.mode "cronjob") }}
apiVersion: batch/v1
kind: CronJob
metadata:
  name: {{ include "infrahub-backup.fullname" . }}
  labels:
    {{- include "infrahub-backup.labels" . | nindent 4 }}
spec:
  schedule: {{ .Values.backup.schedule | quote }}
  concurrencyPolicy: Forbid
  successfulJobsHistoryLimit: 3
  failedJobsHistoryLimit: 3
  jobTemplate:
    spec:
      template:
        metadata:
          labels:
            {{- include "infrahub-backup.selectorLabels" . | nindent 12 }}
        spec:
          serviceAccountName: {{ include "infrahub-backup.serviceAccountName" . }}
          restartPolicy: Never
          containers:
            - name: backup
              image: "{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}"
              imagePullPolicy: {{ .Values.image.pullPolicy }}
              args:
                - create
                {{- if .Values.backup.options.force }}
                - --force
                {{- end }}
                {{- if .Values.backup.options.excludeTaskmanager }}
                - --exclude-taskmanager
                {{- end }}
                - --neo4jmetadata={{ .Values.backup.options.neo4jMetadata }}
                {{- if eq .Values.backup.storage.type "s3" }}
                - --s3-upload
                {{- if .Values.backup.options.keepLocal }}
                - --s3-keep-local
                {{- end }}
                {{- end }}
              env:
                - name: INFRAHUB_K8S_NAMESPACE
                  valueFrom:
                    fieldRef:
                      fieldPath: metadata.namespace
                {{- if eq .Values.backup.storage.type "s3" }}
                - name: INFRAHUB_S3_BUCKET
                  value: {{ .Values.backup.storage.s3.bucket | quote }}
                {{- if .Values.backup.storage.s3.prefix }}
                - name: INFRAHUB_S3_PREFIX
                  value: {{ .Values.backup.storage.s3.prefix | quote }}
                {{- end }}
                {{- if .Values.backup.storage.s3.endpoint }}
                - name: INFRAHUB_S3_ENDPOINT
                  value: {{ .Values.backup.storage.s3.endpoint | quote }}
                {{- end }}
                - name: INFRAHUB_S3_REGION
                  value: {{ .Values.backup.storage.s3.region | quote }}
                {{- if .Values.backup.storage.s3.secretName }}
                - name: AWS_ACCESS_KEY_ID
                  valueFrom:
                    secretKeyRef:
                      name: {{ .Values.backup.storage.s3.secretName }}
                      key: AWS_ACCESS_KEY_ID
                - name: AWS_SECRET_ACCESS_KEY
                  valueFrom:
                    secretKeyRef:
                      name: {{ .Values.backup.storage.s3.secretName }}
                      key: AWS_SECRET_ACCESS_KEY
                {{- end }}
                {{- end }}
              resources:
                {{- toYaml .Values.resources | nindent 16 }}
          {{- with .Values.nodeSelector }}
          nodeSelector:
            {{- toYaml . | nindent 12 }}
          {{- end }}
          {{- with .Values.tolerations }}
          tolerations:
            {{- toYaml . | nindent 12 }}
          {{- end }}
          {{- with .Values.affinity }}
          affinity:
            {{- toYaml . | nindent 12 }}
          {{- end }}
{{- end }}
```

**2.4 Restore Job template** (`templates/job-restore.yaml`):

```yaml
{{- if .Values.restore.enabled }}
apiVersion: batch/v1
kind: Job
metadata:
  name: {{ include "infrahub-backup.fullname" . }}-restore
  labels:
    {{- include "infrahub-backup.labels" . | nindent 4 }}
spec:
  template:
    metadata:
      labels:
        {{- include "infrahub-backup.selectorLabels" . | nindent 8 }}
    spec:
      serviceAccountName: {{ include "infrahub-backup.serviceAccountName" . }}
      restartPolicy: Never
      containers:
        - name: restore
          image: "{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}"
          imagePullPolicy: {{ .Values.image.pullPolicy }}
          args:
            - restore
            {{- if .Values.restore.options.excludeTaskmanager }}
            - --exclude-taskmanager
            {{- end }}
            {{- if .Values.restore.options.migrateFormat }}
            - --migrate-format
            {{- end }}
            {{- if .Values.restore.s3.endpoint }}
            - --s3-endpoint={{ .Values.restore.s3.endpoint }}
            {{- end }}
            - "s3://{{ .Values.restore.s3.bucket }}/{{ .Values.restore.s3.key }}"
          env:
            - name: INFRAHUB_K8S_NAMESPACE
              valueFrom:
                fieldRef:
                  fieldPath: metadata.namespace
            - name: INFRAHUB_S3_REGION
              value: {{ .Values.restore.s3.region | quote }}
            {{- if .Values.restore.s3.secretName }}
            - name: AWS_ACCESS_KEY_ID
              valueFrom:
                secretKeyRef:
                  name: {{ .Values.restore.s3.secretName }}
                  key: AWS_ACCESS_KEY_ID
            - name: AWS_SECRET_ACCESS_KEY
              valueFrom:
                secretKeyRef:
                  name: {{ .Values.restore.s3.secretName }}
                  key: AWS_SECRET_ACCESS_KEY
            {{- end }}
          resources:
            {{- toYaml .Values.resources | nindent 12 }}
      {{- with .Values.nodeSelector }}
      nodeSelector:
        {{- toYaml . | nindent 8 }}
      {{- end }}
{{- end }}
```

### Phase 3: Integrate Subchart into Main Infrahub Chart

**3.1 Update infrahub Chart.yaml** - Add infrahub-backup as optional dependency:

```yaml
# Add to dependencies section
dependencies:
  # ... existing dependencies ...
  - name: infrahub-backup
    version: "0.1.0"
    repository: "file://../infrahub-backup"  # Local reference for development
    condition: infrahub-backup.enabled
```

**3.2 Update infrahub values.yaml** - Add backup section:

```yaml
# Add to values.yaml
infrahub-backup:
  enabled: false  # Disabled by default

  backup:
    enabled: false
    mode: "cronjob"
    schedule: "0 2 * * *"
    storage:
      type: "s3"
      s3:
        bucket: ""
        prefix: ""
        endpoint: ""
        region: "us-east-1"
        secretName: ""

  restore:
    enabled: false
```

This allows users to enable backup with a simple values override:

```yaml
# User's values.yaml
infrahub-backup:
  enabled: true
  backup:
    enabled: true
    mode: "cronjob"
    schedule: "0 2 * * *"
    storage:
      type: "s3"
      s3:
        bucket: "my-infrahub-backups"
        secretName: "backup-s3-credentials"
```

## Critical Files to Create/Modify

### This Repository (infrahub-ops-cli)

| File | Action | Description |
|------|--------|-------------|
| `Dockerfile` | CREATE | Multi-arch container build (amd64 + arm64) |
| `Makefile` | MODIFY | Add docker-build-multi target |
| `.github/workflows/release.yml` | MODIFY | Add Docker build & push step |

### infrahub-helm Repository

| File | Action | Description |
|------|--------|-------------|
| `charts/infrahub-backup/Chart.yaml` | CREATE | Subchart metadata |
| `charts/infrahub-backup/values.yaml` | CREATE | Default values |
| `charts/infrahub-backup/templates/_helpers.tpl` | CREATE | Template helpers |
| `charts/infrahub-backup/templates/serviceaccount.yaml` | CREATE | ServiceAccount |
| `charts/infrahub-backup/templates/role.yaml` | CREATE | RBAC Role |
| `charts/infrahub-backup/templates/rolebinding.yaml` | CREATE | RoleBinding |
| `charts/infrahub-backup/templates/job-backup.yaml` | CREATE | One-shot backup Job |
| `charts/infrahub-backup/templates/cronjob-backup.yaml` | CREATE | Scheduled backup CronJob |
| `charts/infrahub-backup/templates/job-restore.yaml` | CREATE | Restore Job |
| `charts/infrahub/Chart.yaml` | MODIFY | Add infrahub-backup dependency |
| `charts/infrahub/values.yaml` | MODIFY | Add infrahub-backup default values |

## Prerequisites

1. **Merge PR #56** - S3 support must be merged first (in infrahub-ops-cli)
2. **Docker Hub credentials** - Need DOCKERHUB_USERNAME and DOCKERHUB_TOKEN secrets in GitHub
3. **Access to infrahub-helm repo** - Need permissions to create PR in opsmill/infrahub-helm

## Deployment Modes

Users can deploy the backup chart in two ways:

### 1. As part of Infrahub deployment (Recommended)

```bash
# Enable backup subchart when deploying Infrahub
helm install infrahub opsmill/infrahub \
  --set infrahub-backup.enabled=true \
  --set infrahub-backup.backup.enabled=true \
  --set infrahub-backup.backup.mode=cronjob \
  --set infrahub-backup.backup.storage.s3.bucket=my-backups \
  --set infrahub-backup.backup.storage.s3.secretName=s3-creds
```

### 2. Standalone installation

```bash
# Install backup chart separately (same namespace as Infrahub)
helm install infrahub-backup opsmill/infrahub-backup \
  --namespace infrahub \
  --set backup.enabled=true \
  --set backup.storage.s3.bucket=my-backups
```

## Verification

### Docker Image

```bash
# Build locally for current platform
make docker-build

# Test multi-arch build (requires buildx)
docker buildx build --platform linux/amd64,linux/arm64 -t test:latest .

# Verify image runs
docker run --rm opsmill/infrahub-backup:latest version
```

### Helm Validation

```bash
helm lint charts/infrahub-backup
helm template test charts/infrahub-backup --set backup.enabled=true
helm template test charts/infrahub-backup --set backup.enabled=true,backup.mode=cronjob
helm template test charts/infrahub-backup --set restore.enabled=true,restore.s3.bucket=test,restore.s3.key=backup.tar.gz
```

### E2E Test (Kubernetes with MinIO)

```bash
# Create Kind cluster
kind create cluster

# Deploy MinIO for testing
helm repo add minio https://charts.min.io/
helm install minio minio/minio --set rootUser=admin,rootPassword=password

# Create S3 credentials secret
kubectl create secret generic backup-s3-credentials \
  --from-literal=AWS_ACCESS_KEY_ID=admin \
  --from-literal=AWS_SECRET_ACCESS_KEY=password

# Deploy backup chart (one-shot to S3)
helm install test charts/infrahub-backup \
  --set backup.enabled=true \
  --set backup.mode=job \
  --set backup.storage.type=s3 \
  --set backup.storage.s3.bucket=backups \
  --set backup.storage.s3.endpoint=http://minio:9000 \
  --set backup.storage.s3.secretName=backup-s3-credentials

# Verify job completes
kubectl get jobs -w
kubectl logs -l app.kubernetes.io/name=infrahub-backup
```

## Implementation Order

1. **This repo (infrahub-ops-cli)**:
   - Create Dockerfile (multi-arch)
   - Update Makefile with docker targets
   - Update release workflow for Docker image publishing
   - Test Docker image locally

2. **infrahub-helm repo**:
   - Create `charts/infrahub-backup/` subchart
   - Update `charts/infrahub/Chart.yaml` to add dependency
   - Update `charts/infrahub/values.yaml` with defaults
   - Run `helm dependency update charts/infrahub`
   - Test with helm template and local deployment

## Notes

- Documentation already exists and is comprehensive (kubernetes-backup.mdx, helm-values.mdx)
- Existing KubernetesBackend in environment.go handles pod discovery and kubectl operations
- Neo4j watchdog binary must be embedded in container for Community Edition backup
- S3 credentials should come from Kubernetes Secrets via secretName reference
- Multi-arch support (amd64 + arm64) required per user feedback
- Subchart approach allows single `helm install infrahub` to deploy everything
- Users can still install backup chart standalone if needed
