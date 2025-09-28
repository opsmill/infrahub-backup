package main

import (
	"fmt"
	"strconv"

	"github.com/sirupsen/logrus"
)

// Task Manager (Prefect) maintenance
func (iops *InfrahubOps) flushFlowRuns(daysToKeep, batchSize int) error {
	if err := iops.checkPrerequisites(); err != nil {
		return err
	}
	if err := iops.detectEnvironment(); err != nil {
		return err
	}

	if daysToKeep < 0 {
		daysToKeep = 30
	}
	if batchSize <= 0 {
		batchSize = 200
	}

	logrus.Infof("Flushing Prefect flow runs older than %d days (batch size %d)...", daysToKeep, batchSize)

	var scriptContent []byte
	var err error
	if scriptContent, err = scripts.ReadFile("scripts/clean_old_tasks.py"); err != nil {
		return fmt.Errorf("could not retrieve script: %w", err)
	}

	if _, err := iops.executeScript("task-worker", string(scriptContent), "/tmp/infrahubops_clean_old_tasks.py", "python", "-u", "/tmp/infrahubops_clean_old_tasks.py", strconv.Itoa(daysToKeep), strconv.Itoa(batchSize)); err != nil {
		return err
	}

	logrus.Info("Flow runs cleanup completed:")

	return nil
}

func (iops *InfrahubOps) flushStaleRuns(daysToKeep, batchSize int) error {
	if err := iops.checkPrerequisites(); err != nil {
		return err
	}
	if err := iops.detectEnvironment(); err != nil {
		return err
	}

	if daysToKeep < 0 {
		daysToKeep = 2
	}
	if batchSize <= 0 {
		batchSize = 200
	}

	logrus.Infof("Flushing Prefect flow runs older than %d days (batch size %d)...", daysToKeep, batchSize)

	var scriptContent []byte
	var err error
	if scriptContent, err = scripts.ReadFile("scripts/clean_stale_tasks.py"); err != nil {
		return fmt.Errorf("could not retrieve script: %w", err)
	}

	if _, err := iops.executeScript("task-worker", string(scriptContent), "/tmp/infrahubops_clean_stale_tasks.py", "python", "-u", "/tmp/infrahubops_clean_stale_tasks.py", strconv.Itoa(daysToKeep), strconv.Itoa(batchSize)); err != nil {
		return err
	}

	logrus.Info("Stale flow runs cleanup completed:")

	return nil
}
