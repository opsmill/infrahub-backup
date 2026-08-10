package main

import (
	"fmt"
	"os"
	"time"

	app "infrahub-ops/src/internal/app"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// validateBackendFlags checks for invalid flag combinations related to the --backend flag.
func validateBackendFlags(iops *app.InfrahubOps) error {
	cfg := iops.Config()

	// Validate backend value
	switch cfg.Backend {
	case app.BackendTarball, app.BackendPlakar:
		// valid
	default:
		return fmt.Errorf("unknown backend: %s, expected 'tarball' or 'plakar'", cfg.Backend)
	}

	if cfg.Backend == app.BackendPlakar {
		// --repo is required for plakar backend
		if cfg.Plakar.RepoPath == "" {
			return fmt.Errorf("--repo is required when using plakar backend")
		}

		// S3 flags conflict with plakar backend
		if viper.GetBool("s3-upload") || cfg.S3.Bucket != "" || cfg.S3.Prefix != "" ||
			cfg.S3.Endpoint != "" || (cfg.S3.Region != "" && cfg.S3.Region != "us-east-1") {
			return fmt.Errorf("--s3-upload and related S3 flags cannot be used with plakar backend; use --repo s3://... instead")
		}
	}

	return nil
}

// restoreRequest is the restore one command line asks for, once the invocation has been
// validated. It is what turns the flag combinations below into a single delegation
// decision, so the routing cannot disagree with the validation that allowed it.
type restoreRequest struct {
	// Archive is the archive named on the command line, empty when none was. The plakar
	// backend's own latest-group resolution is reached with an empty Archive too, which
	// is why --latest needs no separate route there.
	Archive string
	// Latest routes the run through app.RestoreLatestBackup, which chooses the archive.
	// It is never set for the plakar backend: that backend's no-argument restore already
	// resolves the latest complete backup group, so --latest is an alias for it (FR-004).
	Latest bool
	// S3 selects the configured S3 bucket/prefix as the pool --latest chooses from,
	// instead of the local backup directory. It is only ever set alongside Latest.
	S3 bool
}

// resolveRestoreInvocation validates one `restore` command line and reports the restore
// it asks for. It is the whole of this command's argument contract, in one place and
// with no side effects, so an invocation that cannot mean what it says is rejected
// before anything is listed, downloaded, or stopped.
//
// The refusals are deliberate rather than best-guess resolutions: this command
// overwrites a deployment's data, so an ambiguous command line must fail rather than
// pick one of the two things it could have meant. Bare `restore` keeps failing on the
// tarball backend for the same reason — a typo must not become a data-overwriting
// default — with the error now naming --latest as the way to restore without a filename
// (FR-003).
func resolveRestoreInvocation(backend app.BackendType, args []string, latest, s3 bool) (restoreRequest, error) {
	if latest && len(args) > 0 {
		return restoreRequest{}, fmt.Errorf("--latest and a named archive are mutually exclusive: pass either --latest or %s, not both", args[0])
	}

	if s3 && !latest {
		return restoreRequest{}, fmt.Errorf("--s3 selects the pool --latest chooses from and requires it; to restore one exact remote archive, pass its s3://bucket/key URI as the argument instead")
	}

	if backend == app.BackendPlakar {
		if s3 {
			return restoreRequest{}, fmt.Errorf("--s3 does not apply to the %s backend: the repository location comes from --repo, and a plakar restore already resolves the latest complete backup group", app.BackendPlakar)
		}

		// --latest is what this backend has always done without an argument, so it takes
		// the same route rather than a parallel one that could drift from it (FR-004).
		return restoreRequest{}, nil
	}

	if len(args) != 1 && !latest {
		return restoreRequest{}, fmt.Errorf("requires exactly 1 arg(s), only received %d: pass the backup archive to restore, or --latest to restore the most recent backup without naming one", len(args))
	}

	if latest {
		return restoreRequest{Latest: true, S3: s3}, nil
	}

	return restoreRequest{Archive: args[0]}, nil
}

// restoreOptions are the `restore` options that resolve through viper, and therefore from
// the environment as well as from the command line.
type restoreOptions struct {
	// DecryptKey is the path to the private key PEM file an encrypted archive is decrypted
	// with, from --decrypt-key or INFRAHUB_DECRYPT_KEY.
	DecryptKey string
	// ResetDeploymentID generates a new Root node UUID after the restore, from
	// --reset-deployment-id or INFRAHUB_RESET_DEPLOYMENT_ID.
	ResetDeploymentID bool
}

// resolveRestoreOptions reads the two `restore` options bound to viper below, the way
// `create` reads its own: through viper, so the flag and the INFRAHUB_ variable are both
// live and the flag wins where both supply a value.
//
// Reading them from the flag variables instead is what made the bindings dead — the
// variables only ever hold what the command line supplied, so both environment variables
// silently did nothing. They matter to an unattended restore in particular: a scheduled job
// configured entirely through the environment could not pass a decryption key at all.
func resolveRestoreOptions() restoreOptions {
	return restoreOptions{
		DecryptKey:        viper.GetString("decrypt-key"),
		ResetDeploymentID: viper.GetBool("reset-deployment-id"),
	}
}

// retentionRuleInput reads one retention rule's raw configuration for the command
// being run: the invoked command's own flag, and the environment variable.
//
// Reading the command's own flag matters because `create` and `prune` register the
// same flag name, and viper resolves a bound flag through whichever command bound the
// key last. The environment variable is read directly rather than through viper
// because viper coerces a malformed value to 0, which means "rule inactive" — the one
// answer a mistyped variable must not produce. Deciding what these inputs mean belongs
// to app.ResolveRetentionConfig.
func retentionRuleInput(cmd *cobra.Command, flag, envVar string) (app.RetentionRuleInput, error) {
	input := app.RetentionRuleInput{}

	if cmd.Flags().Changed(flag) {
		value, err := cmd.Flags().GetInt(flag)
		if err != nil {
			return input, err
		}
		input.FlagValue, input.FlagSet = value, true
	}

	input.EnvValue, input.EnvSet = os.LookupEnv(envVar)

	return input, nil
}

// resolveRetentionFlags stores the resolved retention rules on the configuration so
// the operation can read them like any other option. It runs before any backup work so
// an invalid policy aborts the run without touching the deployment.
func resolveRetentionFlags(cmd *cobra.Command, iops *app.InfrahubOps) error {
	days, err := retentionRuleInput(cmd, app.RetentionDaysFlag, app.RetentionDaysEnvVar)
	if err != nil {
		return err
	}

	count, err := retentionRuleInput(cmd, app.RetentionCountFlag, app.RetentionCountEnvVar)
	if err != nil {
		return err
	}

	retention, err := app.ResolveRetentionConfig(app.RetentionInputs{Days: days, Count: count})
	if err != nil {
		return err
	}
	iops.Config().Retention = retention

	return nil
}

// version is set via ldflags at build time
var version string

func main() {
	app.SetVersion(version)
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
	app.AttachUpdateCommand(rootCmd, "infrahub-backup")

	var force bool
	var redact bool
	var neo4jMetadata string
	var excludeTaskManagerDB bool
	var encrypt bool
	var encryptKey string
	var restoreExcludeTaskManagerDB bool
	var restoreMigrateFormat bool
	var restoreLatest bool
	var restoreLatestFromS3 bool
	var s3Upload bool
	var s3KeepLocal bool
	var sleepDuration time.Duration
	var restoreSleepDuration time.Duration

	// Variables for from-files subcommand
	var neo4jPath string
	var postgresPath string
	var neo4jEdition string
	var infrahubVersion string
	var fromFilesEncrypt bool
	var fromFilesEncryptKey string

	createCmd := &cobra.Command{
		Use:          "create",
		Short:        "Create a backup of the current Infrahub instance",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateBackendFlags(iops); err != nil {
				return err
			}
			if err := resolveRetentionFlags(cmd, iops); err != nil {
				return err
			}
			// Plakar uses native symmetric (passphrase) encryption; reject the
			// tarball --encrypt-key flag and resolve/validate the passphrase
			// before any repository is created.
			if iops.Config().Backend == app.BackendPlakar {
				if err := iops.PreparePlakarEncryption(
					viper.GetBool("encrypt"),
					viper.GetString("encrypt-key"),
					viper.GetString("passphrase-file"),
				); err != nil {
					return err
				}
			}
			return iops.CreateBackup(
				viper.GetBool("force"),
				viper.GetString("neo4jmetadata"),
				viper.GetBool("exclude-taskmanager"),
				viper.GetBool("s3-upload"),
				viper.GetBool("s3-keep-local"),
				viper.GetDuration("sleep"),
				viper.GetBool("redact"),
				viper.GetBool("encrypt"),
				viper.GetString("encrypt-key"),
			)
		},
	}
	createCmd.Flags().BoolVar(&force, "force", false, "Force backup creation even if there are running tasks")
	createCmd.Flags().BoolVar(&redact, "redact", false, "Redact all attribute values in the database before backup (destructive, requires --force)")
	createCmd.Flags().StringVar(&neo4jMetadata, "neo4jmetadata", "all", "Whether to backup neo4j metadata or not (all, none, users, roles)")
	createCmd.Flags().BoolVar(&excludeTaskManagerDB, "exclude-taskmanager", false, "Exclude task manager database from the backup")
	createCmd.Flags().BoolVar(&s3Upload, "s3-upload", false, "Upload backup to S3 after creation")
	createCmd.Flags().BoolVar(&s3KeepLocal, "s3-keep-local", false, "Keep local backup file after successful S3 upload (default: delete local file)")
	createCmd.Flags().DurationVar(&sleepDuration, "sleep", 0, "Sleep duration after backup creation (e.g., 5m, 300s) for manual file transfer")
	createCmd.Flags().BoolVar(&encrypt, "encrypt", false, "Encrypt the backup archive (uses built-in OpsMill key unless --encrypt-key is set)")
	createCmd.Flags().StringVar(&encryptKey, "encrypt-key", "", "Path to custom public key file for encryption (implies --encrypt)")
	createCmd.Flags().Int(app.RetentionDaysFlag, 0, "Prune backups older than N days after a successful backup (N >= 1, omit to disable)")
	createCmd.Flags().Int(app.RetentionCountFlag, 0, "Keep only the N most recent backups after a successful backup (N >= 1, omit to disable)")

	// Bind create flags to Viper for environment variable support (INFRAHUB_<FLAG_NAME>)
	viper.BindPFlag("force", createCmd.Flags().Lookup("force"))
	viper.BindPFlag("redact", createCmd.Flags().Lookup("redact"))
	viper.BindPFlag("neo4jmetadata", createCmd.Flags().Lookup("neo4jmetadata"))
	viper.BindPFlag("exclude-taskmanager", createCmd.Flags().Lookup("exclude-taskmanager"))
	viper.BindPFlag("s3-upload", createCmd.Flags().Lookup("s3-upload"))
	viper.BindPFlag("s3-keep-local", createCmd.Flags().Lookup("s3-keep-local"))
	viper.BindPFlag("sleep", createCmd.Flags().Lookup("sleep"))
	viper.BindPFlag("encrypt", createCmd.Flags().Lookup("encrypt"))
	viper.BindPFlag("encrypt-key", createCmd.Flags().Lookup("encrypt-key"))
	// The retention flags are deliberately not bound: resolveRetentionFlags reads the
	// invoked command's own flag and INFRAHUB_RETENTION_* directly, because a bound key
	// resolves through whichever command bound it last and because viper coerces a
	// malformed environment value to 0, which reads as "rule inactive".

	// Undocumented subcommand: create from-files
	fromFilesCmd := &cobra.Command{
		Use:          "from-files",
		Short:        "Create a backup archive from local database dump files",
		Hidden:       true,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return iops.CreateBackupFromFiles(neo4jPath, postgresPath, neo4jEdition, infrahubVersion, fromFilesEncrypt, fromFilesEncryptKey)
		},
	}
	fromFilesCmd.Flags().StringVar(&neo4jPath, "neo4j-path", "", "Path to Neo4j backup directory or dump file (required)")
	fromFilesCmd.Flags().StringVar(&postgresPath, "postgres-path", "", "Path to PostgreSQL dump file (optional)")
	fromFilesCmd.Flags().StringVar(&neo4jEdition, "neo4j-edition", "", "Neo4j edition (enterprise or community, auto-detected if not specified)")
	fromFilesCmd.Flags().StringVar(&infrahubVersion, "infrahub-version", "", "Infrahub version to record in backup metadata")
	fromFilesCmd.Flags().BoolVar(&fromFilesEncrypt, "encrypt", false, "Encrypt the backup archive")
	fromFilesCmd.Flags().StringVar(&fromFilesEncryptKey, "encrypt-key", "", "Path to custom public key file for encryption (implies --encrypt)")
	fromFilesCmd.MarkFlagRequired("neo4j-path")

	createCmd.AddCommand(fromFilesCmd)

	restoreCmd := &cobra.Command{
		Use:          "restore [backup-file]",
		Short:        "Restore Infrahub from a backup archive",
		SilenceUsage: true,
		// Cobra parses flags before validating arguments, so the invocation contract —
		// which spans both — is enforced here, before RunE can act on it.
		Args: func(cmd *cobra.Command, args []string) error {
			_, err := resolveRestoreInvocation(iops.Config().Backend, args, restoreLatest, restoreLatestFromS3)
			return err
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateBackendFlags(iops); err != nil {
				return err
			}
			request, err := resolveRestoreInvocation(iops.Config().Backend, args, restoreLatest, restoreLatestFromS3)
			if err != nil {
				return err
			}
			// An encrypted repository cannot be opened without the passphrase, so it is
			// resolved once the invocation is known to be valid and before either restore
			// route runs — the repository is opened inside both. For this backend
			// resolveRestoreInvocation always reports an empty Archive, so the named-archive
			// route below is the one plakar takes, and its own latest-group resolution
			// handles choosing the snapshot.
			if iops.Config().Backend == app.BackendPlakar {
				if err := iops.LoadPlakarPassphrase(viper.GetString("passphrase-file")); err != nil {
					return err
				}
			}
			forceRestore, _ := cmd.Flags().GetBool("force")
			options := resolveRestoreOptions()

			if request.Latest {
				return iops.RestoreLatestBackup(request.S3, restoreExcludeTaskManagerDB, restoreMigrateFormat, restoreSleepDuration, options.DecryptKey, forceRestore, options.ResetDeploymentID)
			}
			return iops.RestoreBackup(request.Archive, restoreExcludeTaskManagerDB, restoreMigrateFormat, restoreSleepDuration, options.DecryptKey, forceRestore, options.ResetDeploymentID)
		},
	}
	restoreCmd.Flags().BoolVar(&restoreExcludeTaskManagerDB, "exclude-taskmanager", false, "Skip restoring the task manager database even if present in the archive")
	restoreCmd.Flags().BoolVar(&restoreMigrateFormat, "migrate-format", false, "Run neo4j-admin database migrate --to-format=block after the restore completes")
	restoreCmd.Flags().DurationVar(&restoreSleepDuration, "sleep", 0, "Sleep duration before restore begins (e.g., 5m, 300s) for manual file transfer")
	// Registered without a flag variable on purpose: resolveRestoreOptions reads these two
	// through viper, and a variable next to the binding is what let the two disagree.
	restoreCmd.Flags().String("decrypt-key", "", "Path to private key PEM file for decrypting an encrypted backup")
	restoreCmd.Flags().Bool("force", false, "Force restore of incomplete backup group")
	restoreCmd.Flags().Bool("reset-deployment-id", false, "Generate a new Root node UUID after restore to detach this instance from the source deployment ID")
	restoreCmd.Flags().BoolVar(&restoreLatest, "latest", false, "Restore the most recent backup instead of naming an archive (mutually exclusive with [backup-file])")
	restoreCmd.Flags().BoolVar(&restoreLatestFromS3, "s3", false, "With --latest: choose from the configured S3 bucket/prefix instead of the local backup directory")

	// Bind restore flags to Viper for environment variable support (INFRAHUB_<FLAG_NAME>).
	viper.BindPFlag("decrypt-key", restoreCmd.Flags().Lookup("decrypt-key"))
	viper.BindPFlag("reset-deployment-id", restoreCmd.Flags().Lookup("reset-deployment-id"))
	// The rest of restore's flags are deliberately not bound, and are read from their own
	// flag variables:
	//   --latest and --s3 are per-invocation switches: a persistent --latest would turn a
	//   mistyped `restore` into a data-overwriting default, and a persistent --s3 would
	//   silently move the pool for every restore.
	//   --exclude-taskmanager, --migrate-format, and --sleep share a name with a bound
	//   `create` flag, and viper resolves a key through whichever command bound it last, so
	//   binding them here would make each command answer with the other's value.
	//   INFRAHUB_SLEEP therefore configures `create` only.

	rootCmd.AddCommand(createCmd)
	rootCmd.AddCommand(restoreCmd)

	var pruneDryRun bool
	var pruneForce bool
	var pruneS3 bool

	pruneCmd := &cobra.Command{
		Use:          "prune",
		Short:        "Delete backups that fall outside the retention policy",
		Long:         "Apply a retention policy to existing backups without creating a new one.\n\nThe most recent backup at each location always survives. Without --force the candidates are listed and a single confirmation is asked; with --dry-run nothing is deleted. S3 objects are only ever considered when --s3 is passed.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// The Plakar backend is rejected by Prune itself with the reason that
			// matters here — retention is not implemented for it yet (FR-012) —
			// rather than by the repository/S3 checks a real Plakar run needs.
			if iops.Config().Backend != app.BackendPlakar {
				if err := validateBackendFlags(iops); err != nil {
					return err
				}
			}
			if err := resolveRetentionFlags(cmd, iops); err != nil {
				return err
			}

			return iops.Prune(iops.Config().Retention.Policy(), app.PruneOptions{
				DryRun: pruneDryRun,
				Force:  pruneForce,
				S3:     pruneS3,
			})
		},
	}
	pruneCmd.Flags().Int(app.RetentionDaysFlag, 0, "Delete backups older than N days (N >= 1)")
	pruneCmd.Flags().Int(app.RetentionCountFlag, 0, "Keep only the N most recent backups (N >= 1)")
	pruneCmd.Flags().BoolVar(&pruneDryRun, "dry-run", false, "List exactly what a real run would delete, delete nothing, and never prompt")
	pruneCmd.Flags().BoolVar(&pruneForce, "force", false, "Skip the confirmation prompt (for non-interactive and scripted use)")
	pruneCmd.Flags().BoolVar(&pruneS3, "s3", false, "Also prune backups under the configured S3 bucket/prefix (S3 is never touched without this flag)")

	// Like `create`'s, these retention flags are not bound to viper: resolveRetentionFlags
	// reads the invoked command's own flag and INFRAHUB_RETENTION_* directly, which keeps
	// FR-011 working on both commands. --dry-run, --force, and --s3 are per-invocation
	// switches and are intentionally not configurable at all (contracts/cli.md).

	rootCmd.AddCommand(pruneCmd)

	// Key generation command
	var keygenOutput string

	keygenCmd := &cobra.Command{
		Use:          "keygen",
		Short:        "Generate an ECIES keypair for backup encryption",
		Long:         "Generate a P-256 ECIES keypair. The private key is written to a PEM file. The public key is written to a .pub file and printed to stdout.",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			privPEM, pubB64, err := app.GenerateKeyPair()
			if err != nil {
				return fmt.Errorf("failed to generate keypair: %w", err)
			}

			if err := os.WriteFile(keygenOutput, privPEM, 0600); err != nil {
				return fmt.Errorf("failed to write private key: %w", err)
			}
			logrus.Infof("Private key written to: %s", keygenOutput)

			pubPath := keygenOutput + ".pub"
			if err := os.WriteFile(pubPath, []byte(pubB64+"\n"), 0644); err != nil {
				return fmt.Errorf("failed to write public key: %w", err)
			}
			logrus.Infof("Public key written to: %s", pubPath)

			fmt.Println(pubB64)

			return nil
		},
	}
	keygenCmd.Flags().StringVarP(&keygenOutput, "output", "o", "backup.key", "Output path for the private key PEM file (public key gets .pub suffix)")
	rootCmd.AddCommand(keygenCmd)

	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "Print Infrahub Ops CLI build information",
		Run: func(cmd *cobra.Command, args []string) {
			logrus.Infof("Version: %s", app.BuildRevision())
		},
	}

	rootCmd.AddCommand(versionCmd)

	// Snapshots subcommand
	snapshotsCmd := &cobra.Command{
		Use:   "snapshots",
		Short: "Manage Plakar backup snapshots",
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}

	snapshotsListCmd := &cobra.Command{
		Use:          "list",
		Short:        "List all snapshots in a Plakar repository",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if iops.Config().Plakar.RepoPath == "" {
				return fmt.Errorf("--repo is required for snapshots list")
			}
			if err := iops.LoadPlakarPassphrase(viper.GetString("passphrase-file")); err != nil {
				return err
			}
			jsonOutput := viper.GetString("log-format") == "json"
			return iops.ListSnapshots(jsonOutput)
		},
	}

	snapshotsCmd.AddCommand(snapshotsListCmd)
	rootCmd.AddCommand(snapshotsCmd)

	// Hidden worker invoked inside the co-located runner (Deliverable B).
	rootCmd.AddCommand(app.RunConnectorCommand())

	if err := rootCmd.Execute(); err != nil {
		logrus.Errorf("Command failed: %v", err)
		os.Exit(1)
	}
}
