# 16. One ownership decider: label evidence and declared release prefixes, never a bare name

**Status**: Accepted
**Date**: 2026-09-07
**Source**: specs/archive/007-external-database-backup/retrospective.md (R10), spec FR-002/FR-028, the shipped design in `src/internal/app/kubernetes_ownership.go`

## Context

Three destructive decisions on the Kubernetes backend rest on "is this resource the deployment's":
where a database lives (ADR 0012), which workload to scale to zero, and whether a service is
running before an offline capture proceeds. Before spec 007 each answered it with its own
substring test — `strings.Contains(name, service)` — and swallowed kubectl errors as empty results.
The retrospective recorded the consequences: `redis-cache` resolved as `cache` and was scaled to
zero, a `postgres-database-0` from an unrelated StatefulSet would have put a genuinely external
database on the in-deployment path, and a transient API error fell through to the name match.
The remediation phases replaced the three rules with one.

## Decision

- **`deploymentClaimsResource` is the only function that decides ownership**, and every caller
  that acts destructively goes through it. Nothing else may decide from a name; the acceptance
  check is that `grep -rn 'nameMatchesService(' src/ | grep -v _test` returns only its definition
  and its call sites inside the ownership file.
- **Evidence is read in order.** First, a label *declaration* — the pod or workload says which
  service it is, under any of the four service-label keys, including the chart's shortened values
  such as `infrahub/service=server`. Second, a name the deployment's *declared release prefixes*
  claim: `<prefix>-<service>-<generated suffix>`, where the prefixes are read namespace-wide from
  every labelled resource, across both workload kinds together. Third, only for a namespace that
  declared no prefix at all, a per-service policy: an app service still claims an unanchored
  prefixed name (the cost of narrowing is a workload left running through an offline capture); a
  database refuses it (the cost of widening is scaling or restoring the wrong database).
- **The right-hand side is read the same way in every tier**: a controller-generated suffix (a
  StatefulSet ordinal, a ReplicaSet hash) is what follows the service; a word does not, so
  `infrahub-cache-exporter` is never the cache.
- **`nameMatchesService` answers what a name *looks like*, never what it belongs to.** The gap
  between the two — a name that names the service and is not claimed — is logged as a near miss.
- **One parser for every pod listing**, carrying name, phase and the four label values, so a label
  selector's four queries can be answered out of a single namespace listing and a declaration read
  from the same fields.
- **A kubectl failure is collected and reported, never treated as an empty result.** A namespace
  that answered every query and matched nothing reports "no workload"; one that failed to answer
  reports the failures, and the caller's non-destructive branch depends on telling those apart.

## Consequences

- Every ownership decision is now one grep away, and a change to the rule is made once.
- The running check costs one kubectl call per service instead of four selector queries plus a
  fallback, because the selectors are reproduced from the listing's own label fields.
- Two shapes remain deliberately distinct: ownership claims a pod carrying a release prefix, and a
  selector reproduction does not, so `IsRunning` cannot answer true from evidence no selector
  produced.
- The generated-suffix recogniser is a shape heuristic with an acknowledged miss (a hash with no
  digit is declined). Reading `pod-template-hash` and the StatefulSet pod-name label from the
  listing would make it exact; that is recorded as follow-up, not done.

## Alternatives Considered

- **`strings.Contains(name, service)`** — the shipped behaviour that scaled another product's
  workload to zero.
- **A rule per caller** — the three callers had drifted on the unanchored case without any of them
  recording it as a choice.
- **Label evidence only** — the chart does not label every resource, and a deployment with no
  labels at all still has to be backed up; the prefix tier is what serves it.
- **Fall back to a bare name listing when the labelled listing fails** — the one remaining path
  where a kubectl failure could end in a foreign workload being scaled.
