package app

import (
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

	bind := func(name string) {
		if err := viper.BindPFlag(name, cmd.PersistentFlags().Lookup(name)); err != nil {
			panic(err)
		}
	}

	bind("project")
	bind("backup-dir")
	bind("k8s-namespace")
	bind("log-format")

	cobra.OnInitialize(func() {
		viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
		viper.AutomaticEnv()
		viper.SetEnvPrefix("INFRAHUB")

		if viper.IsSet("project") {
			cfg.DockerComposeProject = viper.GetString("project")
		}
		if viper.IsSet("backup-dir") {
			cfg.BackupDir = viper.GetString("backup-dir")
		}
		if viper.IsSet("k8s-namespace") {
			cfg.K8sNamespace = viper.GetString("k8s-namespace")
		}

		switch viper.GetString("log-format") {
		case "json":
			logrus.SetFormatter(&logrus.JSONFormatter{})
		default:
			logrus.SetFormatter(&logrus.TextFormatter{FullTimestamp: true})
		}
	})
}
