package app

import (
	"fmt"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// A capture from a database outside the deployment has to settle four things
// before its first byte moves, and none of them can be settled from here: the
// server's version, so the FR-006 gate can compare it against the utility that
// will read it; its edition, because the Community mechanism cannot work
// remotely at all (FR-008); the store size, so the capture's scratch space can
// be sized to the database rather than to a guess (FR-022); and each member's
// role, so the run records what it actually observed about its source (FR-005).
//
// All four are read by a short probe workload, because the only client that can
// ask is one running inside the namespace, and a pod's storage sizing is fixed
// when the pod is created — so the pod that answers "how big is the store" can
// never be the pod sized from the answer. That ordering is why chunk 2d built
// two roles rather than one, and this file is where the two are sequenced.
//
// The probe runs at the gate, before the edition probe and before anything is
// stopped, and the capture workload is created later, around the capture itself.
// Splitting them that way is not tidiness. The edition of an external database
// is not readable by exec'ing a container that is not there, so a run that
// probed later would abort in requireDeterminedEdition before it ever reached a
// capture path; and a capture pod created at the gate would spend the run's
// wait-for-running-tasks window burning the activeDeadlineSeconds that FR-025
// sizes for the capture.
//
// Everything here is reached only for a database this run has positively
// established as external. A deployment whose databases are all internal returns
// from prepareExternalSources before a run identifier is minted, a workload is
// created, a stray is reaped, or a setting of this feature's is consulted
// (FR-015).

// probedFacts is what the probe workload read.
type probedFacts struct {
	// ServerVersion and UtilityVersion are the two halves of the FR-006
	// comparison, both as their source worded them.
	ServerVersion  string
	UtilityVersion string

	// Edition is the server's own edition, lower-cased. It is empty for
	// PostgreSQL, which has no such distinction, and it is what an external
	// Neo4j deployment's capture-method decision is made from — the in-container
	// `cypher-shell` probe every internal run uses cannot answer for a database
	// with no container (FR-008).
	Edition string

	// StoreSizeBytes is the size the server reported, or zero when it reported
	// none. Zero is not a licence to guess a scratch size: it is what makes
	// resolveScratchSize fail naming --external-db-scratch-size (FR-022).
	StoreSizeBytes int64

	// Members are the cluster members hosting the database and their roles, as
	// observed at capture time (FR-005).
	Members []observedMember

	// RolesObserved records that this run asked the server for its members'
	// roles at all, which is a different fact from what the answer was.
	//
	// The two have to stay apart because roleUnknown is a real recorded value:
	// a member the server named but gave no role for, or a role query that
	// failed, both record unknown, and an operator reading that knows the run
	// looked. PostgreSQL is never asked — it has no such notion here — so its
	// roles are *absent* rather than unknown, which is the distinction
	// contracts/artifact-and-metadata.md draws for `source_roles`. Recording
	// unknown for a database whose roles were never sought would claim an
	// observation the run never made.
	RolesObserved bool

	// Answered is the member that responded to the probe. It is carried because
	// it is the one endpoint this run has evidence is reachable, and the
	// PostgreSQL dump is taken from it: indexing Hosts[0] instead, as an earlier
	// version of this path did, discards that evidence and dumps from a member
	// the probe may have just walked past.
	//
	// It is deliberately *not* used for Neo4j's capture, which is given the whole
	// ordered list and reports no such thing (FR-005).
	Answered HostPort
}

// externalSource is what a run established about one external database before
// anything was stopped: where it is, what this run decided about its
// certificate, and what the probe read.
type externalSource struct {
	Endpoint *DatabaseEndpoint
	TLS      externalTLSDecision
	Facts    probedFacts
}

// captureTargets is the endpoint list the capture will use, in try order.
func (s *externalSource) captureTargets() []HostPort {
	return s.Endpoint.captureTargets()
}

// observedRoles maps each endpoint the run supplied to the role observed for it
// at capture time (FR-005). Nothing in it says which endpoint served the
// capture, because with several endpoints supplied that is not exposed by any
// documented interface.
func (s *externalSource) observedRoles() map[string]string {
	return captureRoles(s.captureTargets(), s.Facts.Members)
}

// externalCapture is an externalSource with a workload to run the capture in.
type externalCapture struct {
	*externalSource

	// Pod is the transient workload's pod, named on every exec and every copy
	// this capture makes. It is named rather than registered: publishing it
	// under the service's name is what the reverted version of this path did,
	// and the resolver's cache is replaced wholesale by Start and scaleServices
	// (see ExecOptions.Pod).
	Pod string

	// Bound is the timeout on the capture operation itself (FR-025). It is the
	// operator's --external-db-timeout rather than the probe's much shorter
	// bound, because a capture of a large store can legitimately take hours.
	Bound time.Duration

	// ClientEnv is the environment every client command in this pod needs
	// beyond its credentials — the TLS mode and the trust anchor for it. It is
	// resolved once, when the workload is created, because resolving it writes
	// a certificate into that pod: recomputing it per command would repeat the
	// write, and computing it without the pod is what left `verify-full` set
	// with no anchor to verify against.
	ClientEnv map[string]string

	// release gives back the capture workload. Every path that obtains an
	// externalCapture must call it, including the failing ones (FR-011). The
	// run's own deferred ReleaseTransientWorkloads is the backstop, not the
	// plan.
	release func()
}

// Release gives back the capture workload. It tolerates a nil capture, which is
// what an internal database resolves to, so a caller can defer it
// unconditionally rather than guarding the defer.
func (c *externalCapture) Release() {
	if c == nil || c.release == nil {
		return
	}

	c.release()
}

// execOptions names the capture's workload as the target of one command, with
// any additional environment the client needs beyond its credentials.
//
// The credentials are never here. ExecOptions.Env becomes an `env KEY=VALUE`
// prefix on the exec command line (see prepareCommand), which is exactly the
// exposure FR-014 forbids; the clients read their credentials from the secret
// the transient workload owns.
func (c *externalCapture) execOptions(env map[string]string) *ExecOptions {
	return &ExecOptions{Pod: c.Pod, Env: env}
}

// ---------------------------------------------------------------------------
// Preparing the sources (the gate)
// ---------------------------------------------------------------------------

// prepareExternalSources probes every external database this run will read and
// records what it established, so that the capture paths and the edition
// decision both read one answer rather than asking again.
//
// It returns before doing anything at all when no database is external, which is
// what keeps FR-015 structural rather than promised.
//
// It takes the targets the gate resolved rather than a list of service names,
// because where each database lives is exactly what a target already carries.
// Taking the names meant asking the location question a second time, in a
// second place, with its own wrapping of the failure — the shape this phase
// exists to remove, and here the copy was the one that was not the gate.
func (iops *InfrahubOps) prepareExternalSources(targets databaseTargets) error {
	external := targets.external()
	if len(external) == 0 {
		return nil
	}

	ops, err := iops.externalCaptureOps()
	if err != nil {
		return err
	}

	for _, target := range external {
		if err := iops.prepareExternalSourceWith(ops, target); err != nil {
			return err
		}
	}

	return nil
}

// prepareExternalSourceWith probes one external database.
//
// The order is the requirement. The probe runs first because it is the only
// thing that can answer the questions; the edition refusal and the version gate
// run on what it read, before a capture workload exists at all, because FR-008
// and FR-006 both require their answer before any data is read and before any
// Infrahub service is stopped for a capture that cannot happen.
//
// It takes the target rather than a service name so that the one thing it may
// only be called for — a database the gate established lives outside the
// deployment — is carried by its argument rather than checked by its caller.
// That is only sound while a target cannot be built without passing the gate,
// which is what the first line below requires and what databaseTarget.gated
// makes checkable.
func (iops *InfrahubOps) prepareExternalSourceWith(ops externalCaptureOps, target databaseTarget) error {
	service := target.Service

	endpoint, tls, err := iops.resolveExternalTarget(target, "capture")
	if err != nil {
		return err
	}

	pod, releaseProbe, err := ops.startProbe(service, len(endpoint.Hosts))
	if err != nil {
		return err
	}

	facts, err := ops.probe(pod, endpoint, tls)

	// The probe has answered, or failed to; either way it is given back before
	// anything else happens, so the probe and the capture workload never hold
	// scratch space in the namespace at the same time.
	releaseProbe()

	if err != nil {
		return fmt.Errorf("failed to read what the external %s capture needs from %s: %w", service, endpoint.endpointTarget(), err)
	}

	// FR-008 before FR-006: an edition whose capture mechanism cannot work
	// remotely is refused whatever the versions say, and refusing on the version
	// first would send the operator to change an image that was never the
	// problem.
	if service == serviceNeo4j {
		if err := refuseExternalCommunityCapture(endpoint, facts.Edition); err != nil {
			return err
		}
	}

	// FR-006, at the only point where it can hold: the utility has been read
	// from the image that will run the capture, the server has been read over
	// the wire, and no data has moved and no service has been stopped.
	if err := checkVersionCompatibility(service, facts.UtilityVersion, facts.ServerVersion); err != nil {
		return err
	}

	iops.recordExternalSource(service, &externalSource{Endpoint: endpoint, TLS: tls, Facts: facts})

	return nil
}

// recordExternalSource stores what the probe established for one database.
func (iops *InfrahubOps) recordExternalSource(service string, source *externalSource) {
	if iops.externalSources == nil {
		iops.externalSources = map[string]*externalSource{}
	}

	iops.externalSources[service] = source
}

// externalSourceFor reports what this run established about one database, and
// nil for a database that runs inside the deployment.
//
// A nil return is the internal case and is not an error: every existing path
// reaches an internal database through its own container, and the capture paths
// branch on this rather than on a location they would have to re-resolve.
func (iops *InfrahubOps) externalSourceFor(service string) *externalSource {
	return iops.externalSources[service]
}

// externalDatabaseFor is what every path that branches on where a database
// lives dispatches on: what this run established about a database outside the
// deployment, whichever direction the run is going, and nil for one that lives
// inside it.
//
// It reads both of the run's records because the question does not depend on
// direction. A capture records a source at its gate; a restore records a
// workload whose source is the same probe of the same server; and the edition
// decision, the cluster check and the redaction refusal ask only what that
// probe read. The edition decision used to read the capture's record alone, so
// a restore against an external Neo4j found nothing there and the run stopped
// in requireDeterminedEdition against a database whose edition its own gate had
// already established — the feature's restore path was unreachable (T148). No
// capture-only counterpart is left to pick instead: a path that needs a capture
// *workload* builds it in externalCaptureOps from the source this returns, and
// a path that needs the restore workload holds what externalRestoreFor returns,
// which is a different type.
//
// Those paths used to branch on externalSourceFor alone, which asks "did
// preparation record something?" rather than "where does this database live?".
// The two coincide only for an entry point that ran a gate. Any other one
// leaves both records empty, a nil then reads as internal, and the run execs
// `neo4j-admin` or `pg_dump` inside a container the database does not live in —
// silently, because nothing on that path asks the question again. So the
// location is the authority here: a nil against a database located *external*
// is neither branch, and it is an error naming what was not done rather than
// the in-deployment path taken by default.
//
// It issues no query a deployment did not already make. The gate memoises every
// location it establishes (see locateService), and on Docker Compose the answer
// is internal by construction — so a run that went through the gate reads a
// memo, and an internal deployment's path is unchanged (FR-015).
func (iops *InfrahubOps) externalDatabaseFor(service string) (*externalSource, error) {
	if source := iops.externalSourceFor(service); source != nil {
		return source, nil
	}
	if restore := iops.externalRestores[service]; restore != nil {
		return restore.externalSource, nil
	}

	return nil, iops.refuseUnpreparedExternal(service,
		"nothing was prepared to reach it, so there is no container to run its client in; "+
			"this run reached a database path without the gate that establishes what an external database needs")
}

// refuseUnpreparedExternal is the answer for a database nothing was prepared
// for: nil where the database runs in the deployment, and a refusal where it
// does not — because a run reaching an external database without its
// preparation has skipped the gate. notPrepared names, for the refusal, what
// this run did not do.
//
// It is shared by the capture and restore readers so the location question is
// asked one way, and it issues no query a deployment did not already make: the
// gate memoises every location it establishes (see locateService), and on
// Docker Compose the answer is internal by construction (FR-015).
func (iops *InfrahubOps) refuseUnpreparedExternal(service, notPrepared string) error {
	queries, err := iops.deploymentQueries()
	if err != nil {
		return err
	}

	location, err := queries.locate(service)
	if err != nil {
		return fmt.Errorf("cannot determine whether the %s database runs in this deployment: %w", service, err)
	}

	if location == EndpointLocationExternal {
		return fmt.Errorf("the %s database does not run in this deployment and %s. %s",
			service, notPrepared, externalMisdiagnosisHint(service))
	}

	return nil
}

// resolveExternalTarget is what both preparers do before they create anything:
// require the gate's evidence, resolve the endpoint, and decide TLS for it.
// verb names the operation in the refusal, "capture" or "restore into".
//
// The empty-host assertion is unreachable through ResolveEndpoint, which
// refuses an external endpoint it could not address. It is asserted anyway,
// because every path after it walks the host list and an empty one reaching
// them would be an operation addressed at nothing, reported as done.
func (iops *InfrahubOps) resolveExternalTarget(target databaseTarget, verb string) (*DatabaseEndpoint, externalTLSDecision, error) {
	if err := target.requireGated(verb); err != nil {
		return nil, externalTLSDecision{}, err
	}

	endpoint, err := iops.ResolveEndpoint(target.Service)
	if err != nil {
		return nil, externalTLSDecision{}, err
	}

	if len(endpoint.Hosts) == 0 {
		return nil, externalTLSDecision{}, fmt.Errorf("cannot %s the external %s database: no endpoint was resolved for it", verb, target.Service)
	}

	tls := resolveExternalTLS(target.Service, iops.config.ExternalDB, endpoint.TLS)
	endpoint.Report()
	tls.Report()

	return endpoint, tls, nil
}

// ---------------------------------------------------------------------------
// Opening the capture
// ---------------------------------------------------------------------------

// openExternalCapture creates the workload the capture will run in, sized from
// the store size the probe reported (FR-022), and returns nil for a database
// that runs inside the deployment.
func (iops *InfrahubOps) openExternalCapture(service string) (*externalCapture, error) {
	source := iops.externalSourceFor(service)
	if source == nil {
		return nil, nil
	}

	ops, err := iops.externalCaptureOps()
	if err != nil {
		return nil, err
	}

	pod, release, err := ops.startCapture(service, source.Facts.StoreSizeBytes)
	if err != nil {
		return nil, err
	}

	return &externalCapture{
		externalSource: source,
		Pod:            pod,
		Bound:          externalDBBound(iops.config),
		ClientEnv:      iops.transientClientEnv(service, pod, source.TLS),
		release:        release,
	}, nil
}

// transientClientEnv is the environment the clients in a freshly created
// transient workload need beyond their credentials, installing whatever trust
// material that takes into the pod first.
//
// Neo4j's arm is empty on purpose: its capture speaks the backup protocol,
// which authenticates through the server's own backup SSL policy rather than
// through INFRAHUB_DB_TLS_CA_FILE, so the CA the probe's Bolt clients need has
// nothing to do in the capture pod (see external_ca.go).
func (iops *InfrahubOps) transientClientEnv(service, pod string, tls externalTLSDecision) map[string]string {
	if service == serviceTaskManagerDB {
		return iops.postgresClientEnv(pod, tls)
	}

	return nil
}

// ---------------------------------------------------------------------------
// Binding the sequence to a cluster
// ---------------------------------------------------------------------------

// externalCaptureOps are the things preparing an operation against an external
// database does to the cluster, and the one thing it does to the database, as
// function values.
//
// They are injected rather than called directly so the sequence above — probe
// first, refusal and gate before any data moves, capture workload sized from
// what the probe read — is assertable without a cluster and without a database.
// That sequence is the part FR-006, FR-008 and FR-022 actually constrain.
//
// The restore path shares them rather than growing its own. What it needs from
// the cluster is what a capture needs — the refusal for a run that may not
// create workloads, one run identifier, one sweep of earlier runs' strays, and
// a workload to execute in — and a second copy of that would be a second place
// a run identifier is minted, which is the invariant the reaper rests on.
type externalCaptureOps struct {
	// startProbe creates the probe workload and returns the pod to run in and
	// how to give it back. members is how many endpoints the probe may walk,
	// which is what budgets the pod's deadline.
	startProbe func(service string, members int) (string, func(), error)

	// startCapture creates the capture workload, sized from the store size the
	// probe reported, and returns the pod to run in and how to give it back.
	startCapture func(service string, storeSizeBytes int64) (string, func(), error)

	// startRestore creates the one workload a restore into an external database
	// uses, and returns the pod to run in and how to give it back. It takes no
	// size, because nothing a restore puts through the pod is sized by the
	// server (see restoreWorkloadSpec).
	startRestore func(service string) (string, func(), error)

	// probe reads the facts a capture cannot be built without.
	probe func(pod string, endpoint *DatabaseEndpoint, tls externalTLSDecision) (probedFacts, error)
}

// externalCaptureOps binds the sequence to the detected environment.
//
// External databases are a Kubernetes topology: a Compose deployment has no
// namespace to stand a workload up in, and locateServiceIn answers "internal"
// there by construction, so this is unreachable on Docker rather than refused
// there.
func (iops *InfrahubOps) externalCaptureOps() (externalCaptureOps, error) {
	backend, err := iops.ensureBackend()
	if err != nil {
		return externalCaptureOps{}, err
	}

	k, ok := backend.(*KubernetesBackend)
	if !ok {
		return externalCaptureOps{}, fmt.Errorf("cannot reach a database outside the deployment from a %s deployment: reading or writing one needs a transient workload, which only a Kubernetes namespace can host", backend.Name())
	}

	// Before the reaper, which deletes. This is the first line of the only
	// function in the tool from which a transient object can come, so a run
	// that may not create one is turned back here and nothing in the namespace
	// has been touched.
	if err := iops.refuseForbiddenExternalCapture(k.namespace); err != nil {
		return externalCaptureOps{}, err
	}

	runID, err := iops.externalRunID()
	if err != nil {
		return externalCaptureOps{}, err
	}

	// Strays are reaped once, here, before this run creates the first workload
	// of its own (FR-011).
	//
	// Not earlier: this is the first moment the run knows it will create one at
	// all, and reaping lists pods *and secrets* in the namespace — two calls,
	// and two RBAC verbs — which an all-internal deployment neither needs nor
	// necessarily grants. Putting it on every Kubernetes run would make FR-015's
	// "unchanged in behaviour" false, and would fail runs that work today on
	// clusters where this tool cannot list secrets.
	iops.reapTransientStrays(k, runID)

	start := func(spec transientWorkloadSpec, specErr error) (string, func(), error) {
		if specErr != nil {
			return "", nil, specErr
		}

		workload, err := k.CreateTransientWorkload(spec)
		if err != nil {
			return "", nil, err
		}

		// Asked for rather than read off the struct: ExecutionTarget is where
		// the "only a ready workload may be an execution target" precondition is
		// enforced, and reading PodName directly would be the caller deciding it
		// had been met.
		pod, err := workload.ExecutionTarget()
		if err != nil {
			workload.releaseAfterFailure()

			return "", nil, err
		}

		return pod, func() {
			if err := workload.Release(); err != nil {
				logrus.Warnf("Could not give back the transient %s workload: %v", workload.Service, err)
			}
		}, nil
	}

	return externalCaptureOps{
		startProbe: func(service string, members int) (string, func(), error) {
			return start(probeWorkloadSpec(iops.config, k.namespace, runID, service, members))
		},
		startCapture: func(service string, storeSizeBytes int64) (string, func(), error) {
			return start(captureWorkloadSpec(iops.config, k.namespace, runID, service, storeSizeBytes))
		},
		startRestore: func(service string) (string, func(), error) {
			return start(restoreWorkloadSpec(iops.config, k.namespace, runID, service))
		},
		probe: iops.probeExternalDatabase,
	}, nil
}

// forbidExternalCapture marks this run as one that may not capture a database
// living outside the deployment.
//
// It exists for `infrahub-collect`. That binary is read-only with respect to
// the deployment's workloads and ADR-0003 states so without qualification —
// but `--include-backup` delegates to CreateBackup (ADR-0006), and CreateBackup
// on an external database creates a Pod and a Secret and then deletes them. The
// boundary is drawn here rather than by amending the ADR: a support engineer
// hands the bundle tool to a customer precisely because it provably cannot
// mutate their cluster, and that guarantee is worth more than a backup inside
// the bundle. The accepted cost is that an external-database deployment gets no
// backup collected with its diagnostics; `infrahub-backup create` takes it.
func (iops *InfrahubOps) forbidExternalCapture() {
	iops.externalCaptureForbidden = true
}

// refuseForbiddenExternalCapture is what the operator reads when a run that may
// not create workloads has reached a database that would need one.
//
// It names the condition (a database outside the deployment), the resource (a
// transient Pod and Secret in this namespace) and the action (take the backup
// with infrahub-backup, and re-run the collection without --include-backup),
// which is the three-part shape contracts/cli-surface.md requires of every
// failure on this path.
func (iops *InfrahubOps) refuseForbiddenExternalCapture(namespace string) error {
	if !iops.externalCaptureForbidden {
		return nil
	}

	return fmt.Errorf(
		"a database this run must read does not run in this deployment, and capturing it means creating and then deleting a transient pod and secret in namespace %s; "+
			"infrahub-collect never creates, deletes or scales a workload, so it will not take this backup. "+
			"Take it with `infrahub-backup create`, which performs the external capture, and re-run the collection without --include-backup for the diagnostics themselves",
		namespace)
}

// externalRunID is the identifier every transient object this run creates is
// labelled with, minted once.
//
// Once per run, not once per database. Two external databases produce two
// workloads, and minting an identifier for each would label them as two runs —
// which breaks the invariant the reaper and the create-collision refusal both
// rest on, that a transient workload is adopted by exactly one run, and would
// make each database's own workloads look like strays to the other's sweep.
func (iops *InfrahubOps) externalRunID() (string, error) {
	if iops.externalRun != "" {
		return iops.externalRun, nil
	}

	id, err := newRunID()
	if err != nil {
		return "", err
	}

	iops.externalRun = id

	return id, nil
}

// reapTransientStrays removes what earlier runs left behind, once per run.
//
// A failure to reap is reported and not returned. The objects it would have
// removed are already bounded — a pod by its own activeDeadlineSeconds, a secret
// by the owner reference to that pod — so failing this run over another run's
// litter would trade a working backup for a tidy namespace.
func (iops *InfrahubOps) reapTransientStrays(k *KubernetesBackend, runID string) {
	if iops.transientStraysReaped {
		return
	}
	iops.transientStraysReaped = true

	if err := k.ReapTransientStrays(runID); err != nil {
		logrus.Warnf("Could not remove transient external-database objects left behind by earlier runs; each is bounded by its own deadline and owner reference: %v", err)
	}
}

// releaseTransientWorkloads gives back every transient workload this run
// created. It is the run's exit path for them (FR-011), deferred by every entry
// point that may create one, and it is safe to call more than once — after the
// individual releases the capture paths already make, and after an outer entry
// point's own deferred call, which then finds nothing left to give back.
//
// Every entry point, not one per run: `restore --latest --s3` runs the restore
// gate before its download and RestoreBackup runs it again, and the paths
// between the two — a failed download, a second database that fails after the
// first was recorded — never reach the inner defer.
func (iops *InfrahubOps) releaseTransientWorkloads() {
	k, ok := iops.backend.(*KubernetesBackend)
	if !ok {
		return
	}

	if err := k.ReleaseTransientWorkloads(); err != nil {
		logrus.Warnf("Could not remove every transient external-database workload this run created; each carries its own deadline and the cluster reclaims it: %v", err)
	}
}

// ---------------------------------------------------------------------------
// The probe itself
// ---------------------------------------------------------------------------

// probeExternalDatabase reads the facts from the running probe workload.
//
// Every command it runs is bounded by the probe bound rather than the capture
// bound (FR-025). A server that has accepted a connection and cannot report its
// own version is not slow, and sizing this wait to a capture's is what would let
// a run sit for hours before saying so.
func (iops *InfrahubOps) probeExternalDatabase(pod string, endpoint *DatabaseEndpoint, tls externalTLSDecision) (probedFacts, error) {
	switch endpoint.Service {
	case serviceNeo4j:
		return iops.probeExternalNeo4j(pod, endpoint, tls)
	case serviceTaskManagerDB:
		return iops.probeExternalPostgres(pod, endpoint, tls)
	default:
		return probedFacts{}, errNotADatabaseService("probe", endpoint.Service)
	}
}

func (iops *InfrahubOps) probeExternalNeo4j(pod string, endpoint *DatabaseEndpoint, tls externalTLSDecision) (probedFacts, error) {
	facts := probedFacts{}

	// The deployment's own certificate authority, carried into the workload
	// before the first Bolt connection is opened, so that a server whose
	// certificate a private CA issued can be verified rather than only trusted
	// blind (see external_ca.go). It installs nothing where there is nothing to
	// install, and every Bolt command below runs under whatever it returns.
	opts := &ExecOptions{Pod: pod, Env: iops.installExternalCATrust(pod, tls)}

	utility, err := iops.probeUtilityVersion(opts, endpoint, neo4jUtilityVersionCommand())
	if err != nil {
		return facts, err
	}
	facts.UtilityVersion = utility

	uris := endpoint.boltURIs(tls)
	if len(uris) == 0 {
		return facts, fmt.Errorf("no %s endpoint was resolved to probe", endpoint.Service)
	}

	// The server's version and edition are the one probe result the run cannot
	// proceed without, so they are what tries every member. The rest is read
	// from the member that answered, because a fact one member reports about a
	// database is a fact about the database.
	uri, index, err := iops.probeNeo4jComponents(opts, endpoint, uris, &facts)
	if err != nil {
		return facts, err
	}
	facts.Answered = endpoint.Hosts[index]

	// A store size the server will not report is not fatal here. FR-022 places
	// that failure at sizing time, where the message can name
	// --external-db-scratch-size and where an operator who supplied it is not
	// stopped at all.
	if size, err := iops.probeStoreSize(opts, endpoint, uri); err != nil {
		logrus.Warnf("Could not read the size of the external %s store, so the capture's scratch space cannot be derived from it: %v", endpoint.Service, err)
	} else {
		facts.StoreSizeBytes = size
	}

	// Roles are likewise best-effort: roleUnknown is a value the run records, so
	// a server that will not enumerate its members costs the capture's report a
	// detail rather than costing the run. RolesObserved is set either way — the
	// run asked, and a failed answer is recorded as unknown rather than as
	// nothing having been looked at.
	facts.RolesObserved = true
	if members, err := iops.probeNeo4jMembers(opts, endpoint, uri); err != nil {
		logrus.Warnf("Could not observe the cluster members of the external %s database, so their roles are recorded as %s: %v", endpoint.Service, roleUnknown, err)
	} else {
		facts.Members = members
	}

	return facts, nil
}

// firstAnsweringEndpoint walks a database's endpoints in try order and returns
// the index of the first one that answers *and* answers something the caller
// accepts.
//
// The second half is the whole reason this is one function. Both probes walk a
// member list, and both are the FR-006 gate's only source for the server
// version — but only the Neo4j arm validated what came back. The PostgreSQL arm
// took whatever the exec returned and stopped, so a member that connected and
// answered nothing ended the walk and the version gate then aborted the run
// with a healthy member never tried. That was found once already, before this
// path was rebuilt, and reintroduced by the rebuild; a shared walk is what stops
// it from being found a third time. What the two legitimately differ on is
// accept — how an answer is read and what makes it usable — and that is now the
// only thing they differ on.
//
// A candidate that fails is warned about and walked past, never returned: the
// error the walk ends with is the last failure, so an operator reading it sees
// why the final member did not answer rather than why the first did not.
func firstAnsweringEndpoint[T any](endpoints []T, what, target string, ask func(T) (string, error), accept func(T, string) error) (int, error) {
	var lastErr error

	for index, endpoint := range endpoints {
		output, err := ask(endpoint)
		if err != nil {
			lastErr = err
			logrus.Warnf("Could not read %s from %v, trying the next endpoint: %v", what, endpoint, err)

			continue
		}

		if err := accept(endpoint, output); err != nil {
			lastErr = err
			logrus.Warn(lastErr)

			continue
		}

		return index, nil
	}

	return 0, fmt.Errorf("failed to read %s of %s: %w", what, target, lastErr)
}

// probeNeo4jComponents walks the members in try order until one reports the
// server's version and edition, which is the same "tried in order" behaviour the
// capture's own --from list has. It reports which member answered so the probes
// that follow do not repeat the walk.
func (iops *InfrahubOps) probeNeo4jComponents(opts *ExecOptions, endpoint *DatabaseEndpoint, uris []string, facts *probedFacts) (string, int, error) {
	index, err := firstAnsweringEndpoint(uris, "the server version and edition", endpoint.endpointTarget(),
		func(uri string) (string, error) {
			return iops.execBoundedAgainst(
				externalDBProbeBound(iops.config), "reading the server version", endpoint.endpointTarget(), endpoint.Service,
				neo4jComponentsCommand(uri), opts,
			)
		},
		func(uri, output string) error {
			row := parseCypherRow(output, neo4jVersionColumn, neo4jEditionColumn)
			if row[neo4jVersionColumn] == "" || row[neo4jEditionColumn] == "" {
				return fmt.Errorf("%s answered %q when asked for its version and edition", uri, strings.TrimSpace(output))
			}

			facts.ServerVersion = row[neo4jVersionColumn]
			facts.Edition = strings.ToLower(row[neo4jEditionColumn])

			return nil
		},
	)
	if err != nil {
		return "", 0, err
	}

	return uris[index], index, nil
}

func (iops *InfrahubOps) probeExternalPostgres(pod string, endpoint *DatabaseEndpoint, tls externalTLSDecision) (probedFacts, error) {
	facts := probedFacts{}

	utility, err := iops.probeUtilityVersion(&ExecOptions{Pod: pod}, endpoint, postgresUtilityVersionCommand())
	if err != nil {
		return facts, err
	}
	facts.UtilityVersion = utility

	if len(endpoint.Hosts) == 0 {
		return facts, fmt.Errorf("no %s endpoint was resolved to probe", endpoint.Service)
	}

	opts := &ExecOptions{Pod: pod, Env: iops.postgresClientEnv(pod, tls)}

	index, err := firstAnsweringEndpoint(endpoint.Hosts, "the server version", endpoint.endpointTarget(),
		func(host HostPort) (string, error) {
			return iops.execBoundedAgainst(
				externalDBProbeBound(iops.config), "reading the server version", endpoint.endpointTarget(), endpoint.Service,
				postgresServerVersionCommand(host, endpoint.Database), opts,
			)
		},
		func(host HostPort, output string) error {
			// A member that connects and reports no version is not the member
			// this capture can be taken from: the FR-006 gate compares the
			// server version against the utility's, and an empty one fails it
			// on a member that never said anything rather than on a mismatch.
			version := strings.TrimSpace(output)
			if version == "" {
				return fmt.Errorf("%s answered nothing when asked for its version", host)
			}

			facts.ServerVersion = version

			return nil
		},
	)
	if err != nil {
		return facts, err
	}
	facts.Answered = endpoint.Hosts[index]

	// The size is read from the member that answered, which is also the member
	// the dump will be taken from — so the size sizing the scratch volume and
	// the database being dumped are the same server's.
	output, err := iops.execBoundedAgainst(
		externalDBProbeBound(iops.config), "reading the database size", endpoint.endpointTarget(), endpoint.Service,
		postgresStoreSizeCommand(facts.Answered, endpoint.Database), opts,
	)
	if err != nil {
		logrus.Warnf("Could not read the size of the external %s database, so the capture's scratch space cannot be derived from it: %v", endpoint.Service, err)

		return facts, nil
	}

	size, err := parseStoreSizeValue(output)
	if err != nil {
		logrus.Warnf("Could not read the size of the external %s database, so the capture's scratch space cannot be derived from it: %v", endpoint.Service, err)

		return facts, nil
	}
	facts.StoreSizeBytes = size

	return facts, nil
}

// probeUtilityVersion reads the version of the tooling in the workload's image.
// It touches no database, so a failure here is the image not carrying the
// utility it was selected for — which is worth saying plainly, because the
// remedy is the image flag rather than anything about the server.
func (iops *InfrahubOps) probeUtilityVersion(opts *ExecOptions, endpoint *DatabaseEndpoint, command []string) (string, error) {
	output, err := iops.execBoundedAgainst(
		externalDBProbeBound(iops.config), "reading the utility version", transientWorkloadTarget(endpoint.Service), endpoint.Service,
		command, opts,
	)
	if err != nil {
		return "", fmt.Errorf("failed to read the version of %s in the transient %s workload's image: supply %s with an image that carries it: %w", command[0], endpoint.Service, versionImageFlag(endpoint.Service), err)
	}

	version, err := extractVersionToken(output)
	if err != nil {
		return "", fmt.Errorf("failed to read the version of %s in the transient %s workload's image: %w", command[0], endpoint.Service, err)
	}

	return version, nil
}

// probeStoreSize reads the store size that sizes the capture's scratch space
// (FR-022).
func (iops *InfrahubOps) probeStoreSize(opts *ExecOptions, endpoint *DatabaseEndpoint, uri string) (int64, error) {
	command, err := neo4jStoreSizeCommand(uri, endpoint.Database)
	if err != nil {
		return 0, err
	}

	output, err := iops.execBoundedAgainst(
		externalDBProbeBound(iops.config), "reading the store size", endpoint.endpointTarget(), endpoint.Service,
		command, opts,
	)
	if err != nil {
		return 0, err
	}

	return parseStoreSizeValue(parseCypherScalar(output, neo4jStoreSizeColumn))
}

// probeNeo4jMembers observes which members host the database and in what role
// (FR-005). It records what it saw; it does not record, and cannot observe,
// which of them will serve the capture.
func (iops *InfrahubOps) probeNeo4jMembers(opts *ExecOptions, endpoint *DatabaseEndpoint, uri string) ([]observedMember, error) {
	command, err := neo4jMemberRolesCommand(uri, endpoint.Database)
	if err != nil {
		return nil, err
	}

	output, err := iops.execBoundedAgainst(
		externalDBProbeBound(iops.config), "observing the cluster members", endpoint.endpointTarget(), endpoint.Service,
		command, opts,
	)
	if err != nil {
		return nil, err
	}

	members := parseNeo4jMemberRoles(output)
	if len(members) == 0 {
		return nil, fmt.Errorf("the server reported no members hosting %s", endpoint.Database)
	}

	return members, nil
}

// copyFromTransientWorkload copies an artifact out of a named transient pod,
// bounded like every other call this path makes (FR-025).
//
// It names the pod rather than resolving the service, for the reason
// ExecOptions.Pod exists: the workload stands in for a database with no
// container, so resolving "database" here would find nothing — and registering
// the pod under that name, which is what the reverted version of this path did,
// is undone by any StartServices the run makes.
func (iops *InfrahubOps) copyFromTransientWorkload(pod, src, dest string) error {
	return iops.copyAcrossTransientWorkload(copyOutOfTransientWorkload, pod, src, dest)
}
