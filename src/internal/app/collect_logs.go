package app

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/sirupsen/logrus"
)

// collectServices is the canonical Infrahub service list (the constitution's
// public deployment contract) in the order log collection walks it.
var collectServices = []string{
	"infrahub-server",
	"task-worker",
	"database",
	"message-queue",
	"cache",
	"task-manager",
	"task-manager-db",
	"task-manager-background-svc",
}

// serviceLogCollectors returns one collector per canonical service, each
// writing per-replica log files under bundle/logs/<service>/ and recording a
// manifest entry named logs/<service>. The run plan (T025) consumes this
// slice unchanged.
func serviceLogCollectors() []collector {
	collectors := make([]collector, 0, len(collectServices))
	for _, service := range collectServices {
		collectors = append(collectors, serviceLogCollector(service))
	}
	return collectors
}

// serviceLogCollector builds the log collector for one service. A service
// with no replicas (not deployed in this environment) is skipped; enumeration
// errors are left to run() so they surface as failed with a reason.
func serviceLogCollector(service string) collector {
	return collector{
		name: "logs/" + service,
		skip: func(cc *collectContext) (bool, string) {
			cb, err := cc.collect()
			if err != nil {
				return false, "" // run() reports the unsupported backend as failed
			}
			replicas, err := cb.ServiceReplicas(service)
			if err != nil {
				return false, "" // run() reports the enumeration error as failed
			}
			if len(replicas) == 0 {
				return true, "service not deployed"
			}
			return false, ""
		},
		run: func(cc *collectContext) error {
			return collectServiceLogs(cc, service)
		},
	}
}

// collectServiceLogs writes one log file per replica of a service into
// bundle/logs/<service>/, plus a .previous.log per restarted Kubernetes
// container. Per-replica failures never abort the remaining replicas; they
// are aggregated into the returned error so the manifest records the partial
// outcome while the successfully written logs stay in the bundle (FR-009).
func collectServiceLogs(cc *collectContext, service string) error {
	cb, err := cc.collect()
	if err != nil {
		return err
	}

	replicas, err := cb.ServiceReplicas(service)
	if err != nil {
		return fmt.Errorf("failed to enumerate %s replicas: %w", service, err)
	}
	if len(replicas) == 0 {
		return fmt.Errorf("no replicas found for service %s", service)
	}

	serviceDir := filepath.Join(cc.bundleDir, "logs", service)
	if err := os.MkdirAll(serviceDir, 0755); err != nil {
		return fmt.Errorf("failed to create log directory %s: %w", serviceDir, err)
	}

	counts := podContainerCounts(replicas)
	failures := []string{}
	for _, replica := range replicas {
		multiContainer := counts[replica.Pod] > 1

		filename := replicaLogFilename(replica, multiContainer, false)
		if err := writeReplicaLog(cb, replica, cc.opts.LogLines, false, filepath.Join(serviceDir, filename)); err != nil {
			logrus.Warnf("Failed to collect logs for %s: %v", filename, err)
			failures = append(failures, fmt.Sprintf("%s: %v", filename, err))
			continue
		}

		// Previous-container logs exist only for restarted Kubernetes
		// containers; a failure fetching them keeps the current logs and is
		// reported as a partial outcome.
		if !replica.Restarted {
			continue
		}
		previousName := replicaLogFilename(replica, multiContainer, true)
		if err := writeReplicaLog(cb, replica, cc.opts.LogLines, true, filepath.Join(serviceDir, previousName)); err != nil {
			logrus.Warnf("Previous logs unavailable for %s: %v", filename, err)
			failures = append(failures, fmt.Sprintf("previous logs unavailable for %s: %v", filename, err))
		}
	}

	if len(failures) > 0 {
		return fmt.Errorf("partial log collection for %s: %s", service, strings.Join(failures, "; "))
	}
	return nil
}

// podContainerCounts counts replicas per pod, detecting multi-container pods
// for filename derivation. Docker replicas carry no pod and never count.
func podContainerCounts(replicas []Replica) map[string]int {
	counts := map[string]int{}
	for _, replica := range replicas {
		if replica.Pod == "" {
			continue
		}
		counts[replica.Pod]++
	}
	return counts
}

// replicaBaseName derives the bundle-path base name for one replica per
// data-model.md: Docker → container name; Kubernetes single-container pod →
// pod name; multi-container pod → <pod>_<container>. Log files and the
// per-replica task-worker directories share this derivation.
func replicaBaseName(replica Replica, multiContainer bool) string {
	base := replica.Container
	if replica.Pod != "" {
		base = replica.Pod
		if multiContainer {
			base = replica.Pod + "_" + replica.Container
		}
	}
	return base
}

// replicaLogFilename derives the bundle log filename for one replica:
// <base>.log, or <base>.previous.log for previous-container logs.
func replicaLogFilename(replica Replica, multiContainer bool, previous bool) string {
	base := replicaBaseName(replica, multiContainer)
	if previous {
		return base + ".previous.log"
	}
	return base + ".log"
}

// writeReplicaLog streams one replica's logs to path, draining the reader
// before calling wait so large logs never buffer in memory. A failed fetch
// that produced no output removes the empty file so the bundle never carries
// misleading zero-byte logs.
func writeReplicaLog(cb collectBackend, replica Replica, tailLines int, previous bool, path string) error {
	reader, wait, err := cb.ReplicaLogs(replica, tailLines, previous)
	if err != nil {
		return fmt.Errorf("failed to start log stream: %w", err)
	}

	file, err := os.Create(path)
	if err != nil {
		if closeErr := reader.Close(); closeErr != nil {
			logrus.Debugf("Failed to close log stream for %s: %v", path, closeErr)
		}
		if waitErr := wait(); waitErr != nil {
			logrus.Debugf("Log command for %s exited with error: %v", path, waitErr)
		}
		return fmt.Errorf("failed to create log file %s: %w", path, err)
	}

	written, copyErr := io.Copy(file, reader)
	closeErr := file.Close()
	waitErr := wait()

	if (copyErr != nil || waitErr != nil || closeErr != nil) && written == 0 {
		if removeErr := os.Remove(path); removeErr != nil {
			logrus.Warnf("Failed to remove empty log file %s: %v", path, removeErr)
		}
	}

	if copyErr != nil {
		return fmt.Errorf("failed to stream logs to %s: %w", path, copyErr)
	}
	if waitErr != nil {
		return fmt.Errorf("log command failed: %w", waitErr)
	}
	if closeErr != nil {
		return fmt.Errorf("failed to write log file %s: %w", path, closeErr)
	}
	return nil
}
