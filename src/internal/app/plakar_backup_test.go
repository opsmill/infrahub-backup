package app

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PlakarKorp/kloset/connectors/importer"
	neo4jimporter "github.com/opsmill/plakar-integration-neo4j/importer"
)

// --redact destroys the live database, and the pre-redaction data only survives in
// the backup the same run is about to write. So the repository must be proven
// openable — for an encrypted repo, the passphrase must pass the canary — before
// the redaction runs. These tests pin that order; reversing it is how a mistyped
// passphrase destroyed the data and then failed to back it up.
func TestPrepareRepoBeforeRedactOrdering(t *testing.T) {
	t.Run("repository is prepared before the database is redacted", func(t *testing.T) {
		var calls []string
		err := prepareRepoBeforeRedact(true, true,
			func() error { calls = append(calls, "prepare"); return nil },
			func() error { calls = append(calls, "redact"); return nil },
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(calls) != 2 || calls[0] != "prepare" || calls[1] != "redact" {
			t.Fatalf("call order = %v, want [prepare redact]", calls)
		}
	})

	t.Run("a repository that cannot be prepared never reaches the redaction", func(t *testing.T) {
		prepareErr := errors.New("cannot open encrypted repository: incorrect passphrase")
		redacted := false
		err := prepareRepoBeforeRedact(true, true,
			func() error { return prepareErr },
			func() error { redacted = true; return nil },
		)
		if !errors.Is(err, prepareErr) {
			t.Fatalf("err = %v, want the preparation error", err)
		}
		if redacted {
			t.Fatal("the database was redacted even though the repository could not be opened")
		}
	})

	t.Run("--redact without --force refuses before touching either", func(t *testing.T) {
		prepared, redacted := false, false
		err := prepareRepoBeforeRedact(true, false,
			func() error { prepared = true; return nil },
			func() error { redacted = true; return nil },
		)
		if !errors.Is(err, errRedactRequiresForce) {
			t.Fatalf("err = %v, want errRedactRequiresForce", err)
		}
		if prepared || redacted {
			t.Fatalf("refusal did work anyway: prepared=%v redacted=%v", prepared, redacted)
		}
	})

	t.Run("without --redact the repository is still prepared", func(t *testing.T) {
		prepared, redacted := false, false
		if err := prepareRepoBeforeRedact(false, false,
			func() error { prepared = true; return nil },
			func() error { redacted = true; return nil },
		); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !prepared {
			t.Fatal("the repository was not prepared on the ordinary backup path")
		}
		if redacted {
			t.Fatal("the database was redacted without --redact")
		}
	})
}

// The ordering cases above use stub closures. This one runs the REAL preparation
// step — iops.ensurePlakarRepo, i.e. the whole openOrCreateRepo → newRepository →
// VerifyCanary chain — against an encrypted repository, so the claim being pinned
// is the production one: a wrong or absent passphrase fails before redactDatabase
// is reached, not merely before some test double is.
func TestWrongPassphraseFailsBeforeRedaction(t *testing.T) {
	// An encrypted repository to point the backup at.
	encrypted := newTestPlakarConfig(t)
	encrypted.Encrypt = true
	encrypted.Passphrase = "the-right-passphrase"
	writeTestSnapshot(t, encrypted, "seed", randomMarker(t, 128), nil)

	for _, tc := range []struct {
		name       string
		passphrase string
		wantErr    error
	}{
		{"wrong passphrase", "the-wrong-passphrase", errWrongPassphrase},
		{"absent passphrase", "", errEncryptedRepoNeedsPassphrase},
	} {
		t.Run(tc.name, func(t *testing.T) {
			iops := NewInfrahubOps()
			iops.backend = newLifecycleBackend() // records any exec the redaction would make
			iops.config.Plakar = &PlakarConfig{
				RepoPath:   encrypted.RepoPath,
				CacheDir:   t.TempDir(),
				Passphrase: tc.passphrase,
			}

			redacted := false
			err := prepareRepoBeforeRedact(true, true,
				func() error { return iops.ensurePlakarRepo() },
				func() error { redacted = true; return iops.redactDatabase() },
			)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if redacted {
				t.Fatal("the database was redacted despite the repository being unopenable")
			}
			// And nothing reached the database at all.
			for _, call := range iops.backend.(*lifecycleBackend).calls {
				if strings.HasPrefix(call, "exec") {
					t.Errorf("the database was contacted before the repository was proven openable: %q", call)
				}
			}
		})
	}
}

// The Community offline dump takes the database away, so the application tier has
// to be stopped for it — main did, and the runner rewrite kept only
// StopServices("database"). Order matters both ways: the applications go down
// before the database is suspended and come back after it is resumed.
func TestWithDeploymentQuiescedStopsTheApplicationTier(t *testing.T) {
	backend := newLifecycleBackend()
	iops := newLifecycleTestOps(backend)

	got, err := iops.withDeploymentQuiesced(func() (string, error) {
		backend.calls = append(backend.calls, "dump")
		return "deadbeef", nil
	})
	if err != nil {
		t.Fatalf("withDeploymentQuiesced: %v", err)
	}
	if got != "deadbeef" {
		t.Errorf("snapshot id = %q, want it passed through", got)
	}

	joined := strings.Join(backend.calls, " | ")
	order := []string{"stop:infrahub-server", "suspend:neo4j", "dump", "resume:neo4j", "start:cache"}
	last := -1
	for _, want := range order {
		idx := indexOf(backend.calls, want)
		if idx == -1 {
			t.Fatalf("%q never happened; calls: %s", want, joined)
		}
		if idx <= last {
			t.Fatalf("%q happened out of order; calls: %s", want, joined)
		}
		last = idx
	}

	for _, svc := range appServices {
		if indexOf(backend.calls, "stop:"+svc) == -1 {
			t.Errorf("%s was left running during the offline dump; calls: %s", svc, joined)
		}
	}
}

// The database container must stay up for the offline dump.
//
// Stopping it is what made this path Docker-only: neo4j-admin then has to run in
// a sibling that shares the data volume, and no such sibling exists on
// Kubernetes — which is how plakar went from working on main to refused. The
// window is now a suspended process inside a running container, so `Stop` must
// never be called on the database.
func TestTheOfflineDumpLeavesTheDatabaseContainerRunning(t *testing.T) {
	backend := newLifecycleBackend()
	iops := newLifecycleTestOps(backend)

	if _, err := iops.withDeploymentQuiesced(func() (string, error) { return "deadbeef", nil }); err != nil {
		t.Fatalf("withDeploymentQuiesced: %v", err)
	}

	joined := strings.Join(backend.calls, " | ")
	if indexOf(backend.calls, "stop:database") != -1 {
		t.Errorf("the database container was stopped; neo4j-admin then has nowhere to run on Kubernetes. calls: %s", joined)
	}
	if indexOf(backend.calls, "suspend:neo4j") == -1 || indexOf(backend.calls, "resume:neo4j") == -1 {
		t.Errorf("the Neo4j process was not suspended and resumed; calls: %s", joined)
	}
}

// A restore takes the same window, for the same reason.
func TestTheRestoreLeavesTheDatabaseContainerRunning(t *testing.T) {
	backend := newLifecycleBackend()
	iops := newLifecycleTestOps(backend)

	// The staged restore itself needs a repository; this exercises the lifecycle
	// around it, so the body's failure is expected and not what is asserted.
	_ = iops.restoreNeo4jComponent("00", true, Neo4jMigration{})

	joined := strings.Join(backend.calls, " | ")
	if indexOf(backend.calls, "stop:database") != -1 {
		t.Errorf("the database container was stopped during the restore; calls: %s", joined)
	}
	if indexOf(backend.calls, "suspend:neo4j") == -1 {
		t.Errorf("the Neo4j process was not suspended for the restore; calls: %s", joined)
	}
	if indexOf(backend.calls, "resume:neo4j") == -1 {
		t.Errorf("the Neo4j process was left suspended after the restore; calls: %s", joined)
	}
}

// A failure to quiesce must not run the dump, and must put back whatever it
// already stopped.
func TestWithDeploymentQuiescedRefusesWhenTheTierWillNotStop(t *testing.T) {
	backend := newLifecycleBackend()
	backend.stopErr["task-manager"] = errors.New("container is unhealthy")
	iops := newLifecycleTestOps(backend)

	dumped := false
	if _, err := iops.withDeploymentQuiesced(func() (string, error) { dumped = true; return "", nil }); err == nil {
		t.Fatal("a tier that would not stop produced no error")
	}
	if dumped {
		t.Error("the offline dump ran against a deployment that was still writing")
	}
	if indexOf(backend.calls, "start:") == -1 {
		t.Errorf("nothing was restarted after the failed stop; calls: %s", strings.Join(backend.calls, " | "))
	}
}

// --neo4jmetadata=none has to reach neo4j-admin as --include-metadata=none.
// Omitting the option instead lets neo4j-admin apply its default of `all`, so
// users and roles landed in a backup the operator asked to exclude them from.
func TestNeo4jConnectorConfigForwardsMetadataChoice(t *testing.T) {
	iops := &InfrahubOps{config: &Configuration{Neo4jDatabase: "neo4j"}}

	for _, value := range []string{"none", "all", "users", "roles"} {
		_, config := iops.neo4jConnectorConfig(false, value)
		if config["include_metadata"] != value {
			t.Errorf("neo4jConnectorConfig(false, %q) include_metadata = %q, want %q", value, config["include_metadata"], value)
		}
	}

	// Only an unset value may omit the option, leaving the engine default in place.
	_, unset := iops.neo4jConnectorConfig(false, "")
	if _, ok := unset["include_metadata"]; ok {
		t.Error("an empty --neo4jmetadata set include_metadata; it should leave the option unset")
	}

	// Every value has to be one the pinned integration accepts, "none" included —
	// it validates the option set at construction.
	cfg := &PlakarConfig{RepoPath: filepath.Join(t.TempDir(), "repo"), CacheDir: t.TempDir()}
	kctx, err := initPlakarContext(cfg)
	if err != nil {
		t.Fatalf("initPlakarContext: %v", err)
	}
	defer closePlakarContext(kctx)

	for _, value := range []string{"none", "all", "users", "roles"} {
		_, config := iops.neo4jConnectorConfig(false, value)
		imp, err := importer.NewImporter(kctx, connectorOptions(kctx), config)
		if err != nil {
			t.Fatalf("the pinned neo4j integration rejected include_metadata=%q: %v", value, err)
		}
		_ = imp.Close(kctx.Context)
	}

	// And a value it does not accept must fail loudly at the connector rather than
	// being silently dropped here.
	_, config := iops.neo4jConnectorConfig(false, "everything")
	if _, err := importer.NewImporter(kctx, connectorOptions(kctx), config); err == nil {
		t.Error("include_metadata=everything was accepted; the connector is expected to reject unknown values")
	} else if !strings.Contains(err.Error(), "include_metadata") {
		t.Errorf("error = %v, want it to name include_metadata", err)
	}
}

// The online location must carry NO host.
//
// The integration adds --from only when one is set, and with it neo4j-admin
// leaves loopback for the network: every Enterprise deployment then had to
// publish server.backup.listen_address before a backup could work at all. Now
// that neo4j-admin runs inside the database container, omitting the host is what
// makes that prerequisite disappear — so it is pinned here rather than left to
// be re-broken by a URI that "looks more complete".
func TestNeo4jOnlineLocationOmitsTheHost(t *testing.T) {
	iops := &InfrahubOps{config: &Configuration{
		Neo4jDatabase: "neo4j",
		Neo4jUsername: "neo4j",
		Neo4jPassword: "admin",
	}}

	proto, config := iops.neo4jConnectorConfig(false, "")
	if proto != "neo4j" {
		t.Errorf("proto = %q, want neo4j", proto)
	}
	location := config["location"]
	if location != "neo4j:///neo4j" {
		t.Errorf("location = %q, want neo4j:///neo4j (no host)", location)
	}

	// The credentials must not be in the location either: neo4j-admin does not
	// authenticate over the backup service, and a URI on an argv is readable.
	if strings.Contains(location, "admin") {
		t.Errorf("location %q carries the database password", location)
	}

	// And prove it against the integration's own parser rather than by reading
	// the string: an empty Host is what suppresses --from.
	dumpArgs := neo4jDumpArgsFor(t, proto, config)
	for _, arg := range dumpArgs {
		if strings.HasPrefix(arg, "--from") {
			t.Errorf("neo4j-admin argv carries %q; the online backup must use loopback: %v", arg, dumpArgs)
		}
	}
}

// The Community location keeps the data directory and the database name, which
// is what the offline dump is keyed on.
func TestNeo4jOfflineLocationCarriesDataDirAndDatabase(t *testing.T) {
	iops := &InfrahubOps{config: &Configuration{Neo4jDatabase: "infrahub"}}

	proto, config := iops.neo4jConnectorConfig(true, "")
	if proto != "neo4j+offline" {
		t.Errorf("proto = %q, want neo4j+offline", proto)
	}
	if config["location"] != "neo4j+offline:///data?database=infrahub" {
		t.Errorf("location = %q, want neo4j+offline:///data?database=infrahub", config["location"])
	}

	// A dump names the database explicitly, so a deployment whose database is not
	// called "neo4j" is dumped — and later loaded — under its real name.
	args := neo4jDumpArgsFor(t, proto, config)
	if !strings.Contains(strings.Join(args, " "), "infrahub") {
		t.Errorf("neo4j-admin argv does not name the database: %v", args)
	}
}

// neo4jDumpArgsFor asks the integration for the neo4j-admin argv it would run,
// through the same staged seam the in-place path uses.
func neo4jDumpArgsFor(t *testing.T, proto string, config map[string]string) []string {
	t.Helper()
	imp, err := neo4jimporter.NewStagedImporter(proto, config, t.TempDir())
	if err != nil {
		t.Fatalf("NewStagedImporter: %v", err)
	}
	dumper, ok := imp.(neo4jDumpArgv)
	if !ok {
		t.Fatal("the pinned neo4j integration does not report its neo4j-admin arguments")
	}
	args, err := dumper.DumpArgs(neo4jStageDir)
	if err != nil {
		t.Fatalf("DumpArgs: %v", err)
	}
	return args
}
