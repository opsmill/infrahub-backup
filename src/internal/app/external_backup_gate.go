package app

import (
	"errors"
	"fmt"

	"github.com/sirupsen/logrus"
)

// Where a database lives has to be settled before a backup or restore run
// touches the deployment at all, because every step after that point assumes a
// container to read the database in — or, on the restore side, to write it in.
//
// Without this gate the sequence for an external database is: the edition probe
// execs `cypher-shell` into a container that is not there, the failure is read
// as Community (NewNeo4jEditionInfo's documented fallback), the run announces a
// ten-second abort window, and then scales infrahub-server, task-worker,
// task-manager, task-manager-background-svc, cache and message-queue to zero
// for an offline capture it cannot perform. That is an outage caused by a
// misdiagnosis, followed by a failure — and constitution Principle II
// (NON-NEGOTIABLE) allows a backup no destructive effect on the running
// instance beyond the documented stop/start of containers.
//
// The restore path is the same sequence with the destruction moved earlier and
// made unrecoverable. Without a gate, an external Neo4j restore misdiagnoses the
// edition, wipes transient data, scales six services to zero, half-restores
// PostgreSQL, and then runs `neo4j-admin database load
// --overwrite-destination=true` inside whatever pod the name fallback resolved.
// The deployment is down, its task-manager database has been partly overwritten,
// and the database the operator meant to restore was never touched.
//
// So the location is established first, and a database that is not in the
// deployment stops the run here, with nothing stopped and nothing misreported.

// Both arms of this gate are now implemented, so neither refuses on the
// capability. What each does instead is resolve every database's location and
// then, for the external ones, prepare them — which is the same "settle it
// before anything is stopped" property the refusals had, with the answer being
// a prepared source or a prepared restore rather than an error.
//
// What the restore arm still refuses is an unauthorised one. Restoring into a
// database the deployment does not manage requires the authorisation FR-009
// defines, which a capture does not, and that check is a property of the target
// rather than a step: see databaseTarget.reachable.

// errExternalRestoreUnauthorised is FR-009's refusal: the authorisation to
// write to a database the deployment does not manage was not supplied.
//
// It is a property of the target rather than a step in each restore path, so
// that it is exercised on every external restore whatever entry point reached
// it — which is what stopped it from being forgotten by the path that made such
// a restore possible, the way all three restore entry points once shipped with
// no reachability check at all.
var errExternalRestoreUnauthorised = errors.New("restoring into a database that lives outside the deployment requires an explicit authorisation, which was not supplied")

// databaseServicesForBackup are the databases a run will read or write, in the
// order they are gated. Neo4j is always touched; the task manager's PostgreSQL
// only when the run includes it, so `--exclude-task-manager` is not made to
// fail on a database it never touches.
func databaseServicesForBackup(includeTaskManager bool) []string {
	services := []string{serviceNeo4j}
	if includeTaskManager {
		services = append(services, serviceTaskManagerDB)
	}

	return services
}

// resolveDatabaseTargets resolves every database an operation will touch, in
// the order they are gated, and is the only way a *run* obtains targets. The
// constructor it calls is directly callable within the package, so what makes
// a target evidence rather than a convention is the witness that constructor
// sets (see databaseTarget.gated).
//
// It is one function rather than one per operation because the thing that was
// wrong was never the content of the two gates — it was that there were two,
// and that adding an entry point meant remembering to call the right one. It is
// also the seam: this is where the resolved locations are handed to the external
// capture and restore paths, and where FR-009's authorisation is checked before
// a restore reaches one. That check happens inside databaseTargetWith — before
// the edition branch, and before a single container has been stopped.
//
// The targets are returned, and every caller reads them. They were discarded —
// `if _, err := databaseTargetWith(…)` — which passes a "is it called?" check
// and leaves T083's design with the shape and none of the substance: the
// location the target carried was established here and then asked for again by
// what came next.
func resolveDatabaseTargets(queries deploymentQueries, operation databaseOperation, includeTaskManager bool, auth ExternalRestoreAuth) (databaseTargets, error) {
	services := databaseServicesForBackup(includeTaskManager)
	targets := make(databaseTargets, 0, len(services))
	for _, service := range services {
		target, err := databaseTargetWith(queries, operation, service, auth)
		if err != nil {
			return nil, err
		}
		targets = append(targets, target)
	}

	return targets, nil
}

// prepareDatabaseCapture establishes where every database this run will read
// lives. It runs before the edition probe, before the abort window, and before
// anything is stopped, and it is the only place the run decides that a database
// is reachable the way the in-deployment paths expect.
func (iops *InfrahubOps) prepareDatabaseCapture(includeTaskManager bool) error {
	queries, err := iops.deploymentQueries()
	if err != nil {
		return err
	}

	return iops.prepareDatabaseCaptureWith(queries, includeTaskManager)
}

// prepareDatabaseCaptureWith is prepareDatabaseCapture against the supplied
// queries, following resolveEndpointWith's injection idiom so the gate is
// testable without a cluster.
func (iops *InfrahubOps) prepareDatabaseCaptureWith(queries deploymentQueries, includeTaskManager bool) error {
	// The authorisation is passed on the capture path too, and read on neither:
	// reachable consults it only for a restore. Passing it uniformly is what
	// keeps the constructor's signature from implying that a capture has an
	// authorisation question of its own — it does not (FR-009).
	targets, err := resolveDatabaseTargets(queries, databaseCapture, includeTaskManager, iops.externalRestoreAuth())
	if err != nil {
		return err
	}

	// Every location is now established, so what remains is to learn what a
	// capture of each external database needs before the run can safely go on:
	// its edition, its version, its size and its members. This is the last point
	// at which those answers are free — after it, the edition decision may stop
	// six Infrahub services for a cold capture.
	//
	// The targets are what carries the locations across, rather than the service
	// names plus a second reading of where each one lives.
	return iops.prepareExternalSources(targets)
}

// externalMisdiagnosisHint is the tail every "this database is external"
// refusal carries: the two things that would make an in-deployment database
// look like an external one.
//
// It is shared rather than repeated because it belongs on all of them equally.
// A misdiagnosed namespace is the likeliest reason an operator is reading any
// of these messages, and a refusal that omits it sends them to configure an
// external database they do not have.
func externalMisdiagnosisHint(service string) string {
	return fmt.Sprintf(
		"If it does run in this namespace, the run is looking in the wrong place rather than at an external database: "+
			"check --k8s-namespace, and that its pods carry one of the labels services are resolved by "+
			"(app.kubernetes.io/component, app, component or infrahub/service, set to %q)", service)
}

// prepareDatabaseRestore establishes where every database this restore will
// write lives. It is prepareDatabaseCapture's sibling, and it exists separately
// because a restore has three entry points rather than two and a different
// thing to say when it refuses.
//
// Its placement in each of them is load-bearing: it must run before
// stopAppContainers, because ensureNeo4jDiscovery and ensurePostgresDiscovery
// exec `env` in infrahub-server and task-manager, which that call has just
// scaled to zero — and before wipeTransientData, which is destructive on its
// own.
func (iops *InfrahubOps) prepareDatabaseRestore(includeTaskManager bool) error {
	queries, err := iops.deploymentQueries()
	if err != nil {
		return err
	}

	return iops.prepareDatabaseRestoreWith(queries, includeTaskManager)
}

// prepareDatabaseRestoreWith is prepareDatabaseRestore against the supplied
// queries, following prepareDatabaseCaptureWith's injection idiom so the gate is
// testable without a cluster.
func (iops *InfrahubOps) prepareDatabaseRestoreWith(queries deploymentQueries, includeTaskManager bool) error {
	targets, err := resolveDatabaseTargets(queries, databaseRestore, includeTaskManager, iops.externalRestoreAuth())
	if err != nil {
		return err
	}

	logrus.Debugf("Restore will write to %d database(s): %v", len(targets), targets.services())

	// Every location is established and every external one carries FR-009's
	// authorisation, so what remains is to prepare what a restore into each
	// external database needs before the run can safely go on: the workload it
	// runs in, the trust anchor that workload's client reads, and the server's
	// edition and version. This is the last point at which those are free —
	// after it, the deployment's own components are scaled to zero and the
	// anchor is no longer readable out of them.
	//
	// The targets are what carries the locations across, rather than the
	// service names plus a second reading of where each one lives.
	return iops.prepareExternalRestores(targets)
}

// externalRestoreAuth reads FR-009's authorisation off the configuration,
// tolerating a Configuration that was never built — an absent authorisation is
// the safe reading of "nothing said so", and the one every other channel
// resolves to when it says nothing.
func (iops *InfrahubOps) externalRestoreAuth() ExternalRestoreAuth {
	if iops == nil || iops.config == nil {
		return ExternalRestoreAuth{}
	}

	return iops.config.ExternalDB.Restore
}

// externalRestoreUnauthorised is what an operator who has not authorised the
// restore reads. It names the two channels FR-010 requires — the flag for an
// attended run, the environment variable for an unattended one — and states
// that nothing was stopped and no data was changed, which on this path is the
// fact that matters most.
//
// It says outright that --force does not confer it, because that is the flag an
// operator reaching a refused restore reaches for: --force overrides a version
// mismatch and an incomplete backup group, and an operator who has learnt that
// it means "I know what I am doing" will try it here. Enabling scheduled
// restore does not confer it either, for the same reason FR-009 names both — a
// capability that arrives as a side effect of an unrelated setting is one
// nobody decided to grant.
//
// It also names the two things that would make an in-deployment database look
// external, because a misdiagnosed namespace is the likeliest reason an
// operator is reading this at all, and authorising a restore into a database
// they do not actually have is the wrong remedy.
func externalRestoreUnauthorised(service string) error {
	return fmt.Errorf(
		"the %s database does not run in this deployment, and %w; no Infrahub service was stopped and no data was changed. "+
			"Supply --%s, or set %s, to authorise it — deliberately distinct from --force, which does not confer it, and from enabling scheduled restore, which does not either. %s",
		service, errExternalRestoreUnauthorised, AllowExternalRestoreFlag, AllowExternalRestoreEnvVar,
		externalMisdiagnosisHint(service))
}
