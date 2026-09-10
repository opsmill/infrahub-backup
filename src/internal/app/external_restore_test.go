package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// externalNeo4jRestore is a prepared restore against a scripted deployment,
// which is what lets the seed, the confirmation and the refusals be driven
// without a cluster and without a database.
// The backend is taken as the interface rather than as scriptedProbeBackend so
// a double that models a *transient* failure can be driven through the same
// setup; the script is only ever used as the deployment.
func externalNeo4jRestore(t *testing.T, backend EnvironmentBackend, seed restoreSeed) (*InfrahubOps, *externalRestore) {
	t.Helper()

	iops := &InfrahubOps{config: &Configuration{S3: &S3Config{}}, backend: backend, executor: NewCommandExecutor()}
	restore := &externalRestore{
		externalSource: &externalSource{
			Endpoint: &DatabaseEndpoint{
				Service:  serviceNeo4j,
				Location: EndpointLocationExternal,
				Hosts:    []HostPort{{Host: "neo4j-a.example", Port: 7687}, {Host: "neo4j-b.example", Port: 7687}},
				Database: "infrahub",
			},
			TLS:   externalTLSDecision{Service: serviceNeo4j, Encrypt: true},
			Facts: probedFacts{ServerVersion: "2025.10.1", Answered: HostPort{Host: "neo4j-a.example", Port: 7687}},
		},
		Pod:   "infrahub-backup-xdb-neo4j-restore-abc123",
		Bound: 2 * time.Second,
		Seed:  seed,
	}
	iops.recordExternalRestore(serviceNeo4j, restore)

	return iops, restore
}

// showDatabaseOutput is what `cypher-shell --format plain` prints for the
// status query, with a notice above the header — the shape every parser on this
// path has to survive (carried from T085).
func showDatabaseOutput(rows ...string) string {
	lines := []string{
		"info: Bolt server version 2025.10.1",
		neo4jStatusColumn + ", " + neo4jStatusMessageColumn,
	}

	return strings.Join(append(lines, rows...), "\n") + "\n"
}

// TestNeo4jSeedStatements pins the Cypher the seed is issued as, in both forms
// the server versions require (research R2).
func TestNeo4jSeedStatements(t *testing.T) {
	t.Run("a current server replaces the database atomically", func(t *testing.T) {
		statements, err := neo4jSeedStatements("infrahub", "s3://bucket/seed/infrahub-2026.backup", "2025.10.1")
		if err != nil {
			t.Fatalf("neo4jSeedStatements() = %v, want the statement", err)
		}
		if len(statements) != 1 {
			t.Fatalf("statements = %v, want one: CREATE OR REPLACE is a single destructive step", statements)
		}
		for _, want := range []string{"CREATE OR REPLACE DATABASE infrahub", "existingData: 'use'", "seedURI: 's3://bucket/seed/infrahub-2026.backup'"} {
			if !strings.Contains(statements[0], want) {
				t.Errorf("statement = %q, want it to contain %q", statements[0], want)
			}
		}
	})

	t.Run("a server older than the floor drops before it creates", func(t *testing.T) {
		statements, err := neo4jSeedStatements("infrahub", "s3://bucket/seed/artifact.backup", "5.26.1")
		if err != nil {
			t.Fatalf("neo4jSeedStatements() = %v, want the statements", err)
		}
		if len(statements) != 2 {
			t.Fatalf("statements = %v, want two: CREATE OR REPLACE did not accept a seed URI before %s", statements, neo4jSeedReplaceFloor)
		}
		if !strings.HasPrefix(statements[0], "DROP DATABASE infrahub IF EXISTS") {
			t.Errorf("first statement = %q, want the drop", statements[0])
		}
		if !strings.Contains(statements[1], "CREATE DATABASE infrahub") || !strings.Contains(statements[1], "seedURI:") {
			t.Errorf("second statement = %q, want the seeded create", statements[1])
		}
	})

	// The calendar scheme is why the comparison cannot be a text one: "2025.01"
	// against "5.26" orders backwards as text and correctly as versions.
	t.Run("the version comparison spans both numbering schemes", func(t *testing.T) {
		for version, atomic := range map[string]bool{
			"5.26.1":    false,
			"5.26":      false,
			"2024.12":   false,
			"2025.01":   true,
			"2025.10.1": true,
			"2026.07":   true,
		} {
			if got := atomicSeedReplaceSupported(version); got != atomic {
				t.Errorf("atomicSeedReplaceSupported(%q) = %t, want %t", version, got, atomic)
			}
		}
	})

	t.Run("an unreadable server version takes the form every version accepts", func(t *testing.T) {
		if atomicSeedReplaceSupported("not-a-version") {
			t.Error("atomicSeedReplaceSupported(unreadable) = true, want the drop-then-create form that works everywhere")
		}
	})

	t.Run("a URI that could close the literal is refused", func(t *testing.T) {
		for _, uri := range []string{
			"s3://bucket/a'} ; DROP DATABASE neo4j //",
			"s3://bucket/with a space",
			"s3://bucket/line\nbreak",
		} {
			if _, err := neo4jSeedStatements("infrahub", uri, "2025.10.1"); err == nil {
				t.Errorf("neo4jSeedStatements(%q) = nil, want the URI refused before it reaches a statement", uri)
			}
		}
	})

	// Only the object-store providers are built in; every other scheme needs
	// dbms.databases.seed_from_uri_providers configured on each server, which
	// this run cannot see (research R2).
	t.Run("only the built-in cloud seed providers are accepted", func(t *testing.T) {
		for _, uri := range []string{"s3://bucket/a.backup", "gs://bucket/a.backup", "azb://bucket/a.backup"} {
			if _, err := safeSeedURI(uri); err != nil {
				t.Errorf("safeSeedURI(%q) = %v, want it accepted", uri, err)
			}
		}

		for _, uri := range []string{"https://host/a.backup", "file:///tmp/a.backup", "ftp://host/a.backup", "/tmp/a.backup"} {
			err := safeSeedURI2(t, uri)
			if err == nil {
				t.Fatalf("safeSeedURI(%q) = nil, want a scheme no server supports as shipped to be refused", uri)
			}
			if !strings.Contains(err.Error(), "s3") {
				t.Errorf("err = %v, want it to name the schemes that do work", err)
			}
		}
	})
}

// safeSeedURI2 is safeSeedURI's error alone, so the loop above reads as one
// assertion per case.
func safeSeedURI2(t *testing.T, uri string) error {
	t.Helper()

	_, err := safeSeedURI(uri)

	return err
}

// TestNeo4jSeedArtifact covers what the pre-flight will stage, which has to be
// exactly one artifact named from the archive rather than a guess.
func TestNeo4jSeedArtifact(t *testing.T) {
	write := func(t *testing.T, names ...string) string {
		t.Helper()

		workDir := t.TempDir()
		databaseDir := filepath.Join(workDir, "backup", "database")
		if err := os.MkdirAll(databaseDir, 0o755); err != nil {
			t.Fatalf("creating the extract directory = %v, want nil", err)
		}
		for _, name := range names {
			if err := os.WriteFile(filepath.Join(databaseDir, name), []byte("artifact"), 0o600); err != nil {
				t.Fatalf("writing %s = %v, want nil", name, err)
			}
		}

		return workDir
	}

	t.Run("the one artifact an Enterprise archive carries", func(t *testing.T) {
		workDir := write(t, "infrahub-2026-09-03T10-00-00.backup")

		artifact, err := neo4jSeedArtifact(workDir)
		if err != nil {
			t.Fatalf("neo4jSeedArtifact() = %v, want the artifact", err)
		}
		if filepath.Base(artifact) != "infrahub-2026-09-03T10-00-00.backup" {
			t.Errorf("artifact = %q, want the .backup file", artifact)
		}
	})

	// A Community archive carries a dump, which no server can be seeded from:
	// loading one writes into the server's own store directory.
	t.Run("an archive with no online-backup artifact is refused", func(t *testing.T) {
		_, err := neo4jSeedArtifact(write(t, "neo4j.dump"))
		if err == nil {
			t.Fatal("neo4jSeedArtifact() = nil, want the archive refused")
		}
		for _, want := range []string{neo4jSeedArtifactSuffix, "no data was changed"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("err = %v, want it to mention %q", err, want)
			}
		}
	})

	t.Run("two artifacts are ambiguous rather than a choice", func(t *testing.T) {
		_, err := neo4jSeedArtifact(write(t, "infrahub-a.backup", "other-b.backup"))
		if err == nil {
			t.Fatal("neo4jSeedArtifact() = nil, want the ambiguity refused rather than one picked by sort order")
		}
	})
}

// TestStageNeo4jSeedRefusesWithoutAnObjectStore is FR-007's first arm: the
// server fetches the artifact itself, so a run with nowhere to stage it cannot
// restore — and must say so before anything is stopped (FR-020).
func TestStageNeo4jSeedRefusesWithoutAnObjectStore(t *testing.T) {
	workDir := t.TempDir()
	databaseDir := filepath.Join(workDir, "backup", "database")
	if err := os.MkdirAll(databaseDir, 0o755); err != nil {
		t.Fatalf("creating the extract directory = %v, want nil", err)
	}
	if err := os.WriteFile(filepath.Join(databaseDir, "infrahub-2026.backup"), []byte("artifact"), 0o600); err != nil {
		t.Fatalf("writing the artifact = %v, want nil", err)
	}

	iops := &InfrahubOps{config: &Configuration{S3: &S3Config{}}, backend: &scriptedProbeBackend{}, executor: NewCommandExecutor()}

	_, err := iops.stageNeo4jSeed(workDir)
	if err == nil {
		t.Fatal("stageNeo4jSeed() = nil, want the missing object store refused")
	}
	for _, want := range []string{"--s3-bucket", "fetches the backup artifact itself", "no data was changed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to mention %q", err, want)
		}
	}
}

// TestVerifySeedReadableRefusesAnUnreadableURI is FR-007's second arm, and the
// one the failure-message contract names: a URI the tool itself cannot read is
// one the database server certainly cannot, and the refusal has to name the URI
// and the server-side prerequisite.
func TestVerifySeedReadableRefusesAnUnreadableURI(t *testing.T) {
	t.Run("a URI that is not an object store's is refused", func(t *testing.T) {
		_, err := verifySeedReadable(t.Context(), &S3Config{}, "https://artifacts.example/infrahub.backup")
		if err == nil {
			t.Fatal("verifySeedReadable() = nil, want a non-object-store URI refused")
		}
		if !strings.Contains(err.Error(), "https://artifacts.example/infrahub.backup") {
			t.Errorf("err = %v, want it to name the URI attempted", err)
		}
	})

	t.Run("an object that cannot be read back is refused, naming the URI", func(t *testing.T) {
		// No endpoint resolves, so the read fails the way an object that is not
		// there fails: the point is what the refusal says and that it says it
		// before anything destructive.
		uri := "s3://infrahub-backups/seed/infrahub-2026.backup"

		_, err := verifySeedReadable(t.Context(), &S3Config{Endpoint: "http://127.0.0.1:1"}, uri)
		if err == nil {
			t.Fatal("verifySeedReadable() = nil, want the unreadable object refused")
		}
		for _, want := range []string{uri, "fetch this artifact themselves", "no data was changed"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("err = %v, want it to mention %q", err, want)
			}
		}
	})
}

// TestSeedPrefixKeepsTheStagedArtifactOutOfRetention pins where the staged copy
// is written. Retention only recognises an object whose key is exactly the one
// an archive of that name would have, so a segment deeper is invisible to it.
func TestSeedPrefixKeepsTheStagedArtifactOutOfRetention(t *testing.T) {
	for prefix, want := range map[string]string{
		"":              seedKeySegment,
		"backups":       "backups/" + seedKeySegment,
		"backups/":      "backups/" + seedKeySegment,
		"  /backups/  ": "backups/" + seedKeySegment,
	} {
		if got := seedPrefix(prefix); got != want {
			t.Errorf("seedPrefix(%q) = %q, want %q", prefix, got, want)
		}
	}

	client := &S3Client{config: &S3Config{Bucket: "infrahub-backups", Prefix: seedPrefix("backups")}}
	key := client.buildS3Key("infrahub_backup_2026-09-03_10-00-00.tar.gz")
	if _, recognised := client.backupRefForKey(key); recognised {
		t.Errorf("a staged artifact at %q is recognised as a retention candidate, want it invisible to retention", key)
	}
}

// TestParseNeo4jDatabaseStatuses covers the reading FR-021 rests on, including
// the two shapes that have bitten this path before: a notice above the header,
// and a clustered database reported once per member.
func TestParseNeo4jDatabaseStatuses(t *testing.T) {
	t.Run("every member is read, not the first row", func(t *testing.T) {
		statuses := parseNeo4jDatabaseStatuses(showDatabaseOutput(`"online", ""`, `"starting", ""`))

		if len(statuses) != 2 {
			t.Fatalf("statuses = %+v, want one per member: a seed that loaded on one server and failed on another is a half-restored database", statuses)
		}
		if verdict := classifySeedStatuses(statuses); verdict.Online {
			t.Errorf("verdict = %+v, want not online while a member is still starting", verdict)
		}
	})

	t.Run("a notice above the header is skipped", func(t *testing.T) {
		statuses := parseNeo4jDatabaseStatuses(showDatabaseOutput(`"online", ""`))

		if len(statuses) != 1 || !statuses[0].online() {
			t.Fatalf("statuses = %+v, want the one online member: the notice line must not be read as data", statuses)
		}
	})

	t.Run("a failed seed is recognised from the status message", func(t *testing.T) {
		verdict := classifySeedStatuses(parseNeo4jDatabaseStatuses(
			showDatabaseOutput(`"offline", "Unable to start database"`)))

		if verdict.Online {
			t.Error("verdict.Online = true on a database the server cannot start")
		}
		if !verdict.Failed {
			t.Error("verdict.Failed = false, want the server's own report of a seed it could not load to end the wait")
		}
		if !strings.Contains(verdict.Detail, "Unable to start database") {
			t.Errorf("verdict.Detail = %q, want the server's own message carried through", verdict.Detail)
		}
	})

	t.Run("no rows is neither online nor failed", func(t *testing.T) {
		// A database being replaced is briefly absent from the listing, and
		// reading that as a failure would fail a restore that is working.
		verdict := classifySeedStatuses(parseNeo4jDatabaseStatuses("info: nothing to report\n"))

		if verdict.Online || verdict.Failed {
			t.Errorf("verdict = %+v, want neither: the database is not in the listing yet", verdict)
		}
	})
}

// TestRestoreNeo4jExternalRequiresThePreflight is what keeps FR-007's check
// from being a step a later entry point can forget: a restore that reached the
// destructive step with no staged artifact is refused there.
func TestRestoreNeo4jExternalRequiresThePreflight(t *testing.T) {
	backend := &scriptedProbeBackend{}
	iops, restore := externalNeo4jRestore(t, backend, restoreSeed{})

	err := iops.restoreNeo4jExternal(restore)
	if err == nil {
		t.Fatal("restoreNeo4jExternal() = nil, want the missing pre-flight to stop the destructive step")
	}
	if !strings.Contains(err.Error(), "pre-flight") {
		t.Errorf("err = %v, want it to name the pre-flight that was not performed", err)
	}
	if len(backend.calls) > 0 {
		t.Errorf("commands ran = %v, want none: no statement may be issued without a staged artifact", backend.calls)
	}
}

// TestRestoreNeo4jExternalSeedsAndConfirms is the FR-021 pair: the seed is
// issued, and the run does not report success until the database is actually
// online.
func TestRestoreNeo4jExternalSeedsAndConfirms(t *testing.T) {
	seed := restoreSeed{URI: "s3://infrahub-backups/seed/infrahub-2026.backup", Bucket: "infrahub-backups", Key: "seed/infrahub-2026.backup", Bytes: 8}

	t.Run("a database that comes online is a successful restore", func(t *testing.T) {
		backend := &scriptedProbeBackend{answers: map[string][]string{
			// The first read finds it still starting, the second finds it up:
			// the wait is what turns an accepted seed into a landed one.
			"cypher-shell": {"", showDatabaseOutput(`"starting", ""`), showDatabaseOutput(`"online", ""`)},
		}}
		iops, restore := externalNeo4jRestore(t, backend, seed)
		// The staged copy is removed only once the database is up, and this
		// run has no bucket to remove it from, so the delete's own failure is
		// warned about rather than returned.
		iops.config.S3 = &S3Config{Endpoint: "http://127.0.0.1:1"}

		if err := iops.restoreNeo4jExternal(restore); err != nil {
			t.Fatalf("restoreNeo4jExternal() = %v, want the restore to succeed", err)
		}

		calls := backend.argvFor("cypher-shell")
		if len(calls) < 2 {
			t.Fatalf("cypher-shell calls = %v, want the seed and at least one status read", calls)
		}

		seeded := strings.Join(calls[0], " ")
		if !strings.Contains(seeded, "CREATE OR REPLACE DATABASE infrahub") || !strings.Contains(seeded, seed.URI) {
			t.Errorf("seed statement = %q, want it to seed infrahub from %s", seeded, seed.URI)
		}
		if !strings.Contains(seeded, "neo4j-a.example:7687") {
			t.Errorf("seed statement = %q, want it addressed at the member that answered the probe", seeded)
		}
		// FR-014: the credentials reach the client through the workload's own
		// secret, never through an argument or an exec environment.
		for _, call := range backend.calls {
			for _, arg := range call {
				if strings.Contains(arg, "NEO4J_PASSWORD") || strings.Contains(arg, "-p") && len(arg) > 2 {
					t.Errorf("argv %v carries what looks like a credential", call)
				}
			}
		}
	})

	t.Run("a seed the server cannot load fails the run", func(t *testing.T) {
		backend := &scriptedProbeBackend{answers: map[string][]string{
			"cypher-shell": {"", showDatabaseOutput(`"offline", "Unable to start database"`)},
		}}
		iops, restore := externalNeo4jRestore(t, backend, seed)

		err := iops.restoreNeo4jExternal(restore)
		if err == nil {
			t.Fatal("restoreNeo4jExternal() = nil, want the failed seed reported: accepting the request is not evidence the data loaded")
		}
		for _, want := range []string{"not usable", "Unable to start database", "neo4j.log", "prior scale"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("err = %v, want it to mention %q", err, want)
			}
		}
	})

	t.Run("a database that never comes online fails at the bound", func(t *testing.T) {
		backend := &scriptedProbeBackend{answers: map[string][]string{
			"cypher-shell": {"", showDatabaseOutput(`"starting", ""`)},
		}}
		iops, restore := externalNeo4jRestore(t, backend, seed)
		restore.Bound = 50 * time.Millisecond

		err := iops.restoreNeo4jExternal(restore)
		if err == nil {
			t.Fatal("restoreNeo4jExternal() = nil, want the run to fail rather than report a restore that never landed")
		}
		for _, want := range []string{"did not come online", "--external-db-timeout", "starting"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("err = %v, want it to mention %q", err, want)
			}
		}
	})

	t.Run("a server that stops answering fails rather than hanging", func(t *testing.T) {
		backend := &scriptedProbeBackend{
			answers: map[string][]string{"cypher-shell": {""}},
			fails:   map[string]error{},
		}
		iops, restore := externalNeo4jRestore(t, backend, seed)
		restore.Bound = 50 * time.Millisecond

		// The seed is accepted and every status read then returns nothing,
		// which is a server that answered and said nothing: the window ends the
		// wait, so a restore cannot sit with Infrahub scaled to zero (FR-025).
		err := iops.restoreNeo4jExternal(restore)
		if err == nil {
			t.Fatal("restoreNeo4jExternal() = nil, want the bound to end the wait")
		}
	})
}

// TestRefuseExternalCommunityRestore is FR-008's restore side. A Community
// server cannot create the database a seed would populate, so there is no
// statement to issue — and the refusal must not read as a missing container.
func TestRefuseExternalCommunityRestore(t *testing.T) {
	endpoint := &DatabaseEndpoint{
		Service:  serviceNeo4j,
		Location: EndpointLocationExternal,
		Hosts:    []HostPort{{Host: "neo4j.example", Port: 7687}},
	}

	t.Run("a Community server is refused", func(t *testing.T) {
		err := refuseExternalCommunityRestore(endpoint, "community")
		if err == nil {
			t.Fatal("refuseExternalCommunityRestore(community) = nil, want the refusal")
		}
		if !errors.Is(err, errExternalCommunityRestore) {
			t.Errorf("err = %v, want it to wrap %v", err, errExternalCommunityRestore)
		}
		for _, want := range []string{"Enterprise", "Nothing is missing from the deployment", "no data was changed"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("err = %v, want it to mention %q", err, want)
			}
		}
	})

	t.Run("an Enterprise server is not refused", func(t *testing.T) {
		if err := refuseExternalCommunityRestore(endpoint, "enterprise"); err != nil {
			t.Errorf("refuseExternalCommunityRestore(enterprise) = %v, want nil", err)
		}
	})

	// The same asymmetry the capture arm documents: an edition the server did
	// not report is not read as Community, because guessing the destructive
	// branch from a probe that did not answer is what Principle II forbids.
	t.Run("an edition the server did not report is not read as Community", func(t *testing.T) {
		if err := refuseExternalCommunityRestore(endpoint, ""); err != nil {
			t.Errorf("refuseExternalCommunityRestore(unreported) = %v, want nil", err)
		}
	})
}

// TestPostgresRestoreCommand pins the argv of both branches. The internal one
// is asserted character for character against the command that path produced
// before this feature existed (FR-015).
func TestPostgresRestoreCommand(t *testing.T) {
	t.Run("the in-container TCP branch is unchanged", func(t *testing.T) {
		got := postgresRestoreCommand(postgresRestoreRequest{User: "postgres", File: "/tmp/infrahubops_prefect.dump"})
		want := []string{"pg_restore", "-h", "localhost", "-d", "postgres", "-U", "postgres", "--clean", "--create", "/tmp/infrahubops_prefect.dump"}

		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("command = %v, want %v", got, want)
		}
	})

	t.Run("the in-container socket branch is unchanged", func(t *testing.T) {
		got := postgresRestoreCommand(postgresRestoreRequest{Socket: true, File: "/tmp/infrahubops_prefect.dump"})
		want := []string{"pg_restore", "-d", "postgres", "--clean", "--create", "/tmp/infrahubops_prefect.dump"}

		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("command = %v, want %v", got, want)
		}
	})

	t.Run("the external branch names the resolved host and no credential", func(t *testing.T) {
		host := HostPort{Host: "pg.example", Port: 5433}
		got := postgresRestoreCommand(postgresRestoreRequest{Host: &host, File: externalPostgresRestoreFile})
		want := []string{"pg_restore", "-h", "pg.example", "-p", "5433", "-d", "postgres", "--clean", "--create", externalPostgresRestoreFile}

		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("command = %v, want %v", got, want)
		}
		// FR-014: no role name and no password on the command line; PGUSER and
		// PGPASSWORD come from the workload's secret.
		for _, arg := range got {
			if arg == "-U" {
				t.Error("the external restore command names a role on the command line")
			}
		}
	})
}

// TestRefuseUnfittableDump keeps the dump's size a refusal that names the flag
// rather than a full volume mid-transfer.
func TestRefuseUnfittableDump(t *testing.T) {
	t.Run("a dump that fits the default is accepted", func(t *testing.T) {
		if err := refuseUnfittableDump("", 1<<20); err != nil {
			t.Errorf("refuseUnfittableDump() = %v, want nil for a 1MiB dump", err)
		}
	})

	t.Run("a dump larger than the scratch space names the override", func(t *testing.T) {
		err := refuseUnfittableDump("1Gi", 2<<30)
		if err == nil {
			t.Fatal("refuseUnfittableDump() = nil, want the dump refused before the copy")
		}
		for _, want := range []string{"--external-db-scratch-size", "1Gi", "No data was changed"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("err = %v, want it to mention %q", err, want)
			}
		}
	})

	t.Run("an override that is not a quantity is refused naming the flag", func(t *testing.T) {
		if _, err := resolveRestoreScratchSize(serviceTaskManagerDB, "quite big"); err == nil {
			t.Error("resolveRestoreScratchSize() = nil, want a malformed quantity refused before any object is created")
		}
	})
}

// TestRestoreWorkloadSpec covers the one workload a restore uses: its role, its
// deadline covering the operations it hosts, and its credentials reaching it
// through the secret rather than the manifest.
func TestRestoreWorkloadSpec(t *testing.T) {
	cfg := NewInfrahubOps().Config()
	cfg.Neo4jUsername = "neo4j"
	cfg.Neo4jPassword = "hunter2-not-in-the-manifest"

	spec, err := restoreWorkloadSpec(cfg, "infrahub", "abc123", serviceNeo4j)
	if err != nil {
		t.Fatalf("restoreWorkloadSpec() = %v, want the spec", err)
	}

	if spec.Role != workloadRoleRestore {
		t.Errorf("role = %q, want %q", spec.Role, workloadRoleRestore)
	}
	if !strings.Contains(spec.jobName(), string(workloadRoleRestore)) {
		t.Errorf("Job name = %q, want the role in it so a restore's workload is not a capture's", spec.jobName())
	}

	// The pod has to outlive the copy, the seed and the wait for the database
	// to come online, or the cluster removes it from under an operation still
	// in flight (FR-025).
	if want := externalDBRestoreFullBoundCalls * externalDBBound(cfg); spec.Deadline <= want {
		t.Errorf("deadline = %v, want more than the %v of bounded work it hosts", spec.Deadline, want)
	}
	if spec.Deadline <= externalDBCaptureDeadline(cfg) {
		t.Errorf("deadline = %v, want it to exceed a capture's %v: this pod is created at the gate and held for the whole run",
			spec.Deadline, externalDBCaptureDeadline(cfg))
	}

	if spec.Credentials["NEO4J_PASSWORD"] != cfg.Neo4jPassword {
		t.Errorf("credentials = %v, want the password carried for the owned secret", spec.Credentials)
	}
	manifest, err := buildTransientWorkloadManifest(spec)
	if err != nil {
		t.Fatalf("buildTransientWorkloadManifest() = %v, want the manifest", err)
	}
	if strings.Contains(string(manifest), cfg.Neo4jPassword) {
		t.Error("the pod manifest carries the password, want it referenced from the owned secret only (FR-014)")
	}
}

// TestExternalRestoreForIsLocationAuthoritative is the T083 property on the
// restore side: a nil prepared restore is only the internal branch where the
// database actually is internal.
func TestExternalRestoreForIsLocationAuthoritative(t *testing.T) {
	locating := func(location EndpointLocation, err error) *InfrahubOps {
		backend := newGatedBackend()
		backend.locateErr = err
		if err == nil {
			backend.locations[serviceNeo4j] = location
		}

		return &InfrahubOps{config: &Configuration{}, backend: backend, executor: NewCommandExecutor()}
	}

	t.Run("an in-deployment database is the internal branch", func(t *testing.T) {
		restore, err := locating(EndpointLocationInternal, nil).externalRestoreFor(serviceNeo4j)
		if err != nil || restore != nil {
			t.Errorf("externalRestoreFor() = %v, %v, want nil, nil: the existing path must be unchanged (FR-015)", restore, err)
		}
	})

	t.Run("an external database with nothing prepared is an error, not the internal branch", func(t *testing.T) {
		restore, err := locating(EndpointLocationExternal, nil).externalRestoreFor(serviceNeo4j)
		if err == nil {
			t.Fatalf("externalRestoreFor() = %v, nil, want the unprepared external database reported rather than written to in a container it does not live in", restore)
		}
		if !strings.Contains(err.Error(), "nothing was prepared to restore into it") {
			t.Errorf("err = %v, want it to name what was not done", err)
		}
	})

	t.Run("a location that could not be established is an error", func(t *testing.T) {
		_, err := locating("", fmt.Errorf("pods is forbidden")).externalRestoreFor(serviceNeo4j)
		if err == nil {
			t.Fatal("externalRestoreFor() = nil, want the unanswered location query reported (FR-002)")
		}
	})
}

// TestExternalRestoreAuthDescribesItsChannel covers the fact FR-010 turns on:
// which channel authorised a destructive write to unmanaged infrastructure,
// since only one of them is available to an unattended run.
func TestExternalRestoreAuthDescribesItsChannel(t *testing.T) {
	for auth, want := range map[ExternalRestoreAuth]string{
		{Source: ExternalRestoreAuthFlag}:   "--" + AllowExternalRestoreFlag,
		{Source: ExternalRestoreAuthConfig}: AllowExternalRestoreEnvVar,
		{}:                                  "no channel",
	} {
		if got := auth.describe(); !strings.Contains(got, want) {
			t.Errorf("describe(%+v) = %q, want it to name %q", auth, got, want)
		}
	}
}

// TestPrepareExternalRestoresIsOncePerDatabase covers the gate being called
// twice in one run, which `restore --latest --s3` does: once before the
// download, and again inside RestoreBackup. A second pass must do nothing at
// all — a second workload for the same database carries the same run
// identifier, so it has the same pod name and the create collides.
func TestPrepareExternalRestoresIsOncePerDatabase(t *testing.T) {
	backend := newGatedBackend()
	backend.locations[serviceNeo4j] = EndpointLocationExternal
	iops := &InfrahubOps{config: &Configuration{S3: &S3Config{}}, backend: backend, executor: NewCommandExecutor()}

	// Out of the gate's own constructor rather than written as a literal: the
	// preparation refuses a target that did not come from there, because the
	// location and FR-009's authorisation are what holding one attests to (see
	// databaseTarget.requireGated).
	gated, err := databaseTargetWith(deploymentQueries{
		locate: func(string) (EndpointLocation, error) { return EndpointLocationExternal, nil },
	}, databaseRestore, serviceNeo4j, ExternalRestoreAuth{Source: ExternalRestoreAuthFlag})
	if err != nil {
		t.Fatalf("databaseTargetWith(restore) = %v, want a target", err)
	}
	targets := databaseTargets{gated}

	// Nothing is prepared yet, so the first pass reaches for a workload — and
	// fails, because this deployment double cannot host one. That failure is
	// what proves the second pass below is a no-op rather than the same refusal
	// arriving twice.
	if err := iops.prepareExternalRestores(targets); err == nil {
		t.Fatal("prepareExternalRestores() = nil on an unprepared external database, want the workload attempted")
	}

	iops.recordExternalRestore(serviceNeo4j, &externalRestore{
		externalSource: &externalSource{Endpoint: &DatabaseEndpoint{Service: serviceNeo4j, Location: EndpointLocationExternal}},
		Pod:            "infrahub-backup-xdb-neo4j-restore-abc123",
	})

	if err := iops.prepareExternalRestores(targets); err != nil {
		t.Errorf("prepareExternalRestores() = %v on a database already prepared, want nil: the second pass must not build a second workload", err)
	}
	if len(iops.unpreparedExternalRestores(targets)) != 0 {
		t.Error("the prepared database is still reported as unprepared")
	}
}

// TestWaitForNeo4jRestoreSeparatesTheDatabaseFromThePathToIt is FR-021 read
// against FR-025's attribution.
//
// The wait retried *every* exec failure until the operator's bound — two hours
// by default, with Infrahub scaled to zero — and then reported "the database is
// not usable… Its reported state is the server could not be asked for its
// state: <kubectl error>". A transient pod that was evicted, a `pods/exec`
// permission that was revoked and an API server that has gone all produce
// exactly that, and none of them is a statement about the customer's database,
// which may well be loading the seed.
//
// The bound belongs to the database, because a large store legitimately takes
// hours. The question does not.
func TestWaitForNeo4jRestoreSeparatesTheDatabaseFromThePathToIt(t *testing.T) {
	seed := restoreSeed{URI: "s3://infrahub-backups/seed/infrahub-2026.backup", Bucket: "infrahub-backups", Key: "seed/infrahub-2026.backup", Bytes: 8}
	denied := errors.New(`Error from server (Forbidden): pods "infrahub-backup-xdb-neo4j-restore-abc123" is forbidden: User cannot create resource "pods/exec"`)

	t.Run("a read that never reached the server is not the database's state", func(t *testing.T) {
		backend := &scriptedProbeBackend{fails: map[string]error{"cypher-shell": denied}}
		iops, restore := externalNeo4jRestore(t, backend, seed)
		// The operator's bound, which is what this must not spend.
		restore.Bound = time.Hour

		err := iops.waitForNeo4jRestoreWithin(restore, seed.URI, 0)
		if err == nil {
			t.Fatal("waitForNeo4jRestoreWithin() = nil, want the run to fail: it cannot tell whether the restore landed")
		}

		// Attribution, which is the whole finding: the message must not say the
		// database is unusable on evidence that never reached it.
		if strings.Contains(err.Error(), "the database is not usable") {
			t.Errorf("err = %v, want a failure of the path to the database rather than a verdict on it", err)
		}
		for _, want := range []string{"pods/exec", "not a report about it", "may still be loading the seed"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("err = %v, want it to mention %q", err, want)
			}
		}

		// And it must not have waited out the hour to say so.
		if calls := len(backend.argvFor("cypher-shell")); calls != 1 {
			t.Errorf("status reads = %d, want 1: with no grace the first unanswered read is the verdict, not the start of an hour", calls)
		}
	})

	t.Run("a bound shorter than the grace still attributes the failure", func(t *testing.T) {
		// The grace is a minute and an operator may set --external-db-timeout
		// well below it, so the deadline expires first. The last thing that
		// happened is still a question that went unanswered.
		backend := &scriptedProbeBackend{fails: map[string]error{"cypher-shell": denied}}
		iops, restore := externalNeo4jRestore(t, backend, seed)
		restore.Bound = 30 * time.Millisecond

		err := iops.waitForNeo4jRestoreWithin(restore, seed.URI, time.Minute)
		if err == nil {
			t.Fatal("waitForNeo4jRestoreWithin() = nil, want the run to fail")
		}
		if strings.Contains(err.Error(), "the database is not usable") {
			t.Errorf("err = %v, want the unanswered question reported rather than a verdict on the database", err)
		}
	})

	t.Run("a moment's refused session is still retried", func(t *testing.T) {
		// The condition the grace exists for: a server that has just replaced a
		// database refuses a session and then answers. Losing that would turn a
		// successful restore into a failed one.
		answers := map[string][]string{"cypher-shell": {showDatabaseOutput(`"online", ""`)}}
		backend := &recoveringProbeBackend{scriptedProbeBackend: scriptedProbeBackend{answers: answers}, failFirst: denied}
		iops, restore := externalNeo4jRestore(t, backend, seed)
		restore.Bound = time.Minute

		if err := iops.waitForNeo4jRestoreWithin(restore, seed.URI, time.Minute); err != nil {
			t.Fatalf("waitForNeo4jRestoreWithin() = %v, want nil: the second read found the database online", err)
		}
	})

	t.Run("a server that answers and is not online is still the database's verdict", func(t *testing.T) {
		// The other direction of the same split: a read that *did* reach the
		// server keeps the database-attributed message and the operator's bound.
		backend := &scriptedProbeBackend{answers: map[string][]string{
			"cypher-shell": {showDatabaseOutput(`"starting", ""`)},
		}}
		iops, restore := externalNeo4jRestore(t, backend, seed)
		restore.Bound = 30 * time.Millisecond

		err := iops.waitForNeo4jRestoreWithin(restore, seed.URI, time.Minute)
		if err == nil {
			t.Fatal("waitForNeo4jRestoreWithin() = nil, want the bound to fail the run")
		}
		for _, want := range []string{"not usable", "did not come online", "starting"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("err = %v, want it to mention %q", err, want)
			}
		}
	})
}

// recoveringProbeBackend fails its first command and then behaves like its
// embedded script. It is the "refused a session for a moment" shape, which no
// existing double can express: scriptedProbeBackend's fails map is permanent.
type recoveringProbeBackend struct {
	scriptedProbeBackend

	failFirst error
	failed    bool
}

func (b *recoveringProbeBackend) ExecSeparateContext(ctx context.Context, bound time.Duration, service string, command []string, opts *ExecOptions) (string, string, error) {
	if !b.failed {
		b.failed = true
		b.calls = append(b.calls, command)

		return "", "", b.failFirst
	}

	return b.scriptedProbeBackend.ExecSeparateContext(ctx, bound, service, command, opts)
}

// TestRefuseExternalCommunityDumpRestoreIsRaisedAtTheGate is Finding 6, and it
// is an ordering rather than a message.
//
// A Neo4j dump is loaded by writing into the server's own store directory, so it
// cannot be loaded into a server this run does not host. Both Plakar restore
// paths knew that from the snapshot's own tags before they touched anything —
// and raised it from inside restoreNeo4jCommunityStream, which they reach only
// after stopAppContainers, confirmAppContainersQuiesced and
// restartDependencies. The run took the deployment down to discover something it
// already knew, and then told the operator "No Infrahub service was stopped",
// which by then was false. Every other refusal on this feature is raised before
// the first destructive step (see external_backup_gate.go).
func TestRefuseExternalCommunityDumpRestoreIsRaisedAtTheGate(t *testing.T) {
	seed := restoreSeed{URI: "s3://infrahub-backups/seed/infrahub-2026.backup"}

	t.Run("a dump bound for an external database is refused", func(t *testing.T) {
		iops, _ := externalNeo4jRestore(t, &scriptedProbeBackend{}, seed)

		err := iops.refuseExternalCommunityDumpRestore(true)
		if err == nil {
			t.Fatal("refuseExternalCommunityDumpRestore(true) = nil, want the refusal: a dump cannot be written into a store this run has no access to")
		}
		// The claim the gate placement is what makes true.
		if !strings.Contains(err.Error(), "No Infrahub service was stopped and no data was changed") {
			t.Errorf("err = %v, want it to state that nothing was stopped — which is only true at the gate", err)
		}
		if !strings.Contains(err.Error(), "Enterprise online backup") {
			t.Errorf("err = %v, want the artifact an external server can be restored from named", err)
		}
	})

	t.Run("an Enterprise artifact bound for an external database is not refused", func(t *testing.T) {
		iops, _ := externalNeo4jRestore(t, &scriptedProbeBackend{}, seed)

		if err := iops.refuseExternalCommunityDumpRestore(false); err != nil {
			t.Errorf("refuseExternalCommunityDumpRestore(false) = %v, want nil: an online backup is exactly what a remote server can seed from", err)
		}
	})

	t.Run("a dump bound for an in-deployment database is not refused", func(t *testing.T) {
		// FR-015: the deployment every customer has today streams its dump into
		// its own container, and the gate must be invisible to it.
		iops := &InfrahubOps{config: &Configuration{}, executor: NewCommandExecutor()}

		if err := iops.refuseExternalCommunityDumpRestore(true); err != nil {
			t.Errorf("refuseExternalCommunityDumpRestore(true) = %v with no external database, want nil", err)
		}
	})

	t.Run("the backstop inside the load does not claim the deployment is untouched", func(t *testing.T) {
		// It is kept because restoreNeo4jCommunityStream is where the container
		// is actually needed, and a path added later could reach it without
		// passing a gate. What it must not do is repeat the gate's claim.
		endpoint := &DatabaseEndpoint{Service: serviceNeo4j, Location: EndpointLocationExternal, Hosts: []HostPort{{Host: "neo4j.example", Port: 7687}}}

		err := refuseExternalDumpRestoreAfterQuiescing(endpoint)
		if strings.Contains(err.Error(), "No Infrahub service was stopped") {
			t.Errorf("err = %v, want it not to claim nothing was stopped: by this point something has been", err)
		}
		if !strings.Contains(err.Error(), "returned to their prior scale") {
			t.Errorf("err = %v, want it to say what state the deployment is left in", err)
		}
	})
}
