package app

import (
	"encoding/json"
	"fmt"
	"regexp"
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

func (iops *InfrahubOps) stopAppContainers() ([]string, error) {
	logrus.Info("Stopping Infrahub application services...")

	services := []string{
		"infrahub-server", "task-worker", "task-manager",
		"task-manager-background-svc", "cache", "message-queue",
	}

	stopped := []string{}

	for _, service := range services {
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

	if _, err := iops.Exec("message-queue", []string{"find", "/var/lib/rabbitmq", "-mindepth", "1", "-delete"}, nil); err != nil {
		logrus.Warnf("Failed to wipe message queue data: %v", err)
	}
	if _, err := iops.Exec("cache", []string{"find", "/data", "-mindepth", "1", "-delete"}, nil); err != nil {
		logrus.Warnf("Failed to wipe cache data: %v", err)
	}
	logrus.Info("Transient data wiped")
	return nil
}
