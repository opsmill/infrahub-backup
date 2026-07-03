package app

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestReplicaLogFilename(t *testing.T) {
	tests := []struct {
		name           string
		replica        Replica
		multiContainer bool
		previous       bool
		want           string
	}{
		{
			name:    "docker container",
			replica: Replica{Service: "infrahub-server", Container: "infrahub-server-1"},
			want:    "infrahub-server-1.log",
		},
		{
			name:     "docker previous suffix replaces .log",
			replica:  Replica{Service: "cache", Container: "cache-1"},
			previous: true,
			want:     "cache-1.previous.log",
		},
		{
			name:    "k8s single-container pod uses pod name",
			replica: Replica{Service: "infrahub-server", Pod: "infrahub-server-0", Container: "infrahub-server"},
			want:    "infrahub-server-0.log",
		},
		{
			name:           "k8s multi-container pod appends container",
			replica:        Replica{Service: "task-manager-db", Pod: "task-manager-db-1", Container: "postgres"},
			multiContainer: true,
			want:           "task-manager-db-1_postgres.log",
		},
		{
			name:     "k8s single-container previous",
			replica:  Replica{Service: "task-worker", Pod: "task-worker-0", Container: "task-worker"},
			previous: true,
			want:     "task-worker-0.previous.log",
		},
		{
			name:           "k8s multi-container previous",
			replica:        Replica{Service: "task-manager-db", Pod: "task-manager-db-1", Container: "metrics-exporter"},
			multiContainer: true,
			previous:       true,
			want:           "task-manager-db-1_metrics-exporter.previous.log",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := replicaLogFilename(tt.replica, tt.multiContainer, tt.previous)
			if got != tt.want {
				t.Errorf("replicaLogFilename(%+v, %v, %v) = %q, want %q", tt.replica, tt.multiContainer, tt.previous, got, tt.want)
			}
		})
	}
}

func TestPodContainerCounts(t *testing.T) {
	replicas := []Replica{
		{Service: "infrahub-server", Pod: "infrahub-server-0", Container: "infrahub-server"},
		{Service: "task-manager-db", Pod: "task-manager-db-1", Container: "postgres"},
		{Service: "task-manager-db", Pod: "task-manager-db-1", Container: "metrics-exporter"},
		{Service: "cache", Container: "cache-1"}, // Docker replica: no pod
	}

	counts := podContainerCounts(replicas)

	want := map[string]int{
		"infrahub-server-0": 1,
		"task-manager-db-1": 2,
	}
	if !reflect.DeepEqual(counts, want) {
		t.Errorf("podContainerCounts() = %v, want %v (Docker replicas must not count)", counts, want)
	}
}

func TestKubectlLogArgs(t *testing.T) {
	replica := Replica{Service: "infrahub-server", Pod: "infrahub-server-0", Container: "infrahub-server"}

	tests := []struct {
		name      string
		tailLines int
		previous  bool
		want      []string
	}{
		{
			name:      "current logs",
			tailLines: 100000,
			want: []string{
				"logs", "-n", "infrahub", "infrahub-server-0",
				"-c", "infrahub-server", "--tail=100000",
			},
		},
		{
			name:      "previous logs append --previous",
			tailLines: 500,
			previous:  true,
			want: []string{
				"logs", "-n", "infrahub", "infrahub-server-0",
				"-c", "infrahub-server", "--tail=500", "--previous",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := kubectlLogArgs("infrahub", replica, tt.tailLines, tt.previous)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("kubectlLogArgs() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestKubectlContainerStatusArgs(t *testing.T) {
	got := kubectlContainerStatusArgs("infrahub", "database-0")
	want := []string{
		"get", "pod", "database-0", "-n", "infrahub",
		"-o", "jsonpath={range .status.containerStatuses[*]}{.name}{\" \"}{.restartCount}{\"\\n\"}{end}",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("kubectlContainerStatusArgs() = %v, want %v", got, want)
	}
}

func TestParsePodContainerStatuses(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		want    []Replica
		wantErr string
	}{
		{
			name:   "single container without restarts",
			output: "infrahub-server 0\n",
			want: []Replica{
				{Service: "infrahub-server", Pod: "pod-a", Container: "infrahub-server", Restarted: false},
			},
		},
		{
			name:   "multi-container pod with per-container restart counts",
			output: "postgres 0\nmetrics-exporter 3\n",
			want: []Replica{
				{Service: "infrahub-server", Pod: "pod-a", Container: "postgres", Restarted: false},
				{Service: "infrahub-server", Pod: "pod-a", Container: "metrics-exporter", Restarted: true},
			},
		},
		{
			name:   "empty output yields no replicas",
			output: "\n\n",
			want:   []Replica{},
		},
		{
			name:    "malformed line",
			output:  "just-one-field\n",
			wantErr: "unexpected container status line",
		},
		{
			name:    "non-numeric restart count",
			output:  "infrahub-server many\n",
			wantErr: "invalid restart count",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parsePodContainerStatuses("infrahub-server", "pod-a", tt.output)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("parsePodContainerStatuses(%q) succeeded, want error containing %q", tt.output, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %q, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePodContainerStatuses(%q) failed: %v", tt.output, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parsePodContainerStatuses(%q) = %+v, want %+v", tt.output, got, tt.want)
			}
		})
	}
}

// logsFakeBackend is a collect backend whose log streams distinguish current
// from previous logs and can fail on demand.
type logsFakeBackend struct {
	bareBackend
	replicas     map[string][]Replica
	replicasErr  error
	logs         map[string]string // keyed by "<pod>/<container>"
	previousLogs map[string]string
	previousErr  error
}

var _ collectBackend = (*logsFakeBackend)(nil)

func (f *logsFakeBackend) ServiceReplicas(service string) ([]Replica, error) {
	if f.replicasErr != nil {
		return nil, f.replicasErr
	}
	return f.replicas[service], nil
}

func (f *logsFakeBackend) ReplicaLogs(replica Replica, tailLines int, previous bool) (io.ReadCloser, func() error, error) {
	key := replica.Pod + "/" + replica.Container
	if previous {
		if f.previousErr != nil {
			// Mimic kubectl: the process starts, produces no output, and the
			// failure surfaces through wait().
			return io.NopCloser(strings.NewReader("")), func() error { return f.previousErr }, nil
		}
		return io.NopCloser(strings.NewReader(f.previousLogs[key])), func() error { return nil }, nil
	}
	return io.NopCloser(strings.NewReader(f.logs[key])), func() error { return nil }, nil
}

func (f *logsFakeBackend) Metrics() (string, error) {
	return "", nil
}

func newLogsFakeBackend() *logsFakeBackend {
	return &logsFakeBackend{
		bareBackend: bareBackend{name: "kubernetes"},
		replicas: map[string][]Replica{
			"infrahub-server": {
				{Service: "infrahub-server", Pod: "infrahub-server-0", Container: "infrahub-server"},
				{Service: "infrahub-server", Pod: "infrahub-server-1", Container: "infrahub-server"},
			},
			"task-manager-db": {
				{Service: "task-manager-db", Pod: "task-manager-db-1", Container: "metrics-exporter", Restarted: true},
				{Service: "task-manager-db", Pod: "task-manager-db-1", Container: "postgres"},
			},
		},
		logs: map[string]string{
			"infrahub-server-0/infrahub-server":  "server zero\n",
			"infrahub-server-1/infrahub-server":  "server one\n",
			"task-manager-db-1/postgres":         "postgres current\n",
			"task-manager-db-1/metrics-exporter": "exporter current\n",
		},
		previousLogs: map[string]string{
			"task-manager-db-1/metrics-exporter": "exporter before crash\n",
		},
	}
}

func newLogsCollectContext(t *testing.T, backend EnvironmentBackend) *collectContext {
	t.Helper()
	return &collectContext{
		backend:   backend,
		opts:      CollectOptions{LogLines: 100},
		bundleDir: t.TempDir(),
	}
}

func TestServiceLogCollectors_CanonicalNames(t *testing.T) {
	collectors := serviceLogCollectors()

	wantNames := []string{
		"logs/infrahub-server",
		"logs/task-worker",
		"logs/database",
		"logs/message-queue",
		"logs/cache",
		"logs/task-manager",
		"logs/task-manager-db",
		"logs/task-manager-background-svc",
	}
	if len(collectors) != len(wantNames) {
		t.Fatalf("serviceLogCollectors() returned %d collectors, want %d", len(collectors), len(wantNames))
	}
	for i, want := range wantNames {
		if collectors[i].name != want {
			t.Errorf("collectors[%d].name = %q, want %q", i, collectors[i].name, want)
		}
	}
}

func TestCollectServiceLogs_PerReplicaFiles(t *testing.T) {
	cc := newLogsCollectContext(t, newLogsFakeBackend())

	if err := collectServiceLogs(cc, "infrahub-server"); err != nil {
		t.Fatalf("collectServiceLogs(infrahub-server) failed: %v", err)
	}

	wantFiles := map[string]string{
		"infrahub-server-0.log": "server zero\n",
		"infrahub-server-1.log": "server one\n",
	}
	for name, wantContent := range wantFiles {
		data, err := os.ReadFile(filepath.Join(cc.bundleDir, "logs", "infrahub-server", name))
		if err != nil {
			t.Errorf("expected log file %s missing: %v", name, err)
			continue
		}
		if string(data) != wantContent {
			t.Errorf("%s content = %q, want %q", name, data, wantContent)
		}
	}
}

func TestCollectServiceLogs_MultiContainerAndPrevious(t *testing.T) {
	cc := newLogsCollectContext(t, newLogsFakeBackend())

	if err := collectServiceLogs(cc, "task-manager-db"); err != nil {
		t.Fatalf("collectServiceLogs(task-manager-db) failed: %v", err)
	}

	serviceDir := filepath.Join(cc.bundleDir, "logs", "task-manager-db")
	wantFiles := map[string]string{
		"task-manager-db-1_postgres.log":                  "postgres current\n",
		"task-manager-db-1_metrics-exporter.log":          "exporter current\n",
		"task-manager-db-1_metrics-exporter.previous.log": "exporter before crash\n",
	}
	for name, wantContent := range wantFiles {
		data, err := os.ReadFile(filepath.Join(serviceDir, name))
		if err != nil {
			t.Errorf("expected log file %s missing: %v", name, err)
			continue
		}
		if string(data) != wantContent {
			t.Errorf("%s content = %q, want %q", name, data, wantContent)
		}
	}

	// The non-restarted container must not get a previous log file.
	if _, err := os.Stat(filepath.Join(serviceDir, "task-manager-db-1_postgres.previous.log")); !os.IsNotExist(err) {
		t.Errorf("previous log written for a container that never restarted (err = %v)", err)
	}
}

func TestCollectServiceLogs_PreviousLogFailureIsPartial(t *testing.T) {
	backend := newLogsFakeBackend()
	backend.previousErr = fmt.Errorf("exit status 1: previous terminated container not found")
	cc := newLogsCollectContext(t, backend)

	err := collectServiceLogs(cc, "task-manager-db")
	if err == nil {
		t.Fatal("collectServiceLogs succeeded, want partial-failure error for previous logs")
	}
	if !strings.Contains(err.Error(), "previous logs unavailable") {
		t.Errorf("error = %q, want mention of unavailable previous logs", err)
	}

	// Current logs of every replica are still in the bundle.
	serviceDir := filepath.Join(cc.bundleDir, "logs", "task-manager-db")
	for _, name := range []string{"task-manager-db-1_postgres.log", "task-manager-db-1_metrics-exporter.log"} {
		if _, statErr := os.Stat(filepath.Join(serviceDir, name)); statErr != nil {
			t.Errorf("current log %s missing after previous-log failure: %v", name, statErr)
		}
	}

	// The failed previous fetch produced no output, so no empty file remains.
	if _, statErr := os.Stat(filepath.Join(serviceDir, "task-manager-db-1_metrics-exporter.previous.log")); !os.IsNotExist(statErr) {
		t.Errorf("empty previous log file left behind (err = %v)", statErr)
	}
}

func TestServiceLogCollector_SkipAndFailure(t *testing.T) {
	t.Run("absent service is skipped with a reason", func(t *testing.T) {
		cc := newLogsCollectContext(t, newLogsFakeBackend())
		c := serviceLogCollector("task-manager-background-svc")

		skip, reason := c.skip(cc)
		if !skip {
			t.Fatal("skip = false for a service with no replicas, want true")
		}
		if reason != "service not deployed" {
			t.Errorf("skip reason = %q, want %q", reason, "service not deployed")
		}
	})

	t.Run("deployed service is not skipped", func(t *testing.T) {
		cc := newLogsCollectContext(t, newLogsFakeBackend())
		c := serviceLogCollector("infrahub-server")

		if skip, reason := c.skip(cc); skip {
			t.Errorf("skip = true (%q) for a deployed service, want false", reason)
		}
	})

	t.Run("enumeration error surfaces as run failure, not skip", func(t *testing.T) {
		backend := newLogsFakeBackend()
		backend.replicasErr = fmt.Errorf("connection to the server refused")
		cc := newLogsCollectContext(t, backend)
		c := serviceLogCollector("infrahub-server")

		if skip, _ := c.skip(cc); skip {
			t.Fatal("skip = true on enumeration error, want false so run reports failed")
		}
		err := c.run(cc)
		if err == nil {
			t.Fatal("run succeeded despite enumeration error, want failure")
		}
		if !strings.Contains(err.Error(), "failed to enumerate infrahub-server replicas") {
			t.Errorf("error = %q, want enumeration context", err)
		}
	})

	t.Run("backend without collect primitives is not skipped and fails in run", func(t *testing.T) {
		cc := newLogsCollectContext(t, &bareBackend{name: "docker"})
		c := serviceLogCollector("infrahub-server")

		if skip, _ := c.skip(cc); skip {
			t.Fatal("skip = true for unsupported backend, want false so run reports failed")
		}
		err := c.run(cc)
		if err == nil {
			t.Fatal("run succeeded on a backend without collect primitives, want failure")
		}
		if !strings.Contains(err.Error(), "does not support bundle collection") {
			t.Errorf("error = %q, want unsupported-backend message", err)
		}
	})
}
