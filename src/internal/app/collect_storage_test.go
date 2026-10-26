package app

import (
	"context"
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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	logrustest "github.com/sirupsen/logrus/hooks/test"
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

// The enums of contracts/storage-breakdown.schema.json: the table names, the
// group key names and the timestamp columns that storage-breakdown.json may
// hold.
var (
	breakdownSchemaTables           = []string{"events", "flow_run", "task_run", "flow_run_state", "task_run_state", "log", "artifact"}
	breakdownSchemaKeys             = []string{"event_name", "flow_name", "state_type", "log_level", "artifact_type"}
	breakdownSchemaTimestampColumns = []string{"occurred", "start_time", "timestamp", "created"}
)

// breakdownSpec returns the entry of breakdownTables for table.
func breakdownSpec(t *testing.T, table string) breakdownTableSpec {
	t.Helper()
	for _, spec := range breakdownTables {
		if spec.table == table {
			return spec
		}
	}
	t.Fatalf("breakdownTables has no entry for %s", table)
	return breakdownTableSpec{}
}

// TestBreakdownTables_ListOrder pins the breakdown list to the seven tables of
// FR-005, in the order the collector reads them (research R7).
func TestBreakdownTables_ListOrder(t *testing.T) {
	got := make([]string, len(breakdownTables))
	for i, spec := range breakdownTables {
		got[i] = spec.table
	}
	if !slices.Equal(got, breakdownSchemaTables) {
		t.Errorf("breakdown list = %v, want %v", got, breakdownSchemaTables)
	}
}

// TestBreakdownQuery checks the SQL built for each table against research R7
// and FR-005 to FR-007: the table it reads, the LEFT JOINs that reach a flow
// name, the group keys, the timestamp column of the oldest and newest times,
// and the count of rows without start_time for the run tables only. Every
// table and key name must be one the file contract allows.
func TestBreakdownQuery(t *testing.T) {
	joinFlowOfFlowRun := "LEFT JOIN flow f ON f.id = t.flow_id"
	joinParentFlowRun := "LEFT JOIN flow_run fr ON fr.id = t.flow_run_id"
	joinFlowOfParentRun := "LEFT JOIN flow f ON f.id = fr.flow_id"
	logLevel := "CASE t.level WHEN 10 THEN 'DEBUG' WHEN 20 THEN 'INFO' WHEN 30 THEN 'WARNING' WHEN 40 THEN 'ERROR' WHEN 50 THEN 'CRITICAL' ELSE t.level::text END"

	tests := []struct {
		table                 string
		wantJoins             []string
		wantKeys              []string
		wantGroupBy           string
		wantTimestamp         string
		wantRowsWithoutStart  bool
		wantKeyValueFragments []string
	}{
		{table: "events", wantKeys: []string{"event_name"}, wantGroupBy: "t.event", wantTimestamp: "occurred", wantKeyValueFragments: []string{"'event_name', t.event"}},
		{table: "flow_run", wantJoins: []string{joinFlowOfFlowRun}, wantKeys: []string{"flow_name", "state_type"}, wantGroupBy: "f.name, t.state_type", wantTimestamp: "start_time", wantRowsWithoutStart: true, wantKeyValueFragments: []string{"'flow_name', f.name", "'state_type', t.state_type"}},
		{table: "task_run", wantJoins: []string{joinParentFlowRun, joinFlowOfParentRun}, wantKeys: []string{"flow_name", "state_type"}, wantGroupBy: "f.name, t.state_type", wantTimestamp: "start_time", wantRowsWithoutStart: true, wantKeyValueFragments: []string{"'flow_name', f.name", "'state_type', t.state_type"}},
		{table: "flow_run_state", wantKeys: []string{"state_type"}, wantGroupBy: "t.type", wantTimestamp: "timestamp", wantKeyValueFragments: []string{"'state_type', t.type"}},
		{table: "task_run_state", wantKeys: []string{"state_type"}, wantGroupBy: "t.type", wantTimestamp: "timestamp", wantKeyValueFragments: []string{"'state_type', t.type"}},
		{table: "log", wantJoins: []string{joinParentFlowRun, joinFlowOfParentRun}, wantKeys: []string{"flow_name", "log_level"}, wantGroupBy: "f.name, " + logLevel, wantTimestamp: "timestamp", wantKeyValueFragments: []string{"'flow_name', f.name", "'log_level', " + logLevel}},
		{table: "artifact", wantKeys: []string{"artifact_type"}, wantGroupBy: "t.type", wantTimestamp: "created", wantKeyValueFragments: []string{"'artifact_type', t.type"}},
	}
	if len(tests) != len(breakdownTables) {
		t.Fatalf("the test covers %d tables, the breakdown list has %d", len(tests), len(breakdownTables))
	}

	for _, tt := range tests {
		t.Run(tt.table, func(t *testing.T) {
			spec := breakdownSpec(t, tt.table)
			sql := breakdownQuery(spec)

			if !slices.Contains(breakdownSchemaTables, spec.table) {
				t.Errorf("table %q is not in the schema enum %v", spec.table, breakdownSchemaTables)
			}
			if got := spec.keyNames(); !slices.Equal(got, tt.wantKeys) {
				t.Errorf("grouped_by = %v, want %v", got, tt.wantKeys)
			}
			for _, key := range spec.keyNames() {
				if !slices.Contains(breakdownSchemaKeys, key) {
					t.Errorf("key %q is not in the schema enum %v", key, breakdownSchemaKeys)
				}
			}
			if spec.timestampColumn != tt.wantTimestamp || !slices.Contains(breakdownSchemaTimestampColumns, spec.timestampColumn) {
				t.Errorf("timestamp column = %q, want %q from the schema enum %v", spec.timestampColumn, tt.wantTimestamp, breakdownSchemaTimestampColumns)
			}

			// One JSON array per table, [] when the table has no rows.
			if !strings.HasPrefix(sql, "SELECT COALESCE(json_agg(b.g), '[]'::json) FROM (SELECT json_build_object(") {
				t.Errorf("SQL does not return one JSON array:\n%s", sql)
			}
			from := "FROM " + strings.Join(slices.Concat([]string{tt.table + " t"}, tt.wantJoins), " ") + " GROUP BY " + tt.wantGroupBy + ") b"
			if !strings.HasSuffix(sql, from) {
				t.Errorf("SQL does not end with %q:\n%s", from, sql)
			}
			if got, want := strings.Count(sql, " JOIN "), len(tt.wantJoins); got != want {
				t.Errorf("SQL has %d joins, want %d:\n%s", got, want, sql)
			}
			for _, fragment := range slices.Concat(tt.wantKeyValueFragments, []string{
				"'rows', count(*)",
				"'oldest', min(t." + tt.wantTimestamp + ")",
				"'newest', max(t." + tt.wantTimestamp + ")",
			}) {
				if !strings.Contains(sql, fragment) {
					t.Errorf("SQL does not hold %q:\n%s", fragment, sql)
				}
			}
			withoutStart := "'rows_without_start_time', count(*) FILTER (WHERE t.start_time IS NULL)"
			if got := strings.Contains(sql, withoutStart); got != tt.wantRowsWithoutStart || strings.Count(sql, "start_time IS NULL") > 1 {
				t.Errorf("SQL counts rows without start_time = %v, want %v:\n%s", got, tt.wantRowsWithoutStart, sql)
			}
		})
	}
}

// TestBreakdownCatalogQuery checks that the catalog query asks PostgreSQL
// about every table on the breakdown list and about no other (research R6).
func TestBreakdownCatalogQuery(t *testing.T) {
	for _, spec := range breakdownTables {
		fragment := fmt.Sprintf("'%s', to_regclass('%s') IS NOT NULL", spec.table, spec.table)
		if !strings.Contains(breakdownCatalogQuery, fragment) {
			t.Errorf("catalog query does not hold %q:\n%s", fragment, breakdownCatalogQuery)
		}
	}
	if got := strings.Count(breakdownCatalogQuery, "to_regclass("); got != len(breakdownTables) {
		t.Errorf("catalog query checks %d tables, want the %d of the breakdown list", got, len(breakdownTables))
	}
}

// breakdownCatalogOutput is breakdownCatalogQuery output shaped as psql
// prints it, with each table of the list present unless named in absent.
func breakdownCatalogOutput(absent ...string) string {
	fields := make([]string, len(breakdownTables))
	for i, spec := range breakdownTables {
		fields[i] = fmt.Sprintf("%q : %t", spec.table, !slices.Contains(absent, spec.table))
	}
	return "{" + strings.Join(fields, ", ") + "}\n"
}

func TestParseBreakdownCatalog(t *testing.T) {
	exists, err := parseBreakdownCatalog(breakdownCatalogOutput("artifact"))
	if err != nil {
		t.Fatalf("parseBreakdownCatalog failed: %v", err)
	}
	for _, spec := range breakdownTables {
		if want := spec.table != "artifact"; exists[spec.table] != want {
			t.Errorf("%s exists = %v, want %v", spec.table, exists[spec.table], want)
		}
	}

	malformed := map[string]string{
		"empty output":  "",
		"not JSON":      "Pager usage is off.",
		"missing table": `{"events" : true, "flow_run" : true, "task_run" : true, "flow_run_state" : true, "task_run_state" : true, "log" : true}`,
		"not a boolean": strings.Replace(breakdownCatalogOutput(), `"log" : true`, `"log" : "yes"`, 1),
	}
	for name, stdout := range malformed {
		t.Run(name, func(t *testing.T) {
			if got, err := parseBreakdownCatalog(stdout); err == nil {
				t.Errorf("parseBreakdownCatalog(%q) = %v, want an error", stdout, got)
			}
		})
	}
}

// flowRunBreakdownFixture is breakdownQuery output for flow_run shaped as
// psql prints it (observed on PostgreSQL 18): a flow run cancelled before it
// started has no start_time, so its group has null oldest and newest times.
const flowRunBreakdownFixture = `[{"group" : {"flow_name" : "e2e-storage-seed", "state_type" : "CANCELLED"}, "rows" : 1, "rows_without_start_time" : 1, "oldest" : null, "newest" : null}, ` +
	`{"group" : {"flow_name" : "name with \"quotes\", commas; and ünïcode", "state_type" : "FAILED"}, "rows" : 1, "rows_without_start_time" : 0, "oldest" : "2026-10-23T11:00:00+00:00", "newest" : "2026-10-23T11:00:00+00:00"}, ` +
	`{"group" : {"flow_name" : "e2e-storage-seed", "state_type" : "COMPLETED"}, "rows" : 3, "rows_without_start_time" : 0, "oldest" : "2026-10-20T08:00:00.123456+02:00", "newest" : "2026-10-22T10:00:00+00:00"}]` + "\n"

func int64Pointer(n int64) *int64 { return &n }

func TestParseBreakdownGroups(t *testing.T) {
	groups, total, err := parseBreakdownGroups(flowRunBreakdownFixture)
	if err != nil {
		t.Fatalf("parseBreakdownGroups failed: %v", err)
	}

	want := []breakdownGroup{
		{
			Group:                map[string]*string{"flow_name": stringPointer("e2e-storage-seed"), "state_type": stringPointer("COMPLETED")},
			Rows:                 3,
			RowsWithoutStartTime: int64Pointer(0),
			Oldest:               stringPointer("2026-10-20T06:00:00Z"),
			Newest:               stringPointer("2026-10-22T10:00:00Z"),
		},
		{
			Group:                map[string]*string{"flow_name": stringPointer("e2e-storage-seed"), "state_type": stringPointer("CANCELLED")},
			Rows:                 1,
			RowsWithoutStartTime: int64Pointer(1),
		},
		{
			Group:                map[string]*string{"flow_name": stringPointer(`name with "quotes", commas; and ünïcode`), "state_type": stringPointer("FAILED")},
			Rows:                 1,
			RowsWithoutStartTime: int64Pointer(0),
			Oldest:               stringPointer("2026-10-23T11:00:00Z"),
			Newest:               stringPointer("2026-10-23T11:00:00Z"),
		},
	}
	if !reflect.DeepEqual(groups, want) {
		gotJSON, _ := json.MarshalIndent(groups, "", "  ")
		wantJSON, _ := json.MarshalIndent(want, "", "  ")
		t.Errorf("parseBreakdownGroups() =\n%s\nwant\n%s", gotJSON, wantJSON)
	}
	if total != 5 {
		t.Errorf("total = %d, want 5, the sum of the group rows", total)
	}
}

// TestParseBreakdownGroups_Order checks that the groups are ordered by rows,
// largest first, then by key values in key-name order with null last, whatever
// order PostgreSQL returns them in.
func TestParseBreakdownGroups_Order(t *testing.T) {
	group := func(flowName, level string, rows int) string {
		name := "null"
		if flowName != "" {
			name = strconv.Quote(flowName)
		}
		return fmt.Sprintf(`{"group" : {"flow_name" : %s, "log_level" : %q}, "rows" : %d, "oldest" : null, "newest" : null}`, name, level, rows)
	}
	entries := []string{
		group("", "INFO", 2),
		group("sync", "INFO", 2),
		group("deploy", "WARNING", 2),
		group("deploy", "ERROR", 2),
		group("sync", "DEBUG", 7),
		group("", "DEBUG", 2),
	}
	want := []string{"sync/DEBUG", "deploy/ERROR", "deploy/WARNING", "sync/INFO", "null/DEBUG", "null/INFO"}

	for _, order := range [][]int{{0, 1, 2, 3, 4, 5}, {5, 4, 3, 2, 1, 0}, {2, 0, 5, 1, 4, 3}, {3, 5, 1, 4, 0, 2}} {
		shuffled := make([]string, len(order))
		for i, index := range order {
			shuffled[i] = entries[index]
		}

		groups, total, err := parseBreakdownGroups("[" + strings.Join(shuffled, ", ") + "]")
		if err != nil {
			t.Fatalf("parseBreakdownGroups failed: %v", err)
		}
		got := make([]string, len(groups))
		for i, g := range groups {
			name := "null"
			if g.Group["flow_name"] != nil {
				name = *g.Group["flow_name"]
			}
			got[i] = name + "/" + *g.Group["log_level"]
		}
		if !slices.Equal(got, want) {
			t.Errorf("input order %v gives %v, want %v", order, got, want)
		}
		if total != 17 {
			t.Errorf("input order %v gives the total %d, want 17", order, total)
		}
	}
}

// TestParseBreakdownGroups_OptionalFields covers the fields a group may not
// have: rows_without_start_time only for the run tables, null times, and a
// null key value, and that each keeps its shape in storage-breakdown.json.
func TestParseBreakdownGroups_OptionalFields(t *testing.T) {
	tests := []struct {
		name          string
		stdout        string
		wantFragments []string
		denyFragments []string
	}{
		{
			name:          "a run table keeps rows_without_start_time, also when it is 0",
			stdout:        `[{"group" : {"flow_name" : "sync", "state_type" : "COMPLETED"}, "rows" : 4, "rows_without_start_time" : 0, "oldest" : "2026-10-20T06:00:00+00:00", "newest" : "2026-10-20T07:00:00+00:00"}]`,
			wantFragments: []string{`"rows_without_start_time":0`, `"oldest":"2026-10-20T06:00:00Z"`, `"newest":"2026-10-20T07:00:00Z"`},
		},
		{
			name:          "other tables have no rows_without_start_time",
			stdout:        `[{"group" : {"event_name" : "prefect.flow-run.Completed"}, "rows" : 2, "oldest" : "2026-10-20T06:00:03+00:00", "newest" : "2026-10-21T09:00:03+00:00"}]`,
			wantFragments: []string{`"group":{"event_name":"prefect.flow-run.Completed"}`},
			denyFragments: []string{"rows_without_start_time"},
		},
		{
			name:          "a group without a flow name and without times keeps the nulls",
			stdout:        `[{"group" : {"flow_name" : null, "state_type" : "PENDING"}, "rows" : 1, "rows_without_start_time" : 1, "oldest" : null, "newest" : null}]`,
			wantFragments: []string{`"flow_name":null`, `"oldest":null`, `"newest":null`, `"rows_without_start_time":1`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			groups, _, err := parseBreakdownGroups(tt.stdout)
			if err != nil {
				t.Fatalf("parseBreakdownGroups failed: %v", err)
			}
			data, err := json.Marshal(groups)
			if err != nil {
				t.Fatalf("marshal failed: %v", err)
			}
			for _, fragment := range tt.wantFragments {
				if !strings.Contains(string(data), fragment) {
					t.Errorf("groups %s do not hold %s", data, fragment)
				}
			}
			for _, fragment := range tt.denyFragments {
				if strings.Contains(string(data), fragment) {
					t.Errorf("groups %s hold %s", data, fragment)
				}
			}
		})
	}
}

// TestParseBreakdownGroups_NoGroups covers an empty table: the query prints
// [] and a JSON null is read the same way, as no groups and no rows.
func TestParseBreakdownGroups_NoGroups(t *testing.T) {
	for _, stdout := range []string{"[]\n", "null"} {
		groups, total, err := parseBreakdownGroups(stdout)
		if err != nil {
			t.Fatalf("parseBreakdownGroups(%q) failed: %v", stdout, err)
		}
		if groups == nil || len(groups) != 0 || total != 0 {
			t.Errorf("parseBreakdownGroups(%q) = %#v, %d, want no groups, never nil, and 0 rows", stdout, groups, total)
		}
	}
}

func TestParseBreakdownGroups_Malformed(t *testing.T) {
	tests := []struct {
		name   string
		stdout string
	}{
		{name: "empty output", stdout: ""},
		{name: "not JSON", stdout: "Pager usage is off."},
		{name: "truncated array", stdout: `[{"group" : {"event_name" : "prefect.flow-run.Completed"}, "rows" : 2`},
		{name: "an object instead of an array", stdout: `{"group" : {"event_name" : "x"}, "rows" : 2, "oldest" : null, "newest" : null}`},
		{name: "rows of the wrong type", stdout: `[{"group" : {"event_name" : "x"}, "rows" : "2", "oldest" : null, "newest" : null}]`},
		{name: "a key value that is not text", stdout: `[{"group" : {"flow_name" : null, "log_level" : 20}, "rows" : 2, "oldest" : null, "newest" : null}]`},
		{name: "timestamp that is not RFC 3339", stdout: `[{"group" : {"event_name" : "x"}, "rows" : 2, "oldest" : "yesterday", "newest" : null}]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if groups, total, err := parseBreakdownGroups(tt.stdout); err == nil {
				t.Errorf("parseBreakdownGroups(%q) = %+v, %d, want an error", tt.stdout, groups, total)
			}
		})
	}
}

// TestStorageBreakdown_JSONFieldNames pins the field names of
// storage-breakdown.json to contracts/storage-breakdown.schema.json, which
// forbids any other field: a collected table has rows and groups and no
// reason, even when it has no rows; an absent or failed table has a reason
// and neither rows nor groups.
func TestStorageBreakdown_JSONFieldNames(t *testing.T) {
	groups, rows, err := parseBreakdownGroups(flowRunBreakdownFixture)
	if err != nil {
		t.Fatalf("parseBreakdownGroups failed: %v", err)
	}
	noRows := int64(0)
	breakdown := storageBreakdown{
		CollectedAt:       formatStorageTime(time.Now()),
		Database:          "prefect",
		TimeLimitPerTable: "300s",
		Tables: []tableBreakdown{
			{Name: "flow_run", Status: breakdownTableCollected, GroupedBy: []string{"flow_name", "state_type"}, TimestampColumn: "start_time", Rows: &rows, Groups: groups},
			{Name: "task_run_state", Status: breakdownTableCollected, GroupedBy: []string{"state_type"}, TimestampColumn: "timestamp", Rows: &noRows, Groups: []breakdownGroup{}},
			{Name: "log", Status: breakdownTableFailed, Reason: "timed out after 300s", GroupedBy: []string{"flow_name", "log_level"}, TimestampColumn: "timestamp"},
			{Name: "artifact", Status: breakdownTableAbsent, Reason: breakdownTableAbsentReason, GroupedBy: []string{"artifact_type"}, TimestampColumn: "created"},
		},
	}
	data, err := json.Marshal(breakdown)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("storage-breakdown.json is not valid JSON: %v", err)
	}

	if got, want := jsonKeys(t, document), []string{"collected_at", "database", "tables", "time_limit_per_table"}; !slices.Equal(got, want) {
		t.Errorf("document fields = %v, want %v", got, want)
	}
	collectedKeys := []string{"grouped_by", "groups", "name", "rows", "status", "timestamp_column"}
	notCollectedKeys := []string{"grouped_by", "name", "reason", "status", "timestamp_column"}
	tables := document["tables"].([]any)
	for i, want := range [][]string{collectedKeys, collectedKeys, notCollectedKeys, notCollectedKeys} {
		if got := jsonKeys(t, tables[i]); !slices.Equal(got, want) {
			t.Errorf("table %d fields = %v, want %v", i, got, want)
		}
	}
	if !strings.Contains(string(data), `"rows":0,"groups":[]`) {
		t.Errorf("the empty table is not written with 0 rows and no groups: %s", data)
	}
	runGroupKeys := []string{"group", "newest", "oldest", "rows", "rows_without_start_time"}
	for _, group := range tables[0].(map[string]any)["groups"].([]any) {
		if got := jsonKeys(t, group); !slices.Equal(got, runGroupKeys) {
			t.Errorf("group fields = %v, want %v", got, runGroupKeys)
		}
	}
}

// breakdownFixtures is breakdownQuery output for each table of the
// breakdown list, shaped as psql prints it. task_run_state is empty.
var breakdownFixtures = map[string]string{
	"events": `[{"group" : {"event_name" : "prefect.task-run.Completed"}, "rows" : 1, "oldest" : "2026-10-20T06:00:02+00:00", "newest" : "2026-10-20T06:00:02+00:00"}, ` +
		`{"group" : {"event_name" : "prefect.flow-run.Completed"}, "rows" : 2, "oldest" : "2026-10-20T06:00:03+00:00", "newest" : "2026-10-21T09:00:03+00:00"}]`,
	"flow_run": flowRunBreakdownFixture,
	"task_run": `[{"group" : {"flow_name" : "e2e-storage-seed", "state_type" : "PENDING"}, "rows" : 1, "rows_without_start_time" : 1, "oldest" : null, "newest" : null}, ` +
		`{"group" : {"flow_name" : null, "state_type" : "COMPLETED"}, "rows" : 1, "rows_without_start_time" : 0, "oldest" : "2026-10-24T12:00:00+00:00", "newest" : "2026-10-24T12:00:00+00:00"}, ` +
		`{"group" : {"flow_name" : "e2e-storage-seed", "state_type" : "COMPLETED"}, "rows" : 2, "rows_without_start_time" : 0, "oldest" : "2026-10-20T06:00:01+00:00", "newest" : "2026-10-20T06:00:02+00:00"}]`,
	"flow_run_state": `[{"group" : {"state_type" : "SCHEDULED"}, "rows" : 2, "oldest" : "2026-10-20T05:59:00+00:00", "newest" : "2026-10-21T08:59:00+00:00"}, ` +
		`{"group" : {"state_type" : "COMPLETED"}, "rows" : 1, "oldest" : "2026-10-20T06:00:03+00:00", "newest" : "2026-10-20T06:00:03+00:00"}]`,
	"task_run_state": "[]",
	"log": `[{"group" : {"flow_name" : null, "log_level" : "DEBUG"}, "rows" : 1, "oldest" : "2026-10-25T00:00:00+00:00", "newest" : "2026-10-25T00:00:00+00:00"}, ` +
		`{"group" : {"flow_name" : "e2e-storage-seed", "log_level" : "25"}, "rows" : 1, "oldest" : "2026-10-23T11:00:01+00:00", "newest" : "2026-10-23T11:00:01+00:00"}, ` +
		`{"group" : {"flow_name" : "e2e-storage-seed", "log_level" : "INFO"}, "rows" : 2, "oldest" : "2026-10-20T06:00:01+00:00", "newest" : "2026-10-20T06:00:02+00:00"}]`,
	"artifact": `[{"group" : {"artifact_type" : null}, "rows" : 4, "oldest" : "2026-10-20T06:00:00+00:00", "newest" : "2026-10-20T06:00:04+00:00"}]`,
}

// breakdownCatalogName is the name breakdownAnswer receives for the catalog
// query.
const breakdownCatalogName = "catalog"

// breakdownAnswer is what the fake psql exec answers to one query.
type breakdownAnswer struct {
	stdout string
	stderr string
	err    error
}

// answerBreakdown answers the catalog query with every table present and each
// table's query with its fixture.
func answerBreakdown(query string) breakdownAnswer {
	if query == breakdownCatalogName {
		return breakdownAnswer{stdout: breakdownCatalogOutput()}
	}
	return breakdownAnswer{stdout: breakdownFixtures[query] + "\n"}
}

// newBreakdownQueryBackend returns a fake deployment with one task-manager-db
// replica whose psql exec answers by matching the SQL: answer receives
// breakdownCatalogName for the catalog query and the table name for a
// table's breakdown query. Any other SQL fails the test: the collector only
// reads (constitution principle II).
func newBreakdownQueryBackend(t *testing.T, answer func(query string) breakdownAnswer) *dbQueryBackend {
	t.Helper()
	queries := map[string]string{breakdownCatalogQuery: breakdownCatalogName}
	for _, spec := range breakdownTables {
		queries[breakdownQuery(spec)] = spec.table
	}

	backend := &dbQueryBackend{fakeCollectBackend: *newFakeCollectBackend()}
	backend.replicas[taskManagerDBService] = []Replica{{Service: taskManagerDBService, Container: "task-manager-db-1"}}
	backend.respond = func(call dbQueryCall) (string, string, error) {
		query, ok := queries[call.command[len(call.command)-1]]
		if !ok {
			t.Errorf("SQL sent to %s = %q, want the catalog query or a breakdown query only", call.service, call.command[len(call.command)-1])
			return "", "", errors.New("unexpected SQL")
		}
		a := answer(query)
		return a.stdout, a.stderr, a.err
	}
	return backend
}

// breakdownQueryNames returns, for each exec call, the query it ran:
// breakdownCatalogName or the table name.
func breakdownQueryNames(calls []dbQueryCall) []string {
	names := make([]string, len(calls))
	for i, call := range calls {
		sql := call.command[len(call.command)-1]
		names[i] = sql
		if sql == breakdownCatalogQuery {
			names[i] = breakdownCatalogName
		}
		for _, spec := range breakdownTables {
			if sql == breakdownQuery(spec) {
				names[i] = spec.table
			}
		}
	}
	return names
}

// runBreakdownCollector runs the breakdown collector alone through
// runCollectPlan, with --include-task-manager-storage set to include,
// requires the archive to be produced, and returns the collector's manifest
// entry and the extracted bundle directory.
func runBreakdownCollector(t *testing.T, backend EnvironmentBackend, include bool) (CollectorResult, string) {
	t.Helper()
	outputDir := filepath.Join(t.TempDir(), "bundles")
	opts := CollectOptions{OutputDir: outputDir, LogLines: 100, IncludeTaskManagerStorage: include}

	if err := NewInfrahubOps().runCollectPlan(backend, opts, []collector{taskManagerDBStorageBreakdownCollector()}); err != nil {
		t.Fatalf("runCollectPlan failed, want the bundle produced whatever the breakdown outcome: %v", err)
	}

	manifest, extracted := extractBundleManifest(t, findArchive(t, outputDir))
	if len(manifest.Collectors) != 1 {
		t.Fatalf("manifest collectors = %+v, want one entry", manifest.Collectors)
	}
	return manifest.Collectors[0], filepath.Join(extracted, "bundle")
}

func storageBreakdownBundlePaths(bundleDir string) (breakdownPath, errPath string) {
	breakdownPath = filepath.Join(bundleDir, taskManagerDBBundleDir, storageBreakdownFilename)
	return breakdownPath, dumpFailurePath(breakdownPath)
}

// readStorageBreakdown decodes storage-breakdown.json from an extracted
// bundle.
func readStorageBreakdown(t *testing.T, bundleDir string) storageBreakdown {
	t.Helper()
	breakdownPath, _ := storageBreakdownBundlePaths(bundleDir)
	data, err := os.ReadFile(breakdownPath)
	if err != nil {
		t.Fatalf("storage-breakdown.json missing from the bundle: %v", err)
	}
	var breakdown storageBreakdown
	if err := json.Unmarshal(data, &breakdown); err != nil {
		t.Fatalf("storage-breakdown.json is not valid JSON: %v\n%s", err, data)
	}
	return breakdown
}

// tableStatuses returns the status of each table of a breakdown, by name.
func tableStatuses(breakdown storageBreakdown) map[string]string {
	statuses := map[string]string{}
	for _, table := range breakdown.Tables {
		statuses[table.Name] = table.Status
	}
	return statuses
}

const breakdownCollectorName = "task-manager-db-storage-breakdown"

// TestTaskManagerDBStorageBreakdownCollector_NotRequested covers FR-004:
// without the flag or its variable the breakdown is skipped as not requested,
// also when task-manager-db is not deployed, and runs no query.
func TestTaskManagerDBStorageBreakdownCollector_NotRequested(t *testing.T) {
	for _, deployed := range []bool{true, false} {
		t.Run(fmt.Sprintf("task-manager-db deployed %v", deployed), func(t *testing.T) {
			useOperatorDBConnection(t, storageCollectorPassword)
			backend := newBreakdownQueryBackend(t, func(query string) breakdownAnswer {
				t.Errorf("query %s ran although the breakdown was not requested", query)
				return breakdownAnswer{}
			})
			if !deployed {
				delete(backend.replicas, taskManagerDBService)
			}

			result, bundleDir := runBreakdownCollector(t, backend, false)

			if want := (CollectorResult{Name: breakdownCollectorName, Status: collectorStatusSkipped, Reason: "not requested"}); result != want {
				t.Errorf("manifest entry = %+v, want %+v", result, want)
			}
			assertFileAbsent(t, filepath.Join(bundleDir, taskManagerDBBundleDir))
		})
	}
}

// TestTaskManagerDBStorageBreakdownCollector_NotDeployed covers FR-010: with
// the flag but no task-manager-db replica the breakdown is skipped with the
// external-PostgreSQL reason.
func TestTaskManagerDBStorageBreakdownCollector_NotDeployed(t *testing.T) {
	useOperatorDBConnection(t, storageCollectorPassword)
	backend := newBreakdownQueryBackend(t, func(query string) breakdownAnswer {
		t.Errorf("query %s ran although task-manager-db is not deployed", query)
		return breakdownAnswer{}
	})
	delete(backend.replicas, taskManagerDBService)

	result, bundleDir := runBreakdownCollector(t, backend, true)

	if want := (CollectorResult{Name: breakdownCollectorName, Status: collectorStatusSkipped, Reason: taskManagerDBNotDeployedReason}); result != want {
		t.Errorf("manifest entry = %+v, want %+v", result, want)
	}
	assertFileAbsent(t, filepath.Join(bundleDir, taskManagerDBBundleDir))
}

// TestTaskManagerDBStorageBreakdownCollector_Success covers FR-005 to FR-009
// through the orchestrator: the catalog query runs under the 60-second limit,
// then each table under the 5-minute limit with the breakdown planner
// settings, and storage-breakdown.json holds every table in list order with
// row totals that match its groups.
func TestTaskManagerDBStorageBreakdownCollector_Success(t *testing.T) {
	useOperatorDBConnection(t, storageCollectorPassword)
	backend := newBreakdownQueryBackend(t, answerBreakdown)

	result, bundleDir := runBreakdownCollector(t, backend, true)

	if want := (CollectorResult{Name: breakdownCollectorName, Status: collectorStatusSuccess}); result != want {
		t.Errorf("manifest entry = %+v, want %+v", result, want)
	}
	if got, want := breakdownQueryNames(backend.calls), slices.Concat([]string{breakdownCatalogName}, breakdownSchemaTables); !slices.Equal(got, want) {
		t.Fatalf("queries = %v, want %v", got, want)
	}
	for i, call := range backend.calls {
		wantBound, wantTimeout, wantBreakdown := collectDBQueryTimeout+collectDBQueryGrace, "statement_timeout=300000", true
		if i == 0 {
			wantBound, wantTimeout, wantBreakdown = collectExecTimeout+collectDBQueryGrace, "statement_timeout=60000", false
		}
		if call.service != taskManagerDBService || call.timeout != wantBound {
			t.Errorf("call %d: exec in %q bounded at %s, want %q at %s", i, call.service, call.timeout, taskManagerDBService, wantBound)
		}
		options := call.opts.Env["PGOPTIONS"]
		if !strings.Contains(options, wantTimeout) || strings.Contains(options, "enable_hashjoin=off") != wantBreakdown {
			t.Errorf("call %d: PGOPTIONS = %q, want %s and the breakdown settings %v", i, options, wantTimeout, wantBreakdown)
		}
	}

	breakdown := readStorageBreakdown(t, bundleDir)
	if _, err := time.Parse(time.RFC3339, breakdown.CollectedAt); err != nil || !strings.HasSuffix(breakdown.CollectedAt, "Z") {
		t.Errorf("collected_at = %q, want an RFC 3339 UTC timestamp", breakdown.CollectedAt)
	}
	if breakdown.Database != "tm-db" || breakdown.TimeLimitPerTable != "300s" {
		t.Errorf("database = %q, time_limit_per_table = %q, want tm-db and 300s", breakdown.Database, breakdown.TimeLimitPerTable)
	}
	if len(breakdown.Tables) != len(breakdownTables) {
		t.Fatalf("storage-breakdown.json has %d tables, want %d", len(breakdown.Tables), len(breakdownTables))
	}
	for i, table := range breakdown.Tables {
		spec := breakdownTables[i]
		wantGroups, wantRows, err := parseBreakdownGroups(breakdownFixtures[spec.table])
		if err != nil {
			t.Fatalf("parseBreakdownGroups(%s fixture) failed: %v", spec.table, err)
		}
		want := tableBreakdown{Name: spec.table, Status: breakdownTableCollected, GroupedBy: spec.keyNames(), TimestampColumn: spec.timestampColumn, Rows: &wantRows, Groups: wantGroups}
		if !reflect.DeepEqual(table, want) {
			t.Errorf("table %d = %+v, want %+v", i, table, want)
		}
		var sum int64
		for _, group := range table.Groups {
			sum += group.Rows
		}
		if table.Rows == nil || *table.Rows != sum {
			t.Errorf("%s rows = %v, want the sum of its groups %d", table.Name, table.Rows, sum)
		}
	}
	_, errPath := storageBreakdownBundlePaths(bundleDir)
	assertFileAbsent(t, errPath)
}

// TestTaskManagerDBStorageBreakdownCollector_AbsentTable covers FR-012: a
// table the catalog query does not find is recorded as absent, its query
// never runs, and the collector still succeeds.
func TestTaskManagerDBStorageBreakdownCollector_AbsentTable(t *testing.T) {
	useOperatorDBConnection(t, storageCollectorPassword)
	backend := newBreakdownQueryBackend(t, func(query string) breakdownAnswer {
		if query == breakdownCatalogName {
			return breakdownAnswer{stdout: breakdownCatalogOutput("artifact")}
		}
		return answerBreakdown(query)
	})

	result, bundleDir := runBreakdownCollector(t, backend, true)

	if want := (CollectorResult{Name: breakdownCollectorName, Status: collectorStatusSuccess}); result != want {
		t.Errorf("manifest entry = %+v, want %+v", result, want)
	}
	if slices.Contains(breakdownQueryNames(backend.calls), "artifact") {
		t.Errorf("queries = %v, want none for the absent table", breakdownQueryNames(backend.calls))
	}
	breakdown := readStorageBreakdown(t, bundleDir)
	last := breakdown.Tables[len(breakdown.Tables)-1]
	want := tableBreakdown{Name: "artifact", Status: breakdownTableAbsent, Reason: "table does not exist in this Prefect version", GroupedBy: []string{"artifact_type"}, TimestampColumn: "created"}
	if !reflect.DeepEqual(last, want) {
		t.Errorf("artifact = %+v, want %+v", last, want)
	}
	for name, status := range tableStatuses(breakdown) {
		if name != "artifact" && status != breakdownTableCollected {
			t.Errorf("%s status = %q, want collected", name, status)
		}
	}
}

// TestTaskManagerDBStorageBreakdownCollector_TableFails covers FR-009 and
// FR-016: a table that fails is recorded as failed with its reason, the other
// tables are still collected, and the manifest reason names each failed table
// with its cause instead of collapsing to the timeout text.
func TestTaskManagerDBStorageBreakdownCollector_TableFails(t *testing.T) {
	statementTimeout := breakdownAnswer{stderr: "ERROR:  canceling statement due to statement timeout", err: exitCodeError{code: 1}}

	tests := []struct {
		name          string
		limitEnv      string
		failures      map[string]breakdownAnswer
		wantReasons   map[string]string
		wantLimit     string
		wantManifest  string
		wantCollected int
	}{
		{
			name:         "PostgreSQL stops the log query at its limit",
			failures:     map[string]breakdownAnswer{"log": statementTimeout},
			wantReasons:  map[string]string{"log": "timed out after 300s"},
			wantLimit:    "300s",
			wantManifest: "partial storage breakdown: log: timed out after 300s",
		},
		{
			name:         "the tool-side bound stops the log query",
			failures:     map[string]breakdownAnswer{"log": {err: &timeoutError{timeout: collectDBQueryTimeout + collectDBQueryGrace}}},
			wantReasons:  map[string]string{"log": "timed out after 300s"},
			wantLimit:    "300s",
			wantManifest: "partial storage breakdown: log: timed out after 300s",
		},
		{
			name:         "the test variable lowers the limit",
			limitEnv:     "5s",
			failures:     map[string]breakdownAnswer{"artifact": statementTimeout},
			wantReasons:  map[string]string{"artifact": "timed out after 5s"},
			wantLimit:    "5s",
			wantManifest: "partial storage breakdown: artifact: timed out after 5s",
		},
		{
			name: "two tables fail, one with output that is not JSON",
			failures: map[string]breakdownAnswer{
				"flow_run": statementTimeout,
				"log":      {stdout: "Pager usage is off."},
			},
			wantReasons: map[string]string{
				"flow_run": "timed out after 300s",
				"log":      "query failed: could not parse the storage breakdown query output: invalid character 'P' looking for beginning of value",
			},
			wantLimit:    "300s",
			wantManifest: "partial storage breakdown: flow_run: timed out after 300s; log: query failed: could not parse the storage breakdown query output: invalid character 'P' looking for beginning of value",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			useOperatorDBConnection(t, storageCollectorPassword)
			t.Setenv(dbQueryTimeoutEnvVar, tt.limitEnv)
			backend := newBreakdownQueryBackend(t, func(query string) breakdownAnswer {
				if failure, ok := tt.failures[query]; ok {
					return failure
				}
				return answerBreakdown(query)
			})

			result, bundleDir := runBreakdownCollector(t, backend, true)

			if want := (CollectorResult{Name: breakdownCollectorName, Status: collectorStatusFailed, Reason: tt.wantManifest}); result != want {
				t.Errorf("manifest entry = %+v, want %+v", result, want)
			}
			if got := len(backend.calls); got != 1+len(breakdownTables) {
				t.Errorf("exec calls = %d, want the catalog query and every table", got)
			}
			breakdown := readStorageBreakdown(t, bundleDir)
			if breakdown.TimeLimitPerTable != tt.wantLimit {
				t.Errorf("time_limit_per_table = %q, want %q", breakdown.TimeLimitPerTable, tt.wantLimit)
			}
			for _, table := range breakdown.Tables {
				if reason, failed := tt.wantReasons[table.Name]; failed {
					if table.Status != breakdownTableFailed || table.Reason != reason || table.Rows != nil || table.Groups != nil {
						t.Errorf("%s = %+v, want failed with %q and neither rows nor groups", table.Name, table, reason)
					}
				} else if table.Status != breakdownTableCollected || table.Reason != "" || table.Rows == nil {
					t.Errorf("%s = %+v, want collected", table.Name, table)
				}
			}
		})
	}
}

// TestTaskManagerDBStorageBreakdownCollector_CatalogFails covers research R6
// and R11: when the catalog query fails, its diagnostics go to
// storage-breakdown.err.txt, no table query runs, and the manifest records
// the classified reason.
func TestTaskManagerDBStorageBreakdownCollector_CatalogFails(t *testing.T) {
	badRole := `psql: error: connection to server at "localhost" (::1), port 5432 failed: FATAL:  role "tm-owner" does not exist`
	tests := []struct {
		name       string
		answer     breakdownAnswer
		wantReason string
	}{
		{name: "PostgreSQL rejects the login", answer: breakdownAnswer{stderr: badRole, err: exitCodeError{code: 2}}, wantReason: "login rejected: " + badRole},
		{name: "the catalog query reaches its limit", answer: breakdownAnswer{stderr: "ERROR:  canceling statement due to statement timeout", err: exitCodeError{code: 1}}, wantReason: "timed out after 60s"},
		{name: "output that is not the catalog document", answer: breakdownAnswer{stdout: "Pager usage is off."}, wantReason: "query failed: could not parse the storage breakdown catalog output: invalid character 'P' looking for beginning of value"},
		{name: "a catalog document without a table", answer: breakdownAnswer{stdout: `{"events" : true}`}, wantReason: "query failed: could not parse the storage breakdown catalog output: table flow_run is missing"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			useOperatorDBConnection(t, storageCollectorPassword)
			backend := newBreakdownQueryBackend(t, func(query string) breakdownAnswer {
				if query != breakdownCatalogName {
					t.Errorf("table query %s ran after the catalog query failed", query)
				}
				return tt.answer
			})

			result, bundleDir := runBreakdownCollector(t, backend, true)

			if want := (CollectorResult{Name: breakdownCollectorName, Status: collectorStatusFailed, Reason: tt.wantReason}); result != want {
				t.Errorf("manifest entry = %+v, want %+v", result, want)
			}
			if len(backend.calls) != 1 {
				t.Errorf("exec calls = %d, want the catalog query only", len(backend.calls))
			}
			breakdownPath, errPath := storageBreakdownBundlePaths(bundleDir)
			assertFileAbsent(t, breakdownPath)
			diagnostics, err := os.ReadFile(errPath)
			if err != nil {
				t.Fatalf("storage-breakdown.err.txt missing from the bundle: %v", err)
			}
			for _, fragment := range []string{"command: psql -h localhost -U tm-owner -d tm-db", "to_regclass('events')", "error: " + tt.wantReason} {
				if !strings.Contains(string(diagnostics), fragment) {
					t.Errorf("storage-breakdown.err.txt does not hold %q:\n%s", fragment, diagnostics)
				}
			}
		})
	}
}

// captureLogs records every entry of the standard logger at info level and
// above for the rest of the test, and puts back the logger's hooks and level
// afterwards.
func captureLogs(t *testing.T) *logrustest.Hook {
	t.Helper()
	logger := logrus.StandardLogger()
	previousHooks := logger.ReplaceHooks(make(logrus.LevelHooks))
	previousLevel := logger.GetLevel()
	logger.SetLevel(logrus.InfoLevel)
	t.Cleanup(func() {
		logger.ReplaceHooks(previousHooks)
		logger.SetLevel(previousLevel)
	})
	return logrustest.NewGlobal()
}

// TestTaskManagerDBStorageBreakdownCollector_ProgressOutput covers FR-013: the
// progress output names each table, in list order, as the collector starts
// on it, then reports its outcome, with a warning for a failed table.
func TestTaskManagerDBStorageBreakdownCollector_ProgressOutput(t *testing.T) {
	useOperatorDBConnection(t, storageCollectorPassword)
	backend := newBreakdownQueryBackend(t, func(query string) breakdownAnswer {
		switch query {
		case breakdownCatalogName:
			return breakdownAnswer{stdout: breakdownCatalogOutput("artifact")}
		case "log":
			return breakdownAnswer{stderr: "ERROR:  canceling statement due to statement timeout", err: exitCodeError{code: 1}}
		}
		return answerBreakdown(query)
	})
	hook := captureLogs(t)

	runBreakdownCollector(t, backend, true)

	var starts []string
	outcomes := map[string]*logrus.Entry{}
	for _, entry := range hook.AllEntries() {
		if table, ok := strings.CutPrefix(entry.Message, "Collecting storage breakdown of table "); ok {
			if entry.Level != logrus.InfoLevel {
				t.Errorf("%q logged at %s, want info", entry.Message, entry.Level)
			}
			starts = append(starts, table)
		} else if rest, ok := strings.CutPrefix(entry.Message, "Table "); ok {
			outcomes[strings.SplitN(rest, ":", 2)[0]] = entry
		}
	}
	wantStarts := make([]string, len(breakdownTables))
	for i, spec := range breakdownTables {
		wantStarts[i] = spec.table + " (time limit 300s)"
	}
	if !slices.Equal(starts, wantStarts) {
		t.Errorf("start lines name %v, want %v", starts, wantStarts)
	}

	wantOutcomes := map[string]struct {
		level   logrus.Level
		pattern string
	}{
		"events":         {logrus.InfoLevel, `^Table events: 3 rows in 2 groups \([0-9.]+[µnm]?s\)$`},
		"flow_run":       {logrus.InfoLevel, `^Table flow_run: 5 rows in 3 groups \(`},
		"task_run":       {logrus.InfoLevel, `^Table task_run: 4 rows in 3 groups \(`},
		"flow_run_state": {logrus.InfoLevel, `^Table flow_run_state: 3 rows in 2 groups \(`},
		"task_run_state": {logrus.InfoLevel, `^Table task_run_state: 0 rows in 0 groups \(`},
		"log":            {logrus.WarnLevel, `^Table log: timed out after 300s$`},
		"artifact":       {logrus.InfoLevel, `^Table artifact: absent$`},
	}
	for table, want := range wantOutcomes {
		entry, ok := outcomes[table]
		if !ok {
			t.Errorf("no outcome line for table %s", table)
			continue
		}
		if entry.Level != want.level || !regexp.MustCompile(want.pattern).MatchString(entry.Message) {
			t.Errorf("outcome of %s = %s %q, want %s matching %s", table, entry.Level, entry.Message, want.level, want.pattern)
		}
	}
}

// TestTaskManagerDBStorageBreakdownCollector_StopsWhenInterrupted checks that
// the collector reads no further table once the run is interrupted. The
// orchestrator owns its context, so the collector runs alone here.
func TestTaskManagerDBStorageBreakdownCollector_StopsWhenInterrupted(t *testing.T) {
	t.Setenv(dbQueryTimeoutEnvVar, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	backend := newBreakdownQueryBackend(t, func(query string) breakdownAnswer {
		if query == breakdownTables[0].table {
			cancel()
		}
		return answerBreakdown(query)
	})
	cc := newDBQueryContext(t, backend, taskManagerDBCredentials{user: "tm-owner", password: storageCollectorPassword, database: "tm-db"})
	cc.ctx = ctx
	cc.opts.IncludeTaskManagerStorage = true

	err := collectTaskManagerDBStorageBreakdown(cc)

	if !errors.Is(err, context.Canceled) {
		t.Errorf("collector error = %v, want the interruption", err)
	}
	if got, want := breakdownQueryNames(backend.calls), []string{breakdownCatalogName, breakdownTables[0].table}; !slices.Equal(got, want) {
		t.Errorf("queries = %v, want %v and nothing after the interruption", got, want)
	}
}

// TestTaskManagerDBStorageBreakdownCollector_PasswordNeverStaged covers
// FR-017: when the container echoes `env PGPASSWORD=<password> psql ...` in
// the output of a failed query, neither the bundle files, the manifest nor
// the progress output hold the password.
func TestTaskManagerDBStorageBreakdownCollector_PasswordNeverStaged(t *testing.T) {
	echoed := "env PGAPPNAME=infrahub-collect PGPASSWORD=" + storageCollectorPassword + " psql -h localhost"
	echoedFailure := breakdownAnswer{
		stdout: echoed,
		stderr: cnpgNotice + "\nOCI runtime exec failed: exec: \"" + echoed + "\": executable file not found in $PATH\ncommand terminated with exit code 127",
		err:    exitCodeError{code: 127},
	}
	tests := []struct {
		name        string
		failedQuery string
		wantFiles   int
	}{
		{name: "a table query fails", failedQuery: "log", wantFiles: 2},
		{name: "the catalog query fails", failedQuery: breakdownCatalogName, wantFiles: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			useOperatorDBConnection(t, storageCollectorPassword)
			backend := newBreakdownQueryBackend(t, func(query string) breakdownAnswer {
				if query == tt.failedQuery {
					return echoedFailure
				}
				return answerBreakdown(query)
			})
			hook := captureLogs(t)

			result, bundleDir := runBreakdownCollector(t, backend, true)

			if result.Status != collectorStatusFailed || !strings.Contains(result.Reason, maskedValue) {
				t.Errorf("manifest entry = %+v, want failed with the password masked", result)
			}
			if call := backend.calls[0]; call.opts == nil || call.opts.Env["PGPASSWORD"] != storageCollectorPassword {
				t.Fatalf("the query did not receive the password, so the test checks nothing")
			}
			for _, entry := range hook.AllEntries() {
				if strings.Contains(entry.Message, storageCollectorPassword) {
					t.Errorf("progress output holds the password: %q", entry.Message)
				}
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
			if staged < tt.wantFiles {
				t.Errorf("bundle holds %d files, want at least the manifest and the breakdown file or its diagnostics", staged)
			}
		})
	}
}
