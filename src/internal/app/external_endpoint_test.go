package app

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestParseHostPortList covers the address shapes Infrahub's own INFRAHUB_DB_ADDRESS
// accepts, and the malformed ones a deployment or an operator can write. The
// inheritance rule is the one that carries the most weight: a member without a
// port takes the client port, which is how Infrahub reads the same setting.
func TestParseHostPortList(t *testing.T) {
	tests := []struct {
		name        string
		address     string
		defaultPort int
		want        []HostPort
		wantErr     bool
	}{
		{
			name:        "single host inherits the client port",
			address:     "neo4j.example.com",
			defaultPort: 7687,
			want:        []HostPort{{Host: "neo4j.example.com", Port: 7687}},
		},
		{
			name:        "host with an explicit port keeps it",
			address:     "neo4j.example.com:7688",
			defaultPort: 7687,
			want:        []HostPort{{Host: "neo4j.example.com", Port: 7688}},
		},
		{
			name:        "mixed list preserves order and inherits per member",
			address:     "core-1:7688,core-2,core-3:7689",
			defaultPort: 7687,
			want: []HostPort{
				{Host: "core-1", Port: 7688},
				{Host: "core-2", Port: 7687},
				{Host: "core-3", Port: 7689},
			},
		},
		{
			name:        "whitespace around members is not part of the host",
			address:     " core-1 , core-2:7688 ",
			defaultPort: 7687,
			want: []HostPort{
				{Host: "core-1", Port: 7687},
				{Host: "core-2", Port: 7688},
			},
		},
		{
			name:        "IPv4 address with a port",
			address:     "10.0.0.7:7688",
			defaultPort: 7687,
			want:        []HostPort{{Host: "10.0.0.7", Port: 7688}},
		},
		{
			name:        "bracketed IPv6 literal with a port",
			address:     "[::1]:7687",
			defaultPort: 5432,
			want:        []HostPort{{Host: "::1", Port: 7687}},
		},
		{
			name:        "bracketed IPv6 literal without a port inherits",
			address:     "[fe80::1]",
			defaultPort: 7687,
			want:        []HostPort{{Host: "fe80::1", Port: 7687}},
		},
		{
			name:        "bare IPv6 literal is a host, not a host:port pair",
			address:     "::1",
			defaultPort: 7687,
			want:        []HostPort{{Host: "::1", Port: 7687}},
		},
		{
			name:        "IPv6 members mixed with a named host",
			address:     "[2001:db8::1]:7688,core-2,fe80::2",
			defaultPort: 7687,
			want: []HostPort{
				{Host: "2001:db8::1", Port: 7688},
				{Host: "core-2", Port: 7687},
				{Host: "fe80::2", Port: 7687},
			},
		},
		{
			name:        "empty address configures no endpoint",
			address:     "",
			defaultPort: 7687,
		},
		{
			name:        "whitespace-only address configures no endpoint",
			address:     "   ",
			defaultPort: 7687,
		},
		{
			name:        "empty member is an error rather than a silently shorter list",
			address:     "core-1,,core-2",
			defaultPort: 7687,
			wantErr:     true,
		},
		{
			name:        "trailing separator is an error",
			address:     "core-1,",
			defaultPort: 7687,
			wantErr:     true,
		},
		{
			name:        "member with no host is an error",
			address:     ":7687",
			defaultPort: 7687,
			wantErr:     true,
		},
		{
			name:        "non-numeric port is an error",
			address:     "core-1:bolt",
			defaultPort: 7687,
			wantErr:     true,
		},
		{
			name:        "empty port is an error",
			address:     "core-1:",
			defaultPort: 7687,
			wantErr:     true,
		},
		{
			name:        "out-of-range port is an error",
			address:     "core-1:99999",
			defaultPort: 7687,
			wantErr:     true,
		},
		{
			name:        "port zero is an error",
			address:     "core-1:0",
			defaultPort: 7687,
			wantErr:     true,
		},
		{
			name:        "unterminated IPv6 bracket is an error",
			address:     "[::1:7687",
			defaultPort: 7687,
			wantErr:     true,
		},
		{
			// T113. Two colons were read as "every colon belongs to an IPv6
			// address", so a semicolon where the operator meant a comma became
			// one host whose name is the whole typo — and that host reached the
			// capture's --from list and the artefact's provenance metadata as a
			// member this run claims to have read.
			name:        "a semicolon in place of a separator is an error, not a host",
			address:     "db-a:7687;db-b:7687",
			defaultPort: 7687,
			wantErr:     true,
		},
		{
			name:        "a member with two ports is an error",
			address:     "core-1:7687:6362",
			defaultPort: 7687,
			wantErr:     true,
		},
		{
			name:        "a host name with a stray colon is an error",
			address:     "core-1:7687,core-2:bolt:7687",
			defaultPort: 7687,
			wantErr:     true,
		},
		{
			// Still a literal, and still carrying the colons that make it look
			// like host:port, so the check is "is this an IP" rather than "is
			// this IPv6-shaped".
			name:        "IPv4-mapped IPv6 literal is a host",
			address:     "::ffff:10.0.0.1",
			defaultPort: 7687,
			want:        []HostPort{{Host: "::ffff:10.0.0.1", Port: 7687}},
		},
		{
			// The zone names the interface the address is reachable on, and an
			// address without it cannot be connected to — so it is kept.
			name:        "link-local literal keeps its zone identifier",
			address:     "fe80::1%eth0",
			defaultPort: 7687,
			want:        []HostPort{{Host: "fe80::1%eth0", Port: 7687}},
		},
		{
			name:        "a bracketed host name is an error",
			address:     "[db-a.example.net]",
			defaultPort: 7687,
			wantErr:     true,
		},
		{
			name:        "a bracketed host name with a port is an error",
			address:     "[db-a.example.net]:7687",
			defaultPort: 7687,
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseHostPortList(tt.address, tt.defaultPort)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseHostPortList(%q) = %v, want an error", tt.address, got)
				}
				if got != nil {
					t.Errorf("parseHostPortList(%q) returned %v alongside the error, want nil", tt.address, got)
				}

				return
			}

			if err != nil {
				t.Fatalf("parseHostPortList(%q) returned an unexpected error: %v", tt.address, err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("parseHostPortList(%q) = %v, want %v", tt.address, got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("member %d = %v, want %v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// TestHostPortString checks the rendered form, since it is what reaches both the
// endpoint report and the backup command's endpoint list — an unbracketed IPv6
// literal there would be read as a host and a port.
func TestHostPortString(t *testing.T) {
	tests := []struct {
		name string
		host HostPort
		want string
	}{
		{name: "named host", host: HostPort{Host: "core-1", Port: 7687}, want: "core-1:7687"},
		{name: "IPv4", host: HostPort{Host: "10.0.0.7", Port: 6362}, want: "10.0.0.7:6362"},
		{name: "IPv6 is bracketed", host: HostPort{Host: "2001:db8::1", Port: 7687}, want: "[2001:db8::1]:7687"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.host.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestDatabaseEndpointResolveHosts is the inheritance rule at the endpoint level:
// the port a member inherits is the endpoint's client port, and when the
// deployment records none it is Infrahub's own default for that service.
func TestDatabaseEndpointResolveHosts(t *testing.T) {
	tests := []struct {
		name     string
		endpoint DatabaseEndpoint
		address  string
		want     []HostPort
	}{
		{
			name:     "members inherit the discovered client port",
			endpoint: DatabaseEndpoint{Service: serviceNeo4j, ClientPort: 7688},
			address:  "core-1,core-2:7690",
			want: []HostPort{
				{Host: "core-1", Port: 7688},
				{Host: "core-2", Port: 7690},
			},
		},
		{
			name:     "Neo4j with no discovered port falls back to Bolt's default",
			endpoint: DatabaseEndpoint{Service: serviceNeo4j},
			address:  "core-1",
			want:     []HostPort{{Host: "core-1", Port: defaultNeo4jClientPort}},
		},
		{
			name:     "task-manager database falls back to the PostgreSQL default",
			endpoint: DatabaseEndpoint{Service: serviceTaskManagerDB},
			address:  "pg-1",
			want:     []HostPort{{Host: "pg-1", Port: defaultPostgresPort}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			endpoint := tt.endpoint
			if err := endpoint.resolveHosts(tt.address); err != nil {
				t.Fatalf("resolveHosts(%q) returned an unexpected error: %v", tt.address, err)
			}

			if len(endpoint.Hosts) != len(tt.want) {
				t.Fatalf("Hosts = %v, want %v", endpoint.Hosts, tt.want)
			}
			for i := range tt.want {
				if endpoint.Hosts[i] != tt.want[i] {
					t.Errorf("member %d = %v, want %v", i, endpoint.Hosts[i], tt.want[i])
				}
			}
		})
	}
}

// TestDatabaseEndpointResolveHostsError names the service in its failure, since
// a deployment with one external database of each kind gives the operator two
// address lists to tell apart.
func TestDatabaseEndpointResolveHostsError(t *testing.T) {
	endpoint := DatabaseEndpoint{Service: serviceNeo4j}

	err := endpoint.resolveHosts("core-1:bolt")
	if err == nil {
		t.Fatal("resolveHosts with a malformed port returned no error")
	}
	if !strings.Contains(err.Error(), serviceNeo4j) {
		t.Errorf("error = %q, want it to name the %q service", err.Error(), serviceNeo4j)
	}
}

// TestResolveNeo4jBackupPort is the port discovery cannot supply: the vendor
// default unless the operator names another one (research R4).
func TestResolveNeo4jBackupPort(t *testing.T) {
	tests := []struct {
		name string
		cfg  ExternalDBConfig
		want int
	}{
		{name: "unset falls back to the vendor default", cfg: ExternalDBConfig{}, want: defaultNeo4jBackupPort},
		{name: "default carries through", cfg: ExternalDBConfig{Neo4jBackupPort: defaultNeo4jBackupPort}, want: 6362},
		{name: "flag override wins", cfg: ExternalDBConfig{Neo4jBackupPort: 6363}, want: 6363},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveNeo4jBackupPort(tt.cfg); got != tt.want {
				t.Errorf("resolveNeo4jBackupPort() = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestDatabaseEndpointDescribe checks the reported line states the values the run
// will use — including the ports and protocol a deployment left at their
// defaults, which are absent from its environment but not from the connection.
func TestDatabaseEndpointDescribe(t *testing.T) {
	endpoint := DatabaseEndpoint{
		Service:    serviceNeo4j,
		Location:   EndpointLocationExternal,
		Hosts:      []HostPort{{Host: "core-1", Port: 7687}, {Host: "2001:db8::1", Port: 7687}},
		BackupPort: defaultNeo4jBackupPort,
		Database:   "infrahub",
		TLS:        ExternalDBTLS{Enabled: true, CAFile: "/certs/ca.pem", Insecure: true},
	}

	got := endpoint.Describe()

	for _, want := range []string{
		"location=external",
		"hosts=core-1:7687,[2001:db8::1]:7687",
		"protocol=bolt",
		"client-port=7687",
		"backup-port=6362",
		"database=infrahub",
		"tls=enabled,ca=/certs/ca.pem,deployment-insecure",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Describe() = %q, want it to contain %q", got, want)
		}
	}
}

// TestDatabaseEndpointDescribeInternal is the internal case: no address list, and
// no protocol invented for a service that has no such setting.
func TestDatabaseEndpointDescribeInternal(t *testing.T) {
	endpoint := DatabaseEndpoint{Service: serviceTaskManagerDB, Location: EndpointLocationInternal}

	got := endpoint.Describe()

	if !strings.Contains(got, "location=internal") {
		t.Errorf("Describe() = %q, want it to contain %q", got, "location=internal")
	}
	if !strings.Contains(got, "hosts=none") {
		t.Errorf("Describe() = %q, want it to contain %q", got, "hosts=none")
	}
	if !strings.Contains(got, "client-port=5432") {
		t.Errorf("Describe() = %q, want it to contain %q", got, "client-port=5432")
	}
	if strings.Contains(got, "protocol=") {
		t.Errorf("Describe() = %q, want no protocol for the task-manager database", got)
	}
	if !strings.Contains(got, "tls=disabled") {
		t.Errorf("Describe() = %q, want it to contain %q", got, "tls=disabled")
	}
}

// TestDatabaseEndpointReport is FR-003's reporting half: the resolved endpoint
// reaches the log, and calling Report again — as a connection path does
// defensively — does not repeat the line.
func TestDatabaseEndpointReport(t *testing.T) {
	endpoint := DatabaseEndpoint{
		Service:    serviceNeo4j,
		Location:   EndpointLocationExternal,
		Hosts:      []HostPort{{Host: "core-1", Port: 7687}},
		BackupPort: defaultNeo4jBackupPort,
	}

	logged := captureLogrus(t, func() {
		endpoint.Report()
	})

	if !strings.Contains(logged, "core-1:7687") {
		t.Errorf("log = %q, want it to name the resolved host", logged)
	}
	if !strings.Contains(logged, serviceNeo4j) {
		t.Errorf("log = %q, want it to name the %q service", logged, serviceNeo4j)
	}

	repeated := captureLogrus(t, func() {
		endpoint.Report()
	})

	if repeated != "" {
		t.Errorf("second Report() logged %q, want nothing", repeated)
	}
}

// mixedTopologyOps is a deployment whose two databases have both already been
// discovered, so the cases below exercise resolution rather than the exec that
// feeds it.
func mixedTopologyOps() *InfrahubOps {
	iops := &InfrahubOps{config: &Configuration{
		Neo4jDatabase:    "infrahub",
		PostgresDatabase: "prefect",
		ExternalDB: ExternalDBConfig{
			Neo4jBackupPort: defaultNeo4jBackupPort,
			Neo4jProtocol:   "neo4j",
			Neo4jTLS:        ExternalDBTLS{Enabled: true},
		},
	}}
	iops.discoveredNeo4j = discoveredEndpoint{Address: "core-1,core-2:7688", ClientPort: 7687, read: true}
	iops.discoveredPostgres = discoveredEndpoint{Address: "pg.example.com", ClientPort: 5433, read: true}

	return iops
}

// stubQueries answers the location question from a fixed map and records which
// databases discovery was asked about, so a case can assert that an internal
// database is never discovered for — the structural half of FR-015.
func stubQueries(locations map[string]EndpointLocation, discovered *[]string) deploymentQueries {
	return deploymentQueries{
		locate: func(service string) (EndpointLocation, error) {
			location, ok := locations[service]
			if !ok {
				return "", fmt.Errorf("no stubbed location for %q", service)
			}

			return location, nil
		},
		discover: func(service string) error {
			*discovered = append(*discovered, service)

			return nil
		},
	}
}

// TestResolveEndpointMixedTopology is FR-001: each database is resolved on its
// own evidence, so a deployment with one database inside and one outside works
// with no additional operator input. Both combinations are covered, because a
// resolver that quietly decided once for the whole deployment would pass one of
// them.
func TestResolveEndpointMixedTopology(t *testing.T) {
	t.Run("Neo4j external, task-manager internal", func(t *testing.T) {
		iops := mixedTopologyOps()
		discovered := []string{}
		queries := stubQueries(map[string]EndpointLocation{
			serviceNeo4j:         EndpointLocationExternal,
			serviceTaskManagerDB: EndpointLocationInternal,
		}, &discovered)

		neo4j, err := iops.resolveEndpointWith(queries, serviceNeo4j)
		if err != nil {
			t.Fatalf("resolving the %s endpoint failed: %v", serviceNeo4j, err)
		}
		if neo4j.Location != EndpointLocationExternal {
			t.Errorf("%s location = %q, want %q", serviceNeo4j, neo4j.Location, EndpointLocationExternal)
		}
		wantHosts := []HostPort{{Host: "core-1", Port: 7687}, {Host: "core-2", Port: 7688}}
		if !reflect.DeepEqual(neo4j.Hosts, wantHosts) {
			t.Errorf("%s hosts = %v, want %v", serviceNeo4j, neo4j.Hosts, wantHosts)
		}
		if neo4j.BackupPort != defaultNeo4jBackupPort {
			t.Errorf("%s backup port = %d, want %d", serviceNeo4j, neo4j.BackupPort, defaultNeo4jBackupPort)
		}
		if neo4j.Protocol != "neo4j" || !neo4j.TLS.Enabled || neo4j.Database != "infrahub" {
			t.Errorf("%s endpoint = %+v, want the deployment's protocol, TLS and database name carried through", serviceNeo4j, neo4j)
		}

		taskManager, err := iops.resolveEndpointWith(queries, serviceTaskManagerDB)
		if err != nil {
			t.Fatalf("resolving the %s endpoint failed: %v", serviceTaskManagerDB, err)
		}
		if taskManager.Location != EndpointLocationInternal {
			t.Errorf("%s location = %q, want %q", serviceTaskManagerDB, taskManager.Location, EndpointLocationInternal)
		}
		if len(taskManager.Hosts) != 0 {
			t.Errorf("%s hosts = %v, want none for a database with a container", serviceTaskManagerDB, taskManager.Hosts)
		}

		// The internal database is never discovered for: an all-internal
		// deployment issues no query this feature added (FR-015).
		if !reflect.DeepEqual(discovered, []string{serviceNeo4j}) {
			t.Errorf("discovery was asked about %v, want only %v", discovered, []string{serviceNeo4j})
		}
	})

	t.Run("Neo4j internal, task-manager external", func(t *testing.T) {
		iops := mixedTopologyOps()
		discovered := []string{}
		queries := stubQueries(map[string]EndpointLocation{
			serviceNeo4j:         EndpointLocationInternal,
			serviceTaskManagerDB: EndpointLocationExternal,
		}, &discovered)

		neo4j, err := iops.resolveEndpointWith(queries, serviceNeo4j)
		if err != nil {
			t.Fatalf("resolving the %s endpoint failed: %v", serviceNeo4j, err)
		}
		if neo4j.Location != EndpointLocationInternal {
			t.Errorf("%s location = %q, want %q", serviceNeo4j, neo4j.Location, EndpointLocationInternal)
		}
		if len(neo4j.Hosts) != 0 {
			t.Errorf("%s hosts = %v, want none for a database with a container", serviceNeo4j, neo4j.Hosts)
		}
		// The address the deployment happens to record for an in-deployment
		// database must not turn into an endpoint the run dials over the network.
		if neo4j.BackupPort != 0 {
			t.Errorf("%s backup port = %d, want none asserted for a database reached through its container", serviceNeo4j, neo4j.BackupPort)
		}

		taskManager, err := iops.resolveEndpointWith(queries, serviceTaskManagerDB)
		if err != nil {
			t.Fatalf("resolving the %s endpoint failed: %v", serviceTaskManagerDB, err)
		}
		if taskManager.Location != EndpointLocationExternal {
			t.Errorf("%s location = %q, want %q", serviceTaskManagerDB, taskManager.Location, EndpointLocationExternal)
		}
		wantHosts := []HostPort{{Host: "pg.example.com", Port: 5433}}
		if !reflect.DeepEqual(taskManager.Hosts, wantHosts) {
			t.Errorf("%s hosts = %v, want %v", serviceTaskManagerDB, taskManager.Hosts, wantHosts)
		}
		if taskManager.Database != "prefect" {
			t.Errorf("%s database = %q, want prefect", serviceTaskManagerDB, taskManager.Database)
		}
		if taskManager.Protocol != "" || taskManager.BackupPort != 0 {
			t.Errorf("%s endpoint = %+v, want no protocol or backup port invented for PostgreSQL", serviceTaskManagerDB, taskManager)
		}

		if !reflect.DeepEqual(discovered, []string{serviceTaskManagerDB}) {
			t.Errorf("discovery was asked about %v, want only %v", discovered, []string{serviceTaskManagerDB})
		}
	})
}

// TestResolveEndpointLocationFailure is FR-002 through the resolver: a location
// query that fails is an error, and no endpoint comes back for a caller to
// connect to.
func TestResolveEndpointLocationFailure(t *testing.T) {
	iops := mixedTopologyOps()
	discovered := []string{}
	queries := deploymentQueries{
		locate: func(string) (EndpointLocation, error) {
			return "", errors.New(`pods is forbidden: cannot list resource "pods"`)
		},
		discover: func(service string) error {
			discovered = append(discovered, service)

			return nil
		},
	}

	endpoint, err := iops.resolveEndpointWith(queries, serviceNeo4j)
	if err == nil {
		t.Fatal("resolveEndpointWith succeeded despite an unanswered location query, want error")
	}
	if endpoint != nil {
		t.Errorf("endpoint = %+v, want none alongside an error", endpoint)
	}
	if len(discovered) != 0 {
		t.Errorf("discovery ran for %v after the location query failed, want none", discovered)
	}
	if !strings.Contains(err.Error(), "is forbidden") {
		t.Errorf("err = %v, want it to keep the reason the query failed", err)
	}
}

// TestResolveEndpointOverrideWins covers the reason the operator's overrides and
// the deployment's own facts are recorded separately: discovery must not
// overwrite what the operator asked for.
func TestResolveEndpointOverrideWins(t *testing.T) {
	discovered := []string{}
	queries := stubQueries(map[string]EndpointLocation{
		serviceNeo4j:         EndpointLocationExternal,
		serviceTaskManagerDB: EndpointLocationExternal,
	}, &discovered)

	iops := mixedTopologyOps()
	iops.config.ExternalDB.Neo4jAddress = "operator-1:7690,operator-2"
	iops.config.ExternalDB.PostgresAddress = "operator-pg:6000"
	iops.config.ExternalDB.Neo4jBackupPort = 7000

	neo4j, err := iops.resolveEndpointWith(queries, serviceNeo4j)
	if err != nil {
		t.Fatalf("resolving the %s endpoint failed: %v", serviceNeo4j, err)
	}
	wantHosts := []HostPort{{Host: "operator-1", Port: 7690}, {Host: "operator-2", Port: 7687}}
	if !reflect.DeepEqual(neo4j.Hosts, wantHosts) {
		t.Errorf("%s hosts = %v, want the operator's list %v", serviceNeo4j, neo4j.Hosts, wantHosts)
	}
	if neo4j.BackupPort != 7000 {
		t.Errorf("%s backup port = %d, want the operator's 7000", serviceNeo4j, neo4j.BackupPort)
	}

	taskManager, err := iops.resolveEndpointWith(queries, serviceTaskManagerDB)
	if err != nil {
		t.Fatalf("resolving the %s endpoint failed: %v", serviceTaskManagerDB, err)
	}
	wantPG := []HostPort{{Host: "operator-pg", Port: 6000}}
	if !reflect.DeepEqual(taskManager.Hosts, wantPG) {
		t.Errorf("%s hosts = %v, want the operator's %v", serviceTaskManagerDB, taskManager.Hosts, wantPG)
	}
}

// TestResolveEndpointExternalWithoutAddress is the case discovery cannot cover:
// the database is demonstrably not in the deployment and nothing records where
// it is. The run must stop naming the override rather than guess a host.
func TestResolveEndpointExternalWithoutAddress(t *testing.T) {
	tests := []struct {
		service  string
		wantFlag string
	}{
		{service: serviceNeo4j, wantFlag: "--neo4j-address"},
		{service: serviceTaskManagerDB, wantFlag: "--postgres-address"},
	}

	for _, tt := range tests {
		t.Run(tt.service, func(t *testing.T) {
			iops := &InfrahubOps{config: &Configuration{}}
			discovered := []string{}
			queries := stubQueries(map[string]EndpointLocation{tt.service: EndpointLocationExternal}, &discovered)

			_, err := iops.resolveEndpointWith(queries, tt.service)
			if err == nil {
				t.Fatalf("resolving the %s endpoint succeeded with no address anywhere, want error", tt.service)
			}
			if !strings.Contains(err.Error(), tt.wantFlag) {
				t.Errorf("err = %v, want it to name %s", err, tt.wantFlag)
			}
		})
	}
}

// TestResolveEndpointSeparatesUnconfiguredFromUnreadable is FR-004 against
// FR-019, which are two conditions with two remedies and were one message.
//
// Both arms have the location positively established and no address to reach
// the database by. The difference is whether the deployment answered: an
// operator who never set the address needs to set it, and an operator whose
// `pods/exec` was denied needs that back — and reads a run that asserted their
// settings "were read for it, and none of them records an address", which is a
// claim about their configuration made on evidence this run never obtained. That
// same conflation is what caused this feature's original reset, one question
// earlier (see KubernetesBackend.undeterminedLocation).
func TestResolveEndpointSeparatesUnconfiguredFromUnreadable(t *testing.T) {
	denied := errors.New(`pods "infrahub-server-0" is forbidden: User "system:serviceaccount:infrahub:backup" cannot create resource "pods/exec"`)

	for _, service := range []string{serviceNeo4j, serviceTaskManagerDB} {
		t.Run(service+": the settings were read and record no address", func(t *testing.T) {
			iops := &InfrahubOps{config: &Configuration{}}
			discovered := []string{}
			queries := stubQueries(map[string]EndpointLocation{service: EndpointLocationExternal}, &discovered)

			_, err := iops.resolveEndpointWith(queries, service)
			if !errors.Is(err, errEndpointUndiscoverable) {
				t.Fatalf("err = %v, want it to wrap %v: discovery answered and nothing records an address (FR-004)", err, errEndpointUndiscoverable)
			}
			if errors.Is(err, errDeploymentSettingsUnreadable) {
				t.Error("a deployment that answered was reported as one that could not be read")
			}
		})

		t.Run(service+": the settings could not be read at all", func(t *testing.T) {
			iops := &InfrahubOps{config: &Configuration{}}
			discovered := []string{}
			queries := stubQueries(map[string]EndpointLocation{service: EndpointLocationExternal}, &discovered)
			queries.discover = func(string) error { return denied }

			_, err := iops.resolveEndpointWith(queries, service)
			if !errors.Is(err, errDeploymentSettingsUnreadable) {
				t.Fatalf("err = %v, want it to wrap %v: the deployment was never asked, so whether it records an address is unknown (FR-019)", err, errDeploymentSettingsUnreadable)
			}
			if errors.Is(err, errEndpointUndiscoverable) {
				t.Error("an RBAC denial was reported as a deployment that records no address; the two have different remedies")
			}

			// The cluster's own reason, and both actions — the flag that
			// unblocks the run now, and the access the tool was relying on.
			if !strings.Contains(err.Error(), "pods/exec") {
				t.Errorf("err = %v, want the cluster's own reason carried through", err)
			}
			if !strings.Contains(err.Error(), "not a report that the setting is unset") {
				t.Errorf("err = %v, want it to say outright what it is not claiming", err)
			}
		})

		t.Run(service+": a supplied address survives a discovery failure", func(t *testing.T) {
			// The existing tolerance, which the split must not cost: an
			// operator who supplied the one thing that cannot be defaulted gets
			// their run, and the failure stays a warning.
			iops := &InfrahubOps{config: &Configuration{ExternalDB: ExternalDBConfig{
				Neo4jAddress:    "neo4j.example:7687",
				PostgresAddress: "pg.example:5432",
			}}}
			discovered := []string{}
			queries := stubQueries(map[string]EndpointLocation{service: EndpointLocationExternal}, &discovered)
			queries.discover = func(string) error { return denied }

			endpoint, err := iops.resolveEndpointWith(queries, service)
			if err != nil {
				t.Fatalf("resolveEndpointWith() = %v, want the supplied address to be enough", err)
			}
			if len(endpoint.Hosts) == 0 {
				t.Error("endpoint has no hosts, want the operator's address")
			}
		})
	}
}

// TestResolveEndpointUnknownService keeps resolution to the two documented
// database service names (FR-017): a typo must not resolve into an endpoint the
// run then tries to reach.
func TestResolveEndpointUnknownService(t *testing.T) {
	iops := mixedTopologyOps()
	discovered := []string{}
	queries := stubQueries(map[string]EndpointLocation{"cache": EndpointLocationExternal}, &discovered)

	if _, err := iops.resolveEndpointWith(queries, "cache"); err == nil {
		t.Fatal("resolveEndpointWith accepted a service that is not a database, want error")
	}
}

// TestEnsureDiscoveryAsksOnce covers the widened gate's own guard. Both helpers
// exec into a deployment component, so an ops with no detected environment
// cannot complete one — which is what makes this a real assertion that a
// database already read for is not read for again.
func TestEnsureDiscoveryAsksOnce(t *testing.T) {
	t.Run("Neo4j", func(t *testing.T) {
		iops := &InfrahubOps{config: &Configuration{}}
		iops.discoveredNeo4j.read = true

		if err := iops.ensureNeo4jDiscovery(); err != nil {
			t.Errorf("ensureNeo4jDiscovery asked the deployment a second time: %v", err)
		}
	})

	t.Run("task-manager", func(t *testing.T) {
		iops := &InfrahubOps{config: &Configuration{}}
		iops.discoveredPostgres.read = true

		if err := iops.ensurePostgresDiscovery(); err != nil {
			t.Errorf("ensurePostgresDiscovery asked the deployment a second time: %v", err)
		}
	})
}

// TestParseDatabaseVersion covers the version shapes these two servers and their
// images produce, and the ones that cannot be ordered at all. The scheme is
// asserted alongside the fields because it, not the digits, is what orders two
// versions from different schemes; the field count is asserted because a version
// that states no patch level must not acquire a zero one.
func TestParseDatabaseVersion(t *testing.T) {
	tests := []struct {
		name           string
		version        string
		wantScheme     versionScheme
		wantFields     []int
		wantPrerelease bool
		wantErr        bool
	}{
		{
			name:       "Neo4j 5.x release",
			version:    "5.26.1",
			wantScheme: versionSchemeSequential,
			wantFields: []int{5, 26, 1},
		},
		{
			name:       "Neo4j calendar release",
			version:    "2025.10.1",
			wantScheme: versionSchemeCalendar,
			wantFields: []int{2025, 10, 1},
		},
		{
			name:       "calendar month keeps its leading zero as a number",
			version:    "2025.06",
			wantScheme: versionSchemeCalendar,
			wantFields: []int{2025, 6},
		},
		{
			name:       "the pinned Neo4j image tag is a release, not a pre-release",
			version:    "2025.10.1-enterprise",
			wantScheme: versionSchemeCalendar,
			wantFields: []int{2025, 10, 1},
		},
		{
			name:       "a community tag is likewise a release",
			version:    "5.26.1-community",
			wantScheme: versionSchemeSequential,
			wantFields: []int{5, 26, 1},
		},
		{
			name:       "the pinned PostgreSQL image tag states only its major",
			version:    "18-alpine",
			wantScheme: versionSchemeSequential,
			wantFields: []int{18},
		},
		{
			name:       "PostgreSQL server_version carries its packaging detail",
			version:    "18.1 (Debian 18.1-1.pgdg120+1)",
			wantScheme: versionSchemeSequential,
			wantFields: []int{18, 1},
		},
		{
			name:           "a numbered alpha is a pre-release",
			version:        "5.26.0-alpha09",
			wantScheme:     versionSchemeSequential,
			wantFields:     []int{5, 26, 0},
			wantPrerelease: true,
		},
		{
			name:           "a release candidate is a pre-release",
			version:        "2026.01.0-rc1",
			wantScheme:     versionSchemeCalendar,
			wantFields:     []int{2026, 1, 0},
			wantPrerelease: true,
		},
		{
			name:           "the marker is matched however it is cased",
			version:        "5.26.0-Beta.2",
			wantScheme:     versionSchemeSequential,
			wantFields:     []int{5, 26, 0},
			wantPrerelease: true,
		},
		{
			name:       "build metadata is not a pre-release",
			version:    "18.1+build.7",
			wantScheme: versionSchemeSequential,
			wantFields: []int{18, 1},
		},
		{
			name:       "surrounding whitespace is not part of the version",
			version:    "  5.26.1  ",
			wantScheme: versionSchemeSequential,
			wantFields: []int{5, 26, 1},
		},
		{
			name:    "an empty version cannot be ordered",
			version: "",
			wantErr: true,
		},
		{
			name:    "whitespace alone cannot be ordered",
			version: "   ",
			wantErr: true,
		},
		{
			name:    "a non-numeric version cannot be ordered",
			version: "unknown",
			wantErr: true,
		},
		{
			name:    "an empty component cannot be ordered",
			version: "5..1",
			wantErr: true,
		},
		{
			name:    "a qualifier with no numbers before it cannot be ordered",
			version: "-enterprise",
			wantErr: true,
		},
		{
			name:    "a non-numeric component cannot be ordered",
			version: "5.26.x",
			wantErr: true,
		},
		{
			name:    "an unparsed utility banner cannot be ordered",
			version: "pg_dump (PostgreSQL) 18.1",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseDatabaseVersion(tt.version)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseDatabaseVersion(%q) = %+v, want an error", tt.version, got)
				}

				return
			}
			if err != nil {
				t.Fatalf("parseDatabaseVersion(%q) returned %v, want no error", tt.version, err)
			}

			if got.scheme != tt.wantScheme {
				t.Errorf("scheme = %d, want %d", got.scheme, tt.wantScheme)
			}
			if !reflect.DeepEqual(got.fields, tt.wantFields) {
				t.Errorf("fields = %v, want %v", got.fields, tt.wantFields)
			}
			if got.prerelease != tt.wantPrerelease {
				t.Errorf("prerelease = %t, want %t", got.prerelease, tt.wantPrerelease)
			}
			if got.Original != strings.TrimSpace(tt.version) {
				t.Errorf("Original = %q, want the version as it was read", got.Original)
			}
		})
	}
}

// TestDatabaseVersionCompare is the ordering FR-006 rests on. Every case is run
// in both directions, because a comparator that answers "older" both ways round
// would satisfy half a table and still abort a working run.
func TestDatabaseVersionCompare(t *testing.T) {
	tests := []struct {
		name string
		// want is the ordering of left against right: -1 older, 0 neither
		// older, 1 newer.
		left  string
		right string
		want  int
	}{
		// Within the 5.x scheme.
		{name: "patch levels order numerically", left: "5.26.0", right: "5.26.1", want: -1},
		{name: "minor versions order numerically, not lexically", left: "5.26.0", right: "5.9.0", want: 1},
		{name: "majors order numerically", left: "4.4.30", right: "5.26.1", want: -1},
		{name: "identical versions are neither older", left: "5.26.1", right: "5.26.1", want: 0},

		// Within the calendar scheme.
		{name: "calendar months order within a year", left: "2025.06.0", right: "2025.10.1", want: -1},
		{name: "calendar years order across a year boundary", left: "2025.12.0", right: "2026.01.0", want: -1},
		{name: "calendar patch levels order numerically", left: "2025.10.1", right: "2025.10.0", want: 1},

		// Across the two schemes: the misordering FR-006 names.
		{name: "a calendar release is newer than the 5.26 LTS", left: "5.26.1", right: "2025.06.0", want: -1},
		{name: "and newer whichever side it is written on", left: "2026.07.0", right: "5.26.1", want: 1},
		{name: "the pair a lexical comparison orders backwards", left: "5.26", right: "2025.06", want: -1},
		{name: "a bare calendar year outranks a bare 5.x major", left: "2025", right: "5", want: 1},

		// Precision: an unstated field is not a zero.
		{name: "a major-only utility matches its own series", left: "18", right: "18.1", want: 0},
		{name: "a major-only utility is still older than the next series", left: "18", right: "19.2", want: -1},
		{name: "a minor-only version matches its own patch series", left: "5.26", right: "5.26.4", want: 0},
		{name: "the pinned image tag matches the server it reads", left: "18-alpine", right: "18.1", want: 0},

		// Pre-releases, and the build qualifiers that only look like them.
		{name: "an alpha precedes the release of the same numbers", left: "2025.10.1-alpha", right: "2025.10.1", want: -1},
		{name: "a release candidate precedes its release", left: "5.26.0-rc1", right: "5.26.0", want: -1},
		{name: "an edition tag does not precede anything", left: "2025.10.1-enterprise", right: "2025.10.1", want: 0},
		{name: "two pre-releases of the same numbers are not ordered", left: "5.26.0-alpha09", right: "5.26.0-beta", want: 0},
		{name: "a pre-release still loses to newer numbers", left: "2026.01.0-rc1", right: "2025.10.1", want: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			left, err := parseDatabaseVersion(tt.left)
			if err != nil {
				t.Fatalf("parseDatabaseVersion(%q) returned %v", tt.left, err)
			}
			right, err := parseDatabaseVersion(tt.right)
			if err != nil {
				t.Fatalf("parseDatabaseVersion(%q) returned %v", tt.right, err)
			}

			if got := sign(left.compare(right)); got != tt.want {
				t.Errorf("compare(%q, %q) = %d, want %d", tt.left, tt.right, got, tt.want)
			}
			if got := sign(right.compare(left)); got != -tt.want {
				t.Errorf("compare(%q, %q) = %d, want %d", tt.right, tt.left, got, -tt.want)
			}
		})
	}
}

// sign reduces a comparison result to -1, 0 or 1, so a case states the ordering
// it means rather than the magnitude a comparator happens to return.
func sign(result int) int {
	switch {
	case result < 0:
		return -1
	case result > 0:
		return 1
	default:
		return 0
	}
}

// TestCheckVersionCompatibility is FR-006's asymmetry: an older utility aborts
// naming both versions, a newer one warns and continues. The two directions are
// asserted separately and neither is derived from the other, because collapsing
// them into one symmetric check is exactly the mistake the requirement forbids.
func TestCheckVersionCompatibility(t *testing.T) {
	tests := []struct {
		name     string
		service  string
		utility  string
		server   string
		wantErr  bool
		wantWarn bool
	}{
		{
			name:    "an older utility aborts",
			service: serviceNeo4j,
			utility: "5.26.1",
			server:  "2025.10.1",
			wantErr: true,
		},
		{
			name:    "an older utility within the same scheme aborts too",
			service: serviceTaskManagerDB,
			utility: "17.5",
			server:  "18.1",
			wantErr: true,
		},
		{
			name:     "a newer utility warns and continues",
			service:  serviceNeo4j,
			utility:  "2025.10.1-enterprise",
			server:   "5.26.1",
			wantWarn: true,
		},
		{
			name:    "a matching pair passes quietly",
			service: serviceNeo4j,
			utility: "2025.10.1-enterprise",
			server:  "2025.10.1",
		},
		{
			name:    "the pinned PostgreSQL tag passes against its own series",
			service: serviceTaskManagerDB,
			utility: "18-alpine",
			server:  "18.1 (Debian 18.1-1.pgdg120+1)",
		},
		{
			// pg_dump's own rule is major-only, so a provider that patches its
			// managed server ahead of the image is not an incompatibility — and
			// aborting on it would name an image flag with nothing to move to,
			// since the official images publish no 18.2-alpine.
			name:    "a provider-patched PostgreSQL server passes against an earlier patch",
			service: serviceTaskManagerDB,
			utility: "18.1",
			server:  "18.2",
		},
		{
			name:     "a newer PostgreSQL major still warns",
			service:  serviceTaskManagerDB,
			utility:  "19.0",
			server:   "18.6",
			wantWarn: true,
		},
		{
			// The Neo4j arm is deliberately not loosened alongside it: no
			// published rule says its backup client tolerates an older patch,
			// and inventing one would put a guess inside an abort condition.
			name:    "the Neo4j arm still compares the patch",
			service: serviceNeo4j,
			utility: "5.26.1",
			server:  "5.26.2",
			wantErr: true,
		},
		{
			name:    "an unreadable server version aborts rather than passing",
			service: serviceNeo4j,
			utility: "2025.10.1-enterprise",
			server:  "unknown",
			wantErr: true,
		},
		{
			name:    "an unreadable utility version aborts rather than passing",
			service: serviceTaskManagerDB,
			utility: "",
			server:  "18.1",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var err error
			logged := captureLogrus(t, func() {
				err = checkVersionCompatibility(tt.service, tt.utility, tt.server)
			})

			if tt.wantErr {
				if err == nil {
					t.Fatalf("checkVersionCompatibility(%q, %q, %q) = nil, want an error", tt.service, tt.utility, tt.server)
				}
				// Both versions have to be in the message: the operator cannot
				// tell which end to move from only one of them.
				for _, want := range []string{tt.utility, tt.server} {
					if want == "" {
						continue
					}
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error = %q, want it to name %q", err, want)
					}
				}

				return
			}
			if err != nil {
				t.Fatalf("checkVersionCompatibility(%q, %q, %q) = %v, want no error", tt.service, tt.utility, tt.server, err)
			}

			warned := strings.Contains(logged, "level=warning")
			if warned != tt.wantWarn {
				t.Errorf("warned = %t, want %t (log = %q)", warned, tt.wantWarn, logged)
			}
		})
	}
}

// TestCheckVersionCompatibilityAbortNamesTheFlag is the other half of a useful
// abort: the message says which image to move, and it is the flag for the
// database that actually failed.
func TestCheckVersionCompatibilityAbortNamesTheFlag(t *testing.T) {
	tests := []struct {
		service  string
		wantFlag string
	}{
		{service: serviceNeo4j, wantFlag: "--external-db-image-neo4j"},
		{service: serviceTaskManagerDB, wantFlag: "--external-db-image-postgres"},
	}

	for _, tt := range tests {
		t.Run(tt.service, func(t *testing.T) {
			err := checkVersionCompatibility(tt.service, "5.26.1", "2025.10.1")
			if err == nil {
				t.Fatal("checkVersionCompatibility() = nil, want an error")
			}
			if !strings.Contains(err.Error(), tt.wantFlag) {
				t.Errorf("error = %q, want it to name %q", err, tt.wantFlag)
			}
			if !strings.Contains(err.Error(), tt.service) {
				t.Errorf("error = %q, want it to name the %q database", err, tt.service)
			}
		})
	}
}

// TestFailureMessageContract is contracts/cli-surface.md's failure-message
// table, asserted row by row: every message names the condition, the specific
// resource, and the operator action (SC-008).
//
// It is one table over every row rather than an assertion beside each
// constructor, because the requirement is uniform and because a row with no
// case here is exactly how this contract goes stale. Each case drives the
// constructor a production path calls — never a message assembled by the test —
// so a condition whose message exists but which nothing reaches would fail
// here only if it were also deleted; the caller-reachability of each is what
// `grep` over non-test callers answers, and the doc comment on each constructor
// names its caller.
//
// `wantAbsent` is as load-bearing as the rest for the two rows the contract
// states negatively. Reporting a denied query as an absent database, or a
// Community refusal as a missing service, is the defect this feature exists to
// remove, and a message can carry all three elements and still say the wrong
// one of those.
func TestFailureMessageContract(t *testing.T) {
	endpoint := &DatabaseEndpoint{
		Service:  serviceNeo4j,
		Location: EndpointLocationExternal,
		Hosts:    []HostPort{{Host: "neo4j.example.com", Port: 7687}},
		Database: "infrahub",
	}
	refused := errors.New(`pods is forbidden: User "system:serviceaccount:infrahub:backup" cannot create resource "pods" in API group "" in the namespace "infrahub"`)

	tests := []struct {
		// row is the condition as contracts/cli-surface.md names it.
		row string
		err error

		// The three elements the contract requires of every message. Split so a
		// failure says which of the three is missing rather than that a long
		// string did not match.
		wantCondition []string
		wantResource  []string
		wantAction    []string

		// wantAbsent is what the message must not say, for the rows the
		// contract states as "explicitly not …".
		wantAbsent []string
	}{
		{
			row: "no container and no discoverable endpoint (Neo4j)",
			err: undiscoverableEndpoint(serviceNeo4j, componentInfrahubServer,
				[]string{neo4jAddressEnvVar, neo4jPortEnvVar}, neo4jAddressFlag),
			wantCondition: []string{"does not run in this deployment", "could be discovered"},
			wantResource:  []string{neo4jAddressEnvVar, neo4jPortEnvVar, componentInfrahubServer, "were read for it"},
			wantAction:    []string{"--" + neo4jAddressFlag, "INFRAHUB_NEO4J_ADDRESS", "--k8s-namespace"},
		},
		{
			// The condition above with its central claim removed: the settings
			// were not read, so what they record is unknown. Two conditions,
			// two remedies — and they were one message.
			row: "no container and the deployment's settings could not be read",
			err: unreadableDeploymentSettings(serviceNeo4j, componentInfrahubServer,
				[]string{neo4jAddressEnvVar, neo4jPortEnvVar}, neo4jAddressFlag, refused),
			wantCondition: []string{"does not run in this deployment", "could not be read"},
			wantResource:  []string{neo4jAddressEnvVar, componentInfrahubServer, "is forbidden"},
			wantAction:    []string{"--" + neo4jAddressFlag, "INFRAHUB_NEO4J_ADDRESS", "restore this run's access"},
			wantAbsent:    []string{"were read for it", "none of them records an address"},
		},
		{
			row: "the restored database's state cannot be read at all",
			err: neo4jRestoreUnreadable(
				&externalRestore{externalSource: &externalSource{Endpoint: endpoint}, Bound: time.Hour},
				refused, 90*time.Second),
			wantCondition: []string{"unable to read the database's state", "1m30s"},
			wantResource:  []string{"infrahub", "neo4j.example.com:7687", "is forbidden"},
			wantAction:    []string{"Check that pod and that permission", "read the database's own state directly"},
			// It must not read as a verdict on the database: the read never
			// reached it, and the seed may be loading successfully.
			wantAbsent: []string{"the database is not usable", "neo4j.log"},
		},
		{
			row: "no container and no discoverable endpoint (task manager)",
			err: undiscoverableEndpoint(serviceTaskManagerDB, componentTaskManager,
				prefectConnectionEnvVars, postgresAddressFlag),
			wantCondition: []string{"does not run in this deployment", "could be discovered"},
			wantResource: []string{
				prefectConnectionEnvVars[0], prefectConnectionEnvVars[1],
				componentTaskManager, "were read for it",
			},
			wantAction: []string{"--" + postgresAddressFlag, "INFRAHUB_POSTGRES_ADDRESS", "--k8s-namespace"},
		},
		{
			row:           "deployment query denied",
			err:           newTestKubernetesBackend().undeterminedLocation(serviceNeo4j, refused),
			wantCondition: []string{"failed to determine whether", "could not be queried", "is forbidden"},
			wantResource:  []string{serviceNeo4j, "namespace infrahub", "pods in namespace infrahub"},
			wantAction:    []string{"get and list on pods", "--k8s-namespace"},
			// FR-002. The contract states this row negatively because the two
			// readings have opposite remedies: an operator told the database is
			// absent configures an external one they do not have.
			wantAbsent: []string{"the database is absent:"},
		},
		{
			row:           "missing workload-creation permission",
			err:           workloadPermissionError(refused, "create", "pods", "infrahub"),
			wantCondition: []string{"refused this tool the transient workload", "is forbidden"},
			wantResource:  []string{"create on pods in namespace infrahub"},
			wantAction:    []string{transientWorkloadRBAC, "grant it", "from a host that can reach the database directly"},
		},
		{
			row:           "external Community edition",
			err:           externalCommunityCaptureRefusal(endpoint),
			wantCondition: []string{"Community Edition backup mechanism requires direct access to the database's own storage"},
			wantResource:  []string{"neo4j.example.com:7687"},
			wantAction:    []string{"Neo4j Enterprise Edition feature"},
			// FR-008. "service not found" is the message that sent a customer
			// looking for a container that was never meant to be there.
			wantAbsent: []string{"service not found", "no such service"},
		},
		{
			row:           "version incompatibility",
			err:           checkVersionCompatibility(serviceNeo4j, "5.26.1", "2025.10.1"),
			wantCondition: []string{"is older than the server"},
			wantResource:  []string{"5.26.1", "2025.10.1"},
			wantAction:    []string{versionImageFlag(serviceNeo4j), "at least as new as the server"},
		},
		{
			row:           "missing authorisation for external restore",
			err:           externalRestoreUnauthorised(serviceNeo4j),
			wantCondition: []string{"requires an explicit authorisation", "does not run in this deployment"},
			wantResource:  []string{serviceNeo4j, "no Infrahub service was stopped and no data was changed"},
			wantAction:    []string{"--" + AllowExternalRestoreFlag, AllowExternalRestoreEnvVar},
		},
		{
			row:           "server cannot read the artifact",
			err:           unreadableSeedArtifact("s3://backups/seed/infrahub.backup", errors.New("NoSuchKey")),
			wantCondition: []string{"could not be read back", "NoSuchKey"},
			wantResource:  []string{"s3://backups/seed/infrahub.backup"},
			wantAction:    []string{"readable both from here and from the database host"},
		},
		{
			row: "restore did not come online",
			err: neo4jRestoreFailure(
				&externalRestore{externalSource: &externalSource{Endpoint: endpoint}, Bound: time.Hour},
				"offline", false),
			wantCondition: []string{"was accepted but the database is not usable"},
			wantResource:  []string{"infrahub", "neo4j.example.com:7687", "Its reported state is offline"},
			wantAction:    []string{"neo4j.log", "debug.log"},
		},
		{
			row:           "infrahub-collect --include-backup against an external database",
			err:           (&InfrahubOps{externalCaptureForbidden: true}).refuseForbiddenExternalCapture("infrahub"),
			wantCondition: []string{"does not run in this deployment"},
			wantResource:  []string{"transient pod and secret in namespace infrahub"},
			wantAction:    []string{"infrahub-backup create", "--include-backup"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.row, func(t *testing.T) {
			if tt.err == nil {
				t.Fatalf("the %q condition produced no error, so there is no message to read", tt.row)
			}
			message := tt.err.Error()

			for element, wants := range map[string][]string{
				"condition":       tt.wantCondition,
				"resource":        tt.wantResource,
				"operator action": tt.wantAction,
			} {
				if len(wants) == 0 {
					t.Errorf("no %s is asserted for this row, so the contract is not checked for it", element)
				}
				for _, want := range wants {
					if !strings.Contains(message, want) {
						t.Errorf("the %s element is missing %q:\n%s", element, want, message)
					}
				}
			}

			for _, absent := range tt.wantAbsent {
				if strings.Contains(strings.ToLower(message), strings.ToLower(absent)) {
					t.Errorf("the message says %q, which the contract states this condition must not:\n%s", absent, message)
				}
			}
		})
	}
}

// TestDiagnosticRefusalsAreDistinguishable is the conflation the contract draws
// two separate rows for: an unanswered deployment query and an endpoint nothing
// records are different conditions with different remedies, and each must be
// identifiable as itself.
//
// It asserts on the sentinel rather than the wording, because that is what a
// caller can branch on — and because a reworded message that merged the two
// would otherwise still pass the table above.
func TestDiagnosticRefusalsAreDistinguishable(t *testing.T) {
	undiscoverable := undiscoverableEndpoint(serviceNeo4j, componentInfrahubServer,
		[]string{neo4jAddressEnvVar}, neo4jAddressFlag)
	undetermined := newTestKubernetesBackend().undeterminedLocation(serviceNeo4j,
		errors.New(`pods is forbidden: cannot list resource "pods"`))
	forbidden := workloadPermissionError(
		errors.New(`pods is forbidden: cannot create resource "pods"`), "create", "pods", "infrahub")

	if !errors.Is(undiscoverable, errEndpointUndiscoverable) {
		t.Errorf("the undiscoverable-endpoint refusal does not carry its own sentinel: %v", undiscoverable)
	}
	if errors.Is(undiscoverable, errLocationUndetermined) || errors.Is(undiscoverable, errTransientWorkloadForbidden) {
		t.Errorf("an endpoint nobody records reads as a cluster failure: %v", undiscoverable)
	}

	if !errors.Is(undetermined, errLocationUndetermined) {
		t.Errorf("the undetermined-location refusal does not carry its own sentinel: %v", undetermined)
	}
	if errors.Is(undetermined, errEndpointUndiscoverable) {
		t.Errorf("a denied deployment query reads as an undiscoverable endpoint, which is the conflation FR-002 and FR-004 are two requirements to prevent: %v", undetermined)
	}

	if !errors.Is(forbidden, errTransientWorkloadForbidden) {
		t.Errorf("the missing-workload-permission refusal does not carry its own sentinel: %v", forbidden)
	}
	if errors.Is(forbidden, errEndpointUndiscoverable) || errors.Is(forbidden, errLocationUndetermined) {
		t.Errorf("an RBAC failure creating the workload reads as a database nobody could find, which is the exact conflation FR-019 is separate from FR-004 to prevent: %v", forbidden)
	}
}

// TestWorkloadPermissionErrorLeavesOtherFailuresAlone keeps FR-019's message on
// the one condition it belongs to. A bound that expired, or a connection that
// dropped, is not a missing permission, and telling the operator to grant a
// Role for it would send them to fix something that is not broken.
func TestWorkloadPermissionErrorLeavesOtherFailuresAlone(t *testing.T) {
	for _, cause := range []error{
		errors.New("Unable to connect to the server: dial tcp: i/o timeout"),
		errors.New("error: timed out waiting for the condition"),
		errors.New(`error: You must be logged in to the server (Unauthorized)`),
	} {
		got := workloadPermissionError(cause, "create", "pods", "infrahub")
		if got != cause {
			t.Errorf("workloadPermissionError(%v) rewrote a failure that was not a refusal: %v", cause, got)
		}
		if errors.Is(got, errTransientWorkloadForbidden) {
			t.Errorf("%v was reported as a missing permission", cause)
		}
	}
}

// TestReportDatabaseLocations is US2 scenario 5 at the command an operator
// reaches for first: `environment detect` has to say, per database, whether it
// runs in the deployment and where an external one was resolved to.
//
// It asserts through the same resolveDatabaseTargets every other path uses, and
// on the operation it uses it with: databaseInspect is what keeps the report
// from creating anything, which is what `infrahub-collect` depends on
// (ADR-0003), and what keeps a failure from naming a backup nobody asked for.
func TestReportDatabaseLocations(t *testing.T) {
	t.Run("a mixed topology reports each database on its own evidence", func(t *testing.T) {
		iops := mixedTopologyOps()
		discovered := []string{}
		queries := stubQueries(map[string]EndpointLocation{
			serviceNeo4j:         EndpointLocationExternal,
			serviceTaskManagerDB: EndpointLocationInternal,
		}, &discovered)

		var reportErr error
		logged := captureLogrus(t, func() {
			reportErr = iops.reportDatabaseLocationsWith(queries)
		})
		if reportErr != nil {
			t.Fatalf("reportDatabaseLocationsWith() error = %v; the report must not fail the command", reportErr)
		}
		t.Logf("environment detect reported:\n%s", logged)

		// Only the external database is discovered for: an internal one has a
		// container, so reporting it costs no new query (FR-015).
		if !reflect.DeepEqual(discovered, []string{serviceNeo4j}) {
			t.Errorf("discovery ran for %v, want only the external %s", discovered, serviceNeo4j)
		}

		// What the operator actually reads. Asserted on the output rather than
		// on the resolution, because US2 scenario 5 is a claim about the report
		// and a resolution nothing prints satisfies none of it.
		for _, want := range []string{
			// Per database, and by name: a report that says "one database is
			// external" leaves the operator to work out which.
			"Database " + serviceNeo4j + ": does not run in this deployment",
			"Database " + serviceTaskManagerDB + ": runs in this deployment",
			// Where the external one was resolved to.
			"core-1:7687", "core-2:7688",
		} {
			if !strings.Contains(logged, want) {
				t.Errorf("the report does not state %q:\n%s", want, logged)
			}
		}
	})

	t.Run("an unanswered location query is reported, not treated as external", func(t *testing.T) {
		iops := mixedTopologyOps()
		discovered := []string{}
		queries := deploymentQueries{
			locate: func(string) (EndpointLocation, error) {
				return "", errors.New(`pods is forbidden: cannot list resource "pods"`)
			},
			discover: func(service string) error {
				discovered = append(discovered, service)

				return nil
			},
		}

		var reportErr error
		logged := captureLogrus(t, func() {
			reportErr = iops.reportDatabaseLocationsWith(queries)
		})
		if reportErr != nil {
			t.Fatalf("reportDatabaseLocationsWith() error = %v; a diagnostic must still report what it could", reportErr)
		}
		t.Logf("environment detect reported:\n%s", logged)

		// FR-002 at the report: nothing may proceed as though the database were
		// outside the deployment on the strength of a query that failed.
		if len(discovered) != 0 {
			t.Errorf("discovery ran for %v after the location query failed, want none", discovered)
		}
		if strings.Contains(logged, "does not run in this deployment") {
			t.Errorf("an unanswered query was reported as a database outside the deployment (FR-002):\n%s", logged)
		}
		if !strings.Contains(logged, "Could not establish where the databases live") {
			t.Errorf("the report is silent about the query it could not answer:\n%s", logged)
		}
	})

	t.Run("an inspection needs no restore authorisation", func(t *testing.T) {
		// Deliberately the zero authorisation, which is what every binary's
		// `environment detect` holds. Were an inspection to consult FR-009,
		// detect would refuse to describe the very deployment an operator is
		// diagnosing because they cannot restore into it.
		target, err := databaseTargetWith(
			stubQueries(map[string]EndpointLocation{serviceNeo4j: EndpointLocationExternal}, &[]string{}),
			databaseInspect, serviceNeo4j, ExternalRestoreAuth{})
		if err != nil {
			t.Fatalf("databaseTargetWith(inspect) = %v, want an external database to be inspectable unauthorised", err)
		}
		if target.Location != EndpointLocationExternal {
			t.Errorf("target.Location = %q, want %q", target.Location, EndpointLocationExternal)
		}
	})

	t.Run("a failure names the report rather than a backup", func(t *testing.T) {
		_, err := databaseTargetWith(
			deploymentQueries{locate: func(string) (EndpointLocation, error) {
				return "", errors.New(`pods is forbidden: cannot list resource "pods"`)
			}},
			databaseInspect, serviceNeo4j, ExternalRestoreAuth{})
		if err == nil {
			t.Fatal("databaseTargetWith(inspect) = nil, want the unanswered location query reported (FR-002)")
		}
		if strings.Contains(err.Error(), "cannot start the backup") {
			t.Errorf("the report's failure names a backup the operator did not ask for: %v", err)
		}
		if !strings.Contains(err.Error(), databaseInspect.describe()) {
			t.Errorf("err = %v, want it to name the %q operation", err, databaseInspect.describe())
		}
	})
}

// TestCypherColumnLookupIsIndifferentToCase pins the contract every reader of a
// `--format plain` result now shares: a column is found under the spelling the
// caller asks with, whatever case either side uses.
//
// It is a test of the helper rather than of a parser because no production
// column constant is mixed-case on the member path today, so no parser test can
// reach the trap. The trap is the next constant: three readers answered "which
// position is this column at?" three ways, and one of them — parseNeo4jMemberRoles
// — indexed the header lowercased and then looked its constants up raw. That is
// correct only while every constant happens to be lowercase. A member column
// spelled the way neo4jStatusColumn already is, `currentStatus`, would not have
// been found, and the parser answers a missing column by returning nil: every
// member's role gone from SourceRoles, FR-005's provenance silently empty, and
// no error raised anywhere.
//
// Both directions are asserted, because both sides of the lookup have to
// normalise or the two disagree: the header carries the server's spelling, the
// query carries the caller's.
func TestCypherColumnLookupIsIndifferentToCase(t *testing.T) {
	// A header as `--format plain` prints one: the server's own spellings, and
	// padding around them.
	columns := indexCypherColumns([]string{" currentStatus ", "ADDRESS", "writer"})

	t.Run("a caller asking with a mixed-case name", func(t *testing.T) {
		for _, asked := range []string{"currentStatus", "currentstatus", "CURRENTSTATUS", " currentStatus "} {
			index, ok := columns.at(asked)
			if !ok || index != 0 {
				t.Errorf("at(%q) = (%d, %t), want (0, true): a column constant spelled with a capital must still be found", asked, index, ok)
			}
		}
	})

	t.Run("a server spelling the header differently from the constant", func(t *testing.T) {
		index, ok := columns.at("address")
		if !ok || index != 1 {
			t.Errorf("at(%q) = (%d, %t), want (1, true): the header carries the server's spelling, not the caller's", "address", index, ok)
		}
	})

	t.Run("a column the result does not carry", func(t *testing.T) {
		if index, ok := columns.at("statusMessage"); ok {
			t.Errorf("at(%q) = (%d, true), want it absent", "statusMessage", index)
		}
	})

	// The same normalisation the header check has to use, or a header can name
	// a column that the lookup then cannot find — which is the shape of the
	// original divergence.
	if !indexCypherColumns([]string{" currentStatus ", "ADDRESS"}).has("currentstatus", "Address") {
		t.Error("has() = false, want the header recognised under either side's spelling")
	}
}
