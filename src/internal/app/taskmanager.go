package app

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/sirupsen/logrus"
)

// FlushFlowRuns removes completed Prefect runs beyond the retention window.
func (iops *InfrahubOps) FlushFlowRuns(daysToKeep, batchSize int) error {
	if err := iops.checkPrerequisites(); err != nil {
		return err
	}
	if err := iops.DetectEnvironment(); err != nil {
		return err
	}

	if daysToKeep < 0 {
		daysToKeep = 30
	}
	if batchSize <= 0 {
		batchSize = 200
	}

	logrus.Infof("Flushing Prefect flow runs older than %d days (batch size %d)...", daysToKeep, batchSize)

	primaryCmd := []string{"infrahub", "tasks", "flush", "flow-runs", "--days-to-keep", strconv.Itoa(daysToKeep), "--batch-size", strconv.Itoa(batchSize)}
	scriptArgs := []string{"python", "-u", "/tmp/infrahubops_clean_old_tasks.py", strconv.Itoa(daysToKeep), strconv.Itoa(batchSize)}
	if err := iops.runTaskCommandWithFallback(primaryCmd, "clean_old_tasks.py", "/tmp/infrahubops_clean_old_tasks.py", scriptArgs); err != nil {
		return err
	}

	logrus.Info("Flow runs cleanup completed:")

	return nil
}

// FlushStaleRuns cancels running Prefect flow runs that exceeded retention.
func (iops *InfrahubOps) FlushStaleRuns(daysToKeep, batchSize int) error {
	if err := iops.checkPrerequisites(); err != nil {
		return err
	}
	if err := iops.DetectEnvironment(); err != nil {
		return err
	}

	if daysToKeep < 0 {
		daysToKeep = 2
	}
	if batchSize <= 0 {
		batchSize = 200
	}

	logrus.Infof("Flushing Prefect flow runs older than %d days (batch size %d)...", daysToKeep, batchSize)

	primaryCmd := []string{"infrahub", "tasks", "flush", "stale-runs", "--days-to-keep", strconv.Itoa(daysToKeep), "--batch-size", strconv.Itoa(batchSize)}
	scriptArgs := []string{"python", "-u", "/tmp/infrahubops_clean_stale_tasks.py", strconv.Itoa(daysToKeep), strconv.Itoa(batchSize)}
	if err := iops.runTaskCommandWithFallback(primaryCmd, "clean_stale_tasks.py", "/tmp/infrahubops_clean_stale_tasks.py", scriptArgs); err != nil {
		return err
	}

	logrus.Info("Stale flow runs cleanup completed:")

	return nil
}

func (iops *InfrahubOps) runTaskCommandWithFallback(primaryCmd []string, scriptName, scriptTarget string, scriptExecArgs []string) error {
	commandLabel := strings.Join(primaryCmd, " ")
	output, err := iops.Exec("task-worker", primaryCmd, nil)
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
		if _, execErr := iops.executeScript("task-worker", string(scriptContent), scriptTarget, scriptExecArgs...); execErr != nil {
			return execErr
		}
		return nil
	}

	return fmt.Errorf("failed to execute %s: %w\n%s", commandLabel, err, output)
}
