package app

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
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
	mask := resolveTaskManagerDBCredentials(cc).passwordMask()

	collectedAt := time.Now()
	result, err := runTaskManagerDBQuery(cc, dbQuery{sql: storageQuery, limit: cc.dbQueryLimit(collectExecTimeout)})
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

// storageBreakdownFilename is the bundle file, in taskManagerDBBundleDir, that
// holds the storage breakdown of the task manager database
// (contracts/storage-breakdown.schema.json).
const storageBreakdownFilename = "storage-breakdown.json"

// breakdownGroupKey is one key a breakdown table is grouped by: its name in
// storage-breakdown.json and the SQL expression that computes it.
type breakdownGroupKey struct {
	name string
	expr string
}

// breakdownTableSpec is one entry of the breakdown list (research R7): the
// table, the keys its rows are grouped by, the LEFT JOINs those keys need,
// the timestamp column the oldest and newest times come from, and whether
// each group also counts its rows without start_time (FR-007). In the SQL,
// the table is t, its parent flow run fr and the flow f.
type breakdownTableSpec struct {
	table                 string
	keys                  []breakdownGroupKey
	joins                 []string
	timestampColumn       string
	countWithoutStartTime bool
}

// keyNames returns the names of the group keys, in order, for grouped_by.
func (s breakdownTableSpec) keyNames() []string {
	names := make([]string, len(s.keys))
	for i, key := range s.keys {
		names[i] = key.name
	}
	return names
}

// The joins that reach a flow name. They are LEFT JOINs, so a task run or log
// record without a parent flow run falls into a group whose flow name is null,
// and the groups of a table add up to its row count (research R7). Each join
// goes through a primary key, which the breakdown planner settings turn into
// an index lookup that never writes temporary files (research R4).
const (
	breakdownJoinFlowOfFlowRun   = "LEFT JOIN flow f ON f.id = t.flow_id"
	breakdownJoinParentFlowRun   = "LEFT JOIN flow_run fr ON fr.id = t.flow_run_id"
	breakdownJoinFlowOfParentRun = "LEFT JOIN flow f ON f.id = fr.flow_id"
)

// breakdownFlowNameKey groups by the name of the flow those joins reach.
var breakdownFlowNameKey = breakdownGroupKey{name: "flow_name", expr: "f.name"}

// breakdownLogLevelExpr names the standard Python log levels that Prefect
// stores as numbers in log.level, and keeps any other level as its number in
// text (data-model.md).
const breakdownLogLevelExpr = "CASE t.level WHEN 10 THEN 'DEBUG' WHEN 20 THEN 'INFO' WHEN 30 THEN 'WARNING'" +
	" WHEN 40 THEN 'ERROR' WHEN 50 THEN 'CRITICAL' ELSE t.level::text END"

// breakdownTables is the breakdown list, in the order the collector reads the
// tables (research R7, FR-005 to FR-007). The column names are those of
// Prefect 3.
var breakdownTables = []breakdownTableSpec{
	{
		table:           "events",
		keys:            []breakdownGroupKey{{name: "event_name", expr: "t.event"}},
		timestampColumn: "occurred",
	},
	{
		table:                 "flow_run",
		keys:                  []breakdownGroupKey{breakdownFlowNameKey, {name: "state_type", expr: "t.state_type"}},
		joins:                 []string{breakdownJoinFlowOfFlowRun},
		timestampColumn:       "start_time",
		countWithoutStartTime: true,
	},
	{
		table:                 "task_run",
		keys:                  []breakdownGroupKey{breakdownFlowNameKey, {name: "state_type", expr: "t.state_type"}},
		joins:                 []string{breakdownJoinParentFlowRun, breakdownJoinFlowOfParentRun},
		timestampColumn:       "start_time",
		countWithoutStartTime: true,
	},
	{
		table:           "flow_run_state",
		keys:            []breakdownGroupKey{{name: "state_type", expr: "t.type"}},
		timestampColumn: "timestamp",
	},
	{
		table:           "task_run_state",
		keys:            []breakdownGroupKey{{name: "state_type", expr: "t.type"}},
		timestampColumn: "timestamp",
	},
	{
		table:           "log",
		keys:            []breakdownGroupKey{breakdownFlowNameKey, {name: "log_level", expr: breakdownLogLevelExpr}},
		joins:           []string{breakdownJoinParentFlowRun, breakdownJoinFlowOfParentRun},
		timestampColumn: "timestamp",
	},
	{
		table:           "artifact",
		keys:            []breakdownGroupKey{{name: "artifact_type", expr: "t.type"}},
		timestampColumn: "created",
	},
}

// breakdownQuery builds the SQL that breaks one table down by its group keys:
// one JSON array with, for each group, its key values, its exact row count,
// for the run tables its rows without start_time, and the oldest and newest
// value of the timestamp column. The aggregation runs in a subquery because
// PostgreSQL cannot nest json_agg around count. A table without rows gives
// [], not NULL, because psql prints NULL as an empty line that is not JSON.
func breakdownQuery(spec breakdownTableSpec) string {
	keys := make([]string, len(spec.keys))
	groupBy := make([]string, len(spec.keys))
	for i, key := range spec.keys {
		keys[i] = fmt.Sprintf("'%s', %s", key.name, key.expr)
		groupBy[i] = key.expr
	}

	timestamp := "t." + spec.timestampColumn
	fields := []string{
		"'group', json_build_object(" + strings.Join(keys, ", ") + ")",
		"'rows', count(*)",
	}
	if spec.countWithoutStartTime {
		fields = append(fields, "'rows_without_start_time', count(*) FILTER (WHERE t.start_time IS NULL)")
	}
	fields = append(fields, "'oldest', min("+timestamp+")", "'newest', max("+timestamp+")")

	from := slices.Concat([]string{spec.table + " t"}, spec.joins)
	return "SELECT COALESCE(json_agg(b.g), '[]'::json) FROM (" +
		"SELECT json_build_object(" + strings.Join(fields, ", ") + ") AS g" +
		" FROM " + strings.Join(from, " ") +
		" GROUP BY " + strings.Join(groupBy, ", ") +
		") b"
}

// breakdownCatalogQuery builds the SQL that reports, for each table of
// breakdownTables, whether it exists in the task manager database (research
// R6), so a table that a Prefect version does not have is recorded as absent
// instead of failing (FR-012). to_regclass reads the catalog only and does not
// wait for a lock on the table. The query also checks that the database can be
// reached before the breakdown starts.
func breakdownCatalogQuery() string {
	fields := make([]string, len(breakdownTables))
	for i, spec := range breakdownTables {
		fields[i] = fmt.Sprintf("'%s', to_regclass('%s') IS NOT NULL", spec.table, spec.table)
	}
	return "SELECT json_build_object(" + strings.Join(fields, ", ") + ")"
}

// parseBreakdownCatalog turns the output of breakdownCatalogQuery into the set
// of breakdown tables that exist. An output that does not name every table on
// the breakdown list is an error, so a table is never taken as absent by
// mistake.
func parseBreakdownCatalog(stdout string) (map[string]bool, error) {
	var exists map[string]bool
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &exists); err != nil {
		return nil, fmt.Errorf("could not parse the storage breakdown catalog output: %w", err)
	}
	for _, spec := range breakdownTables {
		if _, ok := exists[spec.table]; !ok {
			return nil, fmt.Errorf("could not parse the storage breakdown catalog output: table %s is missing", spec.table)
		}
	}
	return exists, nil
}

// The status of one table in storage-breakdown.json (data-model.md).
const (
	breakdownTableCollected = "collected"
	breakdownTableAbsent    = "absent"
	breakdownTableFailed    = "failed"
)

// breakdownTableAbsentReason is the reason of a table that the deployed
// Prefect version does not have (FR-012).
const breakdownTableAbsentReason = "table does not exist in this Prefect version"

// breakdownGroup is one group of a table in storage-breakdown.json. Group has
// one value per key of grouped_by, null when the value is missing.
// RowsWithoutStartTime is set only for flow_run and task_run (FR-007).
type breakdownGroup struct {
	Group                map[string]*string `json:"group"`
	Rows                 int64              `json:"rows"`
	RowsWithoutStartTime *int64             `json:"rows_without_start_time,omitempty"`
	Oldest               *string            `json:"oldest"`
	Newest               *string            `json:"newest"`
}

// tableBreakdown is one table of storage-breakdown.json. Rows and Groups are
// set only when the table was collected, Reason only when it was not: a
// collected table without rows still has "rows": 0 and "groups": [].
type tableBreakdown struct {
	Name            string           `json:"name"`
	Status          string           `json:"status"`
	Reason          string           `json:"reason,omitempty"`
	GroupedBy       []string         `json:"grouped_by"`
	TimestampColumn string           `json:"timestamp_column"`
	Rows            *int64           `json:"rows,omitempty"`
	Groups          []breakdownGroup `json:"groups,omitzero"`
}

// storageBreakdown is the document written to
// task-manager-db/storage-breakdown.json (data-model.md,
// contracts/storage-breakdown.schema.json).
type storageBreakdown struct {
	CollectedAt       string           `json:"collected_at"`
	Database          string           `json:"database"`
	TimeLimitPerTable string           `json:"time_limit_per_table"`
	Tables            []tableBreakdown `json:"tables"`
}

// breakdownQueryGroup is one element of the JSON array breakdownQuery prints.
type breakdownQueryGroup struct {
	Group                map[string]*string `json:"group"`
	Rows                 int64              `json:"rows"`
	RowsWithoutStartTime *int64             `json:"rows_without_start_time"`
	Oldest               *time.Time         `json:"oldest"`
	Newest               *time.Time         `json:"newest"`
}

// parseBreakdownGroups turns the output of breakdownQuery into the groups of
// one table and their row total. A JSON null gives no groups. Timestamps are
// rendered as RFC 3339 UTC. The groups are ordered by rows, largest first,
// then by their key values in key-name order with null last, so two runs on
// the same data list them in the same order.
func parseBreakdownGroups(stdout string) ([]breakdownGroup, int64, error) {
	var output []breakdownQueryGroup
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &output); err != nil {
		return nil, 0, fmt.Errorf("could not parse the storage breakdown query output: %w", err)
	}

	groups := make([]breakdownGroup, 0, len(output))
	var total int64
	for _, g := range output {
		total += g.Rows
		groups = append(groups, breakdownGroup{
			Group:                g.Group,
			Rows:                 g.Rows,
			RowsWithoutStartTime: g.RowsWithoutStartTime,
			Oldest:               formatOptionalStorageTime(g.Oldest),
			Newest:               formatOptionalStorageTime(g.Newest),
		})
	}
	slices.SortFunc(groups, func(a, b breakdownGroup) int {
		if c := cmp.Compare(b.Rows, a.Rows); c != 0 {
			return c
		}
		return compareBreakdownKeys(a.Group, b.Group)
	})
	return groups, total, nil
}

// compareBreakdownKeys orders two groups by their key values, taking the keys
// in name order, with a null or missing value after every string.
func compareBreakdownKeys(a, b map[string]*string) int {
	names := slices.Collect(maps.Keys(a))
	for name := range b {
		if _, ok := a[name]; !ok {
			names = append(names, name)
		}
	}
	slices.Sort(names)

	for _, name := range names {
		av, bv := a[name], b[name]
		switch {
		case av == nil && bv == nil:
			continue
		case av == nil:
			return 1
		case bv == nil:
			return -1
		}
		if c := cmp.Compare(*av, *bv); c != 0 {
			return c
		}
	}
	return 0
}

// taskManagerDBStorageBreakdownCollector writes the storage breakdown of the
// task manager database to bundle/task-manager-db/storage-breakdown.json when
// the operator asks for it with --include-task-manager-storage (FR-004 to
// FR-007). Not being requested takes precedence over the service check, as
// for --benchmark; without a task-manager-db replica it is skipped (FR-010).
func taskManagerDBStorageBreakdownCollector() collector {
	return collector{
		name: "task-manager-db-storage-breakdown",
		skip: func(cc *collectContext) (bool, string) {
			if !cc.opts.IncludeTaskManagerStorage {
				return true, "not requested"
			}
			return skipWhenTaskManagerDBAbsent(cc)
		},
		run: collectTaskManagerDBStorageBreakdown,
	}
}

// collectTaskManagerDBStorageBreakdown runs breakdownCatalogQuery, then the
// breakdown query of each table on the breakdown list, each under its own
// limit that PostgreSQL enforces (FR-009), and writes every table's outcome to
// storage-breakdown.json (FR-016). A failed catalog query writes its
// diagnostics, with the password masked, to storage-breakdown.err.txt instead,
// and no table is tried: it fails for the same cause as every table would. The
// progress output names each table as the collector starts on it (FR-013).
func collectTaskManagerDBStorageBreakdown(cc *collectContext) error {
	dir := filepath.Join(cc.bundleDir, taskManagerDBBundleDir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create %s directory: %w", taskManagerDBBundleDir, err)
	}
	path := filepath.Join(dir, storageBreakdownFilename)
	creds := resolveTaskManagerDBCredentials(cc)
	mask := creds.passwordMask()

	collectedAt := time.Now()
	result, err := runTaskManagerDBQuery(cc, dbQuery{sql: breakdownCatalogQuery(), limit: cc.dbQueryLimit(collectExecTimeout)})
	if err != nil {
		if writeErr := writeDumpFailure(path, result.command, err, result.stdout, result.stderr, mask); writeErr != nil {
			logrus.Warnf("Could not record the failed storage breakdown catalog query: %v", writeErr)
		}
		return err
	}
	exists, err := parseBreakdownCatalog(result.stdout)
	if err != nil {
		queryErr := &dbQueryError{kind: dbQueryFailed, detail: err.Error()}
		if writeErr := writeDumpFailure(path, result.command, queryErr, result.stdout, result.stderr, mask); writeErr != nil {
			logrus.Warnf("Could not record the unreadable storage breakdown catalog output: %v", writeErr)
		}
		return queryErr
	}

	limit := cc.dbQueryLimit(collectDBQueryTimeout)
	breakdown := storageBreakdown{
		CollectedAt:       formatStorageTime(collectedAt),
		Database:          creds.database,
		TimeLimitPerTable: formatCommandTimeout(limit),
		Tables:            make([]tableBreakdown, 0, len(breakdownTables)),
	}
	failures := []string{}
	for _, spec := range breakdownTables {
		if err := cc.ctx.Err(); err != nil {
			return fmt.Errorf("storage breakdown interrupted before table %s: %w", spec.table, err)
		}
		logrus.Infof("Collecting storage breakdown of table %s (time limit %s)", spec.table, formatCommandTimeout(limit))

		table := breakdownTable(cc, spec, exists[spec.table], limit)
		if table.Status == breakdownTableFailed {
			failures = append(failures, spec.table+": "+table.Reason)
		}
		breakdown.Tables = append(breakdown.Tables, table)
	}

	data, err := json.MarshalIndent(breakdown, "", "    ")
	if err != nil {
		return fmt.Errorf("failed to marshal the storage breakdown: %w", err)
	}
	if err := writeDumpFile(path, string(data), nil); err != nil {
		return err
	}

	if len(failures) > 0 {
		// Deliberately not %w: wrapping a table's *timeoutError would make the
		// orchestrator replace this reason with "timed out after <limit>" and
		// drop the table names that support needs (research R11, FR-016).
		return fmt.Errorf("partial storage breakdown: %s", strings.Join(failures, "; "))
	}
	return nil
}

// breakdownTable breaks one table down and logs the outcome: absent when the
// catalog query did not find it (FR-012), else collected or failed with the
// reason of the query runner. The query result is the parsed JSON array and
// is never masked; the reason is masked by the runner.
func breakdownTable(cc *collectContext, spec breakdownTableSpec, exists bool, limit time.Duration) tableBreakdown {
	table := tableBreakdown{Name: spec.table, GroupedBy: spec.keyNames(), TimestampColumn: spec.timestampColumn}
	if !exists {
		table.Status, table.Reason = breakdownTableAbsent, breakdownTableAbsentReason
		logrus.Infof("Table %s: absent", spec.table)
		return table
	}

	started := time.Now()
	result, err := runTaskManagerDBQuery(cc, dbQuery{sql: breakdownQuery(spec), limit: limit, breakdown: true})
	var groups []breakdownGroup
	var rows int64
	if err == nil {
		if groups, rows, err = parseBreakdownGroups(result.stdout); err != nil {
			err = &dbQueryError{kind: dbQueryFailed, detail: err.Error()}
		}
	}
	if err != nil {
		table.Status, table.Reason = breakdownTableFailed, err.Error()
		logrus.Warnf("Table %s: %s", spec.table, table.Reason)
		return table
	}
	if result.stderr != "" {
		logrus.Debugf("stderr from the storage breakdown of table %s in %s: %s", spec.table, taskManagerDBService, result.stderr)
	}

	table.Status, table.Rows, table.Groups = breakdownTableCollected, &rows, groups
	logrus.Infof("Table %s: %d rows in %d groups (%s)", spec.table, rows, len(groups), time.Since(started).Round(time.Millisecond))
	return table
}
