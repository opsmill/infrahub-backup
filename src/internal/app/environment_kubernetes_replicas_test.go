package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeKubectlScript stands in for kubectl against one namespace, "infrahub",
// that holds one deployment, named after the service it runs (the format
// argument). The deployment's replica count lives in the file "replicas": the
// replica count read prints it and `kubectl scale` overwrites it. A file
// "stderr" is written to stderr on that read, and a file "fail" makes the read
// exit 1 after it. Every call's arguments are appended to the file "calls".
const fakeKubectlScript = `#!/bin/sh
dir="$(dirname "$0")"
echo "$*" >> "$dir/calls"
case "$*" in
"get deployment -n infrahub -l "*"=%[1]s -o "*)
	echo %[1]s ;;
"get deployment %[1]s -n infrahub -o jsonpath={.spec.replicas}")
	if [ -f "$dir/stderr" ]; then cat "$dir/stderr" >&2; fi
	if [ -f "$dir/fail" ]; then exit 1; fi
	cat "$dir/replicas" ;;
"scale -n infrahub deployment/%[1]s --replicas="*)
	for last; do :; done
	echo "${last#--replicas=}" > "$dir/replicas"
	echo "deployment.apps/%[1]s scaled" ;;
esac
`

type fakeKubectl struct {
	t   *testing.T
	dir string
}

// newFakeKubectl puts fakeKubectlScript first on PATH for the test, with the
// service's deployment at the given replica count.
func newFakeKubectl(t *testing.T, service, replicas string) *fakeKubectl {
	t.Helper()
	dir := t.TempDir()
	script := fmt.Sprintf(fakeKubectlScript, service)
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	f := &fakeKubectl{t: t, dir: dir}
	f.write("replicas", replicas+"\n")
	return f
}

func (f *fakeKubectl) write(name, content string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, name), []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// scaleCalls returns the --replicas argument of every kubectl scale call, in
// order.
func (f *fakeKubectl) scaleCalls() []string {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.dir, "calls"))
	if err != nil && !os.IsNotExist(err) {
		f.t.Fatal(err)
	}
	calls := []string{}
	for _, line := range nonEmptyLines(string(data)) {
		if strings.HasPrefix(line, "scale ") {
			fields := strings.Fields(line)
			calls = append(calls, fields[len(fields)-1])
		}
	}
	return calls
}

func newTestKubernetesBackend() *KubernetesBackend {
	k := NewKubernetesBackend(&Configuration{}, NewCommandExecutor())
	k.namespace = "infrahub"
	return k
}

func assertScaleCalls(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("kubectl scale calls = %v, want %v", got, want)
	}
}

// TestKubernetesStopStart_RestoresReplicaCount checks that a workload stopped
// and started again comes back with the replica count it had, including when
// kubectl writes a warning to stderr while it reads that count.
func TestKubernetesStopStart_RestoresReplicaCount(t *testing.T) {
	tests := []struct {
		name   string
		stderr string
	}{
		{name: "count on stdout only"},
		{name: "warning on stderr", stderr: "Warning: apps/v1 Deployment is deprecated\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kubectl := newFakeKubectl(t, "infrahub-server", "3")
			if tt.stderr != "" {
				kubectl.write("stderr", tt.stderr)
			}
			k := newTestKubernetesBackend()

			if err := k.Stop("infrahub-server"); err != nil {
				t.Fatalf("Stop failed: %v", err)
			}
			if err := k.Start("infrahub-server"); err != nil {
				t.Fatalf("Start failed: %v", err)
			}
			assertScaleCalls(t, kubectl.scaleCalls(), "--replicas=0", "--replicas=3")
		})
	}
}

// TestKubernetesStop_UnreadableReplicaCount checks that Stop does not scale a
// workload down when it cannot read the workload's replica count, and that the
// error carries kubectl's message.
func TestKubernetesStop_UnreadableReplicaCount(t *testing.T) {
	kubectl := newFakeKubectl(t, "infrahub-server", "3")
	kubectl.write("stderr", `Error from server (Forbidden): deployments.apps "infrahub-server" is forbidden`+"\n")
	kubectl.write("fail", "")
	k := newTestKubernetesBackend()

	err := k.Stop("infrahub-server")
	if err == nil {
		t.Fatal("Stop succeeded, want an error because the replica count cannot be read")
	}
	for _, want := range []string{"deployment/infrahub-server", "Forbidden"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to contain %q", err, want)
		}
	}
	assertScaleCalls(t, kubectl.scaleCalls())
}

// TestKubernetesRecordReplicaCounts_KeepsFirstCount follows a restore: the
// counts are recorded before anything changes, the application services are
// stopped, and then restartDependencies stops the already stopped workload
// again before it starts it. The workload must come back with the count it had
// before the restore, not with the 0 the second Stop reads.
func TestKubernetesRecordReplicaCounts_KeepsFirstCount(t *testing.T) {
	kubectl := newFakeKubectl(t, "infrahub-server", "3")
	k := newTestKubernetesBackend()

	if err := k.RecordReplicaCounts("infrahub-server"); err != nil {
		t.Fatalf("RecordReplicaCounts failed: %v", err)
	}
	for range 2 {
		if err := k.Stop("infrahub-server"); err != nil {
			t.Fatalf("Stop failed: %v", err)
		}
	}
	if err := k.Start("infrahub-server"); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	assertScaleCalls(t, kubectl.scaleCalls(), "--replicas=0", "--replicas=0", "--replicas=3")
}

// TestKubernetesRecordReplicaCounts_SkipsServiceWithoutWorkload checks that a
// service that is not deployed, such as the optional
// task-manager-background-svc, does not fail the recording.
func TestKubernetesRecordReplicaCounts_SkipsServiceWithoutWorkload(t *testing.T) {
	newFakeKubectl(t, "infrahub-server", "3")
	k := newTestKubernetesBackend()

	if err := k.RecordReplicaCounts("task-manager-background-svc"); err != nil {
		t.Fatalf("RecordReplicaCounts failed for a service with no workload: %v", err)
	}
	if len(k.replicaCache) != 0 {
		t.Errorf("replicaCache = %v, want it empty", k.replicaCache)
	}
}

// TestRecordReplicaCounts_FailsBeforeAnyChange checks the check that backup and
// restore run before they change the deployment: an unreadable replica count
// fails it, and no workload is scaled.
func TestRecordReplicaCounts_FailsBeforeAnyChange(t *testing.T) {
	kubectl := newFakeKubectl(t, "infrahub-server", "3")
	kubectl.write("fail", "")
	k := newTestKubernetesBackend()
	iops := &InfrahubOps{config: &Configuration{}, backend: k, executor: k.executor}

	err := iops.recordReplicaCounts(appServices...)
	if err == nil {
		t.Fatal("recordReplicaCounts succeeded, want an error because a replica count cannot be read")
	}
	if !strings.Contains(err.Error(), "nothing in the deployment was changed") {
		t.Errorf("err = %v, want it to say that the deployment was not changed", err)
	}
	assertScaleCalls(t, kubectl.scaleCalls())
}

// TestRecordReplicaCounts_RestoreKeepsTaskManagerDBCount checks that a restore
// starts task-manager-db with the replica count it had, not with 1 replica.
// restorePostgreSQL starts task-manager-db without stopping it first, so only
// the count recorded before the restore changes anything can tell Start what
// it was.
func TestRecordReplicaCounts_RestoreKeepsTaskManagerDBCount(t *testing.T) {
	kubectl := newFakeKubectl(t, "task-manager-db", "2")
	k := newTestKubernetesBackend()
	iops := &InfrahubOps{config: &Configuration{}, backend: k, executor: k.executor}

	if err := iops.recordReplicaCounts(restoreServices...); err != nil {
		t.Fatalf("recordReplicaCounts failed: %v", err)
	}
	if err := iops.StartServices("task-manager-db"); err != nil {
		t.Fatalf("StartServices failed: %v", err)
	}
	assertScaleCalls(t, kubectl.scaleCalls(), "--replicas=2")
}

// TestRecordReplicaCounts_RestoreSkipsExternalTaskManagerDB covers a task
// manager database that runs outside the cluster, or on CloudNativePG, which
// creates its pods without a Deployment or StatefulSet. No workload matches
// task-manager-db, so the restore records the other counts without an error.
// Starting task-manager-db then fails, which restorePostgreSQL reports on
// Kubernetes as a database that may be externally managed, and continues.
func TestRecordReplicaCounts_RestoreSkipsExternalTaskManagerDB(t *testing.T) {
	kubectl := newFakeKubectl(t, "infrahub-server", "3")
	k := newTestKubernetesBackend()
	iops := &InfrahubOps{config: &Configuration{}, backend: k, executor: k.executor}

	if err := iops.recordReplicaCounts(restoreServices...); err != nil {
		t.Fatalf("recordReplicaCounts failed for a task-manager-db with no workload: %v", err)
	}
	if got := k.replicaCache["deployment/infrahub-server"]; got != 3 {
		t.Errorf("recorded count for deployment/infrahub-server = %d, want 3", got)
	}
	if err := iops.StartServices("task-manager-db"); err == nil {
		t.Error("StartServices(task-manager-db) succeeded, want the workload lookup error that restorePostgreSQL treats as an externally managed database")
	}
	assertScaleCalls(t, kubectl.scaleCalls())
}
