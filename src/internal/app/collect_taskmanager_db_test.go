package app

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"
)

// envExecResponse is what envExecBackend answers to `env` in one service.
type envExecResponse struct {
	output string
	err    error
}

// envExecBackend answers the bounded `env` exec per service and counts the
// calls, so the credential lookup can be driven without containers. A service
// without a scripted response fails as an absent container would.
type envExecBackend struct {
	fakeCollectBackend
	env      map[string]envExecResponse
	calls    map[string]int
	timeouts []time.Duration
}

func newEnvExecBackend(env map[string]envExecResponse) *envExecBackend {
	return &envExecBackend{
		fakeCollectBackend: *newFakeCollectBackend(),
		env:                env,
		calls:              map[string]int{},
	}
}

func (b *envExecBackend) ExecContext(ctx context.Context, timeout time.Duration, service string, command []string, opts *ExecOptions) (string, error) {
	if len(command) != 1 || command[0] != "env" {
		return "", errors.New("unexpected command " + strings.Join(command, " "))
	}
	b.calls[service]++
	b.timeouts = append(b.timeouts, timeout)
	response, ok := b.env[service]
	if !ok {
		return "", errors.New("no container found for service " + service)
	}
	return response.output, response.err
}

var _ contextExecer = (*envExecBackend)(nil)

// clearOperatorPrefectEnv hides any Prefect connection URL set in the
// environment that runs the tests.
func clearOperatorPrefectEnv(t *testing.T) {
	t.Helper()
	for _, name := range prefectConnectionEnvVars {
		t.Setenv(name, "")
	}
}

func TestResolveTaskManagerDBCredentials(t *testing.T) {
	execFailed := envExecResponse{err: errors.New("exit status 1")}
	taskManagerEnv := envExecResponse{output: "PATH=/usr/bin\nPREFECT_API_DATABASE_CONNECTION_URL=postgresql+asyncpg://tm-owner:tm-secret@task-manager-db:5432/tm-db\n"}
	databaseEnv := envExecResponse{output: "POSTGRES_USER=db-owner\nPOSTGRES_PASSWORD=db-secret\nPOSTGRES_DB=db-name\n"}

	tests := []struct {
		name        string
		operatorEnv map[string]string
		env         map[string]envExecResponse
		want        taskManagerDBCredentials
		wantCalls   map[string]int
	}{
		{
			name:        "operator environment wins",
			operatorEnv: map[string]string{"PREFECT_SERVER_DATABASE_CONNECTION_URL": "postgresql+asyncpg://op-owner:op-secret@db:5432/op-db"},
			env:         map[string]envExecResponse{"task-manager": taskManagerEnv, taskManagerDBService: databaseEnv},
			want:        taskManagerDBCredentials{user: "op-owner", password: "op-secret", database: "op-db", source: dbCredentialsFromOperator},
			wantCalls:   map[string]int{},
		},
		{
			name:        "operator Prefect 2 variable",
			operatorEnv: map[string]string{"PREFECT_API_DATABASE_CONNECTION_URL": "postgres://op2-owner:op2-secret@db:5432/op2-db"},
			env:         map[string]envExecResponse{"task-manager": taskManagerEnv},
			want:        taskManagerDBCredentials{user: "op2-owner", password: "op2-secret", database: "op2-db", source: dbCredentialsFromOperator},
			wantCalls:   map[string]int{},
		},
		{
			name:        "unparsable operator URL falls through to task-manager",
			operatorEnv: map[string]string{"PREFECT_SERVER_DATABASE_CONNECTION_URL": "postgres://op-owner:op-secret@db:badport/op-db"},
			env:         map[string]envExecResponse{"task-manager": taskManagerEnv},
			want:        taskManagerDBCredentials{user: "tm-owner", password: "tm-secret", database: "tm-db", source: dbCredentialsFromTaskManager},
			wantCalls:   map[string]int{"task-manager": 1},
		},
		{
			name:      "task-manager environment",
			env:       map[string]envExecResponse{"task-manager": taskManagerEnv, taskManagerDBService: databaseEnv},
			want:      taskManagerDBCredentials{user: "tm-owner", password: "tm-secret", database: "tm-db", source: dbCredentialsFromTaskManager},
			wantCalls: map[string]int{"task-manager": 1},
		},
		{
			name:      "task-manager URL without a database takes the default database",
			env:       map[string]envExecResponse{"task-manager": {output: "PREFECT_SERVER_DATABASE_CONNECTION_URL=postgresql+asyncpg://tm-owner:tm-secret@task-manager-db:5432\n"}},
			want:      taskManagerDBCredentials{user: "tm-owner", password: "tm-secret", database: defaultPostgresDatabase, source: dbCredentialsFromTaskManager},
			wantCalls: map[string]int{"task-manager": 1},
		},
		{
			name:      "task-manager exec fails: task-manager-db with POSTGRES_DB",
			env:       map[string]envExecResponse{"task-manager": execFailed, taskManagerDBService: databaseEnv},
			want:      taskManagerDBCredentials{user: "db-owner", password: "db-secret", database: "db-name", source: dbCredentialsFromDatabase},
			wantCalls: map[string]int{"task-manager": 1, taskManagerDBService: 1},
		},
		{
			name: "task-manager exec fails: task-manager-db with POSTGRES_DATABASE",
			env: map[string]envExecResponse{
				"task-manager":       execFailed,
				taskManagerDBService: {output: "POSTGRES_USER=prefect\nPOSTGRES_PASSWORD=helm-secret\nPOSTGRES_DATABASE=prefect-helm\n"},
			},
			want:      taskManagerDBCredentials{user: "prefect", password: "helm-secret", database: "prefect-helm", source: dbCredentialsFromDatabase},
			wantCalls: map[string]int{"task-manager": 1, taskManagerDBService: 1},
		},
		{
			name: "task-manager-db without a database name takes the database named after the user",
			env: map[string]envExecResponse{
				"task-manager":       execFailed,
				taskManagerDBService: {output: "POSTGRES_USER=db-owner\nPOSTGRES_PASSWORD=db-secret\n"},
			},
			want:      taskManagerDBCredentials{user: "db-owner", password: "db-secret", database: "db-owner", source: dbCredentialsFromDatabase},
			wantCalls: map[string]int{"task-manager": 1, taskManagerDBService: 1},
		},
		{
			name: "task-manager without a connection URL: task-manager-db",
			env: map[string]envExecResponse{
				"task-manager":       {output: "PATH=/usr/bin\nPREFECT_API_URL=http://localhost:4200/api\n"},
				taskManagerDBService: databaseEnv,
			},
			want:      taskManagerDBCredentials{user: "db-owner", password: "db-secret", database: "db-name", source: dbCredentialsFromDatabase},
			wantCalls: map[string]int{"task-manager": 1, taskManagerDBService: 1},
		},
		{
			name: "task-manager-db user without a password keeps the user and takes the default password",
			env: map[string]envExecResponse{
				"task-manager":       execFailed,
				taskManagerDBService: {output: "POSTGRES_USER=db-owner\nPOSTGRES_PASSWORD_FILE=/run/secrets/pg\nPOSTGRES_DB=db-name\n"},
			},
			want:      taskManagerDBCredentials{user: "db-owner", password: defaultPostgresPassword, database: "db-name", source: dbCredentialsFromDatabase},
			wantCalls: map[string]int{"task-manager": 1, taskManagerDBService: 1},
		},
		{
			name: "task-manager-db password without a user logs in as postgres to the postgres database",
			env: map[string]envExecResponse{
				"task-manager":       execFailed,
				taskManagerDBService: {output: "POSTGRES_PASSWORD=db-secret\n"},
			},
			want:      taskManagerDBCredentials{user: "postgres", password: "db-secret", database: "postgres", source: dbCredentialsFromDatabase},
			wantCalls: map[string]int{"task-manager": 1, taskManagerDBService: 1},
		},
		{
			name: "task-manager-db without a user or a password falls through to the defaults",
			env: map[string]envExecResponse{
				"task-manager":       execFailed,
				taskManagerDBService: {output: "POSTGRES_DB=db-name\n"},
			},
			want:      taskManagerDBCredentials{user: defaultPostgresUsername, password: defaultPostgresPassword, database: defaultPostgresDatabase, source: dbCredentialsFromDefaults},
			wantCalls: map[string]int{"task-manager": 1, taskManagerDBService: 1},
		},
		{
			name: "task-manager URL without a login takes the defaults, not the operator's PostgreSQL variables",
			operatorEnv: map[string]string{
				"PGUSER": "op-user", "PGPASSWORD": "op-pgpassword", "PGDATABASE": "op-database",
				"PGSERVICE": "op-service", "PGPASSFILE": "/nonexistent/pgpass",
			},
			env:       map[string]envExecResponse{"task-manager": {output: "PREFECT_SERVER_DATABASE_CONNECTION_URL=postgresql+asyncpg://task-manager-db:5432\n"}},
			want:      taskManagerDBCredentials{user: defaultPostgresUsername, password: defaultPostgresPassword, database: defaultPostgresDatabase, source: dbCredentialsFromTaskManager},
			wantCalls: map[string]int{"task-manager": 1},
		},
		{
			name:      "both execs fail: defaults",
			env:       map[string]envExecResponse{"task-manager": execFailed, taskManagerDBService: execFailed},
			want:      taskManagerDBCredentials{user: defaultPostgresUsername, password: defaultPostgresPassword, database: defaultPostgresDatabase, source: dbCredentialsFromDefaults},
			wantCalls: map[string]int{"task-manager": 1, taskManagerDBService: 1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearOperatorPrefectEnv(t)
			for name, value := range tt.operatorEnv {
				t.Setenv(name, value)
			}
			backend := newEnvExecBackend(tt.env)
			cc := &collectContext{ctx: context.Background(), backend: backend, bundleDir: t.TempDir()}

			if got := resolveTaskManagerDBCredentials(cc); got != tt.want {
				t.Errorf("resolveTaskManagerDBCredentials() = %+v, want %+v", got, tt.want)
			}
			if !maps.Equal(backend.calls, tt.wantCalls) {
				t.Errorf("env exec calls = %v, want %v", backend.calls, tt.wantCalls)
			}
			for i, timeout := range backend.timeouts {
				if timeout != collectExecTimeout {
					t.Errorf("env exec %d bounded by %s, want %s", i, timeout, collectExecTimeout)
				}
			}
		})
	}
}

// TestResolveTaskManagerDBCredentials_ResolvesOncePerRun covers critique E1:
// both collectors resolve the credentials, and a run looks them up once.
func TestResolveTaskManagerDBCredentials_ResolvesOncePerRun(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]envExecResponse
	}{
		{
			name: "found in task-manager",
			env: map[string]envExecResponse{
				"task-manager": {output: "PREFECT_SERVER_DATABASE_CONNECTION_URL=postgres://tm-owner:tm-secret@db:5432/prefect\n"},
			},
		},
		{
			name: "every step tried before the defaults",
			env: map[string]envExecResponse{
				"task-manager":       {err: errors.New("exit status 1")},
				taskManagerDBService: {output: "PATH=/usr/bin\n"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearOperatorPrefectEnv(t)
			backend := newEnvExecBackend(tt.env)
			cc := &collectContext{ctx: context.Background(), backend: backend, bundleDir: t.TempDir()}

			first := resolveTaskManagerDBCredentials(cc)
			second := resolveTaskManagerDBCredentials(cc)

			if first != second {
				t.Errorf("second call = %+v, want the cached %+v", second, first)
			}
			for service, calls := range backend.calls {
				if calls > 1 {
					t.Errorf("env ran %d times in %s, want at most once", calls, service)
				}
			}
			if cc.dbCredentials == nil || *cc.dbCredentials != first {
				t.Errorf("cached credentials = %v, want %+v", cc.dbCredentials, first)
			}
		})
	}
}

// TestResolveTaskManagerDBCredentials_LogsSourceOnly checks that the debug log
// names where the credentials came from and holds no credential value, even
// when a step fails or falls through.
func TestResolveTaskManagerDBCredentials_LogsSourceOnly(t *testing.T) {
	clearOperatorPrefectEnv(t)
	t.Setenv("PREFECT_SERVER_DATABASE_CONNECTION_URL", "postgres://op-owner:op-secret@db:badport/op-db")
	backend := newEnvExecBackend(map[string]envExecResponse{
		"task-manager":       {output: "PREFECT_API_DATABASE_CONNECTION_URL=postgres://tm-owner:tm-secret@db:badport/tm-db\n"},
		taskManagerDBService: {output: "POSTGRES_USER=db-owner\nPOSTGRES_PASSWORD=db-secret\nPOSTGRES_DB=db-name\n"},
	})
	cc := &collectContext{ctx: context.Background(), backend: backend, bundleDir: t.TempDir()}

	var creds taskManagerDBCredentials
	logs := captureLogrus(t, func() { creds = resolveTaskManagerDBCredentials(cc) })

	if creds.source != dbCredentialsFromDatabase {
		t.Fatalf("source = %q, want %q", creds.source, dbCredentialsFromDatabase)
	}
	if !strings.Contains(logs, dbCredentialsFromDatabase) {
		t.Errorf("debug log does not name the source %q:\n%s", dbCredentialsFromDatabase, logs)
	}
	for _, value := range []string{"op-owner", "op-secret", "op-db", "tm-owner", "tm-secret", "tm-db", "db-owner", "db-secret", "db-name"} {
		if strings.Contains(logs, value) {
			t.Errorf("debug log contains the credential value %q:\n%s", value, logs)
		}
	}
}

// TestResolveTaskManagerDBCredentials_WarnsOnUnparsableURL covers a Prefect
// connection URL that does not parse because its password holds an unencoded
// "#" or "/": the lookup warns, naming the variable and where it was found but
// not the URL or the password, and moves on to the next source.
func TestResolveTaskManagerDBCredentials_WarnsOnUnparsableURL(t *testing.T) {
	taskManagerEnv := envExecResponse{output: "PREFECT_SERVER_DATABASE_CONNECTION_URL=postgres://tm-owner:tm-secret@task-manager-db:5432/tm-db\n"}
	databaseEnv := envExecResponse{output: "POSTGRES_USER=db-owner\nPOSTGRES_PASSWORD=db-secret\nPOSTGRES_DB=db-name\n"}

	tests := []struct {
		name     string
		variable string
		password string
		// source is where the URL that does not parse is set, and wantSource
		// the next source, which then provides the credentials.
		source     string
		wantSource string
	}{
		{
			name:       "operator environment, Prefect 3 variable",
			variable:   "PREFECT_SERVER_DATABASE_CONNECTION_URL",
			password:   unparsablePrefectPassword,
			source:     dbCredentialsFromOperator,
			wantSource: dbCredentialsFromTaskManager,
		},
		{
			name:       "operator environment, Prefect 2 variable",
			variable:   "PREFECT_API_DATABASE_CONNECTION_URL",
			password:   "Kx9q/Wz3v",
			source:     dbCredentialsFromOperator,
			wantSource: dbCredentialsFromTaskManager,
		},
		{
			name:       "task-manager environment, Prefect 3 variable",
			variable:   "PREFECT_SERVER_DATABASE_CONNECTION_URL",
			password:   "Kx9q/Wz3v",
			source:     dbCredentialsFromTaskManager,
			wantSource: dbCredentialsFromDatabase,
		},
		{
			name:       "task-manager environment, Prefect 2 variable",
			variable:   "PREFECT_API_DATABASE_CONNECTION_URL",
			password:   unparsablePrefectPassword,
			source:     dbCredentialsFromTaskManager,
			wantSource: dbCredentialsFromDatabase,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			url := unparsablePrefectURL(tt.password)
			clearOperatorPrefectEnv(t)
			env := map[string]envExecResponse{"task-manager": taskManagerEnv, taskManagerDBService: databaseEnv}
			if tt.source == dbCredentialsFromOperator {
				t.Setenv(tt.variable, url)
			} else {
				env["task-manager"] = envExecResponse{output: "PATH=/usr/bin\n" + tt.variable + "=" + url + "\n"}
			}
			backend := newEnvExecBackend(env)
			cc := &collectContext{ctx: context.Background(), backend: backend, bundleDir: t.TempDir()}
			hook := captureLogs(t)

			if got := resolveTaskManagerDBCredentials(cc); got.source != tt.wantSource {
				t.Errorf("credentials from the %s, want the %s after the URL that does not parse", got.source, tt.wantSource)
			}
			assertUnparsableURLWarning(t, warningMessages(hook), tt.variable, tt.source, url, tt.password)
		})
	}
}

func TestBuildPsqlCommand(t *testing.T) {
	creds := taskManagerDBCredentials{user: "tm-owner", password: "tm-secret", database: "tm-db", source: dbCredentialsFromTaskManager}
	readOnlyOptions := "-c default_transaction_read_only=on -c statement_timeout=%d -c TimeZone=UTC"
	plannerOptions := " -c enable_hashjoin=off -c enable_mergejoin=off -c max_parallel_workers_per_gather=0"

	tests := []struct {
		name          string
		query         dbQuery
		wantPGOptions string
	}{
		{
			name:          "60-second storage query",
			query:         dbQuery{sql: "SELECT 1", limit: collectExecTimeout},
			wantPGOptions: fmt.Sprintf(readOnlyOptions, 60000),
		},
		{
			name:          "5-minute breakdown query turns off hash joins, merge joins and parallel workers",
			query:         dbQuery{sql: "SELECT 1", limit: collectDBQueryTimeout, breakdown: true},
			wantPGOptions: fmt.Sprintf(readOnlyOptions, 300000) + plannerOptions,
		},
		{
			name:          "5-minute query without the breakdown settings",
			query:         dbQuery{sql: "SELECT 1", limit: collectDBQueryTimeout},
			wantPGOptions: fmt.Sprintf(readOnlyOptions, 300000),
		},
		{
			name:          "a sub-millisecond limit rounds up, never to 0, which PostgreSQL reads as no limit",
			query:         dbQuery{sql: "SELECT 1", limit: time.Microsecond},
			wantPGOptions: fmt.Sprintf(readOnlyOptions, 1),
		},
		{
			name:          "a fractional limit rounds up to the next millisecond",
			query:         dbQuery{sql: "SELECT 1", limit: 1500 * time.Microsecond},
			wantPGOptions: fmt.Sprintf(readOnlyOptions, 2),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			command, env, stdin := buildPsqlCommand(creds, tt.query)

			wantCommand := []string{
				"sh", "-c", psqlWithPasswordFromStdin, "sh",
				"-h", "localhost", "-U", "tm-owner", "-d", "tm-db",
				"-X", "-q", "-A", "-t", "-v", "ON_ERROR_STOP=1", "-c", tt.query.sql,
			}
			if !slices.Equal(command, wantCommand) {
				t.Errorf("command = %q, want %q", command, wantCommand)
			}

			wantEnv := map[string]string{
				"PGAPPNAME":         "infrahub-collect",
				"PGCONNECT_TIMEOUT": "10",
				"PGOPTIONS":         tt.wantPGOptions,
			}
			if !maps.Equal(env, wantEnv) {
				t.Errorf("env = %q, want %q", env, wantEnv)
			}

			// docker and kubectl pass the command and env as arguments, which
			// other users of the host can read: the password goes on stdin.
			if stdin != "tm-secret\n" {
				t.Errorf("stdin = %q, want the password on its first line", stdin)
			}
			for _, value := range slices.Concat(command, slices.Collect(maps.Values(env))) {
				if strings.Contains(value, creds.password) {
					t.Errorf("argument or variable %q carries the password; it belongs on stdin only", value)
				}
			}

			for _, name := range pgOptionsParameterNames(t, env["PGOPTIONS"]) {
				if !slices.Contains(dbSessionParameterAllowlist, name) {
					t.Errorf("PGOPTIONS sets %q, which is not in dbSessionParameterAllowlist: a parameter the owner may not set makes the connection fail (research R2)", name)
				}
			}
		})
	}
}

// pgOptionsParameterNames returns the parameter names of a PGOPTIONS value,
// failing the test unless it is a list of `-c name=value` pairs.
func pgOptionsParameterNames(t *testing.T, pgOptions string) []string {
	t.Helper()
	fields := strings.Fields(pgOptions)
	if len(fields)%2 != 0 {
		t.Fatalf("PGOPTIONS %q is not a list of -c name=value pairs", pgOptions)
	}
	names := []string{}
	for i := 0; i < len(fields); i += 2 {
		name, _, ok := strings.Cut(fields[i+1], "=")
		if fields[i] != "-c" || !ok {
			t.Fatalf("PGOPTIONS %q holds %q %q, want -c name=value", pgOptions, fields[i], fields[i+1])
		}
		names = append(names, name)
	}
	return names
}

func TestDBQueryLimit(t *testing.T) {
	tests := []struct {
		value    string
		want     map[time.Duration]time.Duration // base -> limit
		wantWarn bool
	}{
		{value: "", want: map[time.Duration]time.Duration{collectExecTimeout: collectExecTimeout, collectDBQueryTimeout: collectDBQueryTimeout}},
		{value: "5s", want: map[time.Duration]time.Duration{collectExecTimeout: 5 * time.Second, collectDBQueryTimeout: 5 * time.Second}},
		{value: "90s", want: map[time.Duration]time.Duration{collectExecTimeout: collectExecTimeout, collectDBQueryTimeout: 90 * time.Second}},
		{value: "10m", want: map[time.Duration]time.Duration{collectExecTimeout: collectExecTimeout, collectDBQueryTimeout: collectDBQueryTimeout}},
		{value: "garbage", want: map[time.Duration]time.Duration{collectExecTimeout: collectExecTimeout, collectDBQueryTimeout: collectDBQueryTimeout}, wantWarn: true},
		{value: "0", want: map[time.Duration]time.Duration{collectExecTimeout: collectExecTimeout, collectDBQueryTimeout: collectDBQueryTimeout}, wantWarn: true},
		{value: "-5s", want: map[time.Duration]time.Duration{collectExecTimeout: collectExecTimeout, collectDBQueryTimeout: collectDBQueryTimeout}, wantWarn: true},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s=%q", dbQueryTimeoutEnvVar, tt.value), func(t *testing.T) {
			t.Setenv(dbQueryTimeoutEnvVar, tt.value)
			cc := &collectContext{ctx: context.Background()}
			hook := captureLogs(t)

			// A run asks for each limit more than once, as the collectors do.
			for _, base := range []time.Duration{collectExecTimeout, collectExecTimeout, collectDBQueryTimeout} {
				if got := cc.dbQueryLimit(base); got != tt.want[base] {
					t.Errorf("dbQueryLimit(%s) = %s, want %s", base, got, tt.want[base])
				}
			}

			warnings := 0
			for _, message := range warningMessages(hook) {
				if strings.Contains(message, dbQueryTimeoutEnvVar) {
					warnings++
				}
			}
			if want := map[bool]int{false: 0, true: 1}[tt.wantWarn]; warnings != want {
				t.Errorf("%d warnings about %s, want %d: %q", warnings, dbQueryTimeoutEnvVar, want, warningMessages(hook))
			}
		})
	}
}

// exitCodeError stands in for the *exec.ExitError a failed docker or kubectl
// exec returns.
type exitCodeError struct{ code int }

func (e exitCodeError) Error() string { return fmt.Sprintf("exit status %d", e.code) }
func (e exitCodeError) ExitCode() int { return e.code }

// cnpgNotice is the line kubectl writes to stderr before the command's own
// output when it execs into a CloudNativePG pod (critique E5).
const cnpgNotice = `Defaulted container "postgres" out of: postgres, bootstrap-controller (init)`

// TestClassifyDBQueryFailure drives the classifier with the error output
// recorded in research R2, R8 and R9, each case also preceded by kubectl's
// container notice, which must never be picked as the cause.
func TestClassifyDBQueryFailure(t *testing.T) {
	const limit = 60 * time.Second
	statementTimeout := "ERROR:  canceling statement due to statement timeout"
	dashPsqlMissing := "sh: 1: exec: psql: not found"
	busyboxPsqlMissing := "sh: exec: line 0: psql: not found"
	dockerShMissing := `OCI runtime exec failed: exec failed: unable to start container process: exec: "sh": executable file not found in $PATH: unknown`
	roleMissing := `psql: error: connection to server at "localhost" (::1), port 5432 failed: FATAL:  role "nobody" does not exist`
	badPassword := `psql: error: connection to server at "localhost" (127.0.0.1), port 5432 failed: FATAL:  password authentication failed for user "prefect"`
	noHBAEntry := `psql: error: connection to server at "localhost" (::1), port 5432 failed: FATAL:  no pg_hba.conf entry for host "::1", user "prefect", database "prefect", no encryption`
	refused := `psql: error: connection to server at "localhost" (::1), port 5432 failed: Connection refused`
	notRunning := "Error response from daemon: container 3f2a9c1b7d4e is not running"
	unknownTable := `ERROR:  relation "events" does not exist`

	tests := []struct {
		name       string
		err        error
		stdout     string
		stderr     string
		wantKind   dbQueryErrorKind
		wantDetail string
		wantError  string
	}{
		{
			name:       "PostgreSQL statement timeout",
			err:        exitCodeError{code: 1},
			stderr:     statementTimeout,
			wantKind:   dbQueryTimedOut,
			wantDetail: statementTimeout,
			wantError:  "timed out after 60s",
		},
		{
			name:       "PostgreSQL statement timeout through kubectl",
			err:        exitCodeError{code: 1},
			stderr:     statementTimeout + "\ncommand terminated with exit code 1",
			wantKind:   dbQueryTimedOut,
			wantDetail: statementTimeout,
			wantError:  "timed out after 60s",
		},
		{
			name:       "tool-side bound reports the PostgreSQL limit",
			err:        &timeoutError{timeout: limit + collectDBQueryGrace},
			wantKind:   dbQueryTimedOut,
			wantDetail: "timed out after 75s",
			wantError:  "timed out after 60s",
		},
		{
			name:       "dash: psql not in the image",
			err:        exitCodeError{code: 127},
			stderr:     dashPsqlMissing,
			wantKind:   dbQueryPsqlMissing,
			wantDetail: dashPsqlMissing,
			wantError:  "psql missing: " + dashPsqlMissing,
		},
		{
			name:       "bash: psql not in the image",
			err:        exitCodeError{code: 127},
			stderr:     "sh: line 1: exec: psql: not found",
			wantKind:   dbQueryPsqlMissing,
			wantDetail: "sh: line 1: exec: psql: not found",
			wantError:  "psql missing: sh: line 1: exec: psql: not found",
		},
		{
			name:       "Kubernetes: busybox psql not in the image",
			err:        exitCodeError{code: 127},
			stderr:     busyboxPsqlMissing + "\ncommand terminated with exit code 127",
			wantKind:   dbQueryPsqlMissing,
			wantDetail: busyboxPsqlMissing,
			wantError:  "psql missing: " + busyboxPsqlMissing,
		},
		{
			name:       "psql missing needs exit code 127",
			err:        exitCodeError{code: 1},
			stderr:     dashPsqlMissing,
			wantKind:   dbQueryFailed,
			wantDetail: dashPsqlMissing,
			wantError:  "query failed: " + dashPsqlMissing,
		},
		{
			name:       "Docker: sh not in the image is not reported as psql missing",
			err:        exitCodeError{code: 127},
			stdout:     dockerShMissing,
			wantKind:   dbQueryFailed,
			wantDetail: dockerShMissing,
			wantError:  "query failed: " + dockerShMissing,
		},
		{
			name:       "unknown role",
			err:        exitCodeError{code: 2},
			stderr:     roleMissing,
			wantKind:   dbQueryLoginRejected,
			wantDetail: roleMissing,
			wantError:  "login rejected: " + roleMissing,
		},
		{
			name:       "wrong password",
			err:        exitCodeError{code: 2},
			stderr:     badPassword,
			wantKind:   dbQueryLoginRejected,
			wantDetail: badPassword,
			wantError:  "login rejected: " + badPassword,
		},
		{
			name:       "unknown database",
			err:        exitCodeError{code: 2},
			stderr:     `psql: error: connection to server at "localhost" (::1), port 5432 failed: FATAL:  database "tm-db" does not exist`,
			wantKind:   dbQueryLoginRejected,
			wantDetail: `psql: error: connection to server at "localhost" (::1), port 5432 failed: FATAL:  database "tm-db" does not exist`,
			wantError:  `login rejected: psql: error: connection to server at "localhost" (::1), port 5432 failed: FATAL:  database "tm-db" does not exist`,
		},
		{
			name:       "no pg_hba.conf entry",
			err:        exitCodeError{code: 2},
			stderr:     noHBAEntry,
			wantKind:   dbQueryLoginRejected,
			wantDetail: noHBAEntry,
			wantError:  "login rejected: " + noHBAEntry,
		},
		{
			name:       "connection refused, with psql's hint line after it",
			err:        exitCodeError{code: 2},
			stderr:     refused + "\n\tIs the server running on that host and accepting TCP/IP connections?",
			wantKind:   dbQueryNotReachable,
			wantDetail: refused,
			wantError:  "not reachable: " + refused,
		},
		{
			name:       "container not running",
			err:        exitCodeError{code: 1},
			stderr:     notRunning,
			wantKind:   dbQueryNotReachable,
			wantDetail: notRunning,
			wantError:  "not reachable: " + notRunning,
		},
		{
			name:       "no pod for the service",
			err:        errors.New("no pods found for service task-manager-db in namespace infrahub"),
			wantKind:   dbQueryNotReachable,
			wantDetail: "no pods found for service task-manager-db in namespace infrahub",
			wantError:  "not reachable: no pods found for service task-manager-db in namespace infrahub",
		},
		{
			name:       "pod gone between resolution and exec",
			err:        exitCodeError{code: 1},
			stderr:     `Error from server (NotFound): pods "task-manager-db-1" not found`,
			wantKind:   dbQueryNotReachable,
			wantDetail: `Error from server (NotFound): pods "task-manager-db-1" not found`,
			wantError:  `not reachable: Error from server (NotFound): pods "task-manager-db-1" not found`,
		},
		{
			name:       "unknown error: a missing relation is not a rejected login",
			err:        exitCodeError{code: 1},
			stderr:     unknownTable,
			wantKind:   dbQueryFailed,
			wantDetail: unknownTable,
			wantError:  "query failed: " + unknownTable,
		},
		{
			name:       "unknown error without a marker line takes the last line",
			err:        exitCodeError{code: 3},
			stderr:     "first line\nsomething went wrong",
			wantKind:   dbQueryFailed,
			wantDetail: "something went wrong",
			wantError:  "query failed: something went wrong",
		},
		{
			name:       "unknown error without output takes the Go error",
			err:        exitCodeError{code: 3},
			wantKind:   dbQueryFailed,
			wantDetail: "exit status 3",
			wantError:  "query failed: exit status 3",
		},
	}

	for _, tt := range tests {
		for _, prefix := range []string{"", cnpgNotice + "\n"} {
			name := tt.name
			if prefix != "" {
				name += " after the kubectl container notice"
			}
			t.Run(name, func(t *testing.T) {
				got := classifyDBQueryFailure(tt.err, tt.stdout, prefix+tt.stderr, limit)

				if got.kind != tt.wantKind {
					t.Errorf("kind = %q, want %q", got.kind, tt.wantKind)
				}
				if got.detail != tt.wantDetail {
					t.Errorf("detail = %q, want %q", got.detail, tt.wantDetail)
				}
				if got.Error() != tt.wantError {
					t.Errorf("Error() = %q, want %q", got.Error(), tt.wantError)
				}

				var timeout *timeoutError
				isTimeout := errors.As(error(got), &timeout)
				if isTimeout != (tt.wantKind == dbQueryTimedOut) {
					t.Errorf("errors.As(*timeoutError) = %v, want %v", isTimeout, tt.wantKind == dbQueryTimedOut)
				}
				if isTimeout && timeout.timeout != limit {
					t.Errorf("unwrapped timeout = %s, want the PostgreSQL limit %s", timeout.timeout, limit)
				}
			})
		}
	}
}

func TestMaskSecretValue(t *testing.T) {
	tests := []struct {
		name   string
		secret string
		input  string
		want   string
	}{
		{name: "every occurrence", secret: "s3cret", input: "env PGPASSWORD=s3cret psql; s3cret again", want: "env PGPASSWORD=******** psql; ******** again"},
		{name: "no occurrence", secret: "s3cret", input: "nothing to hide", want: "nothing to hide"},
		{name: "empty secret masks nothing", secret: "", input: "env PGPASSWORD= psql", want: "env PGPASSWORD= psql"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := maskSecretValue(tt.secret)(tt.input); got != tt.want {
				t.Errorf("maskSecretValue(%q)(%q) = %q, want %q", tt.secret, tt.input, got, tt.want)
			}
		})
	}
}

// TestPasswordMask checks that a password equal to the user or the database
// name is left as is, so the reason of a failed query still names them.
func TestPasswordMask(t *testing.T) {
	const missingDatabase = `FATAL:  database "prefect" does not exist; role "postgres"; password s3cret`
	tests := []struct {
		name  string
		creds taskManagerDBCredentials
		want  string
	}{
		{
			name:  "distinct password",
			creds: taskManagerDBCredentials{user: "postgres", password: "s3cret", database: "prefect"},
			want:  `FATAL:  database "prefect" does not exist; role "postgres"; password ********`,
		},
		{
			name:  "default login: the password is the database name",
			creds: taskManagerDBCredentials{user: defaultPostgresUsername, password: defaultPostgresPassword, database: defaultPostgresDatabase},
			want:  missingDatabase,
		},
		{
			name:  "Docker Compose login: the password is the user name",
			creds: taskManagerDBCredentials{user: "postgres", password: "postgres", database: "prefect"},
			want:  missingDatabase,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.creds.passwordMask()(missingDatabase); got != tt.want {
				t.Errorf("passwordMask()(%q) = %q, want %q", missingDatabase, got, tt.want)
			}
		})
	}
}

// dbQueryCall is one ExecSeparateInputContext call that dbQueryBackend
// received.
type dbQueryCall struct {
	timeout time.Duration
	service string
	command []string
	opts    *ExecOptions
	input   string
}

// dbQueryBackend answers the bounded, separated-stream exec with stdin through
// respond and records every call, standing in for docker or kubectl exec into
// task-manager-db. respond can answer by the SQL, the last argument of the
// psql command.
type dbQueryBackend struct {
	fakeCollectBackend
	respond func(call dbQueryCall) (stdout, stderr string, err error)
	calls   []dbQueryCall
}

func (b *dbQueryBackend) ExecSeparateInputContext(ctx context.Context, timeout time.Duration, service string, command []string, opts *ExecOptions, input string) (string, string, error) {
	call := dbQueryCall{timeout: timeout, service: service, command: command, opts: opts, input: input}
	b.calls = append(b.calls, call)
	return b.respond(call)
}

var _ inputExecer = (*dbQueryBackend)(nil)

// newDBQueryContext returns a collect context whose task manager database
// credentials are already resolved, so no env lookup runs.
func newDBQueryContext(t *testing.T, backend EnvironmentBackend, creds taskManagerDBCredentials) *collectContext {
	t.Helper()
	return &collectContext{ctx: context.Background(), backend: backend, bundleDir: t.TempDir(), dbCredentials: &creds}
}

func TestRunTaskManagerDBQuery_ExecArguments(t *testing.T) {
	creds := taskManagerDBCredentials{user: "tm-owner", password: "tm-secret", database: "tm-db", source: dbCredentialsFromTaskManager}
	tests := []struct {
		name      string
		query     dbQuery
		wantBound time.Duration
	}{
		{name: "storage query", query: dbQuery{sql: "SELECT 1", limit: collectExecTimeout}, wantBound: 75 * time.Second},
		{name: "breakdown query", query: dbQuery{sql: "SELECT 2", limit: collectDBQueryTimeout, breakdown: true}, wantBound: 315 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := &dbQueryBackend{
				fakeCollectBackend: *newFakeCollectBackend(),
				respond: func(dbQueryCall) (string, string, error) {
					return `{"database" : "tm-db"}`, "", nil
				},
			}
			cc := newDBQueryContext(t, backend, creds)

			result, err := runTaskManagerDBQuery(cc, tt.query)
			if err != nil {
				t.Fatalf("runTaskManagerDBQuery failed: %v", err)
			}
			if len(backend.calls) != 1 {
				t.Fatalf("exec calls = %d, want 1", len(backend.calls))
			}

			call := backend.calls[0]
			wantCommand, wantEnv, wantStdin := buildPsqlCommand(creds, tt.query)
			if call.service != taskManagerDBService {
				t.Errorf("service = %q, want %q", call.service, taskManagerDBService)
			}
			if call.timeout != tt.wantBound {
				t.Errorf("exec bound = %s, want the limit plus %s: %s", call.timeout, collectDBQueryGrace, tt.wantBound)
			}
			if !slices.Equal(call.command, wantCommand) {
				t.Errorf("command = %q, want %q", call.command, wantCommand)
			}
			if call.opts == nil || !maps.Equal(call.opts.Env, wantEnv) {
				t.Errorf("exec options = %+v, want the environment %q", call.opts, wantEnv)
			}
			if call.input != wantStdin {
				t.Errorf("stdin = %q, want %q", call.input, wantStdin)
			}
			if call.opts != nil && call.opts.User != "" {
				t.Errorf("exec user = %q, want none", call.opts.User)
			}

			if !slices.Equal(result.command, wantCommand) {
				t.Errorf("result command = %q, want %q", result.command, wantCommand)
			}
			if result.stdout != `{"database" : "tm-db"}` || result.stderr != "" {
				t.Errorf("result streams = %q / %q, want the backend's stdout and no stderr", result.stdout, result.stderr)
			}
		})
	}
}

func TestRunTaskManagerDBQuery_Timeouts(t *testing.T) {
	creds := taskManagerDBCredentials{user: "tm-owner", password: "tm-secret", database: "tm-db"}
	tests := []struct {
		name   string
		limit  time.Duration
		stderr string
		err    error
		want   string
	}{
		{
			name:   "PostgreSQL stops the storage query",
			limit:  collectExecTimeout,
			stderr: "ERROR:  canceling statement due to statement timeout",
			err:    exitCodeError{code: 1},
			want:   "timed out after 60s",
		},
		{
			name:  "the tool-side bound stops the storage query",
			limit: collectExecTimeout,
			err:   &timeoutError{timeout: collectExecTimeout + collectDBQueryGrace},
			want:  "timed out after 60s",
		},
		{
			name:   "PostgreSQL stops a breakdown query",
			limit:  collectDBQueryTimeout,
			stderr: "ERROR:  canceling statement due to statement timeout",
			err:    exitCodeError{code: 1},
			want:   "timed out after 300s",
		},
		{
			name:  "the tool-side bound stops a breakdown query",
			limit: collectDBQueryTimeout,
			err:   &timeoutError{timeout: collectDBQueryTimeout + collectDBQueryGrace},
			want:  "timed out after 300s",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := &dbQueryBackend{
				fakeCollectBackend: *newFakeCollectBackend(),
				respond: func(dbQueryCall) (string, string, error) {
					return "", tt.stderr, tt.err
				},
			}
			cc := newDBQueryContext(t, backend, creds)

			_, err := runTaskManagerDBQuery(cc, dbQuery{sql: "SELECT 1", limit: tt.limit})
			if err == nil {
				t.Fatal("runTaskManagerDBQuery succeeded, want a timeout")
			}
			if err.Error() != tt.want {
				t.Errorf("error = %q, want %q", err.Error(), tt.want)
			}
			var timeout *timeoutError
			if !errors.As(err, &timeout) || timeout.Error() != tt.want {
				t.Errorf("errors.As(*timeoutError) = %v (%v), want %q so the manifest records it as every other timeout", timeout, err, tt.want)
			}
			var queryErr *dbQueryError
			if !errors.As(err, &queryErr) || queryErr.kind != dbQueryTimedOut {
				t.Errorf("error %v is not a *dbQueryError of kind %q", err, dbQueryTimedOut)
			}
		})
	}
}

// TestRunTaskManagerDBQuery_MasksPassword covers FR-017: the password reaches
// psql on stdin, and if the error or either stream still quotes it, as
// `env PGPASSWORD=<password> psql ...` did when the exec carried it, it must
// reach none of the strings the runner returns, since they all go to the
// failure diagnostics.
func TestRunTaskManagerDBQuery_MasksPassword(t *testing.T) {
	const password = "Sup3r-Secr3t!"
	echoed := "env PGAPPNAME=infrahub-collect PGPASSWORD=" + password + " psql -h localhost"

	tests := []struct {
		name     string
		creds    taskManagerDBCredentials
		stdout   string
		stderr   string
		err      error
		wantKind dbQueryErrorKind
	}{
		{
			name:     "echoed on the detail line of stderr and in stdout",
			creds:    taskManagerDBCredentials{user: "tm-owner", password: password, database: "tm-db"},
			stdout:   echoed,
			stderr:   cnpgNotice + "\nOCI runtime exec failed: exec: \"" + echoed + "\": executable file not found in $PATH\ncommand terminated with exit code 127",
			err:      exitCodeError{code: 127},
			wantKind: dbQueryFailed,
		},
		{
			name:     "echoed in the Go error only",
			creds:    taskManagerDBCredentials{user: "tm-owner", password: password, database: "tm-db"},
			err:      fmt.Errorf("kubectl exec %s: %w", echoed, exitCodeError{code: 1}),
			wantKind: dbQueryFailed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := &dbQueryBackend{
				fakeCollectBackend: *newFakeCollectBackend(),
				respond: func(dbQueryCall) (string, string, error) {
					return tt.stdout, tt.stderr, tt.err
				},
			}
			cc := newDBQueryContext(t, backend, tt.creds)

			result, err := runTaskManagerDBQuery(cc, dbQuery{sql: "SELECT 1", limit: collectExecTimeout})
			if err == nil {
				t.Fatal("runTaskManagerDBQuery succeeded, want a failure")
			}
			var queryErr *dbQueryError
			if !errors.As(err, &queryErr) || queryErr.kind != tt.wantKind {
				t.Errorf("error %v, want a *dbQueryError of kind %q", err, tt.wantKind)
			}
			if !strings.Contains(err.Error(), maskedValue) {
				t.Errorf("error %q does not show where the password was masked", err.Error())
			}

			for what, value := range map[string]string{
				"command": strings.Join(result.command, " "),
				"stdout":  result.stdout,
				"stderr":  result.stderr,
				"error":   err.Error(),
				"detail":  queryErr.detail,
			} {
				if strings.Contains(value, password) {
					t.Errorf("%s holds the password: %q", what, value)
				}
			}
			if call := backend.calls[0]; call.input != password+"\n" {
				t.Errorf("the backend did not receive the unmasked password on stdin")
			}
		})
	}
}

// TestRunTaskManagerDBQuery_DefaultLoginKeepsNames checks the reason of a
// failed query with the default login, whose password "prefect" is also the
// database name: the reason and the command still name the database.
func TestRunTaskManagerDBQuery_DefaultLoginKeepsNames(t *testing.T) {
	creds := taskManagerDBCredentials{user: defaultPostgresUsername, password: defaultPostgresPassword, database: defaultPostgresDatabase}
	const stderr = `psql: error: connection to server at "localhost" (::1), port 5432 failed: FATAL:  database "prefect" does not exist`
	backend := &dbQueryBackend{
		fakeCollectBackend: *newFakeCollectBackend(),
		respond: func(dbQueryCall) (string, string, error) {
			return "", stderr, exitCodeError{code: 2}
		},
	}
	cc := newDBQueryContext(t, backend, creds)

	result, err := runTaskManagerDBQuery(cc, dbQuery{sql: "SELECT 1", limit: collectExecTimeout})

	if err == nil || err.Error() != "login rejected: "+stderr {
		t.Errorf("error = %v, want the reason with the database name: %q", err, "login rejected: "+stderr)
	}
	if command := strings.Join(result.command, " "); !strings.Contains(command, "-d prefect") {
		t.Errorf("command = %q, want it to name the database", command)
	}
}

// TestRunTaskManagerDBQuery_SuccessKeepsQueryResult covers the other half of
// the masking contract: a successful query's stdout is the query result, which
// never holds the password, so it comes back unmasked. With a password that is
// a common word, such as "flow-run", masking it would turn event names into
// "prefect.********.Completed". The command and stderr stay masked.
func TestRunTaskManagerDBQuery_SuccessKeepsQueryResult(t *testing.T) {
	const password = "flow-run"
	creds := taskManagerDBCredentials{user: "prefect", password: password, database: "prefect"}
	const stdout = `{"database" : "prefect", "events" : ["prefect.flow-run.Completed"]}`
	backend := &dbQueryBackend{
		fakeCollectBackend: *newFakeCollectBackend(),
		respond: func(dbQueryCall) (string, string, error) {
			return stdout, cnpgNotice + "\nenv PGPASSWORD=" + password + " psql", nil
		},
	}
	cc := newDBQueryContext(t, backend, creds)

	result, err := runTaskManagerDBQuery(cc, dbQuery{sql: "SELECT 1", limit: collectExecTimeout})
	if err != nil {
		t.Fatalf("runTaskManagerDBQuery failed: %v", err)
	}
	if result.stdout != stdout {
		t.Errorf("stdout = %q, want the query result unchanged: %q", result.stdout, stdout)
	}
	if command := strings.Join(result.command, " "); strings.Contains(command, password) {
		t.Errorf("command holds the password: %q", command)
	}
	if strings.Contains(result.stderr, password) || !strings.Contains(result.stderr, "PGPASSWORD="+maskedValue) {
		t.Errorf("stderr = %q, want the password masked", result.stderr)
	}
}

func TestRunTaskManagerDBQuery_BackendWithoutBoundedExec(t *testing.T) {
	cc := newDBQueryContext(t, newFakeCollectBackend(), taskManagerDBCredentials{user: "tm-owner", password: "tm-secret", database: "tm-db"})

	result, err := runTaskManagerDBQuery(cc, dbQuery{sql: "SELECT 1", limit: collectExecTimeout})

	var queryErr *dbQueryError
	if !errors.As(err, &queryErr) || queryErr.kind != dbQueryFailed {
		t.Fatalf("error = %v, want a *dbQueryError of kind %q", err, dbQueryFailed)
	}
	if !strings.Contains(err.Error(), "does not support bounded exec with stdin") {
		t.Errorf("error = %q, want it to say the backend cannot run a bounded exec with stdin", err.Error())
	}
	if len(result.command) < 3 || result.command[2] != psqlWithPasswordFromStdin {
		t.Errorf("result command = %q, want the psql command for the failure diagnostics", result.command)
	}
}

func TestSkipWhenTaskManagerDBAbsent(t *testing.T) {
	withReplica := newFakeCollectBackend()
	withReplica.replicas[taskManagerDBService] = []Replica{{Service: taskManagerDBService, Container: "task-manager-db-1"}}

	tests := []struct {
		name       string
		backend    EnvironmentBackend
		wantSkip   bool
		wantReason string
	}{
		{name: "deployed", backend: withReplica},
		{name: "not deployed", backend: newFakeCollectBackend(), wantSkip: true, wantReason: taskManagerDBNotDeployedReason},
		{name: "enumeration error leaves the decision to run", backend: &logsFakeBackend{replicasErr: errors.New("kubectl: connection refused")}},
		{name: "backend without collect primitives leaves the decision to run", backend: &bareBackend{name: "docker"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			skip, reason := skipWhenTaskManagerDBAbsent(&collectContext{ctx: context.Background(), backend: tt.backend})
			if skip != tt.wantSkip || reason != tt.wantReason {
				t.Errorf("skipWhenTaskManagerDBAbsent() = (%v, %q), want (%v, %q)", skip, reason, tt.wantSkip, tt.wantReason)
			}
		})
	}
}
