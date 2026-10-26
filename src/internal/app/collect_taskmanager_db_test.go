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
			name: "task-manager-db without a database name takes the default database",
			env: map[string]envExecResponse{
				"task-manager":       execFailed,
				taskManagerDBService: {output: "POSTGRES_USER=db-owner\nPOSTGRES_PASSWORD=db-secret\n"},
			},
			want:      taskManagerDBCredentials{user: "db-owner", password: "db-secret", database: defaultPostgresDatabase, source: dbCredentialsFromDatabase},
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
			name: "task-manager-db user without a password falls through to the defaults",
			env: map[string]envExecResponse{
				"task-manager":       execFailed,
				taskManagerDBService: {output: "POSTGRES_USER=db-owner\nPOSTGRES_DB=db-name\n"},
			},
			want:      taskManagerDBCredentials{user: defaultPostgresUsername, password: defaultPostgresPassword, database: defaultPostgresDatabase, source: dbCredentialsFromDefaults},
			wantCalls: map[string]int{"task-manager": 1, taskManagerDBService: 1},
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
				taskManagerDBService: {output: "POSTGRES_USER=db-owner\n"},
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
			command, env := buildPsqlCommand(creds, tt.query)

			wantCommand := []string{
				"psql", "-h", "localhost", "-U", "tm-owner", "-d", "tm-db",
				"-X", "-q", "-A", "-t", "-v", "ON_ERROR_STOP=1", "-c", tt.query.sql,
			}
			if !slices.Equal(command, wantCommand) {
				t.Errorf("command = %q, want %q", command, wantCommand)
			}
			if slices.ContainsFunc(command, func(arg string) bool { return strings.Contains(arg, creds.password) }) {
				t.Errorf("command %q carries the password; it belongs in the environment only", command)
			}

			wantEnv := map[string]string{
				"PGPASSWORD":        "tm-secret",
				"PGAPPNAME":         "infrahub-collect",
				"PGCONNECT_TIMEOUT": "10",
				"PGOPTIONS":         tt.wantPGOptions,
			}
			if !maps.Equal(env, wantEnv) {
				t.Errorf("env = %q, want %q", env, wantEnv)
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

func TestEffectiveDBQueryLimit(t *testing.T) {
	tests := []struct {
		value    string
		want     map[time.Duration]time.Duration // base -> limit
		wantWarn bool
	}{
		{value: "", want: map[time.Duration]time.Duration{collectExecTimeout: collectExecTimeout, collectDBQueryTimeout: collectDBQueryTimeout}},
		{value: "5s", want: map[time.Duration]time.Duration{collectExecTimeout: 5 * time.Second, collectDBQueryTimeout: 5 * time.Second}},
		{value: "10m", want: map[time.Duration]time.Duration{collectExecTimeout: collectExecTimeout, collectDBQueryTimeout: collectDBQueryTimeout}},
		{value: "garbage", want: map[time.Duration]time.Duration{collectExecTimeout: collectExecTimeout, collectDBQueryTimeout: collectDBQueryTimeout}, wantWarn: true},
		{value: "0", want: map[time.Duration]time.Duration{collectExecTimeout: collectExecTimeout, collectDBQueryTimeout: collectDBQueryTimeout}, wantWarn: true},
		{value: "-5s", want: map[time.Duration]time.Duration{collectExecTimeout: collectExecTimeout, collectDBQueryTimeout: collectDBQueryTimeout}, wantWarn: true},
	}

	for _, tt := range tests {
		for _, base := range []time.Duration{collectExecTimeout, collectDBQueryTimeout} {
			t.Run(fmt.Sprintf("%s=%q base %s", dbQueryTimeoutEnvVar, tt.value, base), func(t *testing.T) {
				getenv := func(name string) string {
					if name == dbQueryTimeoutEnvVar {
						return tt.value
					}
					return ""
				}

				var got time.Duration
				logs := captureLogrus(t, func() { got = effectiveDBQueryLimit(base, getenv) })

				if got != tt.want[base] {
					t.Errorf("effectiveDBQueryLimit(%s) = %s, want %s", base, got, tt.want[base])
				}
				warned := strings.Contains(logs, "level=warning") && strings.Contains(logs, dbQueryTimeoutEnvVar)
				if warned != tt.wantWarn {
					t.Errorf("warning logged = %v, want %v:\n%s", warned, tt.wantWarn, logs)
				}
			})
		}
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
	dockerPsqlMissing := `OCI runtime exec failed: exec failed: unable to start container process: exec: "psql": executable file not found in $PATH: unknown`
	k8sPsqlMissing := "env: can't execute 'psql': No such file or directory"
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
			name:       "Docker: psql not in the image",
			err:        exitCodeError{code: 127},
			stderr:     dockerPsqlMissing,
			wantKind:   dbQueryPsqlMissing,
			wantDetail: dockerPsqlMissing,
			wantError:  "psql missing: " + dockerPsqlMissing,
		},
		{
			name:       "Docker: runtime error written to stdout",
			err:        exitCodeError{code: 127},
			stdout:     dockerPsqlMissing,
			wantKind:   dbQueryPsqlMissing,
			wantDetail: dockerPsqlMissing,
			wantError:  "psql missing: " + dockerPsqlMissing,
		},
		{
			name:       "Kubernetes: busybox env cannot execute psql",
			err:        exitCodeError{code: 127},
			stderr:     k8sPsqlMissing + "\ncommand terminated with exit code 127",
			wantKind:   dbQueryPsqlMissing,
			wantDetail: k8sPsqlMissing,
			wantError:  "psql missing: " + k8sPsqlMissing,
		},
		{
			name:       "Kubernetes: GNU env cannot execute psql",
			err:        exitCodeError{code: 127},
			stderr:     "env: 'psql': No such file or directory\ncommand terminated with exit code 127",
			wantKind:   dbQueryPsqlMissing,
			wantDetail: "env: 'psql': No such file or directory",
			wantError:  "psql missing: env: 'psql': No such file or directory",
		},
		{
			name:       "psql missing needs exit code 127",
			err:        exitCodeError{code: 1},
			stderr:     dockerPsqlMissing,
			wantKind:   dbQueryFailed,
			wantDetail: dockerPsqlMissing,
			wantError:  "query failed: " + dockerPsqlMissing,
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

// dbQueryCall is one ExecSeparateContext call that dbQueryBackend received.
type dbQueryCall struct {
	timeout time.Duration
	service string
	command []string
	opts    *ExecOptions
}

// dbQueryBackend answers the bounded, separated-stream exec through respond
// and records every call, standing in for docker or kubectl exec into
// task-manager-db. respond can answer by the SQL, the last argument of the
// psql command.
type dbQueryBackend struct {
	fakeCollectBackend
	respond func(call dbQueryCall) (stdout, stderr string, err error)
	calls   []dbQueryCall
}

func (b *dbQueryBackend) ExecSeparateContext(ctx context.Context, timeout time.Duration, service string, command []string, opts *ExecOptions) (string, string, error) {
	call := dbQueryCall{timeout: timeout, service: service, command: command, opts: opts}
	b.calls = append(b.calls, call)
	return b.respond(call)
}

var _ separateExecer = (*dbQueryBackend)(nil)

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
			wantCommand, wantEnv := buildPsqlCommand(creds, tt.query)
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

// TestRunTaskManagerDBQuery_MasksPassword covers FR-017: on Kubernetes the exec
// runs `env PGPASSWORD=<password> psql ...`, and kubectl or the container can
// echo it in the error and both streams. When the query fails, the password
// must reach none of the strings the runner returns, since they all go to the
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
			wantKind: dbQueryPsqlMissing,
		},
		{
			name:     "echoed in the Go error only",
			creds:    taskManagerDBCredentials{user: "tm-owner", password: password, database: "tm-db"},
			err:      fmt.Errorf("kubectl exec %s: %w", echoed, exitCodeError{code: 1}),
			wantKind: dbQueryFailed,
		},
		{
			name:     "user and password are the same word, as in the Docker Compose default",
			creds:    taskManagerDBCredentials{user: password, password: password, database: "tm-db"},
			stderr:   `psql: error: connection to server at "localhost" (::1), port 5432 failed: FATAL:  password authentication failed for user "` + password + `"`,
			err:      exitCodeError{code: 2},
			wantKind: dbQueryLoginRejected,
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
			if call := backend.calls[0]; call.opts == nil || call.opts.Env["PGPASSWORD"] != password {
				t.Errorf("the backend did not receive the unmasked password in PGPASSWORD")
			}
		})
	}
}

// TestRunTaskManagerDBQuery_SuccessKeepsQueryResult covers the other half of
// the masking contract: a successful query's stdout is the query result, which
// never holds the command line, so it comes back unmasked. With the default
// password "prefect", masking it would turn the database name and event names
// into "********". The command and stderr stay masked.
func TestRunTaskManagerDBQuery_SuccessKeepsQueryResult(t *testing.T) {
	creds := taskManagerDBCredentials{user: "prefect", password: defaultPostgresPassword, database: "prefect"}
	const stdout = `{"database" : "prefect", "events" : ["prefect.flow-run.Completed"]}`
	backend := &dbQueryBackend{
		fakeCollectBackend: *newFakeCollectBackend(),
		respond: func(dbQueryCall) (string, string, error) {
			return stdout, cnpgNotice + "\nenv PGPASSWORD=prefect psql", nil
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
	if command := strings.Join(result.command, " "); strings.Contains(command, defaultPostgresPassword) {
		t.Errorf("command holds the password: %q", command)
	}
	if strings.Contains(result.stderr, defaultPostgresPassword) || !strings.Contains(result.stderr, "PGPASSWORD="+maskedValue) {
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
	if !strings.Contains(err.Error(), "does not support bounded exec") {
		t.Errorf("error = %q, want it to say the backend cannot run a bounded exec", err.Error())
	}
	if len(result.command) == 0 || result.command[0] != "psql" {
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
