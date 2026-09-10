package app

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/sirupsen/logrus"
)

// Default database credentials
const (
	defaultNeo4jDatabase    = "neo4j"
	defaultNeo4jUsername    = "neo4j"
	defaultNeo4jPassword    = "admin"
	defaultPostgresDatabase = "prefect"
	defaultPostgresUsername = "postgres"
	defaultPostgresPassword = "prefect"
)

// prefectConnectionEnvVars lists the environment variables that may carry the
// task-manager PostgreSQL connection string, newest Prefect naming first.
// Prefect 3.x (and current prefect-helm) exposes
// PREFECT_SERVER_DATABASE_CONNECTION_URL, while Prefect 2.x used
// PREFECT_API_DATABASE_CONNECTION_URL. The connection string carries the
// database owner (e.g. the unprivileged "prefect" user) and its password, which
// is what pg_dump must authenticate as — the "postgres" superuser is not
// guaranteed to have a usable password (prefect-helm sets
// auth.enablePostgresUser=false by default).
var prefectConnectionEnvVars = []string{
	"PREFECT_SERVER_DATABASE_CONNECTION_URL",
	"PREFECT_API_DATABASE_CONNECTION_URL",
}

// The Infrahub settings that record where the Neo4j database is, read out of
// the server's own environment by applyNeo4jEnvironment.
//
// Named rather than written inline at the one place they are read, because
// FR-004's failure has to state where discovery looked: an operator told only
// "supply --neo4j-address" cannot tell a setting they never set from a
// component this run could not read, and those have different remedies. A
// constant is what keeps the message and the reader from drifting apart.
const (
	neo4jAddressEnvVar = "INFRAHUB_DB_ADDRESS"
	neo4jPortEnvVar    = "INFRAHUB_DB_PORT"
)

// The deployment components discovery reads those settings out of. Neo4j's are
// Infrahub's own, so the server carries them; the task manager's endpoint
// exists only inside the Prefect connection URL, which the task manager
// carries.
const (
	componentInfrahubServer = "infrahub-server"
	componentTaskManager    = "task-manager"
)

// fetchDatabaseCredentials retrieves database credentials from environment or containers
func (iops *InfrahubOps) fetchDatabaseCredentials() error {
	if _, err := iops.ensureBackend(); err != nil {
		return err
	}

	// Try to get credentials from environment first
	iops.loadCredentialsFromEnvironment()

	// Fetch Neo4j credentials if not fully configured
	if !iops.hasNeo4jCredentials() {
		if err := iops.fetchNeo4jCredentials(); err != nil {
			logrus.Warnf("Could not fetch Neo4j credentials from container: %v", err)
		}
		iops.applyNeo4jDefaults()
	}

	// Fetch PostgreSQL credentials if not fully configured
	if !iops.hasPostgresCredentials() {
		if err := iops.fetchPostgresCredentials(); err != nil {
			logrus.Warnf("Could not fetch PostgreSQL credentials from container: %v", err)
		}
		iops.applyPostgresDefaults()
	}

	return nil
}

// loadCredentialsFromEnvironment loads credentials from environment variables
func (iops *InfrahubOps) loadCredentialsFromEnvironment() {
	if value := os.Getenv("INFRAHUB_DB_DATABASE"); value != "" {
		iops.config.Neo4jDatabase = value
	}
	if value := os.Getenv("INFRAHUB_DB_USERNAME"); value != "" {
		iops.config.Neo4jUsername = value
	}
	if value := os.Getenv("INFRAHUB_DB_PASSWORD"); value != "" {
		iops.config.Neo4jPassword = value
	}

	for _, name := range prefectConnectionEnvVars {
		if value := os.Getenv(name); value != "" {
			// The operator handed us the same connection URL the deployment
			// keeps, so the external path has nothing left to ask for — if it
			// could be read, which is applyPrefectConnection's to decide.
			iops.applyPrefectConnection(value)
			break
		}
	}
}

// hasNeo4jCredentials checks if all Neo4j credentials are configured
func (iops *InfrahubOps) hasNeo4jCredentials() bool {
	return iops.config.Neo4jDatabase != "" &&
		iops.config.Neo4jUsername != "" &&
		iops.config.Neo4jPassword != ""
}

// hasPostgresCredentials checks if all PostgreSQL credentials are configured
func (iops *InfrahubOps) hasPostgresCredentials() bool {
	return iops.config.PostgresDatabase != "" &&
		iops.config.PostgresUsername != "" &&
		iops.config.PostgresPassword != ""
}

// fetchNeo4jCredentials fetches Neo4j credentials from the infrahub-server container
func (iops *InfrahubOps) fetchNeo4jCredentials() error {
	envOut, err := iops.Exec(componentInfrahubServer, []string{"env"}, nil)
	if err != nil {
		return err
	}

	iops.applyNeo4jEnvironment(envOut)
	// The server's environment has now been read, whatever it turned out to
	// contain, so the external path does not ask a second time.
	iops.discoveredNeo4j.read = true

	return nil
}

// ensureNeo4jDiscovery makes sure the deployment's own view of where its Neo4j
// database lives has been read, and reads it if not.
//
// fetchNeo4jCredentials — the only caller of applyNeo4jEnvironment, and so the
// only place the address settings are read — runs only when the credentials are
// not already fully configured. That gate is right for credentials and wrong for
// the address: a deployment whose credentials come from the operator's own
// environment would never reach the code that reads the address, and the address
// is needed precisely when there is no container to fall back on.
//
// This widens the gate in the one direction FR-015 permits. The query is issued
// only after a database has been positively established as external, so a
// deployment whose databases are all internal never reaches here and issues no
// exec it does not issue today. It asks once per run: a deployment that exports
// no address has still been asked, and asking again would not change the answer.
//
// The exec is time-bounded, unlike the credential fetch it widens (FR-025). It
// runs only on the external path, so bounding it leaves fetchNeo4jCredentials —
// the shared path an internal deployment takes — exactly as it was (FR-015).
func (iops *InfrahubOps) ensureNeo4jDiscovery() error {
	if iops.discoveredNeo4j.read {
		return nil
	}

	bound := externalDBControlBound(iops.config)
	envOut, err := iops.execBoundedAgainst(bound, "reading the database settings", "the "+componentInfrahubServer+" container", componentInfrahubServer, []string{"env"}, nil)
	if err != nil {
		return fmt.Errorf("failed to read the database settings from infrahub-server: %w", err)
	}

	iops.applyNeo4jEnvironment(envOut)
	iops.discoveredNeo4j.read = true

	return nil
}

// ensurePostgresDiscovery is ensureNeo4jDiscovery for the task-manager database,
// whose endpoint the deployment records only inside the Prefect connection URL.
//
// It applies the endpoint alone and leaves the credentials untouched. The
// credentials already have their own resolution, in which the operator's
// environment wins over the deployment's, and re-reading the connection URL for
// an address must not quietly overrule it.
//
// It is time-bounded for the same reason, and with the same FR-015 argument, as
// ensureNeo4jDiscovery: this is the external path's own read, and
// fetchPostgresCredentials keeps the unbounded exec an internal deployment
// already makes.
func (iops *InfrahubOps) ensurePostgresDiscovery() error {
	if iops.discoveredPostgres.read {
		return nil
	}

	bound := externalDBControlBound(iops.config)
	envOut, err := iops.execBoundedAgainst(bound, "reading the task-manager database connection", "the "+componentTaskManager+" container", componentTaskManager, []string{"env"}, nil)
	if err != nil {
		return fmt.Errorf("failed to read the task-manager database connection from task-manager: %w", err)
	}

	iops.applyPrefectEndpointFrom(prefectConnectionFromEnvironment(envOut))
	// Recorded as read even when the deployment exports no connection URL: the
	// question has been put, and the answer will not change within the run.
	iops.discoveredPostgres.read = true

	return nil
}

// applyNeo4jEnvironment reads what the deployment's own server says about its
// database out of an `env` dump: the credentials this tool has always read, and
// — additively — where the database is and how a client reaches it. The address
// keys are real Infrahub settings that this tool simply never looked at
// (research R4).
//
// Reading them changes nothing for a deployment that does not set them: an
// absent variable leaves its field untouched, and every field they fill is
// consulted only once a database has been positively established as external
// (FR-015).
func (iops *InfrahubOps) applyNeo4jEnvironment(envOut string) {
	env := envValues(envOut)

	if value, ok := env["INFRAHUB_DB_DATABASE"]; ok && iops.config.Neo4jDatabase == "" {
		iops.config.Neo4jDatabase = value
	}
	if value, ok := env["INFRAHUB_DB_USERNAME"]; ok && iops.config.Neo4jUsername == "" {
		iops.config.Neo4jUsername = value
	}
	if value, ok := env["INFRAHUB_DB_PASSWORD"]; ok && iops.config.Neo4jPassword == "" {
		iops.config.Neo4jPassword = value
	}

	// The host list, in the shape --from already wants: comma-separated
	// host[:port] members. Recorded as discovered rather than as the
	// configured address, so --neo4j-address still wins over it.
	if value := env[neo4jAddressEnvVar]; value != "" {
		iops.discoveredNeo4j.Address = value
	}
	// The Bolt port the application itself uses — what a member of the
	// address list inherits when it carries no port. It is not the backup
	// listener's port; see resolveNeo4jBackupPort.
	if value := env[neo4jPortEnvVar]; value != "" {
		if port, err := parsePort(value); err == nil {
			iops.discoveredNeo4j.ClientPort = port
		} else {
			logrus.Warnf("Ignoring %s reported by the deployment: %v", neo4jPortEnvVar, err)
		}
	}
	if value := env["INFRAHUB_DB_PROTOCOL"]; value != "" && iops.config.ExternalDB.Neo4jProtocol == "" {
		iops.config.ExternalDB.Neo4jProtocol = value
	}
	if value, ok := env["INFRAHUB_DB_TLS_ENABLED"]; ok {
		iops.config.ExternalDB.Neo4jTLS.Enabled = parseDeploymentBool("INFRAHUB_DB_TLS_ENABLED", value, iops.config.ExternalDB.Neo4jTLS.Enabled)
	}
	if value, ok := env["INFRAHUB_DB_TLS_INSECURE"]; ok {
		iops.config.ExternalDB.Neo4jTLS.Insecure = parseDeploymentBool("INFRAHUB_DB_TLS_INSECURE", value, iops.config.ExternalDB.Neo4jTLS.Insecure)
	}
	if value := env["INFRAHUB_DB_TLS_CA_FILE"]; value != "" {
		iops.config.ExternalDB.Neo4jTLS.CAFile = value
	}
}

// envValues reads an `env` dump into a map, one entry per KEY=VALUE line, the
// value being everything after the first `=` so a password carrying one
// survives. A key the dump repeats keeps its last value. The two readers of
// such dumps used to walk the lines themselves, probing each against every
// name they knew; adding a setting now adds a lookup rather than a probe.
func envValues(envOut string) map[string]string {
	values := map[string]string{}
	for _, line := range strings.Split(envOut, "\n") {
		if key, value, ok := strings.Cut(line, "="); ok {
			values[key] = value
		}
	}

	return values
}

// parseDeploymentBool reads a boolean setting as the deployment wrote it,
// keeping the current value and warning when the text is not a boolean: a
// setting nobody can read is not evidence for either answer, and quietly
// choosing false for a TLS switch would downgrade a connection the deployment
// meant to protect.
func parseDeploymentBool(name, text string, current bool) bool {
	if text == "" {
		return current
	}

	value, err := strconv.ParseBool(text)
	if err != nil {
		logrus.Warnf("Ignoring %s reported by the deployment: %q is not a boolean", name, text)

		return current
	}

	return value
}

// fetchPostgresCredentials fetches PostgreSQL credentials from the task-manager
// container.
//
// The read is recorded only when the connection URL could actually be read,
// which is the same reading loadCredentialsFromEnvironment takes of the same
// return value and for the same reason: a URL that did not parse — or a
// container that exports none at all — applied nothing, and recording the read
// anyway suppresses ensurePostgresDiscovery, the one read that would have
// supplied the endpoint a restore writes into. Recording it unconditionally
// here made this the second instance of the defect the bool was added to fix.
//
// Unlike fetchNeo4jCredentials, "the question has been put" is not the right
// reading: that one reads several independent settings out of one `env` dump,
// so the dump is the answer whatever it contained, while this one reads a
// single URL and has nothing at all if that URL is unusable.
func (iops *InfrahubOps) fetchPostgresCredentials() error {
	envOut, err := iops.Exec(componentTaskManager, []string{"env"}, nil)
	if err != nil {
		return err
	}

	iops.applyPrefectConnection(prefectConnectionFromEnvironment(envOut))

	return nil
}

// prefectConnectionFromEnvironment picks the task-manager connection URL out of
// an `env` dump, preferring the newest Prefect naming regardless of the order
// the container reports its environment. It returns "" when the container
// carries none, which every caller treats as nothing to apply.
func prefectConnectionFromEnvironment(envOut string) string {
	connections := envValues(envOut)
	for _, name := range prefectConnectionEnvVars {
		if conn := connections[name]; conn != "" {
			return conn
		}
	}

	return ""
}

// applyNeo4jDefaults applies default Neo4j credentials
func (iops *InfrahubOps) applyNeo4jDefaults() {
	if iops.config.Neo4jDatabase == "" {
		iops.config.Neo4jDatabase = defaultNeo4jDatabase
	}
	if iops.config.Neo4jUsername == "" {
		iops.config.Neo4jUsername = defaultNeo4jUsername
	}
	if iops.config.Neo4jPassword == "" {
		iops.config.Neo4jPassword = defaultNeo4jPassword
	}
}

// applyPostgresDefaults applies default PostgreSQL credentials
func (iops *InfrahubOps) applyPostgresDefaults() {
	if iops.config.PostgresDatabase == "" {
		iops.config.PostgresDatabase = defaultPostgresDatabase
	}
	if iops.config.PostgresUsername == "" {
		iops.config.PostgresUsername = defaultPostgresUsername
	}
	if iops.config.PostgresPassword == "" {
		iops.config.PostgresPassword = defaultPostgresPassword
	}
}

// applyPrefectConnection parses and applies a Prefect database connection
// string: the credentials it carries, where it says the database lives, and
// that the connection has been read. That last is recorded only when the URL
// parsed, because an unreadable URL applied nothing — recording the read anyway
// suppressed the deployment-side read that would have supplied the endpoint:
// one typo in an operator's environment, and it was never discovered at all.
func (iops *InfrahubOps) applyPrefectConnection(connStr string) {
	connConfig := parsePrefectConnection(connStr)
	if connConfig == nil {
		return
	}

	if connConfig.Database != "" {
		iops.config.PostgresDatabase = connConfig.Database
	}
	if connConfig.User != "" {
		iops.config.PostgresUsername = connConfig.User
	}
	if connConfig.Password != "" {
		iops.config.PostgresPassword = connConfig.Password
	}

	iops.applyPrefectEndpoint(connStr)
	iops.discoveredPostgres.read = true
}

// applyPrefectEndpointFrom applies only where the task-manager database lives,
// from a connection URL, leaving the credentials as they already resolved. It is
// the external path's discovery read — see ensurePostgresDiscovery for why the
// credentials are deliberately left alone.
func (iops *InfrahubOps) applyPrefectEndpointFrom(connStr string) {
	// Parsed first and then read for its endpoint: a string pgx cannot read is
	// not a connection URL, and scraping a host out of one anyway would adopt
	// text that means nothing.
	if parsePrefectConnection(connStr) != nil {
		iops.applyPrefectEndpoint(connStr)
	}
}

// applyPrefectEndpoint records where the task-manager database lives. The host
// and port were parsed and thrown away until this feature. They are the only
// record the deployment keeps of where that database is, so an externally hosted
// one is discoverable from exactly here (research R4). Kept as discovered rather
// than as the configured address, so --postgres-address still wins over them.
//
// It reads the connection string rather than pgx's parse of it, and that is the
// whole of the difference. pgx.ParseConfig fills a Host and Port it was not
// given, from libpq's own defaults: PGHOST and PGPORT out of the *operator's*
// environment, and failing those a local socket directory. So a URL that states
// no host — `postgres:///prefect`, which the operator's own PGHOST completes for
// them every day — made the discovered "external database" the machine the tool
// happens to be running on. The restore path writes into what discovery
// resolved. The `!= ""` and `!= 0` guards below could never fire on the pgx
// values, because pgx always supplies both.
func (iops *InfrahubOps) applyPrefectEndpoint(connStr string) {
	host, port, caFile := statedPrefectConnection(connStr)
	if host != "" {
		iops.discoveredPostgres.Address = host
	}
	if port != 0 {
		iops.discoveredPostgres.ClientPort = port
	}
	if caFile != "" {
		iops.config.ExternalDB.PostgresTLS.CAFile = caFile
	}
}

// statedPrefectSSLRootCert returns the certificate authority the connection
// string names, and "" when it names none.
//
// It is the PostgreSQL half of what INFRAHUB_DB_TLS_CA_FILE is for Neo4j: the
// anchor the deployment's own client verifies this server against, and so the
// anchor the transient workload's client needs if it is to verify at all rather
// than only encrypt. Read here rather than from pgx's parse of the URL for the
// reason applyPrefectEndpoint reads the string itself — pgx loads the file at
// parse time, from a path that exists inside the *deployment's* container and
// not on the machine running this tool, so asking pgx for it turns a usable URL
// into a parse failure.
func statedPrefectSSLRootCert(connStr string) string {
	_, _, caFile := statedPrefectConnection(connStr)

	return caFile
}

// statedPrefectConnection returns what a connection string itself states — the
// host and port the database is at, and the certificate authority it names —
// and nothing else: an empty host means the string named none, not that the
// database is somewhere convenient.
//
// Both shapes libpq accepts are read, because both reach here — the URL form
// the deployment writes, including the query-parameter form a socket directory
// takes (`postgres:///prefect?host=/var/run/postgresql`), and the keyword form
// an operator may have in their environment. One parse yields all three
// values; the endpoint and the CA used to be read by two functions that parsed
// the same string twice.
func statedPrefectConnection(connStr string) (host string, port int, caFile string) {
	trimmed := normalizePrefectConnection(connStr)
	if trimmed == "" {
		return "", 0, ""
	}
	if !strings.Contains(trimmed, "://") {
		host, port = statedKeywordEndpoint(trimmed)

		return host, port, statedKeywordValue(trimmed, "sslrootcert")
	}

	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", 0, ""
	}

	query := parsed.Query()
	host, port = parsed.Hostname(), statedPort(parsed.Port())
	if host == "" {
		host = strings.TrimSpace(query.Get("host"))
	}
	if port == 0 {
		port = statedPort(query.Get("port"))
	}

	return host, port, strings.TrimSpace(query.Get("sslrootcert"))
}

// statedKeywordEndpoint reads the keyword/value connection form —
// `host=db.example.com port=5432 dbname=prefect`.
func statedKeywordEndpoint(connStr string) (string, int) {
	fields := statedKeywordFields(connStr)

	return fields["host"], statedPort(fields["port"])
}

// statedKeywordValue is statedKeywordEndpoint for one keyword, so a second
// setting read out of the same form does not bring a second copy of the parse
// with it.
func statedKeywordValue(connStr, key string) string {
	return statedKeywordFields(connStr)[key]
}

// statedKeywordFields splits the keyword/value connection form into its
// settings, lower-casing the keywords and stripping the quoting libpq allows
// around a value.
func statedKeywordFields(connStr string) map[string]string {
	fields := map[string]string{}
	for _, field := range strings.Fields(connStr) {
		key, value, found := strings.Cut(field, "=")
		if !found {
			continue
		}
		fields[strings.ToLower(strings.TrimSpace(key))] = strings.Trim(strings.TrimSpace(value), `'"`)
	}

	return fields
}

// statedPort is parsePort for a value a connection string may simply not carry:
// the range check is the same one every other port in this package gets, and a
// port that is absent or unusable is reported as "not stated" rather than as an
// error, because the caller's next step is to leave the field alone.
func statedPort(text string) int {
	port, err := parsePort(strings.TrimSpace(text))
	if err != nil {
		return 0
	}

	return port
}

// parsePrefectConnection reads a Prefect connection URL, returning nil for an
// absent or unreadable one so every caller has a single thing to check.
func parsePrefectConnection(connStr string) *pgx.ConnConfig {
	if connStr == "" {
		return nil
	}

	connConfig, err := pgx.ParseConfig(normalizePrefectConnection(connStr))
	if err != nil {
		logrus.Warnf("Could not parse the task-manager database connection URL: %v", err)
		return nil
	}

	return connConfig
}

// normalizePrefectConnection rewrites the driver-qualified schemes Prefect
// writes — postgresql+asyncpg://, postgresql+psycopg:// — to the plain one, so
// the URL is read the same way wherever it is read.
func normalizePrefectConnection(connStr string) string {
	return prefectConnectionScheme.ReplaceAllString(strings.TrimSpace(connStr), "postgres://$2")
}

var prefectConnectionScheme = regexp.MustCompile("postgres(.*)://(.*)")
