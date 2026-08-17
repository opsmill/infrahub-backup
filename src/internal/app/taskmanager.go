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
	// staleRunsConfig has no commandType: stale runs never go through the infrahub
	// CLI, see FlushStaleRuns.
	staleRunsConfig = flushConfig{
		scriptName:        "clean_stale_tasks.py",
		scriptPath:        "/tmp/infrahubops_clean_stale_tasks.py",
		defaultDaysToKeep: defaultStaleRunsRetention,
	}
)

// FlushFlowRuns removes completed Prefect runs beyond the retention window.
func (iops *InfrahubOps) FlushFlowRuns(daysToKeep, batchSize int) error {
	daysToKeep, batchSize, err := iops.prepareFlush(flowRunsConfig, daysToKeep, batchSize)
	if err != nil {
		return err
	}
	return iops.flushTaskRuns(flowRunsConfig, daysToKeep, batchSize)
}

// FlushStaleRuns crashes Prefect flow runs stuck in RUNNING or PENDING beyond the
// retention window.
//
// Unlike flow-runs, this does not call `infrahub tasks flush stale-runs`: that command
// hardcodes RUNNING, so runs left in PENDING — a worker that died between accepting a
// run and starting it — are never cleared. The script drives the same internal helper
// the command wraps with PENDING added to the state list, and falls back to a
// standalone Prefect implementation where those internals are unavailable.
func (iops *InfrahubOps) FlushStaleRuns(daysToKeep, batchSize int) error {
	daysToKeep, batchSize, err := iops.prepareFlush(staleRunsConfig, daysToKeep, batchSize)
	if err != nil {
		return err
	}

	logrus.Infof("Crashing Prefect flow runs stuck in RUNNING or PENDING for more than %d days (batch size %d)...", daysToKeep, batchSize)

	scriptContent, err := readEmbeddedScript(staleRunsConfig.scriptName)
	if err != nil {
		return fmt.Errorf("could not retrieve %s: %w", staleRunsConfig.scriptName, err)
	}

	execOpts := iops.buildTaskWorkerExecOpts(nil)
	scriptArgs := []string{"python", "-u", staleRunsConfig.scriptPath, strconv.Itoa(daysToKeep), strconv.Itoa(batchSize)}
	if _, err := iops.executeScriptWithOpts("task-worker", string(scriptContent), staleRunsConfig.scriptPath, execOpts, scriptArgs...); err != nil {
		return err
	}

	logrus.Info("Stale runs cleanup completed")

	return nil
}

// prepareFlush validates the deployment and resolves the retention window and batch
// size shared by every pass of a flush command.
func (iops *InfrahubOps) prepareFlush(config flushConfig, daysToKeep, batchSize int) (int, int, error) {
	if err := iops.checkPrerequisites(); err != nil {
		return 0, 0, err
	}
	if err := iops.DetectEnvironment(); err != nil {
		return 0, 0, err
	}

	if daysToKeep < 0 {
		daysToKeep = config.defaultDaysToKeep
	}
	if batchSize <= 0 {
		batchSize = defaultBatchSize
	}

	// Clamp the batch size to the task-manager cap so a large --batch-size cannot be
	// rejected by Prefect with a 422. The cleanup scripts pass batchSize straight to
	// read_flow_runs, so clamping up front is simpler than a reactive retry.
	if maxLimit, ok := iops.discoverPrefectPaginationLimit(); ok && batchSize > maxLimit {
		logrus.Warnf("Requested batch size %d exceeds the task-manager cap of %d; clamping to %d", batchSize, maxLimit, maxLimit)
		batchSize = maxLimit
	}

	return daysToKeep, batchSize, nil
}

func (iops *InfrahubOps) flushTaskRuns(config flushConfig, daysToKeep, batchSize int) error {
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
