package app

import (
	"errors"
	"fmt"
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
