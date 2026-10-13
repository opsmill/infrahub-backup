# Quickstart: validating external database backup and restore

**Feature**: 007-external-database-backup | **Date**: 2026-09-02

Runnable scenarios that prove the feature works. Each maps to acceptance scenarios in
[spec.md](./spec.md) and to success criteria. Details of flags and metadata live in
[contracts/](./contracts/) and [data-model.md](./data-model.md) rather than being repeated here.

---

## Prerequisites

- A Kubernetes context with an Infrahub deployment, as used by the existing
  `tests/e2e/test_k8s_*.py` suite.
- Neo4j **Enterprise** running **outside** the Infrahub namespace, with:
  - the backup service bound to a non-loopback address (it defaults to localhost, so this is a
    server configuration change — see spec Assumptions, prerequisite 1);
  - the Infrahub components pointed at it via `INFRAHUB_DB_ADDRESS`.
- PostgreSQL outside the namespace for the both-external scenario.
- An object store reachable **from the database server**, with the server-side prerequisites for
  seeding in place (prerequisite 2).
- Permission in the namespace to create, get and delete Jobs; get, list, watch and delete pods;
  exec into pods; and create, list and delete secrets.

The standard gates apply throughout: `make test`, `make vet`, `make lint`.

---

## Scenario 1 — Mixed topology, external Neo4j (US1 scenario 1)

```bash
bin/infrahub-backup create --k8s-namespace <ns>
```

**Expect**: the resolved Neo4j endpoint is logged *before* any connection is opened; one artifact
covering both databases; metadata records the Neo4j `source_endpoints`, the roles observed, and `external_location: true`
for Neo4j only.

**Proves**: FR-001 (per-service resolution), FR-003 (discovery and reporting), SC-004.

## Scenario 2 — Both databases external (US1 scenario 2)

Same command against a deployment with both databases outside the namespace.

**Expect**: both captured; per-database `source_endpoints` and `source_roles`; no transient pod or secret remaining
afterwards.

**Proves**: FR-001, FR-011, SC-006.

## Scenario 3 — Full round trip (US1 scenario 3)

```bash
bin/infrahub-backup create --k8s-namespace <ns>
# destroy the data
bin/infrahub-backup restore --k8s-namespace <ns> \
  --allow-external-restore <artifact>
```

**Expect**: workloads quiesced, both databases restored, workloads returned to their prior scale,
Infrahub serving pre-backup data. The run must not report success until the restored database
reports a usable state.

**Proves**: FR-013, FR-021, SC-001 — the criterion the whole feature exists to satisfy.

## Scenario 4 — Version incompatibility aborts early (US1 scenario 4)

Declare a client image older than the server.

**Expect**: non-zero exit, both versions named, **zero bytes written**. Then declare a *newer*
client and expect a warning rather than an abort — the asymmetry established in research R3.

**Proves**: FR-006, SC-003.

## Scenario 5 — Restore refused without authorisation (US1 scenario 5)

Run scenario 3's restore without `--allow-external-restore`.

**Expect**: refusal naming the authorisation, its flag and its configuration key; databases
untouched.

Then enable scheduled restore **only**, and confirm it does not confer the capability.

**Proves**: FR-009, FR-010, SC-005.

## Scenario 6 — Internal deployments unchanged (US1 scenario 6)

Run the existing `tests/e2e/test_k8s_tarball.py`, `test_k8s_s3.py`, `test_k8s_plakar.py` and
`test_k8s_plakar_s3.py` unmodified, plus the Docker equivalents.

**Expect**: all pass with no change; artifact layout identical.

Additionally: restore an artifact produced by the **previous released version**, and restore an
artifact taken from an external database into an **internal** deployment.

**Proves**: FR-015, FR-016, SC-007, and the cross-path interchangeability the artifact contract
requires.

---

## Diagnostic scenarios (US2)

| Provoke | Expect | Proves |
|---|---|---|
| External Neo4j **Community** | Refusal naming the storage-access limitation of the Community mechanism — not "service not found" | FR-008 |
| No container, no discoverable endpoint | Report that discovery was attempted, where it looked, and the override flag | FR-003, FR-004 |
| Pod-list permission revoked | **Failure**, not a switch to external mode | FR-002 |
| Pod-create permission absent | Failure naming the missing verbs and resource | FR-019 |
| Unreadable artifact URI | Refusal before any destructive step; database untouched | FR-007 |
| Corrupt artifact that is readable | Restore fails; success is never reported | FR-021 |

Each message is checked against the three required elements in the CLI contract: condition,
resource, operator action (SC-008).

---

## Robustness scenarios

| Provoke | Expect | Proves |
|---|---|---|
| Kill the tool mid-capture | No surviving pod or secret; deployment not left quiesced | FR-011, FR-013, SC-006 |
| Kill the tool between credential creation and workload deletion | Credentials gone once the workload is reclaimed, with no tool involvement | FR-023 |
| Kill the tool, then start a new run | The new run reaps the stray and does not adopt it | FR-011 |
| Two concurrent runs | Neither adopts the other's transient workload | Data-model invariant |
| One endpoint of several unreachable | Artifact marked incomplete; retention and latest-backup selection both skip it | FR-012 |
| Database larger than default scratch | Succeeds, or fails cleanly without exhausting shared host storage | FR-022 |

The unreachable-endpoint case is the one to write first: it exercises the finding that a partially
successful backup and a failed backup are indistinguishable by exit status, which is the most
likely source of a silently unusable artifact.

---

## Compatibility envelope (deliberately measured, not assumed)

Research R3 left the exact client/server version envelope for the backup command open, because the
vendor documentation does not state one. Establish it here as a matrix across the supported server
versions, covering both the `5.x` and calendar (`YYYY.MM`) numbering schemes, and record the result
in the spec's assumptions. Encoding a guessed rule in an abort condition would make the tool refuse
work it could have done.
