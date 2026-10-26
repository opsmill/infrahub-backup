package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// fakeRabbitMQBackend answers the rabbitmqctl calls that deleteMessageQueues
// makes and records every command run in a container. ctl returns the stdout,
// stderr and error of one rabbitmqctl call, given its arguments.
type fakeRabbitMQBackend struct {
	bareBackend
	ctl      func(args []string) (string, string, error)
	execs    [][]string
	ctlCalls [][]string
}

func (f *fakeRabbitMQBackend) Exec(service string, command []string, opts *ExecOptions) (string, error) {
	f.execs = append(f.execs, append([]string{service}, command...))
	return "", nil
}

func (f *fakeRabbitMQBackend) ExecSeparateContext(ctx context.Context, timeout time.Duration, service string, command []string, opts *ExecOptions) (string, string, error) {
	f.execs = append(f.execs, append([]string{service}, command...))
	if service != "message-queue" || len(command) == 0 || command[0] != "rabbitmqctl" {
		return "", "", errors.New("unexpected command")
	}
	f.ctlCalls = append(f.ctlCalls, command[1:])
	return f.ctl(command[1:])
}

// deleteCalls returns the queues deleted, as "vhost name".
func (f *fakeRabbitMQBackend) deleteCalls() []string {
	var deleted []string
	for _, args := range f.ctlCalls {
		if args[0] == "delete_queue" {
			deleted = append(deleted, args[3]+" "+args[len(args)-1])
		}
	}
	return deleted
}

// newFakeRabbitMQBackend serves the given queue list, as list_queues prints it
// in JSON, for each virtual host. A docker compose warning is written to
// stderr on every call, as it is on a deployment that leaves a variable unset.
func newFakeRabbitMQBackend(queues map[string]string, failDelete map[string]bool) *fakeRabbitMQBackend {
	const warning = `WARN[0000] The "INFRAHUB_LICENSE_KEY" variable is not set. Defaulting to a blank string.`
	vhosts := make([]string, 0, len(queues))
	for vhost := range queues {
		vhosts = append(vhosts, `{"name":"`+vhost+`"}`)
	}
	f := &fakeRabbitMQBackend{bareBackend: bareBackend{name: "docker"}}
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
	return f
}

// TestDeleteMessageQueues_DeletesTheQueuesOfEveryVhost checks that the queues
// of every virtual host are deleted, except the exclusive queues of connected
// clients, and that a warning on stderr does not stop the lists from parsing.
func TestDeleteMessageQueues_DeletesTheQueuesOfEveryVhost(t *testing.T) {
	backend := newFakeRabbitMQBackend(map[string]string{
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
	backend := newFakeRabbitMQBackend(map[string]string{
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
	backend := newFakeRabbitMQBackend(nil, nil)
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

// TestWipeTransientData_KeepsTheBrokerFiles checks that the wipe deletes the
// message queue's queues through rabbitmqctl and never deletes files under the
// running broker.
func TestWipeTransientData_KeepsTheBrokerFiles(t *testing.T) {
	backend := newFakeRabbitMQBackend(map[string]string{
		"/": `[{"name":"infrahub.rpcs","exclusive":false}]`,
	}, nil)
	iops := &InfrahubOps{backend: backend}

	if err := iops.wipeTransientData(); err != nil {
		t.Fatalf("wipeTransientData() = %v, want nil", err)
	}

	for _, command := range backend.execs {
		if command[0] == "message-queue" && command[1] != "rabbitmqctl" {
			t.Errorf("ran %v in message-queue, want only rabbitmqctl", command)
		}
	}
	if got := backend.deleteCalls(); !reflect.DeepEqual(got, []string{"/ infrahub.rpcs"}) {
		t.Errorf("deleted %v, want [/ infrahub.rpcs]", got)
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
