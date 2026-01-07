package app

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
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
	}
	return &InfrahubOps{
		config:   config,
		executor: executor,
	}
}

func (iops *InfrahubOps) Config() *Configuration {
	return iops.config
}

// CommandExecutor handles command execution
type CommandExecutor struct{}

func NewCommandExecutor() *CommandExecutor {
	return &CommandExecutor{}
}

type lineLogger struct {
	buf     bytes.Buffer
	logFunc func(string)
}

func newLineLogger(logFunc func(string)) *lineLogger {
	return &lineLogger{logFunc: logFunc}
}

func (l *lineLogger) Write(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 {
		if idx := bytes.IndexByte(p, '\n'); idx >= 0 {
			l.buf.Write(p[:idx])
			l.flush()
			p = p[idx+1:]
			continue
		}
		l.buf.Write(p)
		break
	}
	return total, nil
}

func (l *lineLogger) flush() {
	if l.buf.Len() == 0 {
		return
	}
	l.logFunc(l.buf.String())
	l.buf.Reset()
}

func (l *lineLogger) Flush() {
	l.flush()
}

func (ce *CommandExecutor) runCommand(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	output, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(output)), err
}

func (ce *CommandExecutor) runCommandQuiet(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	return cmd.Run()
}

func (ce *CommandExecutor) runCommandWithStream(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", err
	}

	if err := cmd.Start(); err != nil {
		return "", err
	}

	var stdoutBuf bytes.Buffer
	stdoutLogger := newLineLogger(func(line string) {
		logrus.Info(line)
	})
	stderrLogger := newLineLogger(func(line string) {
		logrus.Info(line)
	})

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		if _, copyErr := io.Copy(io.MultiWriter(&stdoutBuf, stdoutLogger), stdout); copyErr != nil {
			logrus.WithError(copyErr).Warn("failed reading command stdout")
		}
		stdoutLogger.Flush()
	}()

	go func() {
		defer wg.Done()
		if _, copyErr := io.Copy(stderrLogger, stderr); copyErr != nil {
			logrus.WithError(copyErr).Warn("failed reading command stderr")
		}
		stderrLogger.Flush()
	}()

	wg.Wait()

	err = cmd.Wait()
	return stdoutBuf.String(), err
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
			if !errors.Is(err, ErrEnvironmentNotFound) {
				detectionErrors = append(detectionErrors, fmt.Sprintf("%s: %v", backend.Name(), err))
			}
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
// set to INFRAHUB_INTERNAL_ADDRESS. If existingOpts is provided, its values are preserved
// and merged (existing Env values take precedence).
func (iops *InfrahubOps) buildTaskWorkerExecOpts(existingOpts *ExecOptions) *ExecOptions {
	internalAddr := iops.getInfrahubInternalAddress()

	if existingOpts == nil {
		if internalAddr == "" {
			return nil
		}
		return &ExecOptions{
			Env: map[string]string{
				"INFRAHUB_ADDRESS": internalAddr,
			},
		}
	}

	// Clone existing options
	opts := &ExecOptions{
		User: existingOpts.User,
		Env:  make(map[string]string),
	}

	// Set INFRAHUB_ADDRESS first (if available)
	if internalAddr != "" {
		opts.Env["INFRAHUB_ADDRESS"] = internalAddr
	}

	// Merge existing env vars (they take precedence)
	for k, v := range existingOpts.Env {
		opts.Env[k] = v
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

// Prerequisites checker
func (iops *InfrahubOps) checkPrerequisites() error {
	// Docker and kubectl are now optional. This function always succeeds.
	return nil
}

func (iops *InfrahubOps) fetchDatabaseCredentials() error {
	if _, err := iops.ensureBackend(); err != nil {
		return err
	}

	if value := os.Getenv("INFRAHUB_DB_DATABASE"); value != "" {
		iops.config.Neo4jDatabase = value
	}
	if value := os.Getenv("INFRAHUB_DB_USERNAME"); value != "" {
		iops.config.Neo4jUsername = value
	}
	if value := os.Getenv("INFRAHUB_DB_PASSWORD"); value != "" {
		iops.config.Neo4jPassword = value
	}

	iops.applyPrefectConnection(os.Getenv("PREFECT_API_DATABASE_CONNECTION_URL"))

	if iops.config.Neo4jDatabase == "" || iops.config.Neo4jUsername == "" || iops.config.Neo4jPassword == "" {
		envOut, err := iops.Exec("infrahub-server", []string{"env"}, nil)
		if err != nil {
			logrus.Warnf("Could not get infrahub-server env, using default Neo4j credentials: %v", err)
		} else {
			for _, line := range strings.Split(envOut, "\n") {
				if after, ok := strings.CutPrefix(line, "INFRAHUB_DB_DATABASE="); ok && iops.config.Neo4jDatabase == "" {
					iops.config.Neo4jDatabase = after
				}
				if after, ok := strings.CutPrefix(line, "INFRAHUB_DB_USERNAME="); ok && iops.config.Neo4jUsername == "" {
					iops.config.Neo4jUsername = after
				}
				if after, ok := strings.CutPrefix(line, "INFRAHUB_DB_PASSWORD="); ok && iops.config.Neo4jPassword == "" {
					iops.config.Neo4jPassword = after
				}
			}
		}

		if iops.config.Neo4jDatabase == "" {
			iops.config.Neo4jDatabase = "neo4j"
		}
		if iops.config.Neo4jUsername == "" {
			iops.config.Neo4jUsername = "neo4j"
		}
		if iops.config.Neo4jPassword == "" {
			iops.config.Neo4jPassword = "admin"
		}
	}

	if iops.config.PostgresDatabase == "" || iops.config.PostgresUsername == "" || iops.config.PostgresPassword == "" {
		envOut, err := iops.Exec("task-manager", []string{"env"}, nil)
		if err != nil {
			logrus.Warnf("Could not get task-manager env, using default Postgres credentials: %v", err)
		} else {
			for _, line := range strings.Split(envOut, "\n") {
				if after, ok := strings.CutPrefix(line, "PREFECT_API_DATABASE_CONNECTION_URL="); ok {
					iops.applyPrefectConnection(after)
					break
				}
			}
		}

		if iops.config.PostgresDatabase == "" {
			iops.config.PostgresDatabase = "prefect"
		}
		if iops.config.PostgresUsername == "" {
			iops.config.PostgresUsername = "postgres"
		}
		if iops.config.PostgresPassword == "" {
			iops.config.PostgresPassword = "prefect"
		}
	}

	return nil
}

func (iops *InfrahubOps) applyPrefectConnection(connStr string) {
	if connStr == "" {
		return
	}

	re := regexp.MustCompile("postgres(.*)://(.*)")
	normalized := re.ReplaceAllString(connStr, "postgres://$2")

	connConfig, err := pgx.ParseConfig(normalized)
	if err != nil {
		logrus.Warnf("Could not parse PREFECT_API_DATABASE_CONNECTION_URL: %v", err)
		return
	}

	if connConfig.Database != "" {
		iops.config.PostgresDatabase = connConfig.Database
	}
	if connConfig.User != "" {
		iops.config.PostgresUsername = connConfig.User
	}
	if connConfig.Password != "" {
		iops.config.PostgresPassword = connConfig.Password
	}
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
