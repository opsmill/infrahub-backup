package app

import (
	"slices"
	"strings"

	"github.com/sirupsen/logrus"
)

// Scaling a workload to zero is the only thing the backup does to a Kubernetes
// namespace that the namespace does not get back by itself, and it is decided
// from a service name that is not Infrahub's to own. `cache` and
// `message-queue` in particular name whatever a namespace happens to co-host:
// `redis-cache`, `othertool-cache-0`, `rabbitmq-message-queue`,
// `nats-message-queue`. Resolving a stop target by "the name contains the
// service" therefore does not resolve Infrahub's workload — it resolves the
// first workload in the namespace whose name reads like one, reports it as
// Infrahub's, stops it and scales it to zero.
//
// A token boundary alone does not separate them either: `redis-cache` names
// `cache` at a boundary exactly as `infrahub-cache-0` does. What separates them
// is not the shape of the name but who the name belongs to, so this file
// answers a narrower question than "which pod is this service?":
//
//	does this deployment claim it?
//
// Two things count as a claim, and nothing else does:
//
//   - a label. A pod or workload carrying one of serviceLabelKeys set to the
//     service — or to the chart's shortened form of it, since `infrahub-server`
//     pods carry `infrahub/service=server` — is the resource declaring what it
//     is. This is the same evidence locateServiceWith decides on, for the same
//     reason: a name resemblance is evidence about a name.
//   - a name. Either the service's own name — nothing before it — or a name
//     carrying a release prefix the deployment declared; and in either case
//     nothing after the service a controller did not generate
//     (`task-manager-db-1`, `infrahub-database-0`; see
//     controllerGeneratedSuffix). `infrahub-cache-0` is claimed because
//     `infrahub-server-…` beside it, labelled `infrahub/service=server`, says
//     this deployment's release is named `infrahub`; `othertool-cache-0` is not
//     claimed, because nothing in the namespace says `othertool` is this
//     deployment; `infrahub-cache-exporter` is not claimed, because a word
//     after the service names a different component, whatever precedes it.
//     This is what keeps a deployment whose pods are labelled differently
//     working, instead of trading a destructive defect for a silently skipped
//     stop.
//
// Anything else is *unknown*, and unknown never authorises a stop. Absence of
// evidence is not evidence: neither "no label matched" nor "the query failed"
// says the workload is Infrahub's, and neither says it is not.
//
// Where no ownership evidence exists at all — a namespace in which nothing
// carries a service label — a *prefixed* name is read as the current release
// reads it for the app services, and refused for the two databases. Being
// stricter than the shipped behaviour requires evidence to be strict with, and
// there is none; being permissive about a database means running a capture, a
// scale or a restore against a server that is not the deployment's. Which of
// the two applies is unanchoredNamePolicyFor's answer and no caller's.

// serviceLabelKeys are the label keys a deployment declares its services with,
// in the order they are trusted. podSelectors builds its selectors from these,
// so the keys the tool queries by and the keys it reads ownership from cannot
// drift apart — and they are the same four externalMisdiagnosisHint names to
// the operator.
var serviceLabelKeys = []string{
	"app.kubernetes.io/component",
	"app",
	"component",
	"infrahub/service",
}

// infrahubServiceLabelValues is the deployment's own vocabulary: the values a
// service label takes on a resource that belongs to Infrahub. A resource
// declaring one of these is what establishes the release prefix the rest of the
// deployment's names are built from.
//
// `server` is listed beside `infrahub-server` because the Helm chart labels
// those pods with the shortened form (see serviceLabelValues).
var infrahubServiceLabelValues = map[string]bool{
	"infrahub-server":             true,
	"server":                      true,
	"task-worker":                 true,
	"task-manager":                true,
	"task-manager-background-svc": true,
	"cache":                       true,
	"message-queue":               true,
	"database":                    true,
	"task-manager-db":             true,
}

// serviceLabelValues returns the values a deployment may label service with:
// the canonical name, plus the chart's shortened form for the services whose
// canonical name carries the product prefix. `infrahub-server` pods carry
// `infrahub/service=server`, which is the one documented reason the name
// fallback exists at all — reading it as a label puts that case back on label
// evidence rather than on a name guess.
func serviceLabelValues(service string) []string {
	values := []string{service}
	if short := strings.TrimPrefix(service, "infrahub-"); short != service && short != "" {
		values = append(values, short)
	}

	return values
}

// declaredServiceIn returns the service the supplied label sets declare,
// reading serviceLabelKeys in the order podSelectors tries them so a resource
// carrying several of the keys is read the same way a selector query would
// have matched it. It returns "" when none of the keys names a service.
func declaredServiceIn(labelSets ...map[string]string) string {
	for _, key := range serviceLabelKeys {
		for _, labels := range labelSets {
			if service := declaredServiceValue(labels[key]); service != "" {
				return service
			}
		}
	}

	return ""
}

// declaredServiceValue returns value when it names a service in this
// deployment's own vocabulary, and "" when it does not.
//
// It is what makes "the first key that is set" and "the first key that declares
// a service" different answers, and only the second is ownership evidence. The
// Helm chart labels every pod `app: <release>`, so a resource carrying both
// `app: infrahub` and `infrahub/service: server` declares its service in the
// *later* key — and reading the first non-empty value returned the release
// name, which is not a service, from every pod in the namespace. Nothing then
// declared a service, releasePrefixes found no prefix to anchor a name to, and
// the two decisions that rest on that evidence both failed silently: the
// running check stopped nothing and the location decision read an in-cluster
// database as external.
//
// A value outside the vocabulary is not evidence either way, and is skipped
// rather than returned: `app: redis` on `redis-cache-0` is a co-hosted chart
// describing itself, which is exactly as unclaimed as a pod carrying no label
// at all.
func declaredServiceValue(value string) string {
	trimmed := strings.TrimSpace(value)
	if !infrahubServiceLabelValues[trimmed] {
		return ""
	}

	return trimmed
}

// declaresService reports whether a declared service label value names service,
// allowing for the chart's shortened form.
func declaresService(declared, service string) bool {
	if declared == "" {
		return false
	}

	return slices.Contains(serviceLabelValues(service), declared)
}

// releasePrefixes reads the deployment's own naming from the resources that
// declared themselves: `infrahub-server-7d9f4-abcde` carrying
// `infrahub/service=server` says the release is named `infrahub`, so
// `infrahub-cache-0` is this deployment's and `othertool-cache-0` is not.
//
// declared maps a resource name to the service value its labels declare. Only
// values in the deployment's own vocabulary are read, so a co-hosted chart's
// pod cannot contribute its prefix. The result is sorted, so a namespace
// produces one stable answer rather than one per map iteration.
func releasePrefixes(declared map[string]string) []string {
	prefixes := map[string]bool{}
	for name, service := range declared {
		if !infrahubServiceLabelValues[service] {
			continue
		}
		if prefix := releasePrefixOf(name, service); prefix != "" {
			prefixes[prefix] = true
		}
	}

	ordered := make([]string, 0, len(prefixes))
	for prefix := range prefixes {
		ordered = append(ordered, prefix)
	}
	slices.Sort(ordered)

	return ordered
}

// releasePrefixOf returns the text a name carries before the service it
// declares — `infrahub` for (`infrahub-cache-0`, `cache`) — or "" when the name
// starts with the service or does not carry it at a token boundary. An empty
// prefix is not a prefix: a deployment naming its pods bare (`cache`) declares
// nothing about the names of its other services.
func releasePrefixOf(name, service string) string {
	index := serviceBoundaryIndex(name, service)
	if index <= 0 {
		return ""
	}

	return name[:index-1]
}

// nameIsDeploymentOwned reports whether a name-shaped match may be acted on.
//
// A name is read at both ends, and the two ends answer different questions.
//
// What follows the service says whether this is the service's resource at all,
// and it has one answer for every name, whatever precedes it: nothing a
// controller did not generate (see controllerGeneratedSuffix).
// `infrahub-task-manager-db-exporter` carries the deployment's own release
// prefix and is still a metrics sidecar; claiming it read an external RDS as
// the in-deployment database and ran `pg_restore --clean --create` into the
// sidecar. `infrahub-task-manager-db-0` carries `task-manager` at a boundary
// and is the database, not the task manager.
//
// What precedes the service says whose it is, and there are two ways it can be
// this deployment's; they are not the same evidence:
//
//   - Nothing (see nameIsServiceOwnName): `cache`, `database-0`,
//     `task-manager-db-1`. There is no release name for the match to be
//     misreading. This is the shape an externally-managed CloudNativePG
//     cluster has — pods `<cluster>-1`, labelled `cnpg.io/cluster` and so
//     declaring nothing this tool reads — and refusing it is what read an
//     in-cluster task-manager database as external and refused its restore
//     outright.
//   - A release prefix this deployment declared. `infrahub-cache-0` is claimed
//     because `infrahub-server-…` beside it says this deployment's release is
//     named `infrahub`; `postgres-database-0` and `pg-task-manager-db-1` are
//     not, because nothing in the namespace says `postgres` or `pg` is this
//     deployment. That refusal does not turn on whether some *other* release
//     declared itself: a database under a prefix nothing declared is refused
//     with `infrahub` declared beside it, and — through
//     unanchoredNamePolicyFor — with nothing declared at all.
//
// Every resource the note at the top of this file was written to keep out
// carries something before the service — `redis-cache`, `othertool-cache-0`,
// `rabbitmq-message-queue`, `nats-message-queue`, `postgres-database-0` — so
// the first rule admits none of them; each of them has to clear the second.
func nameIsDeploymentOwned(prefixes []string, name, service string) bool {
	if nameIsServiceOwnName(name, service) {
		return true
	}
	if serviceWithGeneratedSuffixIndex(name, service) < 0 {
		return false
	}

	for _, prefix := range prefixes {
		if strings.HasPrefix(name, prefix+"-") {
			return true
		}
	}

	return false
}

// nameIsServiceOwnName reports whether a resource is named for the service
// itself: the service's name, with nothing before it and nothing after it but
// what a controller generated.
//
// It is the one name-shaped claim that needs no release prefix behind it,
// because there is no prefix in it to attribute. `task-manager-db-1` is a
// CloudNativePG cluster named for the service; `postgres-database-0` is another
// product's, and says so in the nine characters before the service name.
func nameIsServiceOwnName(name, service string) bool {
	return serviceWithGeneratedSuffixIndex(name, service) == 0
}

// serviceWithGeneratedSuffixIndex returns the index at which name carries
// service at a token boundary with nothing after it a controller did not
// generate, or -1 when it does not.
//
// It is the one place the right-hand side of a name is read as a claim.
// nameIsServiceOwnName asks whether the index is 0, nameIsDeploymentOwned
// whether what precedes it is a declared release, and the unanchored tier of
// deploymentClaimsResource whether it exists at all — so the three cannot come
// to disagree about what an `-exporter` is. nameMatchesService is deliberately
// not this: it answers whether a name names the service, which
// `infrahub-cache-exporter` does, and that is the near miss
// logUnclaimedResource reports.
func serviceWithGeneratedSuffixIndex(name, service string) int {
	index := serviceBoundaryIndex(name, service)
	if index < 0 || !controllerGeneratedSuffix(name[index+len(service):]) {
		return -1
	}

	return index
}

// controllerGeneratedSuffix reports whether what follows the service in a name
// is something a controller generated rather than a word somebody chose, which
// is what separates the service's own resource from a different component named
// after it:
//
//   - nothing (`cache`, `infrahub-task-manager-db`) — the workload itself.
//   - one segment carrying a digit (`database-0`, `infrahub-task-manager-5f8c9`)
//     — a StatefulSet ordinal, or a hash on its own.
//   - a digit-carrying segment and then five characters
//     (`infrahub-server-7d9f4b6c8-abcde`) — a ReplicaSet pod's template hash
//     and random suffix. The suffix may be letters alone, because five
//     characters drawn at random often are; the hash slot may not, because
//     that is where `exporter`, `migrate` and `db` sit.
//   - anything else — a *different* workload named after this one.
//     `-exporter` is a metrics sidecar, `-backup-28123456-x9v2t` is a
//     CronJob's pod, `-exporter-6b8f7c9d4-lm2xq` is the sidecar's Deployment
//     pod, and `-db-0` is what `infrahub-task-manager-db-0` carries after
//     `task-manager`. A restore that resolved any of them would scale it and
//     then try to load a database into it.
//
// A template hash that happens to contain no digit reads as a word here and is
// declined. That is the safe direction, and a rare one: the hash alphabet is
// six digits to four letters.
func controllerGeneratedSuffix(rest string) bool {
	if rest == "" {
		return true
	}
	if !strings.HasPrefix(rest, "-") {
		// `databasetool-0` starts with `database` and is not it: the service
		// name has to end where a segment ends.
		return false
	}

	segments := strings.Split(rest[1:], "-")
	switch len(segments) {
	case 1:
		return isGeneratedSegment(segments[0])
	case 2:
		return isGeneratedSegment(segments[0]) && isRandomSuffix(segments[1])
	default:
		return false
	}
}

// isGeneratedSegment reports whether a name segment is lowercase letters and
// digits with at least one digit among them: an ordinal or a hash, and not a
// word anyone chose.
func isGeneratedSegment(segment string) bool {
	digit := false
	for _, r := range segment {
		switch {
		case r >= '0' && r <= '9':
			digit = true
		case r >= 'a' && r <= 'z':
		default:
			return false
		}
	}

	return digit
}

// isRandomSuffix reports whether a segment has the shape of the five characters
// a controller appends to a pod's name.
func isRandomSuffix(segment string) bool {
	if len(segment) != 5 {
		return false
	}
	for _, r := range segment {
		if (r < '0' || r > '9') && (r < 'a' || r > 'z') {
			return false
		}
	}

	return true
}

// unanchoredNamePolicy is what a *prefixed* name match means in a namespace
// that declared no release prefix at all — the one thing the callers of
// deploymentClaimsResource legitimately disagree about. A name that is the
// service's own is not covered by it: that case is decided in
// nameIsDeploymentOwned and decided the same way for every caller.
type unanchoredNamePolicy bool

const (
	// unanchoredNameClaims keeps the shipped reading of a prefix nothing
	// anchors: it claims. It is what the app services use: with nothing in the
	// namespace declaring a service label there is no prefix to be strict with,
	// and narrowing would leave a workload running through an offline capture
	// on a deployment that has always worked — `infrahub-server-7d9f4-abcde` in
	// a kustomize namespace has a prefix and nothing to check it against. What
	// follows the service is not the policy's to decide; see
	// deploymentClaimsResource.
	unanchoredNameClaims unanchoredNamePolicy = true

	// unanchoredNameIsNotEvidence refuses it. It is what the databases use,
	// because there the wrong answer is not a skipped stop: an unrelated
	// `postgres-database-0` accepted as the deployment's own routes a database
	// that lives outside the cluster onto the in-deployment path, and the run
	// then execs `neo4j-admin` — on restore, a load — inside it.
	unanchoredNameIsNotEvidence unanchoredNamePolicy = false
)

// unanchoredNamePolicyFor is the package's single answer to "what does an
// unanchored prefixed name mean for this service?", and every caller of
// deploymentClaimsResource takes its policy from here.
//
// It used to be one hard-coded constant per call site, which is how the same
// evidence produced two answers to one question. Pod resolution claimed a
// prefixed name nothing anchored while the location decision refused it, so a
// co-hosted `postgres-database-0` was read as external — correctly — and then
// exec'd into anyway by the discovery read that adopts *its* connection URL as
// the endpoint a restore writes into. The policy belongs to the service, not to
// the caller:
//
//   - An app service. Refusing a name nothing anchors leaves a workload running
//     through an offline capture on a deployment that has always worked, and
//     there is no evidence in such a namespace to be strict with. This is the
//     case unanchoredNameClaims was written for.
//   - A database. Every wrong answer about a database is a command run against
//     the wrong server: a capture read from it, a scale applied to it, a
//     restore loaded into it. Nothing in this tool stops a database for a
//     backup, and the one caller that scales one is a restore starting
//     `task-manager-db` — a step it already skips when the workload cannot be
//     resolved. So the permissive reading buys nothing and costs an unrelated
//     `postgres-database-0` being acted on.
func unanchoredNamePolicyFor(service string) unanchoredNamePolicy {
	if service == serviceNeo4j || service == serviceTaskManagerDB {
		return unanchoredNameIsNotEvidence
	}

	return unanchoredNameClaims
}

// logUnclaimedResource records a resource this deployment did not claim but
// whose name names the service. That near miss is what an operator
// investigating a skipped stop, an unexpected "external" or an empty pod
// listing needs to see; a name that does not name the service at all is not a
// near miss and is not logged.
//
// It is here rather than at each call site so that nameMatchesService — a
// question about the *shape* of a name — has no caller outside this file, where
// the question every decision actually asks is "does this deployment claim it?".
func logUnclaimedResource(kind, name, declared, service string, prefixes []string) {
	if !nameMatchesService(name, service) {
		return
	}

	logrus.Debugf("Ignoring %s %s for service %s: it names the service but this deployment does not claim it (declared %q, release prefixes %v)",
		kind, name, service, declared, prefixes)
}

// deploymentClaimsResource is the whole rule, in one place: a named resource is
// this deployment's when it declares the service outright, or when it carries a
// name this deployment's declared release prefixes claim.
//
// It exists because two callers were asking the same question in two shapes.
// The running check read a namespace's pods for what they declared and what
// they were called; the location decision read the same pods and expressed the
// same test in its own code, so the two could drift — and they had, on the
// unanchored case, without either saying that it was a choice.
//
// It is a choice, and it is a choice about one case only: a name carrying a
// prefix in a namespace that declared none. The choice is taken from
// unanchoredNamePolicyFor here, so it follows the service rather than the call
// site and the same evidence cannot yield two answers — every caller used to
// pass that policy in, and none could have passed another. What none of them
// does any more is express the rule in its own code: there is one
// implementation, and the asymmetry lives in the policy.
func deploymentClaimsResource(prefixes []string, name, declared, service string) bool {
	if declaresService(declared, service) {
		return true
	}
	if nameIsDeploymentOwned(prefixes, name, service) {
		return true
	}

	// A prefixed name and nothing that declared a prefix to measure it against.
	// The policy decides whether an unknown *prefix* claims; what follows the
	// service is read the same way as in every other tier, so an
	// `infrahub-cache-exporter` is not the cache in a namespace that declared
	// nothing any more than in one that declared `infrahub`.
	return len(prefixes) == 0 && bool(unanchoredNamePolicyFor(service)) && serviceWithGeneratedSuffixIndex(name, service) >= 0
}

// podOwnershipJSONPath lists one `<name>;<phase>;<transient>;<label value…>`
// line per pod, with one field per serviceLabelKeys entry after the transient
// marker. Asking for the labels alongside the name is what lets the fallback
// listing carry its own ownership evidence instead of costing a second query
// per pod.
//
// The transient marker sits at one fixed field rather than among the service
// labels because it is read positionally (see podTransientField): the service
// fields are searched for the first value that names a service, and a marker
// found by searching would be a marker whose absence could not be told from a
// field this tool failed to read.
var podOwnershipJSONPath = buildPodOwnershipJSONPath()

// podTransientField is the index of the transient marker in an ownership line,
// and podServiceFieldStart the index the service label fields begin at. A
// podNamePhaseJSONPath line stops before both, which is why every read of them
// is bounds-checked rather than assumed.
const (
	podTransientField    = 2
	podServiceFieldStart = 3
)

func buildPodOwnershipJSONPath() string {
	var path strings.Builder
	path.WriteString(`jsonpath={range .items[*]}{.metadata.name}{";"}{.status.phase}`)
	path.WriteString(`{";"}{.metadata.labels.` + escapeJSONPathKey(transientLabelMarker) + `}`)
	for _, key := range serviceLabelKeys {
		path.WriteString(`{";"}{.metadata.labels.` + escapeJSONPathKey(key) + `}`)
	}
	path.WriteString(`{"\n"}{end}`)

	return path.String()
}

// escapeJSONPathKey escapes the dots in a label key so kubectl reads
// `app.kubernetes.io/component` as one map key rather than as a path. The "/"
// needs no escaping, exactly as helmChartJSONPath has it.
func escapeJSONPathKey(key string) string {
	return strings.ReplaceAll(key, ".", `\.`)
}

// labelledPod is one pod from a podOwnershipJSONPath listing: its name, the
// phase kubectl reported, the service-label values it carries, the service
// those labels declare, and whether it declares itself one of this tool's own
// transient workloads.
type labelledPod struct {
	Name  string
	Phase string
	// Labels are the serviceLabelKeys values the listing carried for this pod,
	// with the keys it carries none of absent.
	//
	// They are kept alongside Service rather than reduced to it because a
	// label *selector* and a *declaration* are two different readings of the
	// same four fields, and the running check needs the first: a selector
	// matches one key set to exactly the service, while Service is the first
	// key whose value names a service in this deployment's own vocabulary.
	// Keeping the fields is what lets the four selector queries be answered
	// out of the listing instead of asked of the cluster — see
	// podsServiceSelectorsWouldMatch.
	Labels map[string]string
	// Service is what those labels declare, in this deployment's vocabulary.
	Service string
	// Transient is what the pod's own transientLabelMarker says. It is the
	// evidence withoutTransientPods prefers over the pod's name, so the FR-028
	// exclusion in the namespace-listing fallbacks does not rest on
	// transientObjectPrefix's spelling.
	Transient bool
}

// parseLabelledPods reads podOwnershipJSONPath output. A line carrying fewer
// fields than expected still yields the name and whatever it did carry: a label
// this tool could not read is not evidence about the pod, and every decision
// below rejects only on evidence it positively has.
//
// The label fields arrive in serviceLabelKeys order, so they are paired back
// with the keys kubectl was asked for them by (serviceLabelsIn) and the
// declaration is then read by declaredServiceIn — the same function the
// workload listing's labels go through. A pod listing and a workload listing
// therefore cannot come to read a declaration differently, and the rule stays
// the one declaredServiceValue states: the declared service is the first key
// that names one, not the first that is set, because a listing that reported
// `app: <release>` as a pod's service is a listing in which no pod declares
// anything.
//
// The transient marker is read from its own field ahead of them, and a value
// other than kubectl's rendering of the label is not the marker: an unlabelled
// pod's field is empty, so "declared transient" is a positive reading like
// every other here.
func parseLabelledPods(output string) []labelledPod {
	pods := []labelledPod{}
	for _, line := range nonEmptyLines(output) {
		fields := strings.Split(line, ";")
		name := strings.TrimSpace(fields[0])
		if name == "" {
			continue
		}
		pod := labelledPod{Name: name}
		if len(fields) > 1 {
			pod.Phase = strings.TrimSpace(fields[1])
		}
		if len(fields) > podTransientField {
			pod.Transient = strings.EqualFold(strings.TrimSpace(fields[podTransientField]), "true")
		}
		pod.Labels = serviceLabelsIn(fields)
		pod.Service = declaredServiceIn(pod.Labels)
		pods = append(pods, pod)
	}

	// The FR-028 exclusion is applied here rather than by each caller, because
	// every caller applied it and the next one has to remember to. A listing
	// this tool's own stand-in appears in is not a listing any decision above
	// may see: the running check would stop a service on it, and the fallback
	// resolvers go on to match a name against a service. Making the filter part
	// of what parsing *means* is what makes the omission unwritable.
	return withoutTransientPods(pods)
}

// serviceLabelsIn reads an ownership line's service-label fields back into the
// label set kubectl was asked for, pairing each field with the serviceLabelKeys
// entry podOwnershipJSONPath asked it for. That pairing is positional and can
// only be, which is the same reason podTransientField sits ahead of these: the
// line carries values, not keys.
//
// A line carrying fewer fields than that contributes the labels it did carry
// and no others — a podNamePhaseJSONPath line carries none, and yields an empty
// set rather than a wrong one. An empty field is not a label either: kubectl
// renders a key the pod does not carry as nothing, so absence and empty are the
// same reading, and every decision downstream rejects only on evidence it
// positively has.
func serviceLabelsIn(fields []string) map[string]string {
	labels := map[string]string{}
	for i, key := range serviceLabelKeys {
		field := podServiceFieldStart + i
		if field >= len(fields) {
			break
		}
		if value := strings.TrimSpace(fields[field]); value != "" {
			labels[key] = value
		}
	}

	return labels
}

// podsServiceSelectorsWouldMatch returns the pods a run of podSelectors'
// queries would have returned, read out of the listing's own label fields.
//
// It is the running check's first tier, and it exists so that tier costs no
// kubectl call: the namespace listing already carries every field those
// selectors select on, so asking the cluster for them again asked it something
// the bytes in hand already answered — four calls per service, twenty-four per
// stop, and four per service per poll of a quiesce or restart wait.
//
// It reproduces the selectors rather than folding them into the ownership rule,
// because the two are not the same question and the difference is a stop
// decision. A selector matches a pod with one key set to exactly the service;
// ownership also claims a pod that merely carries a name this deployment's
// release prefixes claim. Answering from ownership alone would let IsRunning
// report Running from a claimed-but-unlabelled pod — an unlabelled
// `infrahub-cache-1` beside the labelled `infrahub-cache-0` the selectors match
// — which is a pod this deployment claims but not one any selector ever
// returned, and stopAppContainers would stop and scale the service on the
// strength of it.
//
// The selectors are read in serviceLabelKeys order and the first that matches
// anything is the answer, which is what the loop of queries did: it returned on
// the first selector with a non-empty result rather than accumulating across
// all four. A union is a different answer wherever two keys name different
// pods, and the direction it differs in is one more way for IsRunning to answer
// true.
//
// Matching goes through selectorMatchesLabels, which is what
// findWorkloadResourceWith already reproduces its own selector queries with, so
// "would this selector have matched these labels?" keeps one answer across pods
// and workloads.
func podsServiceSelectorsWouldMatch(pods []labelledPod, selectors []string) []labelledPod {
	for _, selector := range selectors {
		matched := []labelledPod{}
		for _, pod := range pods {
			if selectorMatchesLabels(selector, pod.Labels) {
				matched = append(matched, pod)
			}
		}
		if len(matched) > 0 {
			return matched
		}
	}

	return nil
}

// runnablePods drops the pods that ran to completion from an ownership
// listing: it is the client-side half of completedPodFieldSelector, stated
// twice for the reason livePodNames states its own filter twice — the field
// selector is what keeps them out of the answer, and this is what still holds
// if a listing is ever issued without it.
//
// A Failed pod is kept on purpose. It is not somewhere a command can run, but
// it is still evidence of where the service lives, which is a different
// question (see locateServiceWith).
func runnablePods(pods []labelledPod) []labelledPod {
	return filterLabelledPods(pods, podPhaseIsCompleted)
}

// filterLabelledPods drops the pods whose reported phase `terminal` recognises,
// which is the client-side half of whichever field selector the listing was
// issued with. It is one filter rather than one per caller for the reason
// podPhaseIsTerminal is one predicate: the callers differ on where the line is,
// never on what the filtering means.
func filterLabelledPods(pods []labelledPod, terminal func(string) bool) []labelledPod {
	kept := make([]labelledPod, 0, len(pods))
	for _, pod := range pods {
		if terminal(pod.Phase) {
			logrus.Debugf("Ignoring pod %s: phase %s means nothing is running in it", pod.Name, pod.Phase)

			continue
		}
		kept = append(kept, pod)
	}

	return kept
}

// declaredPodServices maps each listed pod's name to the service its labels
// declare, which is the input releasePrefixes reads.
func declaredPodServices(pods []labelledPod) map[string]string {
	declared := make(map[string]string, len(pods))
	for _, pod := range pods {
		if pod.Service != "" {
			declared[pod.Name] = pod.Service
		}
	}

	return declared
}

// declaredWorkloadServices is declaredPodServices for workloads, whose labels
// arrive as the selector and template maps listWorkloads parses.
func declaredWorkloadServices(workloads []kubernetesWorkload) map[string]string {
	declared := make(map[string]string, len(workloads))
	for _, workload := range workloads {
		if service := declaredServiceIn(workload.SelectorLabels, workload.TemplateLabels); service != "" {
			declared[workload.Name] = service
		}
	}

	return declared
}
