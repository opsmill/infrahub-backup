package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/sirupsen/logrus"
)

// backupCollectorName is the manifest identifier of the include-backup
// collector (specs/003-collect-tool/data-model.md, CollectorResult.Name).
const backupCollectorName = "backup"

// backupArchiveGlob matches the tarball archives CreateBackup writes into the
// backup directory (see generateBackupFilename).
const backupArchiveGlob = "infrahub_backup_*.tar.gz"

// backupRunner produces a backup and returns the path of the created archive
// (empty when the artifact cannot be located). The production runner is
// runStandardBackup; unit tests inject fakes.
type backupRunner func(cc *collectContext) (string, error)

// includeBackupCollector returns the opt-in --include-backup collector
// (FR-014, research R10). It must stay last in the run plan: the delegated
// backup inherits the standard backup behavior, which may stop and restart
// application containers, so every read-only collector stages its files
// first and a backup failure cannot taint the diagnostics (US3 scenario 2).
// The produced archive stays a standalone file in the standard backup
// directory — referenced by the manifest, not embedded in the bundle.
func includeBackupCollector(run backupRunner) collector {
	return collector{
		name: backupCollectorName,
		skip: func(cc *collectContext) (bool, string) {
			return !cc.opts.IncludeBackup, "not requested"
		},
		run: func(cc *collectContext) error {
			artifact, err := run(cc)
			if err != nil {
				return fmt.Errorf("backup failed: %w", err)
			}
			cc.setArtifact(artifact)
			return nil
		},
	}
}

// runStandardBackup delegates to the existing CreateBackup unmodified, with
// the non-interactive `infrahub-backup create` defaults: no --force (the
// running-tasks safety check is preserved, Principle II), all Neo4j metadata,
// task-manager database included, no S3 upload, no sleep, no redaction, no
// encryption (research R10). CreateBackup does not return the path it wrote,
// so the new archive is identified by diffing the backup directory listing
// around the call.
func runStandardBackup(cc *collectContext) (string, error) {
	backupDir := cc.iops.config.BackupDir

	before, err := listBackupArchives(backupDir)
	if err != nil {
		return "", err
	}

	if err := cc.iops.CreateBackup(false, "all", false, false, false, 0, false, false, ""); err != nil {
		return "", err
	}

	after, err := listBackupArchives(backupDir)
	if err != nil {
		return "", err
	}

	artifact := newestNewArchive(before, after)
	if artifact == "" {
		// Nothing to reference (e.g. a non-tarball backend wrote elsewhere).
		// The backup itself succeeded, so this is a warning, not a failure.
		logrus.Warnf("Backup completed but no new archive was found in %s; the manifest entry will carry no artifact path", backupDir)
		return "", nil
	}

	if abs, absErr := filepath.Abs(artifact); absErr == nil {
		artifact = abs
	}
	return artifact, nil
}

// listBackupArchives returns the backup archives currently present in dir.
// A missing directory yields an empty list (CreateBackup creates it).
func listBackupArchives(dir string) ([]string, error) {
	archives, err := filepath.Glob(filepath.Join(dir, backupArchiveGlob))
	if err != nil {
		return nil, fmt.Errorf("failed to list backup archives in %s: %w", dir, err)
	}
	return archives, nil
}

// newestNewArchive returns the newest archive present in after but absent
// from before. Archive names embed a YYYYMMDD_HHMMSS timestamp, so within one
// directory the lexically greatest new path is the most recent one.
func newestNewArchive(before, after []string) string {
	seen := make(map[string]struct{}, len(before))
	for _, path := range before {
		seen[path] = struct{}{}
	}

	newest := ""
	for _, path := range after {
		if _, ok := seen[path]; ok {
			continue
		}
		if path > newest {
			newest = path
		}
	}
	return newest
}

// --- benchmark (T036, FR-013, research R11) ---

// benchmarkCollectorName is the manifest identifier of the opt-in --benchmark
// collector (specs/003-collect-tool/data-model.md, CollectorResult.Name).
const benchmarkCollectorName = "benchmark"

// benchmarkBundleDir is the bundle directory the benchmark collector owns
// (contracts/bundle-layout.md: present only when --benchmark ran).
const benchmarkBundleDir = "benchmark"

// defaultBenchmarkImage is the OpsMill benchmark image, ported from the Python
// `invoke bundle collect` implementation (opsmill/infrahub
// tasks/container_ops.py, collect_benchmark: `docker run --pull always --rm
// registry.opsmill.io/opsmill/bench`). The image measures the host against
// Infrahub's hardware requirements: disk read/write IOPS (fio), total memory,
// and single-core CPU performance.
const defaultBenchmarkImage = "registry.opsmill.io/opsmill/bench"

// benchmarkImageEnvVar overrides the benchmark image reference, keeping
// air-gapped-with-private-registry workflows possible (research R11).
const benchmarkImageEnvVar = "INFRAHUB_BENCHMARK_IMAGE"

// resolveBenchmarkImage returns the effective benchmark image reference:
// the INFRAHUB_BENCHMARK_IMAGE override when set, the build-time default
// otherwise. getenv is injectable for tests; production passes os.Getenv.
func resolveBenchmarkImage(getenv func(string) string) string {
	if image := getenv(benchmarkImageEnvVar); image != "" {
		return image
	}
	return defaultBenchmarkImage
}

// benchmarkResourceName names the transient benchmark container/pod after the
// collect run. Pod names must be valid RFC 1123 labels, so the collect ID is
// lowercased and its underscore replaced.
func benchmarkResourceName(collectID string) string {
	return "infrahub-collect-benchmark-" + strings.ReplaceAll(strings.ToLower(collectID), "_", "-")
}

// benchmarkRunner executes the benchmark image against the active environment
// and returns its raw combined output. The production runner is
// runEnvironmentBenchmark; unit tests inject fakes.
type benchmarkRunner func(cc *collectContext, image string) (string, error)

// benchmarkCollector returns the opt-in --benchmark collector (FR-013,
// research R11). The benchmark image download is the only network egress the
// tool ever performs, and only under this flag. A failing image pull or run
// is recorded as skipped with a warning — never failed — so air-gapped runs
// stay clean (spec air-gapped edge case); only a local staging error marks
// the collector failed.
func benchmarkCollector(run benchmarkRunner) collector {
	return collector{
		name: benchmarkCollectorName,
		skip: func(cc *collectContext) (bool, string) {
			return !cc.opts.Benchmark, "not requested"
		},
		run: func(cc *collectContext) error {
			image := resolveBenchmarkImage(os.Getenv)
			logrus.Infof("Running benchmark image %s (requires image download)", image)
			output, err := run(cc, image)
			if err != nil {
				return &collectSkipError{
					reason: fmt.Sprintf("benchmark image %s could not be pulled or run: %v", image, err),
				}
			}
			return stageBenchmarkOutput(cc.bundleDir, output)
		},
	}
}

// runEnvironmentBenchmark dispatches the benchmark to the active backend: a
// transient `docker run` attached to the compose project's network, or a
// one-off attached pod in the Kubernetes namespace. Both backends delete the
// container/pod they created — permitted under FR-010, which protects the
// deployment's workloads: the benchmark resource is the tool's own.
func runEnvironmentBenchmark(cc *collectContext, image string) (string, error) {
	name := benchmarkResourceName(cc.manifest.CollectID)
	switch backend := cc.backend.(type) {
	case *DockerBackend:
		return backend.RunBenchmark(cc.ctx, image, name)
	case *KubernetesBackend:
		return backend.RunBenchmark(cc.ctx, image, name)
	default:
		return "", fmt.Errorf("%s environment does not support the benchmark", cc.backend.Name())
	}
}

// commandErrorLine condenses a failed command's combined output into its one
// actionable line: the last line mentioning an error (docker and kubectl both
// print one), falling back to the last non-empty line. This keeps manifest
// reasons readable without the image pull-progress noise.
func commandErrorLine(output string) string {
	lines := nonEmptyLines(output)
	if len(lines) == 0 {
		return ""
	}
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.Contains(strings.ToLower(lines[i]), "error") {
			return lines[i]
		}
	}
	return lines[len(lines)-1]
}

// benchmarkResultRe extracts one benchmark result line
// ("<category>: <value>[ MB] - Required: <required>[ MB] : <status>"),
// ported from the Python collect_benchmark parser.
var benchmarkResultRe = regexp.MustCompile(`(\w+(?: \w+)*): (\d+)(?: MB)? - Required: (\d+)(?: MB)? : (\w+)`)

// ansiEscapeRe matches the ANSI SGR color sequences the benchmark entrypoint
// wraps its result lines in.
var ansiEscapeRe = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// benchmarkMeasure is one parsed benchmark result; the field names mirror the
// Python tool's benchmark.json entries.
type benchmarkMeasure struct {
	Value    int    `json:"value"`
	Required int    `json:"required"`
	Status   string `json:"status"`
}

// benchmarkReport is the benchmark.json document, shape-compatible with the
// Python tool's output.
type benchmarkReport struct {
	RawOutput string                      `json:"raw_output"`
	Results   map[string]benchmarkMeasure `json:"results"`
}

// parseBenchmarkResults strips color codes from the benchmark output and
// extracts the result lines into keyed measures ("memory", "disk_read_iops",
// ...). It returns the cleaned output and the measures; unmatched output
// yields an empty map, never an error — the raw log is authoritative.
func parseBenchmarkResults(output string) (string, map[string]benchmarkMeasure) {
	clean := ansiEscapeRe.ReplaceAllString(output, "")
	results := map[string]benchmarkMeasure{}
	for _, match := range benchmarkResultRe.FindAllStringSubmatch(clean, -1) {
		value, err := strconv.Atoi(match[2])
		if err != nil {
			continue
		}
		required, err := strconv.Atoi(match[3])
		if err != nil {
			continue
		}
		key := strings.ToLower(strings.ReplaceAll(match[1], " ", "_"))
		results[key] = benchmarkMeasure{Value: value, Required: required, Status: match[4]}
	}
	return clean, results
}

// stageBenchmarkOutput writes the raw benchmark output (benchmark.log) and
// the parsed results (benchmark.json) into bundle/benchmark/.
func stageBenchmarkOutput(bundleDir, output string) error {
	dir := filepath.Join(bundleDir, benchmarkBundleDir)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create %s directory: %w", benchmarkBundleDir, err)
	}

	if err := writeDumpFile(filepath.Join(dir, "benchmark.log"), output, nil); err != nil {
		return err
	}

	raw, results := parseBenchmarkResults(output)
	data, err := json.MarshalIndent(benchmarkReport{RawOutput: raw, Results: results}, "", "    ")
	if err != nil {
		return fmt.Errorf("failed to marshal benchmark results: %w", err)
	}
	return writeDumpFile(filepath.Join(dir, "benchmark.json"), string(data), nil)
}
