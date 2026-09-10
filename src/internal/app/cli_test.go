package app

import (
	"io"
	"os"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// externalDBEnvVars are the environment variables that can reach the external
// database configuration, including the two that must not (FR-027). Every test
// starts with all of them absent, so a value in the developer's or CI runner's
// environment cannot decide the outcome.
var externalDBEnvVars = []string{
	"INFRAHUB_NEO4J_ADDRESS",
	"INFRAHUB_NEO4J_BACKUP_PORT",
	"INFRAHUB_POSTGRES_ADDRESS",
	"INFRAHUB_EXTERNAL_DB_SCRATCH_SIZE",
	"INFRAHUB_EXTERNAL_DB_TIMEOUT",
	"INFRAHUB_EXTERNAL_DB_IMAGE_NEO4J",
	"INFRAHUB_EXTERNAL_DB_IMAGE_POSTGRES",
	"INFRAHUB_EXTERNAL_DB_IMAGE_PULL_SECRETS",
	"INFRAHUB_EXTERNAL_DB_SERVICE_ACCOUNT",
	"INFRAHUB_EXTERNAL_DB_NODE_SELECTOR",
	"INFRAHUB_EXTERNAL_DB_TOLERATIONS",
	"INFRAHUB_EXTERNAL_DB_PRIORITY_CLASS",
	AllowExternalRestoreEnvVar,
}

// newConfiguredRootCommand wires a root command exactly as a binary's main does
// and hands back both it and the app it resolves into, without executing it —
// which is what lets a test observe the configuration before and after the
// resolution runs. viper is global, so each call resets it and restores it
// afterwards.
func newConfiguredRootCommand(t *testing.T, env map[string]string) (*cobra.Command, *InfrahubOps) {
	t.Helper()

	viper.Reset()
	t.Cleanup(viper.Reset)

	for _, name := range externalDBEnvVars {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unsetting %s = %v, want nil", name, err)
		}
	}
	for name, value := range env {
		t.Setenv(name, value)
	}

	iops := NewInfrahubOps()
	cmd := &cobra.Command{Use: "root", RunE: func(*cobra.Command, []string) error { return nil }}
	ConfigureRootCommand(cmd, iops)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)

	return cmd, iops
}

// configureRootCommand wires a root command exactly as a binary's main does,
// parses the given command line, and returns the configuration the flags and
// environment resolved to.
func configureRootCommand(t *testing.T, env map[string]string, args ...string) *Configuration {
	t.Helper()

	cmd, iops := newConfiguredRootCommand(t, env)
	cmd.SetArgs(args)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("executing %v = %v, want nil", args, err)
	}

	return iops.Config()
}

// TestResolveConfigurationIsCallableBeforeArgumentValidation covers the ordering
// T123 turned on. Cobra validates a command's arguments before it runs any
// persistent pre-run, so a command whose argument contract reads a resolved
// value — `restore`, whose arity rule differs per backend — cannot wait for the
// hook and has to ask for the resolution by name.
func TestResolveConfigurationIsCallableBeforeArgumentValidation(t *testing.T) {
	cmd, iops := newConfiguredRootCommand(t, map[string]string{"INFRAHUB_NEO4J_ADDRESS": "early.example.internal"})

	if got := iops.Config().ExternalDB.Neo4jAddress; got != "" {
		t.Fatalf("Neo4jAddress = %q before anything ran, want the unresolved default", got)
	}

	if err := iops.ResolveConfiguration(); err != nil {
		t.Fatalf("ResolveConfiguration() = %v, want nil", err)
	}
	if got := iops.Config().ExternalDB.Neo4jAddress; got != "early.example.internal" {
		t.Errorf("Neo4jAddress = %q after ResolveConfiguration, want the configured value: nothing that runs before the persistent pre-run can read the configuration", got)
	}

	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("executing the command afterwards = %v, want nil", err)
	}
	if got := iops.Config().ExternalDB.Neo4jAddress; got != "early.example.internal" {
		t.Errorf("Neo4jAddress = %q after the command ran, want the configured value", got)
	}
}

// TestResolveConfigurationRunsOnce pins what makes calling the resolution early
// safe: the second and later calls read nothing again. The body is otherwise
// repeat-safe, but it warns about a malformed timeout and a malformed
// authorisation, and an operator must not be told twice about one mistake.
//
// The second call is given a different environment, so a resolution that ran
// again would be visible in the value it produced.
func TestResolveConfigurationRunsOnce(t *testing.T) {
	cmd, iops := newConfiguredRootCommand(t, map[string]string{"INFRAHUB_NEO4J_ADDRESS": "first.example.internal"})

	if err := iops.ResolveConfiguration(); err != nil {
		t.Fatalf("ResolveConfiguration() = %v, want nil", err)
	}

	t.Setenv("INFRAHUB_NEO4J_ADDRESS", "second.example.internal")

	if err := iops.ResolveConfiguration(); err != nil {
		t.Fatalf("second ResolveConfiguration() = %v, want nil", err)
	}
	if got := iops.Config().ExternalDB.Neo4jAddress; got != "first.example.internal" {
		t.Errorf("Neo4jAddress = %q after a second ResolveConfiguration, want the first call's value: the resolution ran twice", got)
	}

	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("executing the command afterwards = %v, want nil", err)
	}
	if got := iops.Config().ExternalDB.Neo4jAddress; got != "first.example.internal" {
		t.Errorf("Neo4jAddress = %q after the persistent pre-run, want the first call's value: the pre-run's call must be the no-op", got)
	}
}

// TestResolveConfigurationWithoutARootCommand covers the app every test in this
// package builds without configuring a command: it has nothing to resolve and
// keeps the defaults NewInfrahubOps gave it.
func TestResolveConfigurationWithoutARootCommand(t *testing.T) {
	iops := NewInfrahubOps()

	if err := iops.ResolveConfiguration(); err != nil {
		t.Fatalf("ResolveConfiguration() = %v, want nil", err)
	}
	if got := iops.Config().Backend; got != BackendTarball {
		t.Errorf("Backend = %q, want %q", got, BackendTarball)
	}
}

// TestConfigureRootCommandLeavesNoResolutionBehind is the pollution that made
// this file's own cases interfere with each other.
//
// Registering the resolution through cobra.OnInitialize put a closure holding
// this call's Configuration into package-global state nothing can unregister
// from, so every later Execute in the process ran it again — against whatever
// environment that later case had set, writing into a Configuration the earlier
// case was still holding.
func TestConfigureRootCommandLeavesNoResolutionBehind(t *testing.T) {
	first := configureRootCommand(t, map[string]string{"INFRAHUB_NEO4J_ADDRESS": "first.example.internal"})
	if first.ExternalDB.Neo4jAddress != "first.example.internal" {
		t.Fatalf("Neo4jAddress = %q, want the value this command was given", first.ExternalDB.Neo4jAddress)
	}

	configureRootCommand(t, map[string]string{"INFRAHUB_NEO4J_ADDRESS": "second.example.internal"})

	if first.ExternalDB.Neo4jAddress != "first.example.internal" {
		t.Errorf("the first command's Neo4jAddress became %q when a second command ran: its resolution outlived the command it was registered for", first.ExternalDB.Neo4jAddress)
	}
}

// TestNoSubcommandShadowsTheRootPersistentPreRun protects the one property the
// move to a per-command hook depends on. Cobra runs only the first persistent
// pre-run it finds walking up from the executed command, so a subcommand that
// defined one of its own would silently take the whole of ConfigureRootCommand's
// resolution with it — every flag and environment variable would read as unset.
//
// It walks the tree this package builds. A binary's own subcommands are
// constructed inside its main, out of reach from here, so this is the shared
// subtree rather than the whole of any binary's.
func TestNoSubcommandShadowsTheRootPersistentPreRun(t *testing.T) {
	root := &cobra.Command{Use: "root", RunE: func(*cobra.Command, []string) error { return nil }}
	iops := NewInfrahubOps()
	ConfigureRootCommand(root, iops)
	AttachEnvironmentCommands(root, iops)
	t.Cleanup(viper.Reset)

	if root.PersistentPreRunE == nil {
		t.Fatal("ConfigureRootCommand registered no persistent pre-run, so nothing resolves the configuration")
	}

	var walk func(cmd *cobra.Command)
	walk = func(cmd *cobra.Command) {
		for _, child := range cmd.Commands() {
			if child.PersistentPreRunE != nil || child.PersistentPreRun != nil {
				t.Errorf("%q defines a persistent pre-run, which shadows the root's and drops the whole configuration resolution; fold it into the root's hook or set cobra.EnableTraverseRunHooks", child.CommandPath())
			}
			walk(child)
		}
	}
	walk(root)
}

// TestExternalDBFlagDefaults pins the defaults an internal-only deployment sees.
// They are what makes the whole surface inert for it (FR-015): the two values
// discovery cannot supply have defaults, and nothing else is set.
func TestExternalDBFlagDefaults(t *testing.T) {
	cfg := configureRootCommand(t, nil)

	if cfg.ExternalDB.Neo4jBackupPort != defaultNeo4jBackupPort {
		t.Errorf("Neo4jBackupPort = %d, want %d", cfg.ExternalDB.Neo4jBackupPort, defaultNeo4jBackupPort)
	}
	if cfg.ExternalDB.Timeout != defaultExternalDBTimeout {
		t.Errorf("Timeout = %v, want %v", cfg.ExternalDB.Timeout, defaultExternalDBTimeout)
	}
	if cfg.ExternalDB.ImageNeo4j != defaultExternalDBImageNeo4j {
		t.Errorf("ImageNeo4j = %q, want %q", cfg.ExternalDB.ImageNeo4j, defaultExternalDBImageNeo4j)
	}
	if cfg.ExternalDB.ImagePostgres != defaultExternalDBImagePostgres {
		t.Errorf("ImagePostgres = %q, want %q", cfg.ExternalDB.ImagePostgres, defaultExternalDBImagePostgres)
	}
	if cfg.ExternalDB.Neo4jAddress != "" || cfg.ExternalDB.PostgresAddress != "" {
		t.Errorf("addresses = %q/%q, want both empty", cfg.ExternalDB.Neo4jAddress, cfg.ExternalDB.PostgresAddress)
	}
	if cfg.ExternalDB.ScratchSize != "" {
		t.Errorf("ScratchSize = %q, want it empty", cfg.ExternalDB.ScratchSize)
	}
	if cfg.ExternalDB.Restore != (ExternalRestoreAuth{}) {
		t.Errorf("Restore = %+v, want the zero value (unauthorised)", cfg.ExternalDB.Restore)
	}
}

// TestExternalDBFlagsFromCommandLine asserts each flag reaches the field it
// configures. The address overrides are asserted together because FR-001
// requires overriding one to leave the other's resolution alone.
func TestExternalDBFlagsFromCommandLine(t *testing.T) {
	cfg := configureRootCommand(t, nil,
		"--neo4j-address", "db-a:7688,db-b",
		"--neo4j-backup-port", "7000",
		"--postgres-address", "pg.example.net:5433",
		"--external-db-scratch-size", "80Gi",
		"--external-db-timeout", "45m",
		"--external-db-image-neo4j", "registry.example.net/neo4j:5.26.0-enterprise",
		"--external-db-image-postgres", "registry.example.net/postgres:18-alpine",
	)

	tests := []struct {
		field string
		got   string
		want  string
	}{
		{"Neo4jAddress", cfg.ExternalDB.Neo4jAddress, "db-a:7688,db-b"},
		{"PostgresAddress", cfg.ExternalDB.PostgresAddress, "pg.example.net:5433"},
		{"ScratchSize", cfg.ExternalDB.ScratchSize, "80Gi"},
		{"ImageNeo4j", cfg.ExternalDB.ImageNeo4j, "registry.example.net/neo4j:5.26.0-enterprise"},
		{"ImagePostgres", cfg.ExternalDB.ImagePostgres, "registry.example.net/postgres:18-alpine"},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s = %q, want %q", tt.field, tt.got, tt.want)
		}
	}

	if cfg.ExternalDB.Neo4jBackupPort != 7000 {
		t.Errorf("Neo4jBackupPort = %d, want 7000", cfg.ExternalDB.Neo4jBackupPort)
	}
	if cfg.ExternalDB.Timeout != 45*time.Minute {
		t.Errorf("Timeout = %v, want 45m", cfg.ExternalDB.Timeout)
	}
}

// TestExternalDBFlagsFromEnvironment asserts the flags follow the existing
// INFRAHUB_-prefixed binding convention, which is what makes the feature usable
// from a scheduled job's configuration.
func TestExternalDBFlagsFromEnvironment(t *testing.T) {
	cfg := configureRootCommand(t, map[string]string{
		"INFRAHUB_NEO4J_ADDRESS":            "db.example.net",
		"INFRAHUB_NEO4J_BACKUP_PORT":        "7001",
		"INFRAHUB_POSTGRES_ADDRESS":         "pg.example.net",
		"INFRAHUB_EXTERNAL_DB_SCRATCH_SIZE": "120Gi",
		"INFRAHUB_EXTERNAL_DB_TIMEOUT":      "90m",
	})

	if cfg.ExternalDB.Neo4jAddress != "db.example.net" {
		t.Errorf("Neo4jAddress = %q, want %q", cfg.ExternalDB.Neo4jAddress, "db.example.net")
	}
	if cfg.ExternalDB.Neo4jBackupPort != 7001 {
		t.Errorf("Neo4jBackupPort = %d, want 7001", cfg.ExternalDB.Neo4jBackupPort)
	}
	if cfg.ExternalDB.PostgresAddress != "pg.example.net" {
		t.Errorf("PostgresAddress = %q, want %q", cfg.ExternalDB.PostgresAddress, "pg.example.net")
	}
	if cfg.ExternalDB.ScratchSize != "120Gi" {
		t.Errorf("ScratchSize = %q, want %q", cfg.ExternalDB.ScratchSize, "120Gi")
	}
	if cfg.ExternalDB.Timeout != 90*time.Minute {
		t.Errorf("Timeout = %v, want 90m", cfg.ExternalDB.Timeout)
	}
}

// TestExternalDBImagesIgnoreEnvironment is FR-027's flag-layer guarantee: the
// tool holds workload-creation rights in the customer's namespace, so the image
// it runs there resolves from the pinned default or an explicit flag and never
// from ambient environment. The image flags break the binding convention every
// other flag follows, which is exactly why it is asserted rather than assumed.
func TestExternalDBImagesIgnoreEnvironment(t *testing.T) {
	env := map[string]string{
		"INFRAHUB_EXTERNAL_DB_IMAGE_NEO4J":    "attacker.example.net/anything:latest",
		"INFRAHUB_EXTERNAL_DB_IMAGE_POSTGRES": "attacker.example.net/anything:latest",
	}

	t.Run("environment alone leaves the pinned defaults", func(t *testing.T) {
		cfg := configureRootCommand(t, env)

		if cfg.ExternalDB.ImageNeo4j != defaultExternalDBImageNeo4j {
			t.Errorf("ImageNeo4j = %q, want the pinned %q", cfg.ExternalDB.ImageNeo4j, defaultExternalDBImageNeo4j)
		}
		if cfg.ExternalDB.ImagePostgres != defaultExternalDBImagePostgres {
			t.Errorf("ImagePostgres = %q, want the pinned %q", cfg.ExternalDB.ImagePostgres, defaultExternalDBImagePostgres)
		}
	})

	t.Run("an explicit flag still decides", func(t *testing.T) {
		cfg := configureRootCommand(t, env,
			"--external-db-image-neo4j", "registry.example.net/neo4j:5.26.0-enterprise",
		)

		if cfg.ExternalDB.ImageNeo4j != "registry.example.net/neo4j:5.26.0-enterprise" {
			t.Errorf("ImageNeo4j = %q, want the flag's value", cfg.ExternalDB.ImageNeo4j)
		}
		if cfg.ExternalDB.ImagePostgres != defaultExternalDBImagePostgres {
			t.Errorf("ImagePostgres = %q, want the pinned %q", cfg.ExternalDB.ImagePostgres, defaultExternalDBImagePostgres)
		}
	})
}

// TestExternalDBSchedulingFlags is T111 at the flag layer. The image flags are
// unusable on their own: an operator who mirrors the tooling image into their
// own registry — the reason those flags exist — has a registry that needs
// credentials, and a pod that can name no pull secret waits in ImagePullBackOff
// until readiness gives up.
//
// Unlike the image flags these follow the binding convention, and that is
// asserted rather than assumed: an unattended backup is configured by
// environment as often as by argv, and none of these can run code — they say
// where a pod may be placed and which credentials fetch its image, while the
// image itself stays pinned or flag-only.
func TestExternalDBSchedulingFlags(t *testing.T) {
	t.Run("nothing is configured by default", func(t *testing.T) {
		cfg := configureRootCommand(t, nil)

		if cfg.ExternalDB.Scheduling != (ExternalDBScheduling{}) {
			t.Errorf("Scheduling = %+v, want the zero value", cfg.ExternalDB.Scheduling)
		}
	})

	t.Run("from the command line", func(t *testing.T) {
		cfg := configureRootCommand(t, nil,
			"--external-db-image-pull-secrets", "mirror-creds,backup-mirror-creds",
			"--external-db-service-account", "infrahub-backup",
			"--external-db-node-selector", "node-role=backup",
			"--external-db-tolerations", "dedicated=backup:NoSchedule",
			"--external-db-priority-class", "backup-critical",
		)

		want := ExternalDBScheduling{
			ImagePullSecrets: "mirror-creds,backup-mirror-creds",
			ServiceAccount:   "infrahub-backup",
			NodeSelector:     "node-role=backup",
			Tolerations:      "dedicated=backup:NoSchedule",
			PriorityClass:    "backup-critical",
		}
		if cfg.ExternalDB.Scheduling != want {
			t.Errorf("Scheduling = %+v, want %+v", cfg.ExternalDB.Scheduling, want)
		}
	})

	t.Run("from the environment", func(t *testing.T) {
		cfg := configureRootCommand(t, map[string]string{
			"INFRAHUB_EXTERNAL_DB_IMAGE_PULL_SECRETS": "mirror-creds",
			"INFRAHUB_EXTERNAL_DB_SERVICE_ACCOUNT":    "infrahub-backup",
			"INFRAHUB_EXTERNAL_DB_NODE_SELECTOR":      "node-role=backup,disk=ssd",
			"INFRAHUB_EXTERNAL_DB_TOLERATIONS":        "dedicated:NoExecute",
			"INFRAHUB_EXTERNAL_DB_PRIORITY_CLASS":     "backup-critical",
		})

		want := ExternalDBScheduling{
			ImagePullSecrets: "mirror-creds",
			ServiceAccount:   "infrahub-backup",
			NodeSelector:     "node-role=backup,disk=ssd",
			Tolerations:      "dedicated:NoExecute",
			PriorityClass:    "backup-critical",
		}
		if cfg.ExternalDB.Scheduling != want {
			t.Errorf("Scheduling = %+v, want %+v", cfg.ExternalDB.Scheduling, want)
		}
	})
}

// TestExternalRestoreAuthorisationChannels covers FR-010 end to end at this
// layer: the configuration channel authorises an unattended restore, the flag
// channel is recorded as the interactive one, and each is distinguishable from
// the other in the resolved configuration.
func TestExternalRestoreAuthorisationChannels(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		args []string
		want ExternalRestoreAuth
	}{
		{
			name: "absent",
			want: ExternalRestoreAuth{},
		},
		{
			name: "configuration",
			env:  map[string]string{AllowExternalRestoreEnvVar: "true"},
			want: ExternalRestoreAuth{Source: ExternalRestoreAuthConfig},
		},
		{
			name: "flag",
			args: []string{"--" + AllowExternalRestoreFlag},
			want: ExternalRestoreAuth{Source: ExternalRestoreAuthFlag},
		},
		{
			name: "flag refuses what configuration permits",
			env:  map[string]string{AllowExternalRestoreEnvVar: "true"},
			args: []string{"--" + AllowExternalRestoreFlag + "=false"},
			want: ExternalRestoreAuth{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := configureRootCommand(t, tt.env, tt.args...)

			if cfg.ExternalDB.Restore != tt.want {
				t.Errorf("Restore = %+v, want %+v", cfg.ExternalDB.Restore, tt.want)
			}
		})
	}
}

// TestResolveExternalRestoreAuth covers the resolution rules directly, including
// the values a command line cannot produce. The refusals matter more than the
// authorisations: this is the only gate in front of overwriting a database the
// deployment does not manage (FR-009).
func TestResolveExternalRestoreAuth(t *testing.T) {
	tests := []struct {
		name      string
		flagValue bool
		flagSet   bool
		envValue  string
		want      ExternalRestoreAuth
	}{
		{
			name: "nothing supplied",
			want: ExternalRestoreAuth{},
		},
		{
			name:      "flag set",
			flagValue: true,
			flagSet:   true,
			want:      ExternalRestoreAuth{Source: ExternalRestoreAuthFlag},
		},
		{
			name:    "flag explicitly false",
			flagSet: true,
			want:    ExternalRestoreAuth{},
		},
		{
			name:      "flag wins over the environment",
			flagValue: true,
			flagSet:   true,
			envValue:  "false",
			want:      ExternalRestoreAuth{Source: ExternalRestoreAuthFlag},
		},
		{
			name:     "environment true",
			envValue: "true",
			want:     ExternalRestoreAuth{Source: ExternalRestoreAuthConfig},
		},
		{
			name:     "environment 1",
			envValue: "1",
			want:     ExternalRestoreAuth{Source: ExternalRestoreAuthConfig},
		},
		{
			name:     "environment false",
			envValue: "false",
			want:     ExternalRestoreAuth{},
		},
		{
			name:     "environment empty",
			envValue: "",
			want:     ExternalRestoreAuth{},
		},
		{
			name:     "environment malformed",
			envValue: "ture",
			want:     ExternalRestoreAuth{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveExternalRestoreAuth(tt.flagValue, tt.flagSet, tt.envValue)

			if got != tt.want {
				t.Errorf("resolveExternalRestoreAuth(%t, %t, %q) = %+v, want %+v",
					tt.flagValue, tt.flagSet, tt.envValue, got, tt.want)
			}
		})
	}
}
