package app

import "testing"

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
			iops.applyPrefectConnection(tt.connStr)

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
