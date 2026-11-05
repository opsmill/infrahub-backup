package app

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
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

// fakePod is one pod of a fakeNamespace: its name and labels.
type fakePod struct {
	name   string
	labels map[string]string
}

// fakeNamespace answers the kubectl pod queries the pod resolvers make. It
// evaluates label selectors against each pod's labels, so a test describes a
// namespace by what every workload in it labels its pods with.
type fakeNamespace []fakePod

func (ns fakeNamespace) run(name string, args ...string) (string, error) {
	// kubectl get pod <pod> -n <ns> -o jsonpath={.metadata.labels.<key>}
	if len(args) > 2 && args[0] == "get" && args[1] == "pod" {
		jsonpath := args[len(args)-1]
		key := strings.TrimSuffix(strings.TrimPrefix(jsonpath, "jsonpath={.metadata.labels."), "}")
		key = strings.ReplaceAll(key, `\.`, ".")
		for _, pod := range ns {
			if pod.name == args[2] {
				return pod.labels[key], nil
			}
		}
		return "", fmt.Errorf("pods %q not found", args[2])
	}

	// kubectl get pods -n <ns> [-l <selector>] -o jsonpath=...
	selector := ""
	for i, arg := range args {
		if arg == "-l" && i+1 < len(args) {
			selector = args[i+1]
		}
	}
	names := []string{}
	for _, pod := range ns {
		if selector == "" || selectorMatchesLabels(selector, pod.labels) {
			names = append(names, pod.name)
		}
	}
	return strings.Join(names, "\n"), nil
}

// cnpgPod is a CloudNativePG instance pod of the task-manager database, with
// the labels the operator sets plus infrahub/service, which the Infrahub HA
// deployment example adds through the Cluster's inheritedMetadata.
func cnpgPod(name, role string) fakePod {
	return fakePod{name: name, labels: map[string]string{
		"app.kubernetes.io/component":  "database",
		"app.kubernetes.io/managed-by": "cloudnative-pg",
		"cnpg.io/cluster":              "taskmanagerdb",
		"cnpg.io/instanceRole":         role,
		"infrahub/service":             "task-manager-db",
	}}
}

// neo4jPod is a Neo4j server pod deployed by the Neo4j Helm chart. extra adds
// labels on top of the ones the chart always sets.
func neo4jPod(name string, extra map[string]string) fakePod {
	labels := map[string]string{
		"app":                         "infrahub",
		"helm.neo4j.com/pod_category": "neo4j-instance",
	}
	for key, value := range extra {
		labels[key] = value
	}
	return fakePod{name: name, labels: labels}
}

// TestPodResolutionWithPostgresOperatorLabels resolves the Neo4j and
// task-manager database pods in a namespace where the task-manager database
// runs on a PostgreSQL operator that labels its pods
// app.kubernetes.io/component=database, as CloudNativePG does. That is the
// same value as the Neo4j service, so a lookup that tried the generic label
// first sent the Neo4j edition query to the Postgres primary.
func TestPodResolutionWithPostgresOperatorLabels(t *testing.T) {
	postgres := []fakePod{
		cnpgPod("taskmanagerdb-1", "replica"),
		cnpgPod("taskmanagerdb-2", "primary"),
		cnpgPod("taskmanagerdb-3", "replica"),
	}

	tests := []struct {
		name         string
		neo4j        []fakePod
		wantDatabase []string
	}{
		{
			// The HA deployment example labels Neo4j infrahub/service=database.
			name: "HA example with infrahub/service on Neo4j",
			neo4j: []fakePod{
				neo4jPod("database-0-0", map[string]string{"infrahub/service": "database"}),
				neo4jPod("database-1-0", map[string]string{"infrahub/service": "database"}),
				neo4jPod("database-2-0", map[string]string{"infrahub/service": "database"}),
			},
			wantDatabase: []string{"database-0-0", "database-1-0", "database-2-0"},
		},
		{
			// Neo4j installed from its own chart without the infrahub/service
			// label: found through the Neo4j chart's pod label.
			name:         "Neo4j without infrahub/service",
			neo4j:        []fakePod{neo4jPod("database-0", map[string]string{"helm.neo4j.com/instance": "database"})},
			wantDatabase: []string{"database-0"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Postgres pods come first, as they would in a namespace listed by name.
			ns := fakeNamespace(append(append([]fakePod{}, postgres...), tt.neo4j...))
			k := NewKubernetesBackend(&Configuration{}, NewCommandExecutor())
			k.namespace = "infrahub"

			pod, err := k.getPodForServiceWith(ns.run, "database")
			if err != nil {
				t.Fatalf("getPodForServiceWith(database) failed: %v", err)
			}
			if pod != tt.wantDatabase[0] {
				t.Errorf("database resolved to pod %q, want the Neo4j pod %q", pod, tt.wantDatabase[0])
			}

			pods, err := k.getAllPodsWith(ns.run, "database")
			if err != nil {
				t.Fatalf("getAllPodsWith(database) failed: %v", err)
			}
			if !slices.Equal(pods, tt.wantDatabase) {
				t.Errorf("database pods = %v, want the Neo4j pods %v", pods, tt.wantDatabase)
			}

			pod, err = k.getPodForServiceWith(ns.run, "task-manager-db")
			if err != nil {
				t.Fatalf("getPodForServiceWith(task-manager-db) failed: %v", err)
			}
			if pod != "taskmanagerdb-2" {
				t.Errorf("task-manager-db resolved to pod %q, want the Postgres primary taskmanagerdb-2", pod)
			}
		})
	}
}

// TestGetPodForServiceWith_SelectsPrimaryPod covers FR-014 of
// specs/006-taskmgr-db-storage: with a CloudNativePG cluster behind
// task-manager-db, the database queries run on the primary pod, because a
// standby does not run VACUUM or autovacuum. The pod lookup tries the label
// selectors in podSelectors order, and the Infrahub HA examples label the pods
// infrahub/service=task-manager-db.
func TestGetPodForServiceWith_SelectsPrimaryPod(t *testing.T) {
	threePods := "task-manager-db-1\ntask-manager-db-2\ntask-manager-db-3\n"
	tests := []struct {
		name string
		pods string
		// labels maps a label path of findPrimaryPod's jsonpath lookups to
		// the value each pod carries; a pod without a value has no label.
		labels          map[string]map[string]string
		want            string
		wantLabelLookup bool
	}{
		{
			name: "CloudNativePG instance role",
			pods: threePods,
			labels: map[string]map[string]string{
				`cnpg\.io/instanceRole`: {"task-manager-db-1": "replica", "task-manager-db-2": "primary", "task-manager-db-3": "replica"},
			},
			want:            "task-manager-db-2",
			wantLabelLookup: true,
		},
		{
			name: "legacy role label",
			pods: threePods,
			labels: map[string]map[string]string{
				"role": {"task-manager-db-1": "replica", "task-manager-db-2": "primary", "task-manager-db-3": "replica"},
			},
			want:            "task-manager-db-2",
			wantLabelLookup: true,
		},
		{
			name:            "no primary label falls back to the first pod",
			pods:            threePods,
			labels:          map[string]map[string]string{},
			want:            "task-manager-db-1",
			wantLabelLookup: true,
		},
		{
			name:   "a single pod is used without a label lookup",
			pods:   "task-manager-db-0\n",
			labels: map[string]map[string]string{},
			want:   "task-manager-db-0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k := NewKubernetesBackend(&Configuration{}, NewCommandExecutor())
			k.namespace = "infrahub"

			var calls [][]string
			labelLookups := 0
			run := func(name string, args ...string) (string, error) {
				calls = append(calls, args)
				if name != "kubectl" {
					return "", fmt.Errorf("unexpected command %s", name)
				}
				switch {
				case slices.Contains(args, "pods") && slices.Contains(args, "-l"):
					if slices.Contains(args, "infrahub/service=task-manager-db") {
						return tt.pods, nil
					}
					return "", nil // a selector that matches nothing
				case len(args) == 7 && args[0] == "get" && args[1] == "pod":
					labelLookups++
					pod, jsonpath := args[2], args[6]
					for label, values := range tt.labels {
						if jsonpath == "jsonpath={.metadata.labels."+label+"}" {
							return values[pod], nil
						}
					}
					return "", nil
				}
				return "", fmt.Errorf("unexpected kubectl call %q", args)
			}

			pod, err := k.getPodForServiceWith(run, "task-manager-db")
			if err != nil {
				t.Fatalf("getPodForServiceWith failed: %v", err)
			}
			if pod != tt.want {
				t.Errorf("pod = %q, want %q", pod, tt.want)
			}
			if (labelLookups > 0) != tt.wantLabelLookup {
				t.Errorf("label lookups = %d, want any: %v", labelLookups, tt.wantLabelLookup)
			}
			if cached := k.podCache["task-manager-db"]; cached != tt.want {
				t.Errorf("cached pod = %q, want %q", cached, tt.want)
			}

			callsBefore := len(calls)
			again, err := k.getPodForServiceWith(run, "task-manager-db")
			if err != nil || again != tt.want {
				t.Errorf("second lookup = (%q, %v), want the cached %q", again, err, tt.want)
			}
			if len(calls) != callsBefore {
				t.Errorf("second lookup ran %d kubectl calls, want none: %q", len(calls)-callsBefore, calls[callsBefore:])
			}
		})
	}
}
