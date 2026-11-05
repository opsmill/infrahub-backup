package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// includeTaskManagerStorageEnvVar is the environment variable that viper's
// INFRAHUB_ prefix binding derives from --include-task-manager-storage
// (contracts/cli.md).
const includeTaskManagerStorageEnvVar = "INFRAHUB_INCLUDE_TASK_MANAGER_STORAGE"

// createFlagCommand builds a command carrying the flags of `create` exactly as
// main() registers and binds them, with viper's INFRAHUB_ environment
// resolution set up as ConfigureRootCommand does, and parses the given command
// line.
func createFlagCommand(t *testing.T, args ...string) *cobra.Command {
	t.Helper()

	viper.Reset()
	t.Cleanup(viper.Reset)
	viper.SetEnvPrefix("INFRAHUB")
	viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	viper.AutomaticEnv()

	cmd := &cobra.Command{Use: "create", RunE: func(*cobra.Command, []string) error { return nil }}
	addCreateFlags(cmd)
	if err := cmd.Flags().Parse(args); err != nil {
		t.Fatalf("parsing %v = %v, want nil", args, err)
	}

	return cmd
}

// TestResolveCollectOptions_IncludeTaskManagerStorage covers FR-004: the
// breakdown is requested with the flag or its environment variable, and the
// flag outranks the variable.
func TestResolveCollectOptions_IncludeTaskManagerStorage(t *testing.T) {
	tests := []struct {
		name string
		args []string
		env  string
		want bool
	}{
		{name: "neither flag nor variable", want: false},
		{name: "flag", args: []string{"--include-task-manager-storage"}, want: true},
		{name: "environment variable", env: "true", want: true},
		{name: "environment variable set to false", env: "false", want: false},
		{name: "flag set to false outranks the variable", args: []string{"--include-task-manager-storage=false"}, env: "true", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// An empty value counts as unset for viper, so a value in the
			// developer's environment cannot decide the outcome.
			t.Setenv(includeTaskManagerStorageEnvVar, tt.env)
			createFlagCommand(t, tt.args...)

			opts, err := resolveCollectOptions()
			if err != nil {
				t.Fatalf("resolveCollectOptions() = %v, want nil", err)
			}
			if opts.IncludeTaskManagerStorage != tt.want {
				t.Errorf("IncludeTaskManagerStorage = %v, want %v", opts.IncludeTaskManagerStorage, tt.want)
			}
			// The other opt-in collectors stay off.
			if opts.IncludeBackup || opts.Benchmark || opts.IncludeQueries {
				t.Errorf("options = %+v, want only the storage breakdown to change", opts)
			}
		})
	}
}

// TestAddCreateFlags_IncludeTaskManagerStorageHelp pins the help text of the
// flag to contracts/cli.md.
func TestAddCreateFlags_IncludeTaskManagerStorageHelp(t *testing.T) {
	cmd := createFlagCommand(t)

	flag := cmd.Flags().Lookup("include-task-manager-storage")
	if flag == nil {
		t.Fatal("create has no --include-task-manager-storage flag")
	}
	if flag.DefValue != "false" {
		t.Errorf("default = %q, want false", flag.DefValue)
	}
	want := "Also collect the storage breakdown of the task manager database (reads every row of its run, state, log, event and artifact tables; up to 5 minutes per table)"
	if flag.Usage != want {
		t.Errorf("help text = %q, want %q", flag.Usage, want)
	}
}
