package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// storageQueryFixture is storageQuery output shaped as psql prints it with
// -A -t (research R5): one line of JSON from json_build_object, with " : "
// between keys and values. The figures follow the 1.7 GB database observed in
// research R5, where TOAST held most of flow_run.
const storageQueryFixture = `{"database" : "prefect", "database_bytes" : 1748424383, "stats_reset" : "2026-10-20T08:00:00.123456+00:00", "tables" : [` +
	`{"schema" : "public", "name" : "events", "table_bytes" : 211542016, "index_bytes" : 120053760, "toast_bytes" : 8192, "live_rows" : 412000, "dead_rows" : 1200, "last_vacuum" : null, "last_autovacuum" : "2026-10-25T23:10:05.5+00:00"}, ` +
	`{"schema" : "public", "name" : "alembic_version", "table_bytes" : 8192, "index_bytes" : 16384, "toast_bytes" : 0, "live_rows" : 1, "dead_rows" : 0, "last_vacuum" : null, "last_autovacuum" : null}, ` +
	`{"schema" : "public", "name" : "flow_run", "table_bytes" : 1294467072, "index_bytes" : 30474240, "toast_bytes" : 1241513984, "live_rows" : 120000, "dead_rows" : 30000, "last_vacuum" : "2026-10-24T12:00:00+02:00", "last_autovacuum" : null}` +
	"]}\n"

// readableSizePattern is the pattern contracts/storage.schema.json sets for
// every readable size.
var readableSizePattern = regexp.MustCompile(`^[0-9]+(\.[0-9])? (B|KB|MB|GB|TB|PB|EB)$`)

func stringPointer(s string) *string { return &s }

// allSizes returns every size of a report, named for error messages.
func allSizes(report storageReport) map[string]byteSize {
	sizes := map[string]byteSize{"database_size": report.DatabaseSize}
	for _, table := range report.Tables {
		sizes[table.Name+".total_size"] = table.TotalSize
		sizes[table.Name+".data_size"] = table.DataSize
		sizes[table.Name+".index_size"] = table.IndexSize
		sizes[table.Name+".toast_size"] = table.ToastSize
	}
	return sizes
}

func TestParseStorageReport(t *testing.T) {
	collectedAt := time.Date(2026, 10, 26, 19, 30, 0, 0, time.FixedZone("CET", 3600))

	got, err := parseStorageReport(storageQueryFixture, collectedAt)
	if err != nil {
		t.Fatalf("parseStorageReport failed: %v", err)
	}

	want := storageReport{
		CollectedAt:       "2026-10-26T18:30:00Z",
		Database:          "prefect",
		DatabaseSize:      byteSize{Bytes: 1748424383, Readable: "1.6 GB"},
		StatisticsResetAt: stringPointer("2026-10-20T08:00:00Z"),
		Tables: []tableStorage{
			{
				Schema:     "public",
				Name:       "flow_run",
				TotalSize:  byteSize{Bytes: 1324941312, Readable: "1.2 GB"},
				DataSize:   byteSize{Bytes: 52953088, Readable: "50.5 MB"},
				IndexSize:  byteSize{Bytes: 30474240, Readable: "29.1 MB"},
				ToastSize:  byteSize{Bytes: 1241513984, Readable: "1.2 GB"},
				LiveRows:   120000,
				DeadRows:   30000,
				LastVacuum: stringPointer("2026-10-24T10:00:00Z"),
			},
			{
				Schema:         "public",
				Name:           "events",
				TotalSize:      byteSize{Bytes: 331595776, Readable: "316.2 MB"},
				DataSize:       byteSize{Bytes: 211533824, Readable: "201.7 MB"},
				IndexSize:      byteSize{Bytes: 120053760, Readable: "114.5 MB"},
				ToastSize:      byteSize{Bytes: 8192, Readable: "8.0 KB"},
				LiveRows:       412000,
				DeadRows:       1200,
				LastAutovacuum: stringPointer("2026-10-25T23:10:05Z"),
			},
			{
				Schema:    "public",
				Name:      "alembic_version",
				TotalSize: byteSize{Bytes: 24576, Readable: "24.0 KB"},
				DataSize:  byteSize{Bytes: 8192, Readable: "8.0 KB"},
				IndexSize: byteSize{Bytes: 16384, Readable: "16.0 KB"},
				ToastSize: byteSize{Bytes: 0, Readable: "0 B"},
				LiveRows:  1,
			},
		},
	}
	if !reflect.DeepEqual(got, want) {
		gotJSON, _ := json.MarshalIndent(got, "", "  ")
		wantJSON, _ := json.MarshalIndent(want, "", "  ")
		t.Errorf("parseStorageReport() =\n%s\nwant\n%s", gotJSON, wantJSON)
	}
}

// TestParseStorageReport_SizesAddUp covers the size rules of FR-002 and FR-003:
// data, indexes and TOAST add up to the total for every table, including a
// TOAST table that grew past the table size between the two size calls, and
// every readable size is the one formatBytes writes.
func TestParseStorageReport_SizesAddUp(t *testing.T) {
	stdout := strings.Replace(storageQueryFixture, "]}", `, {"schema" : "public", "name" : "log", "table_bytes" : 40960, "index_bytes" : 8192, "toast_bytes" : 49152, "live_rows" : 3, "dead_rows" : 0, "last_vacuum" : null, "last_autovacuum" : null}]}`, 1)

	report, err := parseStorageReport(stdout, time.Now())
	if err != nil {
		t.Fatalf("parseStorageReport failed: %v", err)
	}
	if len(report.Tables) != 4 {
		t.Fatalf("report has %d tables, want 4", len(report.Tables))
	}

	for _, table := range report.Tables {
		if sum := table.DataSize.Bytes + table.IndexSize.Bytes + table.ToastSize.Bytes; sum != table.TotalSize.Bytes {
			t.Errorf("%s: data %d + index %d + TOAST %d = %d, want the total %d", table.Name, table.DataSize.Bytes, table.IndexSize.Bytes, table.ToastSize.Bytes, sum, table.TotalSize.Bytes)
		}
		if table.DataSize.Bytes < 0 {
			t.Errorf("%s: data size %d is negative", table.Name, table.DataSize.Bytes)
		}
	}
	for name, size := range allSizes(report) {
		if size.Readable != formatBytes(size.Bytes) {
			t.Errorf("%s readable = %q, want formatBytes(%d) = %q", name, size.Readable, size.Bytes, formatBytes(size.Bytes))
		}
		if !readableSizePattern.MatchString(size.Readable) {
			t.Errorf("%s readable = %q, does not match the contract pattern %s", name, size.Readable, readableSizePattern)
		}
	}
}

// TestParseStorageReport_Nulls covers the values PostgreSQL may not have: a
// table never vacuumed, statistics never reset, and a database without tables,
// for which json_agg returns null. The file keeps every field, with null or an
// empty list.
func TestParseStorageReport_Nulls(t *testing.T) {
	tests := []struct {
		name       string
		stdout     string
		wantTables int
	}{
		{
			name:       "no vacuum and no statistics reset",
			stdout:     `{"database" : "prefect", "database_bytes" : 8000000, "stats_reset" : null, "tables" : [{"schema" : "public", "name" : "events", "table_bytes" : 8192, "index_bytes" : 8192, "toast_bytes" : 0, "live_rows" : 0, "dead_rows" : 0, "last_vacuum" : null, "last_autovacuum" : null}]}`,
			wantTables: 1,
		},
		{
			name:   "no tables",
			stdout: `{"database" : "prefect", "database_bytes" : 7500000, "stats_reset" : null, "tables" : null}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report, err := parseStorageReport(tt.stdout, time.Now())
			if err != nil {
				t.Fatalf("parseStorageReport failed: %v", err)
			}
			if report.StatisticsResetAt != nil {
				t.Errorf("statistics_reset_at = %q, want null", *report.StatisticsResetAt)
			}
			if report.Tables == nil || len(report.Tables) != tt.wantTables {
				t.Fatalf("tables = %#v, want %d entries and never null", report.Tables, tt.wantTables)
			}
			for _, table := range report.Tables {
				if table.LastVacuum != nil || table.LastAutovacuum != nil {
					t.Errorf("%s vacuum times = %v / %v, want null", table.Name, table.LastVacuum, table.LastAutovacuum)
				}
			}

			data, err := json.Marshal(report)
			if err != nil {
				t.Fatalf("marshal failed: %v", err)
			}
			wantFragments := []string{`"statistics_reset_at":null`}
			if tt.wantTables == 0 {
				wantFragments = append(wantFragments, `"tables":[]`)
			} else {
				wantFragments = append(wantFragments, `"last_vacuum":null`, `"last_autovacuum":null`)
			}
			for _, fragment := range wantFragments {
				if !strings.Contains(string(data), fragment) {
					t.Errorf("storage.json %s does not hold %s", data, fragment)
				}
			}
		})
	}
}

// TestParseStorageReport_EqualSizesOrder checks that tables of equal total size
// are ordered by schema, then name, whatever order PostgreSQL returns them in.
func TestParseStorageReport_EqualSizesOrder(t *testing.T) {
	table := func(schema, name string, tableBytes int64) string {
		return fmt.Sprintf(`{"schema" : %q, "name" : %q, "table_bytes" : %d, "index_bytes" : 8192, "toast_bytes" : 0, "live_rows" : 0, "dead_rows" : 0, "last_vacuum" : null, "last_autovacuum" : null}`, schema, name, tableBytes)
	}
	entries := []string{
		table("public", "work_queue", 8192),
		table("public", "block_type", 8192),
		table("audit", "work_queue", 8192),
		table("public", "events", 81920),
	}
	want := []string{"public.events", "audit.work_queue", "public.block_type", "public.work_queue"}

	for _, order := range [][]int{{0, 1, 2, 3}, {3, 2, 1, 0}, {1, 3, 0, 2}, {2, 0, 3, 1}} {
		shuffled := make([]string, len(order))
		for i, index := range order {
			shuffled[i] = entries[index]
		}
		stdout := `{"database" : "prefect", "database_bytes" : 8000000, "stats_reset" : null, "tables" : [` + strings.Join(shuffled, ", ") + `]}`

		report, err := parseStorageReport(stdout, time.Now())
		if err != nil {
			t.Fatalf("parseStorageReport failed: %v", err)
		}
		got := make([]string, len(report.Tables))
		for i, table := range report.Tables {
			got[i] = table.Schema + "." + table.Name
		}
		if !slices.Equal(got, want) {
			t.Errorf("input order %v gives %v, want %v", order, got, want)
		}
	}
}

func TestParseStorageReport_Malformed(t *testing.T) {
	tests := []struct {
		name   string
		stdout string
	}{
		{name: "empty output", stdout: ""},
		{name: "not JSON", stdout: "Pager usage is off."},
		{name: "truncated document", stdout: `{"database" : "prefect", "database_bytes" : 1748424383, "tables" : [`},
		{name: "size of the wrong type", stdout: `{"database" : "prefect", "database_bytes" : "1.6 GB", "stats_reset" : null, "tables" : null}`},
		{name: "timestamp that is not RFC 3339", stdout: `{"database" : "prefect", "database_bytes" : 1, "stats_reset" : "yesterday", "tables" : null}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if report, err := parseStorageReport(tt.stdout, time.Now()); err == nil {
				t.Errorf("parseStorageReport(%q) = %+v, want an error", tt.stdout, report)
			}
		})
	}
}

// jsonKeys returns the sorted keys of a JSON object.
func jsonKeys(t *testing.T, object any) []string {
	t.Helper()
	fields, ok := object.(map[string]any)
	if !ok {
		t.Fatalf("%v is not a JSON object", object)
	}
	return slices.Sorted(maps.Keys(fields))
}

// TestStorageReport_JSONFieldNames pins the field names of storage.json to the
// required and allowed properties of contracts/storage.schema.json, which
// forbids any other field.
func TestStorageReport_JSONFieldNames(t *testing.T) {
	report, err := parseStorageReport(storageQueryFixture, time.Now())
	if err != nil {
		t.Fatalf("parseStorageReport failed: %v", err)
	}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("storage.json is not valid JSON: %v", err)
	}

	type fieldCheck struct {
		what   string
		object any
		want   []string
	}
	sizeKeys := []string{"bytes", "readable"}
	tableKeys := []string{"data_size", "dead_rows", "index_size", "last_autovacuum", "last_vacuum", "live_rows", "name", "schema", "toast_size", "total_size"}
	checks := []fieldCheck{
		{what: "document", object: document, want: []string{"collected_at", "database", "database_size", "statistics_reset_at", "tables"}},
		{what: "database_size", object: document["database_size"], want: sizeKeys},
	}
	for _, table := range document["tables"].([]any) {
		checks = append(checks, fieldCheck{what: "table", object: table, want: tableKeys})
		for _, size := range []string{"total_size", "data_size", "index_size", "toast_size"} {
			checks = append(checks, fieldCheck{what: "table " + size, object: table.(map[string]any)[size], want: sizeKeys})
		}
	}

	for _, check := range checks {
		if got := jsonKeys(t, check.object); !slices.Equal(got, check.want) {
			t.Errorf("%s fields = %v, want %v", check.what, got, check.want)
		}
	}
}

// storageCollectorPassword is the task manager database password in the
// collector tests. It is not a word of any fixture, so finding it anywhere in
// the bundle means it leaked.
const storageCollectorPassword = "Sup3r-Secr3t"

// useOperatorDBConnection makes the credential lookup take the login from a
// Prefect connection URL in the operator's environment, and keeps the
// developer's environment from lowering the query limit.
func useOperatorDBConnection(t *testing.T, password string) {
	t.Helper()
	clearOperatorPrefectEnv(t)
	t.Setenv(prefectConnectionEnvVars[0], "postgresql+asyncpg://tm-owner:"+password+"@task-manager-db:5432/tm-db")
	t.Setenv(dbQueryTimeoutEnvVar, "")
}

// newStorageQueryBackend returns a fake deployment with one task-manager-db
// replica whose psql exec answers through respond. Any SQL other than
// storageQuery fails the test: the collector only reads (constitution
// principle II).
func newStorageQueryBackend(t *testing.T, respond func(call dbQueryCall) (string, string, error)) *dbQueryBackend {
	t.Helper()
	backend := &dbQueryBackend{fakeCollectBackend: *newFakeCollectBackend()}
	backend.replicas[taskManagerDBService] = []Replica{{Service: taskManagerDBService, Container: "task-manager-db-1"}}
	backend.respond = func(call dbQueryCall) (string, string, error) {
		if sql := call.command[len(call.command)-1]; sql != storageQuery {
			t.Errorf("SQL sent to %s = %q, want storageQuery only", call.service, sql)
		}
		return respond(call)
	}
	return backend
}

// runStorageCollector runs the storage collector alone through runCollectPlan,
// requires the archive to be produced, and returns the collector's manifest
// entry and the extracted bundle directory.
func runStorageCollector(t *testing.T, backend EnvironmentBackend) (CollectorResult, string) {
	t.Helper()
	outputDir := filepath.Join(t.TempDir(), "bundles")
	opts := CollectOptions{OutputDir: outputDir, LogLines: 100}

	if err := NewInfrahubOps().runCollectPlan(backend, opts, []collector{taskManagerDBStorageCollector()}); err != nil {
		t.Fatalf("runCollectPlan failed, want the bundle produced whatever the query outcome: %v", err)
	}

	manifest, extracted := extractBundleManifest(t, findArchive(t, outputDir))
	if len(manifest.Collectors) != 1 {
		t.Fatalf("manifest collectors = %+v, want one entry", manifest.Collectors)
	}
	return manifest.Collectors[0], filepath.Join(extracted, "bundle")
}

func storageBundlePaths(bundleDir string) (storagePath, errPath string) {
	storagePath = filepath.Join(bundleDir, taskManagerDBBundleDir, storageFilename)
	return storagePath, dumpFailurePath(storagePath)
}

// readStorageReport decodes storage.json from an extracted bundle.
func readStorageReport(t *testing.T, bundleDir string) (storageReport, string) {
	t.Helper()
	storagePath, _ := storageBundlePaths(bundleDir)
	data, err := os.ReadFile(storagePath)
	if err != nil {
		t.Fatalf("storage.json missing from the bundle: %v", err)
	}
	var report storageReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("storage.json is not valid JSON: %v\n%s", err, data)
	}
	return report, string(data)
}

func assertFileAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s exists (stat error %v), want it absent", path, err)
	}
}

// TestTaskManagerDBStorageCollector_Success covers FR-001 to FR-003 through the
// orchestrator: the query runs once under the 60-second limit and its result
// becomes storage.json.
func TestTaskManagerDBStorageCollector_Success(t *testing.T) {
	useOperatorDBConnection(t, storageCollectorPassword)
	backend := newStorageQueryBackend(t, func(dbQueryCall) (string, string, error) {
		return storageQueryFixture, cnpgNotice, nil
	})

	result, bundleDir := runStorageCollector(t, backend)

	if want := (CollectorResult{Name: "task-manager-db-storage", Status: collectorStatusSuccess}); result != want {
		t.Errorf("manifest entry = %+v, want %+v", result, want)
	}
	if len(backend.calls) != 1 {
		t.Fatalf("exec calls = %d, want 1", len(backend.calls))
	}
	call := backend.calls[0]
	if call.service != taskManagerDBService || call.timeout != collectExecTimeout+collectDBQueryGrace {
		t.Errorf("exec in %q bounded at %s, want %q at %s", call.service, call.timeout, taskManagerDBService, collectExecTimeout+collectDBQueryGrace)
	}
	if call.opts == nil || !strings.Contains(call.opts.Env["PGOPTIONS"], "statement_timeout=60000") {
		t.Errorf("exec options = %+v, want PostgreSQL to enforce the 60-second limit", call.opts)
	}

	got, _ := readStorageReport(t, bundleDir)
	want, err := parseStorageReport(storageQueryFixture, time.Now())
	if err != nil {
		t.Fatalf("parseStorageReport failed: %v", err)
	}
	if _, err := time.Parse(time.RFC3339, got.CollectedAt); err != nil || !strings.HasSuffix(got.CollectedAt, "Z") {
		t.Errorf("collected_at = %q, want an RFC 3339 UTC timestamp", got.CollectedAt)
	}
	got.CollectedAt, want.CollectedAt = "", ""
	if !reflect.DeepEqual(got, want) {
		t.Errorf("storage.json = %+v, want %+v", got, want)
	}
	_, errPath := storageBundlePaths(bundleDir)
	assertFileAbsent(t, errPath)
}

// TestTaskManagerDBStorageCollector_DefaultPassword runs with the default
// credentials, whose password "prefect" is also the database name. The query
// result must reach storage.json unmasked.
func TestTaskManagerDBStorageCollector_DefaultPassword(t *testing.T) {
	clearOperatorPrefectEnv(t)
	t.Setenv(dbQueryTimeoutEnvVar, "")
	// Neither container environment names a login, so the lookup falls back to
	// the defaults.
	backend := newStorageQueryBackend(t, func(dbQueryCall) (string, string, error) {
		return storageQueryFixture, "", nil
	})

	result, bundleDir := runStorageCollector(t, backend)

	if result.Status != collectorStatusSuccess {
		t.Fatalf("manifest entry = %+v, want success", result)
	}
	if call := backend.calls[0]; call.opts == nil || call.opts.Env["PGPASSWORD"] != defaultPostgresPassword {
		t.Fatalf("the query did not use the default password %q", defaultPostgresPassword)
	}

	report, raw := readStorageReport(t, bundleDir)
	if report.Database != "prefect" || !strings.Contains(raw, `"database": "prefect"`) {
		t.Errorf("storage.json names the database %q, want \"prefect\":\n%s", report.Database, raw)
	}
	if strings.Contains(raw, maskedValue) {
		t.Errorf("storage.json holds %q, want the query result unmasked:\n%s", maskedValue, raw)
	}
}

// TestTaskManagerDBStorageCollector_NotDeployed covers FR-010: without a
// task-manager-db replica the collector is skipped with the external-PostgreSQL
// reason and runs no query.
func TestTaskManagerDBStorageCollector_NotDeployed(t *testing.T) {
	useOperatorDBConnection(t, storageCollectorPassword)
	backend := &dbQueryBackend{
		fakeCollectBackend: *newFakeCollectBackend(),
		respond: func(call dbQueryCall) (string, string, error) {
			t.Errorf("query %q ran although task-manager-db is not deployed", call.command)
			return "", "", nil
		},
	}

	result, bundleDir := runStorageCollector(t, backend)

	want := CollectorResult{Name: "task-manager-db-storage", Status: collectorStatusSkipped, Reason: taskManagerDBNotDeployedReason}
	if result != want {
		t.Errorf("manifest entry = %+v, want %+v", result, want)
	}
	assertFileAbsent(t, filepath.Join(bundleDir, taskManagerDBBundleDir))
}

// TestTaskManagerDBStorageCollector_Failures covers FR-011 and SC-004: each
// kind of failure records the collector as failed with its reason, writes the
// diagnostics to storage.err.txt instead of storage.json, and the archive is
// still produced. The outputs are the ones recorded in research R2 and R9.
func TestTaskManagerDBStorageCollector_Failures(t *testing.T) {
	statementTimeout := "ERROR:  canceling statement due to statement timeout"
	dockerPsqlMissing := `OCI runtime exec failed: exec failed: unable to start container process: exec: "psql": executable file not found in $PATH: unknown`
	badPassword := `psql: error: connection to server at "localhost" (127.0.0.1), port 5432 failed: FATAL:  password authentication failed for user "tm-owner"`
	notRunning := "Error response from daemon: container 3f2a9c1b7d4e is not running"

	tests := []struct {
		name       string
		stdout     string
		stderr     string
		err        error
		wantReason string
	}{
		{name: "PostgreSQL stops the query at its limit", stderr: statementTimeout, err: exitCodeError{code: 1}, wantReason: "timed out after 60s"},
		{name: "the tool-side bound stops the query", err: &timeoutError{timeout: collectExecTimeout + collectDBQueryGrace}, wantReason: "timed out after 60s"},
		{name: "psql missing in the container", stderr: dockerPsqlMissing, err: exitCodeError{code: 127}, wantReason: "psql missing: " + dockerPsqlMissing},
		{name: "PostgreSQL rejects the login", stderr: badPassword, err: exitCodeError{code: 2}, wantReason: "login rejected: " + badPassword},
		{name: "container not reachable", stderr: notRunning, err: exitCodeError{code: 1}, wantReason: "not reachable: " + notRunning},
		{name: "output that is not the storage document", stdout: "Pager usage is off.", wantReason: "query failed: could not parse the storage query output: invalid character 'P' looking for beginning of value"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			useOperatorDBConnection(t, storageCollectorPassword)
			backend := newStorageQueryBackend(t, func(dbQueryCall) (string, string, error) {
				return tt.stdout, tt.stderr, tt.err
			})

			result, bundleDir := runStorageCollector(t, backend)

			want := CollectorResult{Name: "task-manager-db-storage", Status: collectorStatusFailed, Reason: tt.wantReason}
			if result != want {
				t.Errorf("manifest entry = %+v, want %+v", result, want)
			}
			storagePath, errPath := storageBundlePaths(bundleDir)
			assertFileAbsent(t, storagePath)
			diagnostics, err := os.ReadFile(errPath)
			if err != nil {
				t.Fatalf("storage.err.txt missing from the bundle: %v", err)
			}
			for _, fragment := range []string{"command: psql -h localhost -U tm-owner -d tm-db", "error: " + tt.wantReason} {
				if !strings.Contains(string(diagnostics), fragment) {
					t.Errorf("storage.err.txt does not hold %q:\n%s", fragment, diagnostics)
				}
			}
		})
	}
}

// TestTaskManagerDBStorageCollector_PasswordNeverStaged covers FR-017: when
// kubectl or the container echoes `env PGPASSWORD=<password> psql ...` in its
// error and output streams or in the exec error, no file of the bundle,
// including the manifest, holds the password.
func TestTaskManagerDBStorageCollector_PasswordNeverStaged(t *testing.T) {
	echoed := "env PGAPPNAME=infrahub-collect PGPASSWORD=" + storageCollectorPassword + " psql -h localhost"
	tests := []struct {
		name   string
		stdout string
		stderr string
		err    error
	}{
		{
			name:   "echoed in both streams",
			stdout: echoed,
			stderr: cnpgNotice + "\nOCI runtime exec failed: exec: \"" + echoed + "\": executable file not found in $PATH\ncommand terminated with exit code 127",
			err:    exitCodeError{code: 127},
		},
		{
			name: "echoed in the exec error only",
			err:  fmt.Errorf("kubectl exec %s: %w", echoed, exitCodeError{code: 1}),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			useOperatorDBConnection(t, storageCollectorPassword)
			backend := newStorageQueryBackend(t, func(dbQueryCall) (string, string, error) {
				return tt.stdout, tt.stderr, tt.err
			})

			result, bundleDir := runStorageCollector(t, backend)

			if result.Status != collectorStatusFailed {
				t.Errorf("manifest entry = %+v, want failed", result)
			}
			if strings.Contains(result.Reason, storageCollectorPassword) {
				t.Errorf("manifest reason holds the password: %q", result.Reason)
			}
			if call := backend.calls[0]; call.opts == nil || call.opts.Env["PGPASSWORD"] != storageCollectorPassword {
				t.Fatalf("the query did not receive the password, so the test checks nothing")
			}

			_, errPath := storageBundlePaths(bundleDir)
			if diagnostics, err := os.ReadFile(errPath); err != nil || !strings.Contains(string(diagnostics), maskedValue) {
				t.Errorf("storage.err.txt = %q (%v), want the diagnostics with the password masked", diagnostics, err)
			}
			staged := 0
			walkErr := filepath.WalkDir(bundleDir, func(path string, entry fs.DirEntry, err error) error {
				if err != nil || entry.IsDir() {
					return err
				}
				staged++
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				if strings.Contains(string(data), storageCollectorPassword) {
					t.Errorf("%s holds the password:\n%s", path, data)
				}
				return nil
			})
			if walkErr != nil {
				t.Fatalf("walking the bundle failed: %v", walkErr)
			}
			if staged < 2 {
				t.Errorf("bundle holds %d files, want at least the manifest and storage.err.txt", staged)
			}
		})
	}
}
