package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// telemetryBundleDir is the bundle directory for the Infrahub product
// telemetry export (contracts/bundle-layout.md).
const telemetryBundleDir = "telemetry"

// telemetryExportFilename is the bundle file the telemetry export JSON lands
// in; it mirrors infrahubctl's own default --output name so the artifact is
// recognizable to anyone who has run the CLI directly.
const telemetryExportFilename = "telemetry-export.json"

// telemetryErrorFilename preserves infrahubctl's combined output whenever the
// export produces no file, so support can read what the CLI reported (mirrors
// the task-manager events.err.txt convention, FIX-8). The manifest still
// records the skip reason.
const telemetryErrorFilename = "telemetry-export.err.txt"

// telemetryExportContainerPath is where infrahubctl writes the export inside
// the task-worker container before it is copied into the bundle and removed.
const telemetryExportContainerPath = "/tmp/infrahubops_telemetry_export.json"

// telemetryService is the container that ships a configured infrahubctl — the
// same one infrahub-backup uses for `infrahubctl task list`, so its
// INFRAHUB_ADDRESS / API token already point at the deployment's API.
const telemetryService = "task-worker"

// defaultTelemetryDays is the look-back window applied when --telemetry-days is
// unset or non-positive: the last 30 days of telemetry snapshots.
const defaultTelemetryDays = 30

// telemetryStartDateLayout formats the --start-date argument as an ISO 8601
// calendar date. A date-only value parses whether infrahubctl treats the flag
// as a date or a datetime, unlike a full RFC3339 timestamp.
const telemetryStartDateLayout = "2006-01-02"

// telemetryCollector exports Infrahub product telemetry snapshots for the
// configured look-back window (default 30 days) via `infrahubctl telemetry
// export` inside the task-worker container and stages the JSON into
// bundle/telemetry/. It is read-only: the export pulls stored snapshots
// through the Infrahub API and mutates nothing in the deployment.
//
// The export producing nothing is not a bundle defect — a deployment may have
// no telemetry snapshots in the window, or run an Infrahub version without the
// telemetry API — so a failing export degrades to skipped (like --benchmark),
// with the CLI's own output preserved for support. Only failing to enumerate a
// task-worker replica to run in, or a local staging error (creating the
// directory, copying the file out), marks the collector failed.
func telemetryCollector() collector {
	return collector{
		name: "telemetry",
		skip: skipWhenServiceAbsent(telemetryService),
		run:  collectTelemetry,
	}
}

// telemetryStartDate returns the ISO 8601 start-date argument for a look-back
// window of days ending at now. A non-positive days falls back to the 30-day
// default so a missing/invalid flag never widens the export unbounded.
func telemetryStartDate(now time.Time, days int) string {
	if days <= 0 {
		days = defaultTelemetryDays
	}
	return now.UTC().AddDate(0, 0, -days).Format(telemetryStartDateLayout)
}

// telemetryExportCommand builds the infrahubctl invocation that exports
// snapshots from startDate onward to the in-container path. Only --start-date
// is bounded: there are no future snapshots, so "from startDate" is exactly
// "the last N days".
func telemetryExportCommand(startDate string) []string {
	return []string{
		"infrahubctl", "telemetry", "export",
		"--output", telemetryExportContainerPath,
		"--start-date", startDate,
	}
}

// pickTelemetryReplica chooses the one replica the export, the copy-out and
// the cleanup all run in. The three steps must share a replica: on a scaled
// Docker Compose service, `docker compose exec` runs in the lowest-numbered
// running container while `docker compose cp` reads from the first container
// the daemon lists, so service-level calls can write the export in one
// container and look for it in another. A running replica is preferred and,
// among those, a container named after the service, so a sidecar injected into
// a Kubernetes pod (which ships no infrahubctl) is not picked over the
// task-worker container. Ties keep the enumeration order, so the pick is
// stable. replicas must not be empty.
func pickTelemetryReplica(replicas []Replica) Replica {
	best, bestRank := replicas[0], -1
	for _, replica := range replicas {
		rank := 0
		if !replica.Stopped {
			rank += 2
		}
		if strings.Contains(replica.Container, replica.Service) {
			rank++
		}
		if rank > bestRank {
			best, bestRank = replica, rank
		}
	}
	return best
}

// telemetryReplica enumerates the telemetry service's replicas and picks the
// one the whole export runs in (see pickTelemetryReplica).
func (cc *collectContext) telemetryReplica() (Replica, error) {
	cb, err := cc.collect()
	if err != nil {
		return Replica{}, err
	}
	replicas, err := cb.ServiceReplicas(telemetryService)
	if err != nil {
		return Replica{}, fmt.Errorf("failed to enumerate %s replicas: %w", telemetryService, err)
	}
	if len(replicas) == 0 {
		return Replica{}, fmt.Errorf("no replicas found for service %s", telemetryService)
	}
	return pickTelemetryReplica(replicas), nil
}

func collectTelemetry(cc *collectContext) error {
	dir := filepath.Join(cc.bundleDir, telemetryBundleDir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create %s directory: %w", telemetryBundleDir, err)
	}

	replica, err := cc.telemetryReplica()
	if err != nil {
		return err
	}
	replicaName := replicaBaseName(replica, false)

	startDate := telemetryStartDate(time.Now(), cc.opts.TelemetryDays)
	command := telemetryExportCommand(startDate)

	// The export pages through the API, so it gets the transfer bound rather
	// than the shorter status-dump bound.
	output, err := cc.execReplicaDumpTimeout(collectTransferTimeout, replica, command)

	// Best-effort cleanup of the in-container export file, regardless of
	// outcome, in the replica the export ran in. Routed through the bounded
	// collect exec path so a wedged container cannot hang the run during
	// cleanup.
	defer func() {
		if _, rmErr := cc.execReplicaDump(replica, []string{"rm", "-f", telemetryExportContainerPath}); rmErr != nil {
			logrus.Warnf("Failed to clean up telemetry export %s on %s: %v", telemetryExportContainerPath, replicaName, rmErr)
		}
	}()

	if err != nil {
		// infrahubctl runs under combined output, so a failure merges its
		// message/traceback into output. Preserve it beside the (absent)
		// export so support sees what the CLI reported, then degrade to
		// skipped: a missing export is not a bundle defect.
		if strings.TrimSpace(output) != "" {
			if writeErr := writeDumpFile(filepath.Join(dir, telemetryErrorFilename), output, nil); writeErr != nil {
				logrus.Warnf("Failed to write telemetry export output: %v", writeErr)
			}
		}
		return &collectSkipError{reason: telemetrySkipReason(output, err)}
	}

	if err := cc.copyFromReplica(replica, telemetryExportContainerPath, filepath.Join(dir, telemetryExportFilename)); err != nil {
		return fmt.Errorf("failed to copy telemetry export from %s: %w", replicaName, err)
	}

	return nil
}

// telemetrySkipReason renders the manifest reason for a telemetry export that
// produced no file. The common empty case ("No telemetry snapshots found.")
// gets a friendly reason; anything else keeps the CLI's own error line, or the
// raw error when the CLI printed nothing (e.g. a timeout).
func telemetrySkipReason(output string, err error) string {
	if strings.Contains(strings.ToLower(output), "no telemetry snapshots") {
		return "no telemetry snapshots found in the requested window"
	}
	line := commandErrorLine(output)
	if line == "" {
		line = err.Error()
	}
	return "infrahubctl telemetry export could not be run: " + line
}
