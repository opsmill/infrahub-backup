package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// defaultPrefectPaginationSize is the pagination size used for the running-tasks
// check when the task-manager cap cannot be discovered. It matches Prefect's
// built-in PREFECT_API_DEFAULT_LIMIT default.
const defaultPrefectPaginationSize = 200

type tasksOutput struct {
	Id   string `json:"id"`
	Name string `json:"title"`
}

// prefectLimitRe extracts the pagination cap from a task-manager 422 response,
// e.g. "Invalid limit: must be less than or equal to 200.". The captured number
// is Prefect's effective PREFECT_API_DEFAULT_LIMIT.
var prefectLimitRe = regexp.MustCompile(`must be less than or equal to (\d+)`)

// parsePrefectMaxLimit returns the server-enforced pagination cap reported in a
// task-manager limit error, scanning each provided string (error text, command
// output).
func parsePrefectMaxLimit(parts ...string) (int, bool) {
	for _, part := range parts {
		if match := prefectLimitRe.FindStringSubmatch(part); match != nil {
			if limit, err := strconv.Atoi(match[1]); err == nil && limit > 0 {
				return limit, true
			}
		}
	}
	return 0, false
}

// discoverPrefectPaginationLimit reads PREFECT_API_DEFAULT_LIMIT from the
// task-manager container so the running-tasks check can request a valid
// pagination size on the first try. Returns false when the value is unset,
// invalid, or the container cannot be reached (the caller then falls back to the
// default and the reactive retry).
func (iops *InfrahubOps) discoverPrefectPaginationLimit() (int, bool) {
	output, err := iops.Exec("task-manager", []string{"sh", "-c", `printf %s "$PREFECT_API_DEFAULT_LIMIT"`}, nil)
	if err != nil {
		logrus.Debugf("Could not read PREFECT_API_DEFAULT_LIMIT from task-manager: %v", err)
		return 0, false
	}
	value := strings.TrimSpace(output)
	if value == "" {
		return 0, false
	}
	limit, err := strconv.Atoi(value)
	if err != nil || limit <= 0 {
		logrus.Debugf("Ignoring invalid PREFECT_API_DEFAULT_LIMIT value %q from task-manager", value)
		return 0, false
	}
	return limit, true
}

func (iops *InfrahubOps) waitForRunningTasks() error {
	useInfrahubctl := true
	var scriptContent string

	// Layer 1 (proactive): discover the task-manager cap so the first request is
	// valid. Layer 2 (default): fall back to Prefect's built-in default otherwise.
	// The value never exceeds the default, since a larger page is unnecessary for an
	// existence check and must stay within the server cap.
	paginationSize := defaultPrefectPaginationSize
	if discovered, ok := iops.discoverPrefectPaginationLimit(); ok && discovered < paginationSize {
		paginationSize = discovered
		logrus.Debugf("Using task-manager pagination cap of %d for the running-tasks check", discovered)
	}

	loadScriptContent := func() error {
		if scriptContent != "" {
			return nil
		}
		scriptBytes, err := readEmbeddedScript("get_running_tasks.py")
		if err != nil {
			return fmt.Errorf("could not retrieve get_running_tasks.py: %w", err)
		}
		scriptContent = string(scriptBytes)
		return nil
	}

	isCommandNotFound := func(err error, output string) bool {
		if err == nil {
			return false
		}
		errMsg := strings.ToLower(err.Error())
		if strings.Contains(errMsg, "no such command") {
			return true
		}
		outputLower := strings.ToLower(output)
		return strings.Contains(outputLower, "no such command")
	}

	// adaptPaginationLimit (Layer 3, reactive): when the task-manager rejects the
	// request, lower the pagination size to the cap reported in the error and signal
	// a retry. Only lowers the size, so retries converge and cannot loop.
	adaptPaginationLimit := func(parts ...string) bool {
		maxLimit, ok := parsePrefectMaxLimit(parts...)
		if !ok || maxLimit >= paginationSize {
			return false
		}
		logrus.Warnf("task-manager rejected pagination size %d; retrying with %d", paginationSize, maxLimit)
		paginationSize = maxLimit
		return true
	}

	for {
		var (
			output string
			err    error
		)

		// Layer 0 (clamp): inject the bounded pagination size into the exec so the
		// deployment's INFRAHUB_PAGINATION_SIZE cannot exceed the server cap.
		execOpts := iops.buildTaskWorkerExecOpts(&ExecOptions{
			Env: map[string]string{"INFRAHUB_PAGINATION_SIZE": strconv.Itoa(paginationSize)},
		})

		if useInfrahubctl {
			output, err = iops.Exec("task-worker", []string{"infrahubctl", "task", "list", "--json", "--state", "running", "--state", "pending"}, execOpts)
			if err != nil {
				if isCommandNotFound(err, output) {
					logrus.Infof("infrahubctl task list command not available in task-worker, falling back to embedded script")
					useInfrahubctl = false
					if loadErr := loadScriptContent(); loadErr != nil {
						return loadErr
					}
					continue
				}
				if adaptPaginationLimit(err.Error(), output) {
					continue
				}
				return fmt.Errorf("failed to check running tasks: %w\n%s", err, output)
			}
		} else {
			if err := loadScriptContent(); err != nil {
				return err
			}
			output, err = iops.executeScriptWithOpts("task-worker", scriptContent, "/tmp/get_running_tasks.py", execOpts, "python", "-u", "/tmp/get_running_tasks.py")
			if err != nil {
				if adaptPaginationLimit(err.Error(), output) {
					continue
				}
				return fmt.Errorf("failed to check running tasks: %w", err)
			}
		}

		output = strings.TrimSpace(output)
		var tasks []tasksOutput
		if output != "" {
			if err := json.Unmarshal([]byte(output), &tasks); err != nil {
				return fmt.Errorf("could not parse json: %w\n%v", err, output)
			}
		}
		if len(tasks) == 0 {
			logrus.Info("No running tasks detected. Proceeding with backup.")
			return nil
		}

		logrus.Warnf("There are running %v tasks: %v", len(tasks), tasks)
		logrus.Warnf("Waiting for them to complete... (use --force to override)")
		time.Sleep(5 * time.Second)
	}
}

// appServices are the application services that backup and restore stop, and
// later start again, around their database steps.
var appServices = []string{
	"infrahub-server", "task-worker", "task-manager",
	"task-manager-background-svc", "cache", "message-queue",
}

// restoreServices are the services a restore starts: the application services
// and task-manager-db, which restorePostgreSQL starts before it loads the dump.
var restoreServices = slices.Concat(appServices, []string{"task-manager-db"})

// replicaRecorder is implemented by backends that stop a service by scaling
// its workload to 0 replicas, and so must know the workload's replica count to
// start it again.
type replicaRecorder interface {
	RecordReplicaCounts(services ...string) error
}

// recordReplicaCounts reads the replica count of each service's workload, so
// that starting a service brings its workload back with that count. Backup and
// restore call it before they change anything in the deployment, so that they
// stop with an error, and leave the deployment as it was, when a count cannot
// be read. Backends that stop containers directly have nothing to record.
func (iops *InfrahubOps) recordReplicaCounts(services ...string) error {
	backend, err := iops.ensureBackend()
	if err != nil {
		return err
	}
	recorder, ok := backend.(replicaRecorder)
	if !ok {
		return nil
	}
	if err := recorder.RecordReplicaCounts(services...); err != nil {
		return fmt.Errorf("failed to read the workload replica counts; nothing in the deployment was changed: %w", err)
	}
	return nil
}

func (iops *InfrahubOps) stopAppContainers() ([]string, error) {
	logrus.Info("Stopping Infrahub application services...")

	stopped := []string{}

	for _, service := range appServices {
		running, err := iops.IsServiceRunning(service)
		if err != nil {
			logrus.Debugf("Could not determine status of %s: %v", service, err)
			continue
		}

		if running {
			logrus.Infof("Stopping %s...", service)
			if err := iops.StopServices(service); err != nil {
				return stopped, fmt.Errorf("failed to stop %s: %w", service, err)
			}
			stopped = append(stopped, service)
		}
	}

	if len(stopped) == 0 {
		logrus.Info("No application services were running")
	} else {
		logrus.Info("Application services stopped")
	}

	return stopped, nil
}

func (iops *InfrahubOps) startAppContainers(services []string) error {
	if len(services) == 0 {
		return nil
	}

	logrus.Info("Starting Infrahub application services...")

	preferredOrder := []string{
		"cache",
		"message-queue",
		"task-manager",
		"task-manager-background-svc",
		"infrahub-server",
		"task-worker",
	}

	serviceSet := make(map[string]struct{}, len(services))
	for _, svc := range services {
		serviceSet[svc] = struct{}{}
	}

	ordered := make([]string, 0, len(serviceSet))
	for _, svc := range preferredOrder {
		if _, ok := serviceSet[svc]; ok {
			ordered = append(ordered, svc)
			delete(serviceSet, svc)
		}
	}
	for svc := range serviceSet {
		ordered = append(ordered, svc)
	}

	for _, svc := range ordered {
		logrus.Infof("Starting %s...", svc)
		if err := iops.StartServices(svc); err != nil {
			return fmt.Errorf("failed to start %s: %w", svc, err)
		}
	}

	logrus.Info("Application services started")
	return nil
}

func (iops *InfrahubOps) wipeTransientData() error {
	logrus.Info("Wiping cache and message queue data...")

	if err := iops.deleteMessageQueues(); err != nil {
		logrus.Warnf("Failed to wipe message queue data: %v", err)
	}
	if err := iops.deleteCacheKeys(); err != nil {
		logrus.Warnf("Failed to wipe cache data: %v", err)
	}
	logrus.Info("Transient data wiped")
	return nil
}

// redisCLIScript runs redis-cli with the arguments that follow it. When the
// container sets REDIS_PASSWORD_FILE or REDIS_PASSWORD, as the Bitnami Redis
// image does when authentication is enabled, redis-cli reads that password
// from REDISCLI_AUTH. The arguments reach redis-cli as positional parameters,
// never as part of the script.
const redisCLIScript = `if [ -n "${REDIS_PASSWORD_FILE:-}" ]; then REDISCLI_AUTH=$(cat "$REDIS_PASSWORD_FILE") && export REDISCLI_AUTH; elif [ -n "${REDIS_PASSWORD:-}" ]; then REDISCLI_AUTH=$REDIS_PASSWORD && export REDISCLI_AUTH; fi; exec redis-cli "$@"`

// redisDeleteKeysScript deletes every key of the selected Redis database, one
// SCAN page at a time, and returns how many keys it deleted.
const redisDeleteKeysScript = `local cursor = "0"
local deleted = 0
repeat
  local reply = redis.call("SCAN", cursor, "COUNT", 1000)
  cursor = reply[1]
  for _, key in ipairs(reply[2]) do
    redis.call("UNLINK", key)
    deleted = deleted + 1
  end
until cursor == "0"
return deleted`

// redisKeyspaceDBRe matches a database line of `INFO keyspace`, such as
// "db0:keys=12,expires=0,avg_ttl=0".
var redisKeyspaceDBRe = regexp.MustCompile(`^db(\d+):keys=`)

// deleteCacheKeys deletes every key in every database of the cache. Infrahub
// uses database 0 by default, and a task manager configured with Redis
// messaging can use another one. Redis keeps its data in memory and writes it to disk when it
// stops, so deleting the files under /data while Redis runs left the data in
// place.
//
// The keys are deleted by redisDeleteKeysScript rather than FLUSHALL, because
// the Bitnami Redis chart that the Infrahub Helm chart uses disables FLUSHALL
// and FLUSHDB by default. redis-cli exits with 0 when Redis answers with an
// error, such as NOAUTH, so every reply is checked.
func (iops *InfrahubOps) deleteCacheKeys() error {
	master, err := iops.redisMasterArgs()
	if err != nil {
		return err
	}

	keyspace, err := iops.redisCLI(slices.Concat(master, []string{"INFO", "keyspace"})...)
	if err != nil {
		return fmt.Errorf("failed to list the Redis databases: %w", err)
	}
	if !strings.HasPrefix(keyspace, "# Keyspace") {
		return fmt.Errorf("the cache answered INFO keyspace with %q", keyspace)
	}

	var errs []error
	deleted := 0
	for _, line := range strings.Split(keyspace, "\n") {
		match := redisKeyspaceDBRe.FindStringSubmatch(strings.TrimSpace(line))
		if match == nil {
			continue
		}
		db := match[1]
		reply, err := iops.redisCLI(slices.Concat(master, []string{"-n", db, "EVAL", redisDeleteKeysScript, "0"})...)
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to delete the keys of database %s: %w", db, err))
			continue
		}
		count, err := strconv.Atoi(reply)
		if err != nil {
			errs = append(errs, fmt.Errorf("the cache answered the key deletion in database %s with %q", db, reply))
			continue
		}
		logrus.Debugf("Deleted %d keys of cache database %s", count, db)
		deleted += count
	}

	logrus.Infof("Deleted cache keys: %d", deleted)
	return errors.Join(errs...)
}

// redisMasterArgs returns the redis-cli arguments that address the Redis
// master from the cache service. The commands run in one pod or container of
// the service. When ROLE reports a read-only replica, as with Bitnami Redis
// and Sentinel, the arguments name the master that ROLE reports, and the
// replicas apply the master's changes from there.
func (iops *InfrahubOps) redisMasterArgs() ([]string, error) {
	role, err := iops.redisCLI("ROLE")
	if err != nil {
		return nil, fmt.Errorf("failed to read the Redis role: %w", err)
	}

	lines := strings.Split(role, "\n")
	switch strings.TrimSpace(lines[0]) {
	case "master":
		return nil, nil
	case "slave", "replica":
		if len(lines) < 3 {
			return nil, fmt.Errorf("unexpected ROLE reply from a Redis replica: %q", role)
		}
		host, port := strings.TrimSpace(lines[1]), strings.TrimSpace(lines[2])
		logrus.Debugf("The cache is a Redis replica; sending the commands to its master %s:%s", host, port)
		return []string{"-h", host, "-p", port}, nil
	}
	return nil, fmt.Errorf("the cache answered ROLE with %q", role)
}

// redisCLI runs redis-cli in the cache service and returns its stdout.
func (iops *InfrahubOps) redisCLI(args ...string) (string, error) {
	return iops.execOutput("cache", append([]string{"sh", "-c", redisCLIScript, "redis-cli"}, args...))
}

// transientDataExecTimeout bounds each command that wipeTransientData runs in
// a service. Each returns in under a second on a running service.
const transientDataExecTimeout = 60 * time.Second

// rabbitmqVhost is one entry of `rabbitmqctl list_vhosts --formatter json name`.
type rabbitmqVhost struct {
	Name string `json:"name"`
}

// rabbitmqQueue is one entry of
// `rabbitmqctl list_queues --formatter json name exclusive`.
type rabbitmqQueue struct {
	Name      string `json:"name"`
	Exclusive bool   `json:"exclusive"`
}

// deleteMessageQueues deletes the queues of every virtual host on the message
// queue, and with them the messages queued before the restore. Infrahub
// declares its queues again when it starts. Exclusive queues are skipped: each
// belongs to a connected client, and RabbitMQ deletes it when that client
// stops.
//
// The queues are deleted through rabbitmqctl while the broker runs. Deleting
// the broker's data directory under it could leave RabbitMQ, once restarted,
// with a durable queue it cannot start ("home node ... is down or
// inaccessible"), which stops Infrahub from starting. Resetting the node
// instead would remove it from a RabbitMQ cluster. Users, permissions and
// policies are kept.
func (iops *InfrahubOps) deleteMessageQueues() error {
	output, err := iops.rabbitmqctl("list_vhosts", "--quiet", "--formatter", "json", "name")
	if err != nil {
		return fmt.Errorf("failed to list the virtual hosts: %w", err)
	}
	var vhosts []rabbitmqVhost
	if err := json.Unmarshal([]byte(output), &vhosts); err != nil {
		return fmt.Errorf("failed to parse the virtual host list %q: %w", output, err)
	}

	var errs []error
	deleted := 0
	for _, vhost := range vhosts {
		output, err := iops.rabbitmqctl("list_queues", "--quiet", "--formatter", "json", "-p", vhost.Name, "name", "exclusive")
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to list the queues of virtual host %q: %w", vhost.Name, err))
			continue
		}
		var queues []rabbitmqQueue
		if err := json.Unmarshal([]byte(output), &queues); err != nil {
			errs = append(errs, fmt.Errorf("failed to parse the queue list of virtual host %q: %w", vhost.Name, err))
			continue
		}

		for _, queue := range queues {
			if queue.Exclusive {
				continue
			}
			if _, err := iops.rabbitmqctl("delete_queue", "--quiet", "-p", vhost.Name, "--", queue.Name); err != nil {
				errs = append(errs, fmt.Errorf("failed to delete queue %q of virtual host %q: %w", queue.Name, vhost.Name, err))
				continue
			}
			logrus.Debugf("Deleted queue %s of virtual host %s", queue.Name, vhost.Name)
			deleted++
		}
	}

	logrus.Infof("Deleted message queues: %d", deleted)
	return errors.Join(errs...)
}

// rabbitmqctl runs rabbitmqctl in the message-queue service and returns its
// stdout.
func (iops *InfrahubOps) rabbitmqctl(args ...string) (string, error) {
	return iops.execOutput("message-queue", append([]string{"rabbitmqctl"}, args...))
}

// execOutput runs command in service and returns its stdout. Stdout and stderr
// are kept apart, so that a notice written to stderr, such as a docker compose
// warning about an unset variable or kubectl's "Defaulted container" notice,
// does not end up in the output the caller parses. When the command fails, the
// error carries its stderr on one line.
func (iops *InfrahubOps) execOutput(service string, command []string) (string, error) {
	backend, err := iops.ensureBackend()
	if err != nil {
		return "", err
	}
	execer, ok := backend.(separateExecer)
	if !ok {
		return "", fmt.Errorf("the %s backend cannot run %s in %s", backend.Name(), command[0], service)
	}

	stdout, stderr, err := execer.ExecSeparateContext(context.Background(), transientDataExecTimeout, service, command, nil)
	if err != nil {
		if stderr != "" {
			return "", fmt.Errorf("%w: %s", err, strings.Join(strings.Fields(stderr), " "))
		}
		return "", err
	}
	return stdout, nil
}
