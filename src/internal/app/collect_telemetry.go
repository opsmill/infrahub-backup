package app

import (
	"crypto/rand"
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

// telemetryExportContainerPrefix starts the path infrahubctl writes the export
// to inside the task-worker container before it is copied into the bundle and
// removed. Each run appends its own random token (newTelemetryExportContainerPath)
// so two runs against the same deployment never overwrite or delete each
// other's export.
const telemetryExportContainerPrefix = "/tmp/infrahubops_telemetry_export_"

// telemetryService is the container that ships a configured infrahubctl — the
// same one infrahub-backup uses for `infrahubctl task list`, so its
// INFRAHUB_ADDRESS / API token already point at the deployment's API.
const telemetryService = "task-worker"

// defaultTelemetryDays is the look-back window applied when
// CollectOptions.TelemetryDays is unset. The CLI defaults --telemetry-days to
// the same value and rejects values below 1.
const defaultTelemetryDays = 30

// telemetryStartDateLayout formats the --start-date argument as an ISO 8601
// calendar date. A date-only value parses whether infrahubctl treats the flag
// as a date or a datetime, unlike a full RFC3339 timestamp.
const telemetryStartDateLayout = "2006-01-02"

// telemetryCollector exports Infrahub product telemetry snapshots for the
// configured look-back window (default 30 days) via `infrahubctl telemetry
// export` inside the task-worker container and stages the JSON into
// bundle/telemetry/. It changes no Infrahub data: the export reads stored
// snapshots through the Infrahub API. Its only write is the temporary export
// file inside the task-worker container, which it removes afterwards.
//
// The export producing nothing is not a bundle defect — a deployment may have
// no telemetry snapshots in the window, or run an Infrahub version without the
// telemetry API — so any failing export (including an unreachable API, a
// permission error, or a timeout) degrades to skipped (like --benchmark), with
// the attempt recorded in telemetry-export.err.txt. Only a local staging error
// (creating the directory, copying the file out) marks the collector failed.
func telemetryCollector() collector {
	return collector{
		name: "telemetry",
		skip: skipWhenServiceAbsent(telemetryService),
		run:  collectTelemetry,
	}
}

// telemetryStartDate returns the ISO 8601 start-date argument for a look-back
// window of days ending at now. A non-positive days falls back to the 30-day
// default so an unset option never widens the export unbounded.
func telemetryStartDate(now time.Time, days int) string {
	if days <= 0 {
		days = defaultTelemetryDays
	}
	return now.UTC().AddDate(0, 0, -days).Format(telemetryStartDateLayout)
}

// newTelemetryExportContainerPath returns a fresh in-container export path for
// one run.
func newTelemetryExportContainerPath() string {
	return telemetryExportContainerPrefix + strings.ToLower(rand.Text()) + ".json"
}

// telemetryExportCommand builds the infrahubctl invocation that exports
// snapshots from startDate onward to containerPath. Only --start-date is
// bounded, and Infrahub compares it against snapshot timestamps stored in UTC,
// so the window runs from 00:00 UTC on startDate to now: the N previous days
// plus the current day.
func telemetryExportCommand(containerPath, startDate string) []string {
	return []string{
		"infrahubctl", "telemetry", "export",
		"--output", containerPath,
		"--start-date", startDate,
	}
}

func collectTelemetry(cc *collectContext) error {
	dir := filepath.Join(cc.bundleDir, telemetryBundleDir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create %s directory: %w", telemetryBundleDir, err)
	}
	exportPath := filepath.Join(dir, telemetryExportFilename)

	containerPath := newTelemetryExportContainerPath()
	startDate := telemetryStartDate(time.Now(), cc.opts.TelemetryDays)
	command := telemetryExportCommand(containerPath, startDate)

	// The export pages through the API, so it gets the transfer bound rather
	// than the shorter status-dump bound.
	output, err := cc.execDumpTimeout(collectTransferTimeout, telemetryService, command)

	// Best-effort cleanup of the in-container export file, regardless of
	// outcome. Routed through the bounded collect exec path so a wedged
	// container cannot hang the run during cleanup.
	defer func() {
		if _, rmErr := cc.execDump(telemetryService, []string{"rm", "-f", containerPath}); rmErr != nil {
			logrus.Warnf("Failed to clean up telemetry export %s on %s: %v", containerPath, telemetryService, rmErr)
		}
	}()

	if err != nil {
		// Record the attempt beside the (absent) export like every other
		// failed dump: the command, the error, and infrahubctl's combined
		// output (its message or traceback). Then degrade to skipped: a
		// missing export is not a bundle defect.
		if writeErr := writeDumpFailure(exportPath, command, err, output, "", nil); writeErr != nil {
			logrus.Warnf("Failed to write telemetry export failure: %v", writeErr)
		}
		return &collectSkipError{reason: telemetrySkipReason(output, err)}
	}

	if err := cc.copyFrom(telemetryService, containerPath, exportPath); err != nil {
		return fmt.Errorf("failed to copy telemetry export from %s: %w", telemetryService, err)
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
