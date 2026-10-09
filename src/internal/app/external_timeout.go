package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// Every operation against a database outside the deployment is time-bounded, so
// that an endpoint which completes a handshake and then stops responding cannot
// hang the run (FR-025). The hazard is worst on the restore path, where Infrahub
// is scaled down for the whole of the operation's life: an unbounded stall there
// leaves the deployment down indefinitely, which is precisely the outcome
// constitution Principle II forbids.
//
// The bounding mechanism is not new — CommandExecutor already has
// runCommandContext and its pipe variants, which the collect primitives use.
// What was missing is that the external-database paths went through the
// unbounded runCommand and the unbounded Exec instead. This file is where they
// are routed onto the bounded runner, where the bounds are resolved, and where a
// timeout becomes an operator-facing failure, so that the capture and restore
// steps added later are bounded by construction rather than by remembering to
// be.
//
// The bounds are deliberately not one number. A capture of a large store can
// legitimately stall for a long time — research R1 names checkpointing as a
// known cause — so its bound is the operator's --external-db-timeout, generous
// by default. Nothing else in the feature has that excuse.
const (
	// externalDBControlTimeout bounds a control operation: a cluster or
	// deployment metadata call made on behalf of an external database, such as
	// listing the pods of a namespace, reading a pod's UID, reading a
	// component's environment, or deleting a transient workload. None of them
	// touches the database and none has any reason to take minutes, so the
	// bound is short enough that a wedged API server is reported rather than
	// waited out.
	externalDBControlTimeout = 2 * time.Minute

	// externalDBProbeTimeout bounds an operation the probe workload performs
	// against the database itself.
	//
	// It is far shorter than the capture bound on purpose. The probe reads
	// three facts — the server's version, the utility's version and the store
	// size — and a server that has accepted a connection and cannot answer
	// those is not slow, it is not answering; there is no legitimate probe that
	// takes minutes. Sizing it to the capture bound would let a restore sit
	// scaled down for two hours before saying so. It also stays well below
	// externalDBProbeDeadline, so the tool's own timeout is what reports the
	// stall rather than the cluster removing the pod underneath it.
	externalDBProbeTimeout = 5 * time.Minute

	// minExternalDBBound floors the resolved operation bound.
	//
	// Every other bound in this file is derived from that one by min(), so a
	// bound small enough to be absurd is not a slow operation reported early —
	// it is an operation that cannot complete. A `kubectl get pods` in the
	// location decision takes a fraction of a second against a healthy API
	// server and several against a loaded one, and the first thing to fail
	// under a microsecond bound is the internal-versus-external decision, on
	// an internal deployment that has nothing to do with any of this. The run
	// then aborts advising the operator to raise a flag they never passed.
	//
	// So no configuration resolves below this. It is short enough that an
	// operator who wants a tight bound still gets one, and long enough that
	// the bound expiring means something stalled.
	minExternalDBBound = 30 * time.Second

	// externalDBReadyBoundMargin is added to a readiness wait's own timeout to
	// get the bound on the process performing it.
	//
	// `kubectl wait` is the one cluster call whose duration its own argument
	// sets, and it is a watch: if the API server stops answering mid-watch,
	// kubectl's --timeout is not what returns. The margin is what makes the
	// outer bound fire second rather than first, so a readiness failure is
	// still reported as one.
	externalDBReadyBoundMargin = 2 * time.Minute
)

// externalDBBound is the bound on a single operation against an external
// database: the operator's --external-db-timeout, or the default when the
// configuration carries none.
//
// A non-positive value is read as unset rather than as "no bound". Passing zero
// to context.WithTimeout yields an already-expired context, so taking a missing
// setting literally would fail every operation instantly — the opposite of the
// generous default FR-025 asks for, and a far worse failure than the one it
// exists to prevent.
//
// A positive value is floored at minExternalDBBound. resolveExternalDBTimeout
// already applies that floor where the operator's value is read, so this is the
// same rule stated at the point every bound is derived — a Configuration built
// in code, or one whose Timeout was set through a channel that grows later,
// cannot reach the bounded runners with a value nothing could complete in.
func externalDBBound(cfg *Configuration) time.Duration {
	if cfg == nil || cfg.ExternalDB.Timeout <= 0 {
		return defaultExternalDBTimeout
	}

	return max(minExternalDBBound, cfg.ExternalDB.Timeout)
}

// resolveExternalDBTimeout reads the operator's --external-db-timeout /
// INFRAHUB_EXTERNAL_DB_TIMEOUT value, and is why the configuration channel does
// not go through viper.GetDuration.
//
// viper.GetDuration reads a unit-less value as nanoseconds, so
// INFRAHUB_EXTERNAL_DB_TIMEOUT=3600 — which reads as an hour to anyone who
// writes it — resolves to 3.6µs. Every bound below is derived from it, so the
// first thing to expire is the location decision's `kubectl get pods`, and a
// Kubernetes backup of a deployment whose databases are all internal aborts
// telling the operator to raise a flag they never passed. The flag channel
// rejects the same value outright, because pflag's duration parser requires a
// unit; this is what makes the channel FR-010 requires for unattended runs
// agree with it.
//
// An unparseable value resolves to zero, which the caller reads as "leave the
// bound as it was" — the same reading resolveExternalRestoreAuth gives a
// malformed authorisation. A configuration error is reported and ignored rather
// than made fatal: the value bounds operations against an external database,
// and a deployment with none of those must not fail to back up because an
// unrelated variable was misspelt.
func resolveExternalDBTimeout(raw string) (time.Duration, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, nil
	}

	bound, err := time.ParseDuration(trimmed)
	if err != nil {
		return 0, fmt.Errorf(
			"--external-db-timeout / %s is not a duration (%q): it needs a unit — 90s, 30m, 2h, or a combination such as 1h30m. "+
				"A bare number is refused rather than read as nanoseconds, which would bound every operation at a fraction of a millisecond. "+
				"The bound is left at its previous value",
			externalDBTimeoutEnvVar, trimmed)
	}
	if bound <= 0 {
		// Not a bound: see externalDBBound for why zero is read as unset
		// rather than as "no limit".
		return 0, nil
	}
	if bound < minExternalDBBound {
		logrus.Warnf("--external-db-timeout / %s is %v, which is below the %v floor an operation against a database needs; using %v",
			externalDBTimeoutEnvVar, bound, minExternalDBBound, minExternalDBBound)

		return minExternalDBBound, nil
	}

	return bound, nil
}

// externalDBControlBound bounds a control operation. It is the shorter of the
// control bound and the configured operation bound: an operator who lowered
// --external-db-timeout below two minutes has said the whole operation must
// finish sooner than that, and a metadata read allowed to outlive it would make
// the setting a lie.
func externalDBControlBound(cfg *Configuration) time.Duration {
	return min(externalDBControlTimeout, externalDBBound(cfg))
}

// externalDBProbeBound bounds an operation the probe performs, capped against
// the configured bound the same way and for the same reason.
func externalDBProbeBound(cfg *Configuration) time.Duration {
	return min(externalDBProbeTimeout, externalDBBound(cfg))
}

// isCommandTimeout reports whether an error is a bound expiring rather than the
// operation failing on its own terms. Both shapes count: the executor's
// *timeoutError, and a bare context deadline from a path that bounds something
// other than a subprocess.
func isCommandTimeout(err error) bool {
	var timeout *timeoutError

	return errors.As(err, &timeout) || errors.Is(err, context.DeadlineExceeded)
}

// wrapExternalTimeout turns an expired bound into the failure FR-025 asks for:
// one that says it timed out, names what stalled and what it was talking to,
// states the bound it exceeded, and names the setting that changes it.
//
// Anything that is not a timeout is returned untouched. Labelling an ordinary
// failure — a refused connection, a permission error — as a stall would send the
// operator after the wrong problem, and this wrapper sits on paths where most
// failures are ordinary.
func wrapExternalTimeout(err error, bound time.Duration, operation, target string) error {
	if !isCommandTimeout(err) {
		return err
	}

	return fmt.Errorf("%s against %s timed out at its %s bound: the operation started and then stopped making progress, so check that the endpoint is still responding — or raise --external-db-timeout if it legitimately needs longer: %w", operation, target, bound, err)
}

// boundedExternalRunner is the timeout-bounded runner every kubectl call made on
// behalf of an external database goes through, in place of the unbounded
// executor.runCommand the shared pod helpers use.
//
// It is shaped as a podRunner so it drops straight into the injection seams the
// package already has — locateServiceWith, getPodForServiceWith,
// transientClusterOps — which is what makes the routing a substitution rather
// than a rewrite. It reads through separatedRunner, so what it returns is the
// command's stdout alone (see there for why a merged stream is a leaked
// Secret); all it adds is FR-025's account of a bound that expired.
func boundedExternalRunner(executor *CommandExecutor, bound time.Duration, target string) podRunner {
	run := separatedRunner(context.Background(), executor, bound)

	return func(name string, args ...string) (string, error) {
		output, err := run(name, args...)

		return output, wrapExternalTimeout(err, bound, describeExternalCommand(name, args), target)
	}
}

// externalDBRunner is boundedExternalRunner bound to this backend's executor.
func (k *KubernetesBackend) externalDBRunner(bound time.Duration, target string) podRunner {
	return boundedExternalRunner(k.executor, bound, target)
}

// describeExternalCommand names a stalled operation by its command and
// subcommand — `kubectl get`, `kubectl wait`, `kubectl delete` — which is enough
// for an operator to tell a wedged API server from a wedged database.
//
// It quotes no further arguments. They are the only place a value could appear,
// and FR-014 keeps credentials off command lines precisely so that a message
// like this one is safe to print; not printing them anyway keeps that true even
// if a future call site is careless.
func describeExternalCommand(name string, args []string) string {
	if len(args) == 0 {
		return name
	}

	return name + " " + args[0]
}

// externalDBTarget names the deployment object an operation was talking to, for
// a timeout raised before any endpoint has been resolved: the location query
// that decides internal from external, and the transient workload's own
// lifecycle. Once an endpoint exists, endpointTarget is the more useful name.
func externalDBTarget(subject, namespace string) string {
	if strings.TrimSpace(subject) == "" {
		return "namespace " + namespace
	}

	return fmt.Sprintf("%s in namespace %s", subject, namespace)
}

// endpointTarget names a resolved endpoint, which is what FR-025 wants a timeout
// to point at: the host list the stalled operation was actually made to. It
// names no credential, for the reason DatabaseEndpoint's own note gives.
func (e DatabaseEndpoint) endpointTarget() string {
	if len(e.Hosts) == 0 {
		return fmt.Sprintf("the external %s database (no address resolved)", e.Service)
	}

	return fmt.Sprintf("the external %s database at %s", e.Service, renderHostPorts(e.Hosts))
}

// execBounded is Exec under a bound, returning the command's stdout alone.
//
// Stdout alone, because every caller reads what comes back as data: an `env`
// dump parsed for the deployment's own database settings today, and a version,
// a store size and a member-role listing when the capture path lands. Merged
// output is not that data — it is that data with whatever the container runtime
// had to say mixed into it, and kubectl has something to say on any pod with
// more than one container (`Defaulted container "x" out of: …`). A notice at
// the top of a version read does not fail; it parses, as a version.
//
// A backend that cannot bound an exec, or cannot keep the streams apart, is
// refused rather than silently falling back — which is what the collect
// primitives do for their optional capabilities. The difference is what a
// fallback would cost here: FR-025's guarantee has to be structural, and a path
// that can quietly lose its bound is a path that can hang a restore with
// Infrahub scaled down, while one that can quietly re-merge the streams is the
// corrupted parse above. Both real backends implement it (see the separateExecer
// assertions in environment_docker.go and environment_kubernetes.go), so the
// refusal is unreachable in a real deployment.
func (iops *InfrahubOps) execBounded(ctx context.Context, bound time.Duration, service string, command []string, opts *ExecOptions) (string, error) {
	backend, err := iops.ensureBackend()
	if err != nil {
		return "", err
	}

	execer, ok := backend.(separateExecer)
	if !ok {
		return "", fmt.Errorf("cannot run a time-bounded %s command: the %s environment offers no bounded execution with the output streams kept apart, and an unbounded operation against an external database is not permitted", service, backend.Name())
	}

	stdout, stderr, err := execer.ExecSeparateContext(ctx, bound, service, command, opts)
	if err != nil {
		return stdout, withStderr(err, stderr)
	}

	return stdout, nil
}

// withStderr puts a failed command's own account of itself back into its error.
//
// Keeping the streams apart is about the payload, not about discarding the
// diagnostics: merged output is where a failure's reason used to be readable,
// and dropping it would trade a corrupted parse for an unexplained failure.
// Only the error path reads stderr, so a command that succeeded while writing a
// warning still yields its payload alone.
func withStderr(err error, stderr string) error {
	trimmed := strings.TrimSpace(stderr)
	if err == nil || trimmed == "" {
		return err
	}

	return fmt.Errorf("%w: %s", err, trimmed)
}

// execBoundedAgainst runs a command in a service's container under the supplied
// bound and reports a timeout as a failure naming what it was talking to.
//
// This is the seam every external-database command step goes through in place of
// the unbounded Exec. The capture and restore steps added later MUST call this
// rather than Exec whenever the database they operate on is external: the
// 30-minute stream idle timeout in backup_neo4j.go guards the data stream only,
// and the command run before it — separated there so its logs do not contaminate
// the stream — is exactly the step FR-025 found unbounded.
func (iops *InfrahubOps) execBoundedAgainst(bound time.Duration, operation, target, service string, command []string, opts *ExecOptions) (string, error) {
	// One deadline over the whole operation rather than one per phase.
	//
	// A bounded exec is two cluster calls, not one: the backend resolves the
	// service to a pod and then execs into it, and it applies the bound it was
	// handed to each of them separately. So a resolution that took the entire
	// bound and then answered left a second, full bound for the exec — and the
	// operation ran for twice the number its own timeout message reports, which
	// is the FR-025 guarantee stated wrongly rather than merely loosely.
	//
	// A context deadline is what makes the two phases share the budget: every
	// bound underneath is applied with context.WithTimeout against this one, and
	// that takes whichever of the two expires first.
	ctx, cancel := context.WithTimeout(context.Background(), bound)
	defer cancel()

	output, err := iops.execBounded(ctx, bound, service, command, opts)

	return output, wrapExternalTimeout(err, bound, operation, target)
}

// sleepUntilNextPoll is the pause between two reads of a state a run is waiting
// on. It reports true once the deadline has passed, so the caller gives its own
// verdict; otherwise it sleeps until the next poll or the deadline, whichever
// is sooner, so the last read lands on the deadline rather than an interval
// past it.
func sleepUntilNextPoll(deadline time.Time, interval time.Duration) (expired bool) {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return true
	}

	time.Sleep(min(remaining, interval))

	return false
}

// transientWorkloadTarget names a service's transient workload the way a
// timeout report names what it was waiting on.
func transientWorkloadTarget(service string) string {
	return "the transient " + service + " workload"
}

// removeTransientScratch clears what a run staged in a transient workload's
// scratch volume. It is best-effort: the workload is about to be given back and
// its volume goes with it, so a failure here costs nothing (FR-011, FR-023).
// what names the staged content for the timeout report and the debug line.
func (iops *InfrahubOps) removeTransientScratch(service, what string, opts *ExecOptions, paths ...string) {
	if _, err := iops.execBoundedAgainst(
		externalDBControlBound(iops.config), "removing "+what, transientWorkloadTarget(service), service,
		append([]string{"rm", "-rf"}, paths...), opts,
	); err != nil {
		logrus.Debugf("Could not remove %s from the transient %s workload; its scratch volume is removed with it: %v", what, service, err)
	}
}
