package main

import (
	"fmt"
	"os"

	app "infrahub-ops/src/internal/app"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// version is set via ldflags at build time
var version string

func main() {
	app.SetVersion(version)
	iops := app.NewInfrahubOps()
	rootCmd := &cobra.Command{
		Use:   "infrahub-collect",
		Short: "Collect Infrahub troubleshooting bundles",
		Long:  "Collect troubleshooting bundles (service logs, diagnostics, metrics) from Infrahub deployments running on Docker Compose or Kubernetes.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}

	app.ConfigureRootCommand(rootCmd, iops)
	app.AttachEnvironmentCommands(rootCmd, iops)
	app.AttachUpdateCommand(rootCmd, "infrahub-collect")

	// Collect-specific persistent flag (INFRAHUB_OUTPUT_DIR)
	rootCmd.PersistentFlags().String("output-dir", "./infrahub_bundles", "Directory for bundle files")
	viper.BindPFlag("output-dir", rootCmd.PersistentFlags().Lookup("output-dir"))

	var logLines int
	var includeBackup bool
	var includeQueries bool
	var benchmark bool
	var telemetryDays int

	createCmd := &cobra.Command{
		Use:          "create",
		Short:        "Collect a troubleshooting bundle from the Infrahub instance",
		Long:         "Collect a troubleshooting bundle from the Infrahub instance. Collection is read-only: no container or pod is stopped, restarted, or scaled. Individual collector failures are recorded in the bundle manifest and do not abort the run.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			lines := viper.GetInt("log-lines")
			if lines <= 0 {
				return fmt.Errorf("--log-lines must be a positive integer, got %d", lines)
			}

			opts := app.CollectOptions{
				OutputDir:      viper.GetString("output-dir"),
				LogLines:       lines,
				IncludeBackup:  viper.GetBool("include-backup"),
				IncludeQueries: viper.GetBool("include-queries"),
				Benchmark:      viper.GetBool("benchmark"),
				TelemetryDays:  viper.GetInt("telemetry-days"),
			}

			return iops.CollectBundle(opts)
		},
	}
	createCmd.Flags().IntVar(&logLines, "log-lines", 100000, "Maximum log lines collected per container")
	createCmd.Flags().BoolVar(&includeBackup, "include-backup", false, "Also create a backup using the standard backup behavior")
	createCmd.Flags().BoolVar(&includeQueries, "include-queries", false, "Include database query logs (may contain customer data)")
	createCmd.Flags().BoolVar(&benchmark, "benchmark", false, "Run the OpsMill benchmark and include its results (requires image download; skipped with a warning if unavailable)")
	createCmd.Flags().IntVar(&telemetryDays, "telemetry-days", 30, "Look-back window in days for the Infrahub product-telemetry export")

	// Bind create flags to Viper for environment variable support (INFRAHUB_<FLAG_NAME>)
	viper.BindPFlag("log-lines", createCmd.Flags().Lookup("log-lines"))
	viper.BindPFlag("include-backup", createCmd.Flags().Lookup("include-backup"))
	viper.BindPFlag("include-queries", createCmd.Flags().Lookup("include-queries"))
	viper.BindPFlag("benchmark", createCmd.Flags().Lookup("benchmark"))
	viper.BindPFlag("telemetry-days", createCmd.Flags().Lookup("telemetry-days"))

	rootCmd.AddCommand(createCmd)

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
