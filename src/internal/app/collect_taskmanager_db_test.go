package app

import (
	"context"
	"errors"
	"maps"
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
