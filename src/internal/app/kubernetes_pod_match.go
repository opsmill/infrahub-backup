package app

import (
	"strings"
)

// Resolving a deployment service to a pod is the step every other Kubernetes
// operation is built on, and it answers two questions that tolerate a wrong
// answer very differently:
//
//   - "which pod is this service?", asked once the service is known to be here.
//     A wrong answer fails loudly at the next exec.
//   - "is this service here at all?", which is the whole of the
//     internal-versus-external decision. A wrong "yes" routes a database that
//     lives outside the deployment onto the in-deployment path and runs
//     `neo4j-admin` or `pg_dump` — and on the restore side, a load — inside
//     whatever pod happened to match.
//
// This file holds the primitives both use: reading a listing that carries each
// pod's phase, dropping the pods a service cannot be running in, and matching a
// pod name against a service name on something stronger than "contains".

// podNamePhaseJSONPath lists one "<name>;<phase>" pair per pod. Asking for the
// phase alongside the name is what lets a listing be filtered on the pod's
// phase without a second query per pod.
const podNamePhaseJSONPath = "jsonpath={range .items[*]}{.metadata.name}{\";\"}{.status.phase}{\"\\n\"}{end}"

// Pod phases this package reasons about. Succeeded and Failed are the two a pod
// never comes back from.
const (
	podPhaseSucceeded = "Succeeded"
	podPhaseFailed    = "Failed"
)

// livePodFieldSelector keeps a listing to pods a command could still run in, so
// the cluster never offers a finished pod as a candidate for a service. The
// shape that bites is a completed Job pod — a backup CronJob leaves
// `<release>-database-backup-28…` lying around, and a finished Job pod is not a
// database.
const livePodFieldSelector = "status.phase!=" + podPhaseSucceeded + ",status.phase!=" + podPhaseFailed

// completedPodFieldSelector drops only the pods that ran to completion, and is
// what pod *enumeration* uses. The difference from livePodFieldSelector is
// deliberate: a crashed replica is much of what a troubleshooting bundle is
// collected for, so a Failed pod stays enumerable, while a pod that completed
// successfully belongs to a Job rather than to a service.
const completedPodFieldSelector = "status.phase!=" + podPhaseSucceeded

// A pod from either listing is a labelledPod (see kubernetes_ownership.go),
// parsed by parseLabelledPods.
//
// There used to be a second type and a second parser for this listing — a
// name-and-phase podCandidate — and the two parsers differed only in that one
// stopped after the phase. That was not a difference: a `<name>;<phase>` line
// carries no label fields, so the ownership parser reads it identically, while
// the narrower one fed an ownership line's label fields into the phase and
// reported `Running;;;server` as a pod's phase. One parser cannot disagree with
// itself, and every filter below now takes what it returns.

// phaseIs compares a reported phase against one of the constants above. It
// trims, because a phase arrives as a field of a line kubectl formatted and a
// trailing space is not a different phase.
func phaseIs(phase, want string) bool {
	return strings.EqualFold(strings.TrimSpace(phase), want)
}

// podPhaseIsTerminal reports whether a phase is one a pod never comes back
// from, and is the package's single answer to "has this pod finished?".
//
// It is one predicate rather than one per caller because the callers disagree
// about what a wrong answer costs, not about what the question means: the
// resolver uses it to drop a pod it could not exec into, and the transient
// reaper uses it to decide that another run's pod is safe to delete. A second
// copy that differed by so much as whitespace trimming — which is exactly how
// the reaper's own copy differed — would make the reaper read a phase the
// resolver did not.
func podPhaseIsTerminal(phase string) bool {
	return phaseIs(phase, podPhaseSucceeded) || phaseIs(phase, podPhaseFailed)
}

// livePodNames keeps the pods a service could still be running in: everything
// except the two terminal phases. It is the client-side half of
// livePodFieldSelector, stated twice for the reason the transient-workload
// exclusion is stated twice — the field selector is what keeps the pods out of
// the answer, and this is what still holds if a listing is ever issued without
// it.
func livePodNames(pods []labelledPod) []string {
	return podNames(filterLabelledPods(pods, podPhaseIsTerminal))
}

// podNames projects a listing to the names it carries.
func podNames(pods []labelledPod) []string {
	names := make([]string, 0, len(pods))
	for _, pod := range pods {
		names = append(names, pod.Name)
	}

	return names
}

// podPhaseIsCompleted reports whether a pod ran to completion, and is the
// package's single answer to "did this pod finish successfully?" for the same
// reason podPhaseIsTerminal is the single answer to the terminal question: the
// enumerating callers — the listing filter, the ownership filter and the
// fallback resolver — must all draw the line in the same place.
func podPhaseIsCompleted(phase string) bool {
	return phaseIs(phase, podPhaseSucceeded)
}

// enumerablePodNames keeps every pod except those that ran to completion; see
// completedPodFieldSelector for why enumeration draws the line differently
// from resolution.
func enumerablePodNames(pods []labelledPod) []string {
	return podNames(runnablePods(pods))
}

// podPhases projects a listing down to the phases the running check reports.
// It is the answer IsRunning reduces to "is any of them Running?", and it is a
// projection rather than a filter: a phase this package does not recognise is
// still a phase the cluster reported, and dropping it here would report the pod
// as absent instead.
func podPhases(pods []labelledPod) []string {
	phases := make([]string, 0, len(pods))
	for _, pod := range pods {
		phases = append(phases, pod.Phase)
	}

	return phases
}

// nameMatchesService reports whether a pod name names the service at a token
// boundary, which is the whole of the basis a name-shaped match rests on.
//
// It is not a decision, and nothing outside kubernetes_ownership.go calls it.
// "Does this name name the service?" is a question about a name; "is this
// resource this deployment's?" is the question every caller actually has, and
// deploymentClaimsResource is the one answer to it.
//
// Pod names are hyphen-joined — `<release>-<service>-<ordinal-or-hash>` — so a
// name that means the service starts either the name or one of its segments
// with it. Requiring the boundary separates a name that names the service from
// one that merely contains its letters: `infrahub-database-0` names `database`,
// `mydatabasetool-0` does not.
//
// It requires no boundary on the right, and that is what it is for: it answers
// whether a name names the service at all, which `infrahub-cache-exporter`
// does. A claim reads the right-hand side as well (see
// controllerGeneratedSuffix), and the gap between the two — a name that names
// the service and is not claimed — is exactly the near miss
// logUnclaimedResource reports.
func nameMatchesService(name, service string) bool {
	return serviceBoundaryIndex(name, service) >= 0
}

// serviceBoundaryIndex returns the index at which name carries service at a
// token boundary, or -1 when it does not. It is the scan nameMatchesService
// reduces to a yes/no, kept separate because ownership needs the position too:
// what a name carries *before* the service is the release prefix that says
// whose workload it is (see releasePrefixOf).
func serviceBoundaryIndex(name, service string) int {
	if service == "" {
		return -1
	}
	if strings.HasPrefix(name, service) {
		return 0
	}
	if index := strings.Index(name, "-"+service); index >= 0 {
		return index + 1
	}

	return -1
}
