package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// probeFailure is what a transient edition probe failure looks like: the
// wrapped exec error detectNeo4jEdition returns when `cypher-shell` could not
// be run or could not connect.
func probeFailure() error {
	return fmt.Errorf("failed to query neo4j edition: %w", errors.New("exit status 1: Connection refused"))
}

// TestNeo4jEditionInfo_ErroredProbeDoesNotTakeTheOfflineBranch is the defect
// this test file exists for. Community has no online backup, so the Community
// branch stops Infrahub and takes Neo4j offline for a cold dump — and the
// constructor used to report a probe that merely *errored* as Community. A
// momentary `cypher-shell` failure against an internal Enterprise database
// therefore produced an outage the deployment did not need, and an offline dump.
func TestNeo4jEditionInfo_ErroredProbeDoesNotTakeTheOfflineBranch(t *testing.T) {
	info := NewNeo4jEditionInfo("", probeFailure())

	if info.IsDetected {
		t.Error("IsDetected = true for a probe that failed")
	}
	if info.IsCommunity {
		t.Error("IsCommunity = true for a probe that failed: a failure is not evidence of Community")
	}
	if info.RequiresOfflineCapture() {
		t.Error("RequiresOfflineCapture = true for a probe that failed: the run would stop Infrahub and dump Neo4j cold")
	}
}

// TestNeo4jEditionInfo_RequireDeterminedEdition covers the refusal the backup
// paths make before anything is stopped.
func TestNeo4jEditionInfo_RequireDeterminedEdition(t *testing.T) {
	t.Run("an undetermined edition refuses the run", func(t *testing.T) {
		err := NewNeo4jEditionInfo("", probeFailure()).requireDeterminedEdition()
		if err == nil {
			t.Fatal("requireDeterminedEdition allowed a backup whose edition is unknown")
		}
		if !errors.Is(err, errNeo4jEditionUndetermined) {
			t.Errorf("err = %v, want errNeo4jEditionUndetermined so callers can recognise the refusal", err)
		}
		// The operator has to be able to tell an aborted run from an outage.
		if !strings.Contains(err.Error(), "no Infrahub service was stopped") {
			t.Errorf("err = %v, want it to state that nothing was stopped", err)
		}
		if !strings.Contains(err.Error(), "INFRAHUB_DB_USERNAME") {
			t.Errorf("err = %v, want it to name what to check", err)
		}
	})

	t.Run("a determined edition allows the run", func(t *testing.T) {
		for _, edition := range []string{neo4jEditionCommunity, neo4jEditionEnterprise} {
			if err := NewNeo4jEditionInfo(edition, nil).requireDeterminedEdition(); err != nil {
				t.Errorf("requireDeterminedEdition refused a determined %s edition: %v", edition, err)
			}
		}
	})
}

// TestNeo4jEditionInfo_DeterminedEditions keeps the detected cases behaving
// exactly as the release does: a determined Community edition still takes the
// offline branch, and Enterprise still does not.
func TestNeo4jEditionInfo_DeterminedEditions(t *testing.T) {
	tests := []struct {
		name        string
		detected    string
		wantEdition string
		wantOffline bool
	}{
		{"community", "community", neo4jEditionCommunity, true},
		{"community reported in mixed case", "Community", neo4jEditionCommunity, true},
		{"enterprise", "enterprise", neo4jEditionEnterprise, false},
		{"enterprise reported in upper case", "ENTERPRISE", neo4jEditionEnterprise, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := NewNeo4jEditionInfo(tt.detected, nil)
			if !info.IsDetected {
				t.Error("IsDetected = false for a probe that answered")
			}
			if info.Edition != tt.wantEdition {
				t.Errorf("Edition = %q, want %q", info.Edition, tt.wantEdition)
			}
			if got := info.RequiresOfflineCapture(); got != tt.wantOffline {
				t.Errorf("RequiresOfflineCapture = %t, want %t", got, tt.wantOffline)
			}
		})
	}
}

// TestNeo4jEditionInfo_RestoreFallbackIsUnchanged guards the asymmetry the
// type documents. The stop/dump decision now requires a determined edition,
// but the restore side keeps its conservative fallback: a Community dump is
// still restored with the Community method when the probe did not answer,
// which is what an existing deployment already relies on and does nothing to
// the running instance.
// TestDetectNeo4jEditionReadsTheGateOfEitherDirection is T148. The edition of
// an external database is read by the gate's probe, and detectNeo4jEdition
// returned that answer — but only from the capture's record. A restore records
// a workload instead, so against an external Neo4j the decision found nothing,
// and the restore stopped in requireDeterminedEdition against a database whose
// edition its own gate had established minutes earlier. Both restore entry
// points reach this decision after prepareDatabaseRestore, so the feature's
// restore path never ran.
//
// The fake answers `cypher-shell` with Community, so a fall-through to the
// absent container is visible twice over: as the wrong edition, and as a
// recorded exec.
func TestDetectNeo4jEditionReadsTheGateOfEitherDirection(t *testing.T) {
	external := func(t *testing.T) (*InfrahubOps, *gatedBackend) {
		backend := newGatedBackend()
		backend.locations[serviceNeo4j] = EndpointLocationExternal

		return newGatedOps(t, backend, BackendPlakar), backend
	}
	noProbeExec := func(t *testing.T, backend *gatedBackend) {
		t.Helper()
		for _, call := range backend.execs {
			if strings.HasPrefix(call, "cypher-shell") {
				t.Errorf("the edition was asked of a container that does not exist (%q), want the gate's probe read instead", call)
			}
		}
	}

	t.Run("a restore against an external Neo4j reads the edition its gate probed", func(t *testing.T) {
		iops, backend := external(t)
		iops.recordExternalRestore(serviceNeo4j, &externalRestore{externalSource: &externalSource{
			Endpoint: &DatabaseEndpoint{Service: serviceNeo4j},
			Facts:    probedFacts{Edition: neo4jEditionEnterprise},
		}})

		edition, err := iops.detectNeo4jEdition()
		if err != nil {
			t.Fatalf("detectNeo4jEdition() failed: %v; want the edition the restore's gate established (T148)", err)
		}
		if edition != neo4jEditionEnterprise {
			t.Errorf("edition = %q, want %q from the probe's facts", edition, neo4jEditionEnterprise)
		}
		noProbeExec(t, backend)
	})

	t.Run("a capture against an external Neo4j is unchanged", func(t *testing.T) {
		iops, backend := external(t)
		iops.recordExternalSource(serviceNeo4j, &externalSource{
			Endpoint: &DatabaseEndpoint{Service: serviceNeo4j},
			Facts:    probedFacts{Edition: neo4jEditionEnterprise},
		})

		edition, err := iops.detectNeo4jEdition()
		if err != nil {
			t.Fatalf("detectNeo4jEdition() failed: %v", err)
		}
		if edition != neo4jEditionEnterprise {
			t.Errorf("edition = %q, want %q from the probe's facts", edition, neo4jEditionEnterprise)
		}
		noProbeExec(t, backend)
	})

	t.Run("a restore whose probe reported no edition is undetermined, not Community", func(t *testing.T) {
		iops, backend := external(t)
		iops.recordExternalRestore(serviceNeo4j, &externalRestore{externalSource: &externalSource{
			Endpoint: &DatabaseEndpoint{Service: serviceNeo4j},
		}})

		edition, err := iops.detectNeo4jEdition()
		if err == nil {
			t.Fatalf("detectNeo4jEdition() = %q, nil; want an edition the server did not report left undetermined", edition)
		}
		if !strings.Contains(err.Error(), "did not report its edition") {
			t.Errorf("err = %v, want it to say the server did not report an edition", err)
		}
		noProbeExec(t, backend)
	})
}

func TestNeo4jEditionInfo_RestoreFallbackIsUnchanged(t *testing.T) {
	undetermined := NewNeo4jEditionInfo("", probeFailure())

	if undetermined.Edition != neo4jEditionCommunity {
		t.Errorf("Edition = %q, want the community restore fallback", undetermined.Edition)
	}

	edition, err := undetermined.ResolveRestoreEdition(neo4jEditionCommunity)
	if err != nil {
		t.Fatalf("ResolveRestoreEdition failed for a community backup: %v", err)
	}
	if edition != neo4jEditionCommunity {
		t.Errorf("edition = %q, want %q", edition, neo4jEditionCommunity)
	}

	if _, err := undetermined.ResolveRestoreEdition(neo4jEditionEnterprise); err == nil {
		t.Error("ResolveRestoreEdition accepted an Enterprise backup against an unknown edition")
	}

	// And the detected paths are untouched.
	enterprise := NewNeo4jEditionInfo(neo4jEditionEnterprise, nil)
	edition, err = enterprise.ResolveRestoreEdition(neo4jEditionCommunity)
	if err != nil {
		t.Fatalf("ResolveRestoreEdition failed for a community backup on enterprise: %v", err)
	}
	if edition != neo4jEditionCommunity {
		t.Errorf("edition = %q, want the community restore method for a community backup", edition)
	}
}

// ---------------------------------------------------------------------------
// T042/T044: the provenance fields, and the compatibility they must not break
// ---------------------------------------------------------------------------

// releasedBackupMetadata is BackupMetadata as a previously released version of
// the tool declares it: every field that shipped before 007, and none of the
// provenance fields it added.
//
// It is a declared copy rather than the live struct on purpose. Once those
// fields exist on BackupMetadata, unmarshalling a new artefact into it stops
// testing the old-reads-new direction at all — the reader would know every field
// it is supposed to be ignorant of, and FR-024's untested direction would go on
// being untested while looking covered. This is the only shape in the repository
// that can fail when an addition turns out not to be ignorable.
type releasedBackupMetadata struct {
	MetadataVersion int               `json:"metadata_version"`
	BackupID        string            `json:"backup_id"`
	CreatedAt       string            `json:"created_at"`
	ToolVersion     string            `json:"tool_version"`
	InfrahubVersion string            `json:"infrahub_version"`
	Components      []string          `json:"components"`
	Checksums       map[string]string `json:"checksums,omitempty"`
	Neo4jEdition    string            `json:"neo4j_edition,omitempty"`
	Redacted        bool              `json:"redacted,omitempty"`
	Encrypted       bool              `json:"encrypted,omitempty"`
}

// newProvenanceArtefact is an artefact carrying every field 007 added, in the
// shape the writer produces them, plus a field no version has ever defined.
//
// TestProvenanceArtefactMatchesWhatTheWriterProduces keeps this fixture honest:
// a fixture that guessed the shape would pin compatibility for an artefact the
// tool never writes.
func newProvenanceArtefact() []byte {
	return []byte(`{
		"metadata_version": 2025111200,
		"backup_id": "20260101_120000",
		"created_at": "2026-01-01T12:00:00Z",
		"tool_version": "9.9.9",
		"infrahub_version": "1.4.0",
		"components": ["database", "task-manager-db"],
		"checksums": {"neo4j.dump": "abc123"},
		"neo4j_edition": "enterprise",
		"source_endpoints": {
			"database": ["db-a.example.net:6362", "db-b.example.net:6362"],
			"task-manager-db": ["pg-a.example.net:5432"]
		},
		"source_roles": {
			"database": {"db-a.example.net:6362": "primary", "db-b.example.net:6362": "unknown"}
		},
		"capture_complete": true,
		"external_location": {"database": true, "task-manager-db": true},
		"a_field_no_version_has_ever_defined": {"nested": [1, 2, 3]}
	}`)
}

// externalProvenanceOps is a run that read a Neo4j cluster and a PostgreSQL
// server, both outside the deployment, and observed roles for the Neo4j members
// only — which is the shape the two probes actually produce.
func externalProvenanceOps() *InfrahubOps {
	iops := &InfrahubOps{config: &Configuration{}}

	iops.recordExternalSource(serviceNeo4j, &externalSource{
		Endpoint: &DatabaseEndpoint{
			Service:    serviceNeo4j,
			Location:   EndpointLocationExternal,
			Database:   "neo4j",
			Hosts:      []HostPort{{Host: "db-a.example.net", Port: 7687}, {Host: "db-b.example.net", Port: 7687}},
			ClientPort: 7687,
			BackupPort: 6362,
		},
		Facts: probedFacts{
			RolesObserved: true,
			Members:       []observedMember{{Address: "db-a.example.net:7687", Role: "primary"}},
			Answered:      HostPort{Host: "db-b.example.net", Port: 7687},
		},
	})

	iops.recordExternalSource(serviceTaskManagerDB, &externalSource{
		Endpoint: &DatabaseEndpoint{
			Service:    serviceTaskManagerDB,
			Location:   EndpointLocationExternal,
			Database:   "prefect",
			Hosts:      []HostPort{{Host: "pg-a.example.net", Port: 5432}},
			ClientPort: 5432,
		},
		// PostgreSQL is never asked for member roles, so it observed none.
		Facts: probedFacts{Answered: HostPort{Host: "pg-a.example.net", Port: 5432}},
	})

	return iops
}

// artefactMetadata is the metadata an artefact actually carries: built, and
// then completed where both write paths complete it.
//
// The two steps are separate in production for a reason FR-012 turns on.
// createBackupMetadata runs *before* backupDatabase and backupTaskManagerDB, so
// a completeness verdict read there is a prediction of what the captures will
// say — and necessarily "complete", since nothing has run. It is read again at
// serialization, which is what the writers do and what this mirrors; a test that
// exercised only the first half could not tell the two apart, which is how the
// field came to be unfalsifiable (see applyCaptureCompleteness).
func artefactMetadata(iops *InfrahubOps) *BackupMetadata {
	metadata := iops.createBackupMetadata("20260101_120000", true, "1.4.0", "enterprise")
	iops.applyCaptureCompleteness(metadata)

	return metadata
}

// TestProvenanceArtefactMatchesWhatTheWriterProduces ties the compatibility
// fixture to the writer. Every provenance key the fixture carries must be the
// key and the JSON shape the writers actually emit, so that the compatibility
// this file pins is compatibility with a real artefact.
func TestProvenanceArtefactMatchesWhatTheWriterProduces(t *testing.T) {
	written, err := json.Marshal(artefactMetadata(externalProvenanceOps()))
	if err != nil {
		t.Fatalf("marshalling the written metadata = %v, want nil", err)
	}

	var writtenShape, fixtureShape map[string]json.RawMessage
	if err := json.Unmarshal(written, &writtenShape); err != nil {
		t.Fatalf("unmarshalling the written metadata = %v, want nil", err)
	}
	if err := json.Unmarshal(newProvenanceArtefact(), &fixtureShape); err != nil {
		t.Fatalf("unmarshalling the fixture = %v, want nil", err)
	}

	for _, key := range []string{"source_endpoints", "source_roles", "capture_complete", "external_location"} {
		if _, ok := writtenShape[key]; !ok {
			t.Errorf("the writer emitted no %q for an external run; the fixture pins a shape nothing produces", key)

			continue
		}

		// Same JSON kind — object against object, not object against array.
		// This is the mismatch a hand-written fixture actually gets wrong.
		if got, want := fixtureShape[key][0], writtenShape[key][0]; got != want {
			t.Errorf("fixture %q starts with %q, the writer's with %q: the fixture is a different JSON shape", key, got, want)
		}
	}
}

// TestBackupMetadataRecordsWhereAnExternalCaptureCameFrom is T042 against the
// contract: what each field says, and — for source_roles — what its absence
// says, which is a different statement from unknown.
func TestBackupMetadataRecordsWhereAnExternalCaptureCameFrom(t *testing.T) {
	metadata := artefactMetadata(externalProvenanceOps())

	t.Run("the declared metadata version does not move", func(t *testing.T) {
		// FR-024: these fields are additive, so a released tool that compares
		// the version must still accept the artefact.
		if metadata.MetadataVersion != 2025111200 {
			t.Errorf("MetadataVersion = %d, want it unchanged", metadata.MetadataVersion)
		}
	})

	t.Run("the endpoints are recorded in try order", func(t *testing.T) {
		want := []string{"db-a.example.net:6362", "db-b.example.net:6362"}
		if got := metadata.SourceEndpoints[serviceNeo4j]; !slices.Equal(got, want) {
			t.Errorf("SourceEndpoints[%q] = %v, want %v in try order", serviceNeo4j, got, want)
		}
	})

	t.Run("PostgreSQL provenance names the port the dump travelled over", func(t *testing.T) {
		// An earlier version of captureTargets stamped Neo4j's backup port on
		// every service, which recorded :6362 for a dump that went over :5432 —
		// a false statement about where the data came from.
		want := []string{"pg-a.example.net:5432"}
		if got := metadata.SourceEndpoints[serviceTaskManagerDB]; !slices.Equal(got, want) {
			t.Errorf("SourceEndpoints[%q] = %v, want %v", serviceTaskManagerDB, got, want)
		}
	})

	t.Run("no field claims which endpoint served the capture", func(t *testing.T) {
		// FR-005 forbids it: with several endpoints supplied, no documented
		// interface reports it. The probe's Answered is deliberately not here.
		blob, err := json.Marshal(metadata)
		if err != nil {
			t.Fatalf("json.Marshal() = %v, want nil", err)
		}

		var fields map[string]json.RawMessage
		if err := json.Unmarshal(blob, &fields); err != nil {
			t.Fatalf("json.Unmarshal() = %v, want nil", err)
		}
		for key := range fields {
			if strings.Contains(key, "answer") || strings.Contains(key, "served") || strings.Contains(key, "captured_from") {
				t.Errorf("metadata carries %q: no field may claim which endpoint served the capture (FR-005)", key)
			}
		}
	})

	t.Run("an observed role that no member matched is unknown, not absent", func(t *testing.T) {
		roles := metadata.SourceRoles[serviceNeo4j]
		if got := roles["db-a.example.net:6362"]; got != "primary" {
			t.Errorf("role of db-a = %q, want primary as the server reported it", got)
		}
		if got, ok := roles["db-b.example.net:6362"]; !ok || got != roleUnknown {
			t.Errorf("role of db-b = %q (present %t), want %q as a recorded value", got, ok, roleUnknown)
		}
	})

	t.Run("a database whose roles were never sought carries none", func(t *testing.T) {
		// Absent means "roles not observed"; unknown means "observed, and the
		// server named no role". Recording unknown for PostgreSQL, which is
		// never asked, would claim an observation the run never made.
		if roles, ok := metadata.SourceRoles[serviceTaskManagerDB]; ok {
			t.Errorf("SourceRoles[%q] = %v, want it absent: its roles were never observed", serviceTaskManagerDB, roles)
		}
	})

	t.Run("capture_complete states what the capture reported", func(t *testing.T) {
		if metadata.CaptureComplete == nil || !*metadata.CaptureComplete {
			t.Errorf("CaptureComplete = %v, want true for a run whose captures all reported complete", metadata.CaptureComplete)
		}
	})

	t.Run("external_location covers every database, so the other fields do not signal externality", func(t *testing.T) {
		// Keyed by the deployment service name, which is what an endpoint
		// resolves under — not by whatever a backend calls its components.
		for _, service := range []string{serviceNeo4j, serviceTaskManagerDB} {
			if external, ok := metadata.ExternalLocation[service]; !ok || !external {
				t.Errorf("ExternalLocation[%q] = %t (present %t), want true", service, external, ok)
			}
		}
	})

	t.Run("an internal database in an external run is recorded as internal", func(t *testing.T) {
		iops := &InfrahubOps{config: &Configuration{}}
		iops.recordExternalSource(serviceNeo4j, &externalSource{
			Endpoint: &DatabaseEndpoint{Service: serviceNeo4j, Database: "neo4j", Hosts: []HostPort{{Host: "db-a", Port: 7687}}, BackupPort: 6362},
			Facts:    probedFacts{RolesObserved: true},
		})

		mixed := iops.createBackupMetadata("20260101_120000", true, "1.4.0", "enterprise")
		if external, ok := mixed.ExternalLocation[serviceTaskManagerDB]; !ok || external {
			t.Errorf("ExternalLocation[%q] = %t (present %t), want false: it is recorded, and recorded as internal", serviceTaskManagerDB, external, ok)
		}
		if _, ok := mixed.SourceEndpoints[serviceTaskManagerDB]; ok {
			t.Errorf("SourceEndpoints[%q] = %v, want none for a database inside the deployment", serviceTaskManagerDB, mixed.SourceEndpoints[serviceTaskManagerDB])
		}
	})

	t.Run("an incomplete capture is recorded as incomplete", func(t *testing.T) {
		// A reader should never meet this, because FR-012 removes such an
		// artefact rather than retaining it. It is what remains if the removal
		// itself failed, so it has to be writable and it has to survive a
		// round trip as false rather than as absent.
		//
		// The order below is production's order, and that is the assertion.
		// The metadata is built first, the capture reports its verdict second —
		// createBackupMetadata runs before backupDatabase — and the field is
		// written last. Built and read in one step, as it was, the map is always
		// empty when it is read and this case cannot arise at all: `true` went
		// into every artefact ever written, and the reader that refuses `false`
		// guarded a value nothing could produce.
		iops := externalProvenanceOps()
		metadata := iops.createBackupMetadata("20260101_120000", true, "1.4.0", "enterprise")

		iops.recordIncompleteCapture(serviceNeo4j, "the operation produced an artifact and then reported a problem")
		iops.applyCaptureCompleteness(metadata)

		blob, err := json.Marshal(metadata)
		if err != nil {
			t.Fatalf("json.Marshal() = %v, want nil", err)
		}

		var reread BackupMetadata
		if err := json.Unmarshal(blob, &reread); err != nil {
			t.Fatalf("json.Unmarshal() = %v, want nil", err)
		}
		if reread.CaptureComplete == nil {
			t.Fatal("CaptureComplete = absent after a round trip, want false: absent means complete, so this would read as its own opposite")
		}
		if *reread.CaptureComplete {
			t.Error("CaptureComplete = true, want false")
		}
	})
}

// TestInternalDeploymentMetadataIsUnchanged is FR-015. A deployment whose
// databases are all internal must produce the metadata it always has, and the
// check is on the serialized bytes rather than on the struct: a field that
// appears only in JSON is still a change to the artefact.
func TestInternalDeploymentMetadataIsUnchanged(t *testing.T) {
	iops := &InfrahubOps{config: &Configuration{}}

	// Through both steps a writer performs, so the completeness step is held to
	// the same guarantee: an internal run writes no provenance field, and that
	// includes the one now written at serialization.
	metadata := artefactMetadata(iops)

	blob, err := json.Marshal(metadata)
	if err != nil {
		t.Fatalf("json.Marshal() = %v, want nil", err)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(blob, &fields); err != nil {
		t.Fatalf("json.Unmarshal() = %v, want nil", err)
	}

	for _, key := range []string{"source_endpoints", "source_roles", "capture_complete", "external_location"} {
		if _, ok := fields[key]; ok {
			t.Errorf("an all-internal run wrote %q; its metadata must be what it has always been (FR-015)", key)
		}
	}

	// And the keys it does write are exactly the pre-feature set.
	want := []string{"backup_id", "components", "created_at", "infrahub_version", "metadata_version", "neo4j_edition", "tool_version"}
	got := make([]string, 0, len(fields))
	for key := range fields {
		got = append(got, key)
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("metadata keys = %v, want %v", got, want)
	}
}

// TestReleasedReaderIsUnaffectedByTheProvenanceFields is FR-024's untested
// direction: a previously released tool reads an artefact carrying all four new
// fields. It must neither refuse it nor act incorrectly on the fields it can
// see, and it has no way to notice the ones it cannot.
func TestReleasedReaderIsUnaffectedByTheProvenanceFields(t *testing.T) {
	var released releasedBackupMetadata
	if err := json.Unmarshal(newProvenanceArtefact(), &released); err != nil {
		t.Fatalf("a released reader refused a new artefact: %v", err)
	}

	if released.MetadataVersion != metadataVersion {
		t.Errorf("MetadataVersion = %d, want %d: a released tool comparing this must still accept the artefact", released.MetadataVersion, metadataVersion)
	}
	if released.Neo4jEdition != neo4jEditionEnterprise {
		t.Errorf("Neo4jEdition = %q, want enterprise: the restore's edition decision reads this", released.Neo4jEdition)
	}
	if got := released.Components; !slices.Equal(got, []string{serviceNeo4j, serviceTaskManagerDB}) {
		t.Errorf("Components = %v, want both databases: this is what decides which are restored", got)
	}
	if released.Checksums["neo4j.dump"] != "abc123" {
		t.Errorf("Checksums = %v, want the artefact's own", released.Checksums)
	}
	if released.Redacted || released.Encrypted {
		t.Errorf("Redacted/Encrypted = %t/%t, want both false: neither is stated by this artefact", released.Redacted, released.Encrypted)
	}
}

// TestNewReaderTreatsAPreFeatureArtefactAsInternalAndComplete is FR-016 and the
// reader rule in contracts/artifact-and-metadata.md: an artefact stating none of
// the four fields is an internal, complete capture from an unrecorded endpoint —
// which is exactly what every previously released version produced. No warning,
// no migration, no operator action.
func TestNewReaderTreatsAPreFeatureArtefactAsInternalAndComplete(t *testing.T) {
	preFeature := []byte(`{
		"metadata_version": 2025111200,
		"backup_id": "20250101_120000",
		"created_at": "2025-01-01T12:00:00Z",
		"tool_version": "1.0.0",
		"infrahub_version": "1.0.0",
		"components": ["database"],
		"neo4j_edition": "enterprise"
	}`)

	var metadata BackupMetadata
	if err := json.Unmarshal(preFeature, &metadata); err != nil {
		t.Fatalf("the current reader refused a pre-feature artefact: %v", err)
	}

	if metadata.CaptureComplete != nil {
		t.Errorf("CaptureComplete = %v, want absent: absence is what says complete", *metadata.CaptureComplete)
	}
	if metadata.SourceEndpoints != nil || metadata.SourceRoles != nil {
		t.Errorf("SourceEndpoints/SourceRoles = %v/%v, want both absent rather than empty maps that would read as 'recorded nothing'",
			metadata.SourceEndpoints, metadata.SourceRoles)
	}
	if metadata.ExternalLocation != nil {
		t.Errorf("ExternalLocation = %v, want absent: an unstated location is internal", metadata.ExternalLocation)
	}

	// The distinction the reader rule turns on: absent is not false. A reader
	// that collapsed the two would refuse every artefact written before this
	// feature as an incomplete capture.
	if metadata.CaptureComplete != nil && !*metadata.CaptureComplete {
		t.Error("a pre-feature artefact read as an incomplete capture")
	}
}

// TestRefuseIncompleteCaptureIsTheReaderOfTheField is T102. The contract calls
// capture_complete the last-resort identifier of an unusable artefact — "a
// reader that does encounter it MUST NOT offer it as a restore point" — and it
// had no reader on any restore or restore-point-selection path, so the field
// never rejected anything. `--latest` selects by name and recency, so the
// artefact a failed removal left behind is exactly the one it picks.
func TestRefuseIncompleteCaptureIsTheReaderOfTheField(t *testing.T) {
	complete := true
	incomplete := false

	tests := []struct {
		name    string
		field   *bool
		wantErr bool
	}{
		{"a pre-feature artefact states nothing, and absence means complete", nil, false},
		{"a complete capture restores", &complete, false},
		{"an incomplete capture is refused", &incomplete, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			metadata := &BackupMetadata{CaptureComplete: tt.field}
			err := metadata.refuseIncompleteCapture("infrahub_backup_20260903_120000.tar.gz")
			if tt.wantErr != (err != nil) {
				t.Fatalf("refuseIncompleteCapture() = %v, want an error: %t", err, tt.wantErr)
			}
			if err == nil {
				return
			}
			if !strings.Contains(err.Error(), "infrahub_backup_20260903_120000.tar.gz") {
				t.Errorf("err = %v, want the artefact named", err)
			}
			if !strings.Contains(err.Error(), "capture_complete=false") {
				t.Errorf("err = %v, want the field that decided it named", err)
			}
			if !strings.Contains(err.Error(), "restore an earlier backup") {
				t.Errorf("err = %v, want the operator action named (SC-008)", err)
			}
		})
	}
}
