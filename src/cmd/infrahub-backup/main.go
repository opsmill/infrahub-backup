package main

import (
	"os"
	"time"

	app "infrahub-ops/src/internal/app"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

// version is set via ldflags at build time
var version string

func main() {
	app.SetVersion(version)
	iops := app.NewInfrahubOps()
	rootCmd := &cobra.Command{
		Use:   "infrahub-backup",
		Short: "Create and restore Infrahub backups",
		Long:  "Create and restore backups of Infrahub infrastructure components.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}

	app.ConfigureRootCommand(rootCmd, iops)
	app.AttachEnvironmentCommands(rootCmd, iops)

	var force bool
	var neo4jMetadata string
	var excludeTaskManagerDB bool
	var restoreExcludeTaskManagerDB bool
	var restoreMigrateFormat bool
	var s3Upload bool
	var s3KeepLocal bool
	var sleepDuration time.Duration
	var restoreSleepDuration time.Duration

	// Variables for from-files subcommand
	var neo4jPath string
	var postgresPath string
	var neo4jEdition string
	var infrahubVersion string

	createCmd := &cobra.Command{
		Use:          "create",
		Short:        "Create a backup of the current Infrahub instance",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return iops.CreateBackup(force, neo4jMetadata, excludeTaskManagerDB, s3Upload, s3KeepLocal, sleepDuration)
		},
	}
	createCmd.Flags().BoolVar(&force, "force", false, "Force backup creation even if there are running tasks")
	createCmd.Flags().StringVar(&neo4jMetadata, "neo4jmetadata", "all", "Whether to backup neo4j metadata or not (all, none, users, roles)")
	createCmd.Flags().BoolVar(&excludeTaskManagerDB, "exclude-taskmanager", false, "Exclude task manager database from the backup")
	createCmd.Flags().BoolVar(&s3Upload, "s3-upload", false, "Upload backup to S3 after creation")
	createCmd.Flags().BoolVar(&s3KeepLocal, "s3-keep-local", false, "Keep local backup file after successful S3 upload (default: delete local file)")
	createCmd.Flags().DurationVar(&sleepDuration, "sleep", 0, "Sleep duration after backup creation (e.g., 5m, 300s) for manual file transfer")

	// Undocumented subcommand: create from-files
	fromFilesCmd := &cobra.Command{
		Use:          "from-files",
		Short:        "Create a backup archive from local database dump files",
		Hidden:       true,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return iops.CreateBackupFromFiles(neo4jPath, postgresPath, neo4jEdition, infrahubVersion)
		},
	}
	fromFilesCmd.Flags().StringVar(&neo4jPath, "neo4j-path", "", "Path to Neo4j backup directory or dump file (required)")
	fromFilesCmd.Flags().StringVar(&postgresPath, "postgres-path", "", "Path to PostgreSQL dump file (optional)")
	fromFilesCmd.Flags().StringVar(&neo4jEdition, "neo4j-edition", "", "Neo4j edition (enterprise or community, auto-detected if not specified)")
	fromFilesCmd.Flags().StringVar(&infrahubVersion, "infrahub-version", "", "Infrahub version to record in backup metadata")
	fromFilesCmd.MarkFlagRequired("neo4j-path")

	createCmd.AddCommand(fromFilesCmd)

	restoreCmd := &cobra.Command{
		Use:          "restore <backup-file>",
		Short:        "Restore Infrahub from a backup archive",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return iops.RestoreBackup(args[0], restoreExcludeTaskManagerDB, restoreMigrateFormat, restoreSleepDuration)
		},
	}
	restoreCmd.Flags().BoolVar(&restoreExcludeTaskManagerDB, "exclude-taskmanager", false, "Skip restoring the task manager database even if present in the archive")
	restoreCmd.Flags().BoolVar(&restoreMigrateFormat, "migrate-format", false, "Run neo4j-admin database migrate --to-format=block after the restore completes")
	restoreCmd.Flags().DurationVar(&restoreSleepDuration, "sleep", 0, "Sleep duration before restore begins (e.g., 5m, 300s) for manual file transfer")

	rootCmd.AddCommand(createCmd)
	rootCmd.AddCommand(restoreCmd)

	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "Print Infrahub Ops CLI build information",
		Run: func(cmd *cobra.Command, args []string) {
			logrus.Infof("Version: %s", app.BuildRevision())
		},
	}

	rootCmd.AddCommand(versionCmd)

	if err := rootCmd.Execute(); err != nil {
		logrus.Errorf("Command failed: %v", err)
		os.Exit(1)
	}
}
