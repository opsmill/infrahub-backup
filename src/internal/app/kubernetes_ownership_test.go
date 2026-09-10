package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
)

// ownershipLine renders one pod as podOwnershipJSONPath would: the name, the
// phase, the transient marker, then one field per serviceLabelKeys entry, empty
// where the pod carries no such label.
//
// The labels are read by key rather than positioned by the caller, so a pod
// that carries transientLabelMarker renders the field kubectl would render for
// it — which is what lets a test hand the resolver a transient pod without
// knowing the layout.
func ownershipLine(name, phase string, labels map[string]string) string {
	fields := []string{name, phase, labels[transientLabelMarker]}
	for _, key := range serviceLabelKeys {
		fields = append(fields, labels[key])
	}

	return strings.Join(fields, ";")
}

// ownershipListing renders a whole namespace listing from ownershipLine parts.
func ownershipListing(lines ...string) string {
	return strings.Join(lines, "\n") + "\n"
}

// workloadListing renders the `kubectl get <kind> -o json` payload
// listWorkloadsWith parses, with each workload's pod-template labels set.
func workloadListing(t *testing.T, workloads map[string]map[string]string) string {
	t.Helper()

	type item struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			Template struct {
				Metadata struct {
					Labels map[string]string `json:"labels"`
				} `json:"metadata"`
			} `json:"template"`
		} `json:"spec"`
	}

	payload := struct {
		Items []item `json:"items"`
	}{}
	for _, name := range slices.Sorted(maps.Keys(workloads)) {
		one := item{}
		one.Metadata.Name = name
		one.Spec.Template.Metadata.Labels = workloads[name]
		payload.Items = append(payload.Items, one)
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("failed to render the workload listing: %v", err)
	}

	return string(encoded)
}

// infrahubServerLabels is what the Helm chart puts on the server pods: the
// shortened service value that is the documented reason the name fallback
// exists, and the pod that establishes the deployment's release prefix.
var infrahubServerLabels = map[string]string{"infrahub/service": "server"}

// TestNameMatchesService_DeploymentShapes pins the name shapes a real
// deployment produces, one case per shape. Tightening ownership must not narrow
// the matcher itself: every name here resolves today and has to keep resolving.
func TestNameMatchesService_DeploymentShapes(t *testing.T) {
	tests := []struct {
		name    string
		podName string
		service string
		want    bool
	}{
		{"helm neo4j statefulset pod", "infrahub-database-0", "database", true},
		{"helm cache statefulset pod", "infrahub-cache-0", "cache", true},
		{"release-named message queue pod", "myrelease-message-queue-0", "message-queue", true},
		{"deployment pod with replicaset hash", "infrahub-server-7d9f4-abcde", "infrahub-server", true},
		{"task manager database pod", "task-manager-db-0", "task-manager-db", true},
		{"background service pod", "infrahub-task-manager-background-svc-6c9-x2k", "task-manager-background-svc", true},
		{"service name at the very start", "database-0", "database", true},
		{"letters shared but no token boundary", "mydatabasetool-0", "database", false},
		{"letters shared but no token boundary for cache", "mycachetool-0", "cache", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nameMatchesService(tt.podName, tt.service); got != tt.want {
				t.Errorf("nameMatchesService(%q, %q) = %t, want %t", tt.podName, tt.service, got, tt.want)
			}
		})
	}
}

// TestReleasePrefixOf covers the read that turns a self-declared resource into
// the deployment's naming.
func TestReleasePrefixOf(t *testing.T) {
	tests := []struct {
		name    string
		podName string
		service string
		want    string
	}{
		{"helm pod", "infrahub-cache-0", "cache", "infrahub"},
		{"shortened chart label", "infrahub-server-7d9f4-abcde", "server", "infrahub"},
		{"multi-segment release", "infrahub-prod-database-0", "database", "infrahub-prod"},
		{"name starts with the service", "cache-0", "cache", ""},
		{"service not carried at a boundary", "mycachetool-0", "cache", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := releasePrefixOf(tt.podName, tt.service); got != tt.want {
				t.Errorf("releasePrefixOf(%q, %q) = %q, want %q", tt.podName, tt.service, got, tt.want)
			}
		})
	}
}

// TestReleasePrefixes checks that only the deployment's own vocabulary
// contributes a prefix, so a co-hosted chart cannot lend its name to Infrahub.
func TestReleasePrefixes(t *testing.T) {
	prefixes := releasePrefixes(map[string]string{
		"infrahub-server-7d9f4-abcde": "server",
		"infrahub-database-0":         "database",
		"redis-cache-master-0":        "redis",   // not an Infrahub service value
		"grafana-abc":                 "grafana", // not an Infrahub service value
	})
	if !slices.Equal(prefixes, []string{"infrahub"}) {
		t.Errorf("releasePrefixes = %v, want [infrahub] only", prefixes)
	}

	if got := releasePrefixes(map[string]string{"redis-cache": "redis"}); len(got) != 0 {
		t.Errorf("releasePrefixes = %v, want none when nothing declares an Infrahub service", got)
	}
}

// helmChartLabels is what the chart actually puts on an Infrahub pod: the
// release name under `app`, and the service under `infrahub/service`. Both keys
// are in serviceLabelKeys and `app` is read first, which is the whole of T098.
var helmChartLabels = map[string]string{"app": "infrahub", "infrahub/service": "server"}

// TestDeclaredServiceIn_ReadsTheKeyThatNamesAService is T098: the declared
// service is the first key that names one, not the first key that is set.
//
// Reading the first present key returned the release name from every pod in a
// Helm namespace. Nothing then declared a service, so releasePrefixes found no
// prefix, and both decisions resting on that evidence failed quietly — the
// running check stopped nothing, and the location decision read an in-cluster
// database as external.
func TestDeclaredServiceIn_ReadsTheKeyThatNamesAService(t *testing.T) {
	if got := declaredServiceIn(helmChartLabels); got != "server" {
		t.Errorf("declaredServiceIn = %q, want server: `app: infrahub` is the release, not a service", got)
	}

	if got := declaredServiceIn(map[string]string{"app": "redis"}); got != "" {
		t.Errorf("declaredServiceIn = %q, want no declaration for a co-hosted chart's own label", got)
	}

	if got := declaredServiceIn(nil, map[string]string{"component": "task-manager-db"}); got != "task-manager-db" {
		t.Errorf("declaredServiceIn = %q, want task-manager-db from the later key", got)
	}
}

// TestParseLabelledPods_ReadsTheKeyThatNamesAService is the pod-listing half of
// the same read. The fields arrive in serviceLabelKeys order, so a listing that
// took the first non-empty field had exactly the masking above.
func TestParseLabelledPods_ReadsTheKeyThatNamesAService(t *testing.T) {
	pods := parseLabelledPods(ownershipListing(
		ownershipLine("infrahub-server-7d9f4-abcde", "Running", helmChartLabels),
		ownershipLine("redis-cache-master-0", "Running", map[string]string{"app": "redis"}),
	))
	if len(pods) != 2 {
		t.Fatalf("parseLabelledPods returned %d pods, want 2", len(pods))
	}
	if pods[0].Service != "server" {
		t.Errorf("pods[0].Service = %q, want server rather than the release name", pods[0].Service)
	}
	if pods[1].Service != "" {
		t.Errorf("pods[1].Service = %q, want no declaration from a co-hosted chart's label", pods[1].Service)
	}

	// The consequence, which is what the two decisions read: the release prefix
	// is recoverable from a chart-labelled namespace again.
	if got := releasePrefixes(declaredPodServices(pods)); !slices.Equal(got, []string{"infrahub"}) {
		t.Errorf("releasePrefixes = %v, want [infrahub] from the chart-labelled server pod", got)
	}
}

// TestGetPodStatusesWith_HelmChartLabels is T098 where it costs something: a
// namespace labelled the way the chart labels it, with a foreign pod beside the
// deployment's own. With `app: <release>` masking the service label, no pod
// declared anything, so the prefix set was empty, the unanchored rule applied
// and `redis-cache-master-0` was reported as Infrahub's `cache`.
func TestGetPodStatusesWith_HelmChartLabels(t *testing.T) {
	listing := ownershipListing(
		ownershipLine("infrahub-server-7d9f4-abcde", "Running", helmChartLabels),
		ownershipLine("infrahub-cache-0", "Running", map[string]string{"app": "infrahub"}),
		ownershipLine("redis-cache-master-0", "Running", map[string]string{"app": "redis"}),
	)
	run := func(name string, args ...string) (string, error) {
		if isServiceSelectorQuery(args) {
			return "", nil
		}

		return listing, nil
	}

	statuses, err := newTestKubernetesBackend().getPodStatusesWith(run, "cache")
	if err != nil {
		t.Fatalf("getPodStatusesWith failed: %v", err)
	}
	if !slices.Equal(statuses, []string{"Running"}) {
		t.Errorf("statuses = %v, want one Running: infrahub-cache-0 only, not the co-hosted redis", statuses)
	}
}

// TestNameIsDeploymentOwned is the rule the destructive decision rests on: the
// shape of the name is necessary but not sufficient, and the release prefix is
// what says whose workload it is.
func TestNameIsDeploymentOwned(t *testing.T) {
	tests := []struct {
		name     string
		prefixes []string
		resource string
		service  string
		want     bool
	}{
		{"the deployment's own cache", []string{"infrahub"}, "infrahub-cache-0", "cache", true},
		{"the deployment's own message queue", []string{"myrelease"}, "myrelease-message-queue-0", "message-queue", true},
		{"a co-hosted redis", []string{"infrahub"}, "redis-cache", "cache", false},
		{"another tool's cache statefulset", []string{"infrahub"}, "othertool-cache-0", "cache", false},
		{"a co-hosted rabbitmq", []string{"infrahub"}, "rabbitmq-message-queue", "message-queue", false},
		{"a co-hosted nats", []string{"infrahub"}, "nats-message-queue", "message-queue", false},
		{"letters shared but no token boundary", []string{"infrahub"}, "mycachetool-0", "cache", false},
		{"the service's own bare name needs no prefix", []string{"infrahub"}, "task-manager-db-1", "task-manager-db", true},
		{"the service's own workload name needs no prefix", []string{"infrahub"}, "cache", "cache", true},
		{"a different component named after the service", []string{"infrahub"}, "task-manager-db-exporter", "task-manager-db", false},
		{"no ownership evidence is not this rule's answer", nil, "redis-cache", "cache", false},

		// T150, direction A: the release prefix says whose, and the word after
		// the service says it is not the database. Claiming it read an external
		// RDS as internal and ran a restore into a metrics sidecar.
		{"a metrics sidecar under the deployment's own prefix", []string{"infrahub"}, "infrahub-task-manager-db-exporter", "task-manager-db", false},
		{"the sidecar's Deployment pod under the deployment's own prefix", []string{"infrahub"}, "infrahub-task-manager-db-exporter-6b8f7c9d4-lm2xq", "task-manager-db", false},
		{"a cache exporter under the deployment's own prefix", []string{"infrahub"}, "infrahub-cache-exporter", "cache", false},
		{"a backup CronJob's pod under the deployment's own prefix", []string{"infrahub"}, "infrahub-database-backup-28123456-x9v2t", "database", false},
		{"the database is not the task manager", []string{"infrahub"}, "infrahub-task-manager-db-0", "task-manager", false},
		{"the deployment's own database pod", []string{"infrahub"}, "infrahub-task-manager-db-1", "task-manager-db", true},
		{"the deployment's own database workload", []string{"infrahub"}, "infrahub-task-manager-db", "task-manager-db", true},
		{"the deployment's own server pod", []string{"infrahub"}, "infrahub-server-7d9f4b6c8-abcde", "infrahub-server", true},

		// T150, direction B, by design: a prefix nothing declared is another
		// product's — the `postgres-database-0` shape — and that reading does
		// not turn on whether a release happens to be declared beside it.
		{"a database under a prefix nothing declared, beside a declared release", []string{"infrahub"}, "pg-task-manager-db-1", "task-manager-db", false},
		{"a database under a prefix nothing declared, with nothing declared", nil, "pg-task-manager-db-1", "task-manager-db", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := nameIsDeploymentOwned(tt.prefixes, tt.resource, tt.service)
			if got != tt.want {
				t.Errorf("nameIsDeploymentOwned(%v, %q, %q) = %t, want %t", tt.prefixes, tt.resource, tt.service, got, tt.want)
			}
		})
	}
}

// TestGetPodStatusesWith_ForeignWorkloads is the running check that decides
// whether stopAppContainers stops and scales a workload to zero. The shipped
// `strings.Contains(name, service)` reported every foreign pod below as
// Infrahub's `cache` or `message-queue`.
func TestGetPodStatusesWith_ForeignWorkloads(t *testing.T) {
	// Label selectors answer and match nothing, which is what a deployment
	// whose pods are labelled differently looks like; the namespace listing is
	// then what decides.
	listingRunner := func(listing string) podRunner {
		return func(name string, args ...string) (string, error) {
			if isServiceSelectorQuery(args) {
				return "", nil
			}

			return listing, nil
		}
	}

	foreign := []struct {
		name    string
		podName string
		service string
	}{
		{"a co-hosted redis", "redis-cache", "cache"},
		{"another tool's cache statefulset", "othertool-cache-0", "cache"},
		{"a co-hosted rabbitmq", "rabbitmq-message-queue", "message-queue"},
	}

	for _, tt := range foreign {
		t.Run(tt.name+" is not reported as "+tt.service, func(t *testing.T) {
			listing := ownershipListing(
				ownershipLine("infrahub-server-7d9f4-abcde", "Running", infrahubServerLabels),
				ownershipLine(tt.podName, "Running", nil),
			)
			statuses, err := newTestKubernetesBackend().getPodStatusesWith(listingRunner(listing), tt.service)
			if err != nil {
				t.Fatalf("getPodStatusesWith failed: %v", err)
			}
			if len(statuses) != 0 {
				t.Errorf("statuses = %v, want none: %s is not this deployment's %s and must not be stopped", statuses, tt.podName, tt.service)
			}
		})
	}

	legitimate := []struct {
		name    string
		podName string
		service string
	}{
		{"the deployment's own cache", "infrahub-cache-0", "cache"},
		{"the deployment's own message queue", "infrahub-message-queue-0", "message-queue"},
		{"the deployment's own neo4j", "infrahub-database-0", "database"},
	}

	for _, tt := range legitimate {
		t.Run(tt.name+" is still reported as "+tt.service, func(t *testing.T) {
			listing := ownershipListing(
				ownershipLine("infrahub-server-7d9f4-abcde", "Running", infrahubServerLabels),
				ownershipLine("redis-cache", "Running", nil),
				ownershipLine(tt.podName, "Running", nil),
			)
			statuses, err := newTestKubernetesBackend().getPodStatusesWith(listingRunner(listing), tt.service)
			if err != nil {
				t.Fatalf("getPodStatusesWith failed: %v", err)
			}
			if !slices.Equal(statuses, []string{"Running"}) {
				t.Errorf("statuses = %v, want [Running] for %s", statuses, tt.podName)
			}
		})
	}

	t.Run("a CloudNativePG pod resolves on the label it declares", func(t *testing.T) {
		// The cluster's pods are named after the cluster, not after the
		// service, so they never resolved by name — the label is what finds
		// them, including the chart's shortened form.
		listing := ownershipListing(
			ownershipLine("infrahub-server-7d9f4-abcde", "Running", infrahubServerLabels),
			ownershipLine("pg-cluster-1", "Running", map[string]string{"app.kubernetes.io/component": "task-manager-db"}),
		)
		statuses, err := newTestKubernetesBackend().getPodStatusesWith(listingRunner(listing), "task-manager-db")
		if err != nil {
			t.Fatalf("getPodStatusesWith failed: %v", err)
		}
		if !slices.Equal(statuses, []string{"Running"}) {
			t.Errorf("statuses = %v, want [Running] for pg-cluster-1", statuses)
		}
	})

	t.Run("the chart's shortened service label still resolves", func(t *testing.T) {
		listing := ownershipListing(ownershipLine("infrahub-server-7d9f4-abcde", "Running", infrahubServerLabels))
		statuses, err := newTestKubernetesBackend().getPodStatusesWith(listingRunner(listing), "infrahub-server")
		if err != nil {
			t.Fatalf("getPodStatusesWith failed: %v", err)
		}
		if !slices.Equal(statuses, []string{"Running"}) {
			t.Errorf("statuses = %v, want [Running] for the labelled server pod", statuses)
		}
	})

	t.Run("with no ownership evidence the shipped name rule is kept", func(t *testing.T) {
		// Nothing in the namespace carries a service label, so there is no
		// prefix to be strict with. Narrowing here would trade a destructive
		// defect for a silently skipped stop on a deployment that works today.
		listing := ownershipListing(ownershipLine("infrahub-cache-0", "Running", nil))
		statuses, err := newTestKubernetesBackend().getPodStatusesWith(listingRunner(listing), "cache")
		if err != nil {
			t.Fatalf("getPodStatusesWith failed: %v", err)
		}
		if !slices.Equal(statuses, []string{"Running"}) {
			t.Errorf("statuses = %v, want [Running]", statuses)
		}
	})
}

// TestGetPodStatusesWith_QueryFailures separates "queried fine, nothing
// matched" from "the query failed". Only the first is an answer; the second
// must not become a stop.
func TestGetPodStatusesWith_QueryFailures(t *testing.T) {
	t.Run("every kubectl call fails, so the status is not determined", func(t *testing.T) {
		run := func(name string, args ...string) (string, error) {
			return "", fmt.Errorf("Unable to connect to the server: dial tcp: i/o timeout")
		}
		statuses, err := newTestKubernetesBackend().getPodStatusesWith(run, "cache")
		if err == nil {
			t.Fatal("getPodStatusesWith reported a status despite every kubectl call failing, want error")
		}
		if len(statuses) != 0 {
			t.Errorf("statuses = %v, want none", statuses)
		}
		if !strings.Contains(err.Error(), "failed to determine whether cache is running") {
			t.Errorf("err = %v, want the undetermined-status failure", err)
		}
	})

	// T097, in the form T122 leaves it in. The failure T097 fixed was a
	// throttled label-selector query turning a determinable status into an
	// error, on the one topology that matters: podSelectors queries
	// `<key>=infrahub-server`, the chart labels those pods
	// `infrahub/service=server`, so on a Helm deployment the namespace listing
	// was already the only thing that ever matched `infrahub-server`, and
	// refusing to read it gave a capture taken with writers still running.
	//
	// There is now no selector query to be throttled: the listing carries the
	// fields those selectors selected on, so the matches are read out of it
	// (podsServiceSelectorsWouldMatch). The runner below fails any service
	// selector query, so a status that is answered is a status answered without
	// issuing one — and the call is counted, because "it did not fail" and "it
	// was never made" are different facts and only the second is what T122 did.
	t.Run("no selector query is issued, so no selector failure can arise", func(t *testing.T) {
		selectorQueries := 0
		run := func(name string, args ...string) (string, error) {
			if isServiceSelectorQuery(args) {
				selectorQueries++

				return "", fmt.Errorf("exit status 1")
			}

			return ownershipListing(ownershipLine("infrahub-cache-0", "Running", nil)), nil
		}
		statuses, err := newTestKubernetesBackend().getPodStatusesWith(run, "cache")
		if err != nil {
			t.Fatalf("getPodStatusesWith failed despite the namespace listing answering: %v", err)
		}
		if !slices.Equal(statuses, []string{"Running"}) {
			t.Errorf("statuses = %v, want [Running] from the listing", statuses)
		}
		if selectorQueries != 0 {
			t.Errorf("selector queries = %d, want none: the listing carries what they selected on, so asking the cluster again is a call that can only fail", selectorQueries)
		}
	})

	t.Run("the one listing is the only query that can leave the status undetermined", func(t *testing.T) {
		// Nothing establishes whether the workload is running, so nothing may
		// stop it (FR-002) and stopAppContainers fails the run
		// (TestStopAppContainers_UndeterminedStatusFailsTheRun). The count is
		// the other half of the claim: there is one query left, so there is one
		// way for the question to go unanswered.
		calls := 0
		run := func(name string, args ...string) (string, error) {
			calls++

			return "", fmt.Errorf(`pods is forbidden: User "system:serviceaccount:infrahub:backup" cannot list resource "pods"`)
		}
		_, err := newTestKubernetesBackend().getPodStatusesWith(run, "cache")
		if err == nil {
			t.Fatal("getPodStatusesWith answered despite the namespace listing failing, want error")
		}
		if !strings.Contains(err.Error(), "failed to determine whether cache is running") {
			t.Errorf("err = %v, want the undetermined-status failure", err)
		}
		if !strings.Contains(err.Error(), "cannot list resource") {
			t.Errorf("err = %v, want the cluster's own refusal carried through, so an operator can tell a missing RBAC verb from throttling", err)
		}
		if calls != 1 {
			t.Errorf("kubectl calls = %d, want 1: the running check asks the namespace once", calls)
		}
	})

	t.Run("the namespace listing failing is an error, not a not-running answer", func(t *testing.T) {
		run := func(name string, args ...string) (string, error) {
			if isServiceSelectorQuery(args) {
				return "", nil
			}

			return "", fmt.Errorf(`pods is forbidden: User "system:serviceaccount:infrahub:backup" cannot list resource "pods"`)
		}
		_, err := newTestKubernetesBackend().getPodStatusesWith(run, "cache")
		if err == nil {
			t.Fatal("getPodStatusesWith succeeded despite being unable to list pods, want error")
		}
		if !strings.Contains(err.Error(), "failed to list pods") {
			t.Errorf("err = %v, want the wrapped kubectl failure", err)
		}
	})

	t.Run("every query answers and nothing matches is a real not-running", func(t *testing.T) {
		run := func(name string, args ...string) (string, error) { return "", nil }
		statuses, err := newTestKubernetesBackend().getPodStatusesWith(run, "cache")
		if err != nil {
			t.Fatalf("getPodStatusesWith failed: %v", err)
		}
		if len(statuses) != 0 {
			t.Errorf("statuses = %v, want none", statuses)
		}
	})
}

// TestRunningCheckAnswersTheSelectorsFromTheListing is T122. The running check
// used to issue four `<key>=<service>` queries and then, only if none of them
// matched anything, one namespace listing. The listing carries every field
// those selectors selected on, so the matches are now read out of it and the
// listing is the only call.
//
// The subtests below are the three properties that had to survive the removal,
// each of which is a real defect if it does not: the selector match reproduced
// exactly rather than replaced by the broader ownership rule, the phases still
// reported for a pod the selectors found, and FR-028's exclusion holding on the
// new path a selector-shaped match takes.
func TestRunningCheckAnswersTheSelectorsFromTheListing(t *testing.T) {
	cacheLabels := map[string]string{serviceLabelKeys[0]: "cache"}

	t.Run("one kubectl call answers the running check", func(t *testing.T) {
		calls := [][]string{}
		run := func(_ string, args ...string) (string, error) {
			calls = append(calls, args)

			return ownershipListing(ownershipLine("infrahub-cache-0", "Running", cacheLabels)), nil
		}

		statuses, err := newTestKubernetesBackend().getPodStatusesWith(run, "cache")
		if err != nil {
			t.Fatalf("getPodStatusesWith failed: %v", err)
		}
		if !slices.Equal(statuses, []string{"Running"}) {
			t.Errorf("statuses = %v, want [Running]", statuses)
		}
		if len(calls) != 1 {
			t.Errorf("kubectl calls = %d, want 1: %v", len(calls), calls)
		}
		for _, args := range calls {
			if isServiceSelectorQuery(args) {
				t.Errorf("call %v selects on a service label, want the namespace listing alone: the listing already carries those fields", args)
			}
		}
	})

	t.Run("a whole stop's worth of status queries is one call per service", func(t *testing.T) {
		// This is the win the removal is for. stopAppContainers asks about six
		// services, and each of the quiesce and restart waits asks about them
		// again on every poll.
		calls := 0
		run := func(_ string, args ...string) (string, error) {
			calls++

			return ownershipListing(ownershipLine("infrahub-cache-0", "Running", cacheLabels)), nil
		}

		backend := newTestKubernetesBackend()
		for _, service := range appServicesStoppedForBackup {
			if _, err := backend.getPodStatusesWith(run, service); err != nil {
				t.Fatalf("getPodStatusesWith(%s) = %v, want the status determined", service, err)
			}
		}

		if calls != len(appServicesStoppedForBackup) {
			t.Errorf("kubectl calls = %d for %d services, want one apiece: it was five apiece", calls, len(appServicesStoppedForBackup))
		}
	})

	t.Run("a pod under any of the selector keys still answers", func(t *testing.T) {
		// Each of the four keys was its own query, so each of them still has to
		// find the pod — including the ones after the first.
		for _, key := range serviceLabelKeys {
			listing := ownershipListing(ownershipLine("cache-0", "Running", map[string]string{key: "cache"}))
			statuses, err := newTestKubernetesBackend().getPodStatusesWith(namespaceListingRunner(listing), "cache")
			if err != nil {
				t.Fatalf("getPodStatusesWith failed for %s: %v", key, err)
			}
			if !slices.Equal(statuses, []string{"Running"}) {
				t.Errorf("statuses = %v for a pod labelled %s=cache, want [Running]: that key was one of the four queries", statuses, key)
			}
		}
	})

	t.Run("a claimed-but-unlabelled pod is not the labelled pod's status", func(t *testing.T) {
		// The property the naive removal breaks. `infrahub-cache-1` is claimed
		// by the ownership rule — it carries a release prefix this deployment
		// declared and an ordinal after the service — but it carries no label,
		// so no selector ever returned it. Reading the whole listing through
		// the ownership rule would report the service as Running from it while
		// the pod the selectors match is Pending, and stopAppContainers stops
		// and scales to zero on that.
		listing := ownershipListing(
			ownershipLine("infrahub-server-7d9f4-abcde", "Running", helmChartLabels),
			ownershipLine("infrahub-cache-0", "Pending", cacheLabels),
			ownershipLine("infrahub-cache-1", "Running", nil),
		)

		statuses, err := newTestKubernetesBackend().getPodStatusesWith(namespaceListingRunner(listing), "cache")
		if err != nil {
			t.Fatalf("getPodStatusesWith failed: %v", err)
		}
		if !slices.Equal(statuses, []string{"Pending"}) {
			t.Errorf("statuses = %v, want [Pending] alone: only the pod a selector matched is the service's status", statuses)
		}
		if slices.Contains(statuses, "Running") {
			t.Error("the running check answered Running from a pod no selector would have returned; IsRunning is what stopAppContainers stops and scales a workload to zero on")
		}

		// The premise, asserted rather than assumed: the unlabelled pod *is*
		// claimed, so the assertion above is about the selector tier and not
		// about a pod the ownership rule would have refused anyway.
		if !deploymentClaimsResource([]string{"infrahub"}, "infrahub-cache-1", "", "cache") {
			t.Fatal("infrahub-cache-1 is not claimed by the ownership rule, so this fixture does not reproduce the hazard")
		}
	})

	t.Run("the selectors are read in order rather than as a union", func(t *testing.T) {
		// The loop of queries returned on the first selector with a non-empty
		// result; it never accumulated across all four. That is reproduced
		// rather than improved on: a union differs exactly where two keys name
		// different pods, and it differs in the direction of IsRunning
		// answering true more often — which is a change to the running check,
		// not a reproduction of it.
		listing := ownershipListing(
			ownershipLine("infrahub-cache-0", "Pending", map[string]string{serviceLabelKeys[0]: "cache"}),
			ownershipLine("othertool-cache-0", "Running", map[string]string{serviceLabelKeys[2]: "cache"}),
		)

		statuses, err := newTestKubernetesBackend().getPodStatusesWith(namespaceListingRunner(listing), "cache")
		if err != nil {
			t.Fatalf("getPodStatusesWith failed: %v", err)
		}
		if !slices.Equal(statuses, []string{"Pending"}) {
			t.Errorf("statuses = %v, want [Pending]: the first key that matched anything was the answer the queries gave", statuses)
		}
	})

	t.Run("this run's own workload is not a selector match either", func(t *testing.T) {
		// FR-028 on the path the reproduction creates. A transient pod
		// carrying one of the selector keys would be *returned* by that
		// selector, so the exclusion has to hold before the match is read —
		// which is where withoutTransientPods sits, inside the listing.
		name := "infrahub-cache-xdb-neo4j-capture-a1b2c3d4"
		if isTransientObjectName(name) {
			t.Fatalf("pod %q carries today's prefix, so this would pass on the name filter alone", name)
		}

		listing := ownershipListing(ownershipLine(name, "Running", map[string]string{
			transientLabelMarker: "true",
			serviceLabelKeys[0]:  "cache",
		}))

		statuses, err := newTestKubernetesBackend().getPodStatusesWith(namespaceListingRunner(listing), "cache")
		if err != nil {
			t.Fatalf("getPodStatusesWith failed: %v", err)
		}
		if len(statuses) != 0 {
			t.Errorf("statuses = %v, want none: this run's own workload is not the service's status, whatever it is labelled (FR-028)", statuses)
		}
	})
}

// TestParseLabelledPodsKeepsTheSelectorFields is what makes the reproduction
// above possible, and it is a different reading from the declared service: the
// chart labels every pod `app: <release>`, which no selector for a service
// matches and which declaredServiceValue refuses as a declaration — but a
// listing that dropped the raw value could not tell a selector for it from a
// selector for anything else.
func TestParseLabelledPodsKeepsTheSelectorFields(t *testing.T) {
	pods := parseLabelledPods(ownershipListing(
		ownershipLine("infrahub-server-7d9f4-abcde", "Running", helmChartLabels),
		ownershipLine("infrahub-cache-0", "Running", nil),
	))
	if len(pods) != 2 {
		t.Fatalf("parseLabelledPods returned %d pods, want 2", len(pods))
	}

	if !maps.Equal(pods[0].Labels, helmChartLabels) {
		t.Errorf("pods[0].Labels = %v, want %v: the fields a selector is matched against", pods[0].Labels, helmChartLabels)
	}
	if pods[0].Service != "server" {
		t.Errorf("pods[0].Service = %q, want server: the release name in `app` is not a declaration", pods[0].Service)
	}
	if len(pods[1].Labels) != 0 {
		t.Errorf("pods[1].Labels = %v, want none: a pod carrying no service label carries no evidence, and an empty field is not a label", pods[1].Labels)
	}

	// The consequence: the release label is matchable as a selector even though
	// it declares nothing. `app=infrahub` is not a selector this tool builds
	// for a service, and that is precisely why the two readings cannot be one.
	if !selectorMatchesLabels("app=infrahub", pods[0].Labels) {
		t.Error("the raw `app` value was lost, so a selector could not be reproduced from the listing")
	}
}

// namespaceListingRunner answers every kubectl call with the supplied namespace listing.
// The running check issues one call, so there is nothing else to answer.
func namespaceListingRunner(listing string) podRunner {
	return func(_ string, _ ...string) (string, error) {
		return listing, nil
	}
}

// TestFindWorkloadResourceWith is the resolution every scale goes through, so
// each case below is a workload that would have been scaled to zero.
func TestFindWorkloadResourceWith(t *testing.T) {
	// Label selector queries answer and match nothing; the JSON listing decides.
	listingRunner := func(listing string) podRunner {
		return func(name string, args ...string) (string, error) {
			if isServiceSelectorQuery(args) {
				return "", nil
			}

			return listing, nil
		}
	}

	t.Run("a foreign workload is not resolved as the service", func(t *testing.T) {
		listing := workloadListing(t, map[string]map[string]string{
			"infrahub-server": infrahubServerLabels,
			"redis-cache":     {"app.kubernetes.io/name": "redis"},
			"othertool-cache": nil,
		})
		kind, resource, err := newTestKubernetesBackend().findWorkloadResourceWith(listingRunner(listing), "cache")
		if err == nil {
			t.Fatalf("findWorkloadResourceWith resolved %s/%s for cache, want no workload found", kind, resource)
		}
		if !strings.Contains(err.Error(), "no workloads found for cache") {
			t.Errorf("err = %v, want the answered-but-unmatched failure", err)
		}
	})

	t.Run("the deployment's own workload is still resolved by name", func(t *testing.T) {
		listing := workloadListing(t, map[string]map[string]string{
			"infrahub-server": infrahubServerLabels,
			"infrahub-cache":  nil,
			"redis-cache":     nil,
		})
		kind, resource, err := newTestKubernetesBackend().findWorkloadResourceWith(listingRunner(listing), "cache")
		if err != nil {
			t.Fatalf("findWorkloadResourceWith failed: %v", err)
		}
		if resource != "infrahub-cache" {
			t.Errorf("resource = %q (%s), want infrahub-cache", resource, kind)
		}
	})

	t.Run("the chart's shortened service label resolves the server workload", func(t *testing.T) {
		listing := workloadListing(t, map[string]map[string]string{"infrahub-server": infrahubServerLabels})
		_, resource, err := newTestKubernetesBackend().findWorkloadResourceWith(listingRunner(listing), "infrahub-server")
		if err != nil {
			t.Fatalf("findWorkloadResourceWith failed: %v", err)
		}
		if resource != "infrahub-server" {
			t.Errorf("resource = %q, want infrahub-server", resource)
		}
	})

	t.Run("a kubectl failure is reported as a failure, not as no workload", func(t *testing.T) {
		// The shipped `if err != nil || output == ""` read this as an empty
		// result and fell through to matching names.
		run := func(name string, args ...string) (string, error) {
			return "", fmt.Errorf("Unable to connect to the server: dial tcp: i/o timeout")
		}
		_, _, err := newTestKubernetesBackend().findWorkloadResourceWith(run, "cache")
		if err == nil {
			t.Fatal("findWorkloadResourceWith succeeded despite every kubectl call failing, want error")
		}
		if !strings.Contains(err.Error(), "failed to resolve a workload for cache") {
			t.Errorf("err = %v, want the query-failure branch rather than the unmatched branch", err)
		}
		if strings.Contains(err.Error(), "no workloads found") {
			t.Errorf("a kubectl failure was reported as an empty namespace: %v", err)
		}
	})
}

// TestStopAppContainers_UndeterminedStatusFailsTheRun is the caller's half of
// the same rule, and T097's other half. A status the run could not determine
// leaves the deployment alone — stopping on a guess is what took a foreign
// workload down — and it also stops the run, because every caller of
// stopAppContainers is about to do something that requires the services to be
// down. Warning and carrying on gave a cold `neo4j-admin dump` taken with
// writers still attached, reported as a successful backup.
func TestStopAppContainers_UndeterminedStatusFailsTheRun(t *testing.T) {
	backend := &stopRecordingBackend{runningErr: fmt.Errorf("Unable to connect to the server: dial tcp: i/o timeout")}
	iops := &InfrahubOps{config: &Configuration{}, executor: NewCommandExecutor(), backend: backend}

	stopped, err := iops.stopAppContainers()
	if err == nil {
		t.Fatal("stopAppContainers succeeded with a status it could not determine, want the run to fail")
	}
	if !strings.Contains(err.Error(), "cannot determine whether infrahub-server is running") {
		t.Errorf("err = %v, want the service the query could not answer for named", err)
	}
	if len(stopped) != 0 {
		t.Errorf("stopped = %v, want none", stopped)
	}
	if len(backend.stopCalls) != 0 {
		t.Errorf("Stop was called for %v despite an undetermined status", backend.stopCalls)
	}
}

// stopRecordingBackend answers the running check with runningErr (or running)
// and records every Stop it is asked for.
type stopRecordingBackend struct {
	bareBackend
	running    bool
	runningErr error
	stopCalls  []string
}

func (b *stopRecordingBackend) IsRunning(service string) (bool, error) {
	return b.running, b.runningErr
}

func (b *stopRecordingBackend) Stop(services ...string) error {
	b.stopCalls = append(b.stopCalls, services...)

	return nil
}

// TestPodPhaseIsTerminalIsOnePredicate pins the consolidation T082 made: the
// reaper and the resolver read a pod's phase through the same function.
//
// They did not. The reaper carried its own `strings.EqualFold(phase, "Failed")`
// with no trimming, while the resolver's trimmed — and the reaper's answer
// decides whether another run's pod is deleted. The divergence was latent
// rather than live, because parseTransientCandidates happens to trim the field
// before handing it over; it was one parser change away from being real, on the
// path where being wrong deletes a live peer's capture.
func TestPodPhaseIsTerminalIsOnePredicate(t *testing.T) {
	terminal := []string{"Succeeded", "Failed", " Succeeded", "Failed\t", " failed ", "SUCCEEDED"}
	for _, phase := range terminal {
		if !podPhaseIsTerminal(phase) {
			t.Errorf("podPhaseIsTerminal(%q) = false, want true", phase)
		}
	}

	live := []string{"Running", "Pending", " Running ", "Unknown", ""}
	for _, phase := range live {
		if podPhaseIsTerminal(phase) {
			t.Errorf("podPhaseIsTerminal(%q) = true, want false: nothing here says the pod has finished", phase)
		}
	}
}

// TestRunningCheckAndLocationDecisionAgree is the symptom T082 exists to
// remove. The running check and the internal-versus-external decision read the
// same namespace for the same evidence, and expressed the test in two places.
// On a labelled deployment — which is every deployment the chart produces —
// they must not be able to disagree: a namespace cannot both be running a
// database and not be hosting it.
func TestRunningCheckAndLocationDecisionAgree(t *testing.T) {
	// A real deployment, labelled, plus an unrelated pod whose name names the
	// database service. The database itself lives outside the cluster.
	listing := ownershipListing(
		serverPod,
		ownershipLine("postgres-database-0", "Running", nil),
	)
	run := labelBlindNamespace(t, listing, nil)

	location, err := newTestKubernetesBackend().locateServiceWith(run, serviceNeo4j)
	if err != nil {
		t.Fatalf("locateServiceWith failed: %v", err)
	}
	statuses, err := newTestKubernetesBackend().getPodStatusesWith(run, serviceNeo4j)
	if err != nil {
		t.Fatalf("getPodStatusesWith failed: %v", err)
	}

	if location != EndpointLocationExternal {
		t.Errorf("location = %q, want %q: postgres-database-0 carries no prefix this deployment declared", location, EndpointLocationExternal)
	}
	if len(statuses) != 0 {
		t.Errorf("statuses = %v, want none: the same pod that is not this deployment's database cannot be this deployment's database running", statuses)
	}
}

// TestNoDatabaseIsEverAskedWhetherItIsRunning is why the two decisions above
// are allowed to differ on an unanchored name at all (see
// deploymentClaimsResource). The running check is asked only about the services
// a run stops, and a database is not one of them — so the one case in which the
// two rules give different answers is a case neither pair of callers can reach.
func TestNoDatabaseIsEverAskedWhetherItIsRunning(t *testing.T) {
	for _, service := range appServicesStoppedForBackup {
		if service == serviceNeo4j || service == serviceTaskManagerDB {
			t.Errorf("appServicesStoppedForBackup contains %s: a database is now both stopped and located, so the running check and the location decision must agree on an unanchored name", service)
		}
	}
}

// TestUnanchoredNamePolicyFor pins the one policy choice that is made from the
// service rather than at the call site. A workload resolved here is scaled, and
// the two kinds of service disagree about what a bare name may authorise.
func TestUnanchoredNamePolicyFor(t *testing.T) {
	for _, service := range []string{serviceNeo4j, serviceTaskManagerDB} {
		if got := unanchoredNamePolicyFor(service); got != unanchoredNameIsNotEvidence {
			t.Errorf("unanchoredNamePolicyFor(%q) = %v, want the strict policy: no database may be scaled on a name alone", service, got)
		}
	}
	for _, service := range appServicesStoppedForBackup {
		if got := unanchoredNamePolicyFor(service); got != unanchoredNameClaims {
			t.Errorf("unanchoredNamePolicyFor(%q) = %v, want the shipped policy: narrowing here leaves a workload running through an offline capture", service, got)
		}
	}
}

// TestFindWorkloadResourceWithRefusesAForeignDatabase is the restore-side half
// of the same rule. restorePostgreSQL starts `task-manager-db`, which resolves
// through here and is then scaled; in a namespace that declares no service
// labels, an unrelated `task-manager-db-exporter` Deployment satisfied the bare
// token-boundary match and was scaled instead.
func TestFindWorkloadResourceWithRefusesAForeignDatabase(t *testing.T) {
	listingRunner := func(listing string) podRunner {
		return func(_ string, args ...string) (string, error) {
			if isServiceSelectorQuery(args) {
				return "", nil
			}

			return listing, nil
		}
	}

	t.Run("an unanchored name does not authorise scaling a database", func(t *testing.T) {
		listing := workloadListing(t, map[string]map[string]string{
			"task-manager-db-exporter": nil,
		})
		kind, resource, err := newTestKubernetesBackend().findWorkloadResourceWith(listingRunner(listing), serviceTaskManagerDB)
		if err == nil {
			t.Fatalf("findWorkloadResourceWith resolved %s/%s, want no workload: nothing in this namespace claims it", kind, resource)
		}
		if !strings.Contains(err.Error(), "no workloads found for "+serviceTaskManagerDB) {
			t.Errorf("err = %v, want the answered-but-unmatched failure so restorePostgreSQL can skip the start", err)
		}
	})

	t.Run("the deployment's own database workload is still resolved", func(t *testing.T) {
		listing := workloadListing(t, map[string]map[string]string{
			"infrahub-server":          infrahubServerLabels,
			"infrahub-task-manager-db": nil,
			"task-manager-db-exporter": nil,
		})
		_, resource, err := newTestKubernetesBackend().findWorkloadResourceWith(listingRunner(listing), serviceTaskManagerDB)
		if err != nil {
			t.Fatalf("findWorkloadResourceWith failed: %v", err)
		}
		if resource != "infrahub-task-manager-db" {
			t.Errorf("resource = %q, want infrahub-task-manager-db", resource)
		}
	})

	t.Run("an app service still resolves from a name nothing anchors", func(t *testing.T) {
		listing := workloadListing(t, map[string]map[string]string{"cache": nil})
		_, resource, err := newTestKubernetesBackend().findWorkloadResourceWith(listingRunner(listing), "cache")
		if err != nil {
			t.Fatalf("findWorkloadResourceWith failed: %v", err)
		}
		if resource != "cache" {
			t.Errorf("resource = %q, want cache: narrowing an app service here is not what this fixes", resource)
		}
	})
}

// TestPodResolutionAnswersTheOwnershipQuestion is F3: the two pod resolvers used
// a bare name match while the location decision answered the same question
// through deploymentClaimsResource, so a co-hosted product's pod was refused by
// one and accepted by the other. The pods resolved here are where
// ensureNeo4jDiscovery and ensurePostgresDiscovery read the endpoint a restore
// later writes into.
func TestPodResolutionAnswersTheOwnershipQuestion(t *testing.T) {
	// A namespace whose server declares itself — so `infrahub` is a release
	// prefix this deployment established — co-hosting another product's
	// task manager.
	listing := ownershipListing(
		ownershipLine("infrahub-server-7d9f4-abcde", "Running", infrahubServerLabels),
		ownershipLine("othertool-task-manager-0", "Running", nil),
		ownershipLine("infrahub-task-manager-5f8c9", "Running", nil),
	)
	run := func(_ string, args ...string) (string, error) {
		if isServiceSelectorQuery(args) {
			return "", nil
		}

		return listing, nil
	}

	t.Run("the singular resolver refuses the foreign pod", func(t *testing.T) {
		pod, err := newTestKubernetesBackend().getPodForServiceWith(run, "task-manager")
		if err != nil {
			t.Fatalf("getPodForServiceWith failed: %v", err)
		}
		if pod != "infrahub-task-manager-5f8c9" {
			t.Errorf("pod = %q, want infrahub-task-manager-5f8c9: a foreign product's pod is where a wrong answer reads another product's connection URL", pod)
		}
	})

	t.Run("the enumerator refuses it too", func(t *testing.T) {
		pods, err := newTestKubernetesBackend().getAllPodsWith(run, "task-manager")
		if err != nil {
			t.Fatalf("getAllPodsWith failed: %v", err)
		}
		if len(pods) != 1 || pods[0] != "infrahub-task-manager-5f8c9" {
			t.Errorf("pods = %v, want only the deployment's own task manager", pods)
		}
	})

	t.Run("a namespace that declares nothing still resolves as it does today", func(t *testing.T) {
		bare := ownershipListing(ownershipLine("infrahub-task-manager-5f8c9", "Running", nil))
		bareRun := func(_ string, args ...string) (string, error) {
			if isServiceSelectorQuery(args) {
				return "", nil
			}

			return bare, nil
		}
		pod, err := newTestKubernetesBackend().getPodForServiceWith(bareRun, "task-manager")
		if err != nil {
			t.Fatalf("getPodForServiceWith failed: %v", err)
		}
		if pod != "infrahub-task-manager-5f8c9" {
			t.Errorf("pod = %q, want the pod a kustomize deployment has always resolved: strictness needs evidence to be strict with", pod)
		}
	})

	t.Run("a declaring pod outranks a claimed name whatever the listing order", func(t *testing.T) {
		ordered := ownershipListing(
			ownershipLine("infrahub-server-7d9f4-abcde", "Running", infrahubServerLabels),
			ownershipLine("infrahub-cache-0", "Running", nil),
			ownershipLine("redis-0", "Running", map[string]string{"app": "cache"}),
		)
		orderedRun := func(_ string, args ...string) (string, error) {
			if isServiceSelectorQuery(args) {
				return "", nil
			}

			return ordered, nil
		}
		pod, err := newTestKubernetesBackend().getPodForServiceWith(orderedRun, "cache")
		if err != nil {
			t.Fatalf("getPodForServiceWith failed: %v", err)
		}
		if pod != "redis-0" {
			t.Errorf("pod = %q, want redis-0: a pod that says what it is outranks one that merely carries a claimed name", pod)
		}
	})
}

// TestResolutionAndLocationGiveOneAnswer is T103. The resolver's fallback held
// the constant unanchoredNameClaims whatever the service was, while the
// location decision refused an unanchored prefixed name — so a namespace that
// declares nothing gave two answers from one listing, and the pod the resolver
// returned is where ensureNeo4jDiscovery and ensurePostgresDiscovery read the
// endpoint a restore later writes into.
func TestResolutionAndLocationGiveOneAnswer(t *testing.T) {
	// The shape the split cost something in: no pod declares a service, and the
	// only `database`-shaped pod belongs to another product.
	listing := ownershipListing(ownershipLine("postgres-database-0", "Running", nil))
	run := labelBlindNamespace(t, listing, nil)

	location, err := newTestKubernetesBackend().locateServiceWith(run, serviceNeo4j)
	if err != nil {
		t.Fatalf("locateServiceWith failed: %v", err)
	}
	if location != EndpointLocationExternal {
		t.Errorf("location = %q, want %q", location, EndpointLocationExternal)
	}

	pod, err := newTestKubernetesBackend().getPodForServiceWith(run, serviceNeo4j)
	if err == nil {
		t.Fatalf("getPodForServiceWith resolved %q, want no pod: the same evidence the location decision read as external", pod)
	}
	if pod != "" {
		t.Errorf("pod = %q, want none", pod)
	}

	pods, err := newTestKubernetesBackend().claimedPodNamesWith(run, serviceNeo4j, livePodFieldSelector, podPhaseIsTerminal)
	if err != nil {
		t.Fatalf("claimedPodNamesWith failed: %v", err)
	}
	if len(pods) != 0 {
		t.Errorf("claimedPodNamesWith = %v, want none", pods)
	}

	// The app services keep the permissive reading, which is the whole reason
	// the policy is read off the service rather than fixed for everyone: a
	// namespace with no labels at all still resolves its server.
	appListing := ownershipListing(ownershipLine("infrahub-server-7d9f4-abcde", "Running", nil))
	appRun := labelBlindNamespace(t, appListing, nil)

	server, err := newTestKubernetesBackend().getPodForServiceWith(appRun, "infrahub-server")
	if err != nil {
		t.Fatalf("getPodForServiceWith(infrahub-server) failed: %v", err)
	}
	if server != "infrahub-server-7d9f4-abcde" {
		t.Errorf("pod = %q, want infrahub-server-7d9f4-abcde: narrowing an app service here takes a working deployment off its path", server)
	}
}

// TestFindWorkloadResourceWithReadsPrefixesNamespaceWide is T104. The prefixes
// were derived inside the kind loop, so a chart that labels its StatefulSets
// and not its Deployments declared a release the Deployment pass could not see
// — and `redis-cache` was resolved as `cache` and scaled to zero in a namespace
// that had said what its release was called.
func TestFindWorkloadResourceWithReadsPrefixesNamespaceWide(t *testing.T) {
	run := labelBlindNamespace(t, "", map[string]string{
		// The declaration lives on a StatefulSet, which the deployment pass
		// used to reach only after it had already answered.
		"statefulset": workloadListing(t, map[string]map[string]string{
			"infrahub-server": infrahubServerLabels,
		}),
		"deployment": workloadListing(t, map[string]map[string]string{
			"redis-cache": nil,
		}),
	})

	kind, resource, err := newTestKubernetesBackend().findWorkloadResourceWith(run, "cache")
	if err == nil {
		t.Fatalf("findWorkloadResourceWith resolved %s/%s, want no workload: `infrahub` is the release this namespace declared, and redis-cache does not carry it", kind, resource)
	}
	if !strings.Contains(err.Error(), "no workloads found for cache") {
		t.Errorf("err = %v, want the answered-but-unmatched failure", err)
	}

	// The same namespace's own cache is still resolved, so this is a narrowing
	// of the wrong answer rather than of the right one.
	ownRun := labelBlindNamespace(t, "", map[string]string{
		"statefulset": workloadListing(t, map[string]map[string]string{
			"infrahub-server": infrahubServerLabels,
			"infrahub-cache":  nil,
		}),
		"deployment": workloadListing(t, map[string]map[string]string{"redis-cache": nil}),
	})

	_, resource, err = newTestKubernetesBackend().findWorkloadResourceWith(ownRun, "cache")
	if err != nil {
		t.Fatalf("findWorkloadResourceWith failed: %v", err)
	}
	if resource != "infrahub-cache" {
		t.Errorf("resource = %q, want infrahub-cache", resource)
	}
}

// TestLocateServiceWithClaimsACloudNativePGCluster is T105. locateServiceWith's
// own doc block names this topology as the case it fixes — pods `<cluster>-1`,
// labelled `cnpg.io/cluster` and so declaring nothing this tool reads — and it
// classified the database external anyway, which refuses the restore outright.
// It was strictly narrower than the pod resolver's fallback, which claimed the
// very same pod.
func TestLocateServiceWithClaimsACloudNativePGCluster(t *testing.T) {
	namespaces := map[string]string{
		// A chart-labelled deployment beside an operator-managed cluster: the
		// release prefix is `infrahub`, which the cluster's pods do not carry.
		"a labelled deployment": ownershipListing(
			ownershipLine("infrahub-server-7d9f4-abcde", "Running", helmChartLabels),
			ownershipLine("task-manager-db-1", "Running", map[string]string{"app": "cnpg"}),
		),
		// And a namespace that declares nothing at all.
		"an unlabelled deployment": ownershipListing(
			ownershipLine("task-manager-db-1", "Running", nil),
		),
	}

	for name, listing := range namespaces {
		t.Run(name, func(t *testing.T) {
			run := labelBlindNamespace(t, listing, nil)

			location, err := newTestKubernetesBackend().locateServiceWith(run, serviceTaskManagerDB)
			if err != nil {
				t.Fatalf("locateServiceWith failed: %v", err)
			}
			if location != EndpointLocationInternal {
				t.Errorf("location = %q, want %q: task-manager-db-1 is the service's own name with an ordinal after it, and refusing it refuses the restore of a database that is right there",
					location, EndpointLocationInternal)
			}

			// One answer: the resolver reaches the same pod.
			pod, err := newTestKubernetesBackend().getPodForServiceWith(run, serviceTaskManagerDB)
			if err != nil {
				t.Fatalf("getPodForServiceWith failed: %v", err)
			}
			if pod != "task-manager-db-1" {
				t.Errorf("pod = %q, want task-manager-db-1", pod)
			}
		})
	}

	// The foreign shape is still refused, which is what stops this being a
	// return to the bare token-boundary match: `postgres-database-0` states a
	// release this deployment never declared.
	t.Run("a foreign product's database is still external", func(t *testing.T) {
		run := labelBlindNamespace(t, ownershipListing(
			ownershipLine("infrahub-server-7d9f4-abcde", "Running", helmChartLabels),
			ownershipLine("postgres-database-0", "Running", nil),
		), nil)

		location, err := newTestKubernetesBackend().locateServiceWith(run, serviceNeo4j)
		if err != nil {
			t.Fatalf("locateServiceWith failed: %v", err)
		}
		if location != EndpointLocationExternal {
			t.Errorf("location = %q, want %q", location, EndpointLocationExternal)
		}
	})
}

// TestNameIsServiceOwnName pins the line between the service's own resource and
// a different component named after it, which is the one name-shaped claim that
// stands without a release prefix behind it.
func TestNameIsServiceOwnName(t *testing.T) {
	tests := []struct {
		name     string
		resource string
		service  string
		want     bool
	}{
		{"the workload itself", "cache", "cache", true},
		{"a StatefulSet pod's ordinal", "database-0", "database", true},
		{"a CloudNativePG cluster member", "task-manager-db-1", "task-manager-db", true},
		{"a Deployment pod's hash", "task-manager-db-6d4f9-x2klm", "task-manager-db", true},
		{"a metrics sidecar named after the service", "task-manager-db-exporter", "task-manager-db", false},
		{"the sidecar's Deployment pod", "task-manager-db-exporter-6b8f7c9d4-lm2xq", "task-manager-db", false},
		{"the database named after the task manager", "task-manager-db-0", "task-manager", false},
		{"a backup CronJob's pod", "database-backup-28123456-x9v2t", "database", false},
		{"a release prefix in front of the service", "infrahub-database-0", "database", false},
		{"a foreign product's database", "postgres-database-0", "database", false},
		{"the service name is not a whole segment", "databasetool-0", "database", false},
		{"letters shared but no boundary at all", "mycachetool-0", "cache", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nameIsServiceOwnName(tt.resource, tt.service); got != tt.want {
				t.Errorf("nameIsServiceOwnName(%q, %q) = %t, want %t", tt.resource, tt.service, got, tt.want)
			}
		})
	}
}

// TestControllerGeneratedSuffix pins the one reading of what follows a service
// in a name, which every name tier of the ownership rule shares (T150).
func TestControllerGeneratedSuffix(t *testing.T) {
	tests := []struct {
		name string
		rest string
		want bool
	}{
		{"the workload itself", "", true},
		{"a StatefulSet ordinal", "-0", true},
		{"a two-digit ordinal", "-12", true},
		{"a hash on its own", "-5f8c9", true},
		{"a ReplicaSet pod's hash and suffix", "-7d9f4b6c8-abcde", true},
		{"a short hash and a digit-carrying suffix", "-6d4f9-x2klm", true},
		{"an OpenShift deployment number and suffix", "-3-x9v2t", true},
		{"a metrics sidecar", "-exporter", false},
		{"a metrics endpoint", "-metrics", false},
		{"an init workload", "-init", false},
		{"a DaemonSet-managed sidecar", "-exporter-x9v2t", false},
		{"a sidecar's Deployment pod", "-exporter-6b8f7c9d4-lm2xq", false},
		{"a CronJob's pod", "-backup-28123456-x9v2t", false},
		{"a Job's pod", "-migrate-lm2xq", false},
		{"the database named after the task manager", "-db-0", false},
		{"a versioned sibling", "-v2-0", false},
		{"a letters-only hash is declined, the safe direction", "-bcdfgbcdf-x9v2t", false},
		{"the service name is not a whole segment", "tool-0", false},
		{"a bare hyphen", "-", false},
		{"an empty segment", "--0", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := controllerGeneratedSuffix(tt.rest); got != tt.want {
				t.Errorf("controllerGeneratedSuffix(%q) = %t, want %t", tt.rest, got, tt.want)
			}
		})
	}
}

// TestTheSuffixRuleDoesNotTurnOnWhatElseDeclaredItself is the invariant T150
// asks for: whether a name claims a service cannot depend on whether some other
// release happens to have declared itself in the namespace. It is asked through
// deploymentClaimsResource under the service's own policy, which is how every
// production caller asks it.
func TestTheSuffixRuleDoesNotTurnOnWhatElseDeclaredItself(t *testing.T) {
	prefixSets := map[string][]string{
		"nothing declared":      nil,
		"the release declared":  {"infrahub"},
		"two releases declared": {"infrahub", "othertool"},
	}

	// For a database neither end of the name turns on it: a prefix nothing
	// declared is refused under every prefix set (direction B, by design), and
	// so is a word after the service (direction A).
	for _, service := range []string{serviceNeo4j, serviceTaskManagerDB} {
		foreign := "pg-" + service + "-1"
		sidecar := "infrahub-" + service + "-exporter"
		own := service + "-0"
		for label, prefixes := range prefixSets {
			if deploymentClaimsResource(prefixes, foreign, "", service) {
				t.Errorf("%s: %s claimed as %s: a prefix nothing declared is another product's whatever else declared itself", label, foreign, service)
			}
			if deploymentClaimsResource(prefixes, sidecar, "", service) {
				t.Errorf("%s: %s claimed as %s: a word after the service is a different component whatever precedes it", label, sidecar, service)
			}
			if !deploymentClaimsResource(prefixes, own, "", service) {
				t.Errorf("%s: %s not claimed as %s: the service's own name needs nothing declared beside it", label, own, service)
			}
		}
	}

	// For an app service the left-hand rule is allowed to differ — an unknown
	// prefix claims only where nothing declared a release, which is the
	// shipped behaviour unanchoredNameClaims keeps — but the right-hand rule
	// does not: an exporter is not the cache under any of them.
	for label, prefixes := range prefixSets {
		if deploymentClaimsResource(prefixes, "infrahub-cache-exporter", "", "cache") {
			t.Errorf("%s: infrahub-cache-exporter claimed as cache", label)
		}
		if deploymentClaimsResource(prefixes, "cache-exporter", "", "cache") {
			t.Errorf("%s: cache-exporter claimed as cache", label)
		}
		if !deploymentClaimsResource(prefixes, "cache-0", "", "cache") {
			t.Errorf("%s: cache-0 not claimed as cache", label)
		}
	}
}

// TestLocationReadsAnExternalDatabaseBesideAComponentNamedAfterIt is T150's
// direction A at the decisions that matter. The deployment declares its
// release, its task-manager database is an RDS instance, and a
// postgres-exporter deployed alongside carries the release prefix and the
// service name. Claiming the exporter read the database as internal, resolved
// the exporter as the pod to run `pg_dump` in, and on restore scaled the
// exporter's Deployment and ran `pg_restore --clean --create` into it.
func TestLocationReadsAnExternalDatabaseBesideAComponentNamedAfterIt(t *testing.T) {
	exporterPod := ownershipLine("infrahub-task-manager-db-exporter-6b8f7c9d4-lm2xq", "Running", nil)
	deployments := workloadListing(t, map[string]map[string]string{
		"infrahub-server":                   infrahubServerLabels,
		"infrahub-task-manager-db-exporter": nil,
	})

	t.Run("the exporter is not the database", func(t *testing.T) {
		run := labelBlindNamespace(t, ownershipListing(serverPod, exporterPod), map[string]string{"deployment": deployments})

		location, err := newTestKubernetesBackend().locateServiceWith(run, serviceTaskManagerDB)
		if err != nil {
			t.Fatalf("locateServiceWith failed: %v", err)
		}
		if location != EndpointLocationExternal {
			t.Errorf("location = %q, want %q: the only task-manager-db-shaped pod is a metrics sidecar, and the database it stands beside is not in the cluster", location, EndpointLocationExternal)
		}

		pod, err := newTestKubernetesBackend().getPodForServiceWith(run, serviceTaskManagerDB)
		if !errors.Is(err, errNoPodsMatched) {
			t.Errorf("getPodForServiceWith() = %q, %v, want errNoPodsMatched: a sidecar is not somewhere to run pg_dump", pod, err)
		}

		names, err := newTestKubernetesBackend().claimedPodNamesWith(run, serviceTaskManagerDB, livePodFieldSelector, podPhaseIsTerminal)
		if err != nil {
			t.Fatalf("claimedPodNamesWith failed: %v", err)
		}
		if len(names) != 0 {
			t.Errorf("claimedPodNamesWith = %v, want none", names)
		}

		kind, resource, err := newTestKubernetesBackend().findWorkloadResourceWith(run, serviceTaskManagerDB)
		if err == nil {
			t.Fatalf("findWorkloadResourceWith resolved %s/%s, want no workload: a restore scales what this returns and then loads a database into it", kind, resource)
		}
		if !strings.Contains(err.Error(), "no workloads found for "+serviceTaskManagerDB) {
			t.Errorf("err = %v, want the answered-but-unmatched failure so restorePostgreSQL can skip the start", err)
		}
	})

	t.Run("the deployment's own database beside the exporter is still found", func(t *testing.T) {
		// A rule that refused everything would pass the subtest above.
		run := labelBlindNamespace(t,
			ownershipListing(serverPod, exporterPod, ownershipLine("infrahub-task-manager-db-0", "Running", nil)),
			map[string]string{
				"deployment":  deployments,
				"statefulset": workloadListing(t, map[string]map[string]string{"infrahub-task-manager-db": nil}),
			})

		location, err := newTestKubernetesBackend().locateServiceWith(run, serviceTaskManagerDB)
		if err != nil {
			t.Fatalf("locateServiceWith failed: %v", err)
		}
		if location != EndpointLocationInternal {
			t.Errorf("location = %q, want %q", location, EndpointLocationInternal)
		}

		pod, err := newTestKubernetesBackend().getPodForServiceWith(run, serviceTaskManagerDB)
		if err != nil {
			t.Fatalf("getPodForServiceWith failed: %v", err)
		}
		if pod != "infrahub-task-manager-db-0" {
			t.Errorf("pod = %q, want infrahub-task-manager-db-0", pod)
		}

		_, resource, err := newTestKubernetesBackend().findWorkloadResourceWith(run, serviceTaskManagerDB)
		if err != nil {
			t.Fatalf("findWorkloadResourceWith failed: %v", err)
		}
		if resource != "infrahub-task-manager-db" {
			t.Errorf("resource = %q, want infrahub-task-manager-db: the exporter's Deployment is listed first and must not be it", resource)
		}
	})

	t.Run("this run's own workload in a claimable shape is not the database either", func(t *testing.T) {
		// FR-028 at the same decision: a transient pod of this tool's own that
		// happens to carry a claimable name is excluded on its marker before
		// its name is read.
		run := labelBlindNamespace(t, ownershipListing(
			serverPod,
			ownershipLine("infrahub-task-manager-db-0", "Running", map[string]string{transientLabelMarker: "true"}),
		), nil)

		location, err := newTestKubernetesBackend().locateServiceWith(run, serviceTaskManagerDB)
		if err != nil {
			t.Fatalf("locateServiceWith failed: %v", err)
		}
		if location != EndpointLocationExternal {
			t.Errorf("location = %q, want %q: this run's own workload is not the deployment's database, whatever it is called (FR-028)", location, EndpointLocationExternal)
		}
	})
}

// TestADatabaseUnderAPrefixNothingDeclaredIsExternalByDesign is T150's
// direction B, recorded as the decision it is rather than fixed. A
// `pg-task-manager-db-1` is the `postgres-database-0` shape — a StatefulSet
// under a release this deployment never declared — and the rule refuses it
// for a database on purpose (see unanchoredNamePolicyFor). What T150 asks is
// that the refusal not turn on whether a release happens to be declared beside
// it, and that the running check and the location decision read it the same
// way rather than one of them quietly making it "external".
func TestADatabaseUnderAPrefixNothingDeclaredIsExternalByDesign(t *testing.T) {
	foreign := ownershipLine("pg-task-manager-db-1", "Running", map[string]string{"app": "cnpg"})
	namespaces := map[string]string{
		"a labelled deployment beside it": ownershipListing(serverPod, foreign),
		"nothing declared at all":         ownershipListing(foreign),
	}

	for name, listing := range namespaces {
		t.Run(name, func(t *testing.T) {
			run := labelBlindNamespace(t, listing, nil)

			location, err := newTestKubernetesBackend().locateServiceWith(run, serviceTaskManagerDB)
			if err != nil {
				t.Fatalf("locateServiceWith failed: %v", err)
			}
			if location != EndpointLocationExternal {
				t.Errorf("location = %q, want %q: `pg` is a release nothing in the namespace declared", location, EndpointLocationExternal)
			}

			statuses, err := newTestKubernetesBackend().getPodStatusesWith(run, serviceTaskManagerDB)
			if err != nil {
				t.Fatalf("getPodStatusesWith failed: %v", err)
			}
			if len(statuses) != 0 {
				t.Errorf("statuses = %v, want none: the same pod that is not this deployment's database cannot be this deployment's database running", statuses)
			}

			pod, err := newTestKubernetesBackend().getPodForServiceWith(run, serviceTaskManagerDB)
			if !errors.Is(err, errNoPodsMatched) {
				t.Errorf("getPodForServiceWith() = %q, %v, want errNoPodsMatched: one answer across the three readers", pod, err)
			}
		})
	}
}

// TestParseLabelledPodsReadsBothListingShapes is T119's parser merge asserted
// as the reason for it.
//
// There were two parsers for two pod listings, and the narrow one — for
// `<name>;<phase>` — split on the first ";" and took everything after it as the
// phase. Handed an ownership line it reported `Running;;;server` as a pod's
// phase, which `podPhaseIsTerminal` recognises as neither terminal nor
// completed, so the pod stayed in every answer under a phase no cluster
// reports. The ownership parser reads both shapes as they are: a line with no
// label fields simply declares nothing.
func TestParseLabelledPodsReadsBothListingShapes(t *testing.T) {
	t.Run("a name-and-phase line yields the phase and no declaration", func(t *testing.T) {
		pods := parseLabelledPods("infrahub-database-0;Running\ninfrahub-db-migrate-x9v2t;Succeeded\n")
		if len(pods) != 2 {
			t.Fatalf("parseLabelledPods returned %d pods, want 2", len(pods))
		}
		if pods[0].Phase != "Running" || pods[0].Service != "" {
			t.Errorf("pods[0] = %+v, want phase Running and no declared service", pods[0])
		}
		if !podPhaseIsCompleted(pods[1].Phase) {
			t.Errorf("pods[1].Phase = %q, want a phase the completed predicate recognises", pods[1].Phase)
		}
	})

	t.Run("an ownership line's labels do not leak into the phase", func(t *testing.T) {
		pods := parseLabelledPods(ownershipListing(
			ownershipLine("infrahub-db-migrate-x9v2t", "Succeeded", helmChartLabels),
		))
		if len(pods) != 1 {
			t.Fatalf("parseLabelledPods returned %d pods, want 1", len(pods))
		}
		if !podPhaseIsCompleted(pods[0].Phase) {
			t.Errorf("pods[0].Phase = %q, want the phase alone: the label fields belong to the declaration", pods[0].Phase)
		}
	})
}
