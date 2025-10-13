package main

import (
	"fmt"
	"os"

	app "infrahub-ops/src/internal/app"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

func main() {
	iops := app.NewInfrahubOps()
	rootCmd := &cobra.Command{
		Use:   "infrahub-environment",
		Short: "Environment detection and management for Infrahub",
		Long:  "Detect and list Infrahub deployment environments across Docker and Kubernetes.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}

	app.ConfigureRootCommand(rootCmd, iops)

	detectCmd := &cobra.Command{
		Use:   "detect",
		Short: "Detect the active deployment environment",
		RunE: func(cmd *cobra.Command, args []string) error {
			return iops.DetectEnvironment()
		},
	}

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List available Infrahub deployment targets",
		RunE: func(cmd *cobra.Command, args []string) error {
			executor := app.NewCommandExecutor()
			dockerProjects, _ := app.ListDockerProjects(executor)
			k8sNamespaces, _ := app.ListKubernetesNamespaces(executor)

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

	rootCmd.AddCommand(detectCmd)
	rootCmd.AddCommand(listCmd)

	if err := rootCmd.Execute(); err != nil {
		logrus.Errorf("Command failed: %v", err)
		os.Exit(1)
	}
}
