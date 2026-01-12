package app

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/sirupsen/logrus"
)

const (
	defaultFlowRunsRetention  = 30
	defaultStaleRunsRetention = 2
	defaultBatchSize          = 200
)

type flushConfig struct {
	commandType       string
	scriptName        string
	scriptPath        string
	defaultDaysToKeep int
}

var (
	flowRunsConfig = flushConfig{
		commandType:       "flow-runs",
		scriptName:        "clean_old_tasks.py",
		scriptPath:        "/tmp/infrahubops_clean_old_tasks.py",
		defaultDaysToKeep: defaultFlowRunsRetention,
	}
	staleRunsConfig = flushConfig{
		commandType:       "stale-runs",
		scriptName:        "clean_stale_tasks.py",
		scriptPath:        "/tmp/infrahubops_clean_stale_tasks.py",
		defaultDaysToKeep: defaultStaleRunsRetention,
	}
)

// FlushFlowRuns removes completed Prefect runs beyond the retention window.
func (iops *InfrahubOps) FlushFlowRuns(daysToKeep, batchSize int) error {
	return iops.flushTaskRuns(flowRunsConfig, daysToKeep, batchSize)
}

// FlushStaleRuns cancels running Prefect flow runs that exceeded retention.
func (iops *InfrahubOps) FlushStaleRuns(daysToKeep, batchSize int) error {
	return iops.flushTaskRuns(staleRunsConfig, daysToKeep, batchSize)
}

func (iops *InfrahubOps) flushTaskRuns(config flushConfig, daysToKeep, batchSize int) error {
	if err := iops.checkPrerequisites(); err != nil {
		return err
	}
	if err := iops.DetectEnvironment(); err != nil {
		return err
	}

	if daysToKeep < 0 {
		daysToKeep = config.defaultDaysToKeep
	}
	if batchSize <= 0 {
		batchSize = defaultBatchSize
	}

	logrus.Infof("Flushing Prefect flow runs older than %d days (batch size %d)...", daysToKeep, batchSize)

	primaryCmd := []string{"infrahub", "tasks", "flush", config.commandType, "--days-to-keep", strconv.Itoa(daysToKeep), "--batch-size", strconv.Itoa(batchSize)}
	scriptArgs := []string{"python", "-u", config.scriptPath, strconv.Itoa(daysToKeep), strconv.Itoa(batchSize)}

	if err := iops.runTaskCommandWithFallback(primaryCmd, config.scriptName, config.scriptPath, scriptArgs); err != nil {
		return err
	}

	logrus.Info("Flow runs cleanup completed")

	return nil
}

func (iops *InfrahubOps) runTaskCommandWithFallback(primaryCmd []string, scriptName, scriptTarget string, scriptExecArgs []string) error {
	commandLabel := strings.Join(primaryCmd, " ")
	execOpts := iops.buildTaskWorkerExecOpts(nil)
	output, err := iops.Exec("task-worker", primaryCmd, execOpts)
	if err == nil {
		if trimmed := strings.TrimSpace(output); trimmed != "" {
			logrus.Info(trimmed)
		}
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

	if isCommandNotFound(err, output) {
		logrus.Infof("infrahub CLI command not available in task-worker, falling back to %s", scriptName)
		scriptContent, readErr := readEmbeddedScript(scriptName)
		if readErr != nil {
			return fmt.Errorf("could not retrieve script: %w", readErr)
		}
		if _, execErr := iops.executeScriptWithOpts("task-worker", string(scriptContent), scriptTarget, execOpts, scriptExecArgs...); execErr != nil {
			return execErr
		}
		return nil
	}

	return fmt.Errorf("failed to execute %s: %w\n%s", commandLabel, err, output)
}
