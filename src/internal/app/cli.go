package app

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"infrahub-ops/src/internal/updater"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

const (
	// AllowExternalRestoreFlag is the interactive form of the authorisation to
	// restore into a database the deployment does not manage (FR-009).
	AllowExternalRestoreFlag = "allow-external-restore"
	// AllowExternalRestoreEnvVar is its configuration form, and the only channel
	// through which an unattended restore can be authorised (FR-010).
	AllowExternalRestoreEnvVar = "INFRAHUB_ALLOW_EXTERNAL_RESTORE"

	// The endpoint overrides FR-004 requires for a deployment whose own
	// configuration does not expose where its database is. Named because the
	// refusal an undiscoverable endpoint produces has to state the flag that
	// supplies it (see undiscoverableEndpoint), and a message naming a flag this
	// command never registered is worse than no message at all.
	neo4jAddressFlag    = "neo4j-address"
	postgresAddressFlag = "postgres-address"

	// externalDBTimeoutFlag bounds a single operation against an external
	// database, and externalDBTimeoutEnvVar is its configuration form — the one
	// an unattended run has (FR-010). Named once because
	// resolveExternalDBTimeout has to tell the operator which of the two
	// carried the value it refused.
	externalDBTimeoutFlag   = "external-db-timeout"
	externalDBTimeoutEnvVar = "INFRAHUB_EXTERNAL_DB_TIMEOUT"
)

// resolveExternalRestoreAuth decides whether an external restore is authorised
// and which channel authorised it. The channel matters on its own: FR-010 allows
// an unattended restore only where the authorisation came from configuration,
// which is a distinction the boolean alone cannot carry.
//
// An explicit flag wins over the environment, in either direction, so an
// operator can refuse on the command line what a scheduled job's configuration
// permits. A malformed environment value authorises nothing: the safe reading of
// an unparseable authorisation is that none was given, and it is logged so the
// refusal that follows is explainable.
func resolveExternalRestoreAuth(flagValue, flagSet bool, envValue string) ExternalRestoreAuth {
	if flagSet {
		if !flagValue {
			return ExternalRestoreAuth{}
		}
		return ExternalRestoreAuth{Source: ExternalRestoreAuthFlag}
	}

	// Unset and set-but-empty are the same absence of an authorisation.
	if envValue == "" {
		return ExternalRestoreAuth{}
	}

	allowed, err := strconv.ParseBool(envValue)
	if err != nil {
		logrus.Warnf("%s is not a boolean (%q): external restore stays unauthorised", AllowExternalRestoreEnvVar, envValue)
		return ExternalRestoreAuth{}
	}
	if !allowed {
		return ExternalRestoreAuth{}
	}

	return ExternalRestoreAuth{Source: ExternalRestoreAuthConfig}
}

// ConfigureRootCommand wires shared flags, environment variables, and logging for CLI binaries.
func ConfigureRootCommand(cmd *cobra.Command, app *InfrahubOps) {
	cfg := app.Config()

	cmd.PersistentFlags().StringVar(&cfg.DockerComposeProject, "project", cfg.DockerComposeProject, "Target specific Docker Compose project")
	cmd.PersistentFlags().StringVar(&cfg.BackupDir, "backup-dir", cfg.BackupDir, "Backup directory")
	cmd.PersistentFlags().StringVar(&cfg.K8sNamespace, "k8s-namespace", cfg.K8sNamespace, "Target Kubernetes namespace")
	cmd.PersistentFlags().String("log-format", "text", "Log output format: text or json (can also set INFRAHUB_LOG_FORMAT)")

	// Plakar backend flags
	cmd.PersistentFlags().String("backend", string(BackendTarball), "Backup backend: tarball or plakar")
	cmd.PersistentFlags().StringVar(&cfg.Plakar.RepoPath, "repo", cfg.Plakar.RepoPath, "Plakar repository path or URI (required when backend=plakar)")

	// Plakar restore flags
	cmd.PersistentFlags().String("backup-id", "", "Plakar backup group ID to restore (latest complete if empty)")
	cmd.PersistentFlags().String("snapshot", "", "Plakar snapshot ID for single-component restore")

	// S3 configuration flags
	cmd.PersistentFlags().StringVar(&cfg.S3.Bucket, "s3-bucket", cfg.S3.Bucket, "S3 bucket name for backup storage")
	cmd.PersistentFlags().StringVar(&cfg.S3.Prefix, "s3-prefix", cfg.S3.Prefix, "S3 key prefix (path within bucket)")
	cmd.PersistentFlags().StringVar(&cfg.S3.Endpoint, "s3-endpoint", cfg.S3.Endpoint, "Custom S3 endpoint URL (for MinIO or S3-compatible storage)")
	cmd.PersistentFlags().StringVar(&cfg.S3.Region, "s3-region", cfg.S3.Region, "AWS region for S3 bucket")

	// External database flags. Every one of them is inert for a deployment whose
	// databases are all internal (FR-015): each either overrides a value
	// discovery supplies or applies only once a database has been positively
	// established as external.
	//
	// The plain-string settings among them are registered, bound to viper and
	// read back from this one table, so a setting cannot be registered without
	// being bound, or bound without being read — three steps that used to be
	// spelled out per flag and checked by nothing.
	externalDBStrings := []struct {
		name  string
		dst   *string
		usage string
	}{
		{neo4jAddressFlag, &cfg.ExternalDB.Neo4jAddress, "Override the Neo4j host list as host[:port][,host[:port]...] (default: discovered from the deployment)"},
		{postgresAddressFlag, &cfg.ExternalDB.PostgresAddress, "Override the PostgreSQL endpoint as host[:port] (default: discovered from the deployment)"},
		{"external-db-scratch-size", &cfg.ExternalDB.ScratchSize, "Scratch space for the transient backup workload, e.g. 50Gi (default: derived from the database size)"},
		// What the operator's cluster requires of a pod it did not author.
		// These are what make the image flags below usable: mirroring the
		// tooling image into a private registry — the reason those flags exist
		// — needs the pull secret for it to be nameable, or the pod waits in
		// ImagePullBackOff until the readiness bound gives up. Same for a
		// namespace whose capable nodes are tainted, or which admits pods only
		// under a named service account.
		//
		// Unlike the image flags, these *are* bound to viper. FR-027's reason
		// for keeping the image out of ambient environment is that the tool
		// holds workload-creation rights and an environment-settable image
		// turns those into arbitrary code execution in the customer's
		// namespace. None of these can do that while the image stays pinned:
		// they say where a pod may run and which credentials fetch its image,
		// not what it runs. The pod still mounts no service-account token, so
		// naming an account grants it no API access either. And an unattended
		// backup is configured by environment as often as by argv, so making
		// these flag-only would leave a scheduled run in a mirrored-registry
		// cluster with no way to say so.
		{"external-db-image-pull-secrets", &cfg.ExternalDB.Scheduling.ImagePullSecrets, "Comma-separated secrets in the deployment's namespace holding registry credentials for the transient workload's image"},
		{"external-db-service-account", &cfg.ExternalDB.Scheduling.ServiceAccount, "Service account the transient workload runs under (it still mounts no service-account token)"},
		{"external-db-node-selector", &cfg.ExternalDB.Scheduling.NodeSelector, "Comma-separated key=value node labels the transient workload must be scheduled onto"},
		{"external-db-tolerations", &cfg.ExternalDB.Scheduling.Tolerations, "Comma-separated taints the transient workload tolerates, written as key=value:Effect, key:Effect or a bare key"},
		{"external-db-priority-class", &cfg.ExternalDB.Scheduling.PriorityClass, "PriorityClass the transient workload is admitted under"},
	}
	for _, setting := range externalDBStrings {
		cmd.PersistentFlags().StringVar(setting.dst, setting.name, *setting.dst, setting.usage)
	}
	cmd.PersistentFlags().IntVar(&cfg.ExternalDB.Neo4jBackupPort, "neo4j-backup-port", cfg.ExternalDB.Neo4jBackupPort, "Port the Neo4j backup service listens on (not the Bolt port)")
	// The only way certificate verification is skipped for a connection this
	// tool opens. A deployment that runs Infrahub with tls_insecure does not
	// grant it: FR-029 requires the opt-out to be the operator's own decision
	// about this tool, made here, rather than one inherited from a setting that
	// was made about something else.
	//
	// This replaces --external-db-tls-policy, which T095 removed for having no
	// consumer. The channel that landed with the capture path does not take a
	// policy name: cypher-shell's verification is chosen by the Bolt URI scheme
	// and libpq's by PGSSLMODE, and both are two-valued here — verify, or the
	// operator's opt-out. A string naming a policy would have had no value to
	// carry that this does not (see the CLI-surface contract).
	cmd.PersistentFlags().BoolVar(&cfg.ExternalDB.InsecureTLS, insecureTLSFlagName, cfg.ExternalDB.InsecureTLS, "Do not protect connections to an external database server: skip certificate verification, and accept an unencrypted channel where the deployment records TLS as disabled (a discovered insecure-TLS setting does not imply this)")
	cmd.PersistentFlags().DurationVar(&cfg.ExternalDB.Timeout, externalDBTimeoutFlag, cfg.ExternalDB.Timeout, "Bound on any single operation against an external database (e.g. 30m, 2h)")
	// Registered without a flag variable on purpose: resolveExternalRestoreAuth
	// needs to know which channel supplied the authorisation, and reading the
	// flag through a variable loses that.
	cmd.PersistentFlags().Bool(AllowExternalRestoreFlag, false, "Authorise a destructive restore into a database the deployment does not manage (distinct from --force)")

	// The image flags are deliberately not bound to viper below, so they resolve
	// from their pinned default or an explicitly supplied flag and never from
	// ambient environment (FR-027): the tool holds workload-creation rights in
	// the deployment, and an environment-settable image would turn those rights
	// into a means of running an arbitrary image in the customer's namespace.
	cmd.PersistentFlags().StringVar(&cfg.ExternalDB.ImageNeo4j, "external-db-image-neo4j", cfg.ExternalDB.ImageNeo4j, "Image supplying the Neo4j administration tooling for the transient workload")
	cmd.PersistentFlags().StringVar(&cfg.ExternalDB.ImagePostgres, "external-db-image-postgres", cfg.ExternalDB.ImagePostgres, "Image supplying the PostgreSQL client tooling for the transient workload")

	bind := func(name string) {
		if err := viper.BindPFlag(name, cmd.PersistentFlags().Lookup(name)); err != nil {
			panic(err)
		}
	}

	bind("project")
	bind("backup-dir")
	bind("k8s-namespace")
	bind("log-format")
	bind("backend")
	bind("repo")
	bind("backup-id")
	bind("snapshot")
	bind("s3-bucket")
	bind("s3-prefix")
	bind("s3-endpoint")
	bind("s3-region")
	for _, setting := range externalDBStrings {
		bind(setting.name)
	}
	bind("neo4j-backup-port")
	bind(insecureTLSFlagName)
	bind(externalDBTimeoutFlag)
	// --allow-external-restore is deliberately not bound: viper would report the
	// flag and INFRAHUB_ALLOW_EXTERNAL_RESTORE through one key, and FR-010 turns
	// on telling them apart, so resolveExternalRestoreAuth reads each channel
	// itself.
	// --external-db-image-neo4j and --external-db-image-postgres are not bound;
	// see FR-027 at their registration above.

	// The resolution hangs on the command it configures rather than going
	// through cobra.OnInitialize.
	//
	// cobra.OnInitialize appends to a package-global slice that nothing can
	// unregister from, and the closure captures this call's Configuration. A
	// binary calls this once and never notices. The tests call it once per
	// case, so every case ran every earlier case's resolution as well — against
	// its own environment, writing into Configuration values those earlier
	// cases were still holding — and the work grew with the square of the case
	// count. A per-command hook is registered on the command and goes when it
	// does.
	//
	// Only the first persistent pre-run found walking up from the executed
	// command runs, unless cobra.EnableTraverseRunHooks is set: a subcommand
	// that defined one of its own would silently take the whole of this
	// resolution with it. Nothing under these roots defines one, and
	// TestNoSubcommandShadowsTheRootPersistentPreRun is what keeps that true.
	//
	// The resolution is also held as a value, reachable through
	// app.ResolveConfiguration, because cobra validates a command's arguments
	// *before* it runs any persistent pre-run. A command whose argument
	// contract depends on a resolved value — `restore`, whose arity rule
	// differs per backend — cannot wait for the hook, so it asks for the
	// resolution by name and the hook's later call is the no-op. Running it at
	// most once is what makes that safe: the body is otherwise repeat-safe, but
	// its two warnings about malformed configuration would be emitted twice.
	resolve := func() error {
		viper.SetEnvPrefix("INFRAHUB")
		viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
		viper.AutomaticEnv()

		if viper.IsSet("project") {
			cfg.DockerComposeProject = viper.GetString("project")
		}
		if viper.IsSet("backup-dir") {
			cfg.BackupDir = viper.GetString("backup-dir")
		}
		if viper.IsSet("k8s-namespace") {
			cfg.K8sNamespace = viper.GetString("k8s-namespace")
		}
		if viper.IsSet("backend") {
			cfg.Backend = BackendType(viper.GetString("backend"))
		}
		if viper.IsSet("repo") {
			cfg.Plakar.RepoPath = viper.GetString("repo")
		}
		if viper.IsSet("backup-id") {
			cfg.Plakar.BackupID = viper.GetString("backup-id")
		}
		if viper.IsSet("snapshot") {
			cfg.Plakar.SnapshotID = viper.GetString("snapshot")
		}
		if viper.IsSet("s3-bucket") {
			cfg.S3.Bucket = viper.GetString("s3-bucket")
		}
		if viper.IsSet("s3-prefix") {
			cfg.S3.Prefix = viper.GetString("s3-prefix")
		}
		if viper.IsSet("s3-endpoint") {
			cfg.S3.Endpoint = viper.GetString("s3-endpoint")
		}
		if viper.IsSet("s3-region") {
			cfg.S3.Region = viper.GetString("s3-region")
		}
		for _, setting := range externalDBStrings {
			if viper.IsSet(setting.name) {
				*setting.dst = viper.GetString(setting.name)
			}
		}
		if viper.IsSet("neo4j-backup-port") {
			cfg.ExternalDB.Neo4jBackupPort = viper.GetInt("neo4j-backup-port")
		}
		if viper.IsSet(insecureTLSFlagName) {
			cfg.ExternalDB.InsecureTLS = viper.GetBool(insecureTLSFlagName)
		}
		// Read as a string and parsed here rather than through
		// viper.GetDuration, which reads a unit-less value as nanoseconds: see
		// resolveExternalDBTimeout. A value it refuses leaves the bound as it
		// was, and says so.
		if viper.IsSet(externalDBTimeoutFlag) {
			bound, err := resolveExternalDBTimeout(viper.GetString(externalDBTimeoutFlag))
			switch {
			case err != nil:
				logrus.Warn(err)
			case bound > 0:
				cfg.ExternalDB.Timeout = bound
			}
		}

		flagValue, _ := cmd.PersistentFlags().GetBool(AllowExternalRestoreFlag)
		cfg.ExternalDB.Restore = resolveExternalRestoreAuth(
			flagValue,
			cmd.PersistentFlags().Changed(AllowExternalRestoreFlag),
			os.Getenv(AllowExternalRestoreEnvVar),
		)

		switch viper.GetString("log-format") {
		case "json":
			logrus.SetFormatter(&logrus.JSONFormatter{})
		default:
			logrus.SetFormatter(&logrus.TextFormatter{FullTimestamp: true})
		}

		return nil
	}

	var once sync.Once
	var resolveErr error
	app.resolveConfiguration = func() error {
		once.Do(func() { resolveErr = resolve() })
		return resolveErr
	}

	cmd.PersistentPreRunE = func(*cobra.Command, []string) error {
		return app.ResolveConfiguration()
	}
}

// ResolveConfiguration resolves this app's configuration from the flags and
// environment of the root command ConfigureRootCommand was given, if it has not
// been resolved already, and reports what that resolution refused.
//
// It exists for the one caller that cannot wait for the persistent pre-run:
// cobra validates arguments before running it, so a command whose argument
// contract reads a resolved value has to ask for the resolution itself. Calling
// it is idempotent — the second and later calls return the first call's result
// without re-reading anything — so the pre-run's own call stays correct for
// every command that does not need it early.
//
// An app whose root command was never configured has nothing to resolve and
// keeps the defaults NewInfrahubOps gave it.
func (iops *InfrahubOps) ResolveConfiguration() error {
	if iops.resolveConfiguration == nil {
		return nil
	}
	return iops.resolveConfiguration()
}

// AttachEnvironmentCommands wires the environment detection subcommands onto a root command.
func AttachEnvironmentCommands(rootCmd *cobra.Command, app *InfrahubOps) {
	envCmd := &cobra.Command{
		Use:   "environment",
		Short: "Environment detection and management",
		Long:  "Detect and list Infrahub deployment environments across Docker and Kubernetes.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}

	detectCmd := &cobra.Command{
		Use:   "detect",
		Short: "Detect the active deployment environment and where each database lives",
		RunE: func(cmd *cobra.Command, args []string) error {
			// ReportEnvironment rather than DetectEnvironment: this is the
			// command an operator reaches for when a backup fails, so it
			// reports where each database was resolved to as well as which
			// deployment was found (US2 scenario 5).
			return app.ReportEnvironment()
		},
	}

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List available Infrahub deployment targets",
		RunE: func(cmd *cobra.Command, args []string) error {
			executor := NewCommandExecutor()
			dockerProjects, _ := ListDockerProjects(executor)
			k8sNamespaces, _ := ListKubernetesNamespaces(executor)

			if len(dockerProjects) == 0 && len(k8sNamespaces) == 0 {
				logrus.Info("No Infrahub deployments detected")
				return nil
			}

			if len(dockerProjects) > 0 {
				logrus.Info("Docker Compose projects:")
				for _, project := range dockerProjects {
					fmt.Printf("  %s\n", project)
				}
			}

			if len(k8sNamespaces) > 0 {
				logrus.Info("Kubernetes namespaces:")
				for _, ns := range k8sNamespaces {
					fmt.Printf("  %s\n", ns)
				}
			}

			return nil
		},
	}

	envCmd.AddCommand(detectCmd)
	envCmd.AddCommand(listCmd)
	rootCmd.AddCommand(envCmd)
}

// AttachUpdateCommand wires the `update` self-update subcommand onto a root
// command. binaryName is the name of the invoked binary (e.g. "infrahub-backup")
// and is used to select the matching release asset.
func AttachUpdateCommand(rootCmd *cobra.Command, binaryName string) {
	var check bool
	var assumeYes bool
	var targetVersion string

	updateCmd := &cobra.Command{
		Use:   "update",
		Short: "Update this binary to the latest release",
		Long: "Download and install the latest release of " + binaryName + " in place of the running binary.\n" +
			"Use --check to see whether an update is available without installing it.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if check && assumeYes {
				return fmt.Errorf("--check and --yes cannot be used together")
			}

			opts := updater.Options{
				BinaryName:     binaryName,
				CurrentVersion: BuildRevision(),
				TargetVersion:  targetVersion,
			}

			if check {
				return runUpdateCheck(cmd.Context(), opts)
			}
			return runUpdate(cmd.Context(), opts, assumeYes)
		},
	}

	updateCmd.Flags().BoolVar(&check, "check", false, "Report whether an update is available without installing it")
	updateCmd.Flags().BoolVarP(&assumeYes, "yes", "y", false, "Skip the confirmation prompt (required for non-interactive use)")
	updateCmd.Flags().StringVar(&targetVersion, "version", "", "Install a specific release version (e.g. v1.7.2) instead of the latest")

	rootCmd.AddCommand(updateCmd)
}

func runUpdateCheck(ctx context.Context, opts updater.Options) error {
	if ctx == nil {
		ctx = context.Background()
	}
	res, err := updater.Check(ctx, opts)
	if err != nil {
		return err
	}
	switch res.Action {
	case updater.ActionAvailable:
		logrus.Infof("update available: %s → %s (%s)", res.FromVersion, res.ToVersion, res.DetailsURL)
	case updater.ActionAlreadyCurrent:
		logrus.Infof("already up to date (%s)", res.FromVersion)
	case updater.ActionRefused:
		logrus.Infof("update check: %s", res.RefusedReason)
	}
	return nil
}

func runUpdate(ctx context.Context, opts updater.Options, assumeYes bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	proceed := func(from, to string) (bool, error) {
		return updater.Proceed(assumeYes, from, to)
	}

	res, err := updater.Update(ctx, opts, proceed)
	if err != nil {
		return err
	}
	switch res.Action {
	case updater.ActionUpdated:
		logrus.Infof("updated %s → %s", res.FromVersion, res.ToVersion)
	case updater.ActionAlreadyCurrent:
		logrus.Infof("already up to date (%s)", res.FromVersion)
	case updater.ActionRefused:
		if res.RefusedReason == "update cancelled" {
			logrus.Info("update cancelled")
			return nil
		}
		return fmt.Errorf("%s", res.RefusedReason)
	}
	return nil
}
