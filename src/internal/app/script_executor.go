package app

import (
	"fmt"
	"os"

	"github.com/sirupsen/logrus"
)

//lint:ignore U1000
func (iops *InfrahubOps) executeScript(targetService string, scriptContent string, targetPath string, args ...string) (string, error) {
	return iops.executeScriptWithOpts(targetService, scriptContent, targetPath, nil, args...)
}

func (iops *InfrahubOps) executeScriptWithOpts(targetService string, scriptContent string, targetPath string, opts *ExecOptions, args ...string) (string, error) {
	// Write embedded script to a temporary file
	tmpFile, err := os.CreateTemp("", "infrahubops_script_*.py")
	if err != nil {
		return "", fmt.Errorf("failed to create temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name())
	if _, err := tmpFile.WriteString(scriptContent); err != nil {
		tmpFile.Close()
		return "", fmt.Errorf("failed to write script: %w", err)
	}
	tmpFile.Close()

	if err := iops.CopyTo(targetService, tmpFile.Name(), targetPath); err != nil {
		return "", fmt.Errorf("failed to copy script to target: %w", err)
	}
	defer func() {
		if _, err := iops.Exec(targetService, []string{"rm", "-f", targetPath}, nil); err != nil {
			logrus.Warnf("Failed to clean up script %s on %s: %v", targetPath, targetService, err)
		}
	}()

	// Execute script inside container
	logrus.Info("Executing script inside container...")

	output, err := iops.ExecStream(targetService, args, opts)
	if err != nil {
		return output, fmt.Errorf("failed to execute script: %w", err)
	}

	return output, nil
}
