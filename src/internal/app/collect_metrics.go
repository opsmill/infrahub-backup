package app

import (
	"fmt"
	"os"
	"path/filepath"
)

// metricsBundleDir is the bundle directory for one-shot resource metrics
// (docker stats / kubectl top; research R4).
const metricsBundleDir = "metrics"

// metricsFilename is stable across environments so the bundle layout stays
// identical on Docker and Kubernetes (SC-004).
const metricsFilename = "metrics.txt"

// metricsCollector captures one-shot container resource metrics through the
// backend Metrics primitive into bundle/metrics/. A missing metrics source
// (e.g. no metrics-server on Kubernetes) surfaces as a failed manifest entry,
// never a run failure (research R4).
func metricsCollector() collector {
	return collector{
		name: "metrics",
		run: func(cc *collectContext) error {
			cb, err := cc.collect()
			if err != nil {
				return err
			}

			output, err := cb.Metrics()
			if err != nil {
				return fmt.Errorf("failed to capture resource metrics: %w", err)
			}

			dir := filepath.Join(cc.bundleDir, metricsBundleDir)
			if err := os.MkdirAll(dir, 0755); err != nil {
				return fmt.Errorf("failed to create %s directory: %w", metricsBundleDir, err)
			}
			return writeDumpFile(filepath.Join(dir, metricsFilename), output, nil)
		},
	}
}
