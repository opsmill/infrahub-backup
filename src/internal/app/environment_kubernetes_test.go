package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSplitHelmChartLabel(t *testing.T) {
	tests := []struct {
		label       string
		wantName    string
		wantVersion string
		wantOK      bool
	}{
		{"infrahub-1.2.3", "infrahub", "1.2.3", true},
		{"infrahub-enterprise-1.2.3", "infrahub-enterprise", "1.2.3", true},
		{"infrahub-1.2.3-alpha.1", "infrahub", "1.2.3-alpha.1", true},
		{"infrahub-0.16.0-dev0", "infrahub", "0.16.0-dev0", true},
		// Helm replaces "+" with "_" in the chart label's version.
		{"infrahub-1.2.3_build5", "infrahub", "1.2.3_build5", true},
		{"infrahub", "", "", false}, // no version segment
	}
	for _, tt := range tests {
		name, version, ok := splitHelmChartLabel(tt.label)
		if ok != tt.wantOK || name != tt.wantName || version != tt.wantVersion {
			t.Errorf("splitHelmChartLabel(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tt.label, name, version, ok, tt.wantName, tt.wantVersion, tt.wantOK)
		}
	}
}

func TestParseHelmChartLabels(t *testing.T) {
	t.Run("first workload with a chart label wins", func(t *testing.T) {
		// kubectl emits one line per workload; resources without Helm stamps
		// yield an empty chart field (leading tab or blank) and are skipped.
		output := "\t\ninfrahub-1.2.3\tinfrahub\ninfrahub-1.2.3\tinfrahub\n"
		release := parseHelmChartLabels(output)
		if release == nil {
			t.Fatal("parseHelmChartLabels returned nil, want a release")
		}
		if release.Chart != "infrahub" || release.ChartVersion != "1.2.3" || release.ReleaseName != "infrahub" {
			t.Errorf("release = %+v, want chart=infrahub version=1.2.3 release=infrahub", release)
		}
	})

	t.Run("prefers an Infrahub product chart over a co-hosted chart", func(t *testing.T) {
		// A namespace hosting both infrahub-observability and the product chart
		// must report the product chart's version, whichever kubectl lists first.
		output := "infrahub-observability-0.2.0\tobs\ninfrahub-enterprise-1.2.3\tinfrahub\n"
		release := parseHelmChartLabels(output)
		if release == nil {
			t.Fatal("parseHelmChartLabels returned nil, want a release")
		}
		if release.Chart != "infrahub-enterprise" || release.ChartVersion != "1.2.3" || release.ReleaseName != "infrahub" {
			t.Errorf("release = %+v, want chart=infrahub-enterprise version=1.2.3 release=infrahub", release)
		}
	})

	t.Run("falls back to the first chart when no product chart is present", func(t *testing.T) {
		release := parseHelmChartLabels("infrahub-observability-0.2.0\tobs\n")
		if release == nil {
			t.Fatal("parseHelmChartLabels returned nil, want a release")
		}
		if release.Chart != "infrahub-observability" || release.ChartVersion != "0.2.0" || release.ReleaseName != "obs" {
			t.Errorf("release = %+v, want chart=infrahub-observability version=0.2.0 release=obs", release)
		}
	})

	t.Run("chart label without a version segment keeps the whole label", func(t *testing.T) {
		release := parseHelmChartLabels("weirdchart\tmyrelease\n")
		if release == nil {
			t.Fatal("parseHelmChartLabels returned nil, want a release")
		}
		if release.Chart != "weirdchart" || release.ChartVersion != "" || release.ReleaseName != "myrelease" {
			t.Errorf("release = %+v, want chart=weirdchart version=\"\" release=myrelease", release)
		}
	})

	t.Run("no Helm metadata returns nil", func(t *testing.T) {
		if release := parseHelmChartLabels("\t\n\t\n"); release != nil {
			t.Errorf("parseHelmChartLabels = %+v, want nil for an install with no chart labels", release)
		}
		if release := parseHelmChartLabels(""); release != nil {
			t.Errorf("parseHelmChartLabels(\"\") = %+v, want nil", release)
		}
	})
}

// TestGetAllPodsWith_Branches covers the two failure modes GetAllPods must keep
// distinct (FIX-2): kubectl succeeds but nothing matches (errNoPodsMatched, so
// callers record "service not deployed"/skipped) versus a real cluster/API
// failure (a wrapped kubectl error, so callers record failed instead of masking
// the outage as an absent service). getAllPodsWith is the shared core of both
// the unbounded GetAllPods and the bounded getAllPodsContext.
func TestGetAllPodsWith_Branches(t *testing.T) {
	k := NewKubernetesBackend(&Configuration{}, NewCommandExecutor())
	k.namespace = "infrahub"

	t.Run("a matching label selector returns the pods", func(t *testing.T) {
		run := func(name string, args ...string) (string, error) {
			return "infrahub-server-0\ninfrahub-server-1\n", nil
		}
		pods, err := k.getAllPodsWith(run, "infrahub-server")
		if err != nil {
			t.Fatalf("getAllPodsWith failed: %v", err)
		}
		if len(pods) != 2 || pods[0] != "infrahub-server-0" || pods[1] != "infrahub-server-1" {
			t.Errorf("pods = %v, want the two infrahub-server pods", pods)
		}
	})

	t.Run("kubectl succeeds but nothing matches returns errNoPodsMatched", func(t *testing.T) {
		// Every kubectl call succeeds and returns no pods.
		run := func(name string, args ...string) (string, error) { return "", nil }
		pods, err := k.getAllPodsWith(run, "task-manager-background-svc")
		if pods != nil {
			t.Errorf("pods = %v, want nil", pods)
		}
		if !errors.Is(err, errNoPodsMatched) {
			t.Errorf("err = %v, want errNoPodsMatched so callers record skipped", err)
		}
	})

	t.Run("a cluster/API failure surfaces a real error, not the sentinel", func(t *testing.T) {
		// Label-selector calls and the fallback all fail (unreachable API/RBAC).
		run := func(name string, args ...string) (string, error) {
			return "The connection to the server was refused", fmt.Errorf("exit status 1")
		}
		_, err := k.getAllPodsWith(run, "infrahub-server")
		if err == nil {
			t.Fatal("getAllPodsWith succeeded despite an unreachable cluster, want error")
		}
		if errors.Is(err, errNoPodsMatched) {
			t.Errorf("cluster failure was masked as errNoPodsMatched (would exit 0 with an empty bundle): %v", err)
		}
		if !strings.Contains(err.Error(), "failed to list pods") {
			t.Errorf("err = %v, want the wrapped kubectl failure", err)
		}
	})
}

// newTestKubernetesBackend is a backend with an empty pod cache, so each case
// below resolves from its own stub runner rather than from a previous case's
// cached answer.
func newTestKubernetesBackend() *KubernetesBackend {
	k := NewKubernetesBackend(&Configuration{}, NewCommandExecutor())
	k.namespace = "infrahub"

	return k
}

// TestGetPodForServiceWith_Branches is the singular resolver's half of the
// distinction GetAllPods has always drawn (FIX-2). Every caller on the backup
// path resolves through getPodForServiceWith, and the internal-versus-external
// decision reads its error: a genuinely empty match means the database is
// elsewhere, while a kubectl/RBAC/API failure means the question was not
// answered at all.
func TestGetPodForServiceWith_Branches(t *testing.T) {
	t.Run("a matching label selector returns the pod", func(t *testing.T) {
		run := func(name string, args ...string) (string, error) {
			return "infrahub-database-0\n", nil
		}
		pod, err := newTestKubernetesBackend().getPodForServiceWith(run, "database")
		if err != nil {
			t.Fatalf("getPodForServiceWith failed: %v", err)
		}
		if pod != "infrahub-database-0" {
			t.Errorf("pod = %q, want infrahub-database-0", pod)
		}
	})

	t.Run("kubectl succeeds but nothing matches returns errNoPodsMatched", func(t *testing.T) {
		run := func(name string, args ...string) (string, error) { return "", nil }
		pod, err := newTestKubernetesBackend().getPodForServiceWith(run, "database")
		if pod != "" {
			t.Errorf("pod = %q, want no pod", pod)
		}
		if !errors.Is(err, errNoPodsMatched) {
			t.Errorf("err = %v, want errNoPodsMatched so the empty match is distinguishable", err)
		}
	})

	t.Run("a pod-list permission failure surfaces a real error, not the sentinel", func(t *testing.T) {
		// What RBAC that cannot list pods in the namespace actually looks like:
		// every kubectl call fails, including the substring fallback.
		run := func(name string, args ...string) (string, error) {
			return "", fmt.Errorf(`pods is forbidden: User "system:serviceaccount:infrahub:backup" cannot list resource "pods" in API group "" in the namespace "infrahub"`)
		}
		_, err := newTestKubernetesBackend().getPodForServiceWith(run, "database")
		if err == nil {
			t.Fatal("getPodForServiceWith succeeded despite being unable to list pods, want error")
		}
		if errors.Is(err, errNoPodsMatched) {
			t.Errorf("a permission failure was masked as errNoPodsMatched: %v", err)
		}
		if !strings.Contains(err.Error(), "failed to list pods") {
			t.Errorf("err = %v, want the wrapped kubectl failure", err)
		}
		if !strings.Contains(err.Error(), "is forbidden") {
			t.Errorf("err = %v, want it to keep the reason kubectl gave", err)
		}
	})
}

// TestLocateServiceSeedsTheResolver is the FR-015 cost the location gate would
// otherwise add to every Kubernetes run, all-internal ones included: the
// decision issues the pod listing, learns the name, throws it away, and the
// resolver issues the same listing a moment later.
func TestLocateServiceSeedsTheResolver(t *testing.T) {
	t.Run("a single live pod is handed to the resolver", func(t *testing.T) {
		listings := 0
		run := func(_ string, _ ...string) (string, error) {
			listings++

			return "infrahub-database-0;Running\n", nil
		}

		backend := newTestKubernetesBackend()
		if _, err := backend.locateServiceWith(run, serviceNeo4j); err != nil {
			t.Fatalf("locateServiceWith() error = %v", err)
		}

		after := listings
		pod, err := backend.getPodForServiceWith(run, serviceNeo4j)
		if err != nil {
			t.Fatalf("getPodForServiceWith() error = %v", err)
		}
		if pod != "infrahub-database-0" {
			t.Errorf("resolved pod = %q, want the one the location decision already read", pod)
		}
		if listings != after {
			t.Errorf("the resolver issued %d further listings, want none: the decision had already read the answer", listings-after)
		}
	})

	t.Run("a terminated pod is never seeded", func(t *testing.T) {
		// The location decision counts a Failed pod as evidence the database
		// lives here; the resolver must not exec into one. Seeding what the
		// decision matched would put a terminated pod in front of every exec
		// that followed.
		run := func(_ string, _ ...string) (string, error) { return "infrahub-database-0;Failed\n", nil }

		backend := newTestKubernetesBackend()
		if location, err := backend.locateServiceWith(run, serviceNeo4j); err != nil || location != EndpointLocationInternal {
			t.Fatalf("locateServiceWith() = %q, %v; want the failed pod to still read as internal", location, err)
		}
		if pod, cached := backend.podCache[serviceNeo4j]; cached {
			t.Errorf("seeded the resolver with %q, which is in a terminal phase", pod)
		}
	})

	t.Run("an HA cluster is left to the primary lookup", func(t *testing.T) {
		// Above one live pod the resolver asks which member is the primary.
		// Seeding here would replace that with whichever pod the listing
		// happened to put first.
		run := func(_ string, _ ...string) (string, error) {
			return "pg-cluster-1;Running\npg-cluster-2;Running\n", nil
		}

		backend := newTestKubernetesBackend()
		if _, err := backend.locateServiceWith(run, serviceTaskManagerDB); err != nil {
			t.Fatalf("locateServiceWith() error = %v", err)
		}
		if pod, cached := backend.podCache[serviceTaskManagerDB]; cached {
			t.Errorf("seeded the resolver with %q, bypassing the primary lookup an HA cluster needs", pod)
		}
	})
}

// TestLocateServiceMemoisesItsAnswer covers the other half of the same cost.
// Every path that touches a database resolves through a databaseTarget and each
// asks the question again, against an answer that cannot change while the run
// lasts.
func TestLocateServiceMemoisesItsAnswer(t *testing.T) {
	backend := newTestKubernetesBackend()
	backend.serviceLocations[serviceNeo4j] = EndpointLocationExternal

	location, err := backend.locateService(serviceNeo4j)
	if err != nil {
		t.Fatalf("locateService() error = %v", err)
	}
	if location != EndpointLocationExternal {
		t.Errorf("location = %q, want the memoised %q", location, EndpointLocationExternal)
	}

	// A failure must not be memoised: it says the cluster could not answer, not
	// where the database is, and remembering it would take away the retry the
	// caller still has.
	failing := newTestKubernetesBackend()
	if _, err := failing.locateServiceWith(func(string, ...string) (string, error) {
		return "", fmt.Errorf("the server was unable to return a response")
	}, serviceNeo4j); err == nil {
		t.Fatal("locateServiceWith() = nil error, want the listing failure")
	}
	if _, cached := failing.serviceLocations[serviceNeo4j]; cached {
		t.Error("a listing failure was memoised as a location")
	}
}

// TestLocateServiceWith is FR-002 at the point where it can go wrong most
// dangerously. External mode is engaged only on a positively empty match; a
// namespace the run cannot list pods in must fail, because reinterpreting that
// as "the database must be external" would send the run looking for a database
// on the network instead of stopping.
func TestLocateServiceWith(t *testing.T) {
	t.Run("a resolved pod means the database is internal", func(t *testing.T) {
		run := func(name string, args ...string) (string, error) {
			return "infrahub-database-0\n", nil
		}
		location, err := newTestKubernetesBackend().locateServiceWith(run, serviceNeo4j)
		if err != nil {
			t.Fatalf("locateServiceWith failed: %v", err)
		}
		if location != EndpointLocationInternal {
			t.Errorf("location = %q, want %q", location, EndpointLocationInternal)
		}
	})

	t.Run("an empty match means the database is external", func(t *testing.T) {
		run := func(name string, args ...string) (string, error) { return "", nil }
		location, err := newTestKubernetesBackend().locateServiceWith(run, serviceNeo4j)
		if err != nil {
			t.Fatalf("locateServiceWith failed: %v", err)
		}
		if location != EndpointLocationExternal {
			t.Errorf("location = %q, want %q", location, EndpointLocationExternal)
		}
	})

	t.Run("a pod-list permission failure is an error and not external", func(t *testing.T) {
		run := func(name string, args ...string) (string, error) {
			return "", fmt.Errorf(`pods is forbidden: User "system:serviceaccount:infrahub:backup" cannot list resource "pods" in API group "" in the namespace "infrahub"`)
		}
		location, err := newTestKubernetesBackend().locateServiceWith(run, serviceNeo4j)
		if err == nil {
			t.Fatal("locateServiceWith succeeded despite being unable to list pods, want error")
		}
		if location == EndpointLocationExternal {
			t.Error("a permission failure engaged external mode, which would send the run onto the network looking for a database that is in the namespace it could not read")
		}
		if location != "" {
			t.Errorf("location = %q, want the zero value alongside an error", location)
		}
		if !strings.Contains(err.Error(), serviceNeo4j) {
			t.Errorf("err = %v, want it to name the %q service", err, serviceNeo4j)
		}
		if !strings.Contains(err.Error(), "is forbidden") {
			t.Errorf("err = %v, want it to keep the reason kubectl gave", err)
		}
	})

	t.Run("an unreachable API server is an error and not external", func(t *testing.T) {
		run := func(name string, args ...string) (string, error) {
			return "", fmt.Errorf("The connection to the server 10.0.0.1:6443 was refused")
		}
		location, err := newTestKubernetesBackend().locateServiceWith(run, serviceTaskManagerDB)
		if err == nil {
			t.Fatal("locateServiceWith succeeded despite an unreachable API server, want error")
		}
		if location == EndpointLocationExternal {
			t.Errorf("an unreachable control plane engaged external mode: %v", err)
		}
	})
}

// TestGetAllPodsExcludesTheTransientWorkload is FR-028 asserted at the
// resolver: registering the transient external-database pod so that execution
// resolves to it must not make it discoverable as a replica of the service it
// stands in for. GetAllPods and the bounded getAllPodsContext are one-line
// delegations to getAllPodsWith, and ServiceReplicas enumerates through
// getAllPodsContext, so the exclusion is asserted here once for all three.
//
// Both mechanisms are covered, because each protects against a different future
// change: the label selector holds whatever the pod is named, and the name
// filter holds if a label change ever collided with a selector key.
func TestGetAllPodsExcludesTheTransientWorkload(t *testing.T) {
	transientPod := transientObjectPrefix + "-capture-a1b2c3d4"

	t.Run("a transient pod a selector matched is not returned as a replica", func(t *testing.T) {
		// The cluster is pretended to ignore the exclusion, which is what a
		// future label collision would look like: every listing returns the
		// transient pod alongside the deployment's own.
		k := NewKubernetesBackend(&Configuration{}, NewCommandExecutor())
		k.namespace = "infrahub"
		run := func(_ string, _ ...string) (string, error) {
			return transientPod + "\ndatabase-0\n", nil
		}

		pods, err := k.getAllPodsWith(run, serviceNeo4j)
		if err != nil {
			t.Fatalf("getAllPodsWith failed: %v", err)
		}
		if !contains(pods, "database-0") {
			t.Errorf("pods = %v, want the deployment's own database pod", pods)
		}
		if contains(pods, transientPod) {
			t.Errorf("pods = %v, want the transient workload %s excluded (FR-028)", pods, transientPod)
		}
	})

	t.Run("a namespace holding only the transient pod reports no replicas", func(t *testing.T) {
		// The external-database case: the service has no pod of its own, and the
		// transient stand-in must not be mistaken for one — a replica count of
		// one here would put a backup client into troubleshooting bundles.
		k := NewKubernetesBackend(&Configuration{}, NewCommandExecutor())
		k.namespace = "infrahub"
		run := func(_ string, _ ...string) (string, error) {
			return transientPod + "\n", nil
		}

		pods, err := k.getAllPodsWith(run, serviceNeo4j)
		if !errors.Is(err, errNoPodsMatched) {
			t.Errorf("getAllPodsWith() = %v, %v, want errNoPodsMatched", pods, err)
		}
		if len(pods) != 0 {
			t.Errorf("pods = %v, want none", pods)
		}
	})

	t.Run("every pod listing carries the label exclusion", func(t *testing.T) {
		k := NewKubernetesBackend(&Configuration{}, NewCommandExecutor())
		k.namespace = "infrahub"
		listings := [][]string{}
		run := func(_ string, args ...string) (string, error) {
			listings = append(listings, args)

			return "", nil
		}

		if _, err := k.getAllPodsWith(run, serviceNeo4j); !errors.Is(err, errNoPodsMatched) {
			t.Fatalf("getAllPodsWith() error = %v, want errNoPodsMatched", err)
		}
		if len(listings) == 0 {
			t.Fatal("no kubectl listings were issued")
		}
		for _, args := range listings {
			selector := ""
			for i, arg := range args {
				if arg == "-l" && i+1 < len(args) {
					selector = args[i+1]
				}
			}
			if !strings.Contains(selector, transientWorkloadExclusion) {
				t.Errorf("listing %v selected on %q, want it to exclude %s server-side", args, selector, transientLabelMarker)
			}
		}
	})
}

// podListing renders a kubectl pod listing the way podNamePhaseJSONPath does,
// so a stub runner answers in the shape the resolvers parse.
func podListing(pods ...labelledPod) string {
	lines := make([]string, 0, len(pods))
	for _, pod := range pods {
		// Rendered through ownershipLine so a labelledPod handed to a fake
		// cluster round-trips through the real parser. It used to render
		// `<name>;<phase>` alone, which silently dropped Service and would
		// silently drop Transient — a fake that cannot express the field the
		// filter reads is a fake that cannot test the filter.
		labels := map[string]string{}
		if pod.Transient {
			labels[transientLabelMarker] = "true"
		}
		if pod.Service != "" {
			labels[serviceLabelKeys[0]] = pod.Service
		}
		lines = append(lines, ownershipLine(pod.Name, pod.Phase, labels))
	}

	return strings.Join(lines, "\n") + "\n"
}

// selectorOf returns the value of the -l argument in a recorded kubectl call.
func selectorOf(args []string) string {
	for i, arg := range args {
		if arg == "-l" && i+1 < len(args) {
			return args[i+1]
		}
	}

	return ""
}

// serviceSelectorOf returns the service-label clause of a recorded kubectl
// call's -l argument, with the transient-workload exclusion every pod listing
// now carries removed (see podListingArgsFor). A namespace listing selects on
// the exclusion alone and so has no service clause.
func serviceSelectorOf(args []string) string {
	selector := selectorOf(args)
	if selector == transientWorkloadExclusion {
		return ""
	}

	return strings.TrimSuffix(selector, ","+transientWorkloadExclusion)
}

// isServiceSelectorQuery reports whether a recorded kubectl call selected on a
// service label rather than listing the namespace. `-l` is now present on both,
// so its presence no longer tells them apart; what the call selects on does.
func isServiceSelectorQuery(args []string) bool {
	return serviceSelectorOf(args) != ""
}

// labelBlindCluster is the namespace shape every false positive shares: no pod
// declares itself as the service, and the full-namespace listing holds a pod
// whose *name* names it. Label-selected listings answer empty; the unselected
// listing answers with the supplied pods.
func labelBlindCluster(pods ...labelledPod) (podRunner, *int) {
	fallbackCalls := 0
	run := func(_ string, args ...string) (string, error) {
		if isServiceSelectorQuery(args) {
			return "", nil
		}
		fallbackCalls++

		return podListing(pods...), nil
	}

	return run, &fallbackCalls
}

// TestLocateServiceWithRejectsPodsThatOnlyNameTheService is finding 1 at the
// decision that matters. Each of these pods names a database service without
// being one, and the tool execs `neo4j-admin`, `pg_dump` — and on restore, a
// load — inside whatever pod the location decision accepted. So a namespace
// holding one of them must still report a genuinely external database as
// external.
func TestLocateServiceWithRejectsPodsThatOnlyNameTheService(t *testing.T) {
	tests := []struct {
		name    string
		service string
		pod     labelledPod
	}{
		{
			name:    "an unrelated StatefulSet's pod",
			service: serviceNeo4j,
			pod:     labelledPod{Name: "postgres-database-0", Phase: "Running"},
		},
		{
			name:    "a completed backup Job's pod",
			service: serviceNeo4j,
			pod:     labelledPod{Name: "infrahub-database-backup-28123456-x9v2t", Phase: "Succeeded"},
		},
		{
			name:    "a metrics sidecar",
			service: serviceTaskManagerDB,
			pod:     labelledPod{Name: "task-manager-db-exporter-6b8f7c9d4-lm2xq", Phase: "Running"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run, _ := labelBlindCluster(tt.pod)

			location, err := newTestKubernetesBackend().locateServiceWith(run, tt.service)
			if err != nil {
				t.Fatalf("locateServiceWith failed: %v", err)
			}
			if location != EndpointLocationExternal {
				t.Errorf("location = %q for a namespace whose only %s-shaped pod is %s, want %q: the run would capture (and on restore write) inside a pod that belongs to something else",
					location, tt.service, tt.pod.Name, EndpointLocationExternal)
			}
		})
	}
}

// TestGetPodForServiceWithRejectsFinishedPods is the "at minimum" half of
// finding 1 at the resolver an exec target comes from: a pod nothing is running
// in is not somewhere to run a database utility, whatever it is called.
func TestGetPodForServiceWithRejectsFinishedPods(t *testing.T) {
	finished := []labelledPod{
		{Name: "infrahub-database-backup-28123456-x9v2t", Phase: "Succeeded"},
		{Name: "infrahub-database-migrate-lm2xq", Phase: "Failed"},
	}

	t.Run("a finished pod is not resolved as the service", func(t *testing.T) {
		run, _ := labelBlindCluster(finished...)

		pod, err := newTestKubernetesBackend().getPodForServiceWith(run, serviceNeo4j)
		if !errors.Is(err, errNoPodsMatched) {
			t.Errorf("getPodForServiceWith() = %q, %v, want errNoPodsMatched", pod, err)
		}
		if pod != "" {
			t.Errorf("pod = %q, want no pod: nothing runs in a %s pod", pod, "Succeeded/Failed")
		}
	})

	t.Run("a live pod alongside finished ones still resolves", func(t *testing.T) {
		// The namespace declares itself, so `infrahub` is a release prefix this
		// deployment established and the database pod is claimed on its name.
		// A namespace declaring nothing at all is a different question, and one
		// the location decision already answers "external" — the resolver now
		// gives the same answer rather than a second one (see
		// unanchoredNamePolicyFor).
		listing := ownershipListing(
			ownershipLine("infrahub-server-7d9f4-abcde", "Running", infrahubServerLabels),
			ownershipLine(finished[0].Name, finished[0].Phase, nil),
			ownershipLine(finished[1].Name, finished[1].Phase, nil),
			ownershipLine("infrahub-database-0", "Running", nil),
		)
		run := func(_ string, args ...string) (string, error) {
			if isServiceSelectorQuery(args) {
				return "", nil
			}

			return listing, nil
		}

		pod, err := newTestKubernetesBackend().getPodForServiceWith(run, serviceNeo4j)
		if err != nil {
			t.Fatalf("getPodForServiceWith failed: %v", err)
		}
		if pod != "infrahub-database-0" {
			t.Errorf("pod = %q, want infrahub-database-0", pod)
		}
	})

	t.Run("every pod listing asks the cluster for live pods only", func(t *testing.T) {
		listings := [][]string{}
		run := func(_ string, args ...string) (string, error) {
			listings = append(listings, args)

			return "", nil
		}

		if _, err := newTestKubernetesBackend().getPodForServiceWith(run, serviceNeo4j); !errors.Is(err, errNoPodsMatched) {
			t.Fatalf("getPodForServiceWith() error = %v, want errNoPodsMatched", err)
		}
		if len(listings) == 0 {
			t.Fatal("no kubectl listings were issued")
		}
		for _, args := range listings {
			if !contains(args, "--field-selector="+livePodFieldSelector) {
				t.Errorf("listing %v does not filter finished pods server-side, want --field-selector=%s", args, livePodFieldSelector)
			}
		}
	})
}

// TestNameFallbackStillResolvesTheHelmLabelledPod is the constraint the
// tightening had to preserve: the chart labels some pods with a different
// service value than the canonical name (infrahub-server pods carry
// infrahub/service=server), and both resolvers exist in their current shape
// because of it. infrahub-collect's per-replica log collection enumerates
// through getAllPodsWith, so both are asserted.
func TestNameFallbackStillResolvesTheHelmLabelledPod(t *testing.T) {
	helmPod := labelledPod{Name: "infrahub-server-7d9f4b6c8-abcde", Phase: "Running"}

	t.Run("the singular resolver", func(t *testing.T) {
		run, fallbackCalls := labelBlindCluster(helmPod)

		pod, err := newTestKubernetesBackend().getPodForServiceWith(run, "infrahub-server")
		if err != nil {
			t.Fatalf("getPodForServiceWith failed: %v", err)
		}
		if pod != helmPod.Name {
			t.Errorf("pod = %q, want %q resolved by name after the labels missed it", pod, helmPod.Name)
		}
		if *fallbackCalls == 0 {
			t.Error("the name fallback was never reached, so this case did not exercise it")
		}
	})

	t.Run("the plural resolver", func(t *testing.T) {
		run, _ := labelBlindCluster(helmPod, labelledPod{Name: "infrahub-server-7d9f4b6c8-fghij", Phase: "Running"})

		pods, err := newTestKubernetesBackend().getAllPodsWith(run, "infrahub-server")
		if err != nil {
			t.Fatalf("getAllPodsWith failed: %v", err)
		}
		if len(pods) != 2 || pods[0] != helmPod.Name {
			t.Errorf("pods = %v, want both infrahub-server pods resolved by name", pods)
		}
	})

	t.Run("a name that merely contains the letters does not match", func(t *testing.T) {
		run, _ := labelBlindCluster(labelledPod{Name: "mydatabasetool-0", Phase: "Running"})

		pod, err := newTestKubernetesBackend().getPodForServiceWith(run, serviceNeo4j)
		if !errors.Is(err, errNoPodsMatched) {
			t.Errorf("getPodForServiceWith() = %q, %v, want errNoPodsMatched", pod, err)
		}
	})
}

// TestEverySelectorFailingIsNeverAnEmptyMatch is finding 3. Multiple selectors
// exist because charts label differently, so one failing must not abort the
// search — but if every one of them failed, nothing was established, and the
// sentinel would tell the caller "not deployed" (and locateService "external")
// on the strength of queries that were never answered. The shape that reaches
// it: a shrunken --external-db-timeout under which the four selector calls
// expire while the plain listing answers.
func TestEverySelectorFailingIsNeverAnEmptyMatch(t *testing.T) {
	// Label-selected listings fail; the unselected fallback listing succeeds
	// and holds nothing that names the service.
	selectorsExpire := func(_ string, args ...string) (string, error) {
		if isServiceSelectorQuery(args) {
			return "", fmt.Errorf("context deadline exceeded")
		}

		return podListing(labelledPod{Name: "infrahub-server-7d9f4b6c8-abcde", Phase: "Running"}), nil
	}

	t.Run("the singular resolver reports the failure", func(t *testing.T) {
		_, err := newTestKubernetesBackend().getPodForServiceWith(selectorsExpire, serviceNeo4j)
		if err == nil {
			t.Fatal("getPodForServiceWith succeeded despite every selector query failing, want error")
		}
		if errors.Is(err, errNoPodsMatched) {
			t.Errorf("unanswered selector queries were reported as an empty match: %v", err)
		}
		if !strings.Contains(err.Error(), "context deadline exceeded") {
			t.Errorf("err = %v, want it to keep the reason the queries failed", err)
		}
	})

	t.Run("the plural resolver reports the failure", func(t *testing.T) {
		pods, err := newTestKubernetesBackend().getAllPodsWith(selectorsExpire, serviceNeo4j)
		if err == nil {
			t.Fatalf("getAllPodsWith() = %v, nil, want an error", pods)
		}
		if errors.Is(err, errNoPodsMatched) {
			t.Errorf("unanswered selector queries were reported as an empty match: %v", err)
		}
	})

	t.Run("the location decision refuses to call it external", func(t *testing.T) {
		location, err := newTestKubernetesBackend().locateServiceWith(selectorsExpire, serviceNeo4j)
		if err == nil {
			t.Fatal("locateServiceWith succeeded despite every selector query failing, want error")
		}
		if location == EndpointLocationExternal {
			t.Error("a loaded API server turned into an external database, which is FR-002 exactly")
		}
		if location != "" {
			t.Errorf("location = %q, want the zero value alongside an error", location)
		}
	})

	t.Run("one selector failing still resolves through the others", func(t *testing.T) {
		// The first selector fails; a later one matches. Charts label
		// differently, so this must not abort the search.
		run := func(_ string, args ...string) (string, error) {
			switch serviceSelectorOf(args) {
			case "app.kubernetes.io/component=" + serviceNeo4j:
				return "", fmt.Errorf("etcdserver: request timed out")
			case "infrahub/service=" + serviceNeo4j:
				return podListing(labelledPod{Name: "infrahub-database-0", Phase: "Running"}), nil
			}

			return "", nil
		}

		location, err := newTestKubernetesBackend().locateServiceWith(run, serviceNeo4j)
		if err != nil {
			t.Fatalf("locateServiceWith failed: %v", err)
		}
		if location != EndpointLocationInternal {
			t.Errorf("location = %q, want %q: a pod carrying the chart's own label was found", location, EndpointLocationInternal)
		}
	})
}

// resourceOf returns the resource kind a recorded kubectl call asked for.
func resourceOf(args []string) string {
	for i, arg := range args {
		if arg == "get" && i+1 < len(args) {
			return args[i+1]
		}
	}

	return ""
}

// labelBlindNamespace answers the three listings the location decision issues:
// label-selected pod queries, which never match; the whole-namespace ownership
// listing, which carries what each pod declares as well as what it is called;
// and the Deployment/StatefulSet listings. It is labelBlindCluster with the
// ownership and workload evidence a namespace actually offers.
//
// pods is an ownershipListing; workloadsByKind maps "deployment"/"statefulset"
// to a workloadListing. A kind with no entry lists nothing.
func labelBlindNamespace(t *testing.T, pods string, workloadsByKind map[string]string) podRunner {
	t.Helper()
	empty := workloadListing(t, nil)

	return func(_ string, args ...string) (string, error) {
		if isServiceSelectorQuery(args) {
			return "", nil
		}
		if resourceOf(args) == "pods" {
			return pods, nil
		}
		if listing, ok := workloadsByKind[resourceOf(args)]; ok {
			return listing, nil
		}

		return empty, nil
	}
}

// serverPod is the one resource every Infrahub deployment has and labels: it is
// what establishes the release name the rest of the deployment's names are
// built from.
var serverPod = ownershipLine("infrahub-server-7d9f4b6c8-abcde", "Running", map[string]string{"infrahub/service": "server"})

// TestLocateServiceWithReadsOwnershipNotLabelsAlone is the other half of
// finding 1, and the half that had been traded away. Deciding on label evidence
// alone declared a working deployment external and hard-refused the run, for
// three unrelated reasons — and external capture is unavailable in this build,
// so that refusal is the whole backup. Absence of a label is not evidence that
// the database is absent.
func TestLocateServiceWithReadsOwnershipNotLabelsAlone(t *testing.T) {
	t.Run("a database the deployment claims by name is internal", func(t *testing.T) {
		// Kustomize, a hand-rolled manifest or a different chart version:
		// nothing on the database's own pod matches a selector, but the
		// namespace says whose deployment this is.
		run := labelBlindNamespace(t, ownershipListing(
			serverPod,
			ownershipLine("infrahub-database-0", "Running", nil),
			ownershipLine("infrahub-task-manager-db-0", "Running", nil),
		), nil)

		for _, service := range []string{serviceNeo4j, serviceTaskManagerDB} {
			location, err := newTestKubernetesBackend().locateServiceWith(run, service)
			if err != nil {
				t.Fatalf("locateServiceWith(%s) failed: %v", service, err)
			}
			if location != EndpointLocationInternal {
				t.Errorf("location = %q for %s, want %q: the deployment's own release names the pod, and refusing here refuses a backup that works today",
					location, service, EndpointLocationInternal)
			}
		}
	})

	t.Run("a CloudNativePG cluster the deployment claims is internal", func(t *testing.T) {
		// The topology findPrimaryPod exists to support: pods named
		// <cluster>-1, labelled cnpg.io/cluster, which is none of the four
		// keys a selector queries.
		run := labelBlindNamespace(t, ownershipListing(
			serverPod,
			ownershipLine("infrahub-task-manager-db-1", "Running", map[string]string{"cnpg.io/cluster": "infrahub-task-manager-db"}),
			ownershipLine("infrahub-task-manager-db-2", "Running", map[string]string{"cnpg.io/cluster": "infrahub-task-manager-db"}),
		), nil)

		location, err := newTestKubernetesBackend().locateServiceWith(run, serviceTaskManagerDB)
		if err != nil {
			t.Fatalf("locateServiceWith failed: %v", err)
		}
		if location != EndpointLocationInternal {
			t.Errorf("location = %q, want %q", location, EndpointLocationInternal)
		}
	})

	t.Run("a failed pod is not evidence of absence", func(t *testing.T) {
		// A pod evicted mid-recreate, or one on a drained node. Where the
		// database lives is not the same question as which pod to exec in.
		run := labelBlindNamespace(t, ownershipListing(
			serverPod,
			ownershipLine("infrahub-database-0", "Failed", nil),
		), nil)

		location, err := newTestKubernetesBackend().locateServiceWith(run, serviceNeo4j)
		if err != nil {
			t.Fatalf("locateServiceWith failed: %v", err)
		}
		if location != EndpointLocationInternal {
			t.Errorf("location = %q, want %q: a pod that failed is still where the database lives", location, EndpointLocationInternal)
		}
	})

	t.Run("a statefulset scaled to zero is internal", func(t *testing.T) {
		// No pod to read at all, which is what makes the workload evidence
		// necessary rather than merely corroborating.
		run := labelBlindNamespace(t, ownershipListing(serverPod), map[string]string{
			"statefulset": workloadListing(t, map[string]map[string]string{
				"infrahub-database": {"infrahub/service": serviceNeo4j},
			}),
		})

		location, err := newTestKubernetesBackend().locateServiceWith(run, serviceNeo4j)
		if err != nil {
			t.Fatalf("locateServiceWith failed: %v", err)
		}
		if location != EndpointLocationInternal {
			t.Errorf("location = %q, want %q: the StatefulSet declares the service, it just has no pods right now", location, EndpointLocationInternal)
		}
	})

	t.Run("an unlabelled statefulset the deployment claims by name is internal", func(t *testing.T) {
		run := labelBlindNamespace(t, ownershipListing(serverPod), map[string]string{
			"statefulset": workloadListing(t, map[string]map[string]string{"infrahub-database": nil}),
		})

		location, err := newTestKubernetesBackend().locateServiceWith(run, serviceNeo4j)
		if err != nil {
			t.Fatalf("locateServiceWith failed: %v", err)
		}
		if location != EndpointLocationInternal {
			t.Errorf("location = %q, want %q", location, EndpointLocationInternal)
		}
	})

	t.Run("a workload listing that fails does not make the run fail", func(t *testing.T) {
		// The pods answered, so the decision is the one they support. Turning
		// a kubectl failure on additive evidence into an abort would break
		// deployments the previous behaviour served.
		run := func(_ string, args ...string) (string, error) {
			switch {
			case isServiceSelectorQuery(args):
				return "", nil
			case resourceOf(args) == "pods":
				return "", nil
			default:
				return "", fmt.Errorf(`deployments.apps is forbidden: User "system:serviceaccount:infrahub:backup" cannot list resource "deployments"`)
			}
		}

		location, err := newTestKubernetesBackend().locateServiceWith(run, serviceNeo4j)
		if err != nil {
			t.Fatalf("locateServiceWith failed: %v", err)
		}
		if location != EndpointLocationExternal {
			t.Errorf("location = %q, want %q", location, EndpointLocationExternal)
		}
	})

	t.Run("a pod listing that fails is still an error", func(t *testing.T) {
		// The pod listing is the answer, not an addition to it, so FR-002
		// applies to it in full.
		run := func(_ string, args ...string) (string, error) {
			if isServiceSelectorQuery(args) {
				return "", nil
			}

			return "", fmt.Errorf("The connection to the server 10.0.0.1:6443 was refused")
		}

		location, err := newTestKubernetesBackend().locateServiceWith(run, serviceNeo4j)
		if err == nil {
			t.Fatal("locateServiceWith succeeded despite being unable to list the namespace's pods, want error")
		}
		if location != "" {
			t.Errorf("location = %q, want the zero value alongside an error", location)
		}
	})
}

// TestLocateServiceWithStillRefusesWhatTheDeploymentDoesNotClaim is the
// constraint the reintroduced evidence had to preserve. A name-shaped match
// counts only against a release prefix something declared, so a namespace that
// says whose deployment it is still rejects the resources that merely name a
// database service.
func TestLocateServiceWithStillRefusesWhatTheDeploymentDoesNotClaim(t *testing.T) {
	tests := []struct {
		name    string
		service string
		pod     string
	}{
		{
			name:    "an unrelated StatefulSet's pod",
			service: serviceNeo4j,
			pod:     ownershipLine("postgres-database-0", "Running", nil),
		},
		{
			name:    "a completed backup Job's pod",
			service: serviceNeo4j,
			pod:     ownershipLine("infrahub-database-backup-28123456-x9v2t", "Succeeded", nil),
		},
		{
			name:    "a metrics sidecar",
			service: serviceTaskManagerDB,
			pod:     ownershipLine("task-manager-db-exporter-6b8f7c9d4-lm2xq", "Running", nil),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run := labelBlindNamespace(t, ownershipListing(serverPod, tt.pod), nil)

			location, err := newTestKubernetesBackend().locateServiceWith(run, tt.service)
			if err != nil {
				t.Fatalf("locateServiceWith failed: %v", err)
			}
			if location != EndpointLocationExternal {
				t.Errorf("location = %q for a namespace whose only %s-shaped resource is %q, want %q: the run would capture (and on restore write) inside something that belongs elsewhere",
					location, tt.service, tt.pod, EndpointLocationExternal)
			}
		})
	}

	t.Run("with nothing declaring itself, a name alone claims nothing", func(t *testing.T) {
		// No server pod, so no release prefix: the same shape as before this
		// evidence was reintroduced.
		run := labelBlindNamespace(t, ownershipListing(ownershipLine("infrahub-database-0", "Running", nil)), nil)

		location, err := newTestKubernetesBackend().locateServiceWith(run, serviceNeo4j)
		if err != nil {
			t.Fatalf("locateServiceWith failed: %v", err)
		}
		if location != EndpointLocationExternal {
			t.Errorf("location = %q, want %q", location, EndpointLocationExternal)
		}
	})
}

// TestLocateServiceWithAsksForPodsThatFailedToo pins the listing
// filter. Excluding Failed is right for resolving an exec target and wrong for
// asking where a database lives, and the two share podListingArgs.
func TestLocateServiceWithAsksForPodsThatFailedToo(t *testing.T) {
	listings := [][]string{}
	run := func(_ string, args ...string) (string, error) {
		listings = append(listings, args)

		return "", nil
	}

	if _, err := newTestKubernetesBackend().locateServiceWith(run, serviceNeo4j); err != nil {
		t.Fatalf("locateServiceWith failed: %v", err)
	}
	if len(listings) == 0 {
		t.Fatal("no kubectl listings were issued")
	}
	for _, args := range listings {
		if resourceOf(args) != "pods" {
			continue
		}
		if contains(args, "--field-selector="+livePodFieldSelector) {
			t.Errorf("listing %v excludes Failed pods, which is a reason to exclude Succeeded and not Failed from a question about where a database lives", args)
		}
		if !contains(args, "--field-selector="+completedPodFieldSelector) {
			t.Errorf("listing %v does not drop completed pods server-side, want --field-selector=%s", args, completedPodFieldSelector)
		}
	}
}

// TestEveryPodListingExcludesTheTransientWorkload is FR-028 stated as the
// structural claim T082 makes: the exclusion is applied where the arguments are
// built, so it holds for every enumerator rather than for the one that
// remembered it. Before this, getAllPodsWith excluded the transient workload
// while getPodStatusesWith, getPodForServiceWith and locateServiceWith listed
// the namespace with no exclusion at all.
func TestEveryPodListingExcludesTheTransientWorkload(t *testing.T) {
	enumerators := map[string]func(*KubernetesBackend, podRunner){
		"getPodForServiceWith": func(k *KubernetesBackend, run podRunner) {
			_, _ = k.getPodForServiceWith(run, serviceNeo4j)
		},
		"getAllPodsWith": func(k *KubernetesBackend, run podRunner) {
			_, _ = k.getAllPodsWith(run, serviceNeo4j)
		},
		"getPodStatusesWith": func(k *KubernetesBackend, run podRunner) {
			_, _ = k.getPodStatusesWith(run, "cache")
		},
		"locateServiceWith": func(k *KubernetesBackend, run podRunner) {
			_, _ = k.locateServiceWith(run, serviceNeo4j)
		},
	}

	for name, enumerate := range enumerators {
		t.Run(name+" excludes it from every pod listing", func(t *testing.T) {
			listings := [][]string{}
			run := func(_ string, args ...string) (string, error) {
				listings = append(listings, args)

				return "", nil
			}

			enumerate(newTestKubernetesBackend(), run)

			podListings := 0
			for _, args := range listings {
				if resourceOf(args) != "pods" {
					continue
				}
				podListings++
				if !strings.Contains(selectorOf(args), transientWorkloadExclusion) {
					t.Errorf("listing %v does not exclude the transient workload, want %s in its selector", args, transientWorkloadExclusion)
				}
			}
			if podListings == 0 {
				t.Fatal("no pod listings were issued, so the exclusion was not exercised")
			}
		})
	}
}

// TestResolverDropsATransientPodALabelSelectorReturned is the client-side half
// of the same exclusion, at the one place it can actually bite: a pod a label
// selector returned is taken without a name match, so a future label change
// that collided with one of the selector keys would hand the transient workload
// straight back as the service's exec target.
func TestResolverDropsATransientPodALabelSelectorReturned(t *testing.T) {
	transient := "infrahub-backup-xdb-capture-a1b2c3d4"
	run := func(_ string, args ...string) (string, error) {
		if isServiceSelectorQuery(args) {
			return podListing(
				labelledPod{Name: transient, Phase: "Running"},
				labelledPod{Name: "infrahub-database-0", Phase: "Running"},
			), nil
		}

		return "", nil
	}

	pod, err := newTestKubernetesBackend().getPodForServiceWith(run, serviceNeo4j)
	if err != nil {
		t.Fatalf("getPodForServiceWith failed: %v", err)
	}
	if pod == transient {
		t.Fatalf("pod = %q, want the deployment's own pod: this run's transient workload is not a replica of %s (FR-028)", pod, serviceNeo4j)
	}
	if pod != "infrahub-database-0" {
		t.Errorf("pod = %q, want infrahub-database-0", pod)
	}
}

// TestResolutionIssuesOneNamespaceListing pins T119's listing reduction to the
// questions the memo is sound for: where a service lives, and which pod is its.
// Both are settled for as long as the pods are, and the namespace-wide ownership
// query names no service at all — so six services asking it is one query, which
// is what it used to be six of.
//
// It is asserted through locateServiceWith rather than through the running check
// it used to be asserted through. That reading had the memo answering a *phase*,
// which is the one question it must not answer, and the loops that poll a phase
// could then never see it change (TestQuiesceConfirmationObservesPodsTerminating).
// The four label selectors per service stay either way: they name the service, so
// they are six different questions.
func TestResolutionIssuesOneNamespaceListing(t *testing.T) {
	backend := newTestKubernetesBackend()

	selectorQueries := 0
	listings := 0
	run := func(_ string, args ...string) (string, error) {
		if resourceOf(args) != "pods" {
			// The workload listings the location decision adds; not a pod
			// listing and not what this memo holds.
			return "", nil
		}
		if isServiceSelectorQuery(args) {
			selectorQueries++

			return "", nil
		}
		listings++

		return podListing(labelledPod{Name: "infrahub-infrahub-server-7d9f4b6c8-abcde", Phase: "Running"}), nil
	}

	for _, service := range appServicesStoppedForBackup {
		if _, err := backend.locateServiceWith(run, service); err != nil {
			t.Fatalf("locateServiceWith(%s) = %v, want the location determined", service, err)
		}
	}

	if listings != 1 {
		t.Errorf("namespace listings = %d, want 1 for %d location queries (it was one per service)", listings, len(appServicesStoppedForBackup))
	}

	wantSelectors := len(appServicesStoppedForBackup) * len(serviceLabelKeys)
	if selectorQueries != wantSelectors {
		t.Errorf("selector queries = %d, want %d: one per service per service-label key", selectorQueries, wantSelectors)
	}
}

// TestRunningCheckReadsAFreshNamespaceListing is the other half of the same
// split, at the level the bug lived: the running check must read the cluster
// every time it is asked.
//
// The listing is memoised for resolution, and the running check fell through to
// the very same helper — so the first answer was replayed for the rest of the
// run however long the caller waited. The scripted cluster below reports the pod
// Running once and gone afterwards, which is what a terminating pod looks like,
// and the second reading has to see it.
func TestRunningCheckReadsAFreshNamespaceListing(t *testing.T) {
	backend := newTestKubernetesBackend()

	listings := 0
	run := func(_ string, args ...string) (string, error) {
		if isServiceSelectorQuery(args) {
			return "", nil
		}
		listings++
		if listings == 1 {
			return podListing(labelledPod{Name: "infrahub-cache-0", Phase: "Running", Service: "cache"}), nil
		}

		return "", nil
	}

	first, err := backend.getPodStatusesWith(run, "cache")
	if err != nil {
		t.Fatalf("getPodStatusesWith() = %v, want the status determined", err)
	}
	if !slices.Contains(first, "Running") {
		t.Fatalf("first reading = %v, want it to hold Running: the scripted cluster reported the pod up", first)
	}

	second, err := backend.getPodStatusesWith(run, "cache")
	if err != nil {
		t.Fatalf("getPodStatusesWith() = %v on the second reading, want the status determined", err)
	}
	if slices.Contains(second, "Running") {
		t.Errorf("second reading = %v, want the pod gone: the memoised listing was replayed, so no wait can observe a pod terminating", second)
	}

	if listings != 2 {
		t.Errorf("namespace listings = %d, want 2: the running check reads the cluster each time it is asked", listings)
	}

	if len(backend.namespacePodListings) != 0 {
		t.Errorf("namespacePodListings = %v, want it untouched: the running check must not write the memo either, or the next resolution reads a phase it happened to catch",
			backend.namespacePodListings)
	}
}

// TestQuiesceConfirmationDoesNotReplayAnEarlierPollsAnswer is the production
// loop over the property above, and the reason it is worth a stubbed `kubectl`
// rather than a backend double: the double every other test in this file uses
// answers IsRunning from a map, so it never reaches a cache and cannot fail
// this.
//
// The order in RestoreBackup is wipeTransientData → stopAppContainers →
// confirmAppContainersQuiesced. `kubectl scale` returns as soon as the replica
// count is recorded, so a poll legitimately sees Running while the pod is still
// terminating. With the listing memoised, that answer was replayed for the rest
// of the run: the wait burned its whole window and the restore aborted with
// "still running 5m after it was stopped" — naming a pod that had terminated
// normally, with the data already wiped and the deployment at zero.
//
// The two waits below are the two halves of that, in the order the restore meets
// them, and nothing between them invalidates a cache because nothing between
// them scales anything. The first is what makes the second mean something: a
// wait that gave up because the pod was Running is a wait that observed the pod
// Running, so the second cannot pass on a cluster it never saw up. Neither turns
// on timing — a poll interval decides how many queries a window spends, not what
// any of them answers.
func TestQuiesceConfirmationDoesNotReplayAnEarlierPollsAnswer(t *testing.T) {
	// The pod exists for as long as this file does, and only the test writes
	// it. A stub that recorded its own calls would be asserting on whether a
	// subprocess may write to the test's temporary directory, which is a
	// property of the machine rather than of the wait.
	pod := filepath.Join(t.TempDir(), "cache-pod-running")
	if err := os.WriteFile(pod, []byte("Running\n"), 0o644); err != nil {
		t.Fatalf("declaring the cache pod = %v, want nil", err)
	}

	bin := t.TempDir()
	writeTerminatingPodKubectl(t, filepath.Join(bin, "kubectl"), pod)
	t.Setenv("PATH", bin)

	backend := NewKubernetesBackend(&Configuration{}, NewCommandExecutor())
	backend.namespace = "infrahub"
	iops := &InfrahubOps{config: &Configuration{}, executor: NewCommandExecutor(), backend: backend}

	err := iops.confirmAppContainersQuiescedWithin([]string{"cache"}, 100*time.Millisecond, 20*time.Millisecond)
	if err == nil {
		t.Fatal("confirmAppContainersQuiescedWithin() = nil with the cache pod still Running, want the wait to refuse: a restore must not write over a database something may still be writing to")
	}
	if !strings.Contains(err.Error(), "still running") {
		t.Fatalf("err = %v, want the wait's own refusal, so what follows is measured against a pod this run saw Running", err)
	}

	if err := os.Remove(pod); err != nil {
		t.Fatalf("terminating the cache pod = %v, want nil", err)
	}

	if err := iops.confirmAppContainersQuiescedWithin([]string{"cache"}, 2*time.Second, 20*time.Millisecond); err != nil {
		t.Fatalf("confirmAppContainersQuiescedWithin() = %v after the pod terminated, want nil: the wait was replaying an earlier poll's listing, so no amount of waiting could observe the pod going", err)
	}
}

// writeTerminatingPodKubectl writes a `kubectl` that reports one Running cache
// pod for as long as `pod` exists, and an empty namespace once it is gone.
//
// The two listing shapes are told apart by the output they ask for: only the
// ownership jsonpath reads `.metadata.labels`, while the label-selector queries
// ask for name and phase alone (see podListingArgsFor). Answering the selectors
// with nothing is what a Helm deployment does — the chart labels those pods with
// a service value the selectors do not carry — and it is what sends the running
// check to the namespace listing this test is about.
func writeTerminatingPodKubectl(t *testing.T, path, pod string) {
	t.Helper()

	script := fmt.Sprintf(`#!/bin/sh
case "$*" in
*labels*)
	if [ -f %q ]; then
		printf 'infrahub-cache-0;Running;;cache\n'
	fi
	;;
esac
exit 0
`, pod)

	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("writing the kubectl stub = %v, want nil", err)
	}
}

// TestNamespacePodListingIsDroppedByAScale is the memo's safety property: a
// caller that has just changed which pods exist must not read the listing from
// before it did. dropPodCaches is what Start and scaleServices call, and it is
// the only thing either of them does about a cache.
func TestNamespacePodListingIsDroppedByAScale(t *testing.T) {
	backend := newTestKubernetesBackend()
	backend.namespacePodListings[completedPodFieldSelector] = []labelledPod{{Name: "infrahub-cache-0", Phase: "Running"}}
	backend.podCache["cache"] = "infrahub-cache-0"

	backend.dropPodCaches()

	if len(backend.namespacePodListings) != 0 {
		t.Errorf("namespacePodListings = %v after a scale, want it dropped alongside podCache", backend.namespacePodListings)
	}
	if len(backend.podCache) != 0 {
		t.Errorf("podCache = %v after a scale, want the two cleared together", backend.podCache)
	}
	if len(backend.serviceLocations) != 0 {
		t.Error("scaling cleared serviceLocations; a scale changes which pods exist, not whether a database is one of them")
	}
}

// TestNamespacePodListingDoesNotMemoiseAFailure is FR-002 at the memo: a
// listing that failed says the cluster could not answer, not that the namespace
// holds no pods, so the next caller must be able to ask again.
func TestNamespacePodListingDoesNotMemoiseAFailure(t *testing.T) {
	backend := newTestKubernetesBackend()

	calls := 0
	run := func(_ string, _ ...string) (string, error) {
		calls++

		return "", fmt.Errorf("the server was unable to return a response")
	}

	for i := range 2 {
		if _, err := backend.namespacePodListing(run, ""); err == nil {
			t.Fatalf("namespacePodListing() call %d = nil, want the kubectl failure reported", i+1)
		}
	}

	if calls != 2 {
		t.Errorf("kubectl calls = %d, want 2: a failure is not an answer to memoise", calls)
	}
}
