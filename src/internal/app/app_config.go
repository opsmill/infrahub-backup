package app

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

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
			if iops.config.Neo4jUsername == "" || iops.config.Neo4jPassword == "" {
				iops.neo4jCredentialLookupErr = err
				logrus.Warnf("Using the default Neo4j credentials: %v", err)
			} else {
				logrus.Warnf("Using the default Neo4j database name: %v", err)
			}
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
			iops.applyPrefectConnection(name, dbCredentialsFromOperator, value)
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
	envOut, err := iops.Exec("infrahub-server", []string{"env"}, nil)
	if err != nil {
		if line := commandErrorLine(envOut); line != "" {
			err = fmt.Errorf("%w: %s", err, line)
		}
		return fmt.Errorf("could not read the Neo4j credentials from the infrahub-server container: %w", err)
	}

	if iops.config.Neo4jDatabase == "" {
		iops.config.Neo4jDatabase = envOutputValue(envOut, "INFRAHUB_DB_DATABASE")
	}
	if iops.config.Neo4jUsername == "" {
		iops.config.Neo4jUsername = envOutputValue(envOut, "INFRAHUB_DB_USERNAME")
	}
	if iops.config.Neo4jPassword == "" {
		iops.config.Neo4jPassword = envOutputValue(envOut, "INFRAHUB_DB_PASSWORD")
	}

	return nil
}

// fetchPostgresCredentials fetches PostgreSQL credentials from the task-manager container
func (iops *InfrahubOps) fetchPostgresCredentials() error {
	envOut, err := iops.Exec("task-manager", []string{"env"}, nil)
	if err != nil {
		return err
	}

	if name, conn := prefectConnectionFromEnv(envOut); conn != "" {
		iops.applyPrefectConnection(name, dbCredentialsFromTaskManager, conn)
	}

	return nil
}

// prefectConnectionFromEnv returns the Prefect database connection string from
// `env` output and the variable that holds it, or "" for both when none of
// prefectConnectionEnvVars is set. The first match in preference order is
// returned so newer Prefect naming wins regardless of the order the container
// reports its environment.
func prefectConnectionFromEnv(envOutput string) (name, conn string) {
	for _, name := range prefectConnectionEnvVars {
		if conn := envOutputValue(envOutput, name); conn != "" {
			return name, conn
		}
	}
	return "", ""
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

// applyPrefectConnection parses and applies the Prefect database connection
// string that the variable name holds in source, and warns when it does not
// parse.
func (iops *InfrahubOps) applyPrefectConnection(name, source, connStr string) {
	if connStr == "" {
		return
	}

	user, password, database, err := parsePrefectConnection(connStr)
	if err != nil {
		warnUnparsablePrefectConnection(name, source, err)
		return
	}

	if database != "" {
		iops.config.PostgresDatabase = database
	}
	if user != "" {
		iops.config.PostgresUsername = user
	}
	if password != "" {
		iops.config.PostgresPassword = password
	}
}

// warnUnparsablePrefectConnection reports that the Prefect connection URL in
// the variable name, found in source, does not parse and is ignored, and why.
// An operator who sets the URL to fix a rejected login learns this only from
// the warning, as no flag turns on debug logging. The warning never quotes the
// URL, which holds the password: err is a parsePrefectConnection error, which
// names the problem only.
func warnUnparsablePrefectConnection(name, source string, err error) {
	logrus.Warnf("Ignoring %s from the %s: not a valid PostgreSQL URL: %v", name, source, err)
}

// parsePrefectConnection extracts the user, password and database from a
// Prefect database connection URL, such as
// postgresql+asyncpg://<user>:<password>@<host>:<port>/<database>. It reads
// the URL only: a part the URL leaves out is returned empty, and nothing is
// taken from the environment or the files of the machine that runs the tool.
// The error names what does not parse without quoting the URL, because Go's
// parse errors can quote part of the password: `invalid port ":pa" after
// host` for a password that starts with "pa" followed by an unencoded "#" or
// "/".
func parsePrefectConnection(connStr string) (user, password, database string, err error) {
	u, parseErr := url.Parse(connStr)
	if parseErr != nil {
		return "", "", "", errors.New(prefectConnectionParseProblem(parseErr))
	}
	if !strings.HasPrefix(u.Scheme, "postgres") {
		return "", "", "", errors.New("the URL does not start with postgresql:// or postgres://")
	}
	if u.User != nil {
		user = u.User.Username()
		password, _ = u.User.Password()
	}
	return user, password, strings.TrimPrefix(u.Path, "/"), nil
}

// prefectConnectionParseProblem names the part of a connection URL that
// url.Parse rejected, in words that quote no part of the URL.
func prefectConnectionParseProblem(err error) string {
	var escapeErr url.EscapeError
	var hostErr url.InvalidHostError
	switch {
	case errors.As(err, &escapeErr):
		return "a % sign is not followed by two hexadecimal digits; write a % in the password as %25"
	case errors.As(err, &hostErr):
		return "the host name holds a character that is not allowed"
	case strings.Contains(err.Error(), "invalid userinfo"):
		return "the user name or password holds a character that is not allowed, or a ${...} reference; percent-encode special characters in the password"
	case strings.Contains(err.Error(), "invalid port"):
		return "the text after the host name is not a port number, as when the password holds an unencoded / or #; percent-encode special characters in the password"
	default:
		return "the URL does not parse"
	}
}
