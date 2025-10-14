package main

import (
	"os"
	"strconv"

	app "infrahub-ops/src/internal/app"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

func main() {
	iops := app.NewInfrahubOps()
	rootCmd := &cobra.Command{
		Use:   "infrahub-taskmanager",
		Short: "Task manager (Prefect) maintenance operations",
		Long:  "Maintenance operations for the task manager (Prefect) such as flushing old flow runs.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}

	app.ConfigureRootCommand(rootCmd, iops)
	app.AttachEnvironmentCommands(rootCmd, iops)

	flushCmd := &cobra.Command{
		Use:   "flush",
		Short: "Flush / cleanup operations",
		Long:  "Cleanup operations for Prefect resources.",
	}

	flowRunsCmd := &cobra.Command{
		Use:          "flow-runs [days_to_keep] [batch_size]",
		Short:        "Delete completed/failed/cancelled flow runs older than the retention period",
		Args:         cobra.RangeArgs(0, 2),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			days := 30
			batch := 200
			var err error
			if len(args) >= 1 {
				days, err = strconv.Atoi(args[0])
				if err != nil {
					return err
				}
			}
			if len(args) == 2 {
				batch, err = strconv.Atoi(args[1])
				if err != nil {
					return err
				}
			}
			return iops.FlushFlowRuns(days, batch)
		},
	}

	staleRunsCmd := &cobra.Command{
		Use:          "stale-runs [days_to_keep] [batch_size]",
		Short:        "Cancel flow runs still RUNNING and older than the retention period",
		Args:         cobra.RangeArgs(0, 2),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			days := 2
			batch := 200
			var err error
			if len(args) >= 1 {
				days, err = strconv.Atoi(args[0])
				if err != nil {
					return err
				}
			}
			if len(args) == 2 {
				batch, err = strconv.Atoi(args[1])
				if err != nil {
					return err
				}
			}
			return iops.FlushStaleRuns(days, batch)
		},
	}

	flushCmd.AddCommand(flowRunsCmd)
	flushCmd.AddCommand(staleRunsCmd)
	rootCmd.AddCommand(flushCmd)

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
