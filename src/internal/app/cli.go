package app

import (
	"fmt"
	"strings"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

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

	cobra.OnInitialize(func() {
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

		switch viper.GetString("log-format") {
		case "json":
			logrus.SetFormatter(&logrus.JSONFormatter{})
		default:
			logrus.SetFormatter(&logrus.TextFormatter{FullTimestamp: true})
		}
	})
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
		Short: "Detect the active deployment environment",
		RunE: func(cmd *cobra.Command, args []string) error {
			return app.DetectEnvironment()
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
