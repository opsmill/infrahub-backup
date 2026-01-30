package app

import (
	"embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sirupsen/logrus"
)

// scriptsFS holds the embedded maintenance scripts.
//
//go:embed scripts/*
var scriptsFS embed.FS

// Embeddable scripts require exposing a helper to other packages.
func ReadScript(name string) ([]byte, error) {
	return scriptsFS.ReadFile("scripts/" + name)
}

// Configuration holds the application configuration
type Configuration struct {
	BackupDir            string
	DockerComposeProject string
	K8sNamespace         string
	Neo4jUsername        string
	Neo4jPassword        string
	Neo4jDatabase        string
	PostgresUsername     string
	PostgresPassword     string
	PostgresDatabase     string
	S3                   *S3Config
}

// InfrahubOps is the main application struct
type InfrahubOps struct {
	config                  *Configuration
	backend                 EnvironmentBackend
	executor                *CommandExecutor
	dockerBackend           *DockerBackend
	kubernetesBackend       *KubernetesBackend
	infrahubInternalAddress string // cached INFRAHUB_INTERNAL_ADDRESS from task-worker
}

// NewInfrahubOps creates a new InfrahubOps instance
func NewInfrahubOps() *InfrahubOps {
	executor := NewCommandExecutor()
	config := &Configuration{
		BackupDir:    getEnvOrDefault("BACKUP_DIR", filepath.Join(getCurrentDir(), "infrahub_backups")),
		K8sNamespace: os.Getenv("INFRAHUB_K8S_NAMESPACE"),
		S3: &S3Config{
			Region: "us-east-1",
		},
	}
	return &InfrahubOps{
		config:   config,
		executor: executor,
	}
}

func (iops *InfrahubOps) Config() *Configuration {
	return iops.config
}

func (iops *InfrahubOps) getDockerBackend() *DockerBackend {
	if iops.dockerBackend == nil {
		iops.dockerBackend = NewDockerBackend(iops.config, iops.executor)
	}
	return iops.dockerBackend
}

func (iops *InfrahubOps) getKubernetesBackend() *KubernetesBackend {
	if iops.kubernetesBackend == nil {
		iops.kubernetesBackend = NewKubernetesBackend(iops.config, iops.executor)
	}
	return iops.kubernetesBackend
}

func (iops *InfrahubOps) backendOrder() []EnvironmentBackend {
	order := []EnvironmentBackend{}
	add := func(backend EnvironmentBackend) {
		if backend == nil {
			return
		}
		for _, existing := range order {
			if existing.Name() == backend.Name() {
				return
			}
		}
		order = append(order, backend)
	}

	if iops.config.K8sNamespace != "" {
		add(iops.getKubernetesBackend())
	}
	if iops.config.DockerComposeProject != "" {
		add(iops.getDockerBackend())
	}

	add(iops.getDockerBackend())
	add(iops.getKubernetesBackend())

	return order
}

func (iops *InfrahubOps) ensureBackend() (EnvironmentBackend, error) {
	if iops.backend != nil {
		return iops.backend, nil
	}

	detectionErrors := []string{}
	for _, backend := range iops.backendOrder() {
		if backend == nil {
			continue
		}
		if err := backend.Detect(); err != nil {
			// Soft failures: CLI not available OR environment not found during auto-detect
			if errors.Is(err, ErrEnvironmentNotFound) || errors.Is(err, ErrCLIUnavailable) {
				logrus.Debugf("Skipping %s backend: %v", backend.Name(), err)
				continue
			}
			// Hard failures: something went wrong that should be reported
			detectionErrors = append(detectionErrors, fmt.Sprintf("%s: %v", backend.Name(), err))
			continue
		}
		iops.backend = backend
		logrus.Infof("Detected %s environment (%s)", backend.Name(), backend.Info())
		return backend, nil
	}

	if len(detectionErrors) > 0 {
		return nil, fmt.Errorf("environment detection errors: %s", strings.Join(detectionErrors, "; "))
	}

	return nil, fmt.Errorf("no Infrahub environment detected")
}

func (iops *InfrahubOps) Exec(service string, command []string, opts *ExecOptions) (string, error) {
	backend, err := iops.ensureBackend()
	if err != nil {
		return "", err
	}
	return backend.Exec(service, command, opts)
}

// getInfrahubInternalAddress fetches and caches INFRAHUB_INTERNAL_ADDRESS from task-worker.
// Returns empty string if the env var is not set (with a warning logged).
func (iops *InfrahubOps) getInfrahubInternalAddress() string {
	if iops.infrahubInternalAddress != "" {
		return iops.infrahubInternalAddress
	}

	backend, err := iops.ensureBackend()
	if err != nil {
		logrus.Warnf("Could not get backend to fetch INFRAHUB_INTERNAL_ADDRESS: %v", err)
		return ""
	}

	output, err := backend.Exec("task-worker", []string{"printenv", "INFRAHUB_INTERNAL_ADDRESS"}, nil)
	if err != nil {
		logrus.Debugf("INFRAHUB_INTERNAL_ADDRESS not set in task-worker container: %v", err)
		return ""
	}

	iops.infrahubInternalAddress = strings.TrimSpace(output)
	if iops.infrahubInternalAddress != "" {
		logrus.Debugf("Cached INFRAHUB_INTERNAL_ADDRESS: %s", iops.infrahubInternalAddress)
	}
	return iops.infrahubInternalAddress
}

// buildTaskWorkerExecOpts creates ExecOptions for task-worker commands with INFRAHUB_ADDRESS
// set to INFRAHUB_INTERNAL_ADDRESS. Existing options are merged with precedence to user values.
func (iops *InfrahubOps) buildTaskWorkerExecOpts(existingOpts *ExecOptions) *ExecOptions {
	internalAddr := iops.getInfrahubInternalAddress()
	if internalAddr == "" && existingOpts == nil {
		return nil
	}

	opts := &ExecOptions{Env: make(map[string]string)}
	if existingOpts != nil {
		opts.User = existingOpts.User
	}

	// Set INFRAHUB_ADDRESS if available, then merge user env vars (user values override)
	if internalAddr != "" {
		opts.Env["INFRAHUB_ADDRESS"] = internalAddr
	}
	if existingOpts != nil {
		for k, v := range existingOpts.Env {
			opts.Env[k] = v
		}
	}

	if len(opts.Env) == 0 {
		opts.Env = nil
	}
	return opts
}

func (iops *InfrahubOps) ExecStream(service string, command []string, opts *ExecOptions) (string, error) {
	backend, err := iops.ensureBackend()
	if err != nil {
		return "", err
	}
	return backend.ExecStream(service, command, opts)
}

func (iops *InfrahubOps) CopyTo(service, src, dest string) error {
	backend, err := iops.ensureBackend()
	if err != nil {
		return err
	}
	return backend.CopyTo(service, src, dest)
}

func (iops *InfrahubOps) CopyFrom(service, src, dest string) error {
	backend, err := iops.ensureBackend()
	if err != nil {
		return err
	}
	return backend.CopyFrom(service, src, dest)
}

func (iops *InfrahubOps) StartServices(services ...string) error {
	backend, err := iops.ensureBackend()
	if err != nil {
		return err
	}
	return backend.Start(services...)
}

func (iops *InfrahubOps) StopServices(services ...string) error {
	backend, err := iops.ensureBackend()
	if err != nil {
		return err
	}
	return backend.Stop(services...)
}

func (iops *InfrahubOps) IsServiceRunning(service string) (bool, error) {
	backend, err := iops.ensureBackend()
	if err != nil {
		return false, err
	}
	return backend.IsRunning(service)
}

// GetAllPods returns all pod names for a service (Kubernetes only, returns nil for Docker)
func (iops *InfrahubOps) GetAllPods(service string) ([]string, error) {
	backend, err := iops.ensureBackend()
	if err != nil {
		return nil, err
	}
	if k8s, ok := backend.(*KubernetesBackend); ok {
		return k8s.GetAllPods(service)
	}
	// For Docker, return nil (single instance)
	return nil, nil
}

// Prerequisites checker
func (iops *InfrahubOps) checkPrerequisites() error {
	// Docker and kubectl are now optional. This function always succeeds.
	return nil
}

// Environment detection
func (iops *InfrahubOps) DetectEnvironment() error {
	logrus.Info("Detecting Infrahub deployment environment...")
	backend, err := iops.ensureBackend()
	if err != nil {
		return err
	}

	if backend.Info() != "" {
		logrus.Infof("Target: %s", backend.Info())
	}

	if err := iops.fetchDatabaseCredentials(); err != nil {
		return fmt.Errorf("could not fetch database credentials: %w", err)
	}

	return nil
}

func (iops *InfrahubOps) getInfrahubVersion() string {
	output, err := iops.Exec("infrahub-server", []string{"python", "-c", "import infrahub; print(infrahub.__version__)"}, nil)
	if err != nil {
		logrus.Warnf("Could not detect Infrahub version: %v", err)
		return "unknown"
	}

	return strings.TrimSpace(output)
}

func (iops *InfrahubOps) restartDependencies() error {
	logrus.Info("Restarting cache and message-queue")
	if err := iops.StopServices("cache", "message-queue"); err != nil {
		logrus.Debugf("Failed to stop cache/message-queue: %v", err)
	}
	if err := iops.StartServices("cache", "message-queue"); err != nil {
		return fmt.Errorf("failed to restart cache and message-queue: %w", err)
	}

	logrus.Info("Restarting task manager...")
	if err := iops.StopServices("task-manager"); err != nil {
		logrus.Debugf("Failed to stop task-manager: %v", err)
	}
	if err := iops.StopServices("task-manager-background-svc"); err != nil {
		logrus.Debugf("Failed to stop optional task-manager-background-svc: %v", err)
	}
	if err := iops.StartServices("task-manager"); err != nil {
		return fmt.Errorf("failed to restart task-manager: %w", err)
	}
	if err := iops.StartServices("task-manager-background-svc"); err != nil {
		logrus.Infof("Skipping optional task-manager-background-svc restart: %v", err)
	}

	return nil
}
