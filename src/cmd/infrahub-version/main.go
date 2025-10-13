package main

import (
	"os"

	app "infrahub-ops/src/internal/app"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

func main() {
	iops := app.NewInfrahubOps()
	rootCmd := &cobra.Command{
		Use:   "infrahub-version",
		Short: "Print Infrahub Ops CLI build information",
		Run: func(cmd *cobra.Command, args []string) {
			logrus.Infof("Version: %s", app.BuildRevision())
		},
	}

	app.ConfigureRootCommand(rootCmd, iops)

	if err := rootCmd.Execute(); err != nil {
		logrus.Errorf("Command failed: %v", err)
		os.Exit(1)
	}
}
