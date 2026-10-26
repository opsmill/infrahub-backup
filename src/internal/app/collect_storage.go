package app

import (
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// storageFilename is the bundle file, in taskManagerDBBundleDir, that holds the
// storage figures of the task manager database (contracts/storage.schema.json).
const storageFilename = "storage.json"

// storageQuery reads the storage figures of the task manager database in one
// statement (research R5): the database size, when PostgreSQL last reset its
// statistics, and for every table in pg_stat_user_tables its table size (which
// includes TOAST), its index size, its TOAST size (the TOAST table with its
// index), the live and dead row estimates and the last vacuum times. It reads
// file sizes and statistics, not rows, so it takes the same time whatever the
// size of the database. The size functions take an ACCESS SHARE lock on each
// table, so a table locked by a schema change makes the query wait until its
// time limit.
//
// The total of a table is not read with pg_total_relation_size: PostgreSQL
// defines it as pg_table_size plus pg_indexes_size, but each call reads the
// file sizes at its own moment, so a table that grows between two calls would
// break data + indexes + TOAST = total. parseStorageReport adds the parts
// instead.
const storageQuery = `SELECT json_build_object(
    'database', current_database(),
    'database_bytes', pg_database_size(current_database()),
    'stats_reset', (SELECT d.stats_reset FROM pg_stat_database d WHERE d.datname = current_database()),
    'tables', (
        SELECT json_agg(json_build_object(
            'schema', s.schemaname,
            'name', s.relname,
            'table_bytes', pg_table_size(s.relid),
            'index_bytes', pg_indexes_size(s.relid),
            'toast_bytes', CASE WHEN c.reltoastrelid <> 0 THEN pg_total_relation_size(c.reltoastrelid) ELSE 0 END,
            'live_rows', s.n_live_tup,
            'dead_rows', s.n_dead_tup,
            'last_vacuum', s.last_vacuum,
            'last_autovacuum', s.last_autovacuum
        ))
        FROM pg_stat_user_tables s
        JOIN pg_class c ON c.oid = s.relid
    )
)`

// storageQueryOutput is the JSON document storageQuery prints.
type storageQueryOutput struct {
	Database      string              `json:"database"`
	DatabaseBytes int64               `json:"database_bytes"`
	StatsReset    *time.Time          `json:"stats_reset"`
	Tables        []storageQueryTable `json:"tables"`
}

// storageQueryTable is one table of storageQueryOutput. TableBytes includes
// ToastBytes, as pg_table_size does; ToastBytes includes the TOAST index.
type storageQueryTable struct {
	Schema         string     `json:"schema"`
	Name           string     `json:"name"`
	TableBytes     int64      `json:"table_bytes"`
	IndexBytes     int64      `json:"index_bytes"`
	ToastBytes     int64      `json:"toast_bytes"`
	LiveRows       int64      `json:"live_rows"`
	DeadRows       int64      `json:"dead_rows"`
	LastVacuum     *time.Time `json:"last_vacuum"`
	LastAutovacuum *time.Time `json:"last_autovacuum"`
}

// byteSize is one size in the storage files (FR-003): the bytes, for scripts,
// and the same size as formatBytes writes it, for people.
type byteSize struct {
	Bytes    int64  `json:"bytes"`
	Readable string `json:"readable"`
}

func newByteSize(n int64) byteSize {
	return byteSize{Bytes: n, Readable: formatBytes(n)}
}

// tableStorage is one table of storage.json. DataSize, IndexSize and
// ToastSize add up to TotalSize.
type tableStorage struct {
	Schema         string   `json:"schema"`
	Name           string   `json:"name"`
	TotalSize      byteSize `json:"total_size"`
	DataSize       byteSize `json:"data_size"`
	IndexSize      byteSize `json:"index_size"`
	ToastSize      byteSize `json:"toast_size"`
	LiveRows       int64    `json:"live_rows"`
	DeadRows       int64    `json:"dead_rows"`
	LastVacuum     *string  `json:"last_vacuum"`
	LastAutovacuum *string  `json:"last_autovacuum"`
}

// storageReport is the document written to task-manager-db/storage.json
// (data-model.md, contracts/storage.schema.json).
type storageReport struct {
	CollectedAt       string         `json:"collected_at"`
	Database          string         `json:"database"`
	DatabaseSize      byteSize       `json:"database_size"`
	StatisticsResetAt *string        `json:"statistics_reset_at"`
	Tables            []tableStorage `json:"tables"`
}

// parseStorageReport turns the output of storageQuery into the storage.json
// document. The data size is the table size without TOAST, and the total is
// the sum of the data, index and TOAST sizes, so the parts always add up. A
// database without tables gives an empty list, and the tables are ordered by
// total size, largest first, then by schema and name, so two runs on the same
// database list them in the same order.
func parseStorageReport(stdout string, collectedAt time.Time) (storageReport, error) {
	var output storageQueryOutput
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &output); err != nil {
		return storageReport{}, fmt.Errorf("could not parse the storage query output: %w", err)
	}

	report := storageReport{
		CollectedAt:       formatStorageTime(collectedAt),
		Database:          output.Database,
		DatabaseSize:      newByteSize(output.DatabaseBytes),
		StatisticsResetAt: formatOptionalStorageTime(output.StatsReset),
		Tables:            make([]tableStorage, 0, len(output.Tables)),
	}
	for _, t := range output.Tables {
		// The floor keeps the data size at zero when the TOAST table grows
		// between the two size calls.
		data := max(0, t.TableBytes-t.ToastBytes)
		report.Tables = append(report.Tables, tableStorage{
			Schema:         t.Schema,
			Name:           t.Name,
			TotalSize:      newByteSize(data + t.IndexBytes + t.ToastBytes),
			DataSize:       newByteSize(data),
			IndexSize:      newByteSize(t.IndexBytes),
			ToastSize:      newByteSize(t.ToastBytes),
			LiveRows:       t.LiveRows,
			DeadRows:       t.DeadRows,
			LastVacuum:     formatOptionalStorageTime(t.LastVacuum),
			LastAutovacuum: formatOptionalStorageTime(t.LastAutovacuum),
		})
	}
	slices.SortFunc(report.Tables, func(a, b tableStorage) int {
		return cmp.Or(
			cmp.Compare(b.TotalSize.Bytes, a.TotalSize.Bytes),
			cmp.Compare(a.Schema, b.Schema),
			cmp.Compare(a.Name, b.Name),
		)
	})
	return report, nil
}

// formatStorageTime renders a timestamp of the storage files as RFC 3339 in
// UTC, as the bundle manifest does.
func formatStorageTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// formatOptionalStorageTime is formatStorageTime for a timestamp PostgreSQL
// may not have, such as a table never vacuumed: nil stays null in the file.
func formatOptionalStorageTime(t *time.Time) *string {
	if t == nil {
		return nil
	}
	formatted := formatStorageTime(*t)
	return &formatted
}

// taskManagerDBStorageCollector writes the storage figures of the task manager
// database to bundle/task-manager-db/storage.json in every run (FR-001 to
// FR-003). It is skipped when task-manager-db has no replica (FR-010).
func taskManagerDBStorageCollector() collector {
	return collector{
		name: "task-manager-db-storage",
		skip: skipWhenTaskManagerDBAbsent,
		run:  collectTaskManagerDBStorage,
	}
}

// collectTaskManagerDBStorage runs storageQuery under the status-dump limit,
// which PostgreSQL enforces. A failed query writes its diagnostics, with the
// password masked, to storage.err.txt instead of storage.json and returns the
// *dbQueryError, so the manifest records its kind and cause, or
// "timed out after 60s" (FR-011).
func collectTaskManagerDBStorage(cc *collectContext) error {
	dir := filepath.Join(cc.bundleDir, taskManagerDBBundleDir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create %s directory: %w", taskManagerDBBundleDir, err)
	}
	path := filepath.Join(dir, storageFilename)
	mask := maskSecretValue(resolveTaskManagerDBCredentials(cc).password)

	collectedAt := time.Now()
	result, err := runTaskManagerDBQuery(cc, dbQuery{sql: storageQuery, limit: effectiveDBQueryLimit(collectExecTimeout, os.Getenv)})
	if err != nil {
		if writeErr := writeDumpFailure(path, result.command, err, result.stdout, result.stderr, mask); writeErr != nil {
			logrus.Warnf("Could not record the failed storage query: %v", writeErr)
		}
		return err
	}
	if result.stderr != "" {
		logrus.Debugf("stderr from the storage query in %s: %s", taskManagerDBService, result.stderr)
	}

	report, err := parseStorageReport(result.stdout, collectedAt)
	if err != nil {
		// Output that is not the expected document is a query failure: keep it,
		// masked, in the diagnostics for support.
		queryErr := &dbQueryError{kind: dbQueryFailed, detail: err.Error()}
		if writeErr := writeDumpFailure(path, result.command, queryErr, result.stdout, result.stderr, mask); writeErr != nil {
			logrus.Warnf("Could not record the unreadable storage query output: %v", writeErr)
		}
		return queryErr
	}

	data, err := json.MarshalIndent(report, "", "    ")
	if err != nil {
		return fmt.Errorf("failed to marshal the storage report: %w", err)
	}
	return writeDumpFile(path, string(data), nil)
}
