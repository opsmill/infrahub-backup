package app

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
	logrustest "github.com/sirupsen/logrus/hooks/test"
)

func newTestOps() *InfrahubOps {
	return &InfrahubOps{config: &Configuration{}}
}

func TestApplyPrefectConnection(t *testing.T) {
	tests := []struct {
		name     string
		connStr  string
		database string
		username string
		password string
	}{
		{
			name:     "prefect-helm async driver",
			connStr:  "postgresql+asyncpg://prefect:prefect-rocks@release-postgresql:5432/prefect",
			database: "prefect",
			username: "prefect",
			password: "prefect-rocks",
		},
		{
			name:     "plain postgres scheme",
			connStr:  "postgres://prefect:s3cr3t@db:5432/server",
			database: "server",
			username: "prefect",
			password: "s3cr3t",
		},
		{
			name:    "empty connection string leaves config untouched",
			connStr: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			iops := newTestOps()
			iops.applyPrefectConnection(prefectConnectionEnvVars[0], dbCredentialsFromOperator, tt.connStr)

			if iops.config.PostgresDatabase != tt.database {
				t.Errorf("database = %q, want %q", iops.config.PostgresDatabase, tt.database)
			}
			if iops.config.PostgresUsername != tt.username {
				t.Errorf("username = %q, want %q", iops.config.PostgresUsername, tt.username)
			}
			if iops.config.PostgresPassword != tt.password {
				t.Errorf("password = %q, want %q", iops.config.PostgresPassword, tt.password)
			}
		})
	}
}

// TestLoadCredentialsFromEnvironmentPrefect3 verifies the Prefect 3.x env var
// (used by current prefect-helm) is discovered, not just the legacy 2.x name.
func TestLoadCredentialsFromEnvironmentPrefect3(t *testing.T) {
	t.Setenv("PREFECT_API_DATABASE_CONNECTION_URL", "")
	t.Setenv("PREFECT_SERVER_DATABASE_CONNECTION_URL", "postgresql+asyncpg://prefect:pw3@db:5432/prefect")

	iops := newTestOps()
	iops.loadCredentialsFromEnvironment()

	if iops.config.PostgresUsername != "prefect" {
		t.Errorf("username = %q, want %q", iops.config.PostgresUsername, "prefect")
	}
	if iops.config.PostgresPassword != "pw3" {
		t.Errorf("password = %q, want %q", iops.config.PostgresPassword, "pw3")
	}
}

// TestLoadCredentialsFromEnvironmentPrefect2 verifies the legacy env var still works.
func TestLoadCredentialsFromEnvironmentPrefect2(t *testing.T) {
	t.Setenv("PREFECT_SERVER_DATABASE_CONNECTION_URL", "")
	t.Setenv("PREFECT_API_DATABASE_CONNECTION_URL", "postgres://prefect:pw2@db:5432/prefect")

	iops := newTestOps()
	iops.loadCredentialsFromEnvironment()

	if iops.config.PostgresPassword != "pw2" {
		t.Errorf("password = %q, want %q", iops.config.PostgresPassword, "pw2")
	}
}

// TestLoadCredentialsFromEnvironmentPrefersServerName ensures the newer Prefect 3.x
// connection URL wins when both env vars are present.
func TestLoadCredentialsFromEnvironmentPrefersServerName(t *testing.T) {
	t.Setenv("PREFECT_SERVER_DATABASE_CONNECTION_URL", "postgres://prefect:server-pw@db:5432/prefect")
	t.Setenv("PREFECT_API_DATABASE_CONNECTION_URL", "postgres://prefect:api-pw@db:5432/prefect")

	iops := newTestOps()
	iops.loadCredentialsFromEnvironment()

	if iops.config.PostgresPassword != "server-pw" {
		t.Errorf("password = %q, want %q (PREFECT_SERVER_* should win)", iops.config.PostgresPassword, "server-pw")
	}
}

func TestPrefectConnectionFromEnv(t *testing.T) {
	const (
		prefect3 = "postgresql+asyncpg://prefect:server-pw@db:5432/prefect"
		prefect2 = "postgresql+asyncpg://prefect:api-pw@db:5432/prefect"
	)
	tests := []struct {
		name      string
		envOutput string
		wantName  string
		want      string
	}{
		{
			name:      "Prefect 3 variable listed first wins",
			envOutput: "PATH=/usr/bin\nPREFECT_SERVER_DATABASE_CONNECTION_URL=" + prefect3 + "\nPREFECT_API_DATABASE_CONNECTION_URL=" + prefect2 + "\n",
			wantName:  "PREFECT_SERVER_DATABASE_CONNECTION_URL",
			want:      prefect3,
		},
		{
			name:      "Prefect 3 variable listed last wins",
			envOutput: "PREFECT_API_DATABASE_CONNECTION_URL=" + prefect2 + "\nHOME=/root\nPREFECT_SERVER_DATABASE_CONNECTION_URL=" + prefect3,
			wantName:  "PREFECT_SERVER_DATABASE_CONNECTION_URL",
			want:      prefect3,
		},
		{
			name:      "Prefect 2 variable alone",
			envOutput: "HOME=/root\nPREFECT_API_DATABASE_CONNECTION_URL=" + prefect2 + "\n",
			wantName:  "PREFECT_API_DATABASE_CONNECTION_URL",
			want:      prefect2,
		},
		{
			name:      "empty Prefect 3 variable falls back to Prefect 2",
			envOutput: "PREFECT_SERVER_DATABASE_CONNECTION_URL=\nPREFECT_API_DATABASE_CONNECTION_URL=" + prefect2 + "\n",
			wantName:  "PREFECT_API_DATABASE_CONNECTION_URL",
			want:      prefect2,
		},
		{
			name:      "no connection variable",
			envOutput: "PATH=/usr/bin\nHOME=/root\n",
		},
		{
			name:      "empty output",
			envOutput: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if name, got := prefectConnectionFromEnv(tt.envOutput); name != tt.wantName || got != tt.want {
				t.Errorf("prefectConnectionFromEnv() = %q, %q, want %q, %q", name, got, tt.wantName, tt.want)
			}
		})
	}
}

// unparsablePrefectPassword holds an unencoded "#", so a connection URL with
// it does not parse, and Go's parse error quotes the part before the "#"
// (`invalid port ":Kx9q" after host`). Both parts are words of no other text,
// so finding either in a warning means the password leaked.
const unparsablePrefectPassword = "Kx9q#Wz3v"

// unparsablePrefectURL returns a Prefect connection URL that does not parse
// because password is not percent-encoded.
func unparsablePrefectURL(password string) string {
	return "postgresql+asyncpg://owner:" + password + "@task-manager-db:5432/prefect"
}

// warningMessages returns the messages of the warnings that hook recorded.
func warningMessages(hook *logrustest.Hook) []string {
	messages := []string{}
	for _, entry := range hook.AllEntries() {
		if entry.Level == logrus.WarnLevel {
			messages = append(messages, entry.Message)
		}
	}
	return messages
}

// assertUnparsableURLWarning checks the warning about a Prefect connection URL
// that does not parse: it names the variable and where it was found, gives the
// hint, and holds neither the URL, the password nor any part of the password
// around its special character (FR-017).
func assertUnparsableURLWarning(t *testing.T, warnings []string, variable, source, url, password string) {
	t.Helper()
	if len(warnings) != 1 {
		t.Fatalf("warnings = %q, want one about %s", warnings, variable)
	}
	message := warnings[0]
	for _, want := range []string{variable, "from the " + source, "percent-encode special characters in the password"} {
		if !strings.Contains(message, want) {
			t.Errorf("warning %q does not hold %q", message, want)
		}
	}
	for _, other := range prefectConnectionEnvVars {
		if other != variable && strings.Contains(message, other) {
			t.Errorf("warning %q names %s, want only %s", message, other, variable)
		}
	}
	secrets := slices.Concat([]string{url, password}, strings.FieldsFunc(password, func(r rune) bool { return r == '#' || r == '/' }))
	for _, secret := range secrets {
		if strings.Contains(message, secret) {
			t.Errorf("warning %q holds %q from the connection URL", message, secret)
		}
	}
}

// TestLoadCredentialsFromEnvironment_WarnsOnUnparsableURL covers the backup
// tool's lookup in the operator's environment: a URL that does not parse
// leaves the configuration unchanged, and the warning names the variable that
// holds it, whichever of the two it is.
func TestLoadCredentialsFromEnvironment_WarnsOnUnparsableURL(t *testing.T) {
	url := unparsablePrefectURL(unparsablePrefectPassword)
	for _, variable := range prefectConnectionEnvVars {
		t.Run(variable, func(t *testing.T) {
			clearOperatorPrefectEnv(t)
			t.Setenv(variable, url)
			hook := captureLogs(t)

			iops := newTestOps()
			iops.loadCredentialsFromEnvironment()

			if iops.config.PostgresUsername != "" || iops.config.PostgresPassword != "" || iops.config.PostgresDatabase != "" {
				t.Errorf("config = %+v, want the PostgreSQL credentials unchanged", iops.config)
			}
			assertUnparsableURLWarning(t, warningMessages(hook), variable, dbCredentialsFromOperator, url, unparsablePrefectPassword)
		})
	}
}

// TestFetchPostgresCredentials_WarnsOnUnparsableURL covers the backup tool's
// lookup in the task-manager container: the warning names the variable that
// holds the URL in that container and the task-manager environment.
func TestFetchPostgresCredentials_WarnsOnUnparsableURL(t *testing.T) {
	passwords := []struct{ name, password string }{
		{name: "unencoded hash", password: unparsablePrefectPassword},
		{name: "unencoded slash", password: "Kx9q/Wz3v"},
	}
	for _, tt := range passwords {
		for _, variable := range prefectConnectionEnvVars {
			t.Run(variable+" with an "+tt.name, func(t *testing.T) {
				url := unparsablePrefectURL(tt.password)
				backend := newFakeCollectBackend()
				backend.execFn = func(service string, command []string, opts *ExecOptions) (string, error) {
					if service != "task-manager" || strings.Join(command, " ") != "env" {
						return "", errors.New("unexpected exec of " + strings.Join(command, " ") + " in " + service)
					}
					return "PATH=/usr/bin\n" + variable + "=" + url + "\n", nil
				}
				hook := captureLogs(t)

				iops := &InfrahubOps{config: &Configuration{}, backend: backend}
				if err := iops.fetchPostgresCredentials(); err != nil {
					t.Fatalf("fetchPostgresCredentials() = %v, want nil", err)
				}

				if iops.config.PostgresUsername != "" || iops.config.PostgresPassword != "" || iops.config.PostgresDatabase != "" {
					t.Errorf("config = %+v, want the PostgreSQL credentials unchanged", iops.config)
				}
				assertUnparsableURLWarning(t, warningMessages(hook), variable, dbCredentialsFromTaskManager, url, tt.password)
			})
		}
	}
}

func TestParsePrefectConnection(t *testing.T) {
	tests := []struct {
		name     string
		connStr  string
		user     string
		password string
		database string
		wantErr  string
	}{
		{
			name:     "async driver suffix",
			connStr:  "postgresql+asyncpg://prefect:secret@task-manager-db:5432/prefect",
			user:     "prefect",
			password: "secret",
			database: "prefect",
		},
		{
			name:     "plain postgres scheme",
			connStr:  "postgres://owner:s3cr3t@db:5432/server",
			user:     "owner",
			password: "s3cr3t",
			database: "server",
		},
		{
			name:     "percent-encoded password",
			connStr:  "postgresql+asyncpg://owner:p%40ss%2Fw%23rd%25@db:5432/prefect",
			user:     "owner",
			password: "p@ss/w#rd%",
			database: "prefect",
		},
		{
			name:     "query parameters are ignored",
			connStr:  "postgres://prefect:secret@task-manager-db:5432/prefect?sslmode=disable",
			user:     "prefect",
			password: "secret",
			database: "prefect",
		},
		{
			name:    "URL without a login or a database leaves them empty",
			connStr: "postgresql+asyncpg://task-manager-db:5432",
		},
		{
			name:    "invalid port",
			connStr: "postgresql+asyncpg://prefect:secret@task-manager-db:badport/prefect",
			wantErr: "the text after the host name is not a port number",
		},
		{
			name:    "not a PostgreSQL URL",
			connStr: "sqlite+aiosqlite:///var/lib/prefect/prefect.db",
			wantErr: "the URL does not start with postgresql:// or postgres://",
		},
		{
			name:    "empty string",
			connStr: "",
			wantErr: "the URL does not start with postgresql:// or postgres://",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user, password, database, err := parsePrefectConnection(tt.connStr)
			if tt.wantErr != "" {
				if err == nil || !strings.HasPrefix(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want one starting with %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("error = %v, want none", err)
			}
			if user != tt.user {
				t.Errorf("user = %q, want %q", user, tt.user)
			}
			if password != tt.password {
				t.Errorf("password = %q, want %q", password, tt.password)
			}
			if database != tt.database {
				t.Errorf("database = %q, want %q", database, tt.database)
			}
		})
	}
}

// TestParsePrefectConnection_IgnoresOperatorPostgresEnv checks that a part
// the URL leaves out stays empty: nothing comes from the PostgreSQL variables,
// password file or service file of the machine that runs the tool.
func TestParsePrefectConnection_IgnoresOperatorPostgresEnv(t *testing.T) {
	t.Setenv("PGUSER", "op-user")
	t.Setenv("PGPASSWORD", "op-password")
	t.Setenv("PGDATABASE", "op-database")
	t.Setenv("PGSERVICE", "op-service")
	t.Setenv("PGPASSFILE", "/nonexistent/pgpass")

	user, password, database, err := parsePrefectConnection("postgresql+asyncpg://task-manager-db:5432")
	if err != nil || user != "" || password != "" || database != "" {
		t.Errorf("parsePrefectConnection() = %q, %q, %q, %v, want empty parts and no error", user, password, database, err)
	}
}

// TestParsePrefectConnection_ErrorQuotesNoPart checks that each problem is
// named, and that the error quotes no part of the URL, which holds the
// password.
func TestParsePrefectConnection_ErrorQuotesNoPart(t *testing.T) {
	tests := []struct {
		name     string
		password string
		wantErr  string
	}{
		{name: "unencoded hash", password: unparsablePrefectPassword, wantErr: "the text after the host name is not a port number"},
		{name: "unencoded slash", password: "Kx9q/Wz3v", wantErr: "the text after the host name is not a port number"},
		{name: "percent sign without two hexadecimal digits", password: "Kx9q%zzWz3v", wantErr: "a % sign is not followed by two hexadecimal digits"},
		{name: "unexpanded variable reference", password: "${PREFECT_API_DATABASE_PASSWORD}", wantErr: "the user name or password holds a character that is not allowed"},
		{name: "space", password: "Kx9q Wz3v", wantErr: "the user name or password holds a character that is not allowed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			url := unparsablePrefectURL(tt.password)
			_, _, _, err := parsePrefectConnection(url)
			if err == nil || !strings.HasPrefix(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want one starting with %q", err, tt.wantErr)
			}
			parts := strings.FieldsFunc(tt.password, func(r rune) bool { return strings.ContainsRune("#/% ${}", r) })
			for _, secret := range slices.Concat([]string{url, tt.password}, parts) {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("error %q holds %q from the connection URL", err.Error(), secret)
				}
			}
		})
	}
}
