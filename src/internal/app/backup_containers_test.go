package app

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeTransientDataBackend answers the rabbitmqctl calls that
// deleteMessageQueues makes and the redis-cli calls that flushCache makes, and
// records every command run in a container. ctl and redis return the stdout,
// stderr and error of one rabbitmqctl or redis-cli call, given its arguments.
type fakeTransientDataBackend struct {
	bareBackend
	ctl        func(args []string) (string, string, error)
	redis      func(args []string) (string, string, error)
	execs      [][]string
	ctlCalls   [][]string
	redisCalls [][]string
}

func (f *fakeTransientDataBackend) Exec(service string, command []string, opts *ExecOptions) (string, error) {
	f.execs = append(f.execs, append([]string{service}, command...))
	return "", nil
}

func (f *fakeTransientDataBackend) ExecSeparateContext(ctx context.Context, timeout time.Duration, service string, command []string, opts *ExecOptions) (string, string, error) {
	f.execs = append(f.execs, append([]string{service}, command...))
	switch {
	case service == "message-queue" && len(command) > 0 && command[0] == "rabbitmqctl":
		f.ctlCalls = append(f.ctlCalls, command[1:])
		return f.ctl(command[1:])
	case service == "cache" && len(command) > 3 && reflect.DeepEqual(command[:4], []string{"sh", "-c", redisCLIScript, "redis-cli"}):
		f.redisCalls = append(f.redisCalls, command[4:])
		return f.redis(command[4:])
	}
	return "", "", errors.New("unexpected command")
}

// deleteCalls returns the queues deleted, as "vhost name".
func (f *fakeTransientDataBackend) deleteCalls() []string {
	var deleted []string
	for _, args := range f.ctlCalls {
		if args[0] == "delete_queue" {
			deleted = append(deleted, args[3]+" "+args[len(args)-1])
		}
	}
	return deleted
}

// newFakeTransientDataBackend serves the given queue list, as list_queues
// prints it in JSON, for each virtual host, and a standalone Redis master. A
// docker compose warning is written to stderr on every call, as it is on a
// deployment that leaves a variable unset.
func newFakeTransientDataBackend(queues map[string]string, failDelete map[string]bool) *fakeTransientDataBackend {
	const warning = `WARN[0000] The "INFRAHUB_LICENSE_KEY" variable is not set. Defaulting to a blank string.`
	vhosts := make([]string, 0, len(queues))
	for vhost := range queues {
		vhosts = append(vhosts, `{"name":"`+vhost+`"}`)
	}
	f := &fakeTransientDataBackend{bareBackend: bareBackend{name: "docker"}}
	f.ctl = func(args []string) (string, string, error) {
		switch args[0] {
		case "list_vhosts":
			return "[\n" + strings.Join(vhosts, "\n,") + "\n]", warning, nil
		case "list_queues":
			return queues[args[5]], warning, nil
		case "delete_queue":
			if failDelete[args[len(args)-1]] {
				return "", warning + "\nError:\nNo such queue was found", errors.New("exit status 64")
			}
			return "Queue was successfully deleted with 0 ready messages", warning, nil
		}
		return "", "", errors.New("unexpected rabbitmqctl command")
	}
	f.redis = fakeRedis("master\n0", nil)
	return f
}

// fakeRedis answers redis-cli calls like a Redis whose ROLE reply is role and
// which holds 3 keys in database 0 and 1 key in database 1. A reply in
// replies, keyed by the command after any -h and -p arguments, replaces the
// default one.
func fakeRedis(role string, replies map[string]string) func(args []string) (string, string, error) {
	return func(args []string) (string, string, error) {
		if len(args) > 4 && args[0] == "-h" && args[2] == "-p" {
			args = args[4:]
		}
		key := strings.Join(args, " ")
		if len(args) == 5 && args[2] == "EVAL" {
			if args[3] != redisDeleteKeysScript || args[4] != "0" {
				return "", "", errors.New("unexpected EVAL")
			}
			key = "-n " + args[1] + " EVAL"
		}
		if reply, ok := replies[key]; ok {
			return reply, "", nil
		}
		switch key {
		case "ROLE":
			return role, "", nil
		case "INFO keyspace":
			return "# Keyspace\r\ndb0:keys=3,expires=0,avg_ttl=0\r\ndb1:keys=1,expires=0,avg_ttl=0", "", nil
		case "-n 0 EVAL":
			return "3", "", nil
		case "-n 1 EVAL":
			return "1", "", nil
		}
		return "", "", errors.New("unexpected redis-cli command")
	}
}

// TestDeleteMessageQueues_DeletesTheQueuesOfEveryVhost checks that the queues
// of every virtual host are deleted, except the exclusive queues of connected
// clients, and that a warning on stderr does not stop the lists from parsing.
func TestDeleteMessageQueues_DeletesTheQueuesOfEveryVhost(t *testing.T) {
	backend := newFakeTransientDataBackend(map[string]string{
		"/": `[
{"name":"infrahub.rpcs","exclusive":false}
,{"name":"api-callback-1","exclusive":true}
,{"name":"infrahub.delay.5000","exclusive":false}
]`,
		"other": `[
{"name":"-starts-with-a-dash","exclusive":false}
]`,
	}, nil)
	iops := &InfrahubOps{backend: backend}

	if err := iops.deleteMessageQueues(); err != nil {
		t.Fatalf("deleteMessageQueues() = %v, want nil", err)
	}

	got := backend.deleteCalls()
	want := map[string]bool{"/ infrahub.rpcs": true, "/ infrahub.delay.5000": true, "other -starts-with-a-dash": true}
	if len(got) != len(want) {
		t.Fatalf("deleted %v, want %v", got, want)
	}
	for _, queue := range got {
		if !want[queue] {
			t.Errorf("deleted %s, want only %v", queue, want)
		}
	}
	for _, args := range backend.ctlCalls {
		if args[0] == "delete_queue" && args[len(args)-2] != "--" {
			t.Errorf("delete_queue %v does not end its options with --", args)
		}
	}
}

// TestDeleteMessageQueues_ContinuesAfterAFailedDelete checks that a queue that
// cannot be deleted does not stop the others from being deleted, and that the
// error names the queue and carries rabbitmqctl's message on one line.
func TestDeleteMessageQueues_ContinuesAfterAFailedDelete(t *testing.T) {
	backend := newFakeTransientDataBackend(map[string]string{
		"/": `[{"name":"gone","exclusive":false},{"name":"infrahub.rpcs","exclusive":false}]`,
	}, map[string]bool{"gone": true})
	iops := &InfrahubOps{backend: backend}

	err := iops.deleteMessageQueues()
	if err == nil {
		t.Fatal("deleteMessageQueues() = nil, want an error for the queue that could not be deleted")
	}
	if !strings.Contains(err.Error(), `queue "gone"`) || !strings.Contains(err.Error(), "Error: No such queue was found") {
		t.Errorf("err = %q, want it to name the queue and carry rabbitmqctl's message", err)
	}
	if got := backend.deleteCalls(); !reflect.DeepEqual(got, []string{"/ gone", "/ infrahub.rpcs"}) {
		t.Errorf("deleted %v, want both queues attempted", got)
	}
}

// TestDeleteMessageQueues_StopsWhenTheVhostsCannotBeListed checks that nothing
// is deleted when the virtual hosts cannot be listed.
func TestDeleteMessageQueues_StopsWhenTheVhostsCannotBeListed(t *testing.T) {
	backend := newFakeTransientDataBackend(nil, nil)
	backend.ctl = func(args []string) (string, string, error) {
		return "", "Error: unable to perform an operation on node", errors.New("exit status 69")
	}
	iops := &InfrahubOps{backend: backend}

	err := iops.deleteMessageQueues()
	if err == nil || !strings.Contains(err.Error(), "unable to perform an operation on node") {
		t.Errorf("err = %v, want the list_vhosts failure", err)
	}
	if len(backend.ctlCalls) != 1 {
		t.Errorf("rabbitmqctl calls = %v, want only list_vhosts", backend.ctlCalls)
	}
}

// TestWipeTransientData_DeletesNoFiles checks that the wipe empties the message
// queue through rabbitmqctl and the cache through redis-cli, and never deletes
// files under a running service.
func TestWipeTransientData_DeletesNoFiles(t *testing.T) {
	backend := newFakeTransientDataBackend(map[string]string{
		"/": `[{"name":"infrahub.rpcs","exclusive":false}]`,
	}, nil)
	iops := &InfrahubOps{backend: backend}

	if err := iops.wipeTransientData(); err != nil {
		t.Fatalf("wipeTransientData() = %v, want nil", err)
	}

	for _, command := range backend.execs {
		switch {
		case command[0] == "message-queue" && command[1] == "rabbitmqctl":
		case command[0] == "cache" && command[1] == "sh" && command[3] == redisCLIScript:
		default:
			t.Errorf("ran %v, want only rabbitmqctl in message-queue and redis-cli in cache", command)
		}
	}
	if got := backend.deleteCalls(); !reflect.DeepEqual(got, []string{"/ infrahub.rpcs"}) {
		t.Errorf("deleted %v, want [/ infrahub.rpcs]", got)
	}
	want := [][]string{
		{"ROLE"},
		{"INFO", "keyspace"},
		{"-n", "0", "EVAL", redisDeleteKeysScript, "0"},
		{"-n", "1", "EVAL", redisDeleteKeysScript, "0"},
	}
	if !reflect.DeepEqual(backend.redisCalls, want) {
		t.Errorf("redis-cli calls = %q, want %q", backend.redisCalls, want)
	}
}

// TestDeleteCacheKeys_SendsTheCommandsToTheMaster checks that the keys of
// every database are deleted on the cache itself when it is the Redis master,
// and on the master that ROLE names when the cache service answers from a
// read-only replica.
func TestDeleteCacheKeys_SendsTheCommandsToTheMaster(t *testing.T) {
	tests := []struct {
		name       string
		role       string
		wantTarget []string
	}{
		{
			name:       "standalone master",
			role:       "master\n0",
			wantTarget: []string{},
		},
		{
			name:       "master with replicas",
			role:       "master\n102\n10.0.0.7\n6379\n102",
			wantTarget: []string{},
		},
		{
			name:       "Sentinel replica",
			role:       "slave\ncache-node-0.cache-headless.infrahub.svc.cluster.local\n6379\nconnected\n1234",
			wantTarget: []string{"-h", "cache-node-0.cache-headless.infrahub.svc.cluster.local", "-p", "6379"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := newFakeTransientDataBackend(nil, nil)
			backend.redis = fakeRedis(tt.role, nil)
			iops := &InfrahubOps{backend: backend}

			if err := iops.deleteCacheKeys(); err != nil {
				t.Fatalf("deleteCacheKeys() = %v, want nil", err)
			}
			if len(backend.redisCalls) != 4 {
				t.Fatalf("redis-cli calls = %q, want ROLE, INFO keyspace and one EVAL per database", backend.redisCalls)
			}
			for _, call := range backend.redisCalls[1:] {
				if got := call[:len(tt.wantTarget)]; !reflect.DeepEqual(got, tt.wantTarget) {
					t.Errorf("redis-cli call %q, want it to start with %q", call, tt.wantTarget)
				}
			}
		})
	}
}

// TestDeleteCacheKeys_ChecksTheReplies checks that an error answer from Redis
// is reported, although redis-cli exits with 0 when Redis answers with an
// error, and that a database whose keys cannot be deleted does not stop the
// others from being emptied.
func TestDeleteCacheKeys_ChecksTheReplies(t *testing.T) {
	tests := []struct {
		name      string
		role      string
		replies   map[string]string
		wantErr   string
		wantCalls int
	}{
		{
			name:      "password required",
			role:      "NOAUTH Authentication required.",
			wantErr:   "NOAUTH Authentication required.",
			wantCalls: 1,
		},
		{
			name:      "replica without a master address",
			role:      "slave",
			wantErr:   "unexpected ROLE reply",
			wantCalls: 1,
		},
		{
			name:      "databases cannot be listed",
			role:      "master\n0",
			replies:   map[string]string{"INFO keyspace": "ERR unknown command 'INFO', with args beginning with: 'keyspace'"},
			wantErr:   "ERR unknown command 'INFO'",
			wantCalls: 2,
		},
		{
			name:      "script refused in one database",
			role:      "master\n0",
			replies:   map[string]string{"-n 0 EVAL": "NOPERM User has no permissions to run the 'eval' command"},
			wantErr:   "database 0",
			wantCalls: 4,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := newFakeTransientDataBackend(nil, nil)
			backend.redis = fakeRedis(tt.role, tt.replies)
			iops := &InfrahubOps{backend: backend}

			err := iops.deleteCacheKeys()
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("deleteCacheKeys() = %v, want an error containing %q", err, tt.wantErr)
			}
			if len(backend.redisCalls) != tt.wantCalls {
				t.Errorf("redis-cli calls = %q, want %d calls", backend.redisCalls, tt.wantCalls)
			}
		})
	}
}

// fakeRedisCLIScript stands in for redis-cli: it prints the password that
// redis-cli would use, then each argument on its own line.
const fakeRedisCLIScript = `#!/bin/sh
echo "auth=${REDISCLI_AUTH-unset}"
for arg; do echo "arg=$arg"; done
`

// TestRedisCLIScript_PassesThePassword runs redisCLIScript with sh and a fake
// redis-cli. It checks that the password set the Bitnami way reaches
// redis-cli, that no password is set without one, and that the arguments
// reach redis-cli unchanged, with no shell expansion.
func TestRedisCLIScript_PassesThePassword(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "redis-cli"), []byte(fakeRedisCLIScript), 0o755); err != nil {
		t.Fatal(err)
	}
	passwordFile := filepath.Join(dir, "redis-password")
	if err := os.WriteFile(passwordFile, []byte("from-file"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"-h", "cache-0", "FLUSHALL", "$(touch injected)", "a b"}
	wantArgs := "arg=-h\narg=cache-0\narg=FLUSHALL\narg=$(touch injected)\narg=a b"

	tests := []struct {
		name     string
		env      []string
		wantAuth string
	}{
		{name: "no password", env: nil, wantAuth: "auth=unset"},
		{name: "REDIS_PASSWORD", env: []string{"REDIS_PASSWORD=from-env"}, wantAuth: "auth=from-env"},
		{name: "REDIS_PASSWORD_FILE", env: []string{"REDIS_PASSWORD_FILE=" + passwordFile, "REDIS_PASSWORD=ignored"}, wantAuth: "auth=from-file"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exec.Command("sh", append([]string{"-c", redisCLIScript, "redis-cli"}, args...)...)
			cmd.Dir = dir
			cmd.Env = append([]string{"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH")}, tt.env...)
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("sh -c redisCLIScript: %v: %s", err, output)
			}
			if want := tt.wantAuth + "\n" + wantArgs; strings.TrimSpace(string(output)) != want {
				t.Errorf("output = %q, want %q", output, want)
			}
			if _, err := os.Stat(filepath.Join(dir, "injected")); err == nil {
				t.Error("an argument was expanded by the shell")
			}
		})
	}
}

func TestParsePrefectMaxLimit(t *testing.T) {
	tests := []struct {
		name      string
		parts     []string
		wantLimit int
		wantOK    bool
	}{
		{
			name:      "default cap in 422 detail",
			parts:     []string{"", "Response: {'detail': 'Invalid limit: must be less than or equal to 200.'}"},
			wantLimit: 200,
			wantOK:    true,
		},
		{
			name:      "non-default cap",
			parts:     []string{"['InfrahubTask'] Client error '422' ... must be less than or equal to 100."},
			wantLimit: 100,
			wantOK:    true,
		},
		{
			name:      "found in error text rather than output",
			parts:     []string{"exit status 1: must be less than or equal to 50.", ""},
			wantLimit: 50,
			wantOK:    true,
		},
		{
			name:      "unrelated error",
			parts:     []string{"exit status 1", "connection refused"},
			wantLimit: 0,
			wantOK:    false,
		},
		{
			name:      "no parts",
			parts:     nil,
			wantLimit: 0,
			wantOK:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotLimit, gotOK := parsePrefectMaxLimit(tt.parts...)
			if gotLimit != tt.wantLimit || gotOK != tt.wantOK {
				t.Errorf("parsePrefectMaxLimit(%q) = (%d, %t), want (%d, %t)",
					tt.parts, gotLimit, gotOK, tt.wantLimit, tt.wantOK)
			}
		})
	}
}

// quiescingBackend is a deployment whose services can be made to report
// themselves running or stopped, and which records every stop and start.
//
// It is what lets FR-026's "asked for is not stopped" distinction be driven
// without a cluster: a scale-down that is recorded and then not honoured is
// exactly what `kubectl scale` returning before its pods terminate looks like.
type quiescingBackend struct {
	bareBackend

	// running answers the status query per service. A service absent from the
	// map is running, which is the state every restore starts from.
	running map[string]bool
	// statusErr fails the status query, standing in for a deployment that
	// cannot say whether a service is up.
	statusErr error
	// ignoreStop leaves a service reporting itself running after it has been
	// stopped, which is a pod that will not terminate.
	ignoreStop map[string]bool
	// startErr fails the scale-up of one named service while every other one
	// still succeeds. That is the shape a transient failure takes — one
	// workload's controller, one admission webhook, one quota — and it is what
	// distinguishes "this service would not start" from "the deployment is
	// unreachable".
	startErr map[string]error

	stopped []string
	started []string
}

func newQuiescingBackend() *quiescingBackend {
	return &quiescingBackend{
		bareBackend: bareBackend{name: "kubernetes"},
		running:     map[string]bool{},
		ignoreStop:  map[string]bool{},
		startErr:    map[string]error{},
	}
}

func (b *quiescingBackend) IsRunning(service string) (bool, error) {
	if b.statusErr != nil {
		return false, b.statusErr
	}
	if state, ok := b.running[service]; ok {
		return state, nil
	}

	return true, nil
}

func (b *quiescingBackend) Stop(services ...string) error {
	b.stopped = append(b.stopped, services...)
	for _, service := range services {
		if !b.ignoreStop[service] {
			b.running[service] = false
		}
	}

	return nil
}

func (b *quiescingBackend) Start(services ...string) error {
	// Recorded before the failure, because the attempt is what is being
	// asserted: a run that gave up after this service must be distinguishable
	// from one that carried on past it.
	b.started = append(b.started, services...)

	errs := []error{}
	for _, service := range services {
		if err, ok := b.startErr[service]; ok {
			errs = append(errs, err)

			continue
		}
		b.running[service] = true
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	return nil
}

func newQuiescingOps(backend *quiescingBackend) *InfrahubOps {
	return &InfrahubOps{config: &Configuration{}, backend: backend, executor: NewCommandExecutor()}
}

// TestConfirmAppContainersQuiesced is FR-026's first half. A scale-down that
// was requested and not honoured must not be read as a quiesced deployment: the
// next step overwrites a database, and a writer still attached to it produces a
// restore that reports success over data something else was writing.
func TestConfirmAppContainersQuiesced(t *testing.T) {
	t.Run("services that actually stopped are confirmed", func(t *testing.T) {
		backend := newQuiescingBackend()
		iops := newQuiescingOps(backend)

		stopped, err := iops.stopAppContainers()
		if err != nil {
			t.Fatalf("stopAppContainers() = %v, want nil", err)
		}
		if len(stopped) != len(appServicesStoppedForBackup) {
			t.Fatalf("stopped = %v, want every running service", stopped)
		}
		if err := iops.confirmAppContainersQuiesced(stopped); err != nil {
			t.Errorf("confirmAppContainersQuiesced() = %v, want nil", err)
		}
	})

	t.Run("a service that will not stop fails the run", func(t *testing.T) {
		backend := newQuiescingBackend()
		backend.ignoreStop["infrahub-server"] = true
		iops := newQuiescingOps(backend)

		stopped, err := iops.stopAppContainers()
		if err != nil {
			t.Fatalf("stopAppContainers() = %v, want nil: the scale-down itself succeeds, which is the whole problem", err)
		}

		if err := iops.confirmAppContainersQuiescedWithin(stopped, 50*time.Millisecond, 5*time.Millisecond); err == nil {
			t.Fatal("confirmAppContainersQuiesced() = nil, want the run stopped before it writes to a database a writer may still hold")
		} else if !strings.Contains(err.Error(), "infrahub-server") {
			t.Errorf("err = %v, want it to name the service that is still running", err)
		}
	})

	t.Run("a status the deployment cannot answer for fails the run", func(t *testing.T) {
		backend := newQuiescingBackend()
		backend.statusErr = errors.New(`pods is forbidden: User "system:serviceaccount:infrahub:backup" cannot list resource "pods"`)
		iops := newQuiescingOps(backend)

		err := iops.confirmAppContainersQuiescedWithin([]string{"cache"}, 50*time.Millisecond, 5*time.Millisecond)

		if err == nil {
			t.Fatal("confirmAppContainersQuiesced() = nil, want the unanswered status to stop the run rather than be read as stopped")
		}
		if !strings.Contains(err.Error(), "cannot confirm") {
			t.Errorf("err = %v, want it to say the deployment could not be confirmed quiesced", err)
		}
	})

	t.Run("a run that stopped nothing has nothing to confirm", func(t *testing.T) {
		backend := newQuiescingBackend()
		if err := newQuiescingOps(backend).confirmAppContainersQuiesced(nil); err != nil {
			t.Errorf("confirmAppContainersQuiesced(nil) = %v, want nil", err)
		}
		if len(backend.started) > 0 || len(backend.stopped) > 0 {
			t.Errorf("the deployment was touched (%v/%v), want nothing asked of it", backend.stopped, backend.started)
		}
	})
}

// TestReturnAppContainersToScale is FR-013 on the failure path: what a restore
// took down goes back up whether or not the restore worked. Before this, a
// restore that failed after quiescing left the deployment scaled to zero with
// the operator's data still in place and nothing serving it.
func TestReturnAppContainersToScale(t *testing.T) {
	t.Run("everything the run stopped is started again", func(t *testing.T) {
		backend := newQuiescingBackend()
		iops := newQuiescingOps(backend)

		stopped, err := iops.stopAppContainers()
		if err != nil {
			t.Fatalf("stopAppContainers() = %v, want nil", err)
		}

		iops.returnAppContainersToScale(stopped)

		for _, service := range stopped {
			if !slices.Contains(backend.started, service) {
				t.Errorf("%s was stopped and not started again, want the deployment returned to its prior scale", service)
			}
		}
	})

	// Prior scale, not "all six": a service that was already down before the
	// restore is not something this run took away, and starting it would be
	// this tool deciding a deployment's shape for it.
	t.Run("a service that was already down is left down", func(t *testing.T) {
		backend := newQuiescingBackend()
		backend.running["task-manager-background-svc"] = false
		iops := newQuiescingOps(backend)

		stopped, err := iops.stopAppContainers()
		if err != nil {
			t.Fatalf("stopAppContainers() = %v, want nil", err)
		}
		if slices.Contains(stopped, "task-manager-background-svc") {
			t.Fatalf("stopped = %v, want the service that was already down left out of it", stopped)
		}

		iops.returnAppContainersToScale(stopped)

		if slices.Contains(backend.started, "task-manager-background-svc") {
			t.Error("a service that was down before the restore was started by it, want its prior scale kept")
		}
	})

	t.Run("a run that stopped nothing starts nothing", func(t *testing.T) {
		backend := newQuiescingBackend()
		newQuiescingOps(backend).returnAppContainersToScale(nil)

		if len(backend.started) > 0 {
			t.Errorf("services started = %v, want none", backend.started)
		}
	})
}

// TestStartAppContainersAttemptsEveryService is FR-013's "every exit path" read
// as what it costs when only some of it holds.
//
// The restart stopped at the first failure, and preferredOrder starts with
// `cache` — so one transient failure on the first service left the other five
// at zero replicas, and returnAppContainersToScale reported it as a single
// logged line naming one of the six. The deployment was down and the message
// said so about one workload.
//
// The services do not depend on each other for *starting*: preferredOrder is
// about the order they come up in, so a service that will not start is not a
// reason to leave the rest stopped.
func TestStartAppContainersAttemptsEveryService(t *testing.T) {
	t.Run("a failure on the first service does not abandon the rest", func(t *testing.T) {
		backend := newQuiescingBackend()
		// `cache` is first in preferredOrder, which is what made this the
		// whole-deployment case rather than an edge one.
		backend.startErr["cache"] = errors.New("Operation cannot be fulfilled on statefulsets.apps \"infrahub-cache\": the object has been modified")
		iops := newQuiescingOps(backend)

		stopped, err := iops.stopAppContainers()
		if err != nil {
			t.Fatalf("stopAppContainers() = %v, want nil", err)
		}

		startErr := iops.startAppContainers(stopped)
		if startErr == nil {
			t.Fatal("startAppContainers() = nil, want the failure reported")
		}

		for _, service := range stopped {
			if !slices.Contains(backend.started, service) {
				t.Errorf("%s was never attempted, want every service this run stopped attempted whatever the others did", service)
			}
		}

		// Five of the six actually came back up; only the one that failed did
		// not. That is the state the operator is left in, and it is the state
		// the message has to describe.
		for _, service := range stopped {
			if service == "cache" {
				continue
			}
			if running, _ := backend.IsRunning(service); !running {
				t.Errorf("%s is not running, want it returned to its prior scale despite cache failing", service)
			}
		}
	})

	t.Run("every failure is named, not just the first", func(t *testing.T) {
		backend := newQuiescingBackend()
		backend.startErr["cache"] = errors.New("cache would not scale")
		backend.startErr["task-worker"] = errors.New("task-worker would not scale")
		iops := newQuiescingOps(backend)

		stopped, err := iops.stopAppContainers()
		if err != nil {
			t.Fatalf("stopAppContainers() = %v, want nil", err)
		}

		startErr := iops.startAppContainers(stopped)
		if startErr == nil {
			t.Fatal("startAppContainers() = nil, want both failures reported")
		}
		for _, want := range []string{"cache", "task-worker"} {
			if !strings.Contains(startErr.Error(), want) {
				t.Errorf("err = %v, want it to name %s: what is still at zero is the whole of what the operator needs", startErr, want)
			}
		}
	})

	t.Run("a deployment that starts cleanly reports no error", func(t *testing.T) {
		backend := newQuiescingBackend()
		iops := newQuiescingOps(backend)

		stopped, err := iops.stopAppContainers()
		if err != nil {
			t.Fatalf("stopAppContainers() = %v, want nil", err)
		}
		if err := iops.startAppContainers(stopped); err != nil {
			t.Errorf("startAppContainers() = %v, want nil", err)
		}
	})
}
