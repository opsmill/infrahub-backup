package app

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseComposePSContainers(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		want    []composePSContainer
		wantErr string
	}{
		{
			name:   "empty output yields no containers",
			output: "",
			want:   []composePSContainer{},
		},
		{
			name:   "whitespace-only output yields no containers",
			output: "\n  \n",
			want:   []composePSContainer{},
		},
		{
			name: "NDJSON lines (compose >= 2.21)",
			output: `{"Name":"proj-cache-1","Service":"cache","State":"running","Labels":"com.docker.compose.oneoff=False"}
{"Name":"proj-task-worker-2","Service":"task-worker","State":"running","Labels":"com.docker.compose.oneoff=False"}
`,
			want: []composePSContainer{
				{Name: "proj-cache-1", Service: "cache", State: "running", Labels: "com.docker.compose.oneoff=False"},
				{Name: "proj-task-worker-2", Service: "task-worker", State: "running", Labels: "com.docker.compose.oneoff=False"},
			},
		},
		{
			name:   "JSON array (older compose releases)",
			output: `[{"Name":"proj-cache-1","Service":"cache","State":"exited"}]`,
			want: []composePSContainer{
				{Name: "proj-cache-1", Service: "cache", State: "exited"},
			},
		},
		{
			name: "one-off run containers are excluded",
			output: `{"Name":"proj-task-worker-1","Service":"task-worker","State":"running","Labels":"com.docker.compose.oneoff=False"}
{"Name":"proj-task-worker-run-ab12","Service":"task-worker","State":"exited","Labels":"com.docker.compose.oneoff=True"}
`,
			want: []composePSContainer{
				{Name: "proj-task-worker-1", Service: "task-worker", State: "running", Labels: "com.docker.compose.oneoff=False"},
			},
		},
		{
			name:   "leading slash in the container name is stripped",
			output: `{"Name":"/proj-database-1","Service":"database","State":"running"}`,
			want: []composePSContainer{
				{Name: "proj-database-1", Service: "database", State: "running"},
			},
		},
		{
			name:   "image is parsed (used for edition detection)",
			output: `{"Name":"proj-infrahub-server-1","Service":"infrahub-server","State":"running","Image":"registry.opsmill.io/opsmill/infrahub-enterprise:1.5.2"}`,
			want: []composePSContainer{
				{Name: "proj-infrahub-server-1", Service: "infrahub-server", State: "running", Image: "registry.opsmill.io/opsmill/infrahub-enterprise:1.5.2"},
			},
		},
		{
			name:    "invalid JSON line",
			output:  "not-json\n",
			wantErr: "failed to parse docker compose ps line",
		},
		{
			name:    "invalid JSON array",
			output:  `[{"Name":]`,
			wantErr: "failed to parse docker compose ps output",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseComposePSContainers(tt.output)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("parseComposePSContainers(%q) succeeded, want error containing %q", tt.output, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %q, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseComposePSContainers(%q) failed: %v", tt.output, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseComposePSContainers(%q) = %+v, want %+v", tt.output, got, tt.want)
			}
		})
	}
}

func TestReplicasFromComposePS(t *testing.T) {
	containers := []composePSContainer{
		{Name: "proj-task-worker-2", Service: "task-worker", State: "running"},
		{Name: "proj-cache-1", Service: "cache", State: "running"},
		{Name: "proj-task-worker-1", Service: "task-worker", State: "running"},
		{Name: "", Service: "task-worker", State: "running"}, // nameless entries are dropped
	}

	tests := []struct {
		name    string
		service string
		want    []Replica
	}{
		{
			name:    "every replica of a scaled service appears, sorted by name",
			service: "task-worker",
			want: []Replica{
				{Service: "task-worker", Container: "proj-task-worker-1"},
				{Service: "task-worker", Container: "proj-task-worker-2"},
			},
		},
		{
			name:    "single-replica service",
			service: "cache",
			want: []Replica{
				{Service: "cache", Container: "proj-cache-1"},
			},
		},
		{
			name:    "service without containers yields an empty slice",
			service: "task-manager-background-svc",
			want:    []Replica{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := replicasFromComposePS(tt.service, containers)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("replicasFromComposePS(%q) = %+v, want %+v", tt.service, got, tt.want)
			}
			for _, replica := range got {
				if replica.Pod != "" || replica.Restarted {
					t.Errorf("docker replica %+v must have empty Pod and Restarted=false", replica)
				}
			}
		})
	}
}

func TestDockerLogArgs(t *testing.T) {
	replica := Replica{Service: "task-worker", Container: "proj-task-worker-1"}

	got := dockerLogArgs(replica, 500)
	want := []string{"logs", "--tail", "500", "proj-task-worker-1"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("dockerLogArgs() = %v, want %v", got, want)
	}
}

func TestDockerReplicaLogs_PreviousIsUnavailable(t *testing.T) {
	backend := NewDockerBackend(&Configuration{}, NewCommandExecutor())

	reader, wait, err := backend.ReplicaLogs(Replica{Service: "cache", Container: "proj-cache-1"}, 100, true)
	if err == nil {
		t.Fatal("ReplicaLogs(previous=true) succeeded on docker, want error")
	}
	if reader != nil || wait != nil {
		t.Error("ReplicaLogs(previous=true) returned a stream despite the error")
	}
	if !strings.Contains(err.Error(), "not available on docker") {
		t.Errorf("error = %q, want it to mention previous logs are unavailable on docker", err)
	}
}
