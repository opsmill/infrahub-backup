package app

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

const (
	neo4jPIDFile              = "/var/lib/neo4j/run/neo4j.pid"
	neo4jRemoteWorkDir        = "/tmp/infrahubops"
	neo4jRemoteWatchdogBinary = neo4jRemoteWorkDir + "/neo4j_watchdog"
	neo4jRemoteWatchdogReady  = neo4jRemoteWorkDir + "/neo4j_watchdog.ready"
	neo4jRemoteWatchdogLog    = neo4jRemoteWorkDir + "/neo4j_watchdog.log"
)

func selectWatchdogBinary(arch string) ([]byte, error) {
	switch strings.ToLower(arch) {
	case "x86_64", "amd64":
		return neo4jWatchdogLinuxAMD64, nil
	case "aarch64", "arm64":
		return neo4jWatchdogLinuxARM64, nil
	default:
		return nil, fmt.Errorf("unsupported architecture for watchdog: %s", arch)
	}
}

func writeEmbeddedWatchdog(content []byte) (string, func(), error) {
	file, err := os.CreateTemp("", "neo4j_watchdog_*")
	if err != nil {
		return "", nil, fmt.Errorf("failed to create temp watchdog binary: %w", err)
	}

	if _, err := file.Write(content); err != nil {
		file.Close()
		os.Remove(file.Name())
		return "", nil, fmt.Errorf("failed to write watchdog binary: %w", err)
	}
	if err := file.Close(); err != nil {
		os.Remove(file.Name())
		return "", nil, fmt.Errorf("failed to close watchdog binary: %w", err)
	}

	if err := os.Chmod(file.Name(), 0755); err != nil {
		os.Remove(file.Name())
		return "", nil, fmt.Errorf("failed to set watchdog permissions: %w", err)
	}

	cleanup := func() {
		_ = os.Remove(file.Name())
	}
	return file.Name(), cleanup, nil
}

func (iops *InfrahubOps) waitForRemoteFile(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := iops.Exec("database", []string{"test", "-f", path}, nil); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for remote file %s", path)
}

func (iops *InfrahubOps) waitForProcessStopped(pid string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		stateCmd := fmt.Sprintf("sed -n 's/^State:\t//p' /proc/%s/status", pid)
		state, err := iops.Exec("database", []string{"sh", "-c", stateCmd}, nil)
		if err == nil {
			trimmed := strings.TrimSpace(state)
			if strings.HasPrefix(trimmed, "T") {
				return nil
			}
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(1 * time.Second)
	}
	return fmt.Errorf("timed out waiting for neo4j process %s to stop", pid)
}

// getWritableTempDir checks if /tmp is writable in the given container/pod.
// If /tmp is not writable, it falls back to /run.
func (iops *InfrahubOps) getWritableTempDir(service string) string {
	// Try to create a test file in /tmp
	testFile := "/tmp/.infrahubops_write_test"
	if _, err := iops.Exec(service, []string{"touch", testFile}, nil); err == nil {
		// Clean up test file
		_, _ = iops.Exec(service, []string{"rm", "-f", testFile}, nil)
		logrus.Debugf("Using /tmp as temp directory for %s", service)
		return "/tmp"
	}

	// /tmp is not writable, try /run
	testFile = "/run/.infrahubops_write_test"
	if _, err := iops.Exec(service, []string{"touch", testFile}, nil); err == nil {
		// Clean up test file
		_, _ = iops.Exec(service, []string{"rm", "-f", testFile}, nil)
		logrus.Infof("/tmp is not writable in %s, using /run as temp directory", service)
		return "/run"
	}

	// Fall back to /tmp even if both failed (let the actual operation fail with a meaningful error)
	logrus.Warnf("Neither /tmp nor /run appear writable in %s, defaulting to /tmp", service)
	return "/tmp"
}
