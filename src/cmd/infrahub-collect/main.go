package main

import (
	"fmt"
	"os"

	app "infrahub-ops/src/internal/app"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"golang.org/x/term"
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
	var includeTraces bool
	var tracesSince, tracesStart, tracesEnd, tracesMinDuration, tracesTempoURL string
	var tracesMaxSize int

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

			traces, err := readTraceOptions()
			if err != nil {
				return err
			}

			opts := app.CollectOptions{
				OutputDir:      viper.GetString("output-dir"),
				LogLines:       lines,
				IncludeBackup:  viper.GetBool("include-backup"),
				IncludeQueries: viper.GetBool("include-queries"),
				Benchmark:      viper.GetBool("benchmark"),
				TelemetryDays:  viper.GetInt("telemetry-days"),
				Traces:         traces,
			}

			return iops.CollectBundle(opts)
		},
	}
	createCmd.Flags().IntVar(&logLines, "log-lines", 100000, "Maximum log lines collected per container")
	createCmd.Flags().BoolVar(&includeBackup, "include-backup", false, "Also create a backup using the standard backup behavior")
	createCmd.Flags().BoolVar(&includeQueries, "include-queries", false, "Include database query logs (may contain customer data)")
	createCmd.Flags().BoolVar(&benchmark, "benchmark", false, "Run the OpsMill benchmark and include its results (requires image download; skipped with a warning if unavailable)")
	createCmd.Flags().IntVar(&telemetryDays, "telemetry-days", 30, "Look-back window in days for the Infrahub product-telemetry export")
	createCmd.Flags().BoolVar(&includeTraces, "include-traces", false, "Include the traces of slow requests from Tempo (personal data is removed)")
	createCmd.Flags().StringVar(&tracesSince, "traces-since", "2h", "Collect the traces of this period before now, as a duration such as 30m or 2h")
	createCmd.Flags().StringVar(&tracesStart, "traces-start", "", "Start of the traces window, in RFC 3339 (use with --traces-end)")
	createCmd.Flags().StringVar(&tracesEnd, "traces-end", "", "End of the traces window, in RFC 3339 (use with --traces-start)")
	createCmd.Flags().StringVar(&tracesMinDuration, "traces-min-duration", "1s", "Collect only the traces that lasted longer than this duration")
	createCmd.Flags().IntVar(&tracesMaxSize, "traces-max-size", 100, "Size limit of the collected traces, in megabytes (uncompressed)")
	createCmd.Flags().StringVar(&tracesTempoURL, "traces-tempo-url", "", "Read the traces from this Tempo-compatible URL instead of the deployment's Tempo")

	// Bind create flags to Viper for environment variable support (INFRAHUB_<FLAG_NAME>)
	viper.BindPFlag("log-lines", createCmd.Flags().Lookup("log-lines"))
	viper.BindPFlag("include-backup", createCmd.Flags().Lookup("include-backup"))
	viper.BindPFlag("include-queries", createCmd.Flags().Lookup("include-queries"))
	viper.BindPFlag("benchmark", createCmd.Flags().Lookup("benchmark"))
	viper.BindPFlag("telemetry-days", createCmd.Flags().Lookup("telemetry-days"))
	viper.BindPFlag("include-traces", createCmd.Flags().Lookup("include-traces"))
	for _, name := range app.TraceOptionNames() {
		viper.BindPFlag(name, createCmd.Flags().Lookup(name))
	}

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

// readTraceOptions validates the trace options and reads the two tokens before
// any collector runs. A trace option counts as given when its flag is set or
// its environment variable is set.
func readTraceOptions() (app.TraceOptions, error) {
	given := map[string]bool{}
	for _, name := range app.TraceOptionNames() {
		if viper.IsSet(name) {
			given[name] = true
		}
	}

	opts, err := app.ValidateTraceOptions(app.TraceOptionsInput{
		Include:     viper.GetBool("include-traces"),
		Since:       viper.GetString("traces-since"),
		Start:       viper.GetString("traces-start"),
		End:         viper.GetString("traces-end"),
		MinDuration: viper.GetString("traces-min-duration"),
		MaxSizeMB:   viper.GetInt("traces-max-size"),
		TempoURL:    viper.GetString("traces-tempo-url"),
		Given:       given,
	})
	if err != nil {
		return app.TraceOptions{}, err
	}

	err = app.ReadTraceTokens(&opts, app.TraceTokenSources{
		LookupEnv: os.LookupEnv,
		IsTerminal: func() bool {
			return term.IsTerminal(int(os.Stdin.Fd()))
		},
		ReadPassword: func(prompt string) (string, error) {
			fmt.Fprint(os.Stderr, prompt)
			value, readErr := term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Fprintln(os.Stderr)
			return string(value), readErr
		},
	})
	return opts, err
}
