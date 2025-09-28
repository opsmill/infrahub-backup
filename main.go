package main

import (
	"fmt"
	"os"
	"strconv"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// CLI Commands
func createRootCommand(app *InfrahubOps) *cobra.Command {
	rootCmd := &cobra.Command{
		Use:     "infrahub-ops",
		Aliases: []string{"infrahubops"},
		Short:   "Infrahub Operations Tool",
		Long: `Infrahub Operations Tool

This tool provides backup and restore operations for Infrahub infrastructure.`,
	}

	rootCmd.PersistentFlags().StringVar(&app.config.DockerComposeProject, "project", "", "Target specific Docker Compose project")
	rootCmd.PersistentFlags().StringVar(&app.config.BackupDir, "backup-dir", app.config.BackupDir, "Backup directory")
	rootCmd.PersistentFlags().String("log-format", "text", "Log output format: text or json (can also set INFRAHUB_LOG_FORMAT)")

	viper.BindPFlag("project", rootCmd.PersistentFlags().Lookup("project"))
	viper.BindPFlag("backup-dir", rootCmd.PersistentFlags().Lookup("backup-dir"))
	viper.BindPFlag("log-format", rootCmd.PersistentFlags().Lookup("log-format"))

	return rootCmd
}

func createBackupCommand(app *InfrahubOps) *cobra.Command {
	backupCmd := &cobra.Command{
		Use:   "backup",
		Short: "Backup operations for Infrahub",
		Long:  "Create and restore backups of Infrahub infrastructure components",
	}

	// Create backup subcommand
	var force bool
	var neo4jMetadata string
	createCmd := &cobra.Command{
		Use:   "create",
		Short: "Create a backup of Infrahub instance",
		Long:  "Create a complete backup of the Infrahub instance including databases and configurations",
		RunE: func(cmd *cobra.Command, args []string) error {
			return app.createBackup(force, neo4jMetadata)
		},
	}
	createCmd.Flags().BoolVar(&force, "force", false, "Force backup creation even if there are running tasks")
	createCmd.Flags().StringVar(&neo4jMetadata, "neo4jmetadata", "all", "Whether to backup neo4j metadata or not (all, none, users, roles)")

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
				logrus.Info("Available Infrahub projects:")
				for _, project := range projects {
					fmt.Printf("  %s\n", project)
				}
			} else {
				logrus.Info("No Infrahub projects found")
			}

			return nil
		},
	}

	envCmd.AddCommand(detectCmd)
	envCmd.AddCommand(listCmd)

	return envCmd
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

	rootCmd.AddCommand(
		&cobra.Command{
			Use:   "version",
			Short: "Print tool version",
			Run: func(cmd *cobra.Command, args []string) {
				logrus.Infof("Version: %v", BuildRevision())
			},
		},
	)

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

		logFormat := viper.GetString("log-format")
		switch logFormat {
		case "json":
			logrus.SetFormatter(&logrus.JSONFormatter{})
		default:
			logrus.SetFormatter(&logrus.TextFormatter{FullTimestamp: true})
		}
	})

	// Execute the root command
	if err := rootCmd.Execute(); err != nil {
		logrus.Errorf("Command failed: %v", err)
		os.Exit(1)
	}
}
