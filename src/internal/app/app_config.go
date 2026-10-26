package app

import (
	"fmt"
	"os"
	"regexp"
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

	for _, line := range strings.Split(envOut, "\n") {
		if after, ok := strings.CutPrefix(line, "INFRAHUB_DB_DATABASE="); ok && iops.config.Neo4jDatabase == "" {
			iops.config.Neo4jDatabase = after
		}
		if after, ok := strings.CutPrefix(line, "INFRAHUB_DB_USERNAME="); ok && iops.config.Neo4jUsername == "" {
			iops.config.Neo4jUsername = after
		}
		if after, ok := strings.CutPrefix(line, "INFRAHUB_DB_PASSWORD="); ok && iops.config.Neo4jPassword == "" {
			iops.config.Neo4jPassword = after
		}
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
	connections := map[string]string{}
	for _, line := range strings.Split(envOutput, "\n") {
		for _, name := range prefectConnectionEnvVars {
			if after, ok := strings.CutPrefix(line, name+"="); ok {
				connections[name] = after
			}
		}
	}

	for _, name := range prefectConnectionEnvVars {
		if conn := connections[name]; conn != "" {
			return name, conn
		}
	}
	return "", ""
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

	user, password, database, ok := parsePrefectConnection(connStr)
	if !ok {
		warnUnparsablePrefectConnection(name, source)
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
// the variable name, found in source, does not parse and is ignored. An
// operator who sets the URL to fix a rejected login learns this only from the
// warning, as no flag turns on debug logging. The warning names the variable
// and the source only: the URL holds the password, and the parse error can
// quote part of it.
func warnUnparsablePrefectConnection(name, source string) {
	logrus.Warnf("Ignoring %s from the %s: not a valid PostgreSQL URL; percent-encode special characters in the password", name, source)
}

// parsePrefectConnection extracts the user, password and database from a
// Prefect database connection string. The SQLAlchemy driver suffix
// (postgresql+asyncpg://) is normalized to postgres:// first so pgx accepts it.
// ok is false for an empty or unparsable string. The parse error is not
// returned: pgx redacts the password in the URL it quotes, but the cause it
// adds can quote part of the password, for example `invalid port ":pa" after
// host` for a password that starts with "pa" followed by an unencoded "#" or
// "/".
func parsePrefectConnection(connStr string) (user, password, database string, ok bool) {
	if connStr == "" {
		return "", "", "", false
	}

	re := regexp.MustCompile("postgres(.*)://(.*)")
	normalized := re.ReplaceAllString(connStr, "postgres://$2")

	connConfig, err := pgx.ParseConfig(normalized)
	if err != nil {
		return "", "", "", false
	}
	return connConfig.User, connConfig.Password, connConfig.Database, true
}
