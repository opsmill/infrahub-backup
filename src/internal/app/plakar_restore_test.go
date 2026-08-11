package app

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PlakarKorp/kloset/connectors/exporter"
	"github.com/PlakarKorp/kloset/objects"
)

// lifecycleBackend records every service-lifecycle call a restore makes, in order.
// The order is the point: replacing the databases underneath a running deployment
// is the defect these tests pin, and a fake that only recorded that the steps
// happened could not tell the fixed path from the broken one.
type lifecycleBackend struct {
	bareBackend
	calls []string
	// stopErr fails Stop for the named service, simulating a container that will
	// not quiesce.
	stopErr map[string]error
}

func newLifecycleBackend() *lifecycleBackend {
	return &lifecycleBackend{bareBackend: bareBackend{name: "docker"}, stopErr: map[string]error{}}
}

func (b *lifecycleBackend) Exec(service string, command []string, opts *ExecOptions) (string, error) {
	// cypher-shell invocations are recorded with their query, which is how the
	// deployment-ID reset is told apart from the transient-data wipe.
	if len(command) > 0 && command[0] == "cypher-shell" {
		b.calls = append(b.calls, "exec-cypher:"+command[len(command)-1])
		return "", nil
	}
	b.calls = append(b.calls, "exec:"+service)
	return "", nil
}

func (b *lifecycleBackend) Start(services ...string) error {
	b.calls = append(b.calls, "start:"+strings.Join(services, ","))
	return nil
}

func (b *lifecycleBackend) Stop(services ...string) error {
	b.calls = append(b.calls, "stop:"+strings.Join(services, ","))
	for _, s := range services {
		if err := b.stopErr[s]; err != nil {
			return err
		}
	}
	return nil
}

var _ EnvironmentBackend = (*lifecycleBackend)(nil)

func newLifecycleTestOps(backend EnvironmentBackend) *InfrahubOps {
	iops := NewInfrahubOps()
	iops.backend = backend
	return iops
}

// snapshotsFor builds a plan's component snapshots with distinguishable MACs.
func snapshotsFor(components ...string) []SnapshotInfo {
	out := make([]SnapshotInfo, 0, len(components))
	for i, c := range components {
		var mac objects.MAC
		mac[0] = byte(i + 1)
		out = append(out, SnapshotInfo{Component: c, MAC: mac})
	}
	return out
}

// main quiesced the application tier, wiped the transient state that describes the
// database being replaced, restored the task-manager database with nothing
// connected to it, brought its dependencies back, and only then replaced Neo4j —
// restarting infrahub-server and task-worker at the end. The runner rewrite kept
// none of it. This pins the whole sequence.
func TestRestoreComponentsRunsTheDeploymentLifecycle(t *testing.T) {
	backend := newLifecycleBackend()
	iops := newLifecycleTestOps(backend)

	var restored []string
	plan := restorePlan{backupID: "20260810_020000", snapshots: snapshotsFor(ComponentNeo4j, ComponentPostgres, ComponentMetadata)}
	if err := iops.restoreComponents(plan, func(s SnapshotInfo) error {
		restored = append(restored, s.Component)
		backend.calls = append(backend.calls, "restore:"+s.Component)
		return nil
	}, false); err != nil {
		t.Fatalf("restoreComponents: %v", err)
	}

	// Components are applied in a fixed order regardless of the order the group
	// lists them in: the task-manager database first, Neo4j last.
	if got := strings.Join(restored, ","); got != ComponentPostgres+","+ComponentNeo4j+","+ComponentMetadata {
		t.Errorf("restore order = %q, want postgres, neo4j, metadata", got)
	}

	joined := strings.Join(backend.calls, " | ")
	// The transient stores are wiped through the containers that hold them, so
	// before the application tier is stopped.
	for _, svc := range []string{"message-queue", "cache"} {
		if idx := indexOf(backend.calls, "exec:"+svc); idx == -1 {
			t.Errorf("%s was never wiped; calls: %s", svc, joined)
		}
	}
	mustPrecede := func(a, b string) {
		t.Helper()
		ia, ib := indexOf(backend.calls, a), indexOf(backend.calls, b)
		if ia == -1 {
			t.Errorf("%q never happened; calls: %s", a, joined)
			return
		}
		if ib == -1 {
			t.Errorf("%q never happened; calls: %s", b, joined)
			return
		}
		if ia >= ib {
			t.Errorf("%q must precede %q; calls: %s", a, b, joined)
		}
	}
	mustPrecede("exec:message-queue", "stop:infrahub-server")
	mustPrecede("stop:infrahub-server", "restore:"+ComponentPostgres)
	mustPrecede("restore:"+ComponentPostgres, "start:cache,message-queue")
	mustPrecede("start:cache,message-queue", "restore:"+ComponentNeo4j)
	mustPrecede("restore:"+ComponentNeo4j, "start:infrahub-server,task-worker")

	// Every service stopAppContainers stops has to come back before the restore is
	// called done.
	started := map[string]bool{}
	for _, call := range backend.calls {
		rest, ok := strings.CutPrefix(call, "start:")
		if !ok {
			continue
		}
		for _, svc := range strings.Split(rest, ",") {
			started[svc] = true
		}
	}
	for _, svc := range appServices {
		if !started[svc] {
			t.Errorf("%s was stopped for the restore but never started again; calls: %s", svc, joined)
		}
	}
}

// --reset-deployment-id was accepted and warned away. main honoured it, and a
// restored clone that keeps the source deployment's Root UUID reports telemetry as
// the production instance. It has to run after the database is back (the Neo4j
// restore waits for Bolt) and before the application containers restart, because
// they cache the UUID on startup.
func TestRestoreComponentsHonoursResetDeploymentID(t *testing.T) {
	t.Run("a Neo4j restore resets the deployment ID before the apps come back", func(t *testing.T) {
		backend := newLifecycleBackend()
		iops := newLifecycleTestOps(backend)

		plan := restorePlan{backupID: "x", snapshots: snapshotsFor(ComponentNeo4j)}
		if err := iops.restoreComponents(plan, func(SnapshotInfo) error {
			backend.calls = append(backend.calls, "restore:"+ComponentNeo4j)
			return nil
		}, true); err != nil {
			t.Fatalf("restoreComponents: %v", err)
		}

		joined := strings.Join(backend.calls, " | ")
		reset := indexOf(backend.calls, "exec-cypher:MATCH (n:Root)")
		if reset == -1 {
			t.Fatalf("the deployment ID was never reset; calls: %s", joined)
		}
		restarted := indexOf(backend.calls, "start:infrahub-server,task-worker")
		if restarted == -1 || reset > restarted {
			t.Fatalf("the reset must precede the application restart; calls: %s", joined)
		}
	})

	t.Run("a restore with no Neo4j component does not pretend to reset it", func(t *testing.T) {
		backend := newLifecycleBackend()
		iops := newLifecycleTestOps(backend)

		plan := restorePlan{backupID: "x", snapshots: snapshotsFor(ComponentPostgres)}
		if err := iops.restoreComponents(plan, func(SnapshotInfo) error { return nil }, true); err != nil {
			t.Fatalf("restoreComponents: %v", err)
		}
		if indexOf(backend.calls, "exec-cypher:MATCH (n:Root)") != -1 {
			t.Errorf("the deployment ID was reset without a Neo4j restore; calls: %s", strings.Join(backend.calls, " | "))
		}
	})
}

// A format migration has to travel to the runner: neo4j-admin cannot migrate a
// store the server has opened, and by the time the orchestrator restarts the
// database the offline window is gone.
func TestRestoreMigrationTargetsTheRunner(t *testing.T) {
	migrate := Neo4jMigration{Format: "block", Database: "neo4j"}
	if !migrate.Requested() {
		t.Fatal("a migration with a format set does not report itself as requested")
	}
	if (Neo4jMigration{}).Requested() {
		t.Fatal("an empty migration reports itself as requested")
	}
	// A format without a database is a programming error, not a silent no-op.
	if err := (Neo4jMigration{Format: "block"}).run(""); err == nil {
		t.Fatal("a migration without a database was accepted")
	}
}

// A component the tool cannot restore must be reported, not skipped: the fixed
// dispatch order would otherwise pass over it silently.
func TestRestorePlanRejectsUnknownComponents(t *testing.T) {
	plan := restorePlan{backupID: "x", snapshots: snapshotsFor(ComponentNeo4j, "artifacts")}
	err := plan.validate()
	if err == nil || !strings.Contains(err.Error(), "artifacts") {
		t.Fatalf("validate() = %v, want the unknown component named", err)
	}

	empty := restorePlan{snapshotID: "abcd"}
	if err := empty.validate(); err == nil || !strings.Contains(err.Error(), "nothing to restore") {
		t.Fatalf("validate() on an empty plan = %v, want a refusal", err)
	}
}

// A failed restore leaves the application tier down on purpose (pointing Infrahub
// at a half-restored database is worse), but it must say so rather than leave the
// operator guessing.
func TestRestoreComponentsReportsAFailedComponent(t *testing.T) {
	backend := newLifecycleBackend()
	iops := newLifecycleTestOps(backend)

	componentErr := errors.New("neo4j restore failed")
	plan := restorePlan{backupID: "x", snapshots: snapshotsFor(ComponentNeo4j)}
	err := iops.restoreComponents(plan, func(SnapshotInfo) error { return componentErr }, false)
	if !errors.Is(err, componentErr) {
		t.Fatalf("err = %v, want the component error", err)
	}
	if strings.Contains(strings.Join(backend.calls, " "), "start:infrahub-server,task-worker") {
		t.Error("the application tier was restarted over a failed restore")
	}
}

// The restore route must not come from live detection alone. NewNeo4jEditionInfo
// falls back to Community whenever detection fails, so an Enterprise backup would
// silently take the Community path — wrong connector, wrong artifact shape.
// Reconciling with the edition recorded in the backup turns that into a refusal.
func TestResolveRestoreCommunityReconcilesWithTheBackup(t *testing.T) {
	// detectNeo4jEdition runs cypher-shell through the backend; bareBackend returns
	// empty output, which NewNeo4jEditionInfo normalizes to a non-community,
	// non-enterprise edition. Script the two editions explicitly instead.
	for _, tc := range []struct {
		name          string
		detected      string
		backupEdition string
		wantCommunity bool
		wantErr       string
	}{
		{name: "community backup on community target", detected: "community", backupEdition: "community", wantCommunity: true},
		{name: "enterprise backup on enterprise target", detected: "enterprise", backupEdition: "enterprise"},
		{name: "community backup on enterprise target uses the community path", detected: "enterprise", backupEdition: "community", wantCommunity: true},
		{
			name:          "enterprise backup with detection defaulted to community is refused",
			detected:      "community",
			backupEdition: "enterprise",
			wantErr:       "cannot restore Enterprise backup on Community edition",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := &editionBackend{bareBackend: bareBackend{name: "docker"}, edition: tc.detected}
			iops := newLifecycleTestOps(backend)

			community, err := iops.resolveRestoreCommunity(tc.backupEdition)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if community != tc.wantCommunity {
				t.Errorf("community = %v, want %v", community, tc.wantCommunity)
			}
		})
	}
}

// main restored the task-manager database with `pg_restore --clean --create`. The
// runner path passed only `clean`, which drops just the objects the dump names —
// so a rollback to an older Prefect schema left everything added since in place —
// and left the archive's globals being replayed CREATE/ALTER ROLE into the whole
// cluster. These are the option names the pinned connector accepts, so the test
// builds a real exporter from them rather than comparing strings to strings.
func TestPostgresRestoreOptsReproduceCleanCreate(t *testing.T) {
	opts := postgresRestoreOpts()

	if opts["recreate"] != "true" {
		t.Errorf("recreate = %q, want \"true\" — `clean` alone does not drop and recreate the database", opts["recreate"])
	}
	if opts["no_globals"] != "true" {
		t.Errorf("no_globals = %q, want \"true\" — replaying globals.sql alters roles cluster-wide", opts["no_globals"])
	}
	if _, ok := opts["clean"]; ok {
		t.Error("clean is set alongside recreate; the connector rejects the pair as mutually exclusive")
	}

	// Prove the connector accepts them: NewExporter parses the whole option set
	// (and its mutual exclusions) without connecting to a database.
	cfg := &PlakarConfig{RepoPath: filepath.Join(t.TempDir(), "repo"), CacheDir: t.TempDir()}
	kctx, err := initPlakarContext(cfg)
	if err != nil {
		t.Fatalf("initPlakarContext: %v", err)
	}
	defer closePlakarContext(kctx)

	config := map[string]string{"location": "postgres://prefect:pass@task-manager-db:5432/prefect"}
	for k, v := range opts {
		config[k] = v
	}
	exp, err := exporter.NewExporter(kctx, connectorOptions(kctx), config)
	if err != nil {
		t.Fatalf("the pinned integration-postgresql exporter rejected the restore options: %v", err)
	}
	_ = exp.Close(kctx.Context)
}

// editionBackend answers the edition query detectNeo4jEdition makes.
type editionBackend struct {
	bareBackend
	edition string
}

func (b *editionBackend) Exec(service string, command []string, opts *ExecOptions) (string, error) {
	if len(command) > 0 && command[0] == "cypher-shell" {
		return "edition\n" + b.edition + "\n", nil
	}
	return "", nil
}

var _ EnvironmentBackend = (*editionBackend)(nil)
