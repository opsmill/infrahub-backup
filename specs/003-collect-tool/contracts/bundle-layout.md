# Bundle Layout Contract

**Status**: normative — published in `docs/docs/guides/collect-troubleshooting-bundle.mdx` (Step 4). The layout is identical for Docker Compose and Kubernetes deployments (FR-006, SC-004).

## Archive

- Filename: `support_bundle_<YYYYMMDD_HHMMSS>.tar.gz`
- Compression: gzip; created with the existing `createTarball` helper
- All members live under a single top-level `bundle/` directory

## Directory tree

```text
bundle/
├── bundle_information.json        # Manifest — see manifest.schema.json
├── logs/
│   └── <service>/                 # One directory per Infrahub service present
│       ├── <replica>.log          # One file per replica — Docker: container name;
│       │                          # k8s: <pod>.log, or <pod>_<container>.log for
│       │                          # multi-container pods
│       └── <replica>.previous.log # Kubernetes only, per container with restarts
├── database/                      # Neo4j server logs: neo4j.log, debug.log
│                                  # (full log dir incl. query logs with --include-queries)
├── message-queue/                 # RabbitMQ: queues, exchanges, bindings, connections,
│                                  # channels, status, environment (one file per dump)
├── cache/                         # Redis: info, client list, configuration (masked),
│                                  # slow log, database size
├── task-worker/
│   └── <replica>/                 # One directory per replica: Prefect worker CLI output
├── task-manager/                  # Work pools, work queues, recent flow runs, events,
│                                  # automations
├── server/                        # Infrahub version, installed packages, API info /
│                                  # config / schema, environment variables (masked)
├── metrics/                       # docker stats / kubectl top output
└── benchmark/                     # Present only when --benchmark ran successfully
```

## Rules

- Directories for collectors that failed or were skipped MAY be absent or partially populated; the manifest is the authoritative record of what was attempted (FR-007).
- `logs/<service>/` uses per-replica filenames so multi-replica services never interleave (FR-002).
- Env/config dumps are masked before writing (FR-008); raw service logs are written as-is.
- A backup produced by `--include-backup` is **not** inside the archive; it is a standard backup artifact next to the bundle, referenced in the manifest (research R10).
