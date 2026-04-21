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

	var force bool
	var redact bool
	var neo4jMetadata string
	var excludeTaskManagerDB bool
	var encrypt bool
	var encryptKey string
	var restoreExcludeTaskManagerDB bool
	var restoreMigrateFormat bool
	var restoreResetDeploymentID bool
	var restoreDecryptKey string
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
		Args: func(cmd *cobra.Command, args []string) error {
			if iops.Config().Backend == app.BackendPlakar {
				return nil // positional arg not required for plakar
			}
			if len(args) != 1 {
				return fmt.Errorf("requires exactly 1 arg(s), only received %d", len(args))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateBackendFlags(iops); err != nil {
				return err
			}
			forceRestore, _ := cmd.Flags().GetBool("force")
			if iops.Config().Backend == app.BackendPlakar {
				return iops.RestoreBackup("", restoreExcludeTaskManagerDB, restoreMigrateFormat, restoreSleepDuration, restoreDecryptKey, forceRestore, restoreResetDeploymentID)
			}
			return iops.RestoreBackup(args[0], restoreExcludeTaskManagerDB, restoreMigrateFormat, restoreSleepDuration, restoreDecryptKey, forceRestore, restoreResetDeploymentID)
		},
	}
	restoreCmd.Flags().BoolVar(&restoreExcludeTaskManagerDB, "exclude-taskmanager", false, "Skip restoring the task manager database even if present in the archive")
	restoreCmd.Flags().BoolVar(&restoreMigrateFormat, "migrate-format", false, "Run neo4j-admin database migrate --to-format=block after the restore completes")
	restoreCmd.Flags().DurationVar(&restoreSleepDuration, "sleep", 0, "Sleep duration before restore begins (e.g., 5m, 300s) for manual file transfer")
	restoreCmd.Flags().StringVar(&restoreDecryptKey, "decrypt-key", "", "Path to private key PEM file for decrypting an encrypted backup")
	restoreCmd.Flags().Bool("force", false, "Force restore of incomplete backup group")
	restoreCmd.Flags().BoolVar(&restoreResetDeploymentID, "reset-deployment-id", false, "Generate a new Root node UUID after restore to detach this instance from the source deployment ID")
	viper.BindPFlag("decrypt-key", restoreCmd.Flags().Lookup("decrypt-key"))
	viper.BindPFlag("reset-deployment-id", restoreCmd.Flags().Lookup("reset-deployment-id"))

	rootCmd.AddCommand(createCmd)
	rootCmd.AddCommand(restoreCmd)

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
			jsonOutput := viper.GetString("log-format") == "json"
			return iops.ListSnapshots(jsonOutput)
		},
	}

	snapshotsCmd.AddCommand(snapshotsListCmd)
	rootCmd.AddCommand(snapshotsCmd)

	if err := rootCmd.Execute(); err != nil {
		logrus.Errorf("Command failed: %v", err)
		os.Exit(1)
	}
}
