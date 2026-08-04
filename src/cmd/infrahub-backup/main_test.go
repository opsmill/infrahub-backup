package main

import (
	"strings"
	"testing"

	app "infrahub-ops/src/internal/app"

	"github.com/spf13/cobra"
)

// retentionFlagCommand builds a command carrying the retention flags exactly as
// `create` registers them, with the given command line already parsed.
func retentionFlagCommand(t *testing.T, args ...string) *cobra.Command {
	t.Helper()

	cmd := &cobra.Command{Use: "create", RunE: func(*cobra.Command, []string) error { return nil }}
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
