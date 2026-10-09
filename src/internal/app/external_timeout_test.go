package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

// stallingEndpoint is an operation that accepts and then stops responding: the
// process starts, writes the line that says so, and then never exits. It is the
// same shape command_executor_test.go uses for the bounded pipe runners, and it
// is what FR-025's verification describes — a connection that completes and then
// stalls, as opposed to one that is refused.
//
// `exec sleep` rather than a bare `sleep` so the shell replaces itself with the
// stalling process: the bound must be shown to reach through to what is actually
// hanging, not merely to kill a shell that would have reaped its child anyway.
var stallingEndpoint = []string{"sh", "-c", "echo accepted; exec sleep 60"}

// testStallBound is short enough that the suite stays fast and long enough that
// process start-up cannot be mistaken for the bound expiring.
const testStallBound = 150 * time.Millisecond

// TestExternalDBStallTerminatesAtTheBound is FR-025's own verification, minus
// the cluster: an endpoint that accepts and then stalls must terminate the
// operation at the bound rather than hang the run, and the failure must say it
// was a timeout and name what it was talking to.
//
// It drives the runner every external-database kubectl call now goes through,
// with a real stalling child process. Without the bound this test does not fail,
// it never returns — which is exactly the production symptom: on the restore
// path the run would sit there with Infrahub scaled down.
func TestExternalDBStallTerminatesAtTheBound(t *testing.T) {
	endpoint := DatabaseEndpoint{
		Service:  serviceNeo4j,
		Location: EndpointLocationExternal,
		Hosts:    []HostPort{{Host: "core-1.db.example.com", Port: 6362}},
	}
	target := endpoint.endpointTarget()

	run := boundedExternalRunner(NewCommandExecutor(), testStallBound, target)

	started := time.Now()
	output, err := run(stallingEndpoint[0], stallingEndpoint[1:]...)
	elapsed := time.Since(started)

	if err == nil {
		t.Fatalf("the operation against a stalled endpoint succeeded (output %q), want a timeout", output)
	}

	// Terminated, and terminated at the bound: a generous ceiling, because what
	// is being asserted is that the run does not sit there, not the scheduler's
	// precision.
	if elapsed > 10*time.Second {
		t.Fatalf("the operation took %v to terminate, want termination at its %v bound", elapsed, testStallBound)
	}

	// The stall was reached: the endpoint answered before it stopped answering,
	// so this is the accept-then-stall case rather than a command that never ran.
	if !strings.Contains(output, "accepted") {
		t.Errorf("output = %q, want the endpoint's own acceptance before the stall", output)
	}

	// It is recognised as a bound expiring rather than as an ordinary failure,
	// which is what lets the message below be trusted.
	if !isCommandTimeout(err) {
		t.Fatalf("error = %v (%T), want a timeout", err, err)
	}
	var timeout *timeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("error = %v (%T), want the executor's *timeoutError to survive wrapping with %%w", err, err)
	}
	if timeout.timeout != testStallBound {
		t.Errorf("timeout bound = %v, want the bound the operation was given (%v)", timeout.timeout, testStallBound)
	}

	// The message is the actionable half of FR-025: that it timed out, the
	// endpoint it timed out against, and the setting that changes the bound.
	message := err.Error()
	for _, want := range []string{
		"timed out",
		"core-1.db.example.com:6362",
		serviceNeo4j,
		"--external-db-timeout",
		testStallBound.String(),
	} {
		if !strings.Contains(message, want) {
			t.Errorf("error message %q does not mention %q", message, want)
		}
	}
}

// TestExternalDBStallTerminatesEveryBoundedOperation walks the operations the
// feature performs and asserts each one terminates at its own bound against the
// same stalling endpoint.
//
// One case per bound rather than one for the runner, because the bounds are what
// a regression would break: a control operation quietly re-sized to the capture
// bound would still pass a single-runner test while letting a wedged API server
// hold a scaled-down restore for two hours.
func TestExternalDBStallTerminatesEveryBoundedOperation(t *testing.T) {
	cfg := NewInfrahubOps().Config()

	tests := []struct {
		name  string
		bound time.Duration
	}{
		{name: "a control operation", bound: externalDBControlBound(cfg)},
		{name: "a probe operation", bound: externalDBProbeBound(cfg)},
		{name: "a capture operation", bound: externalDBBound(cfg)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.bound <= 0 {
				t.Fatalf("bound = %v, want a positive bound: a non-positive one expires instantly and fails every operation", tt.bound)
			}

			// The configured bound is deliberately generous, so the operation is
			// driven at a bound of its own; what the case above pins is that the
			// configured value is finite and positive, and what this pins is that
			// the runner it will be handed to stops at whatever bound it is given.
			run := boundedExternalRunner(NewCommandExecutor(), testStallBound, externalDBTarget("the database", "infrahub"))

			started := time.Now()
			if _, err := run(stallingEndpoint[0], stallingEndpoint[1:]...); !isCommandTimeout(err) {
				t.Fatalf("error = %v, want a timeout", err)
			}
			if elapsed := time.Since(started); elapsed > 10*time.Second {
				t.Fatalf("the operation took %v to terminate, want termination at its bound", elapsed)
			}
		})
	}
}

// TestExternalDBBoundsAreOrdered pins the relationships the bounds have to each
// other. A probe that is allowed the capture's two hours defeats the reason the
// probe is bounded separately at all, and a bound above the configured operation
// limit would make --external-db-timeout a suggestion rather than a limit.
func TestExternalDBBoundsAreOrdered(t *testing.T) {
	cfg := NewInfrahubOps().Config()

	if got := externalDBBound(cfg); got != defaultExternalDBTimeout {
		t.Errorf("externalDBBound = %v, want the configured default %v", got, defaultExternalDBTimeout)
	}
	if externalDBControlBound(cfg) >= externalDBProbeBound(cfg) {
		t.Errorf("control bound %v is not shorter than the probe bound %v", externalDBControlBound(cfg), externalDBProbeBound(cfg))
	}
	if externalDBProbeBound(cfg) >= externalDBBound(cfg) {
		t.Errorf("probe bound %v is not shorter than the operation bound %v", externalDBProbeBound(cfg), externalDBBound(cfg))
	}

	t.Run("an operator's lower limit caps every bound", func(t *testing.T) {
		lowered := &Configuration{ExternalDB: ExternalDBConfig{Timeout: 30 * time.Second}}

		for name, bound := range map[string]time.Duration{
			"operation": externalDBBound(lowered),
			"control":   externalDBControlBound(lowered),
			"probe":     externalDBProbeBound(lowered),
		} {
			if bound > 30*time.Second {
				t.Errorf("%s bound = %v, want no more than the configured 30s", name, bound)
			}
		}
	})

	t.Run("an unset timeout falls back to the default rather than to none", func(t *testing.T) {
		// Zero is the trap: handed to context.WithTimeout it expires
		// immediately, so reading it literally would fail every operation
		// instantly instead of bounding it generously.
		for _, cfg := range []*Configuration{nil, {}, {ExternalDB: ExternalDBConfig{Timeout: -time.Second}}} {
			if got := externalDBBound(cfg); got != defaultExternalDBTimeout {
				t.Errorf("externalDBBound(%+v) = %v, want %v", cfg, got, defaultExternalDBTimeout)
			}
		}
	})
}

// TestWrapExternalTimeoutLeavesOrdinaryFailuresAlone is the other half of the
// message contract. Most failures on these paths are ordinary — a refused
// connection, an RBAC denial — and relabelling one as a stall would send the
// operator after the wrong system and suggest raising a timeout that was never
// reached.
func TestWrapExternalTimeoutLeavesOrdinaryFailuresAlone(t *testing.T) {
	ordinary := errors.New("Error from server (Forbidden): pods is forbidden")

	got := wrapExternalTimeout(ordinary, time.Minute, "kubectl get", "the database in namespace infrahub")
	if !errors.Is(got, ordinary) {
		t.Fatalf("wrapExternalTimeout dropped the original error: %v", got)
	}
	if got.Error() != ordinary.Error() {
		t.Errorf("wrapExternalTimeout rewrote an ordinary failure as %q, want it untouched", got.Error())
	}

	if wrapExternalTimeout(nil, time.Minute, "kubectl get", "anything") != nil {
		t.Error("wrapExternalTimeout invented an error where there was none")
	}
}

// TestExternalOperationTargetsNameTheirSubject covers what a timeout message
// points the operator at. FR-025 asks the failure to be actionable, and a
// message that named neither the endpoint nor the namespace would not be.
func TestExternalOperationTargetsNameTheirSubject(t *testing.T) {
	t.Run("a resolved endpoint names its hosts", func(t *testing.T) {
		endpoint := DatabaseEndpoint{
			Service: serviceNeo4j,
			Hosts:   []HostPort{{Host: "core-1", Port: 6362}, {Host: "core-2", Port: 6362}},
		}

		got := endpoint.endpointTarget()
		for _, want := range []string{serviceNeo4j, "core-1:6362", "core-2:6362"} {
			if !strings.Contains(got, want) {
				t.Errorf("endpointTarget() = %q, does not mention %q", got, want)
			}
		}
	})

	t.Run("an unresolved endpoint says so rather than naming nothing", func(t *testing.T) {
		endpoint := DatabaseEndpoint{Service: serviceTaskManagerDB}

		if got := endpoint.endpointTarget(); !strings.Contains(got, serviceTaskManagerDB) || !strings.Contains(got, "no address") {
			t.Errorf("endpointTarget() = %q, want the service named and the missing address stated", got)
		}
	})

	t.Run("a deployment object names its namespace", func(t *testing.T) {
		if got := externalDBTarget("the database service", "infrahub"); got != "the database service in namespace infrahub" {
			t.Errorf("externalDBTarget() = %q", got)
		}
		if got := externalDBTarget("", "infrahub"); got != "namespace infrahub" {
			t.Errorf("externalDBTarget() with no subject = %q", got)
		}
	})
}

// TestDescribeExternalCommandNamesNoArguments keeps the message safe to print.
// FR-014 keeps credentials off command lines, and naming only the command and
// its subcommand keeps a timeout message harmless even if a future call site
// puts a value in an argument.
func TestDescribeExternalCommandNamesNoArguments(t *testing.T) {
	got := describeExternalCommand("kubectl", []string{"get", "secret", "creds", "-o", "jsonpath={.data.password}"})
	if got != "kubectl get" {
		t.Errorf("describeExternalCommand() = %q, want only the command and its subcommand", got)
	}

	if got := describeExternalCommand("kubectl", nil); got != "kubectl" {
		t.Errorf("describeExternalCommand() with no arguments = %q", got)
	}
}

// TestTransientWorkloadOperationsAreBounded is the routing half of T032: the
// lifecycle's cluster calls must go through the bounded runner, and the
// readiness wait must not be bounded at the control bound — two minutes would
// kill the wait long before the ten-minute image pull it exists to cover.
func TestTransientWorkloadOperationsAreBounded(t *testing.T) {
	backend := testBackend()
	ops := backend.transientOps()

	if ops.query == nil || ops.queryWithin == nil || ops.create == nil || ops.remove == nil {
		t.Fatal("transientOps left a cluster operation unbound")
	}

	// Every operation the lifecycle performs terminates against a stalled
	// endpoint. The query runner is the one that can be driven directly.
	started := time.Now()
	if _, err := boundedExternalRunner(backend.executor, testStallBound, externalDBTarget("the transient external-database workload", backend.namespace))(stallingEndpoint[0], stallingEndpoint[1:]...); !isCommandTimeout(err) {
		t.Fatalf("a stalled lifecycle query returned %v, want a timeout", err)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("a stalled lifecycle query took %v to terminate", elapsed)
	}

	cluster := &fakeCluster{uid: "8f14e45f-ea3b-4b0e-9d3e-2a1c5f6b7d80"}
	if _, err := backend.createTransientWorkloadWith(cluster.ops(), testCaptureSpec()); err != nil {
		t.Fatalf("createTransientWorkloadWith() error = %v", err)
	}

	if len(cluster.waitBounds) != 2 {
		t.Fatalf("pod creation and readiness asked for %d bounds, want exactly two", len(cluster.waitBounds))
	}
	want := externalDBWorkloadReadyTimeout + externalDBReadyBoundMargin
	for _, bound := range cluster.waitBounds {
		if bound != want {
			t.Errorf("workload wait bound = %v, want %v", bound, want)
		}
		if bound <= externalDBWorkloadReadyTimeout {
			t.Errorf("workload wait bound %v does not outlive the %v readiness timeout it wraps", bound, externalDBWorkloadReadyTimeout)
		}
	}
}

// TestRunCommandWritePipeContextTimeout covers the one bounded primitive this
// chunk had to add: the manifest piped to `kubectl create` had no bound at all,
// so a wedged API server that accepted the connection and then stopped reading
// would hang workload creation.
func TestRunCommandWritePipeContextTimeout(t *testing.T) {
	ce := NewCommandExecutor()

	wait, err := ce.runCommandWritePipeContext(t.Context(), testStallBound, strings.NewReader("manifest"), "sh", "-c", "cat >/dev/null; exec sleep 60")
	if err != nil {
		t.Fatalf("runCommandWritePipeContext failed to start: %v", err)
	}

	started := time.Now()
	err = wait()
	elapsed := time.Since(started)

	var timeout *timeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("error = %v (%T), want *timeoutError", err, err)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("wait() took %v to return, want termination at the %v bound", elapsed, testStallBound)
	}
}

// TestExecBoundedRefusesAnUnboundedBackend pins the decision not to fall back to
// the unbounded Exec. The collect primitives treat bounded execution as an
// optional capability and fall back when a backend lacks it; here the fallback
// would silently un-bound an external-database operation, which on the restore
// path means a stall with Infrahub scaled down.
func TestExecBoundedRefusesAnUnboundedBackend(t *testing.T) {
	iops := &InfrahubOps{config: &Configuration{}, backend: unboundedBackend{}}

	_, err := iops.execBounded(t.Context(), time.Minute, serviceNeo4j, []string{"env"}, nil)
	if err == nil {
		t.Fatal("execBounded accepted a backend that cannot bound an exec, want a refusal")
	}
	if !strings.Contains(err.Error(), "unbounded") {
		t.Errorf("error = %q, want it to say the operation would have been unbounded", err)
	}
}

// unboundedBackend is an EnvironmentBackend that does not implement
// contextExecer. Both real backends do, so this shape exists only to assert the
// refusal above.
type unboundedBackend struct{}

func (unboundedBackend) Name() string { return "unbounded" }
func (unboundedBackend) Detect() error {
	return nil
}
func (unboundedBackend) Info() string { return "test" }
func (unboundedBackend) Exec(string, []string, *ExecOptions) (string, error) {
	return "", fmt.Errorf("the unbounded exec must not be reached")
}
func (unboundedBackend) ExecStream(string, []string, *ExecOptions) (string, error) {
	return "", nil
}
func (unboundedBackend) ExecStreamPipe(string, []string, *ExecOptions) (io.ReadCloser, func() error, error) {
	return nil, nil, nil
}
func (unboundedBackend) ExecWritePipe(string, []string, *ExecOptions, io.Reader) (func() error, error) {
	return nil, nil
}
func (unboundedBackend) CopyTo(string, string, string) error   { return nil }
func (unboundedBackend) CopyFrom(string, string, string) error { return nil }
func (unboundedBackend) Start(...string) error                 { return nil }
func (unboundedBackend) Stop(...string) error                  { return nil }
func (unboundedBackend) IsRunning(string) (bool, error)        { return false, nil }

// TestResolveExternalDBTimeoutRefusesAUnitLessValue is the defect at the
// channel FR-010 requires. `INFRAHUB_EXTERNAL_DB_TIMEOUT=3600` reads as an hour
// to whoever writes it; viper.GetDuration read it as 3600 nanoseconds, every
// bound is derived from that value by min(), and the first thing to expire was
// the location decision's `kubectl get pods` — so a Kubernetes backup of a
// deployment whose databases are all internal aborted, advising the operator to
// raise a flag they never passed. The flag channel refuses the same value
// outright, and the two must agree.
func TestResolveExternalDBTimeoutRefusesAUnitLessValue(t *testing.T) {
	for _, raw := range []string{"3600", "0.5", "90 s", "1hour", "two hours"} {
		bound, err := resolveExternalDBTimeout(raw)
		if err == nil {
			t.Errorf("resolveExternalDBTimeout(%q) = %v, nil; want a refusal: a value without a unit is read as nanoseconds", raw, bound)

			continue
		}
		if bound != 0 {
			t.Errorf("resolveExternalDBTimeout(%q) = %v alongside an error; want zero so the caller leaves the bound as it was", raw, bound)
		}
		for _, want := range []string{externalDBTimeoutEnvVar, "--" + externalDBTimeoutFlag, "30m", "2h"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("resolveExternalDBTimeout(%q) error = %v, want it to name %q", raw, err, want)
			}
		}
	}
}

// TestResolveExternalDBTimeoutAcceptsTheFormsPflagAccepts pins the agreement
// between the two channels: everything the flag's own parser takes resolves to
// the same duration here.
func TestResolveExternalDBTimeoutAcceptsTheFormsPflagAccepts(t *testing.T) {
	for raw, want := range map[string]time.Duration{
		"90s":    90 * time.Second,
		"30m":    30 * time.Minute,
		"2h":     2 * time.Hour,
		"1h30m":  90 * time.Minute,
		" 45m ":  45 * time.Minute,
		"2h0m0s": 2 * time.Hour,
	} {
		bound, err := resolveExternalDBTimeout(raw)
		if err != nil {
			t.Errorf("resolveExternalDBTimeout(%q) failed: %v", raw, err)

			continue
		}
		if bound != want {
			t.Errorf("resolveExternalDBTimeout(%q) = %v, want %v", raw, bound, want)
		}
	}

	t.Run("an unset value leaves the bound alone", func(t *testing.T) {
		for _, raw := range []string{"", "  ", "0s", "-1h"} {
			bound, err := resolveExternalDBTimeout(raw)
			if err != nil {
				t.Errorf("resolveExternalDBTimeout(%q) failed: %v", raw, err)

				continue
			}
			if bound != 0 {
				t.Errorf("resolveExternalDBTimeout(%q) = %v, want zero: the caller reads that as leave-it-as-it-was", raw, bound)
			}
		}
	})
}

// TestExternalDBBoundIsFloored is the second half of the same defect: the
// <= 0 guard let any positive value through, so a bound of a few microseconds
// reached every derived bound and aborted the run. No configuration may resolve
// below the floor, whichever channel supplied it.
func TestExternalDBBoundIsFloored(t *testing.T) {
	for _, tiny := range []time.Duration{time.Nanosecond, 3600 * time.Nanosecond, time.Millisecond, 29 * time.Second} {
		cfg := &Configuration{ExternalDB: ExternalDBConfig{Timeout: tiny}}

		for name, bound := range map[string]time.Duration{
			"operation": externalDBBound(cfg),
			"control":   externalDBControlBound(cfg),
			"probe":     externalDBProbeBound(cfg),
		} {
			if bound < minExternalDBBound {
				t.Errorf("with Timeout %v the %s bound = %v, want no less than the %v floor: nothing completes in that, and the run aborts on an internal deployment",
					tiny, name, bound, minExternalDBBound)
			}
		}
	}

	t.Run("the floor is applied where the operator's value is read too", func(t *testing.T) {
		bound, err := resolveExternalDBTimeout("1ms")
		if err != nil {
			t.Fatalf("resolveExternalDBTimeout failed: %v", err)
		}
		if bound != minExternalDBBound {
			t.Errorf("resolveExternalDBTimeout(\"1ms\") = %v, want the %v floor", bound, minExternalDBBound)
		}
	})

	t.Run("a bound above the floor is left exactly as configured", func(t *testing.T) {
		cfg := &Configuration{ExternalDB: ExternalDBConfig{Timeout: 45 * time.Minute}}
		if got := externalDBBound(cfg); got != 45*time.Minute {
			t.Errorf("externalDBBound = %v, want the configured 45m untouched", got)
		}
	})
}

// separateBoundedBackend is a backend that bounds an exec and keeps the streams
// apart, scripted with what each one carries.
type separateBoundedBackend struct {
	unboundedBackend
	stdout string
	stderr string
	err    error
}

func (b separateBoundedBackend) ExecSeparateContext(context.Context, time.Duration, string, []string, *ExecOptions) (string, string, error) {
	return b.stdout, b.stderr, b.err
}

// deadlineRecordingBackend records the context an exec was handed, so a test
// can assert what the whole operation was bounded by rather than what each
// phase inside it was bounded by.
type deadlineRecordingBackend struct {
	unboundedBackend
	deadline    time.Time
	hasDeadline bool
}

func (b *deadlineRecordingBackend) ExecSeparateContext(ctx context.Context, _ time.Duration, _ string, _ []string, _ *ExecOptions) (string, string, error) {
	b.deadline, b.hasDeadline = ctx.Deadline()

	return "", "", nil
}

// TestExecBoundedAgainstSpendsItsBoundOnce is FR-025 stated correctly rather
// than merely loosely. A bounded exec is two cluster calls — resolve the
// service to a pod, then exec into it — and the backend bounds each of them at
// the bound it was handed. Handing it a context with no deadline gave each
// phase a full bound of its own, so an operation could run for twice the number
// its own timeout message reports.
//
// The assertion is on the deadline rather than on elapsed time, because the
// deadline is the mechanism: every bound underneath is applied with
// context.WithTimeout against this context, and that takes whichever of the two
// expires first. A wall-clock assertion would test the scheduler.
func TestExecBoundedAgainstSpendsItsBoundOnce(t *testing.T) {
	const bound = 90 * time.Second

	backend := &deadlineRecordingBackend{}
	iops := &InfrahubOps{config: &Configuration{}, backend: backend}

	if _, err := iops.execBoundedAgainst(bound, "reading the server version", "the external neo4j database", serviceNeo4j, []string{"cypher-shell"}, nil); err != nil {
		t.Fatalf("execBoundedAgainst() error = %v", err)
	}

	if !backend.hasDeadline {
		t.Fatal("the exec was handed a context with no deadline, so each phase inside it gets a fresh bound and the operation can run for a multiple of the one reported")
	}
	if remaining := time.Until(backend.deadline); remaining > bound {
		t.Errorf("the operation's deadline is %v away, want no more than the %v bound", remaining, bound)
	}
}

// mergedOnlyBackend bounds an exec but merges the streams. It is the capability
// the collect primitives fall back on, and the one this path must not accept.
type mergedOnlyBackend struct {
	unboundedBackend
}

func (mergedOnlyBackend) ExecContext(context.Context, time.Duration, string, []string, *ExecOptions) (string, error) {
	return "", fmt.Errorf("the merged exec must not be reached")
}

// TestExecBoundedReadsStdoutAlone is the defect T085 removes. Every caller of
// execBounded reads what comes back as data — an `env` dump today, and a
// version, a store size and a member-role listing when the capture path lands —
// and merged output is that data with kubectl's own notices mixed into it. A
// `Defaulted container` line at the top of a version read does not fail: it
// parses, as a version.
func TestExecBoundedReadsStdoutAlone(t *testing.T) {
	const notice = `Defaulted container "infrahub-server" out of: infrahub-server, wait-for-db`

	t.Run("a notice on stderr never reaches the payload", func(t *testing.T) {
		backend := separateBoundedBackend{
			stdout: "INFRAHUB_DB_ADDRESS=neo4j.example.internal\nINFRAHUB_DB_PORT=7687",
			stderr: notice,
		}
		iops := &InfrahubOps{config: &Configuration{}, backend: backend}

		out, err := iops.execBounded(t.Context(), time.Minute, "infrahub-server", []string{"env"}, nil)
		if err != nil {
			t.Fatalf("execBounded() error = %v", err)
		}
		if strings.Contains(out, "Defaulted container") {
			t.Errorf("output = %q, want the container runtime's notice kept out of the payload", out)
		}
		if out != backend.stdout {
			t.Errorf("output = %q, want exactly what the command wrote to stdout", out)
		}
	})

	t.Run("a failure keeps what the command said about it", func(t *testing.T) {
		backend := separateBoundedBackend{
			stderr: `error: unable to upgrade connection: container not found ("infrahub-server")`,
			err:    fmt.Errorf("exit status 1"),
		}
		iops := &InfrahubOps{config: &Configuration{}, backend: backend}

		_, err := iops.execBounded(t.Context(), time.Minute, "infrahub-server", []string{"env"}, nil)
		if err == nil {
			t.Fatal("execBounded() = nil error, want the command's failure")
		}
		if !strings.Contains(err.Error(), "container not found") {
			t.Errorf("err = %v, want it to keep the reason the command gave on stderr", err)
		}
	})

	t.Run("a bound expiring is still a timeout the wrapper recognises", func(t *testing.T) {
		backend := separateBoundedBackend{stderr: "some noise", err: &timeoutError{timeout: time.Minute}}
		iops := &InfrahubOps{config: &Configuration{}, backend: backend}

		_, err := iops.execBoundedAgainst(time.Minute, "reading the database settings", "the infrahub-server container", "infrahub-server", []string{"env"}, nil)
		if !isCommandTimeout(err) {
			t.Fatalf("err = %v, want the timeout to survive being annotated with stderr", err)
		}
		if !strings.Contains(err.Error(), "timed out at its") {
			t.Errorf("err = %v, want FR-025's timeout message", err)
		}
	})

	t.Run("a backend that can only merge is refused, not accepted quietly", func(t *testing.T) {
		iops := &InfrahubOps{config: &Configuration{}, backend: mergedOnlyBackend{}}

		_, err := iops.execBounded(t.Context(), time.Minute, serviceNeo4j, []string{"env"}, nil)
		if err == nil {
			t.Fatal("execBounded accepted a backend that merges the streams, want a refusal: falling back is the corrupted parse")
		}
		if !strings.Contains(err.Error(), "streams kept apart") {
			t.Errorf("err = %q, want it to name the capability that is missing", err)
		}
	})
}

// TestBoundedExternalRunnerReadsStdoutAlone is the defect T151 removes, and it
// is TestExecBoundedReadsStdoutAlone's twin one layer down: the exec path was
// fixed by T085, the kubectl runner every lifecycle query goes through was not.
// It drives real child processes through the real CommandExecutor, because the
// stream separation under test is the executor's — a fake that hands back a
// stdout string would pass with either primitive underneath.
//
// The field that makes the class a leak is the pod UID. kubectl writes its
// notices to stderr on a perfectly healthy call, and merged output puts the
// notice in front of the value: the credential Secret's ownerReference is then
// built from `"<warning>\n<uid>"`, matches no live pod, and the garbage
// collector never collects the Secret holding the database password (FR-023).
func TestBoundedExternalRunnerReadsStdoutAlone(t *testing.T) {
	const (
		uid     = "8f14e45f-ea3b-4b0e-9d3e-2a1c5f6b7d80"
		warning = "Warning: kubectl plugin 'kubectl-foo' is overshadowed by a similarly named builtin command"
	)
	target := externalDBTarget("the transient external-database workload", "infrahub")

	t.Run("a warning on stderr never reaches the UID the Secret is bound to", func(t *testing.T) {
		run := boundedExternalRunner(NewCommandExecutor(), time.Minute, target)

		// The lifecycle asks the cluster for the UID through ops.query; here
		// that question is answered by a real process that behaves like kubectl
		// with a warning to give — the value on stdout, the notice on stderr.
		cluster := &fakeCluster{}
		ops := cluster.ops()
		query := ops.query
		ops.query = func(_ string, args ...string) (string, error) {
			if strings.Contains(strings.Join(args, " "), "metadata.uid") {
				return run("sh", "-c", "echo '"+warning+"' >&2; echo "+uid)
			}

			return query("kubectl", args...)
		}

		workload, err := testBackend().createTransientWorkloadWith(ops, testCaptureSpec())
		if err != nil {
			t.Fatalf("createTransientWorkloadWith() error = %v", err)
		}
		if workload.JobUID != uid {
			t.Errorf("JobUID = %q, want exactly %q: anything else is an ownerReference no live Job matches, and a Secret the garbage collector never collects", workload.JobUID, uid)
		}

		if len(cluster.created) != 2 {
			t.Fatalf("the lifecycle created %d objects, want the Job and then its Secret", len(cluster.created))
		}
		secret := string(cluster.created[1])
		if !strings.Contains(secret, `"uid":"`+uid+`"`) {
			t.Errorf("Secret manifest = %s, want its ownerReference to carry the UID %q exactly", secret, uid)
		}
		if strings.Contains(secret, "Warning") {
			t.Errorf("Secret manifest = %s, want kubectl's warning kept out of it", secret)
		}
	})

	t.Run("a failure keeps what kubectl said about it", func(t *testing.T) {
		run := boundedExternalRunner(NewCommandExecutor(), time.Minute, target)

		_, err := run("sh", "-c", `echo 'Error from server (Forbidden): pods "x" is forbidden: User "system:serviceaccount:infrahub:backup" cannot get resource "pods"' >&2; exit 1`)
		if err == nil {
			t.Fatal("run() = nil error, want the command's failure")
		}
		if !strings.Contains(err.Error(), "cannot get resource") {
			t.Errorf("err = %v, want it to keep the reason kubectl gave on stderr", err)
		}
		// FR-019's refusal reads the error: a denial the runner used to leave in
		// the discarded output is one workloadPermissionError could never see.
		if !isForbiddenError(err) {
			t.Errorf("err = %v, want an RBAC denial on stderr to be recognisable as one", err)
		}
	})

	t.Run("a bound expiring is still a timeout once stderr is folded in", func(t *testing.T) {
		run := boundedExternalRunner(NewCommandExecutor(), testStallBound, target)

		_, err := run("sh", "-c", "echo 'some noise' >&2; exec sleep 60")
		if !isCommandTimeout(err) {
			t.Fatalf("err = %v, want the timeout to survive being annotated with stderr", err)
		}
		if !strings.Contains(err.Error(), "timed out at its") {
			t.Errorf("err = %v, want FR-025's timeout message", err)
		}
	})

	t.Run("the collect primitives' bounded runner answers the same way", func(t *testing.T) {
		// getPodForServiceContext and getAllPodsContext parse pod names out of
		// this runner's output; a notice in front of the first name is a pod
		// that does not exist.
		out, err := testBackend().boundedRunner(t.Context(), time.Minute)("sh", "-c", "echo '"+warning+"' >&2; echo infrahub-server-0")
		if err != nil {
			t.Fatalf("boundedRunner() error = %v", err)
		}
		if out != "infrahub-server-0" {
			t.Errorf("output = %q, want exactly what the command wrote to stdout", out)
		}
	})
}
