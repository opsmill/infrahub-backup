package main

import (
	"os"

	app "infrahub-ops/src/internal/app"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

func main() {
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

	createCmd := &cobra.Command{
		Use:          "create",
		Short:        "Create a backup of the current Infrahub instance",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return iops.CreateBackup(force, neo4jMetadata)
		},
	}
	createCmd.Flags().BoolVar(&force, "force", false, "Force backup creation even if there are running tasks")
	createCmd.Flags().StringVar(&neo4jMetadata, "neo4jmetadata", "all", "Whether to backup neo4j metadata or not (all, none, users, roles)")

	restoreCmd := &cobra.Command{
		Use:          "restore <backup-file>",
		Short:        "Restore Infrahub from a backup archive",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return iops.RestoreBackup(args[0])
		},
	}

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
