package app

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

type tasksOutput struct {
	Id   string `json:"id"`
	Name string `json:"title"`
}

func (iops *InfrahubOps) waitForRunningTasks() error {
	useInfrahubctl := true
	var scriptContent string

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

	for {
		var (
			output string
			err    error
		)

		execOpts := iops.buildTaskWorkerExecOpts(nil)

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
				return fmt.Errorf("failed to check running tasks: %w\n%s", err, output)
			}
		} else {
			if err := loadScriptContent(); err != nil {
				return err
			}
			output, err = iops.executeScriptWithOpts("task-worker", scriptContent, "/tmp/get_running_tasks.py", execOpts, "python", "-u", "/tmp/get_running_tasks.py")
			if err != nil {
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
