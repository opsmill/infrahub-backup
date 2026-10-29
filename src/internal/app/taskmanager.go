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

// FlowRunsRequest is what the user asked `flush flow-runs` for.
type FlowRunsRequest struct {
	DaysToKeep int
	BatchSize  int
	// ArgumentsGiven is true when the user passed days_to_keep or batch_size.
	ArgumentsGiven bool
	Rewrite        bool
}

// FlushFlowRuns removes completed Prefect runs beyond the retention window.
//
// From Infrahub 1.13, `infrahub tasks flush flow-runs` takes no number of days: the task manager deletes up to its
// own task history retention, and `--rewrite` returns the disk space. Earlier releases take the number of days.
func (iops *InfrahubOps) FlushFlowRuns(request FlowRunsRequest) error {
	if err := iops.checkPrerequisites(); err != nil {
		return err
	}
	if err := iops.DetectEnvironment(); err != nil {
		return err
	}

	// Wide enough that the Infrahub CLI prints its help and summary lines without wrapping them.
	execOpts := iops.buildTaskWorkerExecOpts(&ExecOptions{Env: map[string]string{"COLUMNS": "200"}})
	help, helpErr := iops.Exec("task-worker", []string{"infrahub", "tasks", "flush", "flow-runs", "--help"}, execOpts)
	if helpErr != nil || !taskManagerOwnsRetention(help) {
		if request.Rewrite {
			logrus.Warn("This Infrahub release cannot rewrite the task history tables; --rewrite is ignored")
		}
		return iops.flushTaskRuns(flowRunsConfig, request.DaysToKeep, request.BatchSize)
	}

	if warning := ignoredArgumentsWarning(request); warning != "" {
		logrus.Warn(warning)
	}
	command := flowRunsCommand(request)
	logrus.Infof("Running %s in the task worker...", strings.Join(command, " "))
	if _, err := iops.ExecStream("task-worker", command, execOpts); err != nil {
		return fmt.Errorf("failed to execute %s: %w", strings.Join(command, " "), err)
	}
	logrus.Info("Flow runs cleanup completed")
	return nil
}

// taskManagerOwnsRetention reports whether the help of `infrahub tasks flush flow-runs` is the one of Infrahub 1.13
// and later, which takes no number of days and offers --rewrite.
func taskManagerOwnsRetention(help string) bool {
	help = ansiEscapeRe.ReplaceAllString(help, "")
	return strings.Contains(help, "--rewrite") && !strings.Contains(help, "--days-to-keep")
}

// flowRunsCommand is the `infrahub tasks flush flow-runs` command of Infrahub 1.13 and later for the request.
func flowRunsCommand(request FlowRunsRequest) []string {
	command := []string{"infrahub", "tasks", "flush", "flow-runs"}
	if request.Rewrite {
		command = append(command, "--rewrite")
	}
	return command
}

// ignoredArgumentsWarning explains that Infrahub 1.13 and later ignore days_to_keep and batch_size, when they were given.
func ignoredArgumentsWarning(request FlowRunsRequest) string {
	if !request.ArgumentsGiven {
		return ""
	}
	return fmt.Sprintf(
		"This Infrahub release deletes the flow runs older than the task history retention set on the task manager, "+
			"not the %d days given; to keep %d days, set INFRAHUB_TASK_MANAGER_RETENTION_TASK_HISTORY=%dd on the task manager",
		request.DaysToKeep, request.DaysToKeep, request.DaysToKeep,
	)
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

	// Clamp the batch size to the task-manager cap so a large --batch-size cannot be
	// rejected by Prefect with a 422. The cleanup scripts pass batchSize straight to
	// read_flow_runs, so clamping up front is simpler than a reactive retry.
	if maxLimit, ok := iops.discoverPrefectPaginationLimit(); ok && batchSize > maxLimit {
		logrus.Warnf("Requested batch size %d exceeds the task-manager cap of %d; clamping to %d", batchSize, maxLimit, maxLimit)
		batchSize = maxLimit
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
