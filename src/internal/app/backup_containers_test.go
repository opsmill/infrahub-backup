package app

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

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
