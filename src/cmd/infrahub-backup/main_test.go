package main

import (
	"strings"
	"testing"

	app "infrahub-ops/src/internal/app"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// retentionFlagCommand builds a command carrying the retention flags exactly as
// `create` registers them, with the given command line already parsed.
func retentionFlagCommand(t *testing.T, args ...string) *cobra.Command {
	t.Helper()

	return namedRetentionFlagCommand(t, "create", args...)
}

// namedRetentionFlagCommand builds one command of the given name carrying the two
// retention flags, with the given command line already parsed.
func namedRetentionFlagCommand(t *testing.T, use string, args ...string) *cobra.Command {
	t.Helper()

	cmd := &cobra.Command{Use: use, RunE: func(*cobra.Command, []string) error { return nil }}
	cmd.Flags().Int("retention-days", 0, "")
	cmd.Flags().Int("retention-count", 0, "")
	if err := cmd.Flags().Parse(args); err != nil {
		t.Fatalf("parsing %v = %v, want nil", args, err)
	}

	return cmd
}

// TestResolveRetentionFlags covers the flag layer's share of FR-001: an omitted
// rule is inactive, a set rule reaches the configuration, and an explicit 0 —
// which RetentionPolicy.Validate cannot distinguish from "unset" — is rejected
// before any backup work starts.
func TestResolveRetentionFlags(t *testing.T) {
	tests := []struct {
		name          string
		args          []string
		wantRetention app.RetentionConfig
		errContains   string
	}{
		{
			name: "no flags leaves retention inactive",
		},
		{
			name:          "days only",
			args:          []string{"--retention-days", "7"},
			wantRetention: app.RetentionConfig{Days: 7},
		},
		{
			name:          "count only",
			args:          []string{"--retention-count", "14"},
			wantRetention: app.RetentionConfig{Count: 14},
		},
		{
			name:          "both rules",
			args:          []string{"--retention-days", "7", "--retention-count", "14"},
			wantRetention: app.RetentionConfig{Days: 7, Count: 14},
		},
		{
			name:        "explicit zero days rejected",
			args:        []string{"--retention-days", "0"},
			errContains: "--retention-days must be at least 1 when set",
		},
		{
			name:        "explicit zero count rejected",
			args:        []string{"--retention-count", "0"},
			errContains: "--retention-count must be at least 1 when set",
		},
		{
			name:        "explicit zero rejected alongside a valid rule",
			args:        []string{"--retention-days", "7", "--retention-count", "0"},
			errContains: "--retention-count must be at least 1 when set",
		},
		{
			name:        "negative days rejected",
			args:        []string{"--retention-days", "-1"},
			errContains: "retention-days must be at least 1",
		},
		{
			name:        "negative count rejected",
			args:        []string{"--retention-count", "-5"},
			errContains: "retention-count must be at least 1",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			iops := app.NewInfrahubOps()
			err := resolveRetentionFlags(retentionFlagCommand(t, tc.args...), iops)

			if tc.errContains != "" {
				if err == nil {
					t.Fatalf("resolveRetentionFlags() = nil, want an error containing %q", tc.errContains)
				}
				if !strings.Contains(err.Error(), tc.errContains) {
					t.Errorf("error = %q, want it to contain %q", err, tc.errContains)
				}
				// A rejected policy never reaches the configuration.
				if got := iops.Config().Retention; got != (app.RetentionConfig{}) {
					t.Errorf("configuration retention = %+v, want it untouched after a validation error", got)
				}
				return
			}

			if err != nil {
				t.Fatalf("resolveRetentionFlags() = %v, want nil", err)
			}
			if got := iops.Config().Retention; got != tc.wantRetention {
				t.Errorf("configuration retention = %+v, want %+v", got, tc.wantRetention)
			}
		})
	}
}

// TestRetentionFlagsResolvePerCommand guards the reason resolveRetentionFlags reads
// the invoked command's own flag: `create` binds the retention keys to viper, and
// viper resolves a bound flag through whichever command bound the key last. Reading
// viper directly would make each command answer with the other's flag, so this test
// registers both commands the way main does — create bound, prune unbound — and
// checks that each still resolves its own command line while the environment
// variable keeps working for both (FR-011).
func TestRetentionFlagsResolvePerCommand(t *testing.T) {
	bindCreate := func(t *testing.T, createCmd *cobra.Command) {
		t.Helper()

		viper.Reset()
		t.Cleanup(viper.Reset)
		viper.SetEnvPrefix("INFRAHUB")
		viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
		viper.AutomaticEnv()
		for _, name := range []string{"retention-days", "retention-count"} {
			if err := viper.BindPFlag(name, createCmd.Flags().Lookup(name)); err != nil {
				t.Fatalf("BindPFlag(%q) = %v, want nil", name, err)
			}
		}
	}

	resolve := func(t *testing.T, cmd *cobra.Command) app.RetentionConfig {
		t.Helper()

		iops := app.NewInfrahubOps()
		if err := resolveRetentionFlags(cmd, iops); err != nil {
			t.Fatalf("resolveRetentionFlags(%s) = %v, want nil", cmd.Use, err)
		}

		return iops.Config().Retention
	}

	// Only ever one command's flags are parsed per process, so each case sets a flag
	// on the invoked command and leaves the other command's flags untouched, exactly
	// as cobra does.
	t.Run("create's flag reaches create", func(t *testing.T) {
		createCmd := namedRetentionFlagCommand(t, "create", "--retention-days", "7")
		bindCreate(t, createCmd)

		if got, want := resolve(t, createCmd), (app.RetentionConfig{Days: 7}); got != want {
			t.Errorf("create retention = %+v, want %+v", got, want)
		}
	})

	t.Run("prune's flag reaches prune despite create owning the viper binding", func(t *testing.T) {
		createCmd := namedRetentionFlagCommand(t, "create")
		pruneCmd := namedRetentionFlagCommand(t, "prune", "--retention-count", "5")
		bindCreate(t, createCmd)

		if got, want := resolve(t, pruneCmd), (app.RetentionConfig{Count: 5}); got != want {
			t.Errorf("prune retention = %+v, want %+v", got, want)
		}
		if got := resolve(t, createCmd); got != (app.RetentionConfig{}) {
			t.Errorf("create retention = %+v, want prune's flag not to leak into create", got)
		}
	})

	t.Run("environment variable reaches both commands", func(t *testing.T) {
		createCmd := namedRetentionFlagCommand(t, "create")
		pruneCmd := namedRetentionFlagCommand(t, "prune")
		bindCreate(t, createCmd)
		t.Setenv("INFRAHUB_RETENTION_DAYS", "9")

		for _, cmd := range []*cobra.Command{createCmd, pruneCmd} {
			if got, want := resolve(t, cmd), (app.RetentionConfig{Days: 9}); got != want {
				t.Errorf("%s retention = %+v, want %+v", cmd.Use, got, want)
			}
		}
	})

	t.Run("a command's own flag outranks the environment", func(t *testing.T) {
		createCmd := namedRetentionFlagCommand(t, "create")
		pruneCmd := namedRetentionFlagCommand(t, "prune", "--retention-days", "3")
		bindCreate(t, createCmd)
		t.Setenv("INFRAHUB_RETENTION_DAYS", "9")

		if got, want := resolve(t, pruneCmd), (app.RetentionConfig{Days: 3}); got != want {
			t.Errorf("prune retention = %+v, want %+v", got, want)
		}
	})
}
