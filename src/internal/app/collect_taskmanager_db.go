package app

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// Task manager database queries (specs/006-taskmgr-db-storage):
//   - the service and bundle directory, and the application name every session
//     reports to PostgreSQL, so an operator can find and stop it;
//   - the time limits: each breakdown table gets collectDBQueryTimeout and the
//     storage query collectExecTimeout. PostgreSQL enforces both, and the
//     tool-side bound adds collectDBQueryGrace so PostgreSQL's own timeout error
//     arrives first (research R3);
//   - dbQueryTimeoutEnvVar, which lowers both limits for the end-to-end tests
//     and is not documented for operators (contracts/cli.md);
//   - the skip reason when task-manager-db has no replica (FR-010): the task
//     manager may then use a PostgreSQL server outside the deployment, which the
//     tool cannot reach.
const (
	taskManagerDBService           = "task-manager-db"
	taskManagerDBBundleDir         = "task-manager-db"
	taskManagerDBAppName           = "infrahub-collect"
	collectDBQueryTimeout          = 5 * time.Minute
	collectDBQueryGrace            = 15 * time.Second
	dbQueryTimeoutEnvVar           = "INFRAHUB_TASK_MANAGER_DB_QUERY_TIMEOUT"
	taskManagerDBNotDeployedReason = "service not deployed; the task manager may use an external PostgreSQL server"
)

// Where resolveTaskManagerDBCredentials found the credentials. Only the source
// is ever logged, never a value.
const (
	dbCredentialsFromOperator    = "operator environment"
	dbCredentialsFromTaskManager = "task-manager environment"
	dbCredentialsFromDatabase    = "task-manager-db environment"
	dbCredentialsFromDefaults    = "defaults"
)

// taskManagerDBCredentials is the login the task manager database queries use,
// and where it was found.
type taskManagerDBCredentials struct {
	user     string
	password string
	database string
	source   string
}

// resolveTaskManagerDBCredentials returns the login for the task manager
// database, resolving it on the first call of a run and caching it on the
// collect context so every collector shares one lookup (critique E1). It tries,
// in order (research R8):
//  1. a Prefect connection URL in the operator's environment;
//  2. a Prefect connection URL in the task-manager container's environment;
//  3. POSTGRES_USER, POSTGRES_PASSWORD, and POSTGRES_DB or POSTGRES_DATABASE in
//     the task-manager-db container's environment, which still answers when the
//     Prefect server is down (critique X1);
//  4. the defaults the backup tool uses.
//
// It reads but never changes iops.config. The `env` output of both containers
// holds secrets: it is parsed here and never written to the bundle or the log.
func resolveTaskManagerDBCredentials(cc *collectContext) taskManagerDBCredentials {
	if cc.dbCredentials != nil {
		return *cc.dbCredentials
	}

	creds := lookupTaskManagerDBCredentials(cc)
	logrus.Debugf("Using task manager database credentials from the %s", creds.source)
	cc.dbCredentials = &creds
	return creds
}

func lookupTaskManagerDBCredentials(cc *collectContext) taskManagerDBCredentials {
	for _, name := range prefectConnectionEnvVars {
		if conn := os.Getenv(name); conn != "" {
			if creds, ok := credentialsFromPrefectConnection(conn, dbCredentialsFromOperator); ok {
				return creds
			}
			break
		}
	}

	if envOut, err := cc.execDumpTimeout(collectExecTimeout, "task-manager", []string{"env"}); err != nil {
		logrus.Debugf("Could not read the task-manager environment for database credentials: %v", err)
	} else if conn := prefectConnectionFromEnv(envOut); conn != "" {
		if creds, ok := credentialsFromPrefectConnection(conn, dbCredentialsFromTaskManager); ok {
			return creds
		}
	}

	if envOut, err := cc.execDumpTimeout(collectExecTimeout, taskManagerDBService, []string{"env"}); err != nil {
		logrus.Debugf("Could not read the %s environment for database credentials: %v", taskManagerDBService, err)
	} else if creds, ok := credentialsFromPostgresEnv(envOut); ok {
		return creds
	}

	return taskManagerDBCredentials{
		user:     defaultPostgresUsername,
		password: defaultPostgresPassword,
		database: defaultPostgresDatabase,
		source:   dbCredentialsFromDefaults,
	}
}

// credentialsFromPrefectConnection parses a Prefect connection URL found in
// source. A part the URL leaves out takes the default, as applyPostgresDefaults
// does for the backup tool.
func credentialsFromPrefectConnection(conn, source string) (taskManagerDBCredentials, bool) {
	user, password, database, ok := parsePrefectConnection(conn)
	if !ok {
		logrus.Debugf("Could not parse the Prefect database connection URL from the %s", source)
		return taskManagerDBCredentials{}, false
	}
	return taskManagerDBCredentials{
		user:     valueOrDefault(user, defaultPostgresUsername),
		password: valueOrDefault(password, defaultPostgresPassword),
		database: valueOrDefault(database, defaultPostgresDatabase),
		source:   source,
	}, true
}

// credentialsFromPostgresEnv reads the database owner from the `env` output of
// task-manager-db. The official image names the database POSTGRES_DB, the
// bitnami image POSTGRES_DATABASE. The login is used only when both the user
// and the password are set.
func credentialsFromPostgresEnv(envOutput string) (taskManagerDBCredentials, bool) {
	user := envOutputValue(envOutput, "POSTGRES_USER")
	password := envOutputValue(envOutput, "POSTGRES_PASSWORD")
	if user == "" || password == "" {
		logrus.Debugf("The %s environment does not name both a database user and a password", taskManagerDBService)
		return taskManagerDBCredentials{}, false
	}

	database := envOutputValue(envOutput, "POSTGRES_DB")
	if database == "" {
		database = envOutputValue(envOutput, "POSTGRES_DATABASE")
	}
	return taskManagerDBCredentials{
		user:     user,
		password: password,
		database: valueOrDefault(database, defaultPostgresDatabase),
		source:   dbCredentialsFromDatabase,
	}, true
}

// envOutputValue returns the value of the variable name in `env` output, or ""
// when it is not set.
func envOutputValue(envOutput, name string) string {
	for _, line := range strings.Split(envOutput, "\n") {
		if after, ok := strings.CutPrefix(line, name+"="); ok {
			return after
		}
	}
	return ""
}

func valueOrDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// dbQuery is one query against the task manager database: the SQL, the time
// limit PostgreSQL enforces on it, and whether it reads every row of a table,
// which turns on the breakdown planner settings (research R4).
type dbQuery struct {
	sql       string
	limit     time.Duration
	breakdown bool
}

// dbSessionParameterAllowlist names every session parameter the tool may set
// through PGOPTIONS. PostgreSQL rejects the whole connection when the login may
// not set one of them (research R2), so each name here must be settable by any
// user: check that on a database owner without superuser rights before adding
// one. A unit test fails when buildPsqlCommand sets a name missing here.
var dbSessionParameterAllowlist = []string{
	"default_transaction_read_only",
	"statement_timeout",
	"TimeZone",
	"enable_hashjoin",
	"enable_mergejoin",
	"max_parallel_workers_per_gather",
}

// buildPsqlCommand returns the psql command and environment for one query in
// task-manager-db (research R1, R2, R4). PostgreSQL enforces both the read-only
// transaction and the time limit through PGOPTIONS, so a query the tool stops
// waiting for still stops on the server. Breakdown queries also turn off hash
// joins, merge joins and parallel workers, so their joins use the primary-key
// index and never write temporary files to a disk that may be nearly full.
// The password travels in the environment only, never in the command.
func buildPsqlCommand(creds taskManagerDBCredentials, q dbQuery) ([]string, map[string]string) {
	command := []string{
		"psql", "-h", "localhost", "-U", creds.user, "-d", creds.database,
		"-X", "-q", "-A", "-t", "-v", "ON_ERROR_STOP=1", "-c", q.sql,
	}

	options := []string{
		"default_transaction_read_only=on",
		fmt.Sprintf("statement_timeout=%d", statementTimeoutMillis(q.limit)),
		"TimeZone=UTC",
	}
	if q.breakdown {
		options = append(options, "enable_hashjoin=off", "enable_mergejoin=off", "max_parallel_workers_per_gather=0")
	}

	env := map[string]string{
		"PGPASSWORD":        creds.password,
		"PGAPPNAME":         taskManagerDBAppName,
		"PGCONNECT_TIMEOUT": "10",
		"PGOPTIONS":         "-c " + strings.Join(options, " -c "),
	}
	return command, env
}

// statementTimeoutMillis renders a limit for statement_timeout, rounded up to a
// whole millisecond and never below 1: PostgreSQL reads 0 as no limit at all.
func statementTimeoutMillis(limit time.Duration) int64 {
	return max(1, (limit + time.Millisecond - 1).Milliseconds())
}

// effectiveDBQueryLimit returns the time limit of one query: base, or the value
// of dbQueryTimeoutEnvVar when it is shorter. The variable lets the end-to-end
// tests reach both limits in seconds and can never raise one
// (contracts/cli.md). A value that does not parse or is not positive is
// ignored with a warning.
func effectiveDBQueryLimit(base time.Duration, getenv func(string) string) time.Duration {
	value := strings.TrimSpace(getenv(dbQueryTimeoutEnvVar))
	if value == "" {
		return base
	}
	limit, err := time.ParseDuration(value)
	if err != nil || limit <= 0 {
		logrus.Warnf("Ignoring %s=%q: not a positive duration; using the %s limit", dbQueryTimeoutEnvVar, value, formatCommandTimeout(base))
		return base
	}
	return min(base, limit)
}

// dbQueryErrorKind names the cause of a failed task manager database query
// (research R9). It starts every manifest reason except a timeout's.
type dbQueryErrorKind string

const (
	dbQueryTimedOut      dbQueryErrorKind = "timed out"
	dbQueryPsqlMissing   dbQueryErrorKind = "psql missing"
	dbQueryLoginRejected dbQueryErrorKind = "login rejected"
	dbQueryNotReachable  dbQueryErrorKind = "not reachable"
	dbQueryFailed        dbQueryErrorKind = "query failed"
)

// dbQueryError is a failed task manager database query: its kind, the line of
// the error output that names the cause, and the limit PostgreSQL enforced.
type dbQueryError struct {
	kind   dbQueryErrorKind
	detail string
	limit  time.Duration
}

// Error returns the manifest reason: "timed out after <limit>", the format
// every collector uses for a timeout, or "<kind>: <detail>".
func (e *dbQueryError) Error() string {
	if e.kind == dbQueryTimedOut {
		return (&timeoutError{timeout: e.limit}).Error()
	}
	return string(e.kind) + ": " + e.detail
}

// Unwrap exposes a timeout as a *timeoutError, so the orchestrator and the
// aggregating helpers recognize it as they do for every other collector.
func (e *dbQueryError) Unwrap() error {
	if e.kind == dbQueryTimedOut {
		return &timeoutError{timeout: e.limit}
	}
	return nil
}

// exitCoder is the part of *exec.ExitError the classifier reads, so tests can
// stand in for a failed docker or kubectl exec.
type exitCoder interface {
	ExitCode() int
}

var _ exitCoder = (*exec.ExitError)(nil)

// execNoticePrefixes start the lines kubectl writes around the command's own
// error output: the container it picked in a multi-container pod, such as a
// CloudNativePG pod (critique E5), and the exit code it propagates. Neither
// names the cause, so the classifier never picks them as the detail.
var execNoticePrefixes = []string{"Defaulted container", "command terminated with exit code"}

// lineMarker matches one line of error output that contains every one of its
// parts. Parts are lower case and lines are compared in lower case, and a
// marker never matches across lines.
type lineMarker []string

func (m lineMarker) matches(line string) bool {
	lower := strings.ToLower(line)
	for _, part := range m {
		if !strings.Contains(lower, part) {
			return false
		}
	}
	return true
}

// Error output the classifier recognizes (research R2, R8, R9).
var (
	dbStatementTimeoutMarkers = []lineMarker{{"canceling statement due to statement timeout"}}
	// How the Docker runtime, busybox env and GNU env report a missing psql.
	dbPsqlMissingMarkers = []lineMarker{
		{"executable file not found"},
		{"can't execute 'psql'"},
		{"psql: not found"},
		{"'psql': no such file or directory"},
	}
	dbLoginRejectedMarkers = []lineMarker{
		{"password authentication failed"},
		{"no pg_hba.conf entry"},
		{`role "`, "does not exist"},
		{`database "`, "does not exist"},
	}
	// "timeout expired" is how psql reports PGCONNECT_TIMEOUT running out.
	dbNotReachableMarkers = []lineMarker{
		{"could not connect to server"},
		{"connection refused"},
		{"connection timed out"},
		{"timeout expired"},
		{"is not running"},
		{"no pods found"},
		{"pod", "not found"},
	}
	// A line with one of these, compared as is, reads as the error itself.
	dbErrorDetailMarkers = []string{"ERROR", "FATAL", "psql:", "error", "Error"}
)

// classifyDBQueryFailure turns a failed psql exec into a *dbQueryError (research
// R9). A tool-side *timeoutError and PostgreSQL's statement timeout both give
// the timeout kind with the PostgreSQL limit, so the reason is the same
// whichever side stopped the query. The detail is the last line of the error
// output that reads as an error, else its last line, else the Go error text.
// The caller masks the password in the detail.
func classifyDBQueryFailure(err error, stdout, stderr string, limit time.Duration) *dbQueryError {
	stderrLines, stdoutLines := errorOutputLines(stderr), errorOutputLines(stdout)
	// The error output is stderr; a runtime that writes its own errors to
	// stdout leaves stderr empty.
	output := stderrLines
	if len(output) == 0 {
		output = stdoutLines
	}

	evidence := slices.Concat(stderrLines, stdoutLines)
	if err != nil {
		evidence = append(evidence, err.Error())
	}
	return &dbQueryError{
		kind:   dbQueryFailureKind(err, evidence),
		detail: dbQueryFailureDetail(err, output),
		limit:  limit,
	}
}

func dbQueryFailureKind(err error, evidence []string) dbQueryErrorKind {
	var timeout *timeoutError
	if errors.As(err, &timeout) || anyLineMatches(evidence, dbStatementTimeoutMarkers) {
		return dbQueryTimedOut
	}
	var coded exitCoder
	if errors.As(err, &coded) && coded.ExitCode() == 127 && anyLineMatches(evidence, dbPsqlMissingMarkers) {
		return dbQueryPsqlMissing
	}
	if anyLineMatches(evidence, dbLoginRejectedMarkers) {
		return dbQueryLoginRejected
	}
	if anyLineMatches(evidence, dbNotReachableMarkers) {
		return dbQueryNotReachable
	}
	return dbQueryFailed
}

func dbQueryFailureDetail(err error, output []string) string {
	for i := len(output) - 1; i >= 0; i-- {
		if slices.ContainsFunc(dbErrorDetailMarkers, func(marker string) bool { return strings.Contains(output[i], marker) }) {
			return output[i]
		}
	}
	if len(output) > 0 {
		return output[len(output)-1]
	}
	if err != nil {
		return err.Error()
	}
	return "no error output"
}

// errorOutputLines returns the non-empty lines of one output stream, trimmed,
// without kubectl's notices.
func errorOutputLines(output string) []string {
	lines := []string{}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		isNotice := slices.ContainsFunc(execNoticePrefixes, func(prefix string) bool { return strings.HasPrefix(line, prefix) })
		if line == "" || isNotice {
			continue
		}
		lines = append(lines, line)
	}
	return lines
}

func anyLineMatches(lines []string, markers []lineMarker) bool {
	for _, line := range lines {
		if slices.ContainsFunc(markers, func(m lineMarker) bool { return m.matches(line) }) {
			return true
		}
	}
	return false
}

// maskSecretValue returns a mask that replaces every occurrence of secret with
// maskedValue (research R10). It adds value-based masking to the key-name
// masking of ADR 0004: on Kubernetes the exec carries the password as
// `env PGPASSWORD=<password>`, which error output can echo. An empty secret
// masks nothing.
func maskSecretValue(secret string) func(string) string {
	if secret == "" {
		return func(s string) string { return s }
	}
	return func(s string) string { return strings.ReplaceAll(s, secret, maskedValue) }
}

// dbQueryResult is what one task manager database query returns: the command,
// for the failure diagnostics, and both output streams. The password is masked
// in the command and stderr, and in stdout when the query failed.
type dbQueryResult struct {
	command []string
	stdout  string
	stderr  string
}

// runTaskManagerDBQuery runs one read-only query with psql inside
// task-manager-db (research R1 to R3). The exec is bounded at the PostgreSQL
// limit plus collectDBQueryGrace, so PostgreSQL normally stops the query and
// reports it first, and the bound still catches a hung docker or kubectl. A
// failure is returned as a *dbQueryError. The password is masked in the
// returned command, stderr and the error text, and in stdout when the query
// failed (FR-017).
func runTaskManagerDBQuery(cc *collectContext, q dbQuery) (dbQueryResult, error) {
	creds := resolveTaskManagerDBCredentials(cc)
	mask := maskSecretValue(creds.password)
	command, env := buildPsqlCommand(creds, q)

	result := dbQueryResult{command: make([]string, len(command))}
	for i, arg := range command {
		result.command[i] = mask(arg)
	}

	// Only the bounded exec is allowed on the collect path; there is no
	// fallback to the unbounded Exec.
	execer, ok := cc.backend.(separateExecer)
	if !ok {
		return result, &dbQueryError{
			kind:   dbQueryFailed,
			detail: fmt.Sprintf("%s environment does not support bounded exec for database queries", cc.backend.Name()),
			limit:  q.limit,
		}
	}

	stdout, stderr, err := execer.ExecSeparateContext(cc.ctx, q.limit+collectDBQueryGrace, taskManagerDBService, command, &ExecOptions{Env: env})
	result.stderr = mask(stderr)
	if err != nil {
		// A failed query's stdout goes to <file>.err.txt as diagnostics, where
		// it may echo the exec command line and with it the password.
		result.stdout = mask(stdout)
		// Classify the unmasked output, so a password that happens to be a word
		// of the error message cannot hide the cause, then mask the detail.
		queryErr := classifyDBQueryFailure(err, stdout, stderr, q.limit)
		queryErr.detail = mask(queryErr.detail)
		return result, queryErr
	}
	// A successful query's stdout is the query result the collectors parse, and
	// the command line never reaches it. Masking it would corrupt the result
	// when the password is a common word: with the default password "prefect",
	// the database name and event names such as prefect.flow-run.Completed.
	result.stdout = stdout
	return result, nil
}

// skipWhenTaskManagerDBAbsent is skipWhenServiceAbsent for task-manager-db,
// with a reason that points at the likely cause (FR-010): without the service,
// the task manager may use a PostgreSQL server outside the deployment, which
// the tool cannot reach. Enumeration errors leave the decision to run(), so the
// run records a real failure.
func skipWhenTaskManagerDBAbsent(cc *collectContext) (bool, string) {
	if skip, _ := skipWhenServiceAbsent(taskManagerDBService)(cc); skip {
		return true, taskManagerDBNotDeployedReason
	}
	return false, ""
}
