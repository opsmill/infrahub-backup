# Quickstart Validation: Infrahub Collect

**Feature**: 003-collect-tool | Validation scenarios proving the feature end-to-end. Contracts: [cli.md](./contracts/cli.md), [bundle-layout.md](./contracts/bundle-layout.md), [manifest.schema.json](./contracts/manifest.schema.json).

## Prerequisites

- Go 1.25 toolchain (`make dev-setup` for lint tooling)
- Docker with Docker Compose (Docker scenarios)
- kind + kubectl + helm (Kubernetes scenarios; e2e fixtures under `tests/e2e/fixtures/helm/`)
- Python + pytest for e2e (`tests/e2e/`)

## 1. Build and static gates

```bash
make build          # bin/infrahub-backup, bin/infrahub-taskmanager, bin/infrahub-collect
make test           # unit tests incl. masking / manifest / collector tables
make vet && make lint && make fmt
```

**Expected**: all three binaries build; all gates pass.

## 2. CLI surface smoke test

```bash
bin/infrahub-collect version               # -> "Version: <rev>"
bin/infrahub-collect --help                # shows create + environment + version
bin/infrahub-collect create --help         # shows --log-lines, --include-backup,
                                           #       --include-queries, --benchmark
bin/infrahub-collect environment detect
bin/infrahub-collect environment list
```

**Expected**: flags/commands match [contracts/cli.md](./contracts/cli.md); environment commands behave identically to `infrahub-backup`.

## 3. Docker Compose collection

With an Infrahub compose project running:

```bash
bin/infrahub-collect create --output-dir=/tmp/bundles
BUNDLE=$(ls -t /tmp/bundles/support_bundle_*.tar.gz | head -1)

tar -tzf "$BUNDLE" > /dev/null && echo "archive OK"
tar -xzOf "$BUNDLE" bundle/bundle_information.json | jq '.environment, .collectors'
```

**Expected**: exit 0; archive valid; manifest reports `"docker"`, one entry per collector, all `success` on a healthy stack; `bundle/logs/<service>/` populated for every running service; env dumps in `bundle/server/` contain `********` for keys matching `password|secret|token|key` and no plaintext secrets.

## 4. Degraded-instance collection (FR-009 / SC-005)

```bash
docker compose -p <project> stop cache
bin/infrahub-collect create --output-dir=/tmp/bundles; echo "exit=$?"
```

**Expected**: `exit=0`; a `WARN` line for the cache collector; manifest entry `{"name": "cache-status", "status": "failed", "reason": ...}`; every other collector still reported. Restart cache afterwards.

## 5. Kubernetes collection (multi-replica + previous logs)

Against a kind cluster with the Helm chart installed (task-worker scaled ≥ 2, one pod restarted at least once):

```bash
bin/infrahub-collect create --k8s-namespace <ns> --output-dir=/tmp/bundles
```

**Expected**: manifest `environment: "kubernetes"`; `bundle/logs/task-worker/` contains one `.log` per pod and a `.previous.log` for the restarted pod; layout byte-identical in structure to the Docker bundle; **zero** pod restarts/scale events attributable to the run (`kubectl get events`) — FR-010/SC-003.

## 6. Flag and env-var behavior

```bash
bin/infrahub-collect create --log-lines=500 --output-dir=/tmp/bundles
INFRAHUB_LOG_LINES=250 bin/infrahub-collect create --output-dir=/tmp/bundles
```

**Expected**: manifests record `"log_lines": 500` and `250` respectively; flag wins over env when both are set.

## 7. Opt-in collectors

```bash
bin/infrahub-collect create --include-backup   # backup artifact created next to bundle,
                                               # referenced in manifest; bundle still
                                               # produced if backup fails
bin/infrahub-collect create --include-queries  # bundle/database/ includes query logs
bin/infrahub-collect create --benchmark        # benchmark results in bundle/benchmark/;
                                               # air-gapped -> status "skipped" + warning
```

## 8. End-to-end suites

```bash
pytest tests/e2e -m docker -k collect
pytest tests/e2e -m k8s -k collect
```

**Expected**: new `test_docker_collect.py` / `test_k8s_collect.py` pass, covering scenarios 3–6 automatically.
