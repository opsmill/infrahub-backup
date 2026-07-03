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
// 60s, log downloads and file copies get 5 minutes.
const (
	collectExecTimeout     = 60 * time.Second
	collectTransferTimeout = 5 * time.Minute
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

// collectPlan builds the ordered collector run plan for this run.
//
// TODO(003-collect-tool T025): register the full ordered plan — logs per
// service, database, message-queue, cache, task-worker, task-manager, server,
// metrics, and the opt-in extras — and populate the manifest's
// infrahub_version.
func (iops *InfrahubOps) collectPlan(opts CollectOptions) []collector {
	return nil
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
	}

	for _, c := range plan {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("bundle collection interrupted: %w", ctxErr)
		}

		if c.skip != nil {
			if skip, reason := c.skip(cc); skip {
				logrus.Infof("Skipping %s: %s", c.name, reason)
				manifest.recordSkipped(c.name, reason)
				continue
			}
		}

		logrus.Infof("Collecting %s", c.name)
		if err := c.run(cc); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return fmt.Errorf("bundle collection interrupted: %w", ctxErr)
			}

			reason := err.Error()
			var timeout *timeoutError
			if errors.As(err, &timeout) {
				reason = timeout.Error()
			}
			logrus.Warnf("Collector %s failed: %v", c.name, err)
			manifest.recordFailed(c.name, reason)
			continue
		}
		manifest.recordSuccess(c.name)
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
