# Implementation Report: External Database Backup and Restore

**Status**: **INCOMPLETE** — 142 of 161 tasks done, 1 descoped; 18 open. **The Kubernetes path has now executed end to end on a real cluster and the round trip passed (§12)**; what is open is 8 e2e/`[INFRA]` tasks, 4 residual ledgers (T152–T155) and 6 findings from that run (T156–T161), three of them HIGH
work gated on CI topology, each with a named reason and owner

**Feature**: 007-external-database-backup
**Spec dir**: `specs/007-external-database-backup`
**Branch**: `007-external-database-backup` — **not pushed, no PR**
**Base commit (run 3)**: `080917d`
**Head commit**: `5c0a3bf` (last code commit; this report's own commit follows it)
**Commits this run**: 29
**Wall clock**: ~08:45Z → ~16:40Z, 2026-09-03 (~8h)

| | Run 1 | Run 2 | Run 3 (this run) |
|---|---|---|---|
| Base → head | `e9008b4` → `c9da687` | `546ccd7` → `b470c67` | `080917d` → `a34c5ad` |
| Commits | 9 | 30 | 29 |
| Tasks closed | 41 (8 later reverted) | 34 | 61 |
| Go tests | 862 → 888 | 888 → 1197 | 1197 → **1450** |
| Review findings | ~35 | ~24 | **21** |
| Outcome | **reset** — Phase 3 reverted | remediation + rebuild | Phase 2T closed, Phase 3–5 landed, third review fixed |

---

## 0. What changed after the first report was written

The run continued past the report below, and three things in it are now out of date:

1. **T121 was taken out of scope, and T107 closed** — on FR-014's own wording, which is scoped to an
   *externally-managed* database and already satisfied. The internal argv exposure is real but
   pre-dates the branch (all 10 sites present unchanged at base `8186fcf`). See §8.
2. **T122, T119 and T141 landed**, closing every task that was actionable without CI topology.
3. **The feature's mechanism was executed against real Neo4j Enterprise servers for the first time**
   (Phase 2W, §9). That retired most of T073's risk, found one critical defect, corrected two
   documentation claims, and proved T072's topology can be built on a laptop.

Open tasks are now **8**, all of them e2e or `[INFRA]`.

---

## 1. Why this is still INCOMPLETE

**8 tasks are open.** None is unexplained, and none is silent. They fall into two groups:

- **6 e2e tasks blocked on CI topology that does not exist** (T054–T058, T064). Gated on **T072**: no CI
  topology has a database outside the cluster under test with an Enterprise licence and an object
  store the database itself can reach. The suites are **written, collectable, and skip by name** on
  `INFRAHUB_TESTING_EXTERNAL_DB`, with skip reasons that name the missing topology and the task.
- **2 `[INFRA]` tasks that cannot be done on a developer machine** (T072, T073). T073 is the one that
  matters most: **the Neo4j seed-from-URI mechanism has never been exercised against a real
  Enterprise server.** It is implemented as designed and unit-tested against fakes; that is not the
  same thing, and the report does not claim otherwise.
- **5 enabling tasks discovered during this run** (T107, T119, T121, T122, T141), each recorded with
  the reason it was not folded into the chunk that found it.

**The k8s e2e leg cannot run on this machine at all** — those suites need `vcluster`, which is not
installed. Only the Docker leg is locally executable. An earlier read that minikube made them
runnable was wrong and is corrected here.

---

## 2. Chunk ledger

Nine planned chunks, then three unplanned fix passes. **No two implementation subagents ran
concurrently.** Every chunk ran in a clean-context subagent; the orchestrator edited no feature code
except three documentation falsehoods (§5) and four task records.

| # | Chunk | Tasks | Outcome | Commits |
|---|---|---|---|---|
| 1 | 2T — Silently wrong or destructive | T097–T102 | 6 ✅ | `64bfdb8` |
| 2 | 2T — Wrong answer, wrong place | T103–T110 | 7 ✅, 1 ⚠️ (T107) | `b83099a` |
| 3 | 2T — Operability code defects | T111–T116 | 6 ✅ | `8a5c4a6` |
| 4 | 2T — Structural dedup + governance | T117–T120 | 3 ✅, 1 ⚠️ (T119) | `7a98676` |
| — | orchestrator: record T121 | — | — | `c8ea8d6` |
| — | orchestrator: record T122 | — | — | `dbfad53` |
| 5 | Phase 3 — Restore path | T046–T053 | 8 ✅ | `7f0bf1d` |
| 6 | Phase 3 — End-to-end | T054–T058 | 5 ⚠️ (topology) | `58f0d91`…`24d27ac` |
| — | **T123 regression fix** (found by chunk 6) | T123 | ✅ | `a181b27` |
| — | orchestrator: gitignore e2e residue | — | — | `5ce2a1f` |
| 7 | Phase 4 — US2 diagnostics | T059–T064 | 5 ✅, 1 ⚠️ (T064) | `4f36c21`, `936a3b6` |
| 8 | Phase 5 — Documentation | T065–T068 | 4 ✅ | `e4bfc2c`, `b018b33` |
| 9 | Phase 5 — Hygiene + T071 audit | T069–T071, T074 | 4 ✅ | `9b3f4b6`, `2e1dd23` |
| — | orchestrator: doc falsehoods | T126–T128 | ✅ | `a65d62a` |
| A | Review fixes — critical + restore safety | T124–T130 | 7 ✅ | `cca32bb`, `25b741f` |
| B | Review fixes — test gaps, dedup, invariants | T131–T141 | 10 ✅, 1 ⚠️ | `42f359b`, `ea943b6`, `74f28e4`, `0cfa614`, `43df4c4`, `a34c5ad` |

### Decisions and surprises flagged upward by chunks

- **Chunk 2 refused to half-fix T107.** Its gate half was already done; the argv half needs a
  stdin-carrying exec primitive neither backend has, because `ExecOptions.Env` renders as an
  `env KEY=VALUE` argv prefix — exactly what FR-014 forbids. Twelve in-deployment sites pass the
  Neo4j password on argv. Recorded as **T121**; T107 stays open on it. Verified independently.
- **Chunk 4 refused T119's fifth clause.** Dropping `getPodStatusesWith`'s selector queries is not a
  dedup: the namespace listing supersedes them only where they matched *nothing*, so a naive removal
  would let `IsRunning` answer true from a claimed-but-unlabelled pod and delete the
  undetermined-status reporting `stopAppContainers` depends on. Recorded as **T122**.
- **Chunk 5 found two live bugs while building.** `transientWorkloadSpec.validate()` rejected every
  role but probe/capture, so **a restore workload could never have been created** — the restore path
  was unreachable before it was written. And `restore --latest --s3` called the restore gate twice,
  colliding on the pod name.
- **Chunk 6 found the T123 regression** (§5), which no unit test could have caught.
- **Chunk 7 caught its own design error before shipping it** — it first hung the location report on
  `DetectEnvironment`, then found that is also step 1 of `create`, `restore`, both Plakar paths and
  the taskmanager flushes, which would have put two pod listings on every flush.
- **Chunk 8 found seven places where spec/plan/research prose contradicted shipped reality** and
  documented reality. Chunk-9 and the comment reviewer later confirmed 5 of the 7 already resolved
  in shipped text; the remaining 2 were fixed in `a65d62a` and `43df4c4`.
- **Chunk 9 disproved a claim chunk 2 had made.** Chunk 2 reported that T103 already covered T074;
  it had guarded the *ownership policy*, a different question from FR-028's exclusion. The client
  side still tested the pod's *name*, so the guard did rest on `transientObjectPrefix`'s spelling.
  Fixed, and the hazard proved latent before being closed.
- **Fix B found a production bug all six reviewers missed**: `initPlakarContext` never set
  `KContext.CacheDir`, so kloset wrote repository state into the operator's **working directory** and
  `--plakar-cache-dir` never reached it. This is the true cause of the "Plakar e2e leaves a repo in
  the project root" hazard the orchestrator had papered over with a `.gitignore` entry.

---

## 3. Tasks not completed

| Task | Reason | Unblocked by |
|---|---|---|
| T054 | External-topology e2e fixtures written; never executed | T072 |
| T055 | 3 cases written, collect, skip by name | T072 |
| T056 | 3 cases written, collect, skip by name | T072 + seed store |
| T057 | 4 robustness cases written, collect, skip by name | T072 |
| T058 | Docker leg **ran** (35 passed); k8s leg needs `vcluster`; previous-release artifact unobtainable | `vcluster` + release artifact |
| T064 | 16 diagnostic cases written, collect, skip by name | T072 |
| T072 | `[INFRA]` — CI topology with an out-of-cluster Enterprise database + reachable object store | provisioning |
| T073 | `[INFRA]` — validate seed prerequisite against a real customer estate | customer conversation |
| T107 | FR-014's argv half; deliberately not half-fixed | T121 |
| T119 | Fifth clause is a running-check change, not a dedup | T122 |
| T121 | Stdin-carrying exec primitive on both backends + 12 call sites | — |
| T122 | Reproduce selector matches from the listing's label fields | — |
| T141 | `TestVerifySeedReadableRefusesAnUnreadableURI` runs 4s–125s on AWS SDK retry | — |

---

## 4. Local-pass evidence

**No `MISSING` rows.** Every chunk returned evidence; where a report truncated in transit, the
evidence was re-requested or read directly from the file the chunk wrote, and the orchestrator
verified the pass lines itself rather than accepting a summary.

Rows are grouped per chunk rather than per test — ~130 tests were added and a 130-row table would
obscure rather than aid audit. Every row names its test identifiers and the file holding the
**verbatim** runner lines, all of which were read and checked. This grouping is an orchestrator
decision, recorded in §6.

Common environment for all Go rows: repo `/Users/bkohler/automation/opsmill/infrahub-backup`, branch
`007-external-database-backup`, Darwin 25.6.0 / arm64, build tag `untested_go_version` required,
scoped to `./src/...`. No cluster, database, kubectl or Plakar repository needed — all table-driven
over injected runners and fakes.

| Chunk / tests | Type | Run command | Passed at (ISO 8601) | Env | Verbatim pass lines |
|---|---|---|---|---|---|
| 1 (T097–T102) — `TestDeclaredServiceIn_ReadsTheKeyThatNamesAService`, `TestParseLabelledPods_ReadsTheKeyThatNamesAService`, `TestGetPodStatusesWith_HelmChartLabels`, `TestGetPodStatusesWith_QueryFailures`, `TestStopAppContainers_UndeterminedStatusFailsTheRun`, `TestTransientObjectNamesAreOnePerDatabase`, `TestBuildTransientPodManifestShape`, `TestResolveExternalTLS`, `TestDiscardPlakarComponentsRemovesWhatWasCommitted`, `TestRefuseIncompleteCaptureIsTheReaderOfTheField` (10 top-level, 18 subtests) | unit | `go test -tags untested_go_version -v -run '<10-test regex>' ./src/internal/app/` | 2026-09-03T09:05:40Z | n/a | `scratchpad/evidence.log` — 28 `--- PASS`, 0 FAIL, `ok infrahub-ops/src/internal/app 0.422s` |
| 2 (T103–T110) — 8 added incl. `TestResolutionAndLocationGiveOneAnswer`, `TestFindWorkloadResourceWithReadsPrefixesNamespaceWide`, `TestLocateServiceWithClaimsACloudNativePGCluster`, `TestPostgresClientEnvCarriesTrustMaterial`, `TestProbeExternalPostgresWalksPastASilentMember` | unit | `go test -tags untested_go_version -v -count=1 ./src/...` | 2026-09-03T11:33:24Z | n/a | `scratchpad/T103-T110-test-evidence.txt` — 381 top-level PASS, 0 FAIL, all 3 packages `ok` |
| 3 (T111–T116) — 5 added (`TestTransientPodPlacement`, `TestResolveTransientSchedulingRefusals`, `TestExternalDBSchedulingFlags`, `TestCaptureRolesWithTwoInstancesOnOneHost`, `TestUncontactableEvidenceIsAttributedToAnEndpoint`), 4 extended, 2 signature-modified; net +46 cases | unit | `go test -tags untested_go_version -count=1 -v ./src/...` | 2026-09-03T09:58:24Z | n/a | `scratchpad/chunk3-test-evidence.txt` — 386 top-level PASS, 0 FAIL |
| 4 (T117–T120) — 4 functions / 11 subtests incl. `TestCollectionRefusesAnExternalDatabaseCapture`, `TestStopAppContainersIssuesOneNamespaceListing` | unit | `go test -tags untested_go_version -count=1 -v ./src/...` | 2026-09-03T10:44:23Z | n/a | `scratchpad/chunk4-test-evidence.txt` — 391 top-level PASS, 0 FAIL |
| 5 (T046–T053) — 58 added incl. `TestRestoreNeo4jExternalSeedsAndConfirms`, `TestVerifySeedReadableRefusesAnUnreadableURI`, `TestNeo4jSeedStatements`, `TestConfirmAppContainersQuiesced`, `TestReturnAppContainersToScale` | unit | `go test -tags untested_go_version -count=1 -v ./src/...` | 2026-09-03T13:07Z | n/a | `scratchpad/chunk5-test-evidence.txt` — 408 top-level PASS, 0 FAIL |
| 6 (T054–T057) — 12 e2e cases across `test_k8s_external_neo4j.py`, `test_k8s_external_both.py` | **e2e** | `uv run pytest -v -m k8s tests/e2e` (CI) | **deferred — local e2e not supported** | needs out-of-cluster Enterprise DB + object store (T072); k8s leg needs `vcluster` | collect-only 10 collected exit 0; normal run 10 skipped, reason names `INFRAHUB_TESTING_EXTERNAL_DB` and T072 |
| 6 (T058) — existing Docker suites, unchanged, + 2 new compatibility cases | **e2e** | `uv run pytest -v -m "docker and not enterprise" tests/e2e` | 2026-09-03T~12:20Z | Docker 29.4.0; `-p no:pytest-infrahub-performance-test`; `INFRAHUB_TESTING_IMAGE_VER=1.11.0`; OrbStack `DOCKER_HOST` | tarball `5 passed in 482.27s`; s3 `2 passed`; collect+retention `23 passed, 2 deselected in 200.21s`; new compat `2 passed in 247.12s`; plakar `3 failed` → **fixed by `a181b27`**, re-run `3 passed, 1 xfailed in 519.64s` |
| T123 fix — `TestShippedRestoreCommandInvocationContract` (11 subtests) + `TestResolveConfiguration` | unit + e2e | `go test -tags untested_go_version -count=1 -run '…' ./src/cmd/… ./src/internal/app/…` | 2026-09-03T12:34Z (RED), green after fix | n/a | `scratchpad/t123/EVIDENCE.txt` — RED RUN shows `--- FAIL: …/plakar:_bare_restore_is_accepted` ×3 against neutralised fix; final run `--- FAIL lines: 0` |
| 7 (T059–T063) — 18 added incl. `TestFailureMessageContract` (10 rows), `TestReportDatabaseLocations`; **mutation-verified** (weakening 2 messages → 5 failures) | unit | `go test -tags untested_go_version -count=1 -v ./src/internal/app/` | 2026-09-03T13:14Z | n/a | `scratchpad/EVIDENCE-chunk7-T059-T064.txt` — 0 FAIL, `ok infrahub-ops/src/internal/app` |
| 7 (T064) — 16 diagnostic e2e cases | **e2e** | `uv run pytest -v -m k8s tests/e2e` (CI) | **deferred — local e2e not supported** | T072; `vcluster` absent | 16 collected 0 errors; 16 skipped with T072 reason string |
| 8 (T065–T068) — no tests; documentation | docs | `vale $(find ./docs …)`; `rumdl check .` | 2026-09-03T13:28Z | n/a | `0 errors, 18 warnings in 18 files` (= baseline); `Success: No issues found in 33 files` |
| 9 (T069–T071, T074) — 2 subtests + T071 audit; guard mutation-proven | unit | `go test -tags untested_go_version -count=1 ./src/...` | 2026-09-03T16:02Z | n/a | `scratchpad/EVIDENCE-chunk9-T069-T071-T074.md`; reverting the guard fails both new subtests (`t074_mutation.log`) |
| Fix A (T124–T130) — incl. `TestResolutionIssuesOneNamespaceListing`, `TestRestoreLatestBackupReleasesTransientWorkloads`; **424 fn / 1427 subtests** | unit | `go test -tags untested_go_version -count=1 ./src/...` | 2026-09-03T15:39Z | n/a | `scratchpad/EVIDENCE-fix-A-critical-and-restore-safety.md`; RED RUN `--- FAIL: TestRestoreLatestBackupReleasesTransientWorkloads` with defer removed |
| Fix B (T131–T141) — incl. `TestCypherColumnLookupIsIndifferentToCase`; 4 mutations killed and reverted; **430 fn / 1450 subtests** | unit | `go test -tags untested_go_version -count=1 ./src/...` | 2026-09-03T16:37Z | n/a | `scratchpad/EVIDENCE-fix-review-b.md`; each mutation block ends `### REVERTED, re-run exit=0` |

**Final gate state, verified by the orchestrator directly (not relayed):**
`go test -tags untested_go_version -count=1 ./src/...` → all packages `ok`, **1450 passed, 0 failed,
0 skipped**. `golangci-lint run --build-tags untested_go_version ./src/...` → `0 issues.`
`go vet -tags untested_go_version ./src/...` → exit 0. `make build` → all three binaries.
Vale `0 errors, 18 warnings` (baseline). rumdl `0 issues in 33 files`.

---

## 5. Review findings

A six-agent review (`code`, `errors`, `tests`, `comments`, `types`, `simplify`) over `080917d..HEAD`,
scoped to this round because earlier commits were reviewed twice. **21 findings; 20 fixed, 1
deferred.**

| Severity | File / symbol | Finding | Status |
|---|---|---|---|
| **CRITICAL** | `environment_kubernetes.go` `namespacePodListing` | Memoized listing froze every liveness poll. On Helm the fallback is the *normal* path, so `confirmAppContainersQuiesced` could never succeed — and it fails **after** `wipeTransientData()`, bricking the restore and blaming a pod that terminated normally. Mirror case: a successful restore reports all six services failed to return. **Found independently by two reviewers.** | ✅ `cca32bb` |
| **CRITICAL** | `backup_containers_test.go` | FR-013's three `defer returnAppContainersToScale` blocks could all be deleted with the suite green. Mutation-proven. | ✅ `ea943b6` |
| **CRITICAL** | `backup_containers_test.go` | FR-026's `confirmAppContainersQuiesced` could be removed from `RestoreBackup` and all four Plakar sites with the suite green. Mutation-proven. | ✅ `ea943b6` |
| important | `main.go` `restoreCmd.Args` | **T123**: cobra runs `ValidateArgs` before `PersistentPreRunE`, where `b867f8e` had moved config resolution — so `--backend plakar restore` was refused for arity. CI red on 3 Docker e2e tests. | ✅ `a181b27` |
| important | `backup_metadata.go` `applySourceProvenance` | `capture_complete` computed **before** the captures ran, so it was `true` in every artefact and `refuseIncompleteCapture` was unreachable in the only direction that matters. | ✅ `25b741f` |
| important | `restore_latest.go` | `RestoreLatestBackup` created transient workloads with **no deferred release**; two doc comments asserted the opposite. | ✅ `25b741f` |
| important | `backup_containers.go` `startAppContainers` | Returned on first failure with `cache` first in order, leaving five services at zero — FR-013 defeated by a failure a retry would have survived. | ✅ `25b741f` |
| important | `external_restore.go` `waitForNeo4jRestore` | Retried **permanent** failures for the full 2h bound with Infrahub at zero, then blamed the database for a cluster failure. | ✅ `25b741f` |
| important | `plakar_restore.go` | Community refusal fired **after** services were stopped, unlike every other refusal on this feature. | ✅ `25b741f` |
| important | `external_endpoint.go` `undiscoverableEndpoint` | An RBAC denial was reported as "no address configured" — the same FR-004/FR-019 conflation that caused the original reset. | ✅ `25b741f` |
| important | `external-databases.mdx`, ADR-0008 | RBAC verb list omitted **`watch`**, which `kubectl wait` needs. An operator granting exactly the documented set fails at the readiness wait. | ✅ `a65d62a` |
| important | `external-databases.mdx` | Claimed "earlier versions connected to it without one" — no released version reaches an external database at all. | ✅ `a65d62a` |
| important | `contracts/cli-surface.md` | `--external-db-insecure-tls` marked "Applies to: backup"; it governs restore too. | ✅ `a65d62a` |
| important | `external_backup_gate.go` | Comment said "both callers"; there are three. | ✅ `43df4c4` |
| important | `plakar.go` `initPlakarContext` | **Found by fix B, missed by all six reviewers**: `KContext.CacheDir` never set, so kloset wrote repository state into the operator's working directory and `--plakar-cache-dir` reached it never. | ✅ `42f359b` |
| important | `transient_workload.go` | Pod-deadline formula `readyTimeout + N*bound + margin` spelled three times; a comment recorded it having been wrong **twice**. | ✅ `0cfa614` |
| important | `external_endpoint.go`, `external_restore.go` | Three divergent Cypher column-lookup conventions; the raw-constant reader breaks once a column **constant** is mixed-case, which `neo4jStatusColumn = "currentStatus"` already is → FR-005 provenance silently empty. | ✅ `0cfa614` |
| important | `external_endpoint.go` `databaseTarget` | Invariant held by convention: no construction witness, and both preparers take a target by value without re-checking. A hand-built literal would perform an unauthorised external restore. | ✅ `43df4c4` |
| important | `app.go` `ExternalRestoreAuth` | Two fields answering one question; `{Allowed: true}` was representable and would authorise a destructive write while logging "no channel this run can name". | ✅ `43df4c4` |
| suggestion | `external_capture.go` / `external_restore.go` | Duplicated copy prologue; duplicated scratch normalisation with the rationale on only one copy. | ✅ `0cfa614` |
| important | `test_k8s_external_both.py` | FR-021 e2e case corrupted the outer `.tar.gz`, failing gzip **upstream** of seed validation — green without testing its scenario. | ⚠️ retargeted `74f28e4`, **not executed** (T072) |

**What the review found clean, and it matters:** a sweep of all 224 function symbols in the nine
changed Kubernetes/external files, and separately a call-graph check over 720 functions from 39
command entry points, found **all 103 new production symbols have non-test callers and are
transitively reachable**. Zero fictions. The inert-requirement failure that reset this feature did
not recur. 13 of 15 mutations were killed, with a no-op control correctly reporting SURVIVED.

**Two reviewer corrections worth recording**: the comment reviewer found the orchestrator's
paraphrase of AGENTS.md's `nameMatchesService` acceptance test was wrong and the shipped text
correct; fix B found the simplify reviewer's finding-5 diagnosis was one step off (the fragile side
is the caller's spelling, not the header's) and fixed the defect generalised rather than as reported.

---

## 6. Autonomous decisions

1. **Chunk plan**: nine chunks on `tasks.md`'s own phase headings, Phase 2T's 24 findings **first** —
   they are defects in already-landed code, and T097+T098 corrupt a backup in composition. Strictly
   sequential; no two implementation subagents ever ran concurrently.
2. **T118 was escalated to the user, not decided here.** The user chose *refuse external capture from
   `infrahub-collect`*, keeping ADR-0003's read-only guarantee unqualified, over amending the ADR.
   Implemented and verified reachable: `infrahub-collect/main.go:63` → `CollectBundle` →
   `runStandardBackup` → `CreateBackup` → `prepareDatabaseCapture` → refusal, before the reaper and
   before `stopAppContainers`.
3. **Four newly-discovered items were recorded as tasks rather than absorbed** (T121, T122, T141, and
   T107/T119 left open on them). Each would have widened its chunk into a different piece of work;
   T121 in particular is an *internal*-path FR-014 gap the external work merely surfaced.
4. **A regression fix was inserted out of plan** (`a181b27`) rather than deferred, because CI was red
   and continuing would have piled work on a broken branch.
5. **Three documentation falsehoods were fixed by the orchestrator directly** (`a65d62a`), staged
   file-by-file because a fix chunk had in-flight edits. Small, localised, and the `watch` verb one
   would have blocked any operator following the docs.
6. **A `.gitignore` entry was added** (`5ce2a1f`) for the Plakar e2e residue. Fix B later found the
   real cause (`CacheDir`) — so that commit treated a symptom. Kept as defence-in-depth; noted here
   because the framing was wrong.
7. **Review scoped to `080917d..HEAD`**, not the whole 70-commit branch, since earlier commits were
   reviewed twice. Six agents run in parallel — read-only, so the no-parallelism rule did not apply.
8. **§4's evidence table is grouped per chunk, not per test.** ~130 tests were added; a 130-row table
   would obscure audit rather than aid it. Every row names its test identifiers and the file holding
   verbatim runner lines, and those files were read and checked by the orchestrator. This is a
   deliberate deviation from the one-row-per-test instruction.
9. **E2E tasks were reported `⚠️ partial`, never `✅ done`.** A skipped test is not a passing test.
   T058's Docker leg was genuinely attempted and run because minikube and Docker were available; the
   k8s leg proved to need `vcluster` and was reported as blocked rather than skipped quietly.
10. **A stray evidence file written into the repo root was moved to the scratchpad** rather than
    committed or deleted.

---

## 7. Suggested next steps

1. **Provision T072's CI topology** — a database outside the cluster under test with an Enterprise
   licence and an object store the database itself can reach, plus `vcluster` for the k8s leg. Six
   e2e tasks are written and waiting on exactly this, and until it exists **no end-to-end evidence
   for this feature exists at all**.
2. **Have T073's conversation before shipping restore.** The seed-from-URI mechanism is unverified
   against a real Enterprise estate: that `CREATE OR REPLACE … seedURI` is accepted in that form at
   2025.01+, that `s3`/`gs`/`azb` are the actual built-in provider set, that `SHOW DATABASE`'s
   `statusMessage` reads as expected, and that a customer's database host can reach an object store
   at all. Restore rests on a single mechanism whose prerequisites are least likely to hold for
   exactly this population, and both fallbacks were rejected. **This is the highest risk on the
   branch.**
3. **Close T121 and T122**, which unblock T107 and T119. T121 is the FR-014 argv gap on the
   *internal* path — real scope, just not scope this feature created.
4. **Bound T141's test** so suite runtime stops swinging 47s–260s.
5. **Reconcile `spec.md` and `research.md`** with shipped reality. Two known contradictions remain
   after this run's fixes, and the spec is now behind the implementation in documented ways.
6. **Consider a process change.** The dominant defect class was identical in all three review rounds:
   *a new single-source-of-truth introduced, an older parallel answer left standing.* Three rounds is
   a property of how this code is being built, not luck. Two cheap checks caught nearly everything
   this round and could run per-chunk: **grep every new exported symbol for a non-test caller**, and
   **mutation-test every new guard** — the second is what exposed FR-013 and FR-026 as unprotected
   after they had been reported done and verified by grep.
7. Then open a PR. Nothing has been pushed.

> **§7 is superseded in part by §8–§10 below.** Items 3 and 4 are done, item 2's mechanism half is
> answered, and item 1 is much cheaper than it reads. Item 6 stands unchanged and is the one worth
> acting on.

---

## 8. T107 closed, T121 descoped — on FR-014's own wording

FR-014 reads: *"Credentials for an **externally-managed** database MUST NOT be exposed on a process
command line or in workload configuration readable by deployment observers."* It is scoped to
external databases and is already satisfied — T040 moved that path onto the mounted secret, and
`TestNoCredentialReachesAnExternalArgv` pins it.

T107's argv clause is on the **internal** path: after the gate landed in `e49b4be`,
`redactDatabase` and `isNeo4jCluster` only run for a database that lives in the deployment, because
an external one is refused before them. So the gate half was the whole of the defect, and it is
fixed and covered. **T107 closed.**

T121 (a stdin-carrying exec primitive, routing 10 internal `cypher-shell -p<password>` sites)
is **out of scope for 007**, to be filed separately. All 10 sites exist unchanged at base `8186fcf`,
so the branch neither introduced nor worsened them, and doing it here would have forced a re-baseline
of the FR-015 pin that asserts the internal argv character-for-character.

**Residual, accepted knowingly**: the internal path passes the Neo4j password on argv, visible to
anything reading `ps` inside the database container and to the Kubernetes API audit log. Both
populations already hold credential-equivalent access — exec rights on the database pod are what put
you in that container, and they also let you read Neo4j's own configuration — so the marginal
exposure is audit-log retention rather than a new attacker path.

**Process note.** The reviewer's finding was accurate as a description and matched the defect class
that had bitten this branch three times, and it was briefed to a chunk before anyone read the
requirement it cited. Reading FR-014's sentence took one grep and reversed the decision.

---

## 9. Phase 2W — findings from real Neo4j Enterprise servers

The first execution of this feature's mechanism against real servers rather than fakes.
`neo4j:5-enterprise` (**5.26.30**) and `neo4j:2025-enterprise` (**2025.12.1**) as host-side
containers with a non-loopback backup listener, plus MinIO, on one Docker network. **No cluster
needed for any of it.** Evidence: `scratchpad/EVIDENCE-real-neo4j-enterprise.md`.

| Severity | Finding | Status |
|---|---|---|
| **CRITICAL** | **`--expand-commands` aborted every external capture before the database was contacted.** `neo4j-admin` refuses to read any config file looser than 0640 given that flag, and reads both `neo4j.conf` and `neo4j-admin.conf`; the pinned image ships them at **0777**. The failure names a permission the operator never set in an image they did not build. | ✅ `b4402f4` |
| important | The e2e Helm overlay wrote `global.infrahubEnv`, a key the chart does not define. Helm ignores unknown keys silently, so Infrahub would have deployed pointed at the Neo4j the overlay had just disabled. Env is per component, nested under the component's own name **twice**. Confirming these keys was named in the fixture's docstring as part of T072. | ✅ `0f85cc0` |
| important | The seed provider needs a **region**, not only credentials — without one, `CREATE DATABASE` succeeds and the database sits `offline`. Also: it is not a CLI, it is the built-in Java SDK. | ✅ `9ead9bc` |
| important | S3-compatible stores work (`AWS_ENDPOINT_URL_S3` is honoured), so **restore needs no AWS account**. The guide said to configure an endpoint without saying how. | ✅ `9ead9bc` |
| — | **The round trip is proven.** Capture from a separate container against the live listener → object store → seeded into a *different* server → `(:Probe {n:1})` verified by content. Artifact naming matches `neo4jArtifactProduced`. | validated |
| — | **Two design decisions confirmed against reality**: the statement returns success while the database is still loading, and `currentStatus`/`statusMessage` carry the real verdict — which is why T050 exists. | validated |
| — | **The version gate sits exactly on the boundary**: the atomic `CREATE OR REPLACE … seedURI` form is *rejected* at 5.26.30 and *accepted* at 2025.12.1, and `neo4jSeedReplaceFloor = "2025.01"`. | validated |
| — | All four probe statements return the expected columns; values come back **quoted** and `splitPlainRow` strips them correctly. One unrepresented shape: `dbms.components()` returns **two** rows on 2025.x. | validated |

**Why no test caught the critical one.** The argv is asserted as a string; nothing executed
`neo4j-admin`. This is a fourth failure class to add to the three this branch already documented:
not unreachable code, not code wired to the wrong thing, not a guarantee that fails in composition —
**a command that is correct as a string and rejected by the program that runs it.**

---

## 10. T072 and T073 are materially cheaper than this report first said

**T072 is provisioning work now, not an open question.** The topology was stood up on a developer
machine: Neo4j Enterprise and MinIO as host-side containers on one network, and from a pod inside
minikube all three of `host.minikube.internal:6362`, `:7687` and `:9000` are reachable — with the
store reachable *by the database*, which the fixture's own comment calls the hard part. The chart and
images pull **anonymously**; `vcluster` installs from Homebrew; no cloud account is needed. The one
step not yet exercised is getting the Infrahub deployment itself healthy, which is why it stays open.

**T073's mechanism questions are answered; the population question is not.** What remains is whether
a given customer's Neo4j hosts can be given egress to an object store, whether their operators will
accept a server configuration change for a non-loopback backup listener, and the criterion for
falling back to the recreate procedure. That is a conversation, not a test — and it is no longer the
highest risk on the branch, because the mechanism it depended on now demonstrably works.

**Revised next steps**, replacing §7 items 1–4:

1. Get an Infrahub deployment healthy against the external topology, then run the six e2e tasks
   (T054–T058, T064). Everything they need except that is proven.
2. Have T073's customer conversation about prerequisites and the fallback criterion.
3. File T121 as its own issue (internal-path FR-014 hardening).
4. Add a fixture for the two-row `dbms.components()` shape (T147).
5. §7 item 6 stands: add the two cheap per-chunk checks — non-test caller grep, and guard mutation.

## 11. Phase 2X — the fourth review round's high-severity findings, fixed in severity order

The review that followed Phase 2W fanned out fourteen finders. Their findings came back verified but
were never written into the ledger — the same hand-off failure that lost two residuals earlier in
this feature — so before any was fixed the four that matter were recorded as T148–T151
(`82e99fe`), and each subsequent subagent's residuals were recorded as a task *before the next
subagent was dispatched* (T152–T155). Four clean-context subagents, strictly sequential, each with a
mutation check (guard removed → pin fails with the production symptom → guard restored) as evidence.

| Task | Defect | Fix | Commit |
|---|---|---|---|
| T148 | External Neo4j **restore** path unreachable: the edition was read through a capture-only accessor, so restore fell through to `cypher-shell` in a container that does not exist | `externalCaptureSourceFor` deleted; `externalDatabaseFor` answers for either direction from the same embedded probe; 7 callers, incl. `isNeo4jCluster` on the `restoreNeo4jEnterprise` path, which carried the same defect | `ddb6233` |
| T149 | A failed capture commits a **0-byte snapshot and exits 0**. Now *confirmed by a running test* against the vendored kloset v1.1.0-beta.6: failing reader → `Backup()=nil, Commit()=nil, bytes=0 errors=1` | `commitComponentSnapshot` reads the source summary after `Backup` and refuses *before* `Commit` (errors>0 or bytes==0), through `recordIncompleteCapture`, so the empty snapshot never exists. Secondary: `restoreSingleSnapshot` resolves the edition tag only for the Neo4j component | `a070bc5` |
| T150 | Ownership claimed across a service boundary: `infrahub-task-manager-db-exporter` read as `task-manager-db` (external RDS → Internal → `pg_restore --clean --create` into a sidecar) | One recogniser, `controllerGeneratedSuffix`; `serviceWithGeneratedSuffixIndex` is the single claim site for all three name tiers. **Direction B (`pg-task-manager-db-1` → External) is by design** (T103/T105, `b83099a`), pinned not reversed — a spec question, recorded in T154 | `db37ac2` |
| T151 | `CombinedOutput` fed `PodUID`, so a kubectl stderr warning became part of the UID, the Secret's ownerReference matched no pod, and a credential Secret outlived the run | `separatedRunner` is the one executor-reading `podRunner`; both bounded runners delegate to it; stderr folded into the error on failure. FR-019 Forbidden detection now fires on the query/remove paths as a side effect | `5c0a3bf` |

Evidence files: `scratchpad/EVIDENCE-T148.md` … `EVIDENCE-T151.md`, each with the mutation FAIL and
restored PASS verbatim. Gates after `5c0a3bf`: build, vet, full `go test ./src/...`, `gofmt -l`
empty, `golangci-lint` 0 issues.

**What this changes about §1.** Two of the four were "the feature's main path does not run" and "a
backup can report success while containing nothing"; both were invisible to every green gate. They
are fixed, but the reason they existed — a new single answer introduced beside an old one, and a
dependency's contract assumed rather than probed — is the reason to prefer one real end-to-end
restore over a fifth review round. §10's revised next steps stand, with one insertion at the top:

0. Decide T154 (i): whether an in-cluster database under a prefix no release declares is *meant* to
   read External. The code says yes; the spec says nothing.

**Still open from that review, not yet recorded as tasks** (lower severity, listed so they are not
lost again): `transientWorkloadRBAC` string omits `watch`; Plakar `--snapshot` postgres branch never
calls `restartDependencies()`; three documented `infrahub-backup restore` examples cannot run and
`--backup-id` is silently ignored on tarball; the e2e `_external_values_overlay` fans
`PREFECT_API_DATABASE_CONNECTION_URL` to the Neo4j-reading components instead of `prefect-server`;
`execReadCloser` data race and a non-wrapped `errStreamIdleTimeout`; `readinessDiagnosis` blind to
`Unschedulable`; `escapeJSONPathKey` correct but unpinned; T122 applied to one of four identical
paths; the pending-pod `IsRunning` hole.

## 12. Phase 2Y — the first real end-to-end run on Kubernetes

Fresh minikube profile `infrahub-ext`; `neo4j:2025.10.1-enterprise` and MinIO outside the cluster on
one Docker network; chart `4.33.2` with `neo4j.enabled=false`, env on both components, **no
workaround needed** — which closes the step T072 said had never been exercised. Mixed topology,
exactly quickstart Scenario 1.

| Stage | Result |
|---|---|
| S1 `environment detect` | Kubernetes, namespace, database **External** `host.minikube.internal:17687`, task-manager-db **Internal** |
| S2 capture | PASS on the **third** attempt: first needed `--external-db-insecure-tls` (error named no flag), second needed `--external-db-scratch-size` (store-size metric absent on 2025.x). Then: Neo4j component 1,351,984 B, `capture_complete: true`, per-service `external_location`, no transient leftovers, replicas untouched |
| S3 marker | Tag created via GraphQL; seen by Cypher (nodes 19102→19109) |
| S4 refusal | Refused without `--allow-external-restore`, names the flag and the config key; nothing stopped |
| S5 restore | Staged to `s3://infrahub-seed/seed`, seeded, `online after 1 status read(s)`; marker gone by Cypher **and** GraphQL; nodes back to 19102; pods back to 1/2/1 Ready; staged object cleaned; no leftovers |
| S6 version gate | Abort: `utility (5.26.30) is older than the server (2025.10.1)`; 0 archives written |
| S7 collect | Created no transient pod (ADR-0003) |

This is the class of evidence none of the four review rounds could produce, and it did what the
rounds could not: it found that **an explicit `--k8s-namespace` is a preference, not a pin** (T156) —
on a host that also runs an Infrahub compose project, a momentarily unreachable cluster would send
a restore into the wrong deployment — and that **the store-size probe fails on every current
server** (T158), so the documented "derived from the database size" default is dead code on 2025.x.
Findings recorded as T156–T161; the doc's five misleads as T161. The three HIGH items are the next
fixes, in that order. Cluster and containers are left running for inspection; teardown commands are
in the evidence file.
