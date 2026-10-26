package app

import (
	"slices"
	"testing"
)

func TestPodSelectors(t *testing.T) {
	k := NewKubernetesBackend(&Configuration{}, NewCommandExecutor())

	tests := []struct {
		service string
		want    []string
	}{
		{
			// Neo4j: Infrahub's own label, then the Neo4j chart's, then the generic
			// labels with CloudNativePG instance pods ruled out.
			service: "database",
			want: []string{
				"infrahub/service=database",
				"helm.neo4j.com/pod_category=neo4j-instance",
				"app.kubernetes.io/component=database,!cnpg.io/cluster",
				"app=database,!cnpg.io/cluster",
				"component=database,!cnpg.io/cluster",
			},
		},
		{
			// The task-manager database may run on CloudNativePG, so its pods are
			// never ruled out.
			service: "task-manager-db",
			want: []string{
				"infrahub/service=task-manager-db",
				"app.kubernetes.io/component=task-manager-db",
				"app=task-manager-db",
				"component=task-manager-db",
			},
		},
		{
			service: "task-worker",
			want: []string{
				"infrahub/service=task-worker",
				"app.kubernetes.io/component=task-worker,!cnpg.io/cluster",
				"app=task-worker,!cnpg.io/cluster",
				"component=task-worker,!cnpg.io/cluster",
			},
		},
	}
	for _, tt := range tests {
		if got := k.podSelectors(tt.service); !slices.Equal(got, tt.want) {
			t.Errorf("podSelectors(%q) = %q, want %q", tt.service, got, tt.want)
		}
	}
}

func TestSelectorMatchesLabels(t *testing.T) {
	cnpgLabels := map[string]string{
		"app.kubernetes.io/component": "database",
		"cnpg.io/cluster":             "taskmanagerdb",
	}
	neo4jLabels := map[string]string{
		"app":                         "infrahub",
		"helm.neo4j.com/pod_category": "neo4j-instance",
	}

	tests := []struct {
		name     string
		selector string
		labels   map[string]string
		want     bool
	}{
		{"equality matches", "app=infrahub", neo4jLabels, true},
		{"equality with a different value", "app=database", neo4jLabels, false},
		{"existence matches", "cnpg.io/cluster", cnpgLabels, true},
		{"existence of a missing label", "cnpg.io/cluster", neo4jLabels, false},
		{"absence rules out a labelled pod", "app.kubernetes.io/component=database,!cnpg.io/cluster", cnpgLabels, false},
		{"absence keeps an unlabelled pod", "!cnpg.io/cluster", neo4jLabels, true},
		{"absence on a pod without labels", "!cnpg.io/cluster", nil, true},
		{"equality on a pod without labels", "app=infrahub", nil, false},
		{"empty selector", "", neo4jLabels, false},
	}
	for _, tt := range tests {
		if got := selectorMatchesLabels(tt.selector, tt.labels); got != tt.want {
			t.Errorf("%s: selectorMatchesLabels(%q, %v) = %v, want %v", tt.name, tt.selector, tt.labels, got, tt.want)
		}
	}
}
