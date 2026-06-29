# Quickstart: Plakar backend on upstream integrations

**Feature**: `003-upstream-plakar-integrations`

Two audiences: an **operator** running backups/restores, and a **contributor** developing the new Neo4j integration.

## Operator — back up & restore (after this feature ships)

Prerequisites: a running Infrahub deployment (Docker Compose or Kubernetes); a backup repository (`fs://` local dir or `s3://`). **No** database client tools required on the host.

```bash
# Full backup (both DBs). Tool detects env + Neo4j edition, drains tasks,
# stops/starts services for Community, runs each integration in a co-located runner.
infrahub-backup create --repo fs:///backups/infrahub        # or s3://bucket/infrahub

# List backup groups (grouped by backup-id; shows complete/incomplete)
infrahub-backup snapshots list --repo fs:///backups/infrahub

# Full-group restore
infrahub-backup restore --repo fs:///backups/infrahub <backup-id>

# Selective per-component restore (FR-024)
infrahub-backup restore --repo fs:///backups/infrahub <backup-id> --component neo4j
infrahub-backup restore --repo fs:///backups/infrahub <backup-id> --component postgres

# Credentials: auto-discovered from the deployment by default; override when needed
infrahub-backup create --repo … --neo4j-password … --pg-url postgres://…
```

Expected: each component snapshot uses the upstream integration layout (postgres globals + per-db dump + manifest; neo4j backup/dump + manifest), tagged into one backup group. Enterprise Neo4j has zero downtime; Community is stopped and restarted.

## Contributor — develop & test `integration-neo4j`

```bash
# Work on a fork of PlakarKorp/integrations, orphan branch per convention
git clone <your-fork-of-PlakarKorp/integrations> && cd integrations
git worktree add --orphan -b integration/neo4j ../neo4j && cd ../neo4j
mkdir neo4j && cd neo4j          # module dir: github.com/PlakarKorp/integration-neo4j

# Build & run the testcontainers suite (Community + Enterprise)
make build
make test                        # spins up neo4j containers, backup+restore round-trips

# Package as a plugin (fallback consumption path)
plakar pkg build neo4j
plakar pkg add ./neo4j_*_$(uname -s)_$(uname -m).ptar
```

Standalone smoke test (no Infrahub):
```bash
plakar at fs:///tmp/repo backup neo4j://neo4j:pass@localhost:6362/neo4j         # enterprise
plakar at fs:///tmp/repo backup "neo4j+offline:///data?database=neo4j"          # community (DB stopped)
```

## Developer — consumption spike (Task 0, gating)

```bash
# Throwaway module: does integration-postgresql build against our kloset?
mkdir /tmp/spike && cd /tmp/spike && go mod init spike
go get github.com/PlakarKorp/integration-postgresql@latest
go get github.com/PlakarKorp/kloset@v1.1.0     # the version this repo targets
# main.go: blank-import importer+exporter, build, assert the "postgres" connector registers
go build ./...
```
Pass → in-process consumption (runner = our binary). Fail → Plakar-CLI/plugin fallback. Record the outcome in `plan.md` before runner work proceeds.

## Validation checklist (maps to Success Criteria)

- [ ] Backup+restore round-trip, **both** Neo4j editions, restored counts match source (SC-001).
- [ ] Succeeds with **no** host DB client tools and **no** port-forward (SC-002).
- [ ] Enterprise = zero downtime; Community stops/restarts to healthy (SC-003).
- [ ] Custom importer + hand-rolled dump/restore removed (SC-004).
- [ ] Standalone neo4j backup/restore works without Infrahub; integration tests pass (SC-005).
- [ ] One-component failure ⇒ group marked incomplete, not restorable (SC-006).
- [ ] Legacy custom-importer snapshot ⇒ clear "unsupported" message (SC-007).
- [ ] All of the above verified in **both** Compose and Kubernetes (SC-008).
