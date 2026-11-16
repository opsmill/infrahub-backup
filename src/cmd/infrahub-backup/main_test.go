package main

import (
	"io"
	"os"
	"strings"
	"testing"

	app "infrahub-ops/src/internal/app"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// unsetRetentionEnv removes the retention environment variables for the duration of one
// test, so a value in the developer's or CI runner's environment cannot decide the
// outcome. t.Setenv records the original value, and unsetting afterwards leaves the
// variable absent until that recorded value is restored.
func unsetRetentionEnv(t *testing.T) {
	t.Helper()

	for _, name := range []string{app.RetentionDaysEnvVar, app.RetentionCountEnvVar} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unsetting %s = %v, want nil", name, err)
		}
	}
}

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
			unsetRetentionEnv(t)

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

// TestResolveRetentionFlagsFromEnvironment covers the unattended configuration channel
// (FR-011). A value that cannot be read as a rule must abort the run: resolving it to 0
// would mean "rule inactive", so a mistyped variable would switch retention off for as
// long as nobody looked — on the scheduled runs this feature exists to serve.
func TestResolveRetentionFlagsFromEnvironment(t *testing.T) {
	tests := []struct {
		name string
		// env holds the variables to set; a key present with an empty value is a
		// variable an operator set to nothing, which is not the same as an unset one.
		env           map[string]string
		args          []string
		wantRetention app.RetentionConfig
		errContains   string
	}{
		{
			name:          "days variable configures the age rule",
			env:           map[string]string{app.RetentionDaysEnvVar: "7"},
			wantRetention: app.RetentionConfig{Days: 7},
		},
		{
			name:          "count variable configures the count rule",
			env:           map[string]string{app.RetentionCountEnvVar: "14"},
			wantRetention: app.RetentionConfig{Count: 14},
		},
		{
			name:          "both variables",
			env:           map[string]string{app.RetentionDaysEnvVar: "7", app.RetentionCountEnvVar: "14"},
			wantRetention: app.RetentionConfig{Days: 7, Count: 14},
		},
		{
			name:          "surrounding whitespace is tolerated",
			env:           map[string]string{app.RetentionDaysEnvVar: " 7 "},
			wantRetention: app.RetentionConfig{Days: 7},
		},
		{
			name:        "a non-numeric value aborts instead of disabling the rule",
			env:         map[string]string{app.RetentionDaysEnvVar: "abc"},
			errContains: `INFRAHUB_RETENTION_DAYS must be a whole number of at least 1 (got "abc")`,
		},
		{
			name:        "a duration-style value aborts",
			env:         map[string]string{app.RetentionDaysEnvVar: "7d"},
			errContains: `INFRAHUB_RETENTION_DAYS must be a whole number of at least 1 (got "7d")`,
		},
		{
			name:        "a fractional value aborts instead of truncating",
			env:         map[string]string{app.RetentionDaysEnvVar: "7.5"},
			errContains: `INFRAHUB_RETENTION_DAYS must be a whole number of at least 1 (got "7.5")`,
		},
		{
			name:        "an explicit zero aborts",
			env:         map[string]string{app.RetentionCountEnvVar: "0"},
			errContains: `INFRAHUB_RETENTION_COUNT must be a whole number of at least 1 (got "0")`,
		},
		{
			name:        "a negative value aborts",
			env:         map[string]string{app.RetentionDaysEnvVar: "-5"},
			errContains: `INFRAHUB_RETENTION_DAYS must be a whole number of at least 1 (got "-5")`,
		},
		{
			name:          "a trailing-space value resolves instead of coercing to zero",
			env:           map[string]string{app.RetentionDaysEnvVar: "1 "},
			wantRetention: app.RetentionConfig{Days: 1},
		},
		{
			// `INFRAHUB_RETENTION_DAYS=${RETENTION_DAYS}` with nothing to substitute is
			// how Docker Compose and Kubernetes render an unset variable, so an empty
			// value must leave the rule inactive instead of failing the run.
			name: "an empty value leaves the rule inactive",
			env:  map[string]string{app.RetentionDaysEnvVar: ""},
		},
		{
			name: "a whitespace-only value leaves the rule inactive",
			env:  map[string]string{app.RetentionCountEnvVar: "  "},
		},
		{
			name:          "an empty value leaves the other rule alone",
			env:           map[string]string{app.RetentionDaysEnvVar: "", app.RetentionCountEnvVar: "14"},
			wantRetention: app.RetentionConfig{Count: 14},
		},
		{
			name:          "the command's own flag outranks the variable",
			env:           map[string]string{app.RetentionDaysEnvVar: "9"},
			args:          []string{"--retention-days", "3"},
			wantRetention: app.RetentionConfig{Days: 3},
		},
		{
			name:          "a flag outranks a variable that could not be read",
			env:           map[string]string{app.RetentionDaysEnvVar: "abc"},
			args:          []string{"--retention-days", "3"},
			wantRetention: app.RetentionConfig{Days: 3},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			unsetRetentionEnv(t)
			for name, value := range tc.env {
				t.Setenv(name, value)
			}

			iops := app.NewInfrahubOps()
			err := resolveRetentionFlags(retentionFlagCommand(t, tc.args...), iops)

			if tc.errContains != "" {
				if err == nil {
					t.Fatalf("resolveRetentionFlags() = nil, want an error containing %q", tc.errContains)
				}
				if !strings.Contains(err.Error(), tc.errContains) {
					t.Errorf("error = %q, want it to contain %q", err, tc.errContains)
				}
				// A rejected policy never reaches the configuration, so the run aborts
				// before any backup work starts.
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
// the invoked command's own flag: `create` and `prune` register the same flag name, and
// viper resolves a bound flag through whichever command bound the key last, so a
// resolution that went through viper would make each command answer with the other's
// flag. The test binds the keys the way a viper-based resolution would need them —
// create bound, prune unbound — and checks that each command still resolves its own
// command line while the environment variable keeps working for both (FR-011).
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

// restoreOptionCommand builds a command carrying restore's two viper-backed flags exactly as
// main() registers and binds them — no flag variables, bound to the same keys, with viper's
// INFRAHUB_ environment resolution in place — and parses the given command line.
//
// Registering them the way main() does is the point: the defect this pins was the binding and
// the read disagreeing, so a test that read the flags directly would pass either way.
func restoreOptionCommand(t *testing.T, args ...string) *cobra.Command {
	t.Helper()

	viper.Reset()
	t.Cleanup(viper.Reset)
	viper.SetEnvPrefix("INFRAHUB")
	viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	viper.AutomaticEnv()

	cmd := &cobra.Command{Use: "restore", RunE: func(*cobra.Command, []string) error { return nil }}
	cmd.Flags().String("decrypt-key", "", "")
	cmd.Flags().Bool("reset-deployment-id", false, "")
	for _, name := range []string{"decrypt-key", "reset-deployment-id"} {
		if err := viper.BindPFlag(name, cmd.Flags().Lookup(name)); err != nil {
			t.Fatalf("BindPFlag(%q) = %v, want nil", name, err)
		}
	}
	if err := cmd.Flags().Parse(args); err != nil {
		t.Fatalf("parsing %v = %v, want nil", args, err)
	}

	return cmd
}

// TestResolveRestoreOptions pins the channels restore's decryption key and deployment-ID
// reset resolve from. Both flags were bound to viper and then read from their flag variables
// instead, so INFRAHUB_DECRYPT_KEY and INFRAHUB_RESET_DEPLOYMENT_ID silently did nothing —
// which meant an unattended restore configured entirely through the environment could not
// supply a decryption key at all.
func TestResolveRestoreOptions(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		args []string
		want restoreOptions
	}{
		{
			name: "neither channel leaves both unset",
		},
		{
			name: "flags configure both",
			args: []string{"--decrypt-key", "/keys/backup.key", "--reset-deployment-id"},
			want: restoreOptions{DecryptKey: "/keys/backup.key", ResetDeploymentID: true},
		},
		{
			name: "the decryption key comes from the environment",
			env:  map[string]string{"INFRAHUB_DECRYPT_KEY": "/keys/from-env.key"},
			want: restoreOptions{DecryptKey: "/keys/from-env.key"},
		},
		{
			name: "the deployment-id reset comes from the environment",
			env:  map[string]string{"INFRAHUB_RESET_DEPLOYMENT_ID": "true"},
			want: restoreOptions{ResetDeploymentID: true},
		},
		{
			name: "both variables together",
			env: map[string]string{
				"INFRAHUB_DECRYPT_KEY":         "/keys/from-env.key",
				"INFRAHUB_RESET_DEPLOYMENT_ID": "true",
			},
			want: restoreOptions{DecryptKey: "/keys/from-env.key", ResetDeploymentID: true},
		},
		{
			name: "the flag outranks the variable",
			env:  map[string]string{"INFRAHUB_DECRYPT_KEY": "/keys/from-env.key"},
			args: []string{"--decrypt-key", "/keys/from-flag.key"},
			want: restoreOptions{DecryptKey: "/keys/from-flag.key"},
		},
		{
			// `INFRAHUB_DECRYPT_KEY=${DECRYPT_KEY}` with nothing to substitute is how
			// Docker Compose and Kubernetes render an unset variable, so an empty value
			// must read as "no key" rather than as a path.
			name: "an empty variable is not a key",
			env:  map[string]string{"INFRAHUB_DECRYPT_KEY": ""},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, name := range []string{"INFRAHUB_DECRYPT_KEY", "INFRAHUB_RESET_DEPLOYMENT_ID"} {
				t.Setenv(name, "")
				if err := os.Unsetenv(name); err != nil {
					t.Fatalf("unsetting %s = %v, want nil", name, err)
				}
			}
			for name, value := range tc.env {
				t.Setenv(name, value)
			}

			restoreOptionCommand(t, tc.args...)

			if got := resolveRestoreOptions(); got != tc.want {
				t.Errorf("resolveRestoreOptions() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestResolveRestoreInvocation is the invocation matrix of contracts/cli.md, backend by
// backend. Every rejected row is a command line that could mean two things on a command
// that overwrites a deployment's data, so each one must fail rather than resolve to a
// guess (FR-002, FR-003, FR-004, surface of FR-006).
func TestResolveRestoreInvocation(t *testing.T) {
	tests := []struct {
		name    string
		backend app.BackendType
		args    []string
		latest  bool
		s3      bool
		want    restoreRequest
		// errContains are fragments the error must carry: what was wrong, and the flag or
		// form that fixes it.
		errContains []string
	}{
		{
			name:    "tarball: a named archive is unchanged",
			backend: app.BackendTarball,
			args:    []string{"infrahub_backup_20260804_120000.tar.gz"},
			want:    restoreRequest{Archive: "infrahub_backup_20260804_120000.tar.gz"},
		},
		{
			name:    "tarball: an s3 URI argument is unchanged",
			backend: app.BackendTarball,
			args:    []string{"s3://bucket/prod/infrahub_backup_20260804_120000.tar.gz"},
			want:    restoreRequest{Archive: "s3://bucket/prod/infrahub_backup_20260804_120000.tar.gz"},
		},
		{
			name:    "tarball: --latest selects from the local pool",
			backend: app.BackendTarball,
			latest:  true,
			want:    restoreRequest{Latest: true},
		},
		{
			name:    "tarball: --latest --s3 selects from the bucket",
			backend: app.BackendTarball,
			latest:  true,
			s3:      true,
			want:    restoreRequest{Latest: true, S3: true},
		},
		{
			// FR-003: bare `restore` stays an error, and now points at the flag that
			// restores without a filename.
			name:        "tarball: bare restore still fails and names --latest",
			backend:     app.BackendTarball,
			errContains: []string{"requires exactly 1 arg(s), only received 0", "--latest"},
		},
		{
			name:        "tarball: more than one archive fails",
			backend:     app.BackendTarball,
			args:        []string{"one.tar.gz", "two.tar.gz"},
			errContains: []string{"requires exactly 1 arg(s), only received 2", "--latest"},
		},
		{
			// FR-002: nothing is restored when the command line asks for both.
			name:        "tarball: --latest with an archive is rejected",
			backend:     app.BackendTarball,
			args:        []string{"infrahub_backup_20260804_120000.tar.gz"},
			latest:      true,
			errContains: []string{"mutually exclusive", "--latest", "infrahub_backup_20260804_120000.tar.gz"},
		},
		{
			name:        "tarball: --s3 without --latest points at the URI form",
			backend:     app.BackendTarball,
			s3:          true,
			errContains: []string{"--s3", "requires it", "s3://bucket/key"},
		},
		{
			name:        "tarball: --s3 with an archive but no --latest is rejected",
			backend:     app.BackendTarball,
			args:        []string{"infrahub_backup_20260804_120000.tar.gz"},
			s3:          true,
			errContains: []string{"--s3", "s3://bucket/key"},
		},
		{
			// FR-004: the no-argument form keeps working unchanged.
			name:    "plakar: bare restore resolves the latest backup group",
			backend: app.BackendPlakar,
			want:    restoreRequest{},
		},
		{
			// FR-004: --latest takes the very same route, so the two cannot drift apart.
			name:    "plakar: --latest is an alias for the no-argument form",
			backend: app.BackendPlakar,
			latest:  true,
			want:    restoreRequest{},
		},
		{
			name:        "plakar: --latest --s3 is rejected",
			backend:     app.BackendPlakar,
			latest:      true,
			s3:          true,
			errContains: []string{"--s3", "plakar", "--repo"},
		},
		{
			name:        "plakar: --s3 without --latest is rejected",
			backend:     app.BackendPlakar,
			s3:          true,
			errContains: []string{"--s3"},
		},
		{
			name:        "plakar: --latest with an archive is rejected",
			backend:     app.BackendPlakar,
			args:        []string{"infrahub_backup_20260804_120000.tar.gz"},
			latest:      true,
			errContains: []string{"mutually exclusive"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveRestoreInvocation(tc.backend, tc.args, tc.latest, tc.s3)

			if len(tc.errContains) > 0 {
				if err == nil {
					t.Fatalf("resolveRestoreInvocation() = %+v, nil, want an error", got)
				}
				for _, want := range tc.errContains {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error = %q, want it to contain %q", err, want)
					}
				}
				// A rejected invocation resolves to nothing, so no route can be taken from it.
				if got != (restoreRequest{}) {
					t.Errorf("request = %+v, want the zero request alongside an error", got)
				}
				return
			}

			if err != nil {
				t.Fatalf("resolveRestoreInvocation() = %v, want nil", err)
			}
			if got != tc.want {
				t.Errorf("resolveRestoreInvocation() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestRestoreCommandValidatesBeforeRunning covers the Args/RunE ordering: a rejected
// invocation is rejected before RunE runs — the only ordering in which "nothing was
// restored" is guaranteed (FR-002, FR-003).
//
// It builds its own command with its own flag registration rather than driving the one
// the binary ships, so it pins neither the published flag names nor where the backend
// comes from: it is handed a backend directly. That is precisely how T123 stayed hidden
// from it, so what the binary actually enforces is pinned by
// TestShippedRestoreCommandInvocationContract instead. This case remains as the
// Args-before-RunE ordering on its own, decoupled from the wiring.
func TestRestoreCommandValidatesBeforeRunning(t *testing.T) {
	tests := []struct {
		name        string
		backend     app.BackendType
		args        []string
		wantRequest restoreRequest
		errContains string
	}{
		{
			name:        "a named archive runs",
			backend:     app.BackendTarball,
			args:        []string{"infrahub_backup_20260804_120000.tar.gz"},
			wantRequest: restoreRequest{Archive: "infrahub_backup_20260804_120000.tar.gz"},
		},
		{
			name:        "--latest runs without an archive",
			backend:     app.BackendTarball,
			args:        []string{"--latest"},
			wantRequest: restoreRequest{Latest: true},
		},
		{
			name:        "--latest --s3 runs against the bucket pool",
			backend:     app.BackendTarball,
			args:        []string{"--latest", "--s3"},
			wantRequest: restoreRequest{Latest: true, S3: true},
		},
		{
			name:        "bare restore never runs",
			backend:     app.BackendTarball,
			errContains: "--latest",
		},
		{
			name:        "--latest with an archive never runs",
			backend:     app.BackendTarball,
			args:        []string{"--latest", "infrahub_backup_20260804_120000.tar.gz"},
			errContains: "mutually exclusive",
		},
		{
			name:        "--s3 alone never runs",
			backend:     app.BackendTarball,
			args:        []string{"--s3"},
			errContains: "--s3",
		},
		{
			name:        "plakar --latest runs the no-argument route",
			backend:     app.BackendPlakar,
			args:        []string{"--latest"},
			wantRequest: restoreRequest{},
		},
		{
			name:        "plakar --latest --s3 never runs",
			backend:     app.BackendPlakar,
			args:        []string{"--latest", "--s3"},
			errContains: "--repo",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var latest, s3 bool
			var ran bool
			var request restoreRequest

			cmd := &cobra.Command{
				Use:          "restore [backup-file]",
				SilenceUsage: true,
				Args: func(cmd *cobra.Command, args []string) error {
					_, err := resolveRestoreInvocation(tc.backend, args, latest, s3)
					return err
				},
				RunE: func(cmd *cobra.Command, args []string) error {
					resolved, err := resolveRestoreInvocation(tc.backend, args, latest, s3)
					if err != nil {
						return err
					}
					ran, request = true, resolved
					return nil
				},
			}
			cmd.Flags().BoolVar(&latest, "latest", false, "")
			cmd.Flags().BoolVar(&s3, "s3", false, "")
			cmd.SetArgs(tc.args)
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)

			err := cmd.Execute()

			if tc.errContains != "" {
				if err == nil {
					t.Fatalf("Execute(%v) = nil, want an error", tc.args)
				}
				if !strings.Contains(err.Error(), tc.errContains) {
					t.Errorf("error = %q, want it to contain %q", err, tc.errContains)
				}
				if ran {
					t.Error("RunE ran, want the invocation rejected before any restore work")
				}
				return
			}

			if err != nil {
				t.Fatalf("Execute(%v) = %v, want nil", tc.args, err)
			}
			if !ran {
				t.Fatal("RunE did not run, want the restore to proceed")
			}
			if request != tc.wantRequest {
				t.Errorf("request = %+v, want %+v", request, tc.wantRequest)
			}
		})
	}
}

// restoreInvocation is what the shipped `restore` command was left holding once cobra
// had parsed its flags, validated its arguments, and reached its action.
type restoreInvocation struct {
	ran     bool
	args    []string
	backend app.BackendType
	latest  bool
	s3      bool
}

// shippedRestoreCommand wires the `restore` command the binary ships under a root
// configured exactly as main configures it, and swaps its action for a recorder so the
// invocation contract can be driven without a deployment to overwrite.
//
// Everything the contract spans is the real thing: the flag names, the Args hook, and
// cobra's ordering between flag parsing, argument validation, and the persistent pre-run
// that resolves --backend. viper is global, so each call resets it and restores it
// afterwards.
func shippedRestoreCommand(t *testing.T, env map[string]string) (*cobra.Command, *restoreInvocation) {
	t.Helper()

	viper.Reset()
	t.Cleanup(viper.Reset)

	for _, name := range []string{"INFRAHUB_BACKEND", "INFRAHUB_REPO", "INFRAHUB_LATEST", "INFRAHUB_S3"} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unsetting %s = %v, want nil", name, err)
		}
	}
	for name, value := range env {
		t.Setenv(name, value)
	}

	iops := app.NewInfrahubOps()
	root := &cobra.Command{Use: "infrahub-backup", SilenceUsage: true, RunE: func(cmd *cobra.Command, args []string) error { return nil }}
	app.ConfigureRootCommand(root, iops)

	restoreCmd := newRestoreCommand(iops)
	record := &restoreInvocation{}
	restoreCmd.RunE = func(cmd *cobra.Command, args []string) error {
		record.ran = true
		record.args = args
		record.backend = iops.Config().Backend
		record.latest, _ = cmd.Flags().GetBool("latest")
		record.s3, _ = cmd.Flags().GetBool("s3")
		return nil
	}

	root.AddCommand(restoreCmd)
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)

	return root, record
}

// TestShippedRestoreCommandInvocationContract is the invocation contract as the binary
// enforces it, driven through the command main ships rather than a copy of it.
//
// A copy is what let T123 through. TestRestoreCommandValidatesBeforeRunning hands
// resolveRestoreInvocation a backend directly, so it stayed green when the shipped
// command started reading a backend nothing had resolved yet: cobra validates arguments
// before it runs the persistent pre-run, so `--backend plakar restore` was judged by the
// tarball arity rule and refused for naming no archive — a Plakar restore names none.
//
// Every row therefore states which backend the invocation actually ran under, because
// "accepted" is only the right answer for the backend the command line asked for.
func TestShippedRestoreCommandInvocationContract(t *testing.T) {
	tests := []struct {
		name        string
		env         map[string]string
		args        []string
		wantBackend app.BackendType
		wantArgs    []string
		wantLatest  bool
		wantS3      bool
		// errContains are fragments the refusal must carry: what was wrong, and the flag
		// or form that fixes it.
		errContains []string
	}{
		{
			// T123: the regression itself. A Plakar restore takes no positional archive.
			name:        "plakar: bare restore is accepted",
			args:        []string{"--backend", "plakar", "--repo", "/repo", "restore"},
			wantBackend: app.BackendPlakar,
		},
		{
			// The same invocation configured the way an unattended run configures it.
			name:        "plakar from the environment: bare restore is accepted",
			env:         map[string]string{"INFRAHUB_BACKEND": "plakar"},
			args:        []string{"restore"},
			wantBackend: app.BackendPlakar,
		},
		{
			// FR-004: --latest is an alias for the no-argument form on this backend, so
			// it is accepted and takes the same route.
			name:        "plakar: --latest is accepted",
			args:        []string{"--backend", "plakar", "--repo", "/repo", "restore", "--latest"},
			wantBackend: app.BackendPlakar,
			wantLatest:  true,
		},
		{
			// resolveRestoreInvocation accepts this: the group to restore comes from
			// --backup-id, and the argument is not consulted. It is pinned here so a
			// change to that reading is a change to this test.
			name:        "plakar: a named archive is accepted and not consulted",
			args:        []string{"--backend", "plakar", "--repo", "/repo", "restore", "infrahub_backup_20260804_120000.tar.gz"},
			wantBackend: app.BackendPlakar,
			wantArgs:    []string{"infrahub_backup_20260804_120000.tar.gz"},
		},
		{
			name:        "plakar: --latest --s3 is refused",
			args:        []string{"--backend", "plakar", "--repo", "/repo", "restore", "--latest", "--s3"},
			errContains: []string{"--s3", "plakar", "--repo"},
		},
		{
			// FR-003: the tarball arity rule is unchanged, message included — a typo must
			// not become a data-overwriting default.
			name:        "tarball: bare restore is still refused",
			args:        []string{"restore"},
			errContains: []string{"requires exactly 1 arg(s), only received 0", "--latest"},
		},
		{
			name:        "tarball: a named archive is accepted",
			args:        []string{"restore", "infrahub_backup_20260804_120000.tar.gz"},
			wantBackend: app.BackendTarball,
			wantArgs:    []string{"infrahub_backup_20260804_120000.tar.gz"},
		},
		{
			name:        "tarball: --latest is accepted without an archive",
			args:        []string{"restore", "--latest"},
			wantBackend: app.BackendTarball,
			wantLatest:  true,
		},
		{
			name:        "tarball: --latest --s3 is accepted",
			args:        []string{"restore", "--latest", "--s3"},
			wantBackend: app.BackendTarball,
			wantLatest:  true,
			wantS3:      true,
		},
		{
			// FR-002: nothing runs when the command line asks for both.
			name:        "tarball: --latest with an archive is refused",
			args:        []string{"restore", "--latest", "infrahub_backup_20260804_120000.tar.gz"},
			errContains: []string{"mutually exclusive"},
		},
		{
			name:        "tarball: --s3 without --latest is refused",
			args:        []string{"restore", "--s3"},
			errContains: []string{"--s3", "requires it", "s3://bucket/key"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root, record := shippedRestoreCommand(t, tc.env)
			root.SetArgs(tc.args)

			err := root.Execute()

			if len(tc.errContains) > 0 {
				if err == nil {
					t.Fatalf("Execute(%v) = nil, want a refusal", tc.args)
				}
				for _, want := range tc.errContains {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error = %q, want it to contain %q", err, want)
					}
				}
				if record.ran {
					t.Error("the restore ran, want the invocation refused before any restore work")
				}
				return
			}

			if err != nil {
				t.Fatalf("Execute(%v) = %v, want nil", tc.args, err)
			}
			if !record.ran {
				t.Fatalf("Execute(%v) accepted nothing, want the restore to proceed", tc.args)
			}
			if record.backend != tc.wantBackend {
				t.Errorf("backend = %q, want %q: the invocation was judged against the wrong backend", record.backend, tc.wantBackend)
			}
			if strings.Join(record.args, ",") != strings.Join(tc.wantArgs, ",") {
				t.Errorf("args = %v, want %v", record.args, tc.wantArgs)
			}
			if record.latest != tc.wantLatest {
				t.Errorf("--latest = %v, want %v", record.latest, tc.wantLatest)
			}
			if record.s3 != tc.wantS3 {
				t.Errorf("--s3 = %v, want %v", record.s3, tc.wantS3)
			}
		})
	}
}
