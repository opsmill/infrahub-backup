package app

import (
	"fmt"
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
