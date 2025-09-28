package main

import (
	"bufio"
	"embed"

	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sirupsen/logrus"
)

// InfrahubOps is the main application struct
type InfrahubOps struct {
	config *Configuration
}

// Embedded scripts
//
//go:embed scripts
var scripts embed.FS

// NewInfrahubOps creates a new InfrahubOps instance
func NewInfrahubOps() *InfrahubOps {
	config := &Configuration{
		BackupDir: getEnvOrDefault("BACKUP_DIR", filepath.Join(getCurrentDir(), "infrahub_backups")),
	}
	return &InfrahubOps{
		config: config,
	}
}

// CommandExecutor handles command execution
type CommandExecutor struct{}

func NewCommandExecutor() *CommandExecutor {
	return &CommandExecutor{}
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
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()

	if err := cmd.Start(); err != nil {
		return "", err
	}

	var output string
	outScanner := bufio.NewScanner(stdout)
	errScanner := bufio.NewScanner(stderr)

	done := make(chan struct{}, 2)

	go func() {
		for outScanner.Scan() {
			line := outScanner.Text()
			logrus.Info(line)
			output += line + "\n"
		}
		done <- struct{}{}
	}()

	go func() {
		for errScanner.Scan() {
			line := errScanner.Text()
			logrus.Info(line)
			output += line + "\n"
		}
		done <- struct{}{}
	}()

	// Wait for both stdout and stderr to finish
	<-done
	<-done

	err := cmd.Wait()
	return output, err
}

// Prerequisites checker
func (iops *InfrahubOps) checkPrerequisites() error {
	// Docker and kubectl are now optional. This function always succeeds.
	return nil
}

// Docker project detection
func (iops *InfrahubOps) detectDockerProjects() ([]string, error) {
	executor := NewCommandExecutor()
	projects := []string{}

	// Check if docker is available
	if err := executor.runCommandQuiet("docker", "--version"); err != nil {
		return projects, nil // Docker not available
	}

	// Get docker compose projects
	output, err := executor.runCommand("docker", "compose", "ls")
	if err != nil {
		return projects, nil // Not an error, just no projects
	}

	lines := strings.Split(output, "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "NAME") {
			continue // Skip header
		}

		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}

		project := fields[0]
		if project == "" {
			continue
		}

		// Check if project has infrahub services
		psOutput, err := executor.runCommand("docker", "compose", "-p", project, "ps", "-a")
		if err != nil {
			continue
		}

		if strings.Contains(strings.ToLower(psOutput), "infrahub") {
			projects = append(projects, project)
		}
	}
	sort.Strings(projects)
	return projects, nil
}

// Kubernetes detection (for Phase 1 warning)
func (iops *InfrahubOps) detectK8sNamespaces() ([]string, error) {
	executor := NewCommandExecutor()

	// Check if kubectl is available
	if err := executor.runCommandQuiet("kubectl", "version", "--client"); err != nil {
		return nil, nil // kubectl not available
	}

	output, err := executor.runCommand("kubectl", "get", "pods", "--all-namespaces",
		"-o", "custom-columns=:metadata.namespace", "-l", "app.kubernetes.io/name=infrahub")
	if err != nil {
		return nil, nil // No error, just no pods found
	}

	namespaces := []string{}
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" && line != "NAMESPACE" {
			namespaces = append(namespaces, line)
		}
	}

	return namespaces, nil
}

// Environment detection
func (iops *InfrahubOps) detectEnvironment() error {
	logrus.Info("Detecting Infrahub deployment environment...")

	logrus.Info("Detecting Docker Compose projects...")
	dockerProjects, err := iops.detectDockerProjects()
	if err != nil {
		return fmt.Errorf("error detecting docker projects: %w", err)
	}

	if len(dockerProjects) > 0 {
		logrus.Info("Found Docker Compose deployment(s):")
		for _, project := range dockerProjects {
			fmt.Printf("  %s\n", project)
		}

		if len(dockerProjects) > 1 {
			logrus.Warn("Multiple projects found. Use --project=NAME to specify target.")
		} else {
			iops.config.DockerComposeProject = dockerProjects[0]
			logrus.Infof("Using project: %s", iops.config.DockerComposeProject)
		}

		return nil
	}

	// Check for Kubernetes
	k8sNamespaces, _ := iops.detectK8sNamespaces()
	if len(k8sNamespaces) > 0 {
		logrus.Warn("Kubernetes deployment detected but not supported in Phase 1")
		logrus.Warn("Phase 1 supports Docker Compose deployments only")
		for _, ns := range k8sNamespaces {
			fmt.Printf("  %s\n", ns)
		}
		return fmt.Errorf("kubernetes deployments not supported in Phase 1")
	}

	logrus.Error("No Infrahub deployment found")
	logrus.Error("Ensure Infrahub is running via Docker Compose")
	return fmt.Errorf("no infrahub deployment found")
}

func (iops *InfrahubOps) getInfrahubVersion() string {
	executor := NewCommandExecutor()

	if iops.config.DockerComposeProject == "" {
		return "unknown"
	}

	cmd := []string{"compose", "-p", iops.config.DockerComposeProject, "exec", "-T", "infrahub-server",
		"python", "-c", "import infrahub; print(infrahub.__version__)"}

	output, err := executor.runCommand("docker", cmd...)
	if err != nil {
		return "unknown"
	}

	return strings.TrimSpace(output)
}

func (iops *InfrahubOps) composeExec(args ...string) (string, error) {
	executor := NewCommandExecutor()
	cmd := []string{"compose"}

	if iops.config.DockerComposeProject != "" {
		cmd = append(cmd, "-p", iops.config.DockerComposeProject)
	}

	cmd = append(cmd, args...)
	return executor.runCommand("docker", cmd...)
}

func (iops *InfrahubOps) composeExecWithStream(args ...string) (string, error) {
	executor := NewCommandExecutor()
	cmd := []string{"compose"}

	if iops.config.DockerComposeProject != "" {
		cmd = append(cmd, "-p", iops.config.DockerComposeProject)
	}

	cmd = append(cmd, args...)
	return executor.runCommandWithStream("docker", cmd...)
}

func (iops *InfrahubOps) restartDependencies() error {
	logrus.Info("Restarting cache and message-queue")
	if _, err := iops.composeExec("start", "cache", "message-queue"); err != nil {
		return fmt.Errorf("failed to restart cache and message-queue: %w", err)
	}

	logrus.Info("Restarting task manager...")
	if _, err := iops.composeExec("start", "task-manager", "task-manager-background-svc"); err != nil {
		return fmt.Errorf("failed to restart task manager: %w", err)
	}

	return nil
}

func (iops *InfrahubOps) executeScript(targetService string, scriptContent string, targetPath string, args ...string) (string, error) {
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

	if output, err := iops.composeExec("cp", "-a", tmpFile.Name(), fmt.Sprintf("%s:%s", targetService, targetPath)); err != nil {
		return "", fmt.Errorf("failed to copy script to container: %w\n%v", err, output)
	}
	defer iops.composeExec("exec", "-T", targetService, "rm", "-f", targetPath)

	// Execute script inside container
	logrus.Info("Executing script inside container...")

	output, err := iops.composeExecWithStream(append([]string{"exec", "-T", targetService}, args...)...)
	if err != nil {
		return output, fmt.Errorf("failed to execute script: %w", err)
	}

	return output, nil
}
