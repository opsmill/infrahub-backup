package app

import (
	"strings"
	"testing"
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

// TestApplyPrefectConnectionKeepsHostAndPort is T009's point: the host and port
// the connection URL carries are the deployment's only record of where the
// task-manager database lives, so they are kept instead of discarded. They land
// in the discovered endpoint rather than in the configured address, so that
// --postgres-address still overrides them.
func TestApplyPrefectConnectionKeepsHostAndPort(t *testing.T) {
	tests := []struct {
		name    string
		connStr string
		host    string
		port    int
	}{
		{
			name:    "external host and explicit port",
			connStr: "postgresql+asyncpg://prefect:prefect-rocks@pg.example.com:6432/prefect",
			host:    "pg.example.com",
			port:    6432,
		},
		{
			name:    "in-deployment service name",
			connStr: "postgres://prefect:s3cr3t@task-manager-db:5432/prefect",
			host:    "task-manager-db",
			port:    5432,
		},
		{
			name:    "empty connection string leaves the endpoint untouched",
			connStr: "",
		},
		{
			name:    "unparseable connection string leaves the endpoint untouched",
			connStr: "postgres://prefect:pw@[::1:5432/prefect",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			iops := newTestOps()
			iops.applyPrefectConnection(tt.connStr)

			if iops.discoveredPostgres.Address != tt.host {
				t.Errorf("discovered address = %q, want %q", iops.discoveredPostgres.Address, tt.host)
			}
			if iops.discoveredPostgres.ClientPort != tt.port {
				t.Errorf("discovered client port = %d, want %d", iops.discoveredPostgres.ClientPort, tt.port)
			}
			if iops.config.ExternalDB.PostgresAddress != "" {
				t.Errorf("configured address = %q, want it left to the operator's override", iops.config.ExternalDB.PostgresAddress)
			}
		})
	}
}

// TestApplyNeo4jEnvironment is T008's point: the address, port, protocol and TLS
// settings the deployment already exports are read rather than ignored.
func TestApplyNeo4jEnvironment(t *testing.T) {
	tests := []struct {
		name         string
		envOut       string
		wantAddress  string
		wantPort     int
		wantProtocol string
		wantTLS      ExternalDBTLS
	}{
		{
			name: "single external host",
			envOut: strings.Join([]string{
				"INFRAHUB_DB_ADDRESS=neo4j.example.com",
				"INFRAHUB_DB_PORT=7688",
				"INFRAHUB_DB_PROTOCOL=bolt",
			}, "\n"),
			wantAddress:  "neo4j.example.com",
			wantPort:     7688,
			wantProtocol: "bolt",
		},
		{
			name: "cluster member list and routed protocol",
			envOut: strings.Join([]string{
				"PATH=/usr/local/bin",
				"INFRAHUB_DB_ADDRESS=core-1:7688,core-2,core-3",
				"INFRAHUB_DB_PROTOCOL=neo4j",
			}, "\n"),
			wantAddress:  "core-1:7688,core-2,core-3",
			wantProtocol: "neo4j",
		},
		{
			name: "TLS settings are read as the deployment wrote them",
			envOut: strings.Join([]string{
				"INFRAHUB_DB_ADDRESS=neo4j.example.com",
				"INFRAHUB_DB_TLS_ENABLED=true",
				"INFRAHUB_DB_TLS_INSECURE=1",
				"INFRAHUB_DB_TLS_CA_FILE=/certs/ca.pem",
			}, "\n"),
			wantAddress: "neo4j.example.com",
			wantTLS:     ExternalDBTLS{Enabled: true, Insecure: true, CAFile: "/certs/ca.pem"},
		},
		{
			name: "TLS explicitly disabled stays disabled",
			envOut: strings.Join([]string{
				"INFRAHUB_DB_TLS_ENABLED=false",
			}, "\n"),
		},
		{
			name: "unreadable port and TLS switch are ignored rather than guessed",
			envOut: strings.Join([]string{
				"INFRAHUB_DB_ADDRESS=neo4j.example.com",
				"INFRAHUB_DB_PORT=bolt",
				"INFRAHUB_DB_TLS_ENABLED=maybe",
			}, "\n"),
			wantAddress: "neo4j.example.com",
		},
		{
			name: "empty values are treated as unset",
			envOut: strings.Join([]string{
				"INFRAHUB_DB_ADDRESS=",
				"INFRAHUB_DB_PORT=",
				"INFRAHUB_DB_PROTOCOL=",
				"INFRAHUB_DB_TLS_CA_FILE=",
			}, "\n"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			iops := newTestOps()
			iops.applyNeo4jEnvironment(tt.envOut)

			if iops.discoveredNeo4j.Address != tt.wantAddress {
				t.Errorf("discovered address = %q, want %q", iops.discoveredNeo4j.Address, tt.wantAddress)
			}
			if iops.discoveredNeo4j.ClientPort != tt.wantPort {
				t.Errorf("discovered client port = %d, want %d", iops.discoveredNeo4j.ClientPort, tt.wantPort)
			}
			if iops.config.ExternalDB.Neo4jProtocol != tt.wantProtocol {
				t.Errorf("protocol = %q, want %q", iops.config.ExternalDB.Neo4jProtocol, tt.wantProtocol)
			}
			if iops.config.ExternalDB.Neo4jTLS != tt.wantTLS {
				t.Errorf("TLS = %+v, want %+v", iops.config.ExternalDB.Neo4jTLS, tt.wantTLS)
			}
			if iops.config.ExternalDB.Neo4jAddress != "" {
				t.Errorf("configured address = %q, want it left to the operator's override", iops.config.ExternalDB.Neo4jAddress)
			}
		})
	}
}

// TestApplyNeo4jEnvironmentCredentialsUnchanged is FR-015: a deployment that sets
// none of the new settings behaves exactly as before. The credentials are read as
// they always were, an already-configured value is still not overwritten, and
// nothing external is engaged.
func TestApplyNeo4jEnvironmentCredentialsUnchanged(t *testing.T) {
	envOut := strings.Join([]string{
		"PATH=/usr/local/bin",
		"INFRAHUB_DB_DATABASE=infrahub",
		"INFRAHUB_DB_USERNAME=neo4j",
		"INFRAHUB_DB_PASSWORD=admin",
	}, "\n")

	iops := newTestOps()
	iops.applyNeo4jEnvironment(envOut)

	if iops.config.Neo4jDatabase != "infrahub" {
		t.Errorf("database = %q, want %q", iops.config.Neo4jDatabase, "infrahub")
	}
	if iops.config.Neo4jUsername != "neo4j" {
		t.Errorf("username = %q, want %q", iops.config.Neo4jUsername, "neo4j")
	}
	if iops.config.Neo4jPassword != "admin" {
		t.Errorf("password = %q, want %q", iops.config.Neo4jPassword, "admin")
	}
	if (iops.discoveredNeo4j != discoveredEndpoint{}) {
		t.Errorf("discovered endpoint = %+v, want nothing discovered", iops.discoveredNeo4j)
	}
	if (iops.config.ExternalDB != ExternalDBConfig{}) {
		t.Errorf("external configuration = %+v, want it untouched", iops.config.ExternalDB)
	}

	// An existing value still wins over the container's, as it did before.
	configured := newTestOps()
	configured.config.Neo4jPassword = "from-the-operator"
	configured.applyNeo4jEnvironment(envOut)

	if configured.config.Neo4jPassword != "from-the-operator" {
		t.Errorf("password = %q, want the already-configured value to survive", configured.config.Neo4jPassword)
	}
}

// TestApplyPrefectEndpointAdoptsOnlyAStatedHost is finding 18 at the point
// where it decides what the tool will connect to — and, on the restore path,
// write into.
//
// pgx.ParseConfig fills a Host and Port the connection string did not give it,
// from libpq's defaults: PGHOST and PGPORT out of the *operator's* own
// environment, and failing those a local socket directory. Adopting its parse
// therefore turned a URL with no host into a "discovered external database"
// pointing at whatever machine the tool was running on.
func TestApplyPrefectEndpointAdoptsOnlyAStatedHost(t *testing.T) {
	t.Setenv("PGHOST", "the-operators-laptop.local")
	t.Setenv("PGPORT", "15432")

	t.Run("a URL that states no host discovers nothing", func(t *testing.T) {
		iops := newTestOps()
		iops.applyPrefectConnection("postgres:///prefect")
		if !iops.discoveredPostgres.read {
			t.Fatal("applyPrefectConnection() did not record the read, want it recorded: the URL is well-formed, it just names no host")
		}

		if iops.discoveredPostgres.Address != "" {
			t.Errorf("discovered address = %q, want none: the URL named no host, and the operator's PGHOST is not the deployment's database",
				iops.discoveredPostgres.Address)
		}
		if iops.discoveredPostgres.ClientPort != 0 {
			t.Errorf("discovered client port = %d, want none: the URL named no port", iops.discoveredPostgres.ClientPort)
		}
		// The credentials it did state are still applied.
		if iops.config.PostgresDatabase != "prefect" {
			t.Errorf("database = %q, want prefect: only the endpoint is withheld", iops.config.PostgresDatabase)
		}
	})

	t.Run("a URL that states a host discovers it", func(t *testing.T) {
		iops := newTestOps()
		iops.applyPrefectConnection("postgresql+asyncpg://prefect:pw@pg.example.com:6432/prefect")

		if iops.discoveredPostgres.Address != "pg.example.com" {
			t.Errorf("discovered address = %q, want pg.example.com", iops.discoveredPostgres.Address)
		}
		if iops.discoveredPostgres.ClientPort != 6432 {
			t.Errorf("discovered client port = %d, want 6432", iops.discoveredPostgres.ClientPort)
		}
	})

	t.Run("a host stated as a query parameter is read", func(t *testing.T) {
		iops := newTestOps()
		iops.applyPrefectConnection("postgres://prefect:pw@/prefect?host=/var/run/postgresql&port=5433")

		if iops.discoveredPostgres.Address != "/var/run/postgresql" {
			t.Errorf("discovered address = %q, want the socket directory the URL named", iops.discoveredPostgres.Address)
		}
		if iops.discoveredPostgres.ClientPort != 5433 {
			t.Errorf("discovered client port = %d, want 5433", iops.discoveredPostgres.ClientPort)
		}
	})

	t.Run("the keyword form is read too", func(t *testing.T) {
		iops := newTestOps()
		iops.applyPrefectConnection("host=pg.example.com port=6432 user=prefect password=pw dbname=prefect")

		if iops.discoveredPostgres.Address != "pg.example.com" {
			t.Errorf("discovered address = %q, want pg.example.com", iops.discoveredPostgres.Address)
		}
		if iops.discoveredPostgres.ClientPort != 6432 {
			t.Errorf("discovered client port = %d, want 6432", iops.discoveredPostgres.ClientPort)
		}
	})

	t.Run("a port outside the usable range is not stated", func(t *testing.T) {
		iops := newTestOps()
		iops.discoveredPostgres.ClientPort = 5432
		iops.applyPrefectEndpoint("postgres://prefect:pw@pg.example.com/prefect?port=0")

		if iops.discoveredPostgres.ClientPort != 5432 {
			t.Errorf("discovered client port = %d, want the previous value left alone", iops.discoveredPostgres.ClientPort)
		}
	})
}

// TestLoadCredentialsFromEnvironmentRecordsOnlyAReadItMade is the other half of
// finding 18. discoveredPostgres.read suppresses the deployment-side discovery
// read, and it was set from the operator's environment whether or not the URL
// there could be parsed — so one typo in a variable permanently suppressed the
// read that would have supplied the endpoint, and the run went looking for a
// database it had no address for.
func TestLoadCredentialsFromEnvironmentRecordsOnlyAReadItMade(t *testing.T) {
	t.Run("an unparseable URL leaves the deployment read outstanding", func(t *testing.T) {
		t.Setenv("PREFECT_SERVER_DATABASE_CONNECTION_URL", "postgres://prefect:pw@[::1:5432/prefect")
		iops := newTestOps()

		iops.loadCredentialsFromEnvironment()

		if iops.discoveredPostgres.read {
			t.Error("discovery was recorded as done from a URL that could not be read; the deployment's own connection string is never consulted after this")
		}
	})

	t.Run("a URL that was read records the read", func(t *testing.T) {
		t.Setenv("PREFECT_SERVER_DATABASE_CONNECTION_URL", "postgres://prefect:pw@pg.example.com:6432/prefect")
		iops := newTestOps()

		iops.loadCredentialsFromEnvironment()

		if !iops.discoveredPostgres.read {
			t.Error("discovery was not recorded as done from a URL that supplied the endpoint")
		}
		if iops.discoveredPostgres.Address != "pg.example.com" {
			t.Errorf("discovered address = %q, want pg.example.com", iops.discoveredPostgres.Address)
		}
	})
}

// prefectEnvBackend answers `env` in the task-manager container with a scripted
// dump, which is what fetchPostgresCredentials reads.
type prefectEnvBackend struct {
	bareBackend
	env string
}

func (b *prefectEnvBackend) Exec(service string, command []string, opts *ExecOptions) (string, error) {
	if service == "task-manager" && len(command) > 0 && command[0] == "env" {
		return b.env, nil
	}

	return "", nil
}

// TestFetchPostgresCredentialsRecordsOnlyAReadItMade is T106: the same defect
// TestLoadCredentialsFromEnvironmentRecordsOnlyAReadItMade fixed at the other
// call site of applyPrefectConnection. The bool exists to say "nothing was
// applied"; discarding it and recording the read anyway suppresses
// ensurePostgresDiscovery, the one read that supplies the endpoint a restore
// writes into.
func TestFetchPostgresCredentialsRecordsOnlyAReadItMade(t *testing.T) {
	tests := []struct {
		name string
		env  string
		read bool
		addr string
	}{
		{
			name: "a URL that was read records the read",
			env:  "PREFECT_API_DATABASE_CONNECTION_URL=postgres://prefect:pw@pg.example.com:6432/prefect\n",
			read: true,
			addr: "pg.example.com",
		},
		{
			// One typo in the deployment's own variable. The URL applied
			// nothing, so the question has not been answered and the external
			// path must still ask it.
			name: "an unparseable URL leaves the deployment read outstanding",
			env:  "PREFECT_API_DATABASE_CONNECTION_URL=postgres://prefect:pw@[::1:6432/prefect\n",
		},
		{
			// A container that exports no connection URL at all is the same
			// case: there is nothing to have read.
			name: "a container exporting no connection URL records nothing",
			env:  "PATH=/usr/bin\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			iops := newTestOps()
			iops.backend = &prefectEnvBackend{env: tt.env}

			if err := iops.fetchPostgresCredentials(); err != nil {
				t.Fatalf("fetchPostgresCredentials() error = %v", err)
			}

			if iops.discoveredPostgres.read != tt.read {
				t.Errorf("discoveredPostgres.read = %t, want %t", iops.discoveredPostgres.read, tt.read)
			}
			if iops.discoveredPostgres.Address != tt.addr {
				t.Errorf("discovered address = %q, want %q", iops.discoveredPostgres.Address, tt.addr)
			}
		})
	}
}

// TestStatedPrefectSSLRootCert is the discovery half of T110: `verify-full` was
// set with no trust anchor beside it, while the anchor the deployment's own
// client uses is stated in the connection URL this tool already reads.
func TestStatedPrefectSSLRootCert(t *testing.T) {
	tests := []struct {
		name    string
		connStr string
		want    string
	}{
		{
			name:    "the URL form",
			connStr: "postgres://prefect:pw@pg.example.com:6432/prefect?sslmode=verify-full&sslrootcert=/etc/ssl/pg-ca.pem",
			want:    "/etc/ssl/pg-ca.pem",
		},
		{
			name:    "the driver-qualified scheme Prefect writes",
			connStr: "postgresql+asyncpg://prefect:pw@pg.example.com/prefect?sslrootcert=/certs/ca.crt",
			want:    "/certs/ca.crt",
		},
		{
			name:    "the keyword form",
			connStr: "host=pg.example.com port=6432 dbname=prefect sslrootcert='/certs/ca.crt'",
			want:    "/certs/ca.crt",
		},
		{name: "a URL naming no authority", connStr: "postgres://prefect:pw@pg.example.com/prefect"},
		{name: "nothing at all", connStr: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := statedPrefectSSLRootCert(tt.connStr); got != tt.want {
				t.Errorf("statedPrefectSSLRootCert(%q) = %q, want %q", tt.connStr, got, tt.want)
			}
		})
	}

	// It reaches the configuration the client env is built from, which is the
	// step that was missing: the CA was read into a log string and nowhere else.
	t.Run("the discovered authority reaches the configuration", func(t *testing.T) {
		iops := newTestOps()
		iops.applyPrefectEndpoint("postgres://prefect:pw@pg.example.com:6432/prefect?sslrootcert=/etc/ssl/pg-ca.pem")

		if got := iops.config.ExternalDB.PostgresTLS.CAFile; got != "/etc/ssl/pg-ca.pem" {
			t.Errorf("ExternalDB.PostgresTLS.CAFile = %q, want the authority the deployment states", got)
		}
	})
}
