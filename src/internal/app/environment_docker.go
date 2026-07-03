package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
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

// buildExecArgs constructs the docker compose exec arguments for a service command.
func (d *DockerBackend) buildExecArgs(service string, command []string, opts *ExecOptions) []string {
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
	return d.composeArgs(args...)
}

func (d *DockerBackend) Exec(service string, command []string, opts *ExecOptions) (string, error) {
	return d.executor.runCommand("docker", d.buildExecArgs(service, command, opts)...)
}

func (d *DockerBackend) ExecStream(service string, command []string, opts *ExecOptions) (string, error) {
	return d.executor.runCommandWithStream("docker", d.buildExecArgs(service, command, opts)...)
}

// DockerBackend implements the collect-side primitives (spec 003-collect-tool,
// research R3/R4) and the optional timeout-bounded and per-replica exec
// capabilities the bundle diagnostics collectors prefer (research R2).
var (
	_ collectBackend = (*DockerBackend)(nil)
	_ contextExecer  = (*DockerBackend)(nil)
	_ replicaExecer  = (*DockerBackend)(nil)
)

// ExecContext is the timeout-bounded variant of Exec used by the bundle
// collectors (research R2: 60s per status dump).
func (d *DockerBackend) ExecContext(ctx context.Context, timeout time.Duration, service string, command []string, opts *ExecOptions) (string, error) {
	return d.executor.runCommandContext(ctx, timeout, "docker", d.buildExecArgs(service, command, opts)...)
}

// ExecReplica executes a command in one specific container by name, unlike
// Exec which targets a compose service (and therefore one arbitrary replica of
// a scaled service). The bundle collectors use it to capture per-replica
// task-worker state.
func (d *DockerBackend) ExecReplica(ctx context.Context, timeout time.Duration, replica Replica, command []string) (string, error) {
	args := append([]string{"exec", replica.Container}, command...)
	return d.executor.runCommandContext(ctx, timeout, "docker", args...)
}

// composePSContainer is the subset of one `docker compose ps --format json`
// entry the collect primitives consume.
type composePSContainer struct {
	Name    string `json:"Name"`
	Service string `json:"Service"`
	State   string `json:"State"`
	Labels  string `json:"Labels"`
}

// parseComposePSContainers decodes `docker compose ps --format json` output.
// Recent Compose releases emit NDJSON (one object per line); older ones emit a
// single JSON array — both are accepted. One-off containers created by
// `docker compose run` are excluded, and container names are normalized
// without the leading slash some Docker APIs prepend.
func parseComposePSContainers(output string) ([]composePSContainer, error) {
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		return []composePSContainer{}, nil
	}

	containers := []composePSContainer{}
	if strings.HasPrefix(trimmed, "[") {
		if err := json.Unmarshal([]byte(trimmed), &containers); err != nil {
			return nil, fmt.Errorf("failed to parse docker compose ps output: %w", err)
		}
	} else {
		for _, line := range nonEmptyLines(trimmed) {
			var container composePSContainer
			if err := json.Unmarshal([]byte(line), &container); err != nil {
				return nil, fmt.Errorf("failed to parse docker compose ps line %q: %w", line, err)
			}
			containers = append(containers, container)
		}
	}

	kept := make([]composePSContainer, 0, len(containers))
	for _, container := range containers {
		if strings.Contains(container.Labels, "com.docker.compose.oneoff=True") {
			continue
		}
		container.Name = strings.TrimPrefix(container.Name, "/")
		kept = append(kept, container)
	}
	return kept, nil
}

// replicasFromComposePS converts compose ps entries for one service into
// Replicas sorted by container name: every replica of a scaled service
// appears, each carrying its human-meaningful container name (critique P2).
// Pod stays empty and Restarted false — previous logs do not exist on Docker.
func replicasFromComposePS(service string, containers []composePSContainer) []Replica {
	replicas := []Replica{}
	for _, container := range containers {
		if container.Service != service || container.Name == "" {
			continue
		}
		replicas = append(replicas, Replica{Service: service, Container: container.Name})
	}
	sort.Slice(replicas, func(i, j int) bool {
		return replicas[i].Container < replicas[j].Container
	})
	return replicas
}

// composePSContainers runs `docker compose ps --format json` (optionally
// including stopped containers) and parses the entries. stdout is read through
// the pipe primitive so compose warnings on stderr never pollute the JSON
// stream.
func (d *DockerBackend) composePSContainers(all bool, services ...string) ([]composePSContainer, error) {
	psArgs := []string{"ps", "--format", "json"}
	if all {
		psArgs = append(psArgs, "-a")
	}
	psArgs = append(psArgs, services...)

	reader, wait, err := d.executor.runCommandPipeContext(context.Background(), collectExecTimeout, "docker", d.composeArgs(psArgs...)...)
	if err != nil {
		return nil, fmt.Errorf("failed to run docker compose ps: %w", err)
	}
	data, readErr := io.ReadAll(reader)
	if waitErr := wait(); waitErr != nil {
		return nil, fmt.Errorf("docker compose ps failed: %w", waitErr)
	}
	if readErr != nil {
		return nil, fmt.Errorf("failed to read docker compose ps output: %w", readErr)
	}
	return parseComposePSContainers(string(data))
}

// ServiceReplicas enumerates the project's containers for one compose service.
// Stopped containers are included (`ps -a`): a deployed-but-stopped service
// must surface as a failed collector with its logs still collected, not vanish
// as skipped (FR-009). A service with no containers is a valid empty
// enumeration so callers record it as skipped.
func (d *DockerBackend) ServiceReplicas(service string) ([]Replica, error) {
	containers, err := d.composePSContainers(true, service)
	if err != nil {
		return nil, fmt.Errorf("failed to list %s containers: %w", service, err)
	}
	return replicasFromComposePS(service, containers), nil
}

// dockerLogArgs builds the docker logs arguments for one replica.
func dockerLogArgs(replica Replica, tailLines int) []string {
	return []string{"logs", "--tail", strconv.Itoa(tailLines), replica.Container}
}

// ReplicaLogs streams one container's logs via docker logs, bounded by the
// collect transfer timeout (research R2). The daemon demuxes container output
// onto stdout and stderr — Infrahub services log to stderr — so both streams
// are merged into the returned reader. Previous-container logs do not exist on
// Docker (Restarted is always false), so previous must never be requested.
func (d *DockerBackend) ReplicaLogs(replica Replica, tailLines int, previous bool) (io.ReadCloser, func() error, error) {
	if previous {
		return nil, nil, fmt.Errorf("previous container logs are not available on docker")
	}
	return d.executor.runCommandCombinedPipeContext(context.Background(), collectTransferTimeout, "docker", dockerLogArgs(replica, tailLines)...)
}

// Metrics captures one-shot resource statistics for the project's running
// containers via `docker stats --no-stream` (research R4). Container names are
// part of the default stats columns, so support can map rows to replicas.
// Stopped containers are excluded — docker stats errors on them.
func (d *DockerBackend) Metrics() (string, error) {
	containers, err := d.composePSContainers(false)
	if err != nil {
		return "", fmt.Errorf("failed to enumerate project containers: %w", err)
	}

	names := []string{}
	for _, container := range containers {
		if container.Name != "" {
			names = append(names, container.Name)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "", fmt.Errorf("no running containers found for project %s", d.project)
	}

	args := append([]string{"stats", "--no-stream"}, names...)
	output, err := d.executor.runCommandContext(context.Background(), collectExecTimeout, "docker", args...)
	if err != nil {
		if output != "" {
			return "", fmt.Errorf("docker stats failed: %w: %s", err, output)
		}
		return "", fmt.Errorf("docker stats failed: %w", err)
	}
	return output, nil
}

func (d *DockerBackend) ExecStreamPipe(service string, command []string, opts *ExecOptions) (io.ReadCloser, func() error, error) {
	return d.executor.runCommandPipe("docker", d.buildExecArgs(service, command, opts)...)
}

func (d *DockerBackend) ExecWritePipe(service string, command []string, opts *ExecOptions, stdin io.Reader) (func() error, error) {
	return d.executor.runCommandWritePipe(stdin, "docker", d.buildExecArgs(service, command, opts)...)
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
