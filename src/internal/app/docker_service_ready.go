package app

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// databaseReadyTimeout bounds how long a restore waits for the database
// container to come back healthy before it starts the services that depend on
// it. A Neo4j restart after a load is tens of seconds; the bound is for a
// database that never comes back, so it fails the run with the state it saw.
const databaseReadyTimeout = 5 * time.Minute

// dockerServiceReadyPollInterval is the pause between two readings of the
// container state.
const dockerServiceReadyPollInterval = time.Second

// dockerServiceReadyConfirmations is how many consecutive ready readings the
// wait needs. One is not enough: right after the Community load resumes the
// suspended Neo4j process, that process finishes the shutdown it was stopped
// in, and for that instant the container still reads as running.
const dockerServiceReadyConfirmations = 2

// dockerContainerState is the part of `docker inspect`'s State the readiness
// wait reads.
type dockerContainerState struct {
	Status  string `json:"Status"`
	Running bool   `json:"Running"`
	Health  *struct {
		Status        string `json:"Status"`
		FailingStreak int    `json:"FailingStreak"`
	} `json:"Health"`
}

// dockerReadiness says whether every container of a service can satisfy a
// `depends_on: condition: service_healthy` (or `service_started` when the
// service has no healthcheck), and whether one of them has to be started for
// that to ever happen.
//
// A container whose health still reads "healthy" while its failing streak is
// above zero is not ready: Docker keeps the last status until the retries run
// out, so a Neo4j that was down for the length of a load reads "healthy" with
// a streak of failed checks behind it, and that reading is stale.
func dockerReadiness(states []dockerContainerState) (ready bool, needsStart bool, detail string) {
	if len(states) == 0 {
		return false, false, "no container"
	}
	ready = true
	details := make([]string, 0, len(states))
	for _, st := range states {
		switch {
		case st.Status == "exited" || st.Status == "created" || st.Status == "dead":
			ready = false
			needsStart = true
			details = append(details, st.Status)
		case !st.Running:
			ready = false
			details = append(details, st.Status)
		case st.Health == nil:
			details = append(details, "running")
		case st.Health.Status == "healthy" && st.Health.FailingStreak == 0:
			details = append(details, "healthy")
		default:
			ready = false
			details = append(details, fmt.Sprintf("%s (failing streak %d)", st.Health.Status, st.Health.FailingStreak))
		}
	}
	return ready, needsStart, strings.Join(details, ", ")
}

// waitForDockerServiceReady polls observe until dockerReadiness reports the
// service ready on dockerServiceReadyConfirmations consecutive readings, or
// timeout passes. A container that exited and that its restart policy does not
// bring back is started once per such reading, because `docker compose start`
// of a dependent refuses an exited dependency instead of starting it.
func waitForDockerServiceReady(
	service string,
	observe func() ([]dockerContainerState, error),
	start func() error,
	timeout, interval time.Duration,
) error {
	deadline := time.Now().Add(timeout)
	confirmed := 0
	var last string
	for {
		states, err := observe()
		if err != nil {
			confirmed = 0
			last = err.Error()
		} else {
			ready, needsStart, detail := dockerReadiness(states)
			last = detail
			switch {
			case ready:
				confirmed++
				if confirmed >= dockerServiceReadyConfirmations {
					return nil
				}
			case needsStart:
				confirmed = 0
				logrus.Infof("Starting %s, which is %s...", service, detail)
				if err := start(); err != nil {
					last = fmt.Sprintf("%s; start failed: %v", detail, err)
				}
			default:
				confirmed = 0
			}
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%s did not become healthy within %s (last state: %s)", service, timeout, last)
		}
		time.Sleep(interval)
	}
}

// serviceContainerStates reads the State of every container of one compose
// service, stopped ones included.
func (d *DockerBackend) serviceContainerStates(service string) ([]dockerContainerState, error) {
	output, err := d.executor.runCommand("docker", d.composeArgs("ps", "-a", "-q", service)...)
	if err != nil {
		return nil, composeLifecycleError(output, err)
	}
	ids := strings.Fields(output)
	states := make([]dockerContainerState, 0, len(ids))
	for _, id := range ids {
		out, err := d.executor.runCommand("docker", "inspect", "--format", "{{json .State}}", id)
		if err != nil {
			return nil, composeLifecycleError(out, err)
		}
		var st dockerContainerState
		if err := json.Unmarshal([]byte(out), &st); err != nil {
			return nil, fmt.Errorf("failed to parse state of container %s: %w", id, err)
		}
		states = append(states, st)
	}
	return states, nil
}

// WaitServiceReady waits until every container of service is running and,
// where it has a healthcheck, freshly healthy.
func (d *DockerBackend) WaitServiceReady(service string, timeout time.Duration) error {
	return waitForDockerServiceReady(
		service,
		func() ([]dockerContainerState, error) { return d.serviceContainerStates(service) },
		func() error { return d.Start(service) },
		timeout, dockerServiceReadyPollInterval,
	)
}

// startAppServicesAfterRestore brings infrahub-server and task-worker back at
// the end of a restore, once the database they depend on is ready.
//
// On Docker, Infrahub's compose file declares infrahub-server
// `depends_on: database: condition: service_healthy`, and `docker compose
// start` checks that condition once and refuses — "dependency failed to start:
// container ... exited" — rather than waiting through it. The Community load
// leaves Neo4j finishing a shutdown that its restart policy then undoes, so the
// start raced the database's restart. Kubernetes has no such gate: scaling the
// deployments up is accepted whatever state the database is in.
func (iops *InfrahubOps) startAppServicesAfterRestore() error {
	backend, err := iops.ensureBackend()
	if err != nil {
		return err
	}
	if docker, ok := backend.(*DockerBackend); ok {
		logrus.Info("Waiting for the database to be healthy...")
		if err := docker.WaitServiceReady(serviceNeo4j, databaseReadyTimeout); err != nil {
			return fmt.Errorf("failed to restart infrahub services: %w", err)
		}
	}
	logrus.Info("Restarting Infrahub services...")
	if err := iops.StartServices("infrahub-server", "task-worker"); err != nil {
		return fmt.Errorf("failed to restart infrahub services: %w", err)
	}
	return nil
}
