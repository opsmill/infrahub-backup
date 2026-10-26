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
			// Neo4j: Infrahub's own label, then the Neo4j chart's, then the
			// generic labels.
			service: "database",
			want: []string{
				"infrahub/service=database",
				"helm.neo4j.com/pod_category=neo4j-instance",
				"app.kubernetes.io/component=database",
				"app=database",
				"component=database",
			},
		},
		{
			service: "task-manager-db",
			want: []string{
				"infrahub/service=task-manager-db",
				"app.kubernetes.io/component=task-manager-db",
				"app=task-manager-db",
				"component=task-manager-db",
			},
		},
	}
	for _, tt := range tests {
		if got := k.podSelectors(tt.service); !slices.Equal(got, tt.want) {
			t.Errorf("podSelectors(%q) = %q, want %q", tt.service, got, tt.want)
		}
	}
}
