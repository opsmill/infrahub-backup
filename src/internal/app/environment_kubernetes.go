package app

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/sirupsen/logrus"
)

type KubernetesBackend struct {
	config       *Configuration
	executor     *CommandExecutor
	namespace    string
	podCache     map[string]string
	replicaCache map[string]int // stores original replica counts before stopping
}

func NewKubernetesBackend(config *Configuration, executor *CommandExecutor) *KubernetesBackend {
	return &KubernetesBackend{
		config:       config,
		executor:     executor,
		podCache:     map[string]string{},
		replicaCache: map[string]int{},
	}
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
	for _, service := range services {
		kind, resource, err := k.findWorkloadResource(service)
		if err != nil {
			return fmt.Errorf("failed to resolve workload for %s: %w", service, err)
		}
		cacheKey := fmt.Sprintf("%s/%s", kind, resource)
		replicas := 1 // default
		if savedCount, ok := k.replicaCache[cacheKey]; ok && savedCount > 0 {
			replicas = savedCount
			logrus.Debugf("Restoring replica count for %s: %d", cacheKey, replicas)
		}
		if err := k.scaleResource(kind, resource, replicas); err != nil {
			return fmt.Errorf("failed to scale %s (%s/%s) to %d replicas: %w", service, kind, resource, replicas, err)
		}
	}
	k.podCache = map[string]string{}
	return nil
}

func (k *KubernetesBackend) Stop(services ...string) error {
	// Save current replica counts before stopping
	for _, service := range services {
		kind, resource, err := k.findWorkloadResource(service)
		if err != nil {
			continue
		}
		if count, err := k.getReplicaCount(kind, resource); err == nil && count > 0 {
			cacheKey := fmt.Sprintf("%s/%s", kind, resource)
			k.replicaCache[cacheKey] = count
			logrus.Debugf("Saved replica count for %s: %d", cacheKey, count)
		}
	}
	return k.scaleServices(services, 0)
}

func (k *KubernetesBackend) IsRunning(service string) (bool, error) {
	statuses, err := k.getPodStatuses(service)
	if err != nil {
		return false, err
	}
	for _, status := range statuses {
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

// getReplicaCount returns the current replica count for a workload
func (k *KubernetesBackend) getReplicaCount(kind, resource string) (int, error) {
	output, err := k.executor.runCommand("kubectl", "get", kind, resource, "-n", k.namespace, "-o", "jsonpath={.spec.replicas}")
	if err != nil {
		return 0, err
	}
	count, err := strconv.Atoi(strings.TrimSpace(output))
	if err != nil {
		return 0, err
	}
	return count, nil
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

func (k *KubernetesBackend) getPodStatuses(service string) ([]string, error) {
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
			// If multiple pods found, try to find the primary (for HA clusters like CloudNativePG)
			if len(pods) > 1 {
				if primary := k.findPrimaryPod(pods); primary != "" {
					k.podCache[service] = primary
					return primary, nil
				}
			}
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

// findPrimaryPod searches for a pod with primary role label (for HA PostgreSQL clusters like CloudNativePG)
func (k *KubernetesBackend) findPrimaryPod(pods []string) string {
	for _, pod := range pods {
		output, err := k.executor.runCommand("kubectl", "get", "pod", pod, "-n", k.namespace, "-o", "jsonpath={.metadata.labels.cnpg\\.io/instanceRole}")
		if err == nil && output == "primary" {
			logrus.Debugf("Found primary pod via cnpg.io/instanceRole: %s", pod)
			return pod
		}
		// Fallback to legacy role label
		output, err = k.executor.runCommand("kubectl", "get", "pod", pod, "-n", k.namespace, "-o", "jsonpath={.metadata.labels.role}")
		if err == nil && output == "primary" {
			logrus.Debugf("Found primary pod via role label: %s", pod)
			return pod
		}
	}
	return ""
}

// GetAllPods returns all pod names for a given service
func (k *KubernetesBackend) GetAllPods(service string) ([]string, error) {
	selectors := k.podSelectors(service)
	for _, selector := range selectors {
		output, err := k.executor.runCommand("kubectl", "get", "pods", "-n", k.namespace, "-l", selector, "-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
		if err != nil {
			continue
		}
		pods := nonEmptyLines(output)
		if len(pods) > 0 {
			return pods, nil
		}
	}
	return nil, fmt.Errorf("no pods found for service %s", service)
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

func ListKubernetesNamespaces(executor *CommandExecutor) ([]string, error) {
	output, err := executor.runCommand("kubectl", "get", "pods", "-A", "-l", "app.kubernetes.io/name=infrahub", "-o", "jsonpath={range .items[*]}{.metadata.namespace}{\"\\n\"}{end}")
	if err != nil {
		return nil, err
	}
	namespaces := unique(nonEmptyLines(output))
	return namespaces, nil
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

	if opts.User != "" {
		commandString := shellQuoteCommand(result)
		result = []string{"su", "-", opts.User, "-s", "/bin/sh", "-c", commandString}
	}

	return result
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
