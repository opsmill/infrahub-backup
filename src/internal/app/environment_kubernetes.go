package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// errNoPodsMatched is returned by GetAllPods when kubectl calls succeeded but
// no pod matched the service. It is distinct from a real kubectl/cluster
// failure so callers (ServiceReplicas) can record "service not deployed"
// (skipped) only for a genuine empty match, and surface API/RBAC/cluster
// errors as collector failures instead of masking them (FIX-2).
var errNoPodsMatched = errors.New("no pods matched the service")

// podRunner executes a kubectl command and returns its trimmed stdout. The
// shared pod-resolution helpers (getPodForService, GetAllPods — used by the
// backup tool) pass the unbounded stdoutRunner; the collect primitives pass a
// timeout-bounded runner so a wedged API server cannot hang the bundle run
// before the exec/copy timeout even applies (research R2, FIX-5). Both read
// stdout alone, for the reason separatedRunner records.
type podRunner func(name string, args ...string) (string, error)

// stdoutRunner is the unbounded podRunner: separatedRunner's answer with no
// bound on the call, for the backup tool's own path. The four resolvers that
// used to pass the merged executor.runCommand here parse the same JSONPath
// values the bounded ones do, so a kubectl notice corrupted them the same way;
// there is one answer to where a notice lands, and this is how the unbounded
// path gives it.
func (k *KubernetesBackend) stdoutRunner() podRunner {
	return func(name string, args ...string) (string, error) {
		stdout, stderr, err := k.executor.runCommandSeparate(name, args...)

		return stdout, withStderr(err, stderr)
	}
}

// boundedRunner returns a podRunner that bounds every kubectl call by timeout.
func (k *KubernetesBackend) boundedRunner(ctx context.Context, timeout time.Duration) podRunner {
	return separatedRunner(ctx, k.executor, timeout)
}

// separatedRunner is the one podRunner that reads the executor under a bound,
// and it answers with the command's stdout alone.
//
// Every consumer of a podRunner parses what comes back as data — a pod name
// from a JSONPath listing, a phase, the UID a credential Secret's ownerReference
// is built from — and kubectl writes its own notices to stderr on a healthy
// call: a deprecation warning, a kubeconfig complaint, a plugin's chatter.
// Merged output puts that notice in front of the value, so the UID field holds
// `"<warning>\n<uid>"`, the ownerReference matches no live pod, and the Secret
// holding the database password outlives the run (FR-023). Stdout alone is the
// value; stderr goes where a failure's reason belongs, into the error
// (withStderr), and is dropped on success. Both bounded runner constructors —
// this backend's boundedRunner and the external path's boundedExternalRunner —
// come through here, so there is one answer to where a kubectl notice lands.
func separatedRunner(ctx context.Context, executor *CommandExecutor, timeout time.Duration) podRunner {
	return func(name string, args ...string) (string, error) {
		stdout, stderr, err := executor.runCommandSeparateContext(ctx, timeout, name, args...)

		return stdout, withStderr(err, stderr)
	}
}

type KubernetesBackend struct {
	config       *Configuration
	executor     *CommandExecutor
	namespace    string
	podCache     map[string]string
	replicaCache map[string]int // stores original replica counts before stopping

	// workloadListings memoises the namespace-wide workload listing per kind.
	//
	// It survives a scale, unlike the pod caches, because of what it reads: a
	// workload's name, its selector labels and its pod template's labels. A
	// scale changes a replica count and none of those. Both readers — the
	// resolver that decides which workload to scale and the location decision
	// — ask the same question of the same two kinds, so without this a run
	// locating two databases and resolving six app services pays a dozen
	// identical full-namespace listings.
	workloadListings map[string][]kubernetesWorkload

	// workloads memoises the workload each service resolved to. Stop,
	// scaleServices and Start each resolve the same service in turn, and one
	// resolution is up to ten kubectl calls; a scale changes a workload's
	// replica count and nothing about its kind or name, so unlike the pod
	// caches this survives one.
	workloads map[string]resolvedWorkload

	// serviceLocations memoises the internal-versus-external decision per
	// service.
	//
	// Every path that touches a database resolves through a databaseTarget, and
	// each of those asks the question again — so a run that captures two
	// databases and reads their metadata sweeps up to four label selectors plus
	// a workload query per operation, against an answer that cannot change
	// while the run lasts. A database does not move out of the deployment
	// mid-backup; where its pods are is a different question, and that is what
	// podCache holds and what Start and scaleServices clear.
	//
	// It is deliberately not cleared alongside podCache for that reason: a
	// restore scales the deployment down and back up, which changes which pods
	// exist and changes nothing about whether the database is one of them.
	serviceLocations map[string]EndpointLocation

	// namespacePodListings memoises the namespace-wide ownership listing, keyed
	// by the field selector it was issued with.
	//
	// It is not a second resolution cache: podCache answers "which pod is this
	// service's?", and this holds the pods kubectl listed for a question that
	// names no service at all — parsed once, with the transient workloads
	// already excluded, and read as-is by every hit. Two call sites ask it —
	// the fallback pod resolver and the location decision — once per service
	// each, and the answer cannot differ between them, because the query does
	// not mention the service.
	//
	// Both of those ask *which pods exist*, and that is the whole of what this
	// memo may answer. It must never answer *what those pods are doing*: a
	// phase is read precisely because it is expected to change, and a memo
	// there is not a saved call but a frozen answer. The running check
	// therefore goes through freshNamespacePodListing, which touches this map
	// neither to read nor to write — see the doc block there for what a
	// replayed phase did to the quiesce and restart waits.
	//
	// It is cleared wherever podCache is, and for the same reason: a scale
	// changes which pods exist. That is also its limit — the stop loop clears
	// it each time it actually stops something — so it saves the repeats within
	// one burst of resolution, not across a mutation.
	namespacePodListings map[string][]labelledPod

	// transientWorkloads are the transient external-database workloads this
	// backend created, tracked from the moment their pod exists so a run's exit
	// path removes every one of them with a single call however many it created
	// (FR-011). A run creates two per external database — a probe and then the
	// capture — and the probe is the one a failure between them would otherwise
	// leak.
	transientWorkloads []*TransientWorkload
}

func NewKubernetesBackend(config *Configuration, executor *CommandExecutor) *KubernetesBackend {
	return &KubernetesBackend{
		config:               config,
		executor:             executor,
		podCache:             map[string]string{},
		replicaCache:         map[string]int{},
		workloadListings:     map[string][]kubernetesWorkload{},
		workloads:            map[string]resolvedWorkload{},
		serviceLocations:     map[string]EndpointLocation{},
		namespacePodListings: map[string][]labelledPod{},
	}
}

// dropPodCaches forgets where the deployment's pods are.
//
// Both caches describe which pods exist — podCache the one resolved per
// service, namespacePodListings the listing that answer came out of — and a
// scale is exactly what changes that. They are cleared together so a caller
// cannot invalidate one and read the other. serviceLocations is deliberately
// not among them: a restore scales the deployment down and back up, which
// changes which pods exist and nothing about whether a database is one of them.
func (k *KubernetesBackend) dropPodCaches() {
	k.podCache = map[string]string{}
	k.namespacePodListings = map[string][]labelledPod{}
}

func (k *KubernetesBackend) Name() string {
	return "kubernetes"
}

func (k *KubernetesBackend) Info() string {
	return k.namespace
}

// RuntimeCommand names the tool this backend shells out to; see
// DockerBackend.RuntimeCommand.
func (*KubernetesBackend) RuntimeCommand() string { return "kubectl" }

func (k *KubernetesBackend) Detect() error {
	if err := k.executor.runCommandQuiet("kubectl", "version", "--client"); err != nil {
		// If user explicitly specified K8s namespace, this is a hard error
		if k.config.K8sNamespace != "" {
			return fmt.Errorf("kubectl CLI not available (required for --k8s-namespace): %w", err)
		}
		// Otherwise, treat as soft failure for auto-detection
		return fmt.Errorf("kubectl CLI not available: %w", ErrCLIUnavailable)
	}

	if k.config.K8sNamespace != "" {
		k.namespace = k.config.K8sNamespace
		if _, err := k.executor.runCommand("kubectl", "get", "pods", "-n", k.namespace, "-l", "app.kubernetes.io/name=infrahub"); err != nil {
			return fmt.Errorf("failed to verify namespace %s: %w", k.namespace, err)
		}
		return nil
	}

	namespaces, err := ListKubernetesNamespaces(k.executor)
	if err != nil {
		return err
	}

	switch len(namespaces) {
	case 0:
		return ErrEnvironmentNotFound
	case 1:
		k.namespace = namespaces[0]
		k.config.K8sNamespace = k.namespace
		return nil
	default:
		return fmt.Errorf("multiple kubernetes namespaces found: %s (set INFRAHUB_K8S_NAMESPACE)", strings.Join(namespaces, ", "))
	}
}

// targetedPod returns the pod the options name, or "" when they name none. See
// ExecOptions.Pod for why naming one exists at all.
func targetedPod(opts *ExecOptions) string {
	if opts == nil {
		return ""
	}

	return strings.TrimSpace(opts.Pod)
}

// execPod is the pod a command runs in: the one the options name, or the one
// the service resolves to. Naming a pod is resolution that already happened,
// not a second way of resolving — so it is read first and the resolver is not
// consulted at all.
func (k *KubernetesBackend) execPod(service string, opts *ExecOptions) (string, error) {
	if pod := targetedPod(opts); pod != "" {
		return pod, nil
	}

	return k.getPodForService(service)
}

// execPodContext is execPod under a bound, for the collect primitives and every
// external-database operation.
func (k *KubernetesBackend) execPodContext(ctx context.Context, timeout time.Duration, service string, opts *ExecOptions) (string, error) {
	if pod := targetedPod(opts); pod != "" {
		return pod, nil
	}

	return k.getPodForServiceContext(ctx, timeout, service)
}

// buildExecArgs resolves the pod and constructs kubectl exec arguments.
func (k *KubernetesBackend) buildExecArgs(service string, command []string, opts *ExecOptions) ([]string, error) {
	pod, err := k.execPod(service, opts)
	if err != nil {
		return nil, err
	}
	finalCmd := k.prepareCommand(command, opts)
	args := []string{"exec", "-n", k.namespace, pod, "--"}
	args = append(args, finalCmd...)
	return args, nil
}

func (k *KubernetesBackend) Exec(service string, command []string, opts *ExecOptions) (string, error) {
	args, err := k.buildExecArgs(service, command, opts)
	if err != nil {
		return "", err
	}
	return k.executor.runCommand("kubectl", args...)
}

func (k *KubernetesBackend) ExecStream(service string, command []string, opts *ExecOptions) (string, error) {
	args, err := k.buildExecArgs(service, command, opts)
	if err != nil {
		return "", err
	}
	return k.executor.runCommandWithStream("kubectl", args...)
}

func (k *KubernetesBackend) ExecStreamPipe(service string, command []string, opts *ExecOptions) (io.ReadCloser, func() error, error) {
	args, err := k.buildExecArgs(service, command, opts)
	if err != nil {
		return nil, nil, err
	}
	return k.executor.runCommandPipe("kubectl", args...)
}

func (k *KubernetesBackend) ExecWritePipe(service string, command []string, opts *ExecOptions, stdin io.Reader) (func() error, error) {
	pod, err := k.execPod(service, opts)
	if err != nil {
		return nil, err
	}
	finalCmd := k.prepareCommand(command, opts)
	args := []string{"exec", "-i", "-n", k.namespace, pod, "--"}
	args = append(args, finalCmd...)
	return k.executor.runCommandWritePipe(stdin, "kubectl", args...)
}

func (k *KubernetesBackend) CopyTo(service, src, dest string) error {
	pod, err := k.getPodForService(service)
	if err != nil {
		return err
	}

	return k.CopyToPod(pod, src, dest)
}

func (k *KubernetesBackend) CopyFrom(service, src, dest string) error {
	pod, err := k.getPodForService(service)
	if err != nil {
		return err
	}

	return k.CopyFromPod(pod, src, dest)
}

// CopyToPod and CopyFromPod are CopyTo and CopyFrom against a pod the caller
// already knows the name of. They are what ExecOptions.Pod is for the exec
// primitives: a copy is the one operation that takes no options, so the
// alternative to naming the pod here would be registering it under the
// service's name — which is the registration ExecOptions.Pod exists to
// replace, and which a restore's own StartServices call silently undid.
//
// CopyTo and CopyFrom resolve a pod and then call these, so there is one place
// a `kubectl cp` argument is built either way.
func (k *KubernetesBackend) CopyToPod(pod, src, dest string) error {
	if _, err := k.executor.runCommand("kubectl", "cp", src, k.podPath(pod, dest)); err != nil {
		return err
	}

	return nil
}

// podPath is the `<namespace>/<pod>:<path>` argument every `kubectl cp` takes,
// built in one place so the four copy primitives cannot disagree about it.
func (k *KubernetesBackend) podPath(pod, path string) string {
	return fmt.Sprintf("%s/%s:%s", k.namespace, pod, path)
}

func (k *KubernetesBackend) CopyFromPod(pod, src, dest string) error {
	if _, err := k.executor.runCommand("kubectl", "cp", k.podPath(pod, src), dest); err != nil {
		return err
	}

	return nil
}

func (k *KubernetesBackend) Start(services ...string) error {
	for _, service := range services {
		kind, resource, err := k.findWorkloadResource(service)
		if err != nil {
			return fmt.Errorf("failed to resolve workload for %s: %w", service, err)
		}
		cacheKey := fmt.Sprintf("%s/%s", kind, resource)
		replicas := 1 // default
		if savedCount, ok := k.replicaCache[cacheKey]; ok && savedCount > 0 {
			replicas = savedCount
			logrus.Debugf("Restoring replica count for %s: %d", cacheKey, replicas)
		}
		if err := k.scaleResource(kind, resource, replicas); err != nil {
			return fmt.Errorf("failed to scale %s (%s/%s) to %d replicas: %w", service, kind, resource, replicas, err)
		}
	}
	k.dropPodCaches()
	return nil
}

func (k *KubernetesBackend) Stop(services ...string) error {
	// Save current replica counts before stopping
	for _, service := range services {
		kind, resource, err := k.findWorkloadResource(service)
		if err != nil {
			continue
		}
		if count, err := k.getReplicaCount(kind, resource); err == nil && count > 0 {
			cacheKey := fmt.Sprintf("%s/%s", kind, resource)
			k.replicaCache[cacheKey] = count
			logrus.Debugf("Saved replica count for %s: %d", cacheKey, count)
		}
	}
	return k.scaleServices(services, 0)
}

func (k *KubernetesBackend) IsRunning(service string) (bool, error) {
	statuses, err := k.getPodStatuses(service)
	if err != nil {
		return false, err
	}
	for _, status := range statuses {
		if strings.EqualFold(status, "Running") {
			return true, nil
		}
	}
	return false, nil
}

// getReplicaCount returns the current replica count for a workload
func (k *KubernetesBackend) getReplicaCount(kind, resource string) (int, error) {
	output, err := k.stdoutRunner()("kubectl", "get", kind, resource, "-n", k.namespace, "-o", "jsonpath={.spec.replicas}")
	if err != nil {
		return 0, err
	}
	count, err := strconv.Atoi(strings.TrimSpace(output))
	if err != nil {
		return 0, err
	}
	return count, nil
}

func (k *KubernetesBackend) getPodStatuses(service string) ([]string, error) {
	return k.getPodStatusesWith(k.stdoutRunner(), service)
}

// getPodStatusesWith is getPodStatuses against the supplied runner, following
// the package's injection idiom so the running check is testable without a
// cluster.
//
// IsRunning has one caller that acts destructively on the answer —
// stopAppContainers, deciding whether to stop and scale a workload to zero — so
// this is a destructive decision dressed as a query. The quiesce and restart
// waits read it too, for the transition rather than the decision (see
// freshNamespacePodListing), so its contract belongs to both: narrowing or
// widening what counts as running changes which workloads get stopped *and*
// what those two loops wait for. One namespace listing answers it, and the
// three answers it can give are drawn apart accordingly:
//
//   - the pods a label selector would have matched: they declared themselves,
//     and their phases are the answer (podsServiceSelectorsWouldMatch).
//   - nothing a selector would have matched: the listing decides on the
//     ownership evidence it carries. `redis-cache`, `othertool-cache-0` and
//     `rabbitmq-message-queue` are not this deployment's, so their phases are
//     not this service's status — the shipped `strings.Contains(name, service)`
//     reported all three as Infrahub's `cache`, and stopAppContainers then
//     stopped them.
//   - the listing failed: the question was not answered. An error, never an
//     empty answer, because a caller that cannot determine whether a workload
//     is running must not stop it (FR-002).
//
// The four `<key>=<service>` queries this used to issue are gone, and with them
// the only failure mode T097 was written about. They asked the cluster for the
// very fields the listing already carries, so they are now reproduced from it —
// exactly, in selector order, on the same evidence a selector matches
// (podsServiceSelectorsWouldMatch). What T097 fixed was a *throttled selector
// call* turning a determinable status into an error, on the one topology that
// matters: podSelectors queries `<key>=infrahub-server`, the chart labels those
// pods `infrahub/service=server`, so on a Helm deployment the listing was
// already the only thing that ever matched `infrahub-server`. There is no
// selector call left to fail, which is the strongest form that property takes
// — and correspondingly the one query's failure is now the whole of the
// undetermined status, reported as such rather than as an empty answer.
//
// Reproducing the selector matches, rather than reading the whole listing
// through the ownership rule, is what keeps this from becoming a *broader*
// running check than the one it replaces: ownership also claims a pod carrying
// a name this deployment's release prefixes claim, and IsRunning answering true
// from one of those is a stop and a scale to zero on evidence no selector ever
// produced.
//
// A listing that names no matching pod is a real "not running", which is what
// an absent optional service looks like, so it stays an empty answer.
//
// The query is fresh every time. IsRunning is what the quiesce and restart
// waits poll in a sleep loop with nothing mutating between iterations, so an
// answer replayed from a memo would make the transition they are waiting for
// unobservable however long they waited (see freshNamespacePodListing).
func (k *KubernetesBackend) getPodStatusesWith(run podRunner, service string) ([]string, error) {
	// Read fresh every time, because a phase is what the callers above are
	// waiting to see change (see freshNamespacePodListing).
	pods, err := k.freshNamespacePodListing(run)
	if err != nil {
		return nil, fmt.Errorf("failed to determine whether %s is running in namespace %s: %w", service, k.namespace, err)
	}

	if declared := podsServiceSelectorsWouldMatch(pods, k.podSelectors(service)); len(declared) > 0 {
		return podPhases(declared), nil
	}

	prefixes := releasePrefixes(declaredPodServices(pods))
	statuses := []string{}
	for _, pod := range pods {
		// The same rule deploymentClaimsService decides the location on, under
		// the same policy: this is only ever asked about the app services (see
		// appServicesStoppedForBackup), for which an unanchored prefixed name
		// still claims, because the cost of narrowing is a workload left
		// running through an offline capture.
		if deploymentClaimsResource(prefixes, pod.Name, pod.Service, service) {
			statuses = append(statuses, pod.Phase)

			continue
		}
		logUnclaimedResource("pod", pod.Name, pod.Service, service, prefixes)
	}

	return statuses, nil
}

// namespacePodListing answers "which pods exist?" from the namespace-wide
// ownership listing, replaying the answer for a field selector it has already
// been asked about.
//
// Two call sites ask it — the fallback pod resolver (claimedPodNamesWith) and
// the location decision (deploymentClaimsService) — identically, once per
// service they were asked about. The query names no service, so its answer
// cannot differ between them, and both questions are settled for as long as the
// pods are: which pod is a service's, and whether the deployment holds one at
// all. That is what makes the memo sound, and it is the only kind of question
// it may be asked. The running check is not one of them and must not become
// one — see freshNamespacePodListing.
//
// The memo is per backend and dropped by every scale, so a caller that is about
// to change which pods exist never reads an answer from before it did. A listing
// that fails is not memoised: it says the cluster could not answer, not that the
// namespace is empty (FR-002).
func (k *KubernetesBackend) namespacePodListing(run podRunner, fieldSelector string) ([]labelledPod, error) {
	if pods, cached := k.namespacePodListings[fieldSelector]; cached {
		return pods, nil
	}

	listed, err := k.issueNamespacePodListing(run, fieldSelector)
	if err != nil {
		return nil, err
	}
	pods := parseLabelledPods(listed)
	k.namespacePodListings[fieldSelector] = pods

	return pods, nil
}

// freshNamespacePodListing answers "what are those pods doing?" — the same
// query as namespacePodListing, never memoised.
//
// It is a separate function rather than a parameter because the difference is
// in the question, not in the caller. A phase is read because it is expected to
// change, and confirmAppContainersQuiescedWithin and
// reportAppContainersRunningWithin poll IsRunning in a sleep loop with nothing
// mutating between iterations — they are waiting for the answer to move.
// Served from the memo, the first poll's bytes were replayed until the window
// ran out, so the transition those loops exist to detect could not be observed:
// a restore whose pods terminated normally aborted after the destructive step,
// blaming "a pod that will not terminate", and a restore that in fact worked
// reported every service as having failed to come back.
//
// Invalidating the memo from those loops would be the wrong fix twice over: it
// is one caller deciding another's freshness, and it would also discard the
// resolution answers the run still needs. Nothing here reads or writes
// namespacePodListings, so the next poll loop someone writes cannot reintroduce
// the bug by forgetting to.
//
// It takes no field selector because the running check narrows by none: a
// terminal pod is part of the answer to what the service is doing.
func (k *KubernetesBackend) freshNamespacePodListing(run podRunner) ([]labelledPod, error) {
	listed, err := k.issueNamespacePodListing(run, "")
	if err != nil {
		return nil, err
	}

	return parseLabelledPods(listed), nil
}

// issueNamespacePodListing issues the namespace-wide ownership listing.
//
// It is the one place the query is built and the one place its failure is
// worded, which is what keeps the memoised and the fresh reading of it the same
// listing — and the reason neither of them can memoise an error: a failure says
// the cluster could not answer, not that the namespace is empty (FR-002).
func (k *KubernetesBackend) issueNamespacePodListing(run podRunner, fieldSelector string) (string, error) {
	listed, err := run("kubectl", k.podListingArgsFor(podOwnershipJSONPath, fieldSelector, "")...)
	if err != nil {
		return "", fmt.Errorf("failed to list pods in namespace %s: %w", k.namespace, err)
	}

	return listed, nil
}

// claimedPodNamesWith lists the namespace's pods with the ownership evidence
// their labels carry, and returns — in listing order — the ones this deployment
// claims for the service.
//
// It is the fallback both pod resolvers use, and it exists because they used to
// answer "whose pod is this?" with a bare name match while the location
// decision answered the same question through kubernetes_ownership.go. So a
// co-hosted `othertool-task-manager-…` carrying no matching label was refused
// by the location rule and accepted here — and the pods these resolvers exec
// `env` in are where ensureNeo4jDiscovery and ensurePostgresDiscovery read the
// endpoint a restore later writes into. Resolution was the permissive one of
// the two, which is the wrong way round.
//
// What an unanchored prefixed name means here is unanchoredNamePolicyFor's
// answer, not this function's, and that is the second half of the same fix. It
// used to be the constant unanchoredNameClaims whatever the service was, so a
// namespace that declared nothing resolved `postgres-database-0` as the
// deployment's `database` while the location decision refused the very same pod
// — and the pod this resolver returns is where ensureNeo4jDiscovery and
// ensurePostgresDiscovery read the endpoint a restore later writes into. For the
// app services the permissive reading stays, and it has to: a namespace whose
// pods carry none of the four service labels — a kustomize deployment, a
// hand-rolled manifest, an older chart — would otherwise answer "no pods" for
// every service, taking a deployment that works today off the path it works on.
//
// terminal says which pods are out of scope and is the client-side half of the
// field selector passed beside it: resolution excludes both terminal phases
// because it is looking for somewhere to exec, enumeration excludes only
// Succeeded because a crashed replica is much of what a bundle is collected for.
func (k *KubernetesBackend) claimedPodNamesWith(run podRunner, service, fieldSelector string, terminal func(string) bool) ([]string, error) {
	listed, err := k.namespacePodListing(run, fieldSelector)
	if err != nil {
		return nil, err
	}

	pods := filterLabelledPods(listed, terminal)
	prefixes := releasePrefixes(declaredPodServices(pods))

	// Declarations first, then claimed names: a pod that says what it is
	// outranks one that merely carries a name this deployment claims, whatever
	// order the listing happened to arrive in. The singular resolver takes the
	// first of these, so the order is the answer.
	names := []string{}
	for _, pod := range pods {
		if declaresService(pod.Service, service) {
			names = append(names, pod.Name)
		}
	}
	for _, pod := range pods {
		if declaresService(pod.Service, service) {
			continue
		}
		if deploymentClaimsResource(prefixes, pod.Name, pod.Service, service) {
			names = append(names, pod.Name)

			continue
		}
		logUnclaimedResource("pod", pod.Name, pod.Service, service, prefixes)
	}

	return names, nil
}

// getPodForService resolves a single pod for a service using the unbounded
// executor. It is the shared helper used by the backup tool. Like its plural
// sibling GetAllPods it returns errNoPodsMatched only when kubectl calls
// succeeded but nothing matched, and the wrapped kubectl error when the
// cluster/API itself is unreachable (FIX-2).
func (k *KubernetesBackend) getPodForService(service string) (string, error) {
	return k.getPodForServiceWith(k.stdoutRunner(), service)
}

// getPodForServiceContext is the timeout-bounded pod resolver the collect
// primitives use so a hung API server cannot stall the run before the exec/copy
// timeout applies (research R2, FIX-5). It shares getPodForServiceWith with the
// unbounded getPodForService, differing only in the runner.
func (k *KubernetesBackend) getPodForServiceContext(ctx context.Context, timeout time.Duration, service string) (string, error) {
	return k.getPodForServiceWith(k.boundedRunner(ctx, timeout), service)
}

// getPodForServiceWith resolves a single pod for a service via the supplied
// runner, caching the result. HA clusters resolve to the primary pod.
//
// Only pods something can still run in are candidates: this resolves an exec
// target, and a completed or failed pod is not one — a finished backup-CronJob
// pod named `<release>-database-backup-28…` is not a database (see
// livePodFieldSelector).
//
// A successful-but-empty match returns errNoPodsMatched; a kubectl failure
// returns the wrapped kubectl error, exactly as getAllPodsWith draws the same
// distinction (FIX-2). "Successful" means the question was actually answered:
// if every label selector query failed, the empty match is not a match at all
// and a real error is returned even when the fallback listing itself succeeded
// (FR-002).
func (k *KubernetesBackend) getPodForServiceWith(run podRunner, service string) (string, error) {
	if pod, ok := k.podCache[service]; ok && pod != "" {
		return pod, nil
	}

	// A selector that matched several pods is read for the primary (HA
	// clusters like CloudNativePG); the ownership fallback takes the first name
	// it claimed, as it always has.
	pickLive := func(pods []labelledPod) []string {
		names := livePodNames(pods)
		if len(names) > 1 {
			if primary := k.findPrimaryPod(run, names); primary != "" {
				return []string{primary}
			}
		}

		return names
	}

	pods, err := k.podsForServiceWith(run, service, livePodFieldSelector, pickLive, podPhaseIsTerminal)
	if err != nil {
		return "", err
	}

	k.podCache[service] = pods[0]

	return pods[0], nil
}

// podsForServiceWith is the resolution the singular and plural pod resolvers
// share: the label-selector tier, then the ownership fallback, then the FR-002
// discrimination between "nothing matched" and "nothing was answered". It is
// one function so that discrimination is spelled once.
//
// fieldSelector is the phase filter the caller resolves under, pick reduces a
// selector's listing to the names the caller accepts (and is where the singular
// resolver chooses a primary), and terminal is the phase test the fallback
// drops pods on. A non-empty answer is returned as soon as a tier gives one.
//
// The fallback is the namespace read for what each pod declares itself to be
// as well as what it is called. The Helm chart labels some pods with a
// different service value than the canonical name (e.g. infrahub-server pods
// carry infrahub/service=server), so label selectors alone would miss them. A
// failure there means the cluster/API is unreachable, not that the service is
// absent, so it is wrapped rather than reported as an empty match.
func (k *KubernetesBackend) podsForServiceWith(run podRunner, service, fieldSelector string, pick func([]labelledPod) []string, terminal func(string) bool) ([]string, error) {
	selectors := k.podSelectors(service)
	selectorErrs := []error{}
	for _, selector := range selectors {
		output, err := run("kubectl", k.podListingArgs(fieldSelector, selector)...)
		if err != nil {
			selectorErrs = append(selectorErrs, fmt.Errorf("selector %s: %w", selector, err))

			continue
		}
		if pods := pick(parseLabelledPods(output)); len(pods) > 0 {
			return pods, nil
		}
	}

	names, err := k.claimedPodNamesWith(run, service, fieldSelector, terminal)
	if err != nil {
		return nil, err
	}
	if len(names) > 0 {
		return names, nil
	}

	// Every label selector query failed, so nothing here established that the
	// service has no pod: the sentinel would tell the caller "not deployed" on
	// the strength of queries that were never answered (FR-002).
	if len(selectors) > 0 && len(selectorErrs) == len(selectors) {
		return nil, fmt.Errorf("failed to list pods for service %s in namespace %s: every label selector query failed: %w", service, k.namespace, errors.Join(selectorErrs...))
	}

	return nil, fmt.Errorf("no pods found for service %s in namespace %s: %w", service, k.namespace, errNoPodsMatched)
}

// errLocationUndetermined is the FR-002 refusal, as a value callers and tests
// can identify rather than a message they have to match: the cluster did not
// answer where the database lives, which is not the same fact as the database
// being absent.
var errLocationUndetermined = errors.New("the deployment could not be queried for it, so where it lives was never established")

// undeterminedLocation is what an operator reads when the cluster could not
// answer where a database lives.
//
// The failure-message contract requires this one to name the query that failed
// and the permission that would be missing, and — explicitly — *not* to say the
// database is absent. Those two readings have opposite remedies: an operator
// told "no database container here" configures an external database they do not
// have, while what they actually need is a Role or a corrected namespace. That
// conflation is the defect this feature exists to remove, so the refusal states
// which of the two it is rather than leaving the wording to imply either.
//
// The permission is named conditionally because a denial is only one of the
// causes: a throttled or unreachable API server produces the same undetermined
// answer, and asserting an RBAC cause for it would send the operator to fix
// something that is not broken.
func (k *KubernetesBackend) undeterminedLocation(service string, cause error) error {
	return fmt.Errorf(
		"failed to determine whether %s runs in namespace %s: %w: %w. "+
			"This is the query failing, not evidence that the database is absent — the run stops here rather than treating %s as a database outside the deployment (FR-002). "+
			"If the query was refused, the identity this tool runs as needs get and list on pods in namespace %s; "+
			"otherwise check that --k8s-namespace names the right namespace and that the API server is reachable",
		service, k.namespace, errLocationUndetermined, cause, service, k.namespace)
}

// locateService answers whether a service runs inside the deployment, which is
// the whole of the internal-versus-external decision (FR-001, FR-002). External
// is returned only when every label selector answered and none matched; every
// other outcome — a wrong namespace, RBAC that hides pods, an unreachable API
// server — is an error, so no failure to query the cluster can be reinterpreted
// as "the database must be somewhere else" and send the run looking for it on
// the network.
//
// The query is time-bounded (FR-025). This is the first step of every external
// path, so a cluster that stops answering here would hang the run before any
// endpoint had even been resolved.
func (k *KubernetesBackend) locateService(service string) (EndpointLocation, error) {
	if location, ok := k.serviceLocations[service]; ok {
		return location, nil
	}

	bound := externalDBControlBound(k.config)

	location, err := k.locateServiceWith(k.externalDBRunner(bound, externalDBTarget("the "+service+" service", k.namespace)), service)
	if err != nil {
		// A failure is not memoised. It says the cluster could not answer, not
		// where the database is, and remembering it would turn one bad moment
		// into a decision the rest of the run cannot revisit — while the
		// caller's own retry is what this would be taking away.
		return "", err
	}

	k.serviceLocations[service] = location

	return location, nil
}

// locateServiceWith is locateService against the supplied runner, following the
// package's existing injection idiom so the decision is testable without a
// cluster.
//
// The evidence is deliberately asymmetric, because the two wrong answers cost
// different things:
//
//   - A wrong "internal" routes a database that lives outside the deployment
//     onto the in-deployment path, and runs `neo4j-admin` or `pg_dump` — and on
//     the restore side a load — inside whatever resource matched.
//   - A wrong "external" takes a deployment that works today off the path it
//     works on. That is not a harmless conservatism even now that external
//     capture exists: the run builds a transient workload, probes over Bolt for
//     an address the deployment describes for its own in-cluster service, and
//     fails somewhere along the way — and on the restore side it refuses
//     outright, so an operator whose backups worked yesterday cannot restore.
//
// So evidence of presence is read from every source the deployment offers, and
// only a namespace in which *nothing* claims the service reads as external.
// Absence of a label is not evidence that the database is absent — three
// unrelated situations made it look like one:
//
//   - Label evidence alone. A pod labelled by kustomize, by a hand-rolled
//     manifest or by a different chart version matches none of the four
//     selectors, and an externally-managed CloudNativePG cluster — pods
//     `<cluster>-1`, labelled `cnpg.io/cluster` — matches none of them either,
//     while findPrimaryPod exists precisely to support that topology.
//   - Live pods only. livePodFieldSelector excludes Failed, whose justification
//     is a finished backup-CronJob pod — a reason to exclude Succeeded, not
//     Failed, from a question about where a database lives. A pod evicted
//     mid-recreate, or one on a drained node, is not evidence of absence, and a
//     StatefulSet scaled to zero has no pods to read at all.
//   - No floor on the control bound, which made every query here expire; see
//     resolveExternalDBTimeout.
//
// The order is: a label selector query, then the namespace's own resources read
// for what they declare and for whether this deployment claims them (see
// kubernetes_ownership.go, which answers "does this deployment claim it?"
// rather than "does this name resemble the service?"). A name carrying a
// release prefix counts only against a prefix some resource *declared* — which
// is what keeps `postgres-database-0` from an unrelated StatefulSet from
// putting a genuinely external database on the in-deployment path — while a
// name that is the service's own with nothing before it counts on its own,
// which is what the CloudNativePG shape above needs and what this used to
// refuse. Refusing it made this decision strictly narrower than the pod
// resolver's own fallback: the resolver claimed `task-manager-db-1` and this
// called the same database external, so a restore the resolver could have run
// was refused outright.
//
// An unanswered selector query is never read as absence: if no selector matched
// and any of them failed, the location was not established and the caller gets
// an error (FR-002).
func (k *KubernetesBackend) locateServiceWith(run podRunner, service string) (EndpointLocation, error) {
	selectors := k.podSelectors(service)
	selectorErrs := []error{}
	for _, selector := range selectors {
		// completedPodFieldSelector, not livePodFieldSelector: this asks where
		// the database lives, not which pod to exec in, and a pod that failed
		// is still where it lives.
		output, err := run("kubectl", k.podListingArgs(completedPodFieldSelector, selector)...)
		if err != nil {
			selectorErrs = append(selectorErrs, fmt.Errorf("selector %s: %w", selector, err))

			continue
		}
		candidates := parseLabelledPods(output)
		if pods := enumerablePodNames(candidates); len(pods) > 0 {
			logrus.Debugf("Service %s runs in namespace %s: pod %s matches %s", service, k.namespace, pods[0], selector)
			k.seedPodCache(service, candidates)

			return EndpointLocationInternal, nil
		}
	}

	if len(selectorErrs) > 0 {
		return "", k.undeterminedLocation(service, errors.Join(selectorErrs...))
	}

	claimed, err := k.deploymentClaimsService(run, service)
	if err != nil {
		return "", k.undeterminedLocation(service, err)
	}
	if claimed {
		return EndpointLocationInternal, nil
	}

	return EndpointLocationExternal, nil
}

// seedPodCache hands the resolver what the location decision already read, so
// the same `kubectl get pods` is not issued twice within a second of itself.
//
// It seeds only when exactly one live pod matched, and that restraint is the
// whole of its safety:
//
//   - The location decision lists with completedPodFieldSelector, because it
//     asks where a database lives and a pod that failed is still where it
//     lives. The resolver lists with livePodFieldSelector, because it resolves
//     an exec target. Seeding the pod this decision found would put a
//     terminated pod in front of every exec that followed, so only the live
//     subset is eligible.
//   - Above one live pod the resolver does something this cannot: it asks which
//     is the primary, which is how an HA CloudNativePG cluster resolves to the
//     member that can be read. Seeding there would quietly replace that choice
//     with whichever pod the listing happened to put first.
//
// So the case it covers is the single-replica database — the one this feature
// meets on every deployment it runs against, and the one paying the duplicated
// listing (FR-015).
func (k *KubernetesBackend) seedPodCache(service string, candidates []labelledPod) {
	if _, cached := k.podCache[service]; cached {
		return
	}

	if live := livePodNames(candidates); len(live) == 1 {
		k.podCache[service] = live[0]
	}
}

// deploymentClaimsService is the evidence the label selectors cannot carry:
// whether anything in the namespace declares itself as the service, or carries
// a name this deployment claims.
//
// Workloads are read as well as pods, and that is what answers the shapes with
// no pod to look at — a StatefulSet scaled to zero, a pod evicted between two
// listings. A workload listing that fails adds no evidence and is not an error:
// without it the decision is exactly the one the pods already support, so a
// namespace whose Deployments cannot be listed is answered as before rather
// than failed. The pod listing is different — it is the answer, not an addition
// to it — so a failure there is reported (FR-002).
func (k *KubernetesBackend) deploymentClaimsService(run podRunner, service string) (bool, error) {
	listed, err := k.namespacePodListing(run, completedPodFieldSelector)
	if err != nil {
		return false, err
	}

	pods := runnablePods(listed)
	for _, pod := range pods {
		if declaresService(pod.Service, service) {
			logrus.Debugf("Service %s runs in namespace %s: pod %s declares it (%s)", service, k.namespace, pod.Name, pod.Service)

			return true, nil
		}
	}

	workloads := k.listAllWorkloadsWith(run)
	for _, workload := range workloads {
		if declaresService(declaredServiceIn(workload.SelectorLabels, workload.TemplateLabels), service) {
			logrus.Debugf("Service %s runs in namespace %s: workload %s declares it", service, k.namespace, workload.Name)

			return true, nil
		}
	}

	// The release the deployment named itself with, read from whatever declared
	// itself — a pod, or a workload whose pods are gone. Without one there is
	// nothing to anchor a *prefixed* name to, and a bare resemblance is
	// evidence about a name: `postgres-database-0` from an unrelated StatefulSet
	// reads exactly like the deployment's own database. Nothing at all is what
	// external is — which is the same rule, and now the same policy, that
	// getPodStatusesWith and the pod resolver decide on (see
	// unanchoredNamePolicyFor).
	prefixes := slices.Concat(
		releasePrefixes(declaredPodServices(pods)),
		releasePrefixes(declaredWorkloadServices(workloads)),
	)

	for _, pod := range pods {
		if deploymentClaimsResource(prefixes, pod.Name, pod.Service, service) {
			logrus.Debugf("Service %s runs in namespace %s: pod %s carries a name this deployment claims (release prefixes %v)", service, k.namespace, pod.Name, prefixes)

			return true, nil
		}
	}
	for _, workload := range workloads {
		declared := declaredServiceIn(workload.SelectorLabels, workload.TemplateLabels)
		if deploymentClaimsResource(prefixes, workload.Name, declared, service) {
			logrus.Debugf("Service %s runs in namespace %s: workload %s carries a name this deployment claims (release prefixes %v)", service, k.namespace, workload.Name, prefixes)

			return true, nil
		}
	}

	return false, nil
}

// listAllWorkloadsWith lists every Deployment and StatefulSet in the namespace,
// skipping a kind it could not read. See deploymentClaimsService for why a
// failure here is skipped rather than reported.
func (k *KubernetesBackend) listAllWorkloadsWith(run podRunner) []kubernetesWorkload {
	workloads := []kubernetesWorkload{}
	for _, kind := range workloadKinds {
		listed, err := k.listWorkloadsWith(run, kind)
		if err != nil {
			logrus.Debugf("Could not list %s in namespace %s while locating a service: %v", kind, k.namespace, err)

			continue
		}
		workloads = append(workloads, listed...)
	}

	return workloads
}

// podListingArgs builds a pod listing that reports each pod's phase, with the
// supplied field selector and label selector applied.
func (k *KubernetesBackend) podListingArgs(fieldSelector, labelSelector string) []string {
	return k.podListingArgsFor(podNamePhaseJSONPath, fieldSelector, labelSelector)
}

// podListingArgsFor is podListingArgs for a listing that asks for something
// other than name-and-phase — the ownership listing, which carries each pod's
// declared service alongside them.
//
// Every pod listing this package issues about a service is built here, and
// every one of them excludes the transient external-database workload (FR-028).
// That is what the exclusion being a *choke point* means: a listing cannot be
// added that forgets it, because the arguments cannot be built without it.
// Before this it was applied at one enumerator of four — getAllPodsWith — while
// the singular resolver, the running check and the location decision each
// listed the namespace with no exclusion at all.
//
// The label selector is a string rather than raw arguments for the same reason:
// the exclusion has to be joined to whatever the caller selects on, and a
// caller that passed `-l` itself could pass it twice.
func (k *KubernetesBackend) podListingArgsFor(output, fieldSelector, labelSelector string) []string {
	args := []string{"get", "pods", "-n", k.namespace, "-l", excludeTransientWorkloads(labelSelector)}
	if fieldSelector != "" {
		args = append(args, "--field-selector="+fieldSelector)
	}

	return append(args, "-o", output)
}

// GetAllPods returns all pod names for a given service using the unbounded
// executor (shared helper used by the backup tool). It returns errNoPodsMatched
// only when kubectl calls succeeded but nothing matched, and the wrapped
// kubectl error when the cluster/API itself is unreachable (FIX-2).
func (k *KubernetesBackend) GetAllPods(service string) ([]string, error) {
	return k.getAllPodsWith(k.stdoutRunner(), service)
}

// getAllPodsContext is the timeout-bounded pod enumerator the collect primitives
// use (research R2, FIX-5).
func (k *KubernetesBackend) getAllPodsContext(ctx context.Context, timeout time.Duration, service string) ([]string, error) {
	return k.getAllPodsWith(k.boundedRunner(ctx, timeout), service)
}

// getAllPodsWith enumerates every pod backing a service via the supplied
// runner. A successful-but-empty match returns errNoPodsMatched; a failed
// kubectl call returns the wrapped kubectl error so cluster failures are never
// mistaken for an undeployed service (FIX-2) — including the case where every
// label selector query failed and only the fallback listing answered, which
// establishes nothing about the service.
//
// Enumeration keeps failed pods and drops only pods that ran to completion:
// this is what per-replica log and diagnostic collection walks, and a crashed
// replica is much of what a bundle is collected for (see
// completedPodFieldSelector).
//
// Like every other pod listing in this package, the listings here exclude the
// transient external-database workload, which is what keeps it from being
// published as a replica of the service it stands in for (FR-028). The
// choke point for that exclusion is podListingArgsFor, where the arguments are
// built — not this function, which used to apply it while the singular
// resolver, the running check and the location decision applied none.
//
// The exclusion is stated twice on purpose. The label selector is the mechanism
// that holds whatever the pod is called; the name filter is what still holds if
// a future listing is issued without going through podListingArgsFor. Neither
// is redundant, and neither on its own is the whole guarantee: the transient
// pod's name also deliberately contains no service name as a substring (see
// transientObjectPrefix), so today the fallback below would not match it even
// unfiltered — but that is a naming convention, not an invariant, and FR-028
// must not rest on it.
func (k *KubernetesBackend) getAllPodsWith(run podRunner, service string) ([]string, error) {
	return k.podsForServiceWith(run, service, completedPodFieldSelector, enumerablePodNames, podPhaseIsCompleted)
}

// KubernetesBackend implements the collect-side primitives (spec
// 003-collect-tool, research R3/R4) and the optional timeout-bounded and
// per-replica exec capabilities the diagnostics collectors prefer.
var (
	_ collectBackend        = (*KubernetesBackend)(nil)
	_ contextExecer         = (*KubernetesBackend)(nil)
	_ replicaExecer         = (*KubernetesBackend)(nil)
	_ separateExecer        = (*KubernetesBackend)(nil)
	_ separateReplicaExecer = (*KubernetesBackend)(nil)
	_ contextCopier         = (*KubernetesBackend)(nil)
	_ deploymentDescriber   = (*KubernetesBackend)(nil)
)

// buildExecArgsContext resolves the pod under a bounded runner and constructs
// kubectl exec arguments, so the pod-resolution kubectl call is bounded too
// (FIX-5) rather than only the exec that follows.
func (k *KubernetesBackend) buildExecArgsContext(ctx context.Context, timeout time.Duration, service string, command []string, opts *ExecOptions) ([]string, error) {
	pod, err := k.execPodContext(ctx, timeout, service, opts)
	if err != nil {
		return nil, err
	}
	finalCmd := k.prepareCommand(command, opts)
	args := []string{"exec", "-n", k.namespace, pod, "--"}
	args = append(args, finalCmd...)
	return args, nil
}

// ExecContext is the timeout-bounded variant of Exec used by the bundle
// collectors (research R2: 60s per status dump). Both the pod resolution and
// the exec itself are bounded (FIX-5).
func (k *KubernetesBackend) ExecContext(ctx context.Context, timeout time.Duration, service string, command []string, opts *ExecOptions) (string, error) {
	args, err := k.buildExecArgsContext(ctx, timeout, service, command, opts)
	if err != nil {
		return "", err
	}
	return k.executor.runCommandContext(ctx, timeout, "kubectl", args...)
}

// ExecSeparateContext is ExecContext with the command's stdout and stderr kept
// apart, so a collector can write the payload (stdout) to the bundle without
// kubectl's own notices — most notably `Defaulted container "x" out of: …` on a
// multi-container pod — or the command's diagnostics being mixed into it.
func (k *KubernetesBackend) ExecSeparateContext(ctx context.Context, timeout time.Duration, service string, command []string, opts *ExecOptions) (string, string, error) {
	args, err := k.buildExecArgsContext(ctx, timeout, service, command, opts)
	if err != nil {
		return "", "", err
	}
	return k.executor.runCommandSeparateContext(ctx, timeout, "kubectl", args...)
}

// ExecReplicaSeparate is ExecReplica with stdout and stderr kept apart (see
// ExecSeparateContext).
func (k *KubernetesBackend) ExecReplicaSeparate(ctx context.Context, timeout time.Duration, replica Replica, command []string) (string, string, error) {
	args := []string{"exec", "-n", k.namespace, replica.Pod, "-c", replica.Container, "--"}
	args = append(args, command...)
	return k.executor.runCommandSeparateContext(ctx, timeout, "kubectl", args...)
}

// CopyFromContext is the timeout-bounded variant of CopyFrom used by the bundle
// collectors (research R2, FIX-1). Pod resolution is bounded by the exec
// timeout (a quick metadata lookup) while the copy itself gets the supplied
// transfer timeout. The shared CopyFrom is left untouched for the backup tool.
func (k *KubernetesBackend) CopyFromContext(ctx context.Context, timeout time.Duration, service, src, dest string) error {
	pod, err := k.getPodForServiceContext(ctx, collectExecTimeout, service)
	if err != nil {
		return err
	}

	return k.CopyFromPodContext(ctx, timeout, pod, src, dest)
}

// CopyFromPodContext is CopyFromContext against a pod the caller already knows
// the name of: CopyFromPod's bounded sibling, and the one an operation against
// an external database uses, where every kubectl call has to be bounded
// (FR-025).
func (k *KubernetesBackend) CopyFromPodContext(ctx context.Context, timeout time.Duration, pod, src, dest string) error {
	if _, err := k.executor.runCommandContext(ctx, timeout, "kubectl", "cp", k.podPath(pod, src), dest); err != nil {
		return err
	}

	return nil
}

// CopyToPodContext is CopyToPod's bounded sibling, and the direction the
// restore path needs: the PostgreSQL dump has to reach the transient workload
// before the client in it can read it back into an external server.
//
// It is bounded for the reason the copy *out* is, only more so. This transfer
// happens with Infrahub scaled to zero, so an unbounded `kubectl cp` onto a
// wedged API server leaves the deployment quiesced with nothing reporting why
// (FR-025).
func (k *KubernetesBackend) CopyToPodContext(ctx context.Context, timeout time.Duration, pod, src, dest string) error {
	if _, err := k.executor.runCommandContext(ctx, timeout, "kubectl", "cp", src, k.podPath(pod, dest)); err != nil {
		return err
	}

	return nil
}

// ExecReplica executes a command in one specific replica (pod container),
// unlike Exec which resolves a single pod per service. The bundle collectors
// use it to capture per-replica task-worker state.
func (k *KubernetesBackend) ExecReplica(ctx context.Context, timeout time.Duration, replica Replica, command []string) (string, error) {
	args := []string{"exec", "-n", k.namespace, replica.Pod, "-c", replica.Container, "--"}
	args = append(args, command...)
	return k.executor.runCommandContext(ctx, timeout, "kubectl", args...)
}

// kubectlContainerStatusArgs builds the kubectl arguments that list one
// "<container> <restartCount>" pair per line for a pod's containers.
func kubectlContainerStatusArgs(namespace, pod string) []string {
	return []string{
		"get", "pod", pod, "-n", namespace,
		"-o", "jsonpath={range .status.containerStatuses[*]}{.name}{\" \"}{.restartCount}{\"\\n\"}{end}",
	}
}

// parsePodContainerStatuses converts kubectlContainerStatusArgs output into
// one Replica per pod container, with Restarted derived from that container's
// restartCount, because restart counts live per container.
func parsePodContainerStatuses(service, pod, output string) ([]Replica, error) {
	replicas := []Replica{}
	for _, line := range nonEmptyLines(output) {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("unexpected container status line %q for pod %s", line, pod)
		}
		restarts, err := strconv.Atoi(fields[1])
		if err != nil {
			return nil, fmt.Errorf("invalid restart count %q for container %s/%s: %w", fields[1], pod, fields[0], err)
		}
		replicas = append(replicas, Replica{
			Service:   service,
			Pod:       pod,
			Container: fields[0],
			Restarted: restarts > 0,
		})
	}
	return replicas, nil
}

// ServiceReplicas enumerates one Replica per container of every pod backing a
// service, sorted by pod then container. A service with no matching pods is a
// valid empty enumeration (nil, nil) so callers can record it as skipped
// rather than failed.
func (k *KubernetesBackend) ServiceReplicas(service string) ([]Replica, error) {
	pods, err := k.getAllPodsContext(context.Background(), collectExecTimeout, service)
	if err != nil {
		if errors.Is(err, errNoPodsMatched) {
			// kubectl succeeded but nothing matched: the service is genuinely
			// not deployed in this namespace, so callers record it as skipped.
			return nil, nil
		}
		// A real kubectl/cluster failure (API/RBAC/unreachable) must surface so
		// the collector records failed with the reason instead of masquerading
		// as "service not deployed" (FIX-2).
		return nil, fmt.Errorf("failed to enumerate %s pods: %w", service, err)
	}

	replicas := []Replica{}
	for _, pod := range pods {
		args := kubectlContainerStatusArgs(k.namespace, pod)
		output, err := k.executor.runCommandContext(context.Background(), collectExecTimeout, "kubectl", args...)
		if err != nil {
			return nil, fmt.Errorf("failed to read container statuses for pod %s: %w", pod, err)
		}
		podReplicas, err := parsePodContainerStatuses(service, pod, output)
		if err != nil {
			return nil, err
		}
		replicas = append(replicas, podReplicas...)
	}

	sort.Slice(replicas, func(i, j int) bool {
		if replicas[i].Pod != replicas[j].Pod {
			return replicas[i].Pod < replicas[j].Pod
		}
		return replicas[i].Container < replicas[j].Container
	})
	return replicas, nil
}

// kubectlLogArgs builds the kubectl logs arguments for one replica.
func kubectlLogArgs(namespace string, replica Replica, tailLines int, previous bool) []string {
	args := []string{
		"logs", "-n", namespace, replica.Pod,
		"-c", replica.Container,
		fmt.Sprintf("--tail=%d", tailLines),
	}
	if previous {
		args = append(args, "--previous")
	}
	return args
}

// ReplicaLogs streams a replica's logs through kubectl, bounded by the
// collect transfer timeout (research R2) so a hung kubelet cannot stall the
// whole run.
func (k *KubernetesBackend) ReplicaLogs(replica Replica, tailLines int, previous bool) (io.ReadCloser, func() error, error) {
	args := kubectlLogArgs(k.namespace, replica, tailLines, previous)
	return k.executor.runCommandPipeContext(context.Background(), collectTransferTimeout, "kubectl", args...)
}

// kubectlBenchmarkRunArgs builds the kubectl run arguments for the one-off
// benchmark pod (research R11): attached so its output is captured, never
// restarted, removed after the attach session completes, and bounded by
// --pod-running-timeout so a failing image pull surfaces as an error instead
// of waiting out the whole benchmark bound.
func kubectlBenchmarkRunArgs(namespace, name, image string) []string {
	return []string{
		"run", name,
		"-n", namespace,
		"--image=" + image,
		"--image-pull-policy=Always",
		"--restart=Never",
		"--attach",
		"--rm",
		"--quiet",
		"--pod-running-timeout=5m",
	}
}

// RunBenchmark runs the opt-in benchmark image as a one-off attached pod in
// the namespace (research R11) and returns its combined output. The pod is
// deleted afterwards even when the run timed out — kubectl's --rm only covers
// a completed attach session. Deleting it is permitted: FR-010 protects the
// deployment's workloads and this pod is the tool's own transient resource.
func (k *KubernetesBackend) RunBenchmark(ctx context.Context, image, podName string) (string, error) {
	defer func() {
		// context.Background(): the pod must be deleted even when ctx was
		// cancelled by a timeout or an interrupt.
		if _, err := k.executor.runCommandContext(context.Background(), collectExecTimeout,
			"kubectl", "delete", "pod", podName, "-n", k.namespace, "--ignore-not-found=true", "--now"); err != nil {
			logrus.Debugf("Failed to delete benchmark pod %s: %v", podName, err)
		}
	}()

	args := kubectlBenchmarkRunArgs(k.namespace, podName, image)
	output, err := k.executor.runCommandContext(ctx, collectBenchmarkTimeout, "kubectl", args...)
	if err != nil {
		if output != "" {
			return "", fmt.Errorf("kubectl run failed: %w: %s", err, commandErrorLine(output))
		}
		return "", fmt.Errorf("kubectl run failed: %w", err)
	}
	return output, nil
}

// Metrics captures one-shot pod resource metrics via kubectl top. A missing
// metrics-server surfaces as a normal error so the metrics collector records
// failed in the manifest without aborting the run (research R4).
func (k *KubernetesBackend) Metrics() (string, error) {
	output, err := k.executor.runCommandContext(context.Background(), collectExecTimeout, "kubectl", "top", "pods", "-n", k.namespace)
	if err != nil {
		if output != "" {
			return "", fmt.Errorf("kubectl top pods failed: %w: %s", err, output)
		}
		return "", fmt.Errorf("kubectl top pods failed: %w", err)
	}
	return output, nil
}

// helmChartJSONPath reads, per workload in the namespace, the standard Helm
// stamps: the helm.sh/chart label ("<name>-<version>") and the
// meta.helm.sh/release-name annotation, tab-separated, one workload per line.
// The dotted keys are escaped for kubectl jsonpath; the "/" needs no escaping.
const helmChartJSONPath = `jsonpath={range .items[*]}` +
	`{.metadata.labels.helm\.sh/chart}{"\t"}` +
	`{.metadata.annotations.meta\.helm\.sh/release-name}{"\n"}{end}`

// HelmRelease reports the Helm chart and version a Kubernetes install was
// templated from, read from the labels/annotations Helm stamps on its
// resources. It needs only kubectl — the helm CLI is not required — and
// returns (nil, nil) when the workloads carry no Helm metadata (e.g. an
// install not managed by Helm).
func (k *KubernetesBackend) HelmRelease() (*HelmRelease, error) {
	output, err := k.executor.runCommandContext(context.Background(), collectExecTimeout,
		"kubectl", "get", "deployments,statefulsets", "-n", k.namespace, "-o", helmChartJSONPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read Helm metadata from namespace %s: %w", k.namespace, err)
	}
	return parseHelmChartLabels(output), nil
}

// infrahubProductCharts are the Helm chart names for the Infrahub product
// itself. A namespace commonly co-hosts auxiliary Infrahub charts — most
// notably infrahub-observability — whose workloads carry their own
// helm.sh/chart stamp. The manifest must report the product chart's version,
// so parseHelmChartLabels prefers a workload templated from one of these.
var infrahubProductCharts = map[string]bool{
	"infrahub":            true,
	"infrahub-enterprise": true,
}

// parseHelmChartLabels extracts the release/chart/version from the
// tab-separated "<chart-label>\t<release-name>" lines HelmRelease's jsonpath
// query produces. It prefers the first workload templated from an Infrahub
// product chart (infrahub / infrahub-enterprise), so a co-hosted chart such as
// infrahub-observability never shadows the product's version regardless of the
// order kubectl lists workloads. When no product chart is present it falls back
// to the first workload carrying any chart label, and returns nil when none do
// (kubectl emits empty fields for resources without the stamps).
func parseHelmChartLabels(output string) *HelmRelease {
	var fallback *HelmRelease
	for _, line := range nonEmptyLines(output) {
		fields := strings.SplitN(line, "\t", 2)
		chart := strings.TrimSpace(fields[0])
		if chart == "" {
			continue
		}
		release := &HelmRelease{}
		if len(fields) == 2 {
			release.ReleaseName = strings.TrimSpace(fields[1])
		}
		if name, version, ok := splitHelmChartLabel(chart); ok {
			release.Chart = name
			release.ChartVersion = version
		} else {
			release.Chart = chart
		}
		if infrahubProductCharts[release.Chart] {
			return release
		}
		if fallback == nil {
			fallback = release
		}
	}
	return fallback
}

// splitHelmChartLabel splits a Helm "<name>-<version>" chart label at the
// first hyphen that begins the version — a hyphen followed by a digit, since
// chart versions are semver. This preserves chart names with embedded hyphens
// (infrahub-enterprise-1.2.3 → "infrahub-enterprise", "1.2.3") and versions
// with prerelease suffixes (infrahub-1.2.3-alpha.1 → "infrahub",
// "1.2.3-alpha.1"). It returns ok=false when no version segment is found.
func splitHelmChartLabel(chart string) (name, version string, ok bool) {
	for i := 0; i < len(chart)-1; i++ {
		if chart[i] == '-' && chart[i+1] >= '0' && chart[i+1] <= '9' {
			return chart[:i], chart[i+1:], true
		}
	}
	return "", "", false
}
