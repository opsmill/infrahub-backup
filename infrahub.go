package main

import (
	"bufio"
	"embed"
	"encoding/json"

	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

const metadataVersion = 2025092500

// BackupMetadata represents the backup metadata structure
type BackupMetadata struct {
	MetadataVersion int               `json:"metadata_version"`
	BackupID        string            `json:"backup_id"`
	CreatedAt       string            `json:"created_at"`
	ToolVersion     string            `json:"tool_version"`
	InfrahubVersion string            `json:"infrahub_version"`
	Components      []string          `json:"components"`
	Checksums       map[string]string `json:"checksums,omitempty"`
}

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

// Backup operations
func (iops *InfrahubOps) generateBackupFilename() string {
	timestamp := time.Now().Format("20060102_150405")
	return fmt.Sprintf("infrahub_backup_%s.tar.gz", timestamp)
}

func (iops *InfrahubOps) createBackupMetadata(backupID string) *BackupMetadata {
	return &BackupMetadata{
		MetadataVersion: metadataVersion,
		BackupID:        backupID,
		CreatedAt:       time.Now().UTC().Format(time.RFC3339),
		ToolVersion:     BuildRevision(),
		InfrahubVersion: iops.getInfrahubVersion(),
		Components:      []string{"database", "task-manager-db", "artifacts"},
	}
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

func (iops *InfrahubOps) stopAppContainers() error {
	logrus.Info("Stopping Infrahub application containers...")

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
			logrus.Infof("Stopping %s...", container)
			if _, err := iops.composeExec("stop", container); err != nil {
				return fmt.Errorf("failed to stop %s: %w", container, err)
			}
		}
	}

	logrus.Info("Application containers stopped")
	return nil
}

func (iops *InfrahubOps) backupDatabase(backupDir string, backupMetadata string) error {
	logrus.Info("Backing up Neo4j database...")

	// Create backup directory in container
	if _, err := iops.composeExec("exec", "-T", "database", "mkdir", "/tmp/infrahubops"); err != nil {
		return fmt.Errorf("failed to create backup directory: %w", err)
	}
	defer iops.composeExec("exec", "-T", "database", "rm", "-rf", "/tmp/infrahubops")

	// Create backup using neo4j-admin
	if output, err := iops.composeExec("exec", "-T", "database", "neo4j-admin", "database", "backup", "--include-metadata="+backupMetadata, "--to-path=/tmp/infrahubops", "neo4j"); err != nil {
		return fmt.Errorf("failed to backup neo4j: %w\nOutput: %v", err, output)
	}

	// Copy backup
	if _, err := iops.composeExec("cp", "database:/tmp/infrahubops", filepath.Join(backupDir, "database")); err != nil {
		return fmt.Errorf("failed to copy database backup: %w", err)
	}

	logrus.Info("Neo4j backup completed")
	return nil
}

func (iops *InfrahubOps) backupTaskManagerDB(backupDir string) error {
	logrus.Info("Backing up PostgreSQL database...")

	// Create dump
	if output, err := iops.composeExec("exec", "-T", "task-manager-db", "pg_dump", "-Fc", "-U", "postgres", "-d", "prefect", "-f", "/tmp/infrahubops_prefect.dump"); err != nil {
		return fmt.Errorf("failed to create postgresql dump: %w\nOutput: %v", err, output)
	}
	defer iops.composeExec("exec", "-T", "task-manager-db", "rm", "/tmp/infrahubops_prefect.dump")

	// Copy dump
	if _, err := iops.composeExec("cp", "task-manager-db:/tmp/infrahubops_prefect.dump", filepath.Join(backupDir, "prefect.dump")); err != nil {
		return fmt.Errorf("failed to copy postgresql dump: %w", err)
	}

	logrus.Info("PostgreSQL backup completed")
	return nil
}

type tasksOutput struct {
	Id   string `json:"id"`
	Name string `json:"title"`
}

func (iops *InfrahubOps) createBackup(force bool, neo4jMetadata string) error {
	if err := iops.checkPrerequisites(); err != nil {
		return err
	}

	if err := iops.detectEnvironment(); err != nil {
		return err
	}

	// Check for running tasks unless --force is set
	if !force {
		logrus.Info("Checking for running tasks before backup...")
		var scriptContent string
		// Read the get_running_tasks.py script from embedded scripts
		if b, err := scripts.ReadFile("scripts/get_running_tasks.py"); err == nil {
			scriptContent = string(b)
		} else {
			return fmt.Errorf("could not retrieve get_running_tasks.py: %w", err)
		}
		for {
			var output string
			var err error
			output, err = iops.executeScript("task-worker", scriptContent, "/tmp/get_running_tasks.py", "python", "-u", "/tmp/get_running_tasks.py")
			if err != nil {
				return fmt.Errorf("failed to check running tasks: %w", err)
			}
			var tasks []tasksOutput
			if err = json.Unmarshal([]byte(output), &tasks); err != nil {
				return fmt.Errorf("could not parse json: %w\n%v", err, output)
			}
			if len(tasks) == 0 {
				logrus.Info("No running tasks detected. Proceeding with backup.")
				break
			} else {
				logrus.Warnf("There are running %v tasks: %v", len(tasks), tasks)
				logrus.Warnf("Waiting for them to complete... (use --force to override)")
				time.Sleep(5 * time.Second)
			}
		}
	}

	backupFilename := iops.generateBackupFilename()
	backupPath := filepath.Join(iops.config.BackupDir, backupFilename)
	workDir, err := os.MkdirTemp("", "infrahub_backup_*")
	if err != nil {
		return fmt.Errorf("failed to create temp directory: %w", err)
	}
	defer os.RemoveAll(workDir)

	logrus.Infof("Creating backup: %s", backupFilename)

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

	// Backup databases
	if err := iops.backupDatabase(backupDir, neo4jMetadata); err != nil {
		return err
	}

	if err := iops.backupTaskManagerDB(backupDir); err != nil {
		return err
	}

	// Calculate checksums for backup files
	checksums := make(map[string]string)
	neo4jDir := filepath.Join(backupDir, "database")
	prefectPath := filepath.Join(backupDir, "prefect.dump")

	// Calculate checksum for each file in Neo4j backup directory
	err = filepath.Walk(neo4jDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			rel, _ := filepath.Rel(backupDir, path)
			if sum, err := calculateSHA256(path); err == nil {
				checksums[rel] = sum
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to calculate Neo4j backup checksums: %w", err)
	}

	// Calculate checksum for Prefect DB dump
	if sum, err := calculateSHA256(prefectPath); err == nil {
		checksums["prefect.dump"] = sum
	}

	metadata.Checksums = checksums

	metadataBytes, err := json.MarshalIndent(metadata, "", "    ")
	if err != nil {
		return fmt.Errorf("failed to marshal metadata: %w", err)
	}

	if err := os.WriteFile(filepath.Join(backupDir, "backup_information.json"), metadataBytes, 0644); err != nil {
		return fmt.Errorf("failed to write metadata: %w", err)
	}

	// TODO: Backup artifact store
	logrus.Info("Artifact store backup will be added in future versions")

	// Create tarball
	logrus.Info("Creating backup archive...")
	if err := createTarball(backupPath, workDir, "backup/"); err != nil {
		return fmt.Errorf("failed to create archive: %w", err)
	}

	logrus.Infof("Backup created: %s", backupPath)

	// Show backup size
	if stat, err := os.Stat(backupPath); err == nil {
		logrus.Infof("Backup size: %s", formatBytes(stat.Size()))
	}

	return nil
}

// Restore operations

func (iops *InfrahubOps) wipeTransientData() error {
	logrus.Info("Wiping cache and message queue data...")

	if _, err := iops.composeExec("exec", "-T", "message-queue", "find", "/var/lib/rabbitmq", "-mindepth", "1", "-delete"); err != nil {
		logrus.Warnf("Failed to wipe message queue data: %v", err)
	}
	if _, err := iops.composeExec("exec", "-T", "cache", "find", "/data", "-mindepth", "1", "-delete"); err != nil {
		logrus.Warnf("Failed to wipe cache data: %v", err)
	}
	logrus.Info("Transient data wiped")
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

	logrus.Infof("Restoring from backup: %s", backupFile)

	// Extract backup
	logrus.Info("Extracting backup archive...")
	if err := extractTarball(backupFile, workDir); err != nil {
		return fmt.Errorf("failed to extract backup: %w", err)
	}

	// Validate backup
	metadataPath := filepath.Join(workDir, "backup", "backup_information.json")
	if _, err := os.Stat(metadataPath); os.IsNotExist(err) {
		return fmt.Errorf("invalid backup file: missing metadata")
	}

	// Read and parse backup info
	metadataBytes, err := os.ReadFile(metadataPath)
	if err != nil {
		return fmt.Errorf("failed to read metadata: %w", err)
	}
	var metadata BackupMetadata
	if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
		return fmt.Errorf("failed to parse metadata: %w", err)
	}

	logrus.Info("Backup metadata:")
	fmt.Println(string(metadataBytes))

	// Validate checksums for Neo4j backup files
	for relPath, expectedSum := range metadata.Checksums {
		if relPath == "prefect.dump" {
			continue
		}
		filePath := filepath.Join(workDir, "backup", relPath)
		if _, err := os.Stat(filePath); err != nil {
			return fmt.Errorf("missing backup file: %s", relPath)
		}
		sum, err := calculateSHA256(filePath)
		if err != nil {
			return fmt.Errorf("failed to calculate checksum for %s: %w", relPath, err)
		}
		if sum != expectedSum {
			return fmt.Errorf("checksum mismatch for %s: expected %s, got %s", relPath, expectedSum, sum)
		}
	}

	// Validate checksum for Prefect DB dump
	prefectPath := filepath.Join(workDir, "backup", "prefect.dump")
	if expectedSum, ok := metadata.Checksums["prefect.dump"]; ok {
		if _, err := os.Stat(prefectPath); err != nil {
			return fmt.Errorf("missing backup file: prefect.dump")
		}
		sum, err := calculateSHA256(prefectPath)
		if err != nil {
			return fmt.Errorf("failed to calculate checksum for prefect.dump: %w", err)
		}
		if sum != expectedSum {
			return fmt.Errorf("checksum mismatch for prefect.dump: expected %s, got %s", expectedSum, sum)
		}
	}

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
	logrus.Info("Restarting Infrahub services...")
	if _, err := iops.composeExec("start", "infrahub-server", "task-worker"); err != nil {
		return fmt.Errorf("failed to restart infrahub services: %w", err)
	}

	logrus.Info("Restore completed successfully")
	logrus.Info("Infrahub should be available shortly")

	return nil
}

func (iops *InfrahubOps) restorePostgreSQL(workDir string) error {
	logrus.Info("Restoring PostgreSQL database...")

	// Start task-manager-db
	if _, err := iops.composeExec("start", "task-manager-db"); err != nil {
		return fmt.Errorf("failed to start task-manager-db: %w", err)
	}

	// Copy dump to container
	dumpPath := filepath.Join(workDir, "backup", "prefect.dump")
	if _, err := iops.composeExec("cp", dumpPath, "task-manager-db:/tmp/infrahubops_prefect.dump"); err != nil {
		return fmt.Errorf("failed to copy dump to container: %w", err)
	}
	defer iops.composeExec("exec", "-T", "task-manager-db", "rm", "/tmp/infrahubops_prefect.dump")

	// Restore database
	if output, err := iops.composeExec("exec", "-T", "task-manager-db", "pg_restore", "-d", "postgres", "-U", "postgres", "--clean", "--create", "/tmp/infrahubops_prefect.dump"); err != nil {
		return fmt.Errorf("failed to restore postgresql: %w\nOutput: %v", err, output)
	}

	return nil
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

func (iops *InfrahubOps) restoreNeo4j(workDir string) error {
	logrus.Info("Restoring Neo4j database...")

	// Copy backup to container
	backupPath := filepath.Join(workDir, "backup", "database")
	if _, err := iops.composeExec("cp", backupPath, "database:/tmp/infrahubops"); err != nil {
		return fmt.Errorf("failed to copy backup to container: %w", err)
	}
	defer iops.composeExec("exec", "-T", "database", "rm", "-rf", "/tmp/infrahubops")

	// Change ownership
	if _, err := iops.composeExec("exec", "-T", "database", "chown", "-R", "neo4j:neo4j", "/tmp/infrahubops"); err != nil {
		return fmt.Errorf("failed to change backup ownership: %w", err)
	}

	// Stop neo4j database
	if _, err := iops.composeExec("exec", "-T", "database", "cypher-shell", "-u", "neo4j", "-padmin", "-d", "system", "stop database neo4j"); err != nil {
		return fmt.Errorf("failed to stop neo4j database: %w", err)
	}

	// Restore database
	if output, err := iops.composeExec("exec", "-T", "-u", "neo4j", "database", "neo4j-admin", "database", "restore", "--overwrite-destination=true", "--from-path=/tmp/infrahubops", "neo4j"); err != nil {
		return fmt.Errorf("failed to restore neo4j: %w\nOutput: %v", err, output)
	}

	// Restore metadata
	if output, err := iops.composeExec("exec", "-T", "-u", "neo4j", "database", "sh", "-c", "cat /data/scripts/neo4j/restore_metadata.cypher | cypher-shell -u neo4j -padmin -d system --param \"database => 'databasename'\""); err != nil {
		return fmt.Errorf("failed to restore neo4j metadata: %w\nOutput: %v", err, output)
	}

	// Start neo4j database
	if _, err := iops.composeExec("exec", "-T", "database", "cypher-shell", "-u", "neo4j", "-padmin", "-d", "system", "start database neo4j"); err != nil {
		return fmt.Errorf("failed to start neo4j database: %w", err)
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

	logrus.Infof("Flushing Prefect flow runs older than %d days (batch size %d)...", daysToKeep, batchSize)

	var scriptContent []byte
	var err error
	if scriptContent, err = scripts.ReadFile("scripts/clean_old_tasks.py"); err != nil {
		return fmt.Errorf("could not retrieve script: %w", err)
	}

	if _, err := iops.executeScript("task-worker", string(scriptContent), "/tmp/infrahubops_clean_old_tasks.py", "python", "-u", "/tmp/infrahubops_clean_old_tasks.py", strconv.Itoa(daysToKeep), strconv.Itoa(batchSize)); err != nil {
		return err
	}

	logrus.Info("Flow runs cleanup completed:")

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

	logrus.Infof("Flushing Prefect flow runs older than %d days (batch size %d)...", daysToKeep, batchSize)

	var scriptContent []byte
	var err error
	if scriptContent, err = scripts.ReadFile("scripts/clean_stale_tasks.py"); err != nil {
		return fmt.Errorf("could not retrieve script: %w", err)
	}

	if _, err := iops.executeScript("task-worker", string(scriptContent), "/tmp/infrahubops_clean_stale_tasks.py", "python", "-u", "/tmp/infrahubops_clean_stale_tasks.py", strconv.Itoa(daysToKeep), strconv.Itoa(batchSize)); err != nil {
		return err
	}

	logrus.Info("Stale flow runs cleanup completed:")

	return nil
}
