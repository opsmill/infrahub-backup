package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"
)

// Per-collector subprocess time bounds (research R2): exec/status dumps get
// 60s, log downloads and file copies get 5 minutes. The opt-in benchmark gets
// its own generous bound: it downloads an image (--pull always) and then runs
// disk and CPU measurements, which can exceed the transfer bound on slow
// disks or links.
const (
	collectExecTimeout      = 60 * time.Second
	collectTransferTimeout  = 5 * time.Minute
	collectBenchmarkTimeout = 10 * time.Minute
)

// CollectOptions aggregates the create-command inputs resolved from
// flags/environment variables by the entry point.
type CollectOptions struct {
	OutputDir      string
	LogLines       int
	IncludeBackup  bool
	IncludeQueries bool
	Benchmark      bool
}

// Replica is a read-only descriptor of one running container of a service.
// On Docker, Container carries the container name and Pod is empty; on
// Kubernetes there is one Replica per pod container, and Restarted reports
// whether that container has restartCount > 0 (previous logs available).
type Replica struct {
	Service   string
	Pod       string
	Container string
	Restarted bool
}

// collectBackend is the narrow read-only surface collectors consume on top of
// the shared EnvironmentBackend. Both DockerBackend and KubernetesBackend
// satisfy it once their replica/log/metrics primitives are implemented; unit
// tests provide a fake.
type collectBackend interface {
	EnvironmentBackend
	// ServiceReplicas enumerates the running replicas of a service.
	ServiceReplicas(service string) ([]Replica, error)
	// ReplicaLogs streams up to tailLines of a replica's logs; previous
	// requests the prior container's logs (Kubernetes only). The caller must
	// drain the reader and then call the wait function.
	ReplicaLogs(replica Replica, tailLines int, previous bool) (io.ReadCloser, func() error, error)
	// Metrics returns one-shot resource metrics for the deployment
	// (docker stats / kubectl top).
	Metrics() (string, error)
}

// collectContext carries the per-run state handed to each collector.
type collectContext struct {
	ctx       context.Context
	iops      *InfrahubOps
	backend   EnvironmentBackend
	opts      CollectOptions
	bundleDir string
	// manifest lets collectors contribute run-level fields (e.g. the
	// server-info collector fills infrahub_version best-effort). Outcome
	// entries stay the orchestrator's job.
	manifest *BundleManifest
	// artifact is a filesystem reference handed off by the running collector
	// (e.g. the --include-backup archive path); the orchestrator moves it
	// into that collector's manifest entry after a successful run.
	artifact string
}

// setArtifact hands the orchestrator a filesystem artifact reference to
// record on the current collector's manifest entry.
func (cc *collectContext) setArtifact(path string) {
	cc.artifact = path
}

// takeArtifact returns and clears the pending artifact reference so it never
// leaks into a later collector's entry.
func (cc *collectContext) takeArtifact() string {
	artifact := cc.artifact
	cc.artifact = ""
	return artifact
}

// collect resolves the active environment backend to the collect-side
// interface. Collectors that need replica/log/metrics primitives call this
// and surface a clear per-collector failure when the backend does not
// implement them.
func (cc *collectContext) collect() (collectBackend, error) {
	backend, ok := cc.backend.(collectBackend)
	if !ok {
		return nil, fmt.Errorf("%s environment does not support bundle collection primitives", cc.backend.Name())
	}
	return backend, nil
}

// collector is the internal unit of collection: a named step that writes
// files under the staging bundle directory. skip is an optional precondition
// returning (true, reason) when the collector does not apply to this run.
type collector struct {
	name string
	skip func(cc *collectContext) (bool, string)
	run  func(cc *collectContext) error
}

// collectSkipError marks a collector run outcome that must be recorded as
// skipped — with a warning reason — instead of failed: the opt-in benchmark
// degrades this way when its image cannot be pulled or run, so air-gapped
// runs stay clean (research R11, spec air-gapped edge case).
type collectSkipError struct{ reason string }

func (e *collectSkipError) Error() string { return e.reason }

// CollectBundle gathers a troubleshooting bundle from the detected
// environment into <OutputDir>/support_bundle_<collect_id>.tar.gz. Individual
// collector failures are recorded in the bundle manifest and never abort the
// run (FR-009); only a missing environment, an unwritable output directory,
// or an archiving failure returns an error.
func (iops *InfrahubOps) CollectBundle(opts CollectOptions) error {
	backend, err := iops.ensureBackend()
	if err != nil {
		return fmt.Errorf("bundle collection requires a usable Infrahub environment: %w", err)
	}

	return iops.runCollectPlan(backend, opts, iops.collectPlan(opts))
}

// collectPlan builds the ordered collector run plan for this run: logs per
// canonical service, then the parity diagnostics (database → message-queue →
// cache → task-worker → task-manager → server), then metrics (research R9).
// The manifest's environment and log_lines are set at construction; the
// server-info collector fills infrahub_version best-effort.
func (iops *InfrahubOps) collectPlan(opts CollectOptions) []collector {
	plan := serviceLogCollectors()
	plan = append(plan,
		databaseLogsCollector(),
		messageQueueCollector(),
		cacheCollector(),
		taskWorkerCollector(),
		taskManagerCollector(),
		serverInfoCollector(),
		metricsCollector(),
	)

	// Opt-in extras run last so the always-on diagnostics are already staged
	// when they start. The benchmark generates load, so it runs only after
	// every read-only collector has captured the undisturbed state; the
	// include-backup collector inherits the standard backup behavior — it may
	// stop/restart application containers — so it must stay last of all.
	plan = append(plan,
		benchmarkCollector(runEnvironmentBenchmark),
		includeBackupCollector(runStandardBackup),
	)
	return plan
}

// deploymentDescriber is an optional backend capability: report the Helm
// chart provenance of the deployment (release name, chart, chart version) for
// the bundle manifest. Only the Kubernetes backend implements it; Docker
// installs are never Helm-managed.
type deploymentDescriber interface {
	HelmRelease() (*HelmRelease, error)
}

// populateHelmRelease best-effort fills the manifest's Helm field on backends
// that can report it (Kubernetes). Helm metadata is deployment provenance, not
// a collector, so a detection failure is logged at debug and simply leaves the
// field unset rather than being recorded as a collector outcome.
func populateHelmRelease(backend EnvironmentBackend, manifest *BundleManifest) {
	describer, ok := backend.(deploymentDescriber)
	if !ok {
		return
	}
	helm, err := describer.HelmRelease()
	if err != nil {
		logrus.Debugf("Could not detect Helm chart metadata: %v", err)
		return
	}
	if helm != nil {
		manifest.Helm = helm
	}
}

// safeRun executes a collector's run function, converting a panic into a
// recorded failure so one misbehaving collector never aborts the whole run
// (FR-009, FIX-3).
func safeRun(c collector, cc *collectContext) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return c.run(cc)
}

// safeSkip evaluates a collector's optional skip precondition, converting a
// panic into an error so the orchestrator records the collector as failed
// rather than crashing the whole run (FIX-3).
func safeSkip(c collector, cc *collectContext) (skip bool, reason string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	if c.skip == nil {
		return false, "", nil
	}
	skip, reason = c.skip(cc)
	return skip, reason, nil
}

// runCollectPlan stages the bundle, runs every collector in order, finalizes
// the manifest, and packages the archive. The staging directory is removed on
// success, failure, and interrupt (SIGINT/SIGTERM).
func (iops *InfrahubOps) runCollectPlan(backend EnvironmentBackend, opts CollectOptions, plan []collector) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := os.MkdirAll(opts.OutputDir, 0755); err != nil {
		return fmt.Errorf("failed to create output directory %s: %w", opts.OutputDir, err)
	}

	workDir, err := os.MkdirTemp(opts.OutputDir, "infrahub_collect_*")
	if err != nil {
		return fmt.Errorf("failed to create staging directory in %s: %w", opts.OutputDir, err)
	}
	defer func() {
		if removeErr := os.RemoveAll(workDir); removeErr != nil {
			logrus.Warnf("Failed to remove staging directory %s: %v", workDir, removeErr)
		}
	}()

	bundleDir := filepath.Join(workDir, "bundle")
	if err := os.MkdirAll(bundleDir, 0755); err != nil {
		return fmt.Errorf("failed to create staging bundle directory %s: %w", bundleDir, err)
	}

	collectID := generateCollectID()
	archivePath := filepath.Join(opts.OutputDir, fmt.Sprintf("support_bundle_%s.tar.gz", collectID))
	manifest := newBundleManifest(collectID, backend.Name(), opts.LogLines)
	populateHelmRelease(backend, manifest)

	logrus.WithFields(logrus.Fields{
		"collect_id":  collectID,
		"output_dir":  opts.OutputDir,
		"environment": backend.Name(),
	}).Info("Collecting troubleshooting bundle")

	cc := &collectContext{
		ctx:       ctx,
		iops:      iops,
		backend:   backend,
		opts:      opts,
		bundleDir: bundleDir,
		manifest:  manifest,
	}

	for _, c := range plan {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("bundle collection interrupted: %w", ctxErr)
		}

		if skip, reason, skipErr := safeSkip(c, cc); skipErr != nil {
			// A panicking skip precondition must not abort the run (FR-009):
			// record it as a failure and continue.
			logrus.Warnf("Collector %s failed: %v", c.name, skipErr)
			manifest.recordFailed(c.name, skipErr.Error())
			continue
		} else if skip {
			logrus.Infof("Skipping %s: %s", c.name, reason)
			manifest.recordSkipped(c.name, reason)
			continue
		}

		logrus.Infof("Collecting %s", c.name)
		if err := safeRun(c, cc); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return fmt.Errorf("bundle collection interrupted: %w", ctxErr)
			}

			var skip *collectSkipError
			if errors.As(err, &skip) {
				logrus.Warnf("Collector %s skipped: %s", c.name, skip.reason)
				cc.takeArtifact() // drop a skipped collector's artifact reference
				manifest.recordSkipped(c.name, skip.reason)
				continue
			}

			reason := err.Error()
			var timeout *timeoutError
			if errors.As(err, &timeout) {
				reason = timeout.Error()
			}
			logrus.Warnf("Collector %s failed: %v", c.name, err)
			cc.takeArtifact() // drop a failed collector's artifact reference
			manifest.recordFailed(c.name, reason)
			continue
		}
		if artifact := cc.takeArtifact(); artifact != "" {
			manifest.recordSuccessArtifact(c.name, artifact)
		} else {
			manifest.recordSuccess(c.name)
		}
	}

	// The manifest is finalized last so it reflects every collector outcome.
	if err := manifest.write(bundleDir); err != nil {
		return err
	}

	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("bundle collection interrupted: %w", ctxErr)
	}

	logrus.Info("Creating bundle archive...")
	if err := createTarball(archivePath, workDir, "bundle/"); err != nil {
		if removeErr := os.Remove(archivePath); removeErr != nil && !os.IsNotExist(removeErr) {
			logrus.Warnf("Failed to remove partial archive %s: %v", archivePath, removeErr)
		}
		return fmt.Errorf("failed to create bundle archive %s: %w", archivePath, err)
	}

	fields := logrus.Fields{"path": archivePath}
	if stat, statErr := os.Stat(archivePath); statErr == nil {
		fields["size_bytes"] = stat.Size()
		fields["size_human"] = formatBytes(stat.Size())
	}
	logrus.WithFields(fields).Info("Bundle created successfully")

	return nil
}
