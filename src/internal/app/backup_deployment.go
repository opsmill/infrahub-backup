package app

import (
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
)

const (
	resetDeploymentIDMaxAttempts = 10
	resetDeploymentIDRetryDelay  = 3 * time.Second
)

// resetDeploymentID rewrites the :Root node UUID so a restored instance does
// not share a deployment identity with the backup source. It runs against the
// already-restored Neo4j database and must be called before the Infrahub app
// containers are restarted, otherwise they will read (and cache) the old UUID.
//
// The enterprise `start database` command and the community SIGCONT resume
// both return before Neo4j is actually accepting queries, so the first
// attempts may legitimately fail while the database comes online — hence the
// retry loop.
func (iops *InfrahubOps) resetDeploymentID() error {
	newUUID := uuid.NewString()
	updatedAt := time.Now().UTC().Format(time.RFC3339)

	statement := []string{
		"--param", fmt.Sprintf("new_uuid => '%s'", newUUID),
		"--param", fmt.Sprintf("updated_at => '%s'", updatedAt),
		"MATCH (n:Root) SET n.uuid = $new_uuid, n.updated_at = $updated_at RETURN n",
	}

	args := append([]string{
		"cypher-shell",
		"-u", iops.config.Neo4jUsername,
		"-p" + iops.config.Neo4jPassword,
		"-d", iops.config.Neo4jDatabase,
	}, statement...)
	run := func() (string, error) {
		return iops.Exec("database", args, nil)
	}

	// A database outside the deployment has no `database` container to exec
	// into, so the statement goes through the restore workload the gate
	// prepared, over Bolt, with the credentials its secret supplies — the same
	// channel the seed and its online confirmation used.
	if restore := iops.externalRestores[serviceNeo4j]; restore != nil {
		uri, err := restore.boltURI()
		if err != nil {
			return err
		}

		external := append([]string{
			"cypher-shell", "--non-interactive", "--format", "plain",
			"-a", uri,
			"-d", restore.Endpoint.Database,
		}, statement...)
		run = func() (string, error) {
			return iops.execBoundedAgainst(
				externalDBProbeBound(iops.config), "resetting the deployment ID", restore.Endpoint.endpointTarget(),
				serviceNeo4j, external, restore.execOptions(),
			)
		}
	}

	logrus.Info("Resetting deployment ID on Root node...")

	var lastErr error
	var lastOutput string
	for attempt := 1; attempt <= resetDeploymentIDMaxAttempts; attempt++ {
		output, err := run()
		if err == nil {
			logrus.WithField("new_uuid", newUUID).Info("Deployment ID reset successfully")
			return nil
		}
		lastErr = err
		lastOutput = output
		logrus.Debugf("Reset deployment ID attempt %d/%d failed: %v", attempt, resetDeploymentIDMaxAttempts, err)
		if attempt < resetDeploymentIDMaxAttempts {
			time.Sleep(resetDeploymentIDRetryDelay)
		}
	}

	return fmt.Errorf("failed to reset deployment ID after %d attempts: %w\nOutput: %s",
		resetDeploymentIDMaxAttempts, lastErr, lastOutput)
}
