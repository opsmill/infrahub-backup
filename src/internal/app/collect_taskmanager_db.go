package app

import (
	"os"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// Task manager database queries (specs/006-taskmgr-db-storage):
//   - the service and bundle directory, and the application name every session
//     reports to PostgreSQL, so an operator can find and stop it;
//   - the time limits: each breakdown table gets collectDBQueryTimeout and the
//     storage query collectExecTimeout. PostgreSQL enforces both, and the
//     tool-side bound adds collectDBQueryGrace so PostgreSQL's own timeout error
//     arrives first (research R3);
//   - dbQueryTimeoutEnvVar, which lowers both limits for the end-to-end tests
//     and is not documented for operators (contracts/cli.md);
//   - the skip reason when task-manager-db has no replica (FR-010): the task
//     manager may then use a PostgreSQL server outside the deployment, which the
//     tool cannot reach.
const (
	taskManagerDBService           = "task-manager-db"
	taskManagerDBBundleDir         = "task-manager-db"
	taskManagerDBAppName           = "infrahub-collect"
	collectDBQueryTimeout          = 5 * time.Minute
	collectDBQueryGrace            = 15 * time.Second
	dbQueryTimeoutEnvVar           = "INFRAHUB_TASK_MANAGER_DB_QUERY_TIMEOUT"
	taskManagerDBNotDeployedReason = "service not deployed; the task manager may use an external PostgreSQL server"
)

// Where resolveTaskManagerDBCredentials found the credentials. Only the source
// is ever logged, never a value.
const (
	dbCredentialsFromOperator    = "operator environment"
	dbCredentialsFromTaskManager = "task-manager environment"
	dbCredentialsFromDatabase    = "task-manager-db environment"
	dbCredentialsFromDefaults    = "defaults"
)

// taskManagerDBCredentials is the login the task manager database queries use,
// and where it was found.
type taskManagerDBCredentials struct {
	user     string
	password string
	database string
	source   string
}

// resolveTaskManagerDBCredentials returns the login for the task manager
// database, resolving it on the first call of a run and caching it on the
// collect context so every collector shares one lookup (critique E1). It tries,
// in order (research R8):
//  1. a Prefect connection URL in the operator's environment;
//  2. a Prefect connection URL in the task-manager container's environment;
//  3. POSTGRES_USER, POSTGRES_PASSWORD, and POSTGRES_DB or POSTGRES_DATABASE in
//     the task-manager-db container's environment, which still answers when the
//     Prefect server is down (critique X1);
//  4. the defaults the backup tool uses.
//
// It reads but never changes iops.config. The `env` output of both containers
// holds secrets: it is parsed here and never written to the bundle or the log.
func resolveTaskManagerDBCredentials(cc *collectContext) taskManagerDBCredentials {
	if cc.dbCredentials != nil {
		return *cc.dbCredentials
	}

	creds := lookupTaskManagerDBCredentials(cc)
	logrus.Debugf("Using task manager database credentials from the %s", creds.source)
	cc.dbCredentials = &creds
	return creds
}

func lookupTaskManagerDBCredentials(cc *collectContext) taskManagerDBCredentials {
	for _, name := range prefectConnectionEnvVars {
		if conn := os.Getenv(name); conn != "" {
			if creds, ok := credentialsFromPrefectConnection(conn, dbCredentialsFromOperator); ok {
				return creds
			}
			break
		}
	}

	if envOut, err := cc.execDumpTimeout(collectExecTimeout, "task-manager", []string{"env"}); err != nil {
		logrus.Debugf("Could not read the task-manager environment for database credentials: %v", err)
	} else if conn := prefectConnectionFromEnv(envOut); conn != "" {
		if creds, ok := credentialsFromPrefectConnection(conn, dbCredentialsFromTaskManager); ok {
			return creds
		}
	}

	if envOut, err := cc.execDumpTimeout(collectExecTimeout, taskManagerDBService, []string{"env"}); err != nil {
		logrus.Debugf("Could not read the %s environment for database credentials: %v", taskManagerDBService, err)
	} else if creds, ok := credentialsFromPostgresEnv(envOut); ok {
		return creds
	}

	return taskManagerDBCredentials{
		user:     defaultPostgresUsername,
		password: defaultPostgresPassword,
		database: defaultPostgresDatabase,
		source:   dbCredentialsFromDefaults,
	}
}

// credentialsFromPrefectConnection parses a Prefect connection URL found in
// source. A part the URL leaves out takes the default, as applyPostgresDefaults
// does for the backup tool.
func credentialsFromPrefectConnection(conn, source string) (taskManagerDBCredentials, bool) {
	user, password, database, ok := parsePrefectConnection(conn)
	if !ok {
		logrus.Debugf("Could not parse the Prefect database connection URL from the %s", source)
		return taskManagerDBCredentials{}, false
	}
	return taskManagerDBCredentials{
		user:     valueOrDefault(user, defaultPostgresUsername),
		password: valueOrDefault(password, defaultPostgresPassword),
		database: valueOrDefault(database, defaultPostgresDatabase),
		source:   source,
	}, true
}

// credentialsFromPostgresEnv reads the database owner from the `env` output of
// task-manager-db. The official image names the database POSTGRES_DB, the
// bitnami image POSTGRES_DATABASE. The login is used only when both the user
// and the password are set.
func credentialsFromPostgresEnv(envOutput string) (taskManagerDBCredentials, bool) {
	user := envOutputValue(envOutput, "POSTGRES_USER")
	password := envOutputValue(envOutput, "POSTGRES_PASSWORD")
	if user == "" || password == "" {
		logrus.Debugf("The %s environment does not name both a database user and a password", taskManagerDBService)
		return taskManagerDBCredentials{}, false
	}

	database := envOutputValue(envOutput, "POSTGRES_DB")
	if database == "" {
		database = envOutputValue(envOutput, "POSTGRES_DATABASE")
	}
	return taskManagerDBCredentials{
		user:     user,
		password: password,
		database: valueOrDefault(database, defaultPostgresDatabase),
		source:   dbCredentialsFromDatabase,
	}, true
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

func valueOrDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
