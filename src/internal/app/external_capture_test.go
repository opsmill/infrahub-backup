package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// The capture path is built so that everything decidable without a database is
// decided by a pure function over values: the argv that reaches a customer's
// server, the refusal an unsupported edition produces, the verdict a capture's
// own report earns, and where the credentials are and are not. Those are what
// this file asserts.
//
// What it cannot assert is an actual capture. There is no external Neo4j or
// PostgreSQL here, so no test below establishes that `neo4j-admin database
// backup --from=…` succeeds against a real server, that the store-size JMX bean
// is exposed by a given deployment, or that a real partial capture produces the
// output these verdicts are written for. Those belong to the end-to-end
// topology (research R8, T072).

// ---------------------------------------------------------------------------
// T034 / FR-015: one argv builder, and the internal one unchanged
// ---------------------------------------------------------------------------

// TestNeo4jCaptureCommandInternalArgvIsUnchanged pins FR-015 character for
// character. Both in-container capture entry points now build their command
// through neo4jCaptureCommand, so the thing that could silently change an
// existing deployment's behaviour is this function, and these are the two argvs
// it must keep producing.
func TestNeo4jCaptureCommandInternalArgvIsUnchanged(t *testing.T) {
	cases := []struct {
		name string
		req  neo4jCaptureRequest
		want []string
	}{
		{
			name: "the directory capture, as backupNeo4jEnterprise wrote it",
			req: neo4jCaptureRequest{
				Database:       "neo4j",
				BackupMetadata: "all",
				ToPath:         neo4jTempBackupDir,
			},
			want: []string{"neo4j-admin", "database", "backup", "--expand-commands", "--include-metadata=all", "--to-path=/tmp/infrahubops", "neo4j"},
		},
		{
			name: "the streaming capture, as backupNeo4jEnterpriseStream wrote it",
			req: neo4jCaptureRequest{
				Database:       "neo4j",
				BackupMetadata: "all",
				ToPath:         neo4jTempBackupDir,
				Uncompressed:   true,
			},
			want: []string{"neo4j-admin", "database", "backup", "--expand-commands", "--include-metadata=all", "--compress=false", "--to-path=/tmp/infrahubops", "neo4j"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := neo4jCaptureCommand(tc.req)
			if strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") {
				t.Errorf("neo4jCaptureCommand() = %q\nwant %q", got, tc.want)
			}
		})
	}
}

// TestNeo4jCaptureCommandAddsTheOrderedFromList is T034. The members are one
// --from value, comma-separated, in the order they were resolved, because that
// is the order the command tries them in (FR-005).
func TestNeo4jCaptureCommandAddsTheOrderedFromList(t *testing.T) {
	got := neo4jCaptureCommand(neo4jCaptureRequest{
		Database:       "neo4j",
		BackupMetadata: "all",
		ToPath:         externalNeo4jCaptureDir,
		TempPath:       externalNeo4jStagingDir,
		From: []HostPort{
			{Host: "db-a.example.net", Port: 6362},
			{Host: "db-b.example.net", Port: 6362},
			{Host: "2001:db8::1", Port: 6362},
		},
		Uncompressed: true,
	})

	want := []string{
		"neo4j-admin", "database", "backup",
		"--include-metadata=all",
		"--compress=false",
		"--from=db-a.example.net:6362,db-b.example.net:6362,[2001:db8::1]:6362",
		"--temp-path=" + externalNeo4jStagingDir,
		"--to-path=" + externalNeo4jCaptureDir,
		"neo4j",
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("neo4jCaptureCommand() = %q\nwant %q", got, want)
	}
}

// TestExpandCommandsIsOnlyForTheInContainerCapture pins a defect found by running
// the real thing rather than asserting a string.
//
// `neo4j-admin` refuses to read a configuration file whose permissions are looser
// than 0640 when it is given `--expand-commands`, and it reads both `neo4j.conf`
// and `neo4j-admin.conf`. The image this tool pins for the transient workload
// ships them world-writable — `neo4j:5-enterprise` has `neo4j.conf` at 0777 —
// so the flag aborted every external capture before the database was contacted,
// blaming a file permission the operator never set:
//
//	Failed to read config /var/lib/neo4j/conf/neo4j.conf: … does not have the
//	correct file permissions to evaluate commands.
//
// Verified 2026-09-03 against neo4j:5-enterprise (5.26.30): with the flag the
// capture fails on config permissions; without it the identical argv completes a
// full remote capture from a live backup listener.
//
// The in-container capture keeps the flag, because there the configuration being
// expanded is the deployment's own — and that is also what holds FR-015.
func TestExpandCommandsIsOnlyForTheInContainerCapture(t *testing.T) {
	hasExpand := func(argv []string) bool {
		for _, arg := range argv {
			if arg == "--expand-commands" {
				return true
			}
		}

		return false
	}

	t.Run("the in-container capture keeps it", func(t *testing.T) {
		argv := neo4jCaptureCommand(neo4jCaptureRequest{
			Database:       "neo4j",
			BackupMetadata: "all",
			ToPath:         neo4jTempBackupDir,
		})
		if !hasExpand(argv) {
			t.Errorf("the in-container capture must keep --expand-commands (FR-015); got %q", argv)
		}
	})

	t.Run("a capture with a member list does not", func(t *testing.T) {
		argv := neo4jCaptureCommand(neo4jCaptureRequest{
			Database:       "neo4j",
			BackupMetadata: "all",
			ToPath:         externalNeo4jCaptureDir,
			From:           []HostPort{{Host: "db.example.net", Port: 6362}},
		})
		if hasExpand(argv) {
			t.Errorf("a remote capture must not pass --expand-commands: the pinned image's conf files are world-writable and neo4j-admin refuses to read them; got %q", argv)
		}
	})
}

// TestCaptureTargetsUsesEachServicesOwnPort is the arm the reverted version of
// this path did not have. Stamping the Neo4j backup port onto a PostgreSQL host
// recorded :6362 as the provenance of a dump that travelled over :5432 — a
// false statement about where the data came from.
func TestCaptureTargetsUsesEachServicesOwnPort(t *testing.T) {
	t.Run("Neo4j members are re-pointed at the backup listener", func(t *testing.T) {
		endpoint := DatabaseEndpoint{
			Service:    serviceNeo4j,
			BackupPort: 6362,
			// Deliberately carrying client ports of their own: an address list's
			// ports are Bolt ports, never the backup listener's.
			Hosts: []HostPort{{Host: "a", Port: 7687}, {Host: "b", Port: 7688}},
		}

		if got := renderHostPorts(endpoint.captureTargets()); got != "a:6362,b:6362" {
			t.Errorf("captureTargets() = %q, want both members on the backup port", got)
		}
	})

	t.Run("a Neo4j endpoint with no backup port falls back to the vendor default", func(t *testing.T) {
		endpoint := DatabaseEndpoint{Service: serviceNeo4j, Hosts: []HostPort{{Host: "a", Port: 7687}}}

		if got := renderHostPorts(endpoint.captureTargets()); got != fmt.Sprintf("a:%d", defaultNeo4jBackupPort) {
			t.Errorf("captureTargets() = %q, want the default backup port", got)
		}
	})

	t.Run("PostgreSQL members keep the client port the dump travels over", func(t *testing.T) {
		endpoint := DatabaseEndpoint{
			Service: serviceTaskManagerDB,
			// A backup port set on the configuration must not reach here: it is
			// a Neo4j setting and PostgreSQL has no second listener.
			BackupPort: 6362,
			Hosts:      []HostPort{{Host: "pg-a", Port: 5432}, {Host: "pg-b", Port: 5433}},
		}

		if got := renderHostPorts(endpoint.captureTargets()); got != "pg-a:5432,pg-b:5433" {
			t.Errorf("captureTargets() = %q, want each member's own client port", got)
		}
	})
}

// ---------------------------------------------------------------------------
// T035 / FR-008: refusing an external Community capture
// ---------------------------------------------------------------------------

func TestRefuseExternalCommunityCapture(t *testing.T) {
	endpoint := &DatabaseEndpoint{Service: serviceNeo4j, Hosts: []HostPort{{Host: "db.example.net", Port: 7687}}}

	t.Run("a Community server is refused, naming the mechanism's requirement", func(t *testing.T) {
		err := refuseExternalCommunityCapture(endpoint, "community")
		if !errors.Is(err, errExternalCommunityCapture) {
			t.Fatalf("err = %v, want %v", err, errExternalCommunityCapture)
		}

		// The failure-message contract: the mechanism needs the database's own
		// storage, the remote alternative is an Enterprise feature, and nothing
		// is missing from the deployment. The last clause is the one the
		// pre-feature "service not found" got wrong.
		for _, want := range []string{
			"direct access to the database's own storage",
			"Enterprise Edition feature",
			"Nothing is missing from the deployment",
			"No Infrahub service was stopped",
			"db.example.net:7687",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("err = %v, want it to say %q", err, want)
			}
		}
	})

	t.Run("an Enterprise server is not refused", func(t *testing.T) {
		if err := refuseExternalCommunityCapture(endpoint, "enterprise"); err != nil {
			t.Errorf("refuseExternalCommunityCapture(enterprise) = %v, want nil", err)
		}
	})

	t.Run("an edition the server did not report is not read as Community", func(t *testing.T) {
		// The same asymmetry Neo4jEditionInfo documents: guessing the
		// destructive branch from an answer that never came is what Principle II
		// forbids. requireDeterminedEdition is what stops such a run instead.
		for _, edition := range []string{"", "   ", "unknown"} {
			if err := refuseExternalCommunityCapture(endpoint, edition); err != nil {
				t.Errorf("refuseExternalCommunityCapture(%q) = %v, want nil", edition, err)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// T036 / FR-012: completeness from what the operation reported
// ---------------------------------------------------------------------------

// TestNeo4jArtifactProducedRequiresTheSeparator is the defect that made a run
// record capture_complete for a capture of nothing. `<name>` as a bare prefix
// is satisfied by a sibling database's artifact, so the run reported a complete
// capture having captured a different database.
func TestNeo4jArtifactProducedRequiresTheSeparator(t *testing.T) {
	cases := []struct {
		name     string
		database string
		listing  string
		want     bool
	}{
		{"the database's own artifact", "neo4j", "neo4j-2026-09-03T00-00-00.backup", true},
		{"a sibling database's artifact does not count", "neo4j", "neo4j2-2026-09-03T00-00-00.backup", false},
		{"a longer sibling name does not count", "infrahub", "infrahub-staging-2026-09-03T00-00-00.backup\n", false},
		{"an unrelated database does not count", "neo4j", "prefect-2026-09-03T00-00-00.backup", false},
		{"a non-artifact file does not count", "neo4j", "neo4j-2026-09-03T00-00-00.tmp", false},
		{"an empty listing", "neo4j", "", false},
		{"the artifact among other entries", "neo4j", "\nstaging\nneo4j-2026-09-03T00-00-00.backup\n", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := neo4jArtifactProduced(tc.database, tc.listing); got != tc.want {
				t.Errorf("neo4jArtifactProduced(%q, %q) = %v, want %v", tc.database, tc.listing, got, tc.want)
			}
		})
	}
}

// TestClassifyNeo4jCapture is the completeness signal itself. Exit status alone
// cannot establish it — code 1 means "failed, or succeeded with some servers
// uncontactable" and the vendor documents no marker separating them — so the
// signal is a reported success *and* the artifact being there, and nothing else
// may promote a capture to complete.
func TestClassifyNeo4jCapture(t *testing.T) {
	failed := errors.New("exit status 1")
	// The endpoints the capture was asked to try. They are what make an
	// "unreachable" line attributable to this database rather than to the
	// cluster the command ran through (see uncontactableEvidence).
	captureEndpoints := []HostPort{{Host: "db-a", Port: 6362}, {Host: "db-b", Port: 6362}, {Host: "db-b.example.net", Port: 6362}}

	t.Run("reported success with the artifact is the only complete verdict", func(t *testing.T) {
		verdict := classifyNeo4jCapture(nil, "Backup complete.", true, captureEndpoints)
		if !verdict.Complete {
			t.Errorf("verdict = %+v, want complete", verdict)
		}
		if verdict.Detail != "" {
			t.Errorf("Detail = %q, want empty for a complete capture", verdict.Detail)
		}
	})

	t.Run("a reported success that mentions an unreachable member is still the vendor's success", func(t *testing.T) {
		// Not overridden on a phrase match: the markers corroborate, they never
		// decide, because a phrasing that changes between releases must not be
		// able to refuse work the server actually did.
		verdict := classifyNeo4jCapture(nil, "Backup complete.\nServer db-b was uncontactable.", true, captureEndpoints)
		if !verdict.Complete {
			t.Errorf("verdict = %+v, want complete: the operation reported success and left the artifact", verdict)
		}
	})

	t.Run("a reported success with no artifact is not complete", func(t *testing.T) {
		verdict := classifyNeo4jCapture(nil, "Nothing to do.", false, captureEndpoints)
		if verdict.Complete {
			t.Fatalf("verdict = %+v, want incomplete", verdict)
		}
		for _, want := range []string{"reported success but left no artifact", "Nothing to do."} {
			if !strings.Contains(verdict.Detail, want) {
				t.Errorf("Detail = %q, want it to say %q", verdict.Detail, want)
			}
		}
	})

	t.Run("an artifact plus a non-zero exit is incomplete, and says why the status is ambiguous", func(t *testing.T) {
		verdict := classifyNeo4jCapture(failed, "", true, captureEndpoints)
		if verdict.Complete {
			t.Fatalf("verdict = %+v, want incomplete", verdict)
		}
		if !strings.Contains(verdict.Detail, "no exit status distinguishes the two") {
			t.Errorf("Detail = %q, want it to name the conflation rather than assert a failure", verdict.Detail)
		}
	})

	t.Run("an artifact plus a named unreachable endpoint quotes the line", func(t *testing.T) {
		verdict := classifyNeo4jCapture(failed, "Server db-b.example.net was uncontactable", true, captureEndpoints)
		if verdict.Complete {
			t.Fatalf("verdict = %+v, want incomplete", verdict)
		}
		if !strings.Contains(verdict.Detail, "db-b.example.net") {
			t.Errorf("Detail = %q, want it to quote the endpoint the command named", verdict.Detail)
		}
	})

	t.Run("no artifact and a non-zero exit is incomplete", func(t *testing.T) {
		verdict := classifyNeo4jCapture(failed, "", false, captureEndpoints)
		if verdict.Complete || verdict.ArtifactProduced {
			t.Errorf("verdict = %+v, want incomplete with no artifact", verdict)
		}
	})
}

// TestUncontactableEvidenceReadsTheErrorToo is a consequence of bounded
// execution returning stdout alone. neo4j-admin writes its diagnostics to
// stderr, which withStderr folds into the error — so evidence read from stdout
// only, as the reverted version did back when the streams were merged, looks for
// the line in the one place a failing command does not write it.
func TestUncontactableEvidenceReadsTheErrorToo(t *testing.T) {
	targets := []HostPort{{Host: "db-a", Port: 6362}, {Host: "db-b.example.net", Port: 6362}}
	runErr := fmt.Errorf("exit status 1: %s", "Failed to connect to db-b.example.net:6362")

	if got := uncontactableEvidence("", runErr, targets); !strings.Contains(got, "db-b.example.net") {
		t.Errorf("uncontactableEvidence() = %q, want the line from the error's stderr", got)
	}
	if got := uncontactableEvidence("Server db-a was unreachable", nil, targets); got != "Server db-a was unreachable" {
		t.Errorf("uncontactableEvidence() = %q, want the stdout line", got)
	}
	if got := uncontactableEvidence("Backup complete.", nil, targets); got != "" {
		t.Errorf("uncontactableEvidence() = %q, want no evidence", got)
	}
}

// TestUncontactableEvidenceIsAttributedToAnEndpoint is T115. The error chain
// carries kubectl's own stderr, and kubectl words a control-plane outage
// exactly the way a database that will not answer is worded — so an API server
// that stopped answering was reported to the operator as a named Neo4j endpoint
// the capture could not reach, pointing the diagnosis at the wrong system.
func TestUncontactableEvidenceIsAttributedToAnEndpoint(t *testing.T) {
	targets := []HostPort{{Host: "db-a", Port: 6362}, {Host: "db-b", Port: 6362}}

	cases := []struct {
		name    string
		output  string
		runErr  error
		want    string
		wantErr string
	}{
		{
			name:   "an API-server outage is not evidence about the database",
			runErr: errors.New("failed to exec in the transient database workload: Unable to connect to the server: dial tcp 10.0.0.1:6443: connect: connection refused"),
			want:   "",
		},
		{
			name:   "a kubectl upgrade failure is not evidence about the database",
			runErr: errors.New("error: unable to upgrade connection: pod does not exist"),
			want:   "",
		},
		{
			name:   "the command's own line naming a supplied endpoint is evidence",
			runErr: errors.New("exit status 1: Server db-b:7687 was uncontactable during the backup"),
			want:   "exit status 1: Server db-b:7687 was uncontactable during the backup",
		},
		{
			name:   "an endpoint named on stdout is evidence",
			output: "Backup failed.\nunable to connect to db-a:6362",
			want:   "unable to connect to db-a:6362",
		},
		{
			name:   "a line naming an endpoint with no failure wording is not evidence",
			output: "Backup of db-a:6362 completed in 4m12s",
			want:   "",
		},
		{
			// Both are present in one chain, and the one that says something
			// about this database is the one the operator is given.
			name:   "the endpoint's line is preferred over the transport's",
			runErr: errors.New("Unable to connect to the server: connection refused: Server db-a was uncontactable"),
			want:   "Unable to connect to the server: connection refused: Server db-a was uncontactable",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := uncontactableEvidence(tc.output, tc.runErr, targets); got != tc.want {
				t.Errorf("uncontactableEvidence() = %q, want %q", got, tc.want)
			}
		})
	}

	// And the verdict that reads it: an API-server outage must not be reported
	// as an endpoint the capture could not reach. The run still fails — the
	// exit status and the missing artefact decide that — it is the diagnosis
	// that must not name the wrong system.
	t.Run("the verdict does not blame an endpoint for a control-plane outage", func(t *testing.T) {
		outage := errors.New("Unable to connect to the server: dial tcp 10.0.0.1:6443: connect: connection refused")

		verdict := classifyNeo4jCapture(outage, "", true, targets)
		if verdict.Complete {
			t.Fatalf("verdict = %+v, want incomplete", verdict)
		}
		if strings.Contains(verdict.Detail, "could not reach") {
			t.Errorf("Detail = %q, want it not to report an endpoint the capture could not reach", verdict.Detail)
		}
		if !strings.Contains(verdict.Detail, "no exit status distinguishes the two") {
			t.Errorf("Detail = %q, want the ambiguous-status verdict", verdict.Detail)
		}
	})
}

// ---------------------------------------------------------------------------
// T037 / T038: reading the probe's answers
// ---------------------------------------------------------------------------

// TestParseCypherRowLocatesItsHeader is the carried-over T085 defect and the
// half of it T116 found still open. The reverted parser skipped index 0; its
// replacement skipped the first *non-empty* line, which handles the blank line
// and gets the notice case — the one its own comment claimed — exactly wrong:
// the notice is skipped as the header and the header is returned as the value.
// A header parses far enough to be wrong rather than to fail, so a version
// reads as "version" and a store size as "attributes.Value.value".
func TestParseCypherRowLocatesItsHeader(t *testing.T) {
	cases := []struct {
		name   string
		output string
		column string
		want   string
	}{
		{"header then value", "version\n\"2025.10.1\"\n", neo4jVersionColumn, "2025.10.1"},
		{"a blank line before the header", "\nversion\n\"2025.10.1\"\n", neo4jVersionColumn, "2025.10.1"},
		{"several blank lines before the header", "\n\n\nversion\n2025.10.1\n", neo4jVersionColumn, "2025.10.1"},
		{"blank lines between header and value", "version\n\n2025.10.1\n", neo4jVersionColumn, "2025.10.1"},
		{"a header with no rows", "version\n", neo4jVersionColumn, ""},
		{"nothing at all", "", neo4jVersionColumn, ""},
		{
			// The T116 case. Skipping one non-empty line returns "version".
			name:   "a notice before the header",
			output: "This query has been deprecated and will be removed.\nversion\n\"2025.10.1\"\n",
			column: neo4jVersionColumn,
			want:   "2025.10.1",
		},
		{
			name:   "several notices and a blank line before the header",
			output: "WARNING: routing table refreshed\n\nNotice: server is in maintenance\nversion\n2025.10.1\n",
			column: neo4jVersionColumn,
			want:   "2025.10.1",
		},
		{
			// The store size is returned under the expression text, and it is
			// the value the sizing arithmetic reads: the header returned as
			// data is what made a store size parse as a column name.
			name:   "a notice before a store size",
			output: "Notice: metric names change in a future release\nattributes.Value.value\n1073741824\n",
			column: neo4jStoreSizeColumn,
			want:   "1073741824",
		},
		{
			// No header for what was asked, so no answer. Every caller turns
			// that into a failure naming what it asked for, which is the safe
			// direction — the alternative is a value the run made up.
			name:   "notices and nothing else",
			output: "Notice: something happened\nNotice: something else happened\n",
			column: neo4jVersionColumn,
			want:   "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseCypherScalar(tc.output, tc.column); got != tc.want {
				t.Errorf("parseCypherScalar(%q, %q) = %q, want %q", tc.output, tc.column, got, tc.want)
			}
		})
	}

	t.Run("both columns of the components row", func(t *testing.T) {
		got := parseCypherRow("\nversion, edition\n\"2025.10.1\", \"enterprise\"\n", neo4jVersionColumn, neo4jEditionColumn)
		if got[neo4jVersionColumn] != "2025.10.1" || got[neo4jEditionColumn] != "enterprise" {
			t.Errorf("parseCypherRow() = %v, want the version and the edition", got)
		}
	})

	t.Run("the components row read by name, not by position", func(t *testing.T) {
		// The edition read as the version would be compared against the
		// utility's version by FR-006 and matched against "enterprise" by the
		// FR-008 gate. Both questions get the wrong field from a positional
		// read of a server that returned the columns the other way round.
		got := parseCypherRow("edition, version\n\"enterprise\", \"2025.10.1\"\n", neo4jVersionColumn, neo4jEditionColumn)
		if got[neo4jVersionColumn] != "2025.10.1" || got[neo4jEditionColumn] != "enterprise" {
			t.Errorf("parseCypherRow() = %v, want each column read under its own name", got)
		}
	})

	t.Run("a notice before the components header", func(t *testing.T) {
		got := parseCypherRow("Notice: deprecated procedure\nversion, edition\n\"2025.10.1\", \"enterprise\"\n", neo4jVersionColumn, neo4jEditionColumn)
		if got[neo4jVersionColumn] != "2025.10.1" || got[neo4jEditionColumn] != "enterprise" {
			t.Errorf("parseCypherRow() = %v, want the row after the header, not the header", got)
		}
	})

	// The statement and its parser must ask for the same column names: a
	// renamed alias in one and not the other finds no header, and "the server
	// answered nothing" is a wrong answer that reads like a server problem.
	t.Run("the statements ask for the columns the parsers look for", func(t *testing.T) {
		if !strings.Contains(neo4jComponentsStatement, "AS "+neo4jVersionColumn) {
			t.Errorf("neo4jComponentsStatement = %q, want it to alias the version as %q", neo4jComponentsStatement, neo4jVersionColumn)
		}
		if !strings.Contains(neo4jStoreSizeStatement("neo4j"), "RETURN "+neo4jStoreSizeColumn) {
			t.Errorf("neo4jStoreSizeStatement() = %q, want it to return %q", neo4jStoreSizeStatement("neo4j"), neo4jStoreSizeColumn)
		}
	})
}

func TestParseStoreSizeValue(t *testing.T) {
	t.Run("a byte count", func(t *testing.T) {
		size, err := parseStoreSizeValue("1073741824")
		if err != nil || size != 1073741824 {
			t.Errorf("parseStoreSizeValue() = %d, %v, want 1073741824", size, err)
		}
	})

	// FR-022's closing clause: a size that cannot be determined must reach
	// resolveScratchSize as zero so the run fails naming the override, rather
	// than as a guess.
	for _, text := range []string{"", "   ", "null", "NULL", "not-a-number", "0", "-1"} {
		t.Run(fmt.Sprintf("%q is not a size", text), func(t *testing.T) {
			if _, err := parseStoreSizeValue(text); err == nil {
				t.Errorf("parseStoreSizeValue(%q) = nil error, want the size reported as undetermined", text)
			}
		})
	}
}

func TestExtractVersionToken(t *testing.T) {
	cases := map[string]string{
		"pg_dump (PostgreSQL) 18.1":       "18.1",
		"neo4j-admin 2025.10.1":           "2025.10.1",
		"5.26.1":                          "5.26.1",
		"18.1 (Debian 18.1-1.pgdg120+1)":  "18.1",
		"\nneo4j-admin, version 2025.1.0": "2025.1.0",
	}

	for output, want := range cases {
		t.Run(output, func(t *testing.T) {
			got, err := extractVersionToken(output)
			if err != nil || got != want {
				t.Errorf("extractVersionToken(%q) = %q, %v, want %q", output, got, err, want)
			}
		})
	}

	if _, err := extractVersionToken("command not found"); err == nil {
		t.Error("extractVersionToken() = nil error for output with no version, want a failure")
	}
}

// TestParseNeo4jMemberRoles reads roles from the header rather than by position,
// so a server returning the columns in another order cannot make every member's
// role its address.
func TestParseNeo4jMemberRoles(t *testing.T) {
	t.Run("the documented column order", func(t *testing.T) {
		members := parseNeo4jMemberRoles("address, role, writer\n\"db-a:7687\", \"primary\", true\n\"db-b:7687\", \"secondary\", false\n")
		if len(members) != 2 {
			t.Fatalf("members = %+v, want two", members)
		}
		if members[0].Address != "db-a:7687" || members[0].Role != "primary" || !members[0].Writer {
			t.Errorf("members[0] = %+v, want the primary writer", members[0])
		}
		if members[1].Writer {
			t.Errorf("members[1] = %+v, want writer false", members[1])
		}
	})

	t.Run("a server that returns the columns in another order", func(t *testing.T) {
		members := parseNeo4jMemberRoles("role, writer, address\n\"primary\", true, \"db-a:7687\"\n")
		if len(members) != 1 || members[0].Address != "db-a:7687" || members[0].Role != "primary" {
			t.Errorf("members = %+v, want the columns read from the header", members)
		}
	})

	t.Run("a role the server did not report becomes a value, not an absence", func(t *testing.T) {
		members := parseNeo4jMemberRoles("address, role, writer\n\"db-a:7687\", null, false\n")
		if len(members) != 1 || members[0].Role != roleUnknown {
			t.Errorf("members = %+v, want the role recorded as %q", members, roleUnknown)
		}
	})

	// The header side of the case question, which this parser has always got
	// right: the header is normalised as it is indexed. The lookup side — the
	// spelling the *caller* asks with — is the one that was fragile here, and
	// it is pinned on the shared helper instead, because no member column is
	// spelled with a capital today and a test of this parser therefore cannot
	// reach it. See TestCypherColumnLookupIsIndifferentToCase.
	t.Run("a server that spells a column with a capital", func(t *testing.T) {
		members := parseNeo4jMemberRoles("Address, Role, Writer\n\"db-a:7687\", \"primary\", true\n")
		if len(members) != 1 || members[0].Address != "db-a:7687" || members[0].Role != "primary" || !members[0].Writer {
			t.Errorf("members = %+v, want the columns matched regardless of case: a role that silently vanishes is worse than a run that fails (FR-005)", members)
		}
	})

	t.Run("output with no rows", func(t *testing.T) {
		if members := parseNeo4jMemberRoles("address, role, writer\n"); members != nil {
			t.Errorf("members = %+v, want none", members)
		}
	})

	// The sibling of the T116 defect: taking the first non-empty line as the
	// header put a notice there, found neither column in it, and reported that
	// a cluster hosts no members — which the caller turns into a failed run for
	// a server that answered perfectly well.
	t.Run("a notice before the header", func(t *testing.T) {
		members := parseNeo4jMemberRoles("Notice: SHOW DATABASE output changes in a future release\naddress, role, writer\n\"db-a:7687\", \"primary\", true\n")
		if len(members) != 1 || members[0].Address != "db-a:7687" || members[0].Role != "primary" {
			t.Errorf("members = %+v, want the row after the header the notice preceded", members)
		}
	})
}

// TestCaptureRolesRecordsEveryEndpointAndClaimsNoServer is FR-005. Every
// endpoint supplied appears, matched on the host because the supplied Neo4j
// endpoint carries the backup port while the server reports its client address;
// and no key or value says which endpoint served the capture, because nothing
// observed it.
func TestCaptureRolesRecordsEveryEndpointAndClaimsNoServer(t *testing.T) {
	targets := []HostPort{{Host: "db-a", Port: 6362}, {Host: "db-b", Port: 6362}, {Host: "db-c", Port: 6362}}
	members := []observedMember{
		{Address: "db-a:7687", Role: "primary", Writer: true},
		{Address: "db-b:7687", Role: "secondary"},
	}

	roles := captureRoles(targets, members)
	if len(roles) != 3 {
		t.Fatalf("roles = %v, want one entry per endpoint supplied", roles)
	}
	if roles["db-a:6362"] != "primary" || roles["db-b:6362"] != "secondary" {
		t.Errorf("roles = %v, want the observed roles matched on host", roles)
	}
	if roles["db-c:6362"] != roleUnknown {
		t.Errorf("roles[db-c:6362] = %q, want %q as a value rather than an absent key", roles["db-c:6362"], roleUnknown)
	}

	// Nothing in the rendering asserts a server. The line an operator reads says
	// so explicitly, and this pins that no role value is a claim of one.
	rendered := describeCaptureRoles(targets, roles)
	for _, forbidden := range []string{"served", "captured from", "source member"} {
		if strings.Contains(rendered, forbidden) {
			t.Errorf("describeCaptureRoles() = %q, want no claim about which member served the capture", rendered)
		}
	}
	if rendered != "db-a:6362=primary,db-b:6362=secondary,db-c:6362=unknown" {
		t.Errorf("describeCaptureRoles() = %q, want the endpoints in try order", rendered)
	}
}

// TestCaptureRolesWithTwoInstancesOnOneHost is T114. Keying members by host
// alone let two instances at one address collapse into whichever the server
// listed last, so a supplied endpoint could be recorded as `secondary` on a run
// that had read a primary there — the field FR-005 says bears on the recovery
// point, wrong in the artefact's own provenance.
func TestCaptureRolesWithTwoInstancesOnOneHost(t *testing.T) {
	t.Run("instances in different roles at one host record no role", func(t *testing.T) {
		targets := []HostPort{{Host: "db-a", Port: 6362}}
		members := []observedMember{
			{Address: "db-a:7687", Role: "primary", Writer: true},
			{Address: "db-a:7688", Role: "secondary"},
		}

		if roles := captureRoles(targets, members); roles["db-a:6362"] != roleUnknown {
			t.Errorf("roles = %v, want %q: the run cannot tell which instance at db-a it named", roles, roleUnknown)
		}
	})

	t.Run("the answer does not depend on the order the server listed them", func(t *testing.T) {
		// A role the server did not report is carried as roleUnknown, which is
		// also the "nothing chosen yet" value a naive fold would start from —
		// so a null before a primary recorded `primary` and the same pair
		// reversed recorded `unknown`.
		targets := []HostPort{{Host: "db-a", Port: 6362}}
		forwards := captureRoles(targets, []observedMember{
			{Address: "db-a:7687", Role: roleUnknown},
			{Address: "db-a:7688", Role: "primary"},
		})
		backwards := captureRoles(targets, []observedMember{
			{Address: "db-a:7688", Role: "primary"},
			{Address: "db-a:7687", Role: roleUnknown},
		})

		if forwards["db-a:6362"] != backwards["db-a:6362"] {
			t.Errorf("roles = %v and %v for the same two instances, want one answer whichever order they were listed in", forwards, backwards)
		}
		if forwards["db-a:6362"] != roleUnknown {
			t.Errorf("roles = %v, want %q where the two instances disagree", forwards, roleUnknown)
		}
	})

	t.Run("instances that agree still record the role", func(t *testing.T) {
		// Nothing is lost by collapsing them here: whichever of the two the
		// endpoint was, the role is the same.
		targets := []HostPort{{Host: "db-a", Port: 6362}}
		members := []observedMember{
			{Address: "db-a:7687", Role: "secondary"},
			{Address: "db-a:7688", Role: "secondary"},
		}

		if roles := captureRoles(targets, members); roles["db-a:6362"] != "secondary" {
			t.Errorf("roles = %v, want secondary: both instances at db-a are in that role", roles)
		}
	})

	t.Run("a whole-address match beats the host fallback", func(t *testing.T) {
		// Where the supplied endpoint is the address the server reports — a
		// PostgreSQL endpoint, or an operator-supplied Neo4j client address —
		// the run knows which instance it named and records that one's role.
		targets := []HostPort{{Host: "db-a", Port: 7688}}
		members := []observedMember{
			{Address: "db-a:7687", Role: "primary", Writer: true},
			{Address: "db-a:7688", Role: "secondary"},
		}

		if roles := captureRoles(targets, members); roles["db-a:7688"] != "secondary" {
			t.Errorf("roles = %v, want the role of the instance at the address supplied", roles)
		}
	})
}

// ---------------------------------------------------------------------------
// T039: the PostgreSQL dump goes to the member that answered
// ---------------------------------------------------------------------------

func TestExternalPostgresDumpRequestUsesTheMemberThatAnswered(t *testing.T) {
	capture := &externalCapture{externalSource: &externalSource{
		Endpoint: &DatabaseEndpoint{
			Service:  serviceTaskManagerDB,
			Database: "prefect",
			Hosts:    []HostPort{{Host: "pg-a", Port: 5432}, {Host: "pg-b", Port: 5432}},
		},
		// The probe walked past pg-a and got its answer from pg-b. Dumping from
		// Hosts[0] would point the dump at the member just watched to fail.
		Facts: probedFacts{Answered: HostPort{Host: "pg-b", Port: 5432}},
	}}

	got := postgresDumpCommand(externalPostgresDumpRequest(capture, externalPostgresDumpFile, false))
	want := []string{"pg_dump", "-Fc", "-h", "pg-b", "-p", "5432", "-d", "prefect", "-f", externalPostgresDumpFile}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("postgresDumpCommand() = %q\nwant %q", got, want)
	}
}

// TestInternalPostgresDumpArgvIsUnchanged is the FR-015 half: the in-container
// dump still connects to localhost with -U and no -p, exactly as before.
func TestInternalPostgresDumpArgvIsUnchanged(t *testing.T) {
	cfg := &Configuration{PostgresUsername: "postgres", PostgresDatabase: "prefect"}

	cases := []struct {
		name string
		req  postgresDumpRequest
		want []string
	}{
		{
			name: "the streaming dump",
			req:  internalPostgresDumpRequest(cfg, "", true),
			want: []string{"pg_dump", "-Fc", "-Z0", "-h", "localhost", "-U", "postgres", "-d", "prefect"},
		},
		{
			name: "the file dump",
			req:  internalPostgresDumpRequest(cfg, "/tmp/infrahubops_prefect.dump", false),
			want: []string{"pg_dump", "-Fc", "-h", "localhost", "-U", "postgres", "-d", "prefect", "-f", "/tmp/infrahubops_prefect.dump"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := postgresDumpCommand(tc.req)
			if strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") {
				t.Errorf("postgresDumpCommand() = %q\nwant %q", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// T040 / FR-014: no credential on any command line
// ---------------------------------------------------------------------------

// TestNoCredentialReachesAnExternalArgv builds every command the external path
// constructs, with credentials chosen to be unmistakable, and asserts none of
// them appears anywhere in the argv or in the exec environment.
//
// The exec environment is included because it is not a separate channel:
// prepareCommand turns ExecOptions.Env into an `env KEY=VALUE …` prefix on the
// command line, so a password placed there is a password on the command line —
// which is exactly how the in-container path passes PGPASSWORD today, and
// exactly what may not follow the dump to a server outside the deployment.
func TestNoCredentialReachesAnExternalArgv(t *testing.T) {
	const (
		neo4jPassword    = "s3cr3t-neo4j-passphrase"
		neo4jUser        = "neo4j-service-account"
		postgresPassword = "s3cr3t-postgres-passphrase"
		postgresUser     = "postgres-service-account"
	)

	cfg := &Configuration{
		Neo4jUsername:    neo4jUser,
		Neo4jPassword:    neo4jPassword,
		Neo4jDatabase:    "neo4j",
		PostgresUsername: postgresUser,
		PostgresPassword: postgresPassword,
		PostgresDatabase: "prefect",
	}

	neo4jEndpoint := &DatabaseEndpoint{
		Service: serviceNeo4j, Database: cfg.Neo4jDatabase, BackupPort: 6362,
		Hosts: []HostPort{{Host: "db-a", Port: 7687}},
	}
	postgresEndpoint := &DatabaseEndpoint{
		Service: serviceTaskManagerDB, Database: cfg.PostgresDatabase,
		Hosts: []HostPort{{Host: "pg-a", Port: 5432}},
	}

	tls := resolveExternalTLS(serviceNeo4j, cfg.ExternalDB, ExternalDBTLS{Enabled: true})
	pgTLS := resolveExternalTLS(serviceTaskManagerDB, cfg.ExternalDB, ExternalDBTLS{})

	neo4jCapture := &externalCapture{externalSource: &externalSource{Endpoint: neo4jEndpoint, TLS: tls}, Pod: "infrahub-transient-capture-abcd1234"}
	postgresCapture := &externalCapture{externalSource: &externalSource{
		Endpoint: postgresEndpoint, TLS: pgTLS,
		Facts: probedFacts{Answered: HostPort{Host: "pg-a", Port: 5432}},
	}, Pod: "infrahub-transient-capture-abcd1234"}

	uri := neo4jEndpoint.boltURIs(tls)[0]

	storeSize, err := neo4jStoreSizeCommand(uri, cfg.Neo4jDatabase)
	if err != nil {
		t.Fatalf("neo4jStoreSizeCommand failed: %v", err)
	}
	memberRoles, err := neo4jMemberRolesCommand(uri, cfg.Neo4jDatabase)
	if err != nil {
		t.Fatalf("neo4jMemberRolesCommand failed: %v", err)
	}

	commands := map[string][]string{
		"the Neo4j capture":          neo4jCaptureCommand(externalNeo4jCaptureRequest(neo4jCapture, "all", true)),
		"the Neo4j components probe": neo4jComponentsCommand(uri),
		"the Neo4j utility version":  neo4jUtilityVersionCommand(),
		"the Neo4j store size":       storeSize,
		"the Neo4j member roles":     memberRoles,
		"the PostgreSQL dump":        postgresDumpCommand(externalPostgresDumpRequest(postgresCapture, externalPostgresDumpFile, false)),
		"the PostgreSQL stream":      postgresDumpCommand(externalPostgresDumpRequest(postgresCapture, "", true)),
		"the PostgreSQL version":     postgresServerVersionCommand(postgresEndpoint.Hosts[0], cfg.PostgresDatabase),
		"the PostgreSQL size":        postgresStoreSizeCommand(postgresEndpoint.Hosts[0], cfg.PostgresDatabase),
		"the PostgreSQL utility":     postgresUtilityVersionCommand(),
	}

	secrets := []string{neo4jPassword, postgresPassword, neo4jUser, postgresUser}

	for name, command := range commands {
		t.Run(name, func(t *testing.T) {
			joined := strings.Join(command, " ")
			for _, secret := range secrets {
				if strings.Contains(joined, secret) {
					t.Errorf("%s carries a credential on its command line: %q", name, joined)
				}
			}
		})
	}

	// The exec environment is the other half of the same command line. Only the
	// TLS mode belongs there; PGUSER and PGPASSWORD come from the secret the
	// transient workload owns.
	t.Run("the exec environment carries the TLS settings and nothing else", func(t *testing.T) {
		opts := postgresCapture.execOptions(postgresCapture.ClientEnv)
		if opts.Pod != postgresCapture.Pod {
			t.Errorf("opts.Pod = %q, want the capture's workload named rather than the service resolved", opts.Pod)
		}
		for key := range opts.Env {
			if key != "PGSSLMODE" && key != "PGSSLROOTCERT" {
				t.Errorf("opts.Env carries %s, want the TLS mode and its trust anchor alone", key)
			}
		}
		for key, value := range opts.Env {
			for _, secret := range secrets {
				if strings.Contains(value, secret) {
					t.Errorf("opts.Env[%s] carries a credential", key)
				}
			}
		}
	})

	// The credentials do reach the workload — through the secret it owns, which
	// is what makes their absence above a relocation rather than a breakage
	// (research R7).
	t.Run("the credentials reach the workload through its secret", func(t *testing.T) {
		for service, want := range map[string][]string{
			serviceNeo4j:         {"NEO4J_USERNAME", "NEO4J_PASSWORD"},
			serviceTaskManagerDB: {"PGUSER", "PGPASSWORD"},
		} {
			credentials, err := transientCredentials(cfg, service)
			if err != nil {
				t.Fatalf("transientCredentials(%s) failed: %v", service, err)
			}
			for _, key := range want {
				if credentials[key] == "" {
					t.Errorf("transientCredentials(%s) has no %s", service, key)
				}
			}
		}
	})
}

// ---------------------------------------------------------------------------
// T041 / FR-029: certificate verification
// ---------------------------------------------------------------------------

func TestResolveExternalTLS(t *testing.T) {
	t.Run("verification is the default for an encrypted channel", func(t *testing.T) {
		decision := resolveExternalTLS(serviceNeo4j, ExternalDBConfig{}, ExternalDBTLS{Enabled: true})
		if !decision.verify() || decision.OptedOut {
			t.Errorf("decision = %+v, want verification on and no opt-out", decision)
		}
		if got := decision.boltScheme("neo4j"); got != "neo4j+s" {
			t.Errorf("boltScheme() = %q, want the verifying scheme", got)
		}
	})

	// The requirement in one assertion: the deployment's own insecure setting is
	// about Infrahub's connections, and this tool sends credentials of its own.
	t.Run("a discovered insecure setting does not disable verification", func(t *testing.T) {
		decision := resolveExternalTLS(serviceNeo4j, ExternalDBConfig{}, ExternalDBTLS{Enabled: true, Insecure: true})
		if !decision.verify() {
			t.Errorf("decision = %+v, want verification still on: the deployment's setting was not made about this tool", decision)
		}
		if decision.OptedOut {
			t.Errorf("decision = %+v, want the discovered setting kept distinct from the operator's opt-out", decision)
		}
		if got := decision.boltScheme("neo4j"); got != "neo4j+s" {
			t.Errorf("boltScheme() = %q, want the verifying scheme", got)
		}
	})

	t.Run("only the operator's own flag disables it", func(t *testing.T) {
		decision := resolveExternalTLS(serviceNeo4j, ExternalDBConfig{InsecureTLS: true}, ExternalDBTLS{Enabled: true})
		if decision.verify() || !decision.OptedOut {
			t.Errorf("decision = %+v, want the opt-out honoured", decision)
		}
		if got := decision.boltScheme("neo4j"); got != "neo4j+ssc" {
			t.Errorf("boltScheme() = %q, want the self-signed-accepting scheme", got)
		}
	})

	t.Run("PostgreSQL negotiates TLS regardless, and the mode carries the decision", func(t *testing.T) {
		verifying := resolveExternalTLS(serviceTaskManagerDB, ExternalDBConfig{}, ExternalDBTLS{})
		if !verifying.Encrypt || !verifying.verify() {
			t.Errorf("decision = %+v, want an encrypted, verified channel", verifying)
		}
		if got := verifying.postgresSSLMode(); got != "verify-full" {
			t.Errorf("postgresSSLMode() = %q, want verify-full", got)
		}

		optedOut := resolveExternalTLS(serviceTaskManagerDB, ExternalDBConfig{InsecureTLS: true}, ExternalDBTLS{Insecure: true})
		if got := optedOut.postgresSSLMode(); got != "require" {
			t.Errorf("postgresSSLMode() = %q, want require", got)
		}
	})

	// T100: a deployment that leaves INFRAHUB_DB_TLS_ENABLED at Infrahub's own
	// default exports nothing, so "absent" is the common observation — and it
	// used to be read as licence to send this tool's database password out of
	// the cluster in clear, with no flag able to prevent it.
	t.Run("an unexported TLS setting does not turn encryption off", func(t *testing.T) {
		decision := resolveExternalTLS(serviceNeo4j, ExternalDBConfig{}, ExternalDBTLS{})
		if !decision.Encrypt || !decision.verify() {
			t.Errorf("decision = %+v, want an encrypted, verified channel by default", decision)
		}
		if got := decision.boltScheme("bolt"); got != "bolt+s" {
			t.Errorf("boltScheme() = %q, want the verifying scheme", got)
		}
	})

	// The escape from the default above, and the only one: the deployment says
	// the server does not speak TLS *and* the operator says to accept that.
	t.Run("an unencrypted channel takes the operator's opt-out as well", func(t *testing.T) {
		decision := resolveExternalTLS(serviceNeo4j, ExternalDBConfig{InsecureTLS: true}, ExternalDBTLS{Enabled: false})
		if decision.Encrypt || decision.verify() {
			t.Errorf("decision = %+v, want no encryption and no verification claim", decision)
		}
		if !decision.OptedOut {
			t.Errorf("decision = %+v, want the opt-out recorded", decision)
		}
		if got := decision.boltScheme("bolt"); got != "bolt" {
			t.Errorf("boltScheme() = %q, want the bare scheme", got)
		}
	})

	// The opt-out does not reach past what the deployment describes: where the
	// server does speak TLS, opting out gives up verification and keeps the
	// encryption.
	t.Run("the opt-out keeps an encrypted channel the deployment declares", func(t *testing.T) {
		decision := resolveExternalTLS(serviceNeo4j, ExternalDBConfig{InsecureTLS: true}, ExternalDBTLS{Enabled: true})
		if !decision.Encrypt || decision.verify() {
			t.Errorf("decision = %+v, want encryption kept and verification given up", decision)
		}
		if got := decision.boltScheme("bolt"); got != "bolt+ssc" {
			t.Errorf("boltScheme() = %q, want the self-signed-accepting scheme", got)
		}
	})
}

// TestSafeDatabaseIdentifier keeps a discovered name from closing a quote and
// continuing a statement.
func TestSafeDatabaseIdentifier(t *testing.T) {
	for _, name := range []string{"neo4j", "infrahub-main", "infrahub_main", "db.one", "n4"} {
		if _, err := safeDatabaseIdentifier(name); err != nil {
			t.Errorf("safeDatabaseIdentifier(%q) = %v, want it accepted", name, err)
		}
	}
	for _, name := range []string{"", "  ", `neo4j" YIELD x RETURN "`, "neo4j;DROP", "neo4j one", "neo4j`"} {
		if _, err := safeDatabaseIdentifier(name); err == nil {
			t.Errorf("safeDatabaseIdentifier(%q) = nil, want it refused", name)
		}
	}
}

// ---------------------------------------------------------------------------
// The preparation sequence (FR-006, FR-008, FR-022, FR-011)
// ---------------------------------------------------------------------------

// newExternalOps is a run whose Neo4j database lives outside the deployment at
// the supplied address.
func newExternalOps(t *testing.T, address string) (*InfrahubOps, *gatedBackend) {
	t.Helper()

	backend := newGatedBackend()
	backend.locations[serviceNeo4j] = EndpointLocationExternal

	cfg := createRetentionConfig(t.TempDir(), RetentionConfig{})
	cfg.Neo4jUsername = "neo4j"
	cfg.Neo4jPassword = "password"
	cfg.Neo4jDatabase = "neo4j"
	cfg.ExternalDB = ExternalDBConfig{Neo4jAddress: address, Neo4jBackupPort: defaultNeo4jBackupPort}

	return &InfrahubOps{config: cfg, backend: backend, executor: NewCommandExecutor()}, backend
}

// externalCaptureTarget is the target the gate hands prepareExternalSourceWith:
// a database this deployment does not host, about to be captured.
//
// It comes out of databaseTargetWith rather than being written as a literal,
// because a literal is no longer a target the preparation accepts: the gate's
// verdict is carried by an unexported field only that constructor sets, so a
// hand-built value is refused (see databaseTarget.requireGated). A test that
// built its own was exactly the shape the witness exists to stop.
func externalCaptureTarget(t *testing.T, service string) databaseTarget {
	t.Helper()

	target, err := databaseTargetWith(deploymentQueries{
		locate: func(string) (EndpointLocation, error) { return EndpointLocationExternal, nil },
	}, databaseCapture, service, ExternalRestoreAuth{})
	if err != nil {
		t.Fatalf("databaseTargetWith(capture, %s) = %v, want a target", service, err)
	}

	return target
}

// recordingCaptureOps is externalCaptureOps with every step recorded, so the
// order the sequence performs them in is assertable without a cluster.
type recordingCaptureOps struct {
	steps []string
	facts probedFacts
	err   error
}

func (r *recordingCaptureOps) ops() externalCaptureOps {
	return externalCaptureOps{
		startProbe: func(_ string, members int) (string, func(), error) {
			r.steps = append(r.steps, fmt.Sprintf("start-probe(members=%d)", members))

			return "probe-pod", func() { r.steps = append(r.steps, "release-probe") }, nil
		},
		startCapture: func(_ string, storeSizeBytes int64) (string, func(), error) {
			r.steps = append(r.steps, fmt.Sprintf("start-capture(size=%d)", storeSizeBytes))

			return "capture-pod", func() { r.steps = append(r.steps, "release-capture") }, nil
		},
		probe: func(pod string, _ *DatabaseEndpoint, _ externalTLSDecision) (probedFacts, error) {
			r.steps = append(r.steps, "probe("+pod+")")

			return r.facts, r.err
		},
	}
}

// TestPrepareExternalSourceWithSequencesTheGate pins the order the requirements
// force: probe first because nothing else can answer, then the edition refusal
// and the version gate on what it read, and the probe given back before the run
// goes on. No capture workload is created here at all — it is built later,
// around the capture itself, sized from what the probe reported.
func TestPrepareExternalSourceWithSequencesTheGate(t *testing.T) {
	t.Run("a healthy Enterprise server records what the probe read", func(t *testing.T) {
		iops, _ := newExternalOps(t, "db-a,db-b")
		recorder := &recordingCaptureOps{facts: probedFacts{
			ServerVersion:  "2025.10.1",
			UtilityVersion: "2025.10.1",
			Edition:        "enterprise",
			StoreSizeBytes: 4 << 30,
			Members:        []observedMember{{Address: "db-a:7687", Role: "primary", Writer: true}},
		}}

		if err := iops.prepareExternalSourceWith(recorder.ops(), externalCaptureTarget(t, serviceNeo4j)); err != nil {
			t.Fatalf("prepareExternalSourceWith() = %v, want the source prepared", err)
		}

		// The probe's deadline is budgeted from the number of endpoints it may
		// walk, so the member count has to reach it.
		want := []string{"start-probe(members=2)", "probe(probe-pod)", "release-probe"}
		if strings.Join(recorder.steps, ",") != strings.Join(want, ",") {
			t.Errorf("steps = %v\nwant %v", recorder.steps, want)
		}

		source := iops.externalSourceFor(serviceNeo4j)
		if source == nil {
			t.Fatal("externalSourceFor() = nil, want the prepared source")
		}
		if source.Facts.StoreSizeBytes != 4<<30 {
			t.Errorf("StoreSizeBytes = %d, want the size the probe read (FR-022)", source.Facts.StoreSizeBytes)
		}
		if got := renderHostPorts(source.captureTargets()); got != "db-a:6362,db-b:6362" {
			t.Errorf("captureTargets() = %q, want both members on the backup port in order", got)
		}
		if roles := source.observedRoles(); roles["db-a:6362"] != "primary" || roles["db-b:6362"] != roleUnknown {
			t.Errorf("observedRoles() = %v, want the observed role and %q for the member none matched", roles, roleUnknown)
		}
	})

	t.Run("a Community server is refused before the version is even compared", func(t *testing.T) {
		iops, backend := newExternalOps(t, "db-a")
		// A utility older than the server, so a run that reached the version gate
		// would fail there instead — which is how this test tells the two apart.
		recorder := &recordingCaptureOps{facts: probedFacts{
			ServerVersion: "2025.10.1", UtilityVersion: "5.26.1", Edition: "community",
		}}

		err := iops.prepareExternalSourceWith(recorder.ops(), externalCaptureTarget(t, serviceNeo4j))
		if !errors.Is(err, errExternalCommunityCapture) {
			t.Fatalf("err = %v, want the FR-008 refusal rather than a version failure", err)
		}
		if iops.externalSourceFor(serviceNeo4j) != nil {
			t.Error("a source was recorded for a database this run refused to capture")
		}
		// Principle II: the refusal costs a probe pod and nothing else.
		if !contains(recorder.steps, "release-probe") {
			t.Errorf("steps = %v, want the probe given back on the refusal path (FR-011)", recorder.steps)
		}
		if len(backend.stopped) > 0 {
			t.Errorf("services stopped = %v, want none before the refusal", backend.stopped)
		}
	})

	t.Run("a utility older than the server aborts before any data moves", func(t *testing.T) {
		iops, _ := newExternalOps(t, "db-a")
		recorder := &recordingCaptureOps{facts: probedFacts{
			ServerVersion: "2025.10.1", UtilityVersion: "5.26.1", Edition: "enterprise",
		}}

		err := iops.prepareExternalSourceWith(recorder.ops(), externalCaptureTarget(t, serviceNeo4j))
		if err == nil {
			t.Fatal("prepareExternalSourceWith() = nil, want the FR-006 abort")
		}
		if !strings.Contains(err.Error(), versionImageFlag(serviceNeo4j)) {
			t.Errorf("err = %v, want it to name the flag that fixes it", err)
		}
		if contains(recorder.steps, "start-capture(size=0)") {
			t.Error("a capture workload was created for a run the version gate aborted")
		}
	})

	t.Run("a probe that failed gives the workload back and reports the endpoint", func(t *testing.T) {
		iops, _ := newExternalOps(t, "db-a")
		recorder := &recordingCaptureOps{err: errors.New("connection refused")}

		err := iops.prepareExternalSourceWith(recorder.ops(), externalCaptureTarget(t, serviceNeo4j))
		if err == nil {
			t.Fatal("prepareExternalSourceWith() = nil, want the probe failure reported")
		}
		if !strings.Contains(err.Error(), "db-a") {
			t.Errorf("err = %v, want it to name the endpoint it could not read", err)
		}
		if !contains(recorder.steps, "release-probe") {
			t.Errorf("steps = %v, want the probe given back on the failure path (FR-011)", recorder.steps)
		}
	})
}

// TestOpenExternalCaptureSizesTheWorkloadFromTheProbe is FR-022's other half: a
// pod's storage is fixed when it is created, so the capture workload is the one
// built from the store size, and it is built only when the capture runs.
func TestOpenExternalCaptureSizesTheWorkloadFromTheProbe(t *testing.T) {
	iops, _ := newExternalOps(t, "db-a")
	iops.recordExternalSource(serviceNeo4j, &externalSource{
		Endpoint: &DatabaseEndpoint{Service: serviceNeo4j, Database: "neo4j", BackupPort: 6362, Hosts: []HostPort{{Host: "db-a", Port: 7687}}},
		Facts:    probedFacts{StoreSizeBytes: 8 << 30},
	})

	recorder := &recordingCaptureOps{}
	pod, release, err := recorder.ops().startCapture(serviceNeo4j, iops.externalSourceFor(serviceNeo4j).Facts.StoreSizeBytes)
	if err != nil {
		t.Fatalf("startCapture failed: %v", err)
	}
	release()

	if pod != "capture-pod" {
		t.Errorf("pod = %q, want the capture workload named", pod)
	}
	if !contains(recorder.steps, fmt.Sprintf("start-capture(size=%d)", int64(8<<30))) {
		t.Errorf("steps = %v, want the workload sized from the store size the probe read", recorder.steps)
	}
}

// TestExternalSourceForIsNilForAnInternalDatabase is the FR-015 branch every
// capture path takes. Nothing was prepared, so nothing external happens.
func TestExternalSourceForIsNilForAnInternalDatabase(t *testing.T) {
	iops := NewInfrahubOps()

	if iops.externalSourceFor(serviceNeo4j) != nil || iops.externalSourceFor(serviceTaskManagerDB) != nil {
		t.Error("externalSourceFor() = non-nil on a run that prepared nothing, want the internal path")
	}
}

// TestExternalRunIDIsMintedOncePerRun is the invariant two external databases
// broke. A per-service identifier labels one run's two workloads as two runs,
// which is what the reaper and the create-collision refusal both read to decide
// whose objects they are looking at.
func TestExternalRunIDIsMintedOncePerRun(t *testing.T) {
	iops := NewInfrahubOps()

	first, err := iops.externalRunID()
	if err != nil {
		t.Fatalf("externalRunID() failed: %v", err)
	}
	second, err := iops.externalRunID()
	if err != nil {
		t.Fatalf("externalRunID() failed: %v", err)
	}

	if first == "" {
		t.Fatal("externalRunID() = empty")
	}
	if first != second {
		t.Errorf("externalRunID() = %q then %q, want one identifier for the whole run", first, second)
	}

	// A different run gets a different one, or the reaper could not tell an
	// earlier run's litter from this run's live workloads.
	other, err := NewInfrahubOps().externalRunID()
	if err != nil {
		t.Fatalf("externalRunID() failed: %v", err)
	}
	if other == first {
		t.Errorf("two runs minted the same identifier %q", first)
	}
}

// scriptedProbeBackend answers each bounded exec from a script keyed on the
// first word of the command, recording every argv it was handed. It is what
// lets the probe walk and the CA install be driven without a cluster.
type scriptedProbeBackend struct {
	unboundedBackend

	// answers maps a command's first token to the outputs it returns, in call
	// order; the last one is repeated once exhausted.
	answers map[string][]string
	// fails maps a command's first token to an error it returns instead.
	fails map[string]error

	calls [][]string
	envs  []map[string]string
}

func (b *scriptedProbeBackend) ExecSeparateContext(_ context.Context, _ time.Duration, _ string, command []string, opts *ExecOptions) (string, string, error) {
	b.calls = append(b.calls, command)
	if opts != nil {
		b.envs = append(b.envs, opts.Env)
	} else {
		b.envs = append(b.envs, nil)
	}

	key := ""
	if len(command) > 0 {
		key = command[0]
	}
	if err, ok := b.fails[key]; ok {
		return "", "", err
	}

	outputs := b.answers[key]
	switch {
	case len(outputs) == 0:
		return "", "", nil
	case len(outputs) == 1:
		return outputs[0], "", nil
	default:
		b.answers[key] = outputs[1:]

		return outputs[0], "", nil
	}
}

func (b *scriptedProbeBackend) argvFor(token string) [][]string {
	matched := [][]string{}
	for _, call := range b.calls {
		if len(call) > 0 && call[0] == token {
			matched = append(matched, call)
		}
	}

	return matched
}

// errTestCommandMissing stands in for a command the workload's image does not
// carry, which is what every best-effort step on this path must degrade around.
var errTestCommandMissing = errors.New("exec: \"keytool\": executable file not found in $PATH")

// TestFirstAnsweringEndpointWalksPastAnEmptyAnswer is F7, and F16 before the
// rebuild. The Neo4j arm validated what a member returned and walked on; the
// PostgreSQL arm took whatever the exec produced and stopped — so a member that
// connected and answered nothing ended the walk, and the FR-006 version gate
// then aborted the run with a healthy member never tried.
func TestFirstAnsweringEndpointWalksPastAnEmptyAnswer(t *testing.T) {
	endpoints := []string{"first", "second", "third"}

	t.Run("a member that answers nothing does not end the walk", func(t *testing.T) {
		asked := []string{}
		accepted := ""
		index, err := firstAnsweringEndpoint(endpoints, "the server version", "the database",
			func(endpoint string) (string, error) {
				asked = append(asked, endpoint)
				if endpoint == "first" {
					return "   \n", nil
				}

				return "16.2", nil
			},
			func(endpoint, output string) error {
				if strings.TrimSpace(output) == "" {
					return fmt.Errorf("%s answered nothing when asked for its version", endpoint)
				}
				accepted = endpoint

				return nil
			},
		)
		if err != nil {
			t.Fatalf("firstAnsweringEndpoint failed: %v", err)
		}
		if index != 1 || accepted != "second" {
			t.Errorf("index = %d (accepted %q), want the second member", index, accepted)
		}
		if len(asked) != 2 {
			t.Errorf("asked = %v, want the walk to have continued past the empty answer", asked)
		}
	})

	t.Run("every member failing reports the last failure", func(t *testing.T) {
		_, err := firstAnsweringEndpoint(endpoints, "the server version", "the database",
			func(endpoint string) (string, error) { return "", fmt.Errorf("dial %s: refused", endpoint) },
			func(string, string) error { return nil },
		)
		if err == nil {
			t.Fatal("firstAnsweringEndpoint() = nil, want the walk reported as a failure")
		}
		if !strings.Contains(err.Error(), "failed to read the server version of the database") {
			t.Errorf("err = %v, want it to name what was being read and of what", err)
		}
		if !strings.Contains(err.Error(), "dial third") {
			t.Errorf("err = %v, want the last member's own failure kept", err)
		}
	})
}

// TestProbeExternalPostgresWalksPastASilentMember drives the walk through the
// PostgreSQL probe itself, which is where the regression lived: it is the arm
// that accepted whatever came back.
func TestProbeExternalPostgresWalksPastASilentMember(t *testing.T) {
	backend := &scriptedProbeBackend{answers: map[string][]string{
		"pg_dump": {"pg_dump (PostgreSQL) 16.2"},
		// The first member connects and reports nothing; the second answers.
		"psql": {"", "16.2", "1024"},
	}}
	iops := &InfrahubOps{config: &Configuration{}, backend: backend, executor: NewCommandExecutor()}

	endpoint := &DatabaseEndpoint{
		Service:  serviceTaskManagerDB,
		Location: EndpointLocationExternal,
		Hosts:    []HostPort{{Host: "silent.example", Port: 5432}, {Host: "healthy.example", Port: 5432}},
		Database: "prefect",
	}

	facts, err := iops.probeExternalPostgres("probe-pod", endpoint, externalTLSDecision{Service: serviceTaskManagerDB, Encrypt: true})
	if err != nil {
		t.Fatalf("probeExternalPostgres failed: %v", err)
	}
	if facts.ServerVersion != "16.2" {
		t.Errorf("ServerVersion = %q, want the version the second member reported", facts.ServerVersion)
	}
	if facts.Answered.Host != "healthy.example" {
		t.Errorf("Answered = %v, want the member that actually answered", facts.Answered)
	}
}

// TestExternalDatabaseForReadsTheLocation is F4. The capture paths branched
// on "did preparation record something?", so an entry point that never ran
// prepareDatabaseCapture left externalSources empty and silently took the
// in-deployment path against a database with no container.
//
// The accessor answers for either direction (T148): a restore's record carries
// the same probe, and the decisions that ask what the probe read are reached
// from both.
func TestExternalDatabaseForReadsTheLocation(t *testing.T) {
	t.Run("an internal database is nil and is not an error", func(t *testing.T) {
		iops := newGatedOps(t, newGatedBackend(), BackendTarball)
		source, err := iops.externalDatabaseFor(serviceNeo4j)
		if err != nil || source != nil {
			t.Errorf("externalDatabaseFor() = %v, %v; want nil, nil for a database inside the deployment (FR-015)", source, err)
		}
	})

	t.Run("a prepared external database returns what was established", func(t *testing.T) {
		backend := newGatedBackend()
		backend.locations[serviceNeo4j] = EndpointLocationExternal
		iops := newGatedOps(t, backend, BackendTarball)
		prepared := &externalSource{Endpoint: &DatabaseEndpoint{Service: serviceNeo4j}}
		iops.recordExternalSource(serviceNeo4j, prepared)

		source, err := iops.externalDatabaseFor(serviceNeo4j)
		if err != nil {
			t.Fatalf("externalDatabaseFor failed: %v", err)
		}
		if source != prepared {
			t.Errorf("source = %v, want the source the gate prepared", source)
		}
	})

	t.Run("a prepared external restore returns what its probe established", func(t *testing.T) {
		backend := newGatedBackend()
		backend.locations[serviceNeo4j] = EndpointLocationExternal
		iops := newGatedOps(t, backend, BackendTarball)
		probed := &externalSource{Endpoint: &DatabaseEndpoint{Service: serviceNeo4j}}
		iops.recordExternalRestore(serviceNeo4j, &externalRestore{externalSource: probed})

		source, err := iops.externalDatabaseFor(serviceNeo4j)
		if err != nil {
			t.Fatalf("externalDatabaseFor failed: %v", err)
		}
		if source != probed {
			t.Errorf("source = %v, want the source the restore's gate probed (T148)", source)
		}
	})

	t.Run("located external with nothing prepared is an error, never internal", func(t *testing.T) {
		backend := newGatedBackend()
		backend.locations[serviceNeo4j] = EndpointLocationExternal
		iops := newGatedOps(t, backend, BackendTarball)

		source, err := iops.externalDatabaseFor(serviceNeo4j)
		if err == nil {
			t.Fatalf("externalDatabaseFor() = %v, nil; want an error rather than the in-deployment path", source)
		}
		if source != nil {
			t.Errorf("source = %v, want none", source)
		}
		if !strings.Contains(err.Error(), "nothing was prepared") {
			t.Errorf("err = %v, want it to name what was not done", err)
		}
	})

	t.Run("a location that could not be established is reported", func(t *testing.T) {
		backend := newGatedBackend()
		backend.locateErr = errors.New("Unable to connect to the server")
		iops := newGatedOps(t, backend, BackendTarball)

		if _, err := iops.externalDatabaseFor(serviceNeo4j); err == nil {
			t.Error("externalDatabaseFor() = nil, want a cluster that did not answer reported rather than read as internal (FR-002)")
		}
	})
}

// TestPostgresClientEnvCarriesTrustMaterial is T110. `PGSSLMODE=verify-full`
// was set with no `PGSSLROOTCERT` beside it, so libpq looked for
// ~/.postgresql/root.crt in a workload that has no such file and refused the
// connection before reaching the server — a secure default that cannot be
// used, whose only escape drops verification altogether. The deployment's own
// `sslrootcert` was read into a log string and nowhere else.
func TestPostgresClientEnvCarriesTrustMaterial(t *testing.T) {
	verifying := externalTLSDecision{Service: serviceTaskManagerDB, Encrypt: true}

	t.Run("the deployment's authority is installed and named", func(t *testing.T) {
		backend := &scriptedProbeBackend{answers: map[string][]string{
			"cat": {"-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"},
		}}
		iops := &InfrahubOps{config: &Configuration{}, backend: backend, executor: NewCommandExecutor()}
		iops.config.ExternalDB.PostgresTLS.CAFile = "/etc/ssl/pg-ca.pem"

		env := iops.postgresClientEnv("capture-pod", verifying)

		if env["PGSSLMODE"] != "verify-full" {
			t.Errorf("PGSSLMODE = %q, want verify-full", env["PGSSLMODE"])
		}
		if env["PGSSLROOTCERT"] != externalPostgresCAPEMPath {
			t.Errorf("PGSSLROOTCERT = %q, want the PEM this run wrote into the workload", env["PGSSLROOTCERT"])
		}

		// Read from the container whose environment named the path, and written
		// into the pod the client will run in.
		if len(backend.argvFor("cat")) != 1 {
			t.Errorf("the deployment's CA was not read: calls = %v", backend.calls)
		}
		if len(backend.argvFor("sh")) != 1 {
			t.Errorf("the CA was not written into the workload: calls = %v", backend.calls)
		}
	})

	t.Run("a deployment naming no authority still verifies", func(t *testing.T) {
		backend := &scriptedProbeBackend{}
		iops := &InfrahubOps{config: &Configuration{}, backend: backend, executor: NewCommandExecutor()}

		env := iops.postgresClientEnv("capture-pod", verifying)

		if env["PGSSLMODE"] != "verify-full" {
			t.Errorf("PGSSLMODE = %q, want verify-full", env["PGSSLMODE"])
		}
		if env["PGSSLROOTCERT"] != postgresSystemTrustStore {
			t.Errorf("PGSSLROOTCERT = %q, want %q: verify-full with no anchor named is a mode nothing can satisfy",
				env["PGSSLROOTCERT"], postgresSystemTrustStore)
		}
		if len(backend.calls) != 0 {
			t.Errorf("calls = %v, want none: there is nothing to install", backend.calls)
		}
	})

	t.Run("an unreadable authority degrades to verifying, never to not verifying", func(t *testing.T) {
		backend := &scriptedProbeBackend{fails: map[string]error{"cat": errTestCommandMissing}}
		iops := &InfrahubOps{config: &Configuration{}, backend: backend, executor: NewCommandExecutor()}
		iops.config.ExternalDB.PostgresTLS.CAFile = "/etc/ssl/pg-ca.pem"

		env := iops.postgresClientEnv("capture-pod", verifying)

		if env["PGSSLMODE"] != "verify-full" {
			t.Errorf("PGSSLMODE = %q, want verify-full: a CA this run could not read is not an instruction to stop verifying", env["PGSSLMODE"])
		}
		if env["PGSSLROOTCERT"] != postgresSystemTrustStore {
			t.Errorf("PGSSLROOTCERT = %q, want %q", env["PGSSLROOTCERT"], postgresSystemTrustStore)
		}
	})

	t.Run("the opt-out installs nothing and names nothing", func(t *testing.T) {
		backend := &scriptedProbeBackend{}
		iops := &InfrahubOps{config: &Configuration{}, backend: backend, executor: NewCommandExecutor()}
		iops.config.ExternalDB.PostgresTLS.CAFile = "/etc/ssl/pg-ca.pem"

		env := iops.postgresClientEnv("capture-pod", externalTLSDecision{
			Service: serviceTaskManagerDB, Encrypt: true, OptedOut: true,
		})

		if env["PGSSLMODE"] != "require" {
			t.Errorf("PGSSLMODE = %q, want require", env["PGSSLMODE"])
		}
		if _, named := env["PGSSLROOTCERT"]; named {
			t.Errorf("PGSSLROOTCERT = %q, want none: nothing is verified, so an anchor would only mislead", env["PGSSLROOTCERT"])
		}
		if len(backend.calls) != 0 {
			t.Errorf("calls = %v, want none: a run that verifies nothing installs nothing", backend.calls)
		}
	})
}
