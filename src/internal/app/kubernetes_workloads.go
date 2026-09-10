package app

import (
	"encoding/json"
	"errors"
	"fmt"
)

// workloadKinds are the resources an Infrahub service can be deployed as, in
// the order they are searched. Named once so the resolver that scales a
// workload and the location decision that reads one for evidence cannot come to
// disagree about what a workload is.
var workloadKinds = []string{"deployment", "statefulset"}

type kubernetesWorkload struct {
	Name           string
	SelectorLabels map[string]string
	TemplateLabels map[string]string
}

// resolvedWorkload is the kind and name a service's workload resolved to.
type resolvedWorkload struct {
	kind, name string
}

// findWorkloadResource locates a workload (deployment or statefulset) for a
// service, remembering the answer for the run (see KubernetesBackend.workloads).
func (k *KubernetesBackend) findWorkloadResource(service string) (string, string, error) {
	if workload, ok := k.workloads[service]; ok {
		return workload.kind, workload.name, nil
	}

	kind, name, err := k.findWorkloadResourceWith(k.stdoutRunner(), service)
	if err != nil {
		return "", "", err
	}
	k.workloads[service] = resolvedWorkload{kind: kind, name: name}

	return kind, name, nil
}

// findWorkloadResourceWith is findWorkloadResource against the supplied runner,
// following the package's injection idiom so workload resolution is testable
// without a cluster.
//
// Everything that resolves a workload here goes on to scale it — Start, Stop
// and scaleServices are the only callers — so a wrong answer is not a failed
// lookup, it is another product's workload scaled to zero. Two shipped defects
// made that reachable on every Kubernetes backup, and both are fixed here:
//
//   - `strings.Contains(workload.Name, service)` accepted any name containing
//     the service, so `redis-cache` was resolved as `cache`. A name is now
//     accepted only when the deployment claims it (see kubernetes_ownership.go),
//     under the policy that service's answer is scaled by — strict for a
//     database, because a restore starting `task-manager-db` in a namespace
//     that declares no labels would otherwise scale an unrelated
//     `task-manager-db-exporter` (see unanchoredNamePolicyFor).
//   - `if err != nil || output == ""` treated a kubectl failure exactly like an
//     empty result, so a transient API error fell straight through to that
//     name match. Failures are now collected and reported.
//
// The order of evidence is: a label selector query, then the workload's own
// labels (which is where the chart's shortened `infrahub/service=server` is
// read), then an ownership-anchored name match. When nothing resolves, a
// namespace that answered every query reports that no workload was found,
// while a namespace that failed to answer reports the failures — the caller's
// non-destructive branch depends on being able to tell those apart.
//
// The release prefixes are derived from every kind together, and that scope is
// the third defect. They used to be derived inside the kind loop, from that
// kind's listing alone, while the location decision derived them namespace-wide
// — so a chart that labels its StatefulSets and not its Deployments declared a
// prefix the Deployment pass could not see, the Deployment pass fell through to
// the unanchored reading, and `redis-cache` was resolved as `cache` and scaled
// to zero in a namespace that had said perfectly clearly what its release was
// called. Both halves of the answer now read the same evidence at the same
// scope.
func (k *KubernetesBackend) findWorkloadResourceWith(run podRunner, service string) (string, string, error) {
	selectors := k.podSelectors(service)
	queryErrs := []error{}
	listed := map[string][]kubernetesWorkload{}

	for _, kind := range workloadKinds {
		for _, selector := range selectors {
			output, err := run("kubectl", "get", kind, "-n", k.namespace, "-l", selector, "-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
			if err != nil {
				queryErrs = append(queryErrs, fmt.Errorf("%s by %s: %w", kind, selector, err))

				continue
			}
			names := nonEmptyLines(output)
			if len(names) > 0 {
				return kind, names[0], nil
			}
		}

		workloads, err := k.listWorkloadsWith(run, kind)
		if err != nil {
			// The listing is where both the label and the ownership evidence
			// come from, so without it there is nothing to match a name
			// against. The shipped code fell back to a bare name listing here,
			// which is the one path where a kubectl failure could still end in
			// a foreign workload being scaled.
			queryErrs = append(queryErrs, fmt.Errorf("list %s: %w", kind, err))

			continue
		}
		listed[kind] = workloads
	}

	// Label evidence across every kind before any name match on any kind: a
	// workload that says what it is outranks one that merely carries a name
	// this deployment claims, whichever kind each of them is — the same
	// precedence claimedPodNamesWith applies to pods.
	for _, kind := range workloadKinds {
		for _, workload := range listed[kind] {
			for _, selector := range selectors {
				if selectorMatchesLabels(selector, workload.SelectorLabels) || selectorMatchesLabels(selector, workload.TemplateLabels) {
					return kind, workload.Name, nil
				}
			}
			if declaresService(declaredServiceIn(workload.SelectorLabels, workload.TemplateLabels), service) {
				return kind, workload.Name, nil
			}
		}
	}

	prefixes := namespaceReleasePrefixes(listed)
	for _, kind := range workloadKinds {
		for _, workload := range listed[kind] {
			declared := declaredServiceIn(workload.SelectorLabels, workload.TemplateLabels)
			if deploymentClaimsResource(prefixes, workload.Name, declared, service) {
				return kind, workload.Name, nil
			}
			logUnclaimedResource(kind, workload.Name, declared, service, prefixes)
		}
	}

	if len(queryErrs) > 0 {
		return "", "", fmt.Errorf("failed to resolve a workload for %s in namespace %s: %w", service, k.namespace, errors.Join(queryErrs...))
	}

	return "", "", fmt.Errorf("no workloads found for %s in namespace %s", service, k.namespace)
}

// namespaceReleasePrefixes reads the release prefixes every listed kind
// declares between them. A kind whose listing failed contributes nothing, which
// is the same reading listAllWorkloadsWith takes: a prefix that could not be
// read is not evidence that the deployment declared none.
func namespaceReleasePrefixes(listed map[string][]kubernetesWorkload) []string {
	declared := map[string]string{}
	for _, workloads := range listed {
		for name, service := range declaredWorkloadServices(workloads) {
			declared[name] = service
		}
	}

	return releasePrefixes(declared)
}

// listWorkloadsWith lists a kind's workloads with the labels their selector and
// pod template carry, which is both the label evidence findWorkloadResourceWith
// matches on and the ownership evidence its name fallback is anchored to.
func (k *KubernetesBackend) listWorkloadsWith(run podRunner, kind string) ([]kubernetesWorkload, error) {
	if listed, cached := k.workloadListings[kind]; cached {
		return listed, nil
	}

	output, err := run("kubectl", "get", kind, "-n", k.namespace, "-o", "json")
	if err != nil {
		return nil, err
	}

	var parsed struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Selector struct {
					MatchLabels map[string]string `json:"matchLabels"`
				} `json:"selector"`
				Template struct {
					Metadata struct {
						Labels map[string]string `json:"labels"`
					} `json:"metadata"`
				} `json:"template"`
			} `json:"spec"`
		} `json:"items"`
	}

	if err := json.Unmarshal([]byte(output), &parsed); err != nil {
		return nil, err
	}

	workloads := make([]kubernetesWorkload, 0, len(parsed.Items))
	for _, item := range parsed.Items {
		workloads = append(workloads, kubernetesWorkload{
			Name:           item.Metadata.Name,
			SelectorLabels: item.Spec.Selector.MatchLabels,
			TemplateLabels: item.Spec.Template.Metadata.Labels,
		})
	}
	// Only a listing that was answered is remembered. A failure returns above
	// without reaching here, for the reason namespacePodListing does not
	// memoise one either: it says the cluster could not answer, not that the
	// namespace holds no workloads of this kind (FR-002).
	k.workloadListings[kind] = workloads

	return workloads, nil
}

func (k *KubernetesBackend) scaleServices(services []string, replicas int) error {
	if len(services) == 0 {
		return nil
	}
	for _, service := range services {
		kind, resource, err := k.findWorkloadResource(service)
		if err != nil {
			return fmt.Errorf("failed to resolve workload for %s: %w", service, err)
		}
		if err := k.scaleResource(kind, resource, replicas); err != nil {
			return fmt.Errorf("failed to scale %s (%s/%s) to %d replicas: %w", service, kind, resource, replicas, err)
		}
	}
	k.dropPodCaches()
	return nil
}

func (k *KubernetesBackend) scaleResource(kind, resource string, replicas int) error {
	_, err := k.executor.runCommand("kubectl", "scale", "-n", k.namespace, fmt.Sprintf("%s/%s", kind, resource), fmt.Sprintf("--replicas=%d", replicas))
	return err
}
