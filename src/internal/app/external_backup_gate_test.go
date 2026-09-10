package app

import (
	"compress/gzip"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// gatedBackend is a deployment whose databases can be placed inside or outside
// it, and which records every service it was asked to stop or start and every
// command it was asked to run.
//
// The recording is the point of the test below: what makes the pre-gate
// behaviour a Principle II violation is not that the run failed, it is that it
// scaled Infrahub to zero on the way to failing.
type gatedBackend struct {
	bareBackend

	// locations answers the location query per service; a service absent from
	// the map is inside the deployment.
	locations map[string]EndpointLocation
	// locateErr fails the location query, standing in for a cluster that did
	// not answer.
	locateErr error

	stopped []string
	started []string
	execs   []string
}

func newGatedBackend() *gatedBackend {
	return &gatedBackend{
		bareBackend: bareBackend{name: "kubernetes"},
		locations:   map[string]EndpointLocation{},
	}
}

func (b *gatedBackend) locateService(service string) (EndpointLocation, error) {
	if b.locateErr != nil {
		return "", b.locateErr
	}
	if location, ok := b.locations[service]; ok {
		return location, nil
	}

	return EndpointLocationInternal, nil
}

func (b *gatedBackend) Exec(service string, command []string, opts *ExecOptions) (string, error) {
	b.execs = append(b.execs, strings.Join(command, " "))

	if len(command) > 0 && command[0] == "cypher-shell" {
		// The edition probe. Community is what an absent container's failed
		// probe defaults to, and it is the branch that stops Infrahub — so a
		// gate that fired late would be visible here as stopped services.
		return "community\n", nil
	}

	return "", nil
}

func (b *gatedBackend) Stop(services ...string) error {
	b.stopped = append(b.stopped, services...)

	return nil
}

func (b *gatedBackend) Start(services ...string) error {
	b.started = append(b.started, services...)

	return nil
}

var (
	_ EnvironmentBackend = (*gatedBackend)(nil)
	_ serviceLocator     = (*gatedBackend)(nil)
)

// newGatedOps is a create run against the supplied deployment, with a backup
// directory of its own.
func newGatedOps(t *testing.T, backend EnvironmentBackend, storage BackendType) *InfrahubOps {
	t.Helper()

	cfg := createRetentionConfig(t.TempDir(), RetentionConfig{})
	cfg.Backend = storage
	cfg.Plakar = &PlakarConfig{RepoPath: t.TempDir()}

	return &InfrahubOps{config: cfg, backend: backend, executor: NewCommandExecutor()}
}

// TestCreateBackupRefusesAnExternalDatabaseBeforeStoppingAnything is the
// regression this gate exists for. An external Neo4j has no container, so the
// edition probe fails and is read as Community; the run then announced a
// ten-second abort window and scaled infrahub-server, task-worker,
// task-manager, task-manager-background-svc, cache and message-queue to zero
// before the capture failed anyway — an outage caused by a misdiagnosis, which
// constitution Principle II forbids.
//
// Both storage backends are covered because CreateBackup and
// CreatePlakarBackup each carried the sequence.
func TestCreateBackupRefusesAnExternalDatabaseBeforeStoppingAnything(t *testing.T) {
	for _, storage := range []BackendType{BackendTarball, BackendPlakar} {
		t.Run(string(storage), func(t *testing.T) {
			backend := newGatedBackend()
			backend.locations[serviceNeo4j] = EndpointLocationExternal
			iops := newGatedOps(t, backend, storage)

			started := time.Now()
			err := iops.CreateBackup(true, "all", false, false, true, 0, false, false, "")
			elapsed := time.Since(started)

			// This deployment cannot host a transient workload — the double is
			// not the real Kubernetes backend — so the run still stops. What
			// changed with the capture path is only *where*: it is no longer the
			// capability that refuses, it is the workload that cannot be built.
			if err == nil {
				t.Fatal("CreateBackup() = nil, want the run stopped where it cannot host a transient workload")
			}
			if !strings.Contains(err.Error(), "transient workload") {
				t.Errorf("err = %v, want it to name the workload the external capture needs", err)
			}

			// Principle II: nothing was stopped on the way to the refusal.
			if len(backend.stopped) > 0 {
				t.Errorf("services stopped = %v, want none: the run took the deployment down before refusing", backend.stopped)
			}
			if len(backend.started) > 0 {
				t.Errorf("services started = %v, want none: nothing was stopped, so nothing needed restarting", backend.started)
			}

			// The edition branch was never reached, so no absent container was
			// read as Community and no abort window was announced.
			for _, call := range backend.execs {
				if strings.HasPrefix(call, "cypher-shell") {
					t.Errorf("the edition probe ran against an absent container (%q), want the location settled first", call)
				}
			}
			if elapsed > 5*time.Second {
				t.Errorf("the run took %v, want the refusal before the Community branch's ten-second abort window", elapsed)
			}
		})
	}
}

// TestPrepareDatabaseCaptureWith covers the gate's own decisions against the
// injected location query, the way resolveEndpointWith is driven.
func TestPrepareDatabaseCaptureWith(t *testing.T) {
	locate := func(locations map[string]EndpointLocation, err error) deploymentQueries {
		return deploymentQueries{
			locate: func(service string) (EndpointLocation, error) {
				if err != nil {
					return "", err
				}
				if location, ok := locations[service]; ok {
					return location, nil
				}

				return EndpointLocationInternal, nil
			},
			discover: func(string) error { return nil },
		}
	}

	t.Run("all-internal databases pass the gate untouched", func(t *testing.T) {
		iops := NewInfrahubOps()
		if err := iops.prepareDatabaseCaptureWith(locate(nil, nil), true); err != nil {
			t.Errorf("prepareDatabaseCaptureWith() = %v, want nil: an in-deployment backup must be unchanged (FR-015)", err)
		}
	})

	t.Run("an external task manager is prepared rather than refused", func(t *testing.T) {
		queries := locate(map[string]EndpointLocation{serviceTaskManagerDB: EndpointLocationExternal}, nil)
		iops := newGatedOps(t, newGatedBackend(), BackendTarball)

		// The gate now hands an external database to the capture path instead of
		// refusing it. Against this double the preparation cannot get as far as a
		// workload, which is what the failure says — and that is the assertion
		// that matters: the run reached the capture path.
		err := iops.prepareDatabaseCaptureWith(queries, true)
		if err == nil {
			t.Fatal("prepareDatabaseCaptureWith() = nil, want the preparation reported")
		}
		if !strings.Contains(err.Error(), "transient workload") {
			t.Errorf("err = %v, want it to name the workload the external capture needs", err)
		}
	})

	t.Run("a database the run excludes is not gated", func(t *testing.T) {
		// --exclude-task-manager: the run never reads that database, so where
		// it lives cannot stop the run.
		queries := locate(map[string]EndpointLocation{serviceTaskManagerDB: EndpointLocationExternal}, nil)

		if err := NewInfrahubOps().prepareDatabaseCaptureWith(queries, false); err != nil {
			t.Errorf("prepareDatabaseCaptureWith() = %v, want nil for a database the run excludes", err)
		}
	})

	t.Run("a location that could not be established is an error, not a refusal", func(t *testing.T) {
		queries := locate(nil, fmt.Errorf(`pods is forbidden: User "system:serviceaccount:infrahub:backup" cannot list resource "pods"`))

		err := NewInfrahubOps().prepareDatabaseCaptureWith(queries, true)
		if err == nil {
			t.Fatal("prepareDatabaseCaptureWith() = nil, want the unanswered location query reported")
		}
		if !strings.Contains(err.Error(), "is forbidden") {
			t.Errorf("err = %v, want it to keep the reason kubectl gave", err)
		}
	})
}

// TestLocateServiceInFallsBackToInternal pins the Docker Compose half of
// FR-015: a backend that cannot answer the location question is not asked to,
// and its deployment stays on exactly the path it is on today.
func TestLocateServiceInFallsBackToInternal(t *testing.T) {
	location, err := locateServiceIn(&bareBackend{name: "docker"})(serviceNeo4j)
	if err != nil {
		t.Fatalf("locateServiceIn failed: %v", err)
	}
	if location != EndpointLocationInternal {
		t.Errorf("location = %q, want %q for a backend with no location query", location, EndpointLocationInternal)
	}
}

// TestRestoreRefusesAnExternalDatabaseBeforeTouchingTheDeployment is the same
// regression on the path where it costs the most. An external Neo4j restore
// misdiagnosed the edition, wiped transient data, scaled six services to zero,
// half-restored PostgreSQL, and then ran `neo4j-admin database load
// --overwrite-destination=true` inside whatever pod the name fallback resolved
// — leaving the deployment down and its task-manager database partly
// overwritten, having never touched the database the operator meant to restore.
//
// All three entry points are covered because each reaches the destructive
// sequence on its own: RestoreBackup carries it, RestorePlakarBackup is not
// reached through RestoreBackup's gate but through the delegation at its first
// line, and RestoreLatestBackup selects an archive and hands it on.
func TestRestoreRefusesAnExternalDatabaseBeforeTouchingTheDeployment(t *testing.T) {
	// A gzip stream with nothing restorable in it. The archive has to get past
	// the format sniff, and then never be extracted: the refusal comes before
	// anything is read out of it.
	writeArchive := func(t *testing.T, iops *InfrahubOps) string {
		t.Helper()

		path := filepath.Join(iops.config.BackupDir, backupNameAt(retentionNow))
		file, err := os.Create(path)
		if err != nil {
			t.Fatalf("creating the archive = %v, want nil", err)
		}
		writer := gzip.NewWriter(file)
		if _, err := writer.Write([]byte("not a real archive")); err != nil {
			t.Fatalf("writing the archive = %v, want nil", err)
		}
		if err := writer.Close(); err != nil {
			t.Fatalf("closing the gzip stream = %v, want nil", err)
		}
		if err := file.Close(); err != nil {
			t.Fatalf("closing the archive = %v, want nil", err)
		}

		return path
	}

	assertRefused := func(t *testing.T, backend *gatedBackend, err error) {
		t.Helper()

		if err == nil {
			t.Fatal("the restore = nil, want the external database refused")
		}
		// The refusal is FR-009's, because none of these runs supplied the
		// authorisation. That is now the whole of what an external restore is
		// refused on — the capability itself landed — so this is the gate that
		// must fire before anything is touched.
		if !errors.Is(err, errExternalRestoreUnauthorised) {
			t.Fatalf("err = %v, want it to wrap errExternalRestoreUnauthorised", err)
		}
		for _, want := range []string{serviceNeo4j, "does not run in this deployment", "no data was changed", "--k8s-namespace", AllowExternalRestoreFlag, AllowExternalRestoreEnvVar} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("err = %v, want the message to mention %q", err, want)
			}
		}

		// Principle II, and the reason the gate sits where it does: the
		// discovery its successor needs reads `env` out of infrahub-server and
		// task-manager, which stopAppContainers has scaled to zero.
		if len(backend.stopped) > 0 {
			t.Errorf("services stopped = %v, want none: the restore took the deployment down before refusing", backend.stopped)
		}
		if len(backend.started) > 0 {
			t.Errorf("services started = %v, want none", backend.started)
		}
		// `env` is DetectEnvironment reading the deployment's own credentials,
		// which is read-only and happens on every restore — and is exactly why
		// the gate cannot be moved any later: those reads need infrahub-server
		// and task-manager up. Nothing else may have run.
		for _, call := range backend.execs {
			if strings.HasPrefix(call, "env") {
				continue
			}
			t.Errorf("command %q ran against the deployment, want the location settled first", call)
		}
	}

	t.Run("restore", func(t *testing.T) {
		backend := newGatedBackend()
		backend.locations[serviceNeo4j] = EndpointLocationExternal
		iops := newGatedOps(t, backend, BackendTarball)

		assertRefused(t, backend, iops.RestoreBackup(writeArchive(t, iops), false, false, 0, "", true, false))
	})

	t.Run("restore --latest", func(t *testing.T) {
		backend := newGatedBackend()
		backend.locations[serviceNeo4j] = EndpointLocationExternal
		iops := newGatedOps(t, backend, BackendTarball)
		writeArchive(t, iops)

		assertRefused(t, backend, iops.RestoreLatestBackup(false, false, false, 0, "", true, false))
	})

	t.Run("restore on the plakar backend", func(t *testing.T) {
		backend := newGatedBackend()
		backend.locations[serviceNeo4j] = EndpointLocationExternal
		iops := newGatedOps(t, backend, BackendPlakar)

		// Through RestoreBackup, which delegates on its first line: the gate it
		// carries is never reached on this backend, so RestorePlakarBackup
		// needs its own.
		assertRefused(t, backend, iops.RestoreBackup("ignored-on-this-backend", false, false, 0, "", true, false))
	})

	t.Run("an external task manager is refused too", func(t *testing.T) {
		backend := newGatedBackend()
		backend.locations[serviceTaskManagerDB] = EndpointLocationExternal
		iops := newGatedOps(t, backend, BackendTarball)

		err := iops.RestoreBackup(writeArchive(t, iops), false, false, 0, "", true, false)
		if !errors.Is(err, errExternalRestoreUnauthorised) {
			t.Fatalf("RestoreBackup() = %v, want the refusal", err)
		}
		if !strings.Contains(err.Error(), serviceTaskManagerDB) {
			t.Errorf("err = %v, want it to name %q", err, serviceTaskManagerDB)
		}
		if len(backend.stopped) > 0 {
			t.Errorf("services stopped = %v, want none", backend.stopped)
		}
	})

	// T046's other half: --force is not the authorisation. It is the flag an
	// operator reaching a refused restore reaches for, since it overrides a
	// version mismatch and an incomplete backup group, and every restore above
	// passes it — so if it conferred the capability, none of them would have
	// refused.
	t.Run("--force does not authorise an external restore", func(t *testing.T) {
		backend := newGatedBackend()
		backend.locations[serviceNeo4j] = EndpointLocationExternal
		iops := newGatedOps(t, backend, BackendTarball)

		forced := iops.RestoreBackup(writeArchive(t, iops), false, false, 0, "", true, false)
		if !errors.Is(forced, errExternalRestoreUnauthorised) {
			t.Fatalf("RestoreBackup(force=true) = %v, want the authorisation still required", forced)
		}
	})

	// With the authorisation the run gets past the gate. It still stops —
	// this deployment double cannot host a transient workload — but it stops
	// where the *capture* path stops for the same reason, which is the
	// evidence that the authorisation is what the gate was refusing on rather
	// than the capability.
	t.Run("the authorisation gets the run past the gate", func(t *testing.T) {
		backend := newGatedBackend()
		backend.locations[serviceNeo4j] = EndpointLocationExternal
		iops := newGatedOps(t, backend, BackendTarball)
		iops.config.ExternalDB.Restore = ExternalRestoreAuth{Source: ExternalRestoreAuthFlag}

		err := iops.RestoreBackup(writeArchive(t, iops), false, false, 0, "", true, false)
		if err == nil {
			t.Fatal("RestoreBackup() = nil, want the run stopped where it cannot host a transient workload")
		}
		if errors.Is(err, errExternalRestoreUnauthorised) {
			t.Fatalf("err = %v, want the authorisation to have been accepted", err)
		}
		if !strings.Contains(err.Error(), "transient workload") {
			t.Errorf("err = %v, want it to name the workload the external restore needs", err)
		}

		// Principle II holds either way: an authorised restore that cannot be
		// performed must not take the deployment down on its way to saying so.
		if len(backend.stopped) > 0 {
			t.Errorf("services stopped = %v, want none", backend.stopped)
		}
	})
}

// TestPrepareDatabaseRestoreWith covers the restore gate's own decisions, the
// way TestPrepareDatabaseCaptureWith covers the capture gate's.
func TestPrepareDatabaseRestoreWith(t *testing.T) {
	locate := func(locations map[string]EndpointLocation, err error) deploymentQueries {
		return deploymentQueries{
			locate: func(service string) (EndpointLocation, error) {
				if err != nil {
					return "", err
				}
				if location, ok := locations[service]; ok {
					return location, nil
				}

				return EndpointLocationInternal, nil
			},
			discover: func(string) error { return nil },
		}
	}

	t.Run("all-internal databases pass the gate untouched", func(t *testing.T) {
		if err := NewInfrahubOps().prepareDatabaseRestoreWith(locate(nil, nil), true); err != nil {
			t.Errorf("prepareDatabaseRestoreWith() = %v, want nil: an in-deployment restore must be unchanged (FR-015)", err)
		}
	})

	t.Run("a database the run excludes is not gated", func(t *testing.T) {
		queries := locate(map[string]EndpointLocation{serviceTaskManagerDB: EndpointLocationExternal}, nil)

		if err := NewInfrahubOps().prepareDatabaseRestoreWith(queries, false); err != nil {
			t.Errorf("prepareDatabaseRestoreWith() = %v, want nil for a database the run excludes", err)
		}
	})

	t.Run("a location that could not be established is an error, not a refusal", func(t *testing.T) {
		queries := locate(nil, fmt.Errorf(`pods is forbidden: User "system:serviceaccount:infrahub:backup" cannot list resource "pods"`))

		err := NewInfrahubOps().prepareDatabaseRestoreWith(queries, true)
		if err == nil {
			t.Fatal("prepareDatabaseRestoreWith() = nil, want the unanswered location query reported")
		}
		if errors.Is(err, errExternalRestoreUnauthorised) {
			t.Errorf("err = %v, want the cluster failure rather than a claim about where the database lives (FR-002)", err)
		}
		if !strings.Contains(err.Error(), "cannot start the restore") {
			t.Errorf("err = %v, want it to say the restore never started", err)
		}
	})

	t.Run("an unauthorised external restore refuses and a capture does not", func(t *testing.T) {
		// They are different capabilities: an external restore additionally
		// needs FR-009's authorisation, and a capture must not be made to ask
		// for one it has no business requiring.
		queries := locate(map[string]EndpointLocation{serviceNeo4j: EndpointLocationExternal}, nil)
		iops := newGatedOps(t, newGatedBackend(), BackendTarball)

		restoreErr := iops.prepareDatabaseRestoreWith(queries, true)
		if !errors.Is(restoreErr, errExternalRestoreUnauthorised) {
			t.Errorf("restore = %v, want the authorisation required", restoreErr)
		}

		captureErr := iops.prepareDatabaseCaptureWith(queries, true)
		if errors.Is(captureErr, errExternalRestoreUnauthorised) {
			t.Errorf("the capture failure %v asks for the restore authorisation", captureErr)
		}
	})

	t.Run("an authorised external restore is prepared rather than refused", func(t *testing.T) {
		queries := locate(map[string]EndpointLocation{serviceNeo4j: EndpointLocationExternal}, nil)
		iops := newGatedOps(t, newGatedBackend(), BackendTarball)
		iops.config.ExternalDB.Restore = ExternalRestoreAuth{Source: ExternalRestoreAuthConfig}

		err := iops.prepareDatabaseRestoreWith(queries, true)
		if errors.Is(err, errExternalRestoreUnauthorised) {
			t.Fatalf("prepareDatabaseRestoreWith() = %v, want the authorisation accepted", err)
		}
		// It fails on the workload this deployment double cannot host, which is
		// the preparation having been attempted rather than refused.
		if err == nil || !strings.Contains(err.Error(), "transient workload") {
			t.Errorf("prepareDatabaseRestoreWith() = %v, want the failure to name the transient workload", err)
		}
	})
}

// TestDatabaseTargetWithIsTheOnlyWayToAReachableTarget is T083's structural
// claim. Reachability was a step each entry point performed, which is why all
// three restore entry points shipped without one while both capture entry
// points had one. It is now a property of the target: databaseTargetWith is the
// only constructor, and it hands back a target only where the in-deployment
// paths can operate on the database it names.
func TestDatabaseTargetWithIsTheOnlyWayToAReachableTarget(t *testing.T) {
	queries := func(location EndpointLocation, err error) deploymentQueries {
		return deploymentQueries{
			locate:   func(string) (EndpointLocation, error) { return location, err },
			discover: func(string) error { return nil },
		}
	}

	authorised := ExternalRestoreAuth{Source: ExternalRestoreAuthFlag}

	t.Run("an internal database yields the target every existing path uses", func(t *testing.T) {
		for _, operation := range []databaseOperation{databaseCapture, databaseRestore} {
			// Deliberately unauthorised: FR-009's authorisation is about
			// databases the deployment does not manage, and requiring it for
			// an in-deployment restore would break every existing one (FR-015).
			target, err := databaseTargetWith(queries(EndpointLocationInternal, nil), operation, serviceNeo4j, ExternalRestoreAuth{})
			if err != nil {
				t.Fatalf("databaseTargetWith(%s) = %v, want a target: an in-deployment run must be unchanged (FR-015)", operation, err)
			}
			if target.Service != serviceNeo4j || target.Location != EndpointLocationInternal || target.Operation != operation {
				t.Errorf("target = %+v, want %s/%s/%s", target, serviceNeo4j, EndpointLocationInternal, operation)
			}
			if err := target.reachable(); err != nil {
				t.Errorf("reachable() = %v on a target that was handed out, want nil", err)
			}
		}
	})

	t.Run("an external database yields a target for either operation", func(t *testing.T) {
		// Both paths exist now, so an external database is reachable for each
		// of them — a capture unconditionally, a restore on the authorisation.
		for _, operation := range []databaseOperation{databaseCapture, databaseRestore} {
			target, err := databaseTargetWith(queries(EndpointLocationExternal, nil), operation, serviceNeo4j, authorised)
			if err != nil {
				t.Fatalf("databaseTargetWith(%s) = %v, want a target: the external %s path exists", operation, err, operation)
			}
			if target.Location != EndpointLocationExternal || target.Operation != operation {
				t.Errorf("target = %+v, want %s/%s", target, EndpointLocationExternal, operation)
			}
			if err := target.reachable(); err != nil {
				t.Errorf("reachable() = %v on a target that was handed out, want nil", err)
			}
		}
	})

	t.Run("a restore refusal says no data was changed", func(t *testing.T) {
		_, restoreErr := databaseTargetWith(queries(EndpointLocationExternal, nil), databaseRestore, serviceNeo4j, ExternalRestoreAuth{})

		if !strings.Contains(restoreErr.Error(), "no data was changed") {
			t.Errorf("restore refusal = %v, want it to say no data was changed: on that path it is the fact that matters most", restoreErr)
		}
	})

	// FR-009: the authorisation is a gate on the target, not a note in a
	// message. An external restore without it refuses differently from one with
	// it, which is what makes --allow-external-restore a flag that does
	// something rather than a flag that is recorded.
	t.Run("an external restore without the authorisation refuses on the authorisation", func(t *testing.T) {
		_, err := databaseTargetWith(queries(EndpointLocationExternal, nil), databaseRestore, serviceNeo4j, ExternalRestoreAuth{})
		if !errors.Is(err, errExternalRestoreUnauthorised) {
			t.Fatalf("err = %v, want %v", err, errExternalRestoreUnauthorised)
		}
		for _, want := range []string{AllowExternalRestoreFlag, AllowExternalRestoreEnvVar, "no data was changed"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("err = %v, want it to name %q", err, want)
			}
		}
		// The two flags an operator would otherwise expect to cover this are
		// named as not covering it, because both of them read as "I accept the
		// risk" elsewhere in this tool (FR-009).
		for _, want := range []string{"--force", "scheduled restore"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("err = %v, want it to say that %q does not confer the authorisation", err, want)
			}
		}

		// With the authorisation the run gets past the gate, and the target is
		// handed out — which is what makes --allow-external-restore a flag that
		// does something rather than one that is recorded.
		target, authorisedErr := databaseTargetWith(queries(EndpointLocationExternal, nil), databaseRestore, serviceNeo4j, authorised)
		if authorisedErr != nil {
			t.Errorf("databaseTargetWith(restore) = %v, want the authorisation accepted", authorisedErr)
		}
		if target.Restore != authorised {
			t.Errorf("target.Restore = %+v, want the authorisation that was supplied", target.Restore)
		}
	})

	t.Run("a location that could not be established names the run, not the database", func(t *testing.T) {
		failed := fmt.Errorf(`pods is forbidden: User "system:serviceaccount:infrahub:backup" cannot list resource "pods"`)

		for operation, want := range map[databaseOperation]string{
			databaseCapture: "cannot start the backup",
			databaseRestore: "cannot start the restore",
		} {
			_, err := databaseTargetWith(queries("", failed), operation, serviceNeo4j, authorised)
			if err == nil {
				t.Fatalf("databaseTargetWith(%s) = nil, want the unanswered location query reported (FR-002)", operation)
			}
			if !strings.Contains(err.Error(), want) {
				t.Errorf("err = %v, want it to say %q", err, want)
			}
			if errors.Is(err, errExternalRestoreUnauthorised) {
				t.Errorf("err = %v, want the cluster failure rather than a claim about where the database lives", err)
			}
		}
	})
}
