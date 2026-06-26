package app

import (
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
	envOut, err := iops.Exec("infrahub-server", []string{"env"}, nil)
	if err != nil {
		return err
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

	connections := map[string]string{}
	for _, line := range strings.Split(envOut, "\n") {
		for _, name := range prefectConnectionEnvVars {
			if after, ok := strings.CutPrefix(line, name+"="); ok {
				connections[name] = after
			}
		}
	}

	// Apply the first match in preference order so newer Prefect naming wins
	// regardless of the order the container reports its environment.
	for _, name := range prefectConnectionEnvVars {
		if conn := connections[name]; conn != "" {
			iops.applyPrefectConnection(conn)
			break
		}
	}

	return nil
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

// applyPrefectConnection parses and applies Prefect database connection string
func (iops *InfrahubOps) applyPrefectConnection(connStr string) {
	if connStr == "" {
		return
	}

	// Normalize connection string
	re := regexp.MustCompile("postgres(.*)://(.*)")
	normalized := re.ReplaceAllString(connStr, "postgres://$2")

	connConfig, err := pgx.ParseConfig(normalized)
	if err != nil {
		logrus.Warnf("Could not parse PREFECT_API_DATABASE_CONNECTION_URL: %v", err)
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
}
