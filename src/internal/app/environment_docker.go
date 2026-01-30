package app

import (
	"fmt"
	"sort"
	"strings"
)

type DockerBackend struct {
	config   *Configuration
	executor *CommandExecutor
	project  string
}

func NewDockerBackend(config *Configuration, executor *CommandExecutor) *DockerBackend {
	return &DockerBackend{config: config, executor: executor}
}

func (d *DockerBackend) Name() string {
	return "docker"
}

func (d *DockerBackend) Info() string {
	return d.project
}

func (d *DockerBackend) Detect() error {
	if err := d.executor.runCommandQuiet("docker", "--version"); err != nil {
		// If user explicitly specified Docker project, this is a hard error
		if d.config.DockerComposeProject != "" {
			return fmt.Errorf("docker CLI not available (required for --project): %w", err)
		}
		// Otherwise, treat as soft failure for auto-detection
		return fmt.Errorf("docker CLI not available: %w", ErrCLIUnavailable)
	}

	projects, err := ListDockerProjects(d.executor)
	if err != nil {
		return err
	}

	if d.config.DockerComposeProject != "" {
		project := d.config.DockerComposeProject
		if !contains(projects, project) {
			if _, err := d.executor.runCommand("docker", "compose", "-p", project, "ps"); err != nil {
				return fmt.Errorf("docker compose project %s not found: %w", project, err)
			}
		}
		d.project = project
		return nil
	}

	switch len(projects) {
	case 0:
		return ErrEnvironmentNotFound
	case 1:
		d.project = projects[0]
		d.config.DockerComposeProject = d.project
		return nil
	default:
		return fmt.Errorf("multiple docker compose projects found: %s (specify --project)", strings.Join(projects, ", "))
	}
}

func (d *DockerBackend) composeArgs(args ...string) []string {
	cmd := []string{"compose"}
	if d.project != "" {
		cmd = append(cmd, "-p", d.project)
	}
	cmd = append(cmd, args...)
	return cmd
}

func (d *DockerBackend) Exec(service string, command []string, opts *ExecOptions) (string, error) {
	args := []string{"exec", "-T"}
	if opts != nil {
		if opts.User != "" {
			args = append(args, "-u", opts.User)
		}
		if len(opts.Env) > 0 {
			keys := make([]string, 0, len(opts.Env))
			for k := range opts.Env {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, key := range keys {
				args = append(args, "-e", fmt.Sprintf("%s=%s", key, opts.Env[key]))
			}
		}
	}
	args = append(args, service)
	args = append(args, command...)
	full := d.composeArgs(args...)
	return d.executor.runCommand("docker", full...)
}

func (d *DockerBackend) ExecStream(service string, command []string, opts *ExecOptions) (string, error) {
	args := []string{"exec", "-T"}
	if opts != nil {
		if opts.User != "" {
			args = append(args, "-u", opts.User)
		}
		if len(opts.Env) > 0 {
			keys := make([]string, 0, len(opts.Env))
			for k := range opts.Env {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, key := range keys {
				args = append(args, "-e", fmt.Sprintf("%s=%s", key, opts.Env[key]))
			}
		}
	}
	args = append(args, service)
	args = append(args, command...)
	full := d.composeArgs(args...)
	return d.executor.runCommandWithStream("docker", full...)
}

func (d *DockerBackend) CopyTo(service, src, dest string) error {
	target := fmt.Sprintf("%s:%s", service, dest)
	cmd := d.composeArgs("cp", "-a", src, target)
	if _, err := d.executor.runCommand("docker", cmd...); err != nil {
		return err
	}
	return nil
}

func (d *DockerBackend) CopyFrom(service, src, dest string) error {
	source := fmt.Sprintf("%s:%s", service, src)
	cmd := d.composeArgs("cp", source, dest)
	if _, err := d.executor.runCommand("docker", cmd...); err != nil {
		return err
	}
	return nil
}

func (d *DockerBackend) Start(services ...string) error {
	if len(services) == 0 {
		return nil
	}
	args := append([]string{"start"}, services...)
	cmd := d.composeArgs(args...)
	_, err := d.executor.runCommand("docker", cmd...)
	return err
}

func (d *DockerBackend) Stop(services ...string) error {
	if len(services) == 0 {
		return nil
	}
	args := append([]string{"stop"}, services...)
	cmd := d.composeArgs(args...)
	_, err := d.executor.runCommand("docker", cmd...)
	return err
}

func (d *DockerBackend) IsRunning(service string) (bool, error) {
	cmd := d.composeArgs("ps", service)
	output, err := d.executor.runCommand("docker", cmd...)
	if err != nil {
		return false, err
	}
	return strings.Contains(output, "Up"), nil
}

func ListDockerProjects(executor *CommandExecutor) ([]string, error) {
	output, err := executor.runCommand("docker", "compose", "ls")
	if err != nil {
		return nil, fmt.Errorf("failed to list docker compose projects: %w", err)
	}

	projects := []string{}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(strings.ToUpper(line), "NAME ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		project := fields[0]
		if project == "" {
			continue
		}
		psOutput, err := executor.runCommand("docker", "compose", "-p", project, "ps", "-a")
		if err != nil {
			continue
		}
		if strings.Contains(strings.ToLower(psOutput), "infrahub") {
			projects = append(projects, project)
		}
	}

	sort.Strings(projects)
	projects = unique(projects)
	return projects, nil
}
