package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

var ErrEnvironmentNotFound = errors.New("environment not found")

type ExecOptions struct {
	User string
	Env  map[string]string
}

type EnvironmentBackend interface {
	Name() string
	Detect() error
	Info() string
	Exec(service string, command []string, opts *ExecOptions) (string, error)
	ExecStream(service string, command []string, opts *ExecOptions) (string, error)
	CopyTo(service, src, dest string) error
	CopyFrom(service, src, dest string) error
	Start(services ...string) error
	Stop(services ...string) error
	IsRunning(service string) (bool, error)
}

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
		return fmt.Errorf("docker CLI not available: %w", err)
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

type KubernetesBackend struct {
	config    *Configuration
	executor  *CommandExecutor
	namespace string
	podCache  map[string]string
}

func NewKubernetesBackend(config *Configuration, executor *CommandExecutor) *KubernetesBackend {
	return &KubernetesBackend{config: config, executor: executor, podCache: map[string]string{}}
}

func (k *KubernetesBackend) Name() string {
	return "kubernetes"
}

func (k *KubernetesBackend) Info() string {
	return k.namespace
}

func (k *KubernetesBackend) Detect() error {
	if err := k.executor.runCommandQuiet("kubectl", "version", "--client"); err != nil {
		return fmt.Errorf("kubectl CLI not available: %w", err)
	}

	namespaces, err := ListKubernetesNamespaces(k.executor)
	if err != nil {
		return err
	}

	if k.config.K8sNamespace != "" {
		k.namespace = k.config.K8sNamespace
		if _, err := k.executor.runCommand("kubectl", "get", "pods", "-n", k.namespace, "-l", "app.kubernetes.io/name=infrahub"); err != nil {
			return fmt.Errorf("failed to verify namespace %s: %w", k.namespace, err)
		}
		return nil
	}

	switch len(namespaces) {
	case 0:
		return ErrEnvironmentNotFound
	case 1:
		k.namespace = namespaces[0]
		k.config.K8sNamespace = k.namespace
		return nil
	default:
		return fmt.Errorf("multiple kubernetes namespaces found: %s (set INFRAHUB_K8S_NAMESPACE)", strings.Join(namespaces, ", "))
	}
}

func (k *KubernetesBackend) Exec(service string, command []string, opts *ExecOptions) (string, error) {
	pod, err := k.getPodForService(service)
	if err != nil {
		return "", err
	}
	finalCmd := k.prepareCommand(command, opts)
	args := []string{"exec", "-n", k.namespace, pod, "--"}
	args = append(args, finalCmd...)
	return k.executor.runCommand("kubectl", args...)
}

func (k *KubernetesBackend) ExecStream(service string, command []string, opts *ExecOptions) (string, error) {
	pod, err := k.getPodForService(service)
	if err != nil {
		return "", err
	}
	finalCmd := k.prepareCommand(command, opts)
	args := []string{"exec", "-n", k.namespace, pod, "--"}
	args = append(args, finalCmd...)
	return k.executor.runCommandWithStream("kubectl", args...)
}

func (k *KubernetesBackend) CopyTo(service, src, dest string) error {
	pod, err := k.getPodForService(service)
	if err != nil {
		return err
	}
	target := fmt.Sprintf("%s/%s:%s", k.namespace, pod, dest)
	if _, err := k.executor.runCommand("kubectl", "cp", src, target); err != nil {
		return err
	}
	return nil
}

func (k *KubernetesBackend) CopyFrom(service, src, dest string) error {
	pod, err := k.getPodForService(service)
	if err != nil {
		return err
	}
	source := fmt.Sprintf("%s/%s:%s", k.namespace, pod, src)
	if _, err := k.executor.runCommand("kubectl", "cp", source, dest); err != nil {
		return err
	}
	return nil
}

func (k *KubernetesBackend) Start(services ...string) error {
	return k.scaleServices(services, 1)
}

func (k *KubernetesBackend) Stop(services ...string) error {
	return k.scaleServices(services, 0)
}

func (k *KubernetesBackend) IsRunning(service string) (bool, error) {
	pods, err := k.listPods(service)
	if err != nil {
		return false, err
	}
	for _, status := range pods {
		if strings.EqualFold(status, "Running") {
			return true, nil
		}
	}
	return false, nil
}

func (k *KubernetesBackend) scaleServices(services []string, replicas int) error {
	if len(services) == 0 {
		return nil
	}
	for _, service := range services {
		kind, resource, err := k.findWorkloadResource(service)
		if err != nil {
			return fmt.Errorf("failed to resolve workload for %s: %w", service, err)
		}
		if err := k.scaleResource(kind, resource, replicas); err != nil {
			return fmt.Errorf("failed to scale %s (%s/%s) to %d replicas: %w", service, kind, resource, replicas, err)
		}
	}
	k.podCache = map[string]string{}
	return nil
}

func (k *KubernetesBackend) scaleResource(kind, resource string, replicas int) error {
	_, err := k.executor.runCommand("kubectl", "scale", "-n", k.namespace, fmt.Sprintf("%s/%s", kind, resource), fmt.Sprintf("--replicas=%d", replicas))
	return err
}

func (k *KubernetesBackend) findWorkloadResource(service string) (string, string, error) {
	kinds := []string{"deployment", "statefulset"}
	selectors := k.podSelectors(service)

	for _, kind := range kinds {
		for _, selector := range selectors {
			output, err := k.executor.runCommand("kubectl", "get", kind, "-n", k.namespace, "-l", selector, "-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
			if err != nil || output == "" {
				continue
			}
			names := nonEmptyLines(output)
			if len(names) > 0 {
				return kind, names[0], nil
			}
		}

		if workloads, err := k.listWorkloads(kind); err == nil {
			var candidate string
			for _, workload := range workloads {
				for _, selector := range selectors {
					if selectorMatchesLabels(selector, workload.SelectorLabels) || selectorMatchesLabels(selector, workload.TemplateLabels) {
						return kind, workload.Name, nil
					}
				}
				if candidate == "" && strings.Contains(workload.Name, service) {
					candidate = workload.Name
				}
			}
			if candidate != "" {
				return kind, candidate, nil
			}
		}

		output, err := k.executor.runCommand("kubectl", "get", kind, "-n", k.namespace, "-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
		if err != nil {
			continue
		}
		for _, name := range nonEmptyLines(output) {
			if strings.Contains(name, service) {
				return kind, name, nil
			}
		}
	}

	return "", "", fmt.Errorf("no workloads found")
}

func (k *KubernetesBackend) listPods(service string) ([]string, error) {
	selectors := k.podSelectors(service)
	for _, selector := range selectors {
		output, err := k.executor.runCommand("kubectl", "get", "pods", "-n", k.namespace, "-l", selector, "-o", "jsonpath={range .items[*]}{.status.phase}{\"\\n\"}{end}")
		if err != nil {
			continue
		}
		statuses := nonEmptyLines(output)
		if len(statuses) > 0 {
			return statuses, nil
		}
	}
	// Fallback to all pods search
	output, err := k.executor.runCommand("kubectl", "get", "pods", "-n", k.namespace, "-o", "jsonpath={range .items[*]}{.metadata.name}{\";\"}{.status.phase}{\"\\n\"}{end}")
	if err != nil {
		return nil, err
	}
	statuses := []string{}
	for _, line := range strings.Split(output, "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, ";")
		if len(parts) != 2 {
			continue
		}
		if strings.Contains(parts[0], service) {
			statuses = append(statuses, parts[1])
		}
	}
	return statuses, nil
}

func (k *KubernetesBackend) getPodForService(service string) (string, error) {
	if pod, ok := k.podCache[service]; ok && pod != "" {
		return pod, nil
	}

	selectors := k.podSelectors(service)
	for _, selector := range selectors {
		output, err := k.executor.runCommand("kubectl", "get", "pods", "-n", k.namespace, "-l", selector, "-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
		if err != nil {
			continue
		}
		pods := nonEmptyLines(output)
		if len(pods) > 0 {
			k.podCache[service] = pods[0]
			return pods[0], nil
		}
	}

	output, err := k.executor.runCommand("kubectl", "get", "pods", "-n", k.namespace, "-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
	if err != nil {
		return "", err
	}
	for _, name := range nonEmptyLines(output) {
		if strings.Contains(name, service) {
			k.podCache[service] = name
			return name, nil
		}
	}

	return "", fmt.Errorf("no pods found for service %s in namespace %s", service, k.namespace)
}

func (k *KubernetesBackend) podSelectors(service string) []string {
	return []string{
		fmt.Sprintf("app.kubernetes.io/component=%s", service),
		fmt.Sprintf("app=%s", service),
		fmt.Sprintf("component=%s", service),
		fmt.Sprintf("infrahub/service=%s", service),
	}
}

type kubernetesWorkload struct {
	Name           string
	SelectorLabels map[string]string
	TemplateLabels map[string]string
}

func (k *KubernetesBackend) listWorkloads(kind string) ([]kubernetesWorkload, error) {
	output, err := k.executor.runCommand("kubectl", "get", kind, "-n", k.namespace, "-o", "json")
	if err != nil {
		return nil, err
	}

	var parsed struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Selector struct {
					MatchLabels map[string]string `json:"matchLabels"`
				} `json:"selector"`
				Template struct {
					Metadata struct {
						Labels map[string]string `json:"labels"`
					} `json:"metadata"`
				} `json:"template"`
			} `json:"spec"`
		} `json:"items"`
	}

	if err := json.Unmarshal([]byte(output), &parsed); err != nil {
		return nil, err
	}

	workloads := make([]kubernetesWorkload, 0, len(parsed.Items))
	for _, item := range parsed.Items {
		workloads = append(workloads, kubernetesWorkload{
			Name:           item.Metadata.Name,
			SelectorLabels: item.Spec.Selector.MatchLabels,
			TemplateLabels: item.Spec.Template.Metadata.Labels,
		})
	}

	return workloads, nil
}

func selectorMatchesLabels(selector string, labels map[string]string) bool {
	if len(labels) == 0 {
		return false
	}
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return false
	}

	conditions := strings.Split(selector, ",")
	for _, condition := range conditions {
		condition = strings.TrimSpace(condition)
		if condition == "" {
			continue
		}
		kv := strings.SplitN(condition, "=", 2)
		if len(kv) != 2 {
			if _, ok := labels[condition]; !ok {
				return false
			}
			continue
		}
		key := strings.TrimSpace(kv[0])
		value := strings.TrimSpace(kv[1])
		if v, ok := labels[key]; !ok || v != value {
			return false
		}
	}

	return true
}

func (k *KubernetesBackend) prepareCommand(command []string, opts *ExecOptions) []string {
	if opts == nil {
		return command
	}

	result := make([]string, len(command))
	copy(result, command)

	if len(opts.Env) > 0 {
		keys := make([]string, 0, len(opts.Env))
		for key := range opts.Env {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		envArgs := []string{"env"}
		for _, key := range keys {
			envArgs = append(envArgs, fmt.Sprintf("%s=%s", key, opts.Env[key]))
		}
		result = append(envArgs, result...)
	}

	/*
		not required since k8s user in neo4j is neo4j
		if opts.User != "" {
			commandString := shellQuoteCommand(result)
			result = []string{"su", "-", opts.User, "-s", "/bin/sh", "-c", commandString}
		}
	*/

	return result
}

/*
func shellQuoteCommand(parts []string) string {
	quoted := make([]string, len(parts))
	for i, part := range parts {
		quoted[i] = shellQuote(part)
	}
	return strings.Join(quoted, " ")
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	if !strings.ContainsAny(value, " ' \"$`!#&()*;<>?|{}[]~") {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
*/

func nonEmptyLines(output string) []string {
	lines := []string{}
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			lines = append(lines, trimmed)
		}
	}
	return lines
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
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

func unique(values []string) []string {
	if len(values) == 0 {
		return values
	}
	result := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, v := range values {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		result = append(result, v)
	}
	sort.Strings(result)
	return result
}

func ListKubernetesNamespaces(executor *CommandExecutor) ([]string, error) {
	output, err := executor.runCommand("kubectl", "get", "pods", "-A", "-l", "app.kubernetes.io/name=infrahub", "-o", "jsonpath={range .items[*]}{.metadata.namespace}{\"\\n\"}{end}")
	if err != nil {
		return nil, err
	}
	namespaces := unique(nonEmptyLines(output))
	return namespaces, nil
}
