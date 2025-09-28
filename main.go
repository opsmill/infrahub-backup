package main

import (
	"archive/tar"
	"compress/gzip"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// Configuration holds the application configuration
type Configuration struct {
	InfrahubImage        string
	BackupDir            string
	DockerComposeProject string
}

// BackupMetadata represents the backup metadata structure
type BackupMetadata struct {
	MetadataVersion int      `json:"metadata_version"`
	BackupID        string   `json:"backup_id"`
	CreatedAt       string   `json:"created_at"`
	ToolVersion     string   `json:"tool_version"`
	InfrahubVersion string   `json:"infrahub_version"`
	Components      []string `json:"components"`
}

// InfrahubOps is the main application struct
type InfrahubOps struct {
	config *Configuration
	logger *Logger
}

// Logger provides colored logging functionality
type Logger struct{}

// Embedded scripts
//
//go:embed scripts/clean_old_tasks.py
var cleanOldTasksScript string

//go:embed scripts/clean_stale_tasks.py
var cleanStaleTasksScript string

const (
	ColorReset  = "\033[0m"
	ColorRed    = "\033[31m"
	ColorGreen  = "\033[32m"
	ColorYellow = "\033[33m"
	ColorBlue   = "\033[34m"
)

func NewLogger() *Logger {
	return &Logger{}
}

func (l *Logger) Info(msg string, args ...interface{}) {
	fmt.Printf(ColorBlue+"[INFO]"+ColorReset+" "+msg+"\n", args...)
}

func (l *Logger) Success(msg string, args ...interface{}) {
	fmt.Printf(ColorGreen+"[SUCCESS]"+ColorReset+" "+msg+"\n", args...)
}

func (l *Logger) Warning(msg string, args ...interface{}) {
	fmt.Printf(ColorYellow+"[WARNING]"+ColorReset+" "+msg+"\n", args...)
}

func (l *Logger) Error(msg string, args ...interface{}) {
	fmt.Printf(ColorRed+"[ERROR]"+ColorReset+" "+msg+"\n", args...)
}

// NewInfrahubOps creates a new InfrahubOps instance
func NewInfrahubOps() *InfrahubOps {
	config := &Configuration{
		InfrahubImage: getEnvOrDefault("INFRAHUB_IMAGE", "infrahub:latest"),
		BackupDir:     getEnvOrDefault("BACKUP_DIR", filepath.Join(getCurrentDir(), "infrahub_backups")),
	}

	return &InfrahubOps{
		config: config,
		logger: NewLogger(),
	}
}

// CommandExecutor handles command execution
type CommandExecutor struct {
	logger *Logger
}

func NewCommandExecutor(logger *Logger) *CommandExecutor {
	return &CommandExecutor{logger: logger}
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

// Prerequisites checker
func (iops *InfrahubOps) checkPrerequisites() error {
	executor := NewCommandExecutor(iops.logger)
	missing := []string{}

	// Check for docker
	if err := executor.runCommandQuiet("docker", "--version"); err != nil {
		missing = append(missing, "docker")
	}

	// Check for docker compose
	if err := executor.runCommandQuiet("docker", "compose", "version"); err != nil {
		missing = append(missing, "docker-compose")
	}

	if len(missing) > 0 {
		iops.logger.Error("Missing required tools: %s", strings.Join(missing, ", "))
		iops.logger.Error("Please install the missing tools and try again")
		return fmt.Errorf("missing prerequisites: %v", missing)
	}

	return nil
}

// Docker project detection
func (iops *InfrahubOps) detectDockerProjects() ([]string, error) {
	executor := NewCommandExecutor(iops.logger)
	projects := []string{}

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

	// Check current directory for docker-compose.yml
	if _, err := os.Stat("docker-compose.yml"); err == nil {
		content, err := os.ReadFile("docker-compose.yml")
		if err == nil && strings.Contains(string(content), "infrahub") {
			currentDir := filepath.Base(getCurrentDir())
			projects = append(projects, currentDir)
		}
	}

	// Remove duplicates and sort
	unique := make(map[string]bool)
	var result []string
	for _, project := range projects {
		if !unique[project] {
			unique[project] = true
			result = append(result, project)
		}
	}
	sort.Strings(result)

	return result, nil
}

// Kubernetes detection (for Phase 1 warning)
func (iops *InfrahubOps) detectK8sNamespaces() ([]string, error) {
	executor := NewCommandExecutor(iops.logger)

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
	iops.logger.Info("Detecting Infrahub deployment environment...")

	iops.logger.Info("Detecting Docker Compose projects...")
	dockerProjects, err := iops.detectDockerProjects()
	if err != nil {
		return fmt.Errorf("error detecting docker projects: %w", err)
	}

	if len(dockerProjects) > 0 {
		iops.logger.Success("Found Docker Compose deployment(s):")
		for _, project := range dockerProjects {
			fmt.Printf("  %s\n", project)
		}

		if len(dockerProjects) > 1 {
			iops.logger.Warning("Multiple projects found. Use --project=NAME to specify target.")
		} else {
			iops.config.DockerComposeProject = dockerProjects[0]
			iops.logger.Info("Using project: %s", iops.config.DockerComposeProject)
		}

		return nil
	}

	// Check for Kubernetes
	k8sNamespaces, _ := iops.detectK8sNamespaces()
	if len(k8sNamespaces) > 0 {
		iops.logger.Warning("Kubernetes deployment detected but not supported in Phase 1")
		iops.logger.Warning("Phase 1 supports Docker Compose deployments only")
		for _, ns := range k8sNamespaces {
			fmt.Printf("  %s\n", ns)
		}
		return fmt.Errorf("kubernetes deployments not supported in Phase 1")
	}

	iops.logger.Error("No Infrahub deployment found")
	iops.logger.Error("Ensure Infrahub is running via Docker Compose")
	return fmt.Errorf("no infrahub deployment found")
}

// Backup operations
func (iops *InfrahubOps) generateBackupFilename() string {
	timestamp := time.Now().Format("20060102_150405")
	return fmt.Sprintf("infrahub_backup_%s.tar.gz", timestamp)
}

func (iops *InfrahubOps) createBackupMetadata(backupID string) *BackupMetadata {
	return &BackupMetadata{
		MetadataVersion: 1,
		BackupID:        backupID,
		CreatedAt:       time.Now().UTC().Format(time.RFC3339),
		ToolVersion:     "1.0.0",
		InfrahubVersion: iops.getInfrahubVersion(),
		Components:      []string{"database", "task-manager-db", "artifacts"},
	}
}

func (iops *InfrahubOps) getInfrahubVersion() string {
	executor := NewCommandExecutor(iops.logger)

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
	executor := NewCommandExecutor(iops.logger)
	cmd := []string{"compose"}

	if iops.config.DockerComposeProject != "" {
		cmd = append(cmd, "-p", iops.config.DockerComposeProject)
	}

	cmd = append(cmd, args...)
	return executor.runCommand("docker", cmd...)
}

func (iops *InfrahubOps) stopAppContainers() error {
	iops.logger.Info("Stopping Infrahub application containers...")

	containers := []string{
		"infrahub-server", "task-worker", "task-manager",
		"task-manager-background-svc", "cache", "message-queue",
	}

	for _, container := range containers {
		psOutput, err := iops.composeExec("ps", container)
		if err != nil {
			continue
		}

		if strings.Contains(psOutput, "Up") {
			iops.logger.Info("Stopping %s...", container)
			if _, err := iops.composeExec("stop", container); err != nil {
				return fmt.Errorf("failed to stop %s: %w", container, err)
			}
		}
	}

	iops.logger.Success("Application containers stopped")
	return nil
}

func (iops *InfrahubOps) backupDatabase(backupDir string) error {
	iops.logger.Info("Backing up Neo4j database...")

	// Create backup directory in container
	if _, err := iops.composeExec("exec", "-T", "database", "mkdir", "/tmp/infrahubops"); err != nil {
		return fmt.Errorf("failed to create backup directory: %w", err)
	}

	// Create backup using neo4j-admin
	if _, err := iops.composeExec("exec", "-T", "database", "neo4j-admin", "database", "backup", "--to-path=/tmp/infrahubops", "neo4j"); err != nil {
		return fmt.Errorf("failed to backup neo4j: %w", err)
	}

	// Copy backup
	if _, err := iops.composeExec("cp", "database:/tmp/infrahubops", filepath.Join(backupDir, "database")); err != nil {
		return fmt.Errorf("failed to copy database backup: %w", err)
	}

	// Cleanup container backup
	if _, err := iops.composeExec("exec", "-T", "database", "rm", "-rf", "/tmp/infrahubops"); err != nil {
		iops.logger.Warning("Failed to cleanup container backup directory")
	}

	iops.logger.Success("Neo4j backup completed")
	return nil
}

func (iops *InfrahubOps) backupTaskManagerDB(backupDir string) error {
	iops.logger.Info("Backing up PostgreSQL database...")

	// Create dump
	if _, err := iops.composeExec("exec", "-T", "task-manager-db", "pg_dump", "-Fc", "-U", "postgres", "-d", "prefect", "-f", "/tmp/infrahubops_prefect.dump"); err != nil {
		return fmt.Errorf("failed to create postgresql dump: %w", err)
	}

	// Copy dump
	if _, err := iops.composeExec("cp", "task-manager-db:/tmp/infrahubops_prefect.dump", filepath.Join(backupDir, "prefect.dump")); err != nil {
		return fmt.Errorf("failed to copy postgresql dump: %w", err)
	}

	// Cleanup container dump
	if _, err := iops.composeExec("exec", "-T", "task-manager-db", "rm", "/tmp/infrahubops_prefect.dump"); err != nil {
		iops.logger.Warning("Failed to cleanup container dump file")
	}

	iops.logger.Success("PostgreSQL backup completed")
	return nil
}

func (iops *InfrahubOps) createBackup() error {
	if err := iops.checkPrerequisites(); err != nil {
		return err
	}

	if err := iops.detectEnvironment(); err != nil {
		return err
	}

	backupFilename := iops.generateBackupFilename()
	backupPath := filepath.Join(iops.config.BackupDir, backupFilename)
	workDir, err := os.MkdirTemp("", "infrahub_backup_*")
	if err != nil {
		return fmt.Errorf("failed to create temp directory: %w", err)
	}
	defer os.RemoveAll(workDir)

	iops.logger.Info("Creating backup: %s", backupFilename)

	// Create backup directory structure
	backupDir := filepath.Join(workDir, "backup")
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		return fmt.Errorf("failed to create backup directory: %w", err)
	}

	if err := os.MkdirAll(iops.config.BackupDir, 0755); err != nil {
		return fmt.Errorf("failed to create backup parent directory: %w", err)
	}

	// Create metadata
	backupID := strings.TrimSuffix(backupFilename, ".tar.gz")
	metadata := iops.createBackupMetadata(backupID)

	metadataBytes, err := json.MarshalIndent(metadata, "", "    ")
	if err != nil {
		return fmt.Errorf("failed to marshal metadata: %w", err)
	}

	if err := os.WriteFile(filepath.Join(backupDir, "backup_information.json"), metadataBytes, 0644); err != nil {
		return fmt.Errorf("failed to write metadata: %w", err)
	}

	// Backup databases
	if err := iops.backupDatabase(backupDir); err != nil {
		return err
	}

	if err := iops.backupTaskManagerDB(backupDir); err != nil {
		return err
	}

	// TODO: Backup artifact store
	iops.logger.Info("Artifact store backup will be added in future versions")

	// Create tarball
	iops.logger.Info("Creating backup archive...")
	if err := iops.createTarball(backupPath, workDir, "backup/"); err != nil {
		return fmt.Errorf("failed to create archive: %w", err)
	}

	iops.logger.Success("Backup created: %s", backupPath)

	// Show backup size
	if stat, err := os.Stat(backupPath); err == nil {
		iops.logger.Info("Backup size: %s", formatBytes(stat.Size()))
	}

	return nil
}

func (iops *InfrahubOps) createTarball(filename, sourceDir, pathInTar string) error {
	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	gw := gzip.NewWriter(file)
	defer gw.Close()

	tw := tar.NewWriter(gw)
	defer tw.Close()

	return filepath.Walk(filepath.Join(sourceDir, pathInTar), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(sourceDir, path)
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(relPath)

		if err := tw.WriteHeader(header); err != nil {
			return err
		}

		if info.IsDir() {
			return nil
		}

		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()

		_, err = io.Copy(tw, file)
		return err
	})
}

// Restore operations
func (iops *InfrahubOps) extractTarball(filename, destDir string) error {
	file, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	gr, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer gr.Close()

	tr := tar.NewReader(gr)

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		target := filepath.Join(destDir, header.Name)

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}

			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY, os.FileMode(header.Mode))
			if err != nil {
				return err
			}

			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
		}
	}

	return nil
}

func (iops *InfrahubOps) wipeTransientData() error {
	iops.logger.Info("Wiping cache and message queue data...")

	if _, err := iops.composeExec("exec", "-T", "message-queue", "find", "/var/lib/rabbitmq", "-mindepth", "1", "-delete"); err != nil {
		iops.logger.Warning("Failed to wipe message queue data: %v", err)
	}

	if _, err := iops.composeExec("exec", "-T", "cache", "find", "/data", "-mindepth", "1", "-delete"); err != nil {
		iops.logger.Warning("Failed to wipe cache data: %v", err)
	}

	iops.logger.Success("Transient data wiped")
	return nil
}

func (iops *InfrahubOps) restoreBackup(backupFile string) error {
	if _, err := os.Stat(backupFile); os.IsNotExist(err) {
		return fmt.Errorf("backup file not found: %s", backupFile)
	}

	if err := iops.checkPrerequisites(); err != nil {
		return err
	}

	if err := iops.detectEnvironment(); err != nil {
		return err
	}

	workDir, err := os.MkdirTemp("", "infrahub_restore_*")
	if err != nil {
		return fmt.Errorf("failed to create temp directory: %w", err)
	}
	defer os.RemoveAll(workDir)

	iops.logger.Info("Restoring from backup: %s", backupFile)

	// Extract backup
	iops.logger.Info("Extracting backup archive...")
	if err := iops.extractTarball(backupFile, workDir); err != nil {
		return fmt.Errorf("failed to extract backup: %w", err)
	}

	// Validate backup
	metadataPath := filepath.Join(workDir, "backup", "backup_information.json")
	if _, err := os.Stat(metadataPath); os.IsNotExist(err) {
		return fmt.Errorf("invalid backup file: missing metadata")
	}

	// Show backup info
	metadataBytes, err := os.ReadFile(metadataPath)
	if err != nil {
		return fmt.Errorf("failed to read metadata: %w", err)
	}

	iops.logger.Info("Backup metadata:")
	fmt.Println(string(metadataBytes))

	// Wipe transient data
	iops.wipeTransientData()

	// Stop application containers
	if err := iops.stopAppContainers(); err != nil {
		return err
	}

	// Restore PostgreSQL
	if err := iops.restorePostgreSQL(workDir); err != nil {
		return err
	}

	// Restart dependencies
	if err := iops.restartDependencies(); err != nil {
		return err
	}

	// Restore Neo4j
	if err := iops.restoreNeo4j(workDir); err != nil {
		return err
	}

	// Restart all services
	iops.logger.Info("Restarting Infrahub services...")
	if _, err := iops.composeExec("start", "infrahub-server", "task-worker"); err != nil {
		return fmt.Errorf("failed to restart infrahub services: %w", err)
	}

	iops.logger.Success("Restore completed successfully")
	iops.logger.Info("Infrahub should be available shortly")

	return nil
}

func (iops *InfrahubOps) restorePostgreSQL(workDir string) error {
	iops.logger.Info("Restoring PostgreSQL database...")

	// Start task-manager-db
	if _, err := iops.composeExec("start", "task-manager-db"); err != nil {
		return fmt.Errorf("failed to start task-manager-db: %w", err)
	}

	// Copy dump to container
	dumpPath := filepath.Join(workDir, "backup", "prefect.dump")
	if _, err := iops.composeExec("cp", dumpPath, "task-manager-db:/tmp/infrahubops_prefect.dump"); err != nil {
		return fmt.Errorf("failed to copy dump to container: %w", err)
	}

	// Restore database
	if _, err := iops.composeExec("exec", "-T", "task-manager-db", "pg_restore", "-d", "postgres", "-U", "postgres", "--clean", "--create", "/tmp/infrahubops_prefect.dump"); err != nil {
		return fmt.Errorf("failed to restore postgresql: %w", err)
	}

	// Cleanup
	if _, err := iops.composeExec("exec", "-T", "task-manager-db", "rm", "/tmp/infrahubops_prefect.dump"); err != nil {
		iops.logger.Warning("Failed to cleanup dump file")
	}

	return nil
}

func (iops *InfrahubOps) restartDependencies() error {
	iops.logger.Info("Restarting cache and message-queue")
	if _, err := iops.composeExec("start", "cache", "message-queue"); err != nil {
		return fmt.Errorf("failed to restart cache and message-queue: %w", err)
	}

	iops.logger.Info("Restarting task manager...")
	if _, err := iops.composeExec("start", "task-manager", "task-manager-background-svc"); err != nil {
		return fmt.Errorf("failed to restart task manager: %w", err)
	}

	return nil
}

func (iops *InfrahubOps) restoreNeo4j(workDir string) error {
	iops.logger.Info("Restoring Neo4j database...")

	// Start database
	if _, err := iops.composeExec("start", "database"); err != nil {
		return fmt.Errorf("failed to start database: %w", err)
	}

	// Copy backup to container
	backupPath := filepath.Join(workDir, "backup", "database")
	if _, err := iops.composeExec("cp", backupPath, "database:/tmp/infrahubops"); err != nil {
		return fmt.Errorf("failed to copy backup to container: %w", err)
	}

	// Change ownership
	if _, err := iops.composeExec("exec", "-T", "database", "chown", "-R", "neo4j:neo4j", "/tmp/infrahubops"); err != nil {
		return fmt.Errorf("failed to change backup ownership: %w", err)
	}

	// Stop neo4j database
	if _, err := iops.composeExec("exec", "-T", "database", "cypher-shell", "-u", "neo4j", "-padmin", "-d", "system", "stop database neo4j"); err != nil {
		return fmt.Errorf("failed to stop neo4j database: %w", err)
	}

	// Restore database
	if _, err := iops.composeExec("exec", "-T", "-u", "neo4j", "database", "neo4j-admin", "database", "restore", "--overwrite-destination=true", "--from-path=/tmp/infrahubops", "neo4j"); err != nil {
		return fmt.Errorf("failed to restore neo4j: %w", err)
	}

	// Start neo4j database
	if _, err := iops.composeExec("exec", "-T", "database", "cypher-shell", "-u", "neo4j", "-padmin", "-d", "system", "start database neo4j"); err != nil {
		return fmt.Errorf("failed to start neo4j database: %w", err)
	}

	// Cleanup
	if _, err := iops.composeExec("exec", "-T", "database", "rm", "-rf", "/tmp/infrahubops"); err != nil {
		iops.logger.Warning("Failed to cleanup backup directory")
	}

	return nil
}

// Utility functions
func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getCurrentDir() string {
	dir, err := os.Getwd()
	if err != nil {
		return "."
	}
	return dir
}

func formatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

// CLI Commands
func createRootCommand(app *InfrahubOps) *cobra.Command {
	rootCmd := &cobra.Command{
		Use:     "infrahub-ops",
		Aliases: []string{"infrahubops"},
		Short:   "Infrahub Operations Tool - Phase 1 MVP",
		Long: `Infrahub Operations Tool - Phase 1 MVP

This tool provides backup and restore operations for Infrahub infrastructure.

PHASE 1 SCOPE:
- Docker Compose deployments only
- Neo4j and PostgreSQL backup/restore
- Automatic environment detection
- Single tarball backup format
- Orchestrated restore process`,
	}

	rootCmd.PersistentFlags().StringVar(&app.config.DockerComposeProject, "project", "", "Target specific Docker Compose project")
	rootCmd.PersistentFlags().StringVar(&app.config.BackupDir, "backup-dir", app.config.BackupDir, "Backup directory")
	rootCmd.PersistentFlags().StringVar(&app.config.InfrahubImage, "image", app.config.InfrahubImage, "Infrahub image to use")

	viper.BindPFlag("project", rootCmd.PersistentFlags().Lookup("project"))
	viper.BindPFlag("backup-dir", rootCmd.PersistentFlags().Lookup("backup-dir"))
	viper.BindPFlag("image", rootCmd.PersistentFlags().Lookup("image"))

	return rootCmd
}

func createBackupCommand(app *InfrahubOps) *cobra.Command {
	backupCmd := &cobra.Command{
		Use:   "backup",
		Short: "Backup operations for Infrahub",
		Long:  "Create and restore backups of Infrahub infrastructure components",
	}

	// Create backup subcommand
	createCmd := &cobra.Command{
		Use:   "create",
		Short: "Create a backup of Infrahub instance",
		Long:  "Create a complete backup of the Infrahub instance including databases and configurations",
		RunE: func(cmd *cobra.Command, args []string) error {
			return app.createBackup()
		},
	}

	// Restore backup subcommand
	restoreCmd := &cobra.Command{
		Use:   "restore <backup-file>",
		Short: "Restore Infrahub from backup file",
		Long:  "Restore Infrahub infrastructure from a previously created backup file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return app.restoreBackup(args[0])
		},
	}

	backupCmd.AddCommand(createCmd)
	backupCmd.AddCommand(restoreCmd)

	return backupCmd
}

func createEnvironmentCommand(app *InfrahubOps) *cobra.Command {
	envCmd := &cobra.Command{
		Use:   "environment",
		Short: "Environment detection and management",
		Long:  "Detect and manage Infrahub deployment environments",
	}

	// Detect environment subcommand
	detectCmd := &cobra.Command{
		Use:   "detect",
		Short: "Detect deployment environment (Docker/k8s)",
		Long:  "Automatically detect the current Infrahub deployment environment",
		RunE: func(cmd *cobra.Command, args []string) error {
			return app.detectEnvironment()
		},
	}

	// List environments subcommand
	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List available Infrahub projects",
		Long:  "List all available Infrahub projects in the current environment",
		RunE: func(cmd *cobra.Command, args []string) error {
			projects, err := app.detectDockerProjects()
			if err != nil {
				return err
			}

			if len(projects) > 0 {
				app.logger.Info("Available Infrahub projects:")
				for _, project := range projects {
					fmt.Printf("  %s\n", project)
				}
			} else {
				app.logger.Info("No Infrahub projects found")
			}

			return nil
		},
	}

	envCmd.AddCommand(detectCmd)
	envCmd.AddCommand(listCmd)

	return envCmd
}

func (iops *InfrahubOps) executeScript(targetService string, scriptContent string, targetPath string, args ...string) (string, error) {
	// Write embedded script to a temporary file
	tmpFile, err := os.CreateTemp("", "infrahubops_clean_old_tasks_*.py")
	if err != nil {
		return "", fmt.Errorf("failed to create temp file: %w", err)
	}
	defer os.Remove(tmpFile.Name())
	if _, err := tmpFile.WriteString(scriptContent); err != nil {
		tmpFile.Close()
		return "", fmt.Errorf("failed to write script: %w", err)
	}
	tmpFile.Close()

	if _, err := iops.composeExec("cp", "-a", tmpFile.Name(), fmt.Sprintf("%s:%s", targetService, targetPath)); err != nil {
		return "", fmt.Errorf("failed to copy script to container: %w", err)
	}

	// Execute script inside container
	iops.logger.Info("Executing cleanup script inside task-worker container...")

	var output string
	if output, err = iops.composeExec(append([]string{"exec", "-T", targetService}, args...)...); err != nil {
		return "", fmt.Errorf("failed to execute cleanup script: %w %v", err, output)
	}

	// (Best effort) cleanup script inside container
	_, _ = iops.composeExec("exec", "-T", targetService, "rm", "-f", targetPath)

	return output, nil
}

// Task Manager (Prefect) maintenance
func (iops *InfrahubOps) flushFlowRuns(daysToKeep, batchSize int) error {
	if err := iops.checkPrerequisites(); err != nil {
		return err
	}
	if err := iops.detectEnvironment(); err != nil {
		return err
	}

	if daysToKeep < 0 {
		daysToKeep = 30
	}
	if batchSize <= 0 {
		batchSize = 200
	}

	iops.logger.Info("Flushing Prefect flow runs older than %d days (batch size %d)...", daysToKeep, batchSize)

	var output string
	var err error
	if output, err = iops.executeScript("task-worker", cleanOldTasksScript, "/tmp/infrahubops_clean_old_tasks.py", "python", "/tmp/infrahubops_clean_old_tasks.py", strconv.Itoa(daysToKeep), strconv.Itoa(batchSize)); err != nil {
		return err
	}

	iops.logger.Info(output)
	iops.logger.Success("Flow runs cleanup completed:")

	return nil
}

func (iops *InfrahubOps) flushStaleRuns(daysToKeep, batchSize int) error {
	if err := iops.checkPrerequisites(); err != nil {
		return err
	}
	if err := iops.detectEnvironment(); err != nil {
		return err
	}

	if daysToKeep < 0 {
		daysToKeep = 2
	}
	if batchSize <= 0 {
		batchSize = 200
	}

	iops.logger.Info("Flushing Prefect flow runs older than %d days (batch size %d)...", daysToKeep, batchSize)

	var output string
	var err error
	if output, err = iops.executeScript("task-worker", cleanStaleTasksScript, "/tmp/infrahubops_clean_stale_tasks.py", "python", "/tmp/infrahubops_clean_stale_tasks.py", strconv.Itoa(daysToKeep), strconv.Itoa(batchSize)); err != nil {
		return err
	}

	iops.logger.Info(output)
	iops.logger.Success("Stale flow runs cleanup completed:")

	return nil
}

func createTaskManagerCommand(app *InfrahubOps) *cobra.Command {
	taskManagerCmd := &cobra.Command{
		Use:   "taskmanager",
		Short: "Task manager (Prefect) maintenance operations",
		Long:  "Maintenance operations for the task manager (Prefect) such as flushing old flow runs.",
	}

	flushCmd := &cobra.Command{
		Use:   "flush",
		Short: "Flush / cleanup operations",
		Long:  "Cleanup operations for Prefect resources.",
	}

	flowRunsCmd := &cobra.Command{
		Use:   "flow-runs [days_to_keep] [batch_size]",
		Short: "Delete completed/failed/cancelled flow runs older than the retention period",
		Args:  cobra.RangeArgs(0, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			days := 30
			batch := 200
			var err error
			if len(args) >= 1 {
				days, err = strconv.Atoi(args[0])
				if err != nil {
					return fmt.Errorf("invalid days_to_keep value: %v", err)
				}
			}
			if len(args) == 2 {
				batch, err = strconv.Atoi(args[1])
				if err != nil {
					return fmt.Errorf("invalid batch_size value: %v", err)
				}
			}
			return app.flushFlowRuns(days, batch)
		},
		Example: `# Use defaults (30 days retention, batch size 200)
infrahubops taskmanager flush flow-runs

# Keep last 45 days
infrahubops taskmanager flush flow-runs 45

# Keep last 60 days with batch size 500
infrahubops taskmanager flush flow-runs 60 500`,
	}

	staleRunsCmd := &cobra.Command{
		Use:   "stale-runs [days_to_keep] [batch_size]",
		Short: "Cancel flow runs still RUNNING and older than the retention period",
		Args:  cobra.RangeArgs(0, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			days := 2
			batch := 200
			var err error
			if len(args) >= 1 {
				days, err = strconv.Atoi(args[0])
				if err != nil {
					return fmt.Errorf("invalid days_to_keep value: %v", err)
				}
			}
			if len(args) == 2 {
				batch, err = strconv.Atoi(args[1])
				if err != nil {
					return fmt.Errorf("invalid batch_size value: %v", err)
				}
			}
			return app.flushStaleRuns(days, batch)
		},
		Example: `# Use defaults (2 days retention, batch size 200)
infrahubops taskmanager flush stale-runs

# Keep last 45 days
infrahubops taskmanager flush stale-runs 45

# Keep last 60 days with batch size 500
infrahubops taskmanager flush stale-runs 60 500`,
	}

	flushCmd.AddCommand(flowRunsCmd)
	flushCmd.AddCommand(staleRunsCmd)
	taskManagerCmd.AddCommand(flushCmd)
	return taskManagerCmd
}

func main() {
	app := NewInfrahubOps()
	rootCmd := createRootCommand(app)

	// Add subcommands
	rootCmd.AddCommand(createBackupCommand(app))
	rootCmd.AddCommand(createEnvironmentCommand(app))
	rootCmd.AddCommand(createTaskManagerCommand(app))

	// Set up configuration
	cobra.OnInitialize(func() {
		viper.AutomaticEnv()
		viper.SetEnvPrefix("INFRAHUB")

		// Update config from viper
		if viper.IsSet("project") {
			app.config.DockerComposeProject = viper.GetString("project")
		}
		if viper.IsSet("backup-dir") {
			app.config.BackupDir = viper.GetString("backup-dir")
		}
		if viper.IsSet("image") {
			app.config.InfrahubImage = viper.GetString("image")
		}
	})

	// Execute the root command
	if err := rootCmd.Execute(); err != nil {
		app.logger.Error("Command failed: %v", err)
		os.Exit(1)
	}
}
