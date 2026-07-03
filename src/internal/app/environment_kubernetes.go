package app

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

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
		// If user explicitly specified K8s namespace, this is a hard error
		if k.config.K8sNamespace != "" {
			return fmt.Errorf("kubectl CLI not available (required for --k8s-namespace): %w", err)
		}
		// Otherwise, treat as soft failure for auto-detection
		return fmt.Errorf("kubectl CLI not available: %w", ErrCLIUnavailable)
	}

	if k.config.K8sNamespace != "" {
		k.namespace = k.config.K8sNamespace
		if _, err := k.executor.runCommand("kubectl", "get", "pods", "-n", k.namespace, "-l", "app.kubernetes.io/name=infrahub"); err != nil {
			return fmt.Errorf("failed to verify namespace %s: %w", k.namespace, err)
		}
		return nil
	}

	namespaces, err := ListKubernetesNamespaces(k.executor)
	if err != nil {
		return err
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

// buildExecArgs resolves the pod and constructs kubectl exec arguments.
func (k *KubernetesBackend) buildExecArgs(service string, command []string, opts *ExecOptions) ([]string, error) {
	pod, err := k.getPodForService(service)
	if err != nil {
		return nil, err
	}
	finalCmd := k.prepareCommand(command, opts)
	args := []string{"exec", "-n", k.namespace, pod, "--"}
	args = append(args, finalCmd...)
	return args, nil
}

func (k *KubernetesBackend) Exec(service string, command []string, opts *ExecOptions) (string, error) {
	args, err := k.buildExecArgs(service, command, opts)
	if err != nil {
		return "", err
	}
	return k.executor.runCommand("kubectl", args...)
}

func (k *KubernetesBackend) ExecStream(service string, command []string, opts *ExecOptions) (string, error) {
	args, err := k.buildExecArgs(service, command, opts)
	if err != nil {
		return "", err
	}
	return k.executor.runCommandWithStream("kubectl", args...)
}

func (k *KubernetesBackend) ExecStreamPipe(service string, command []string, opts *ExecOptions) (io.ReadCloser, func() error, error) {
	args, err := k.buildExecArgs(service, command, opts)
	if err != nil {
		return nil, nil, err
	}
	return k.executor.runCommandPipe("kubectl", args...)
}

func (k *KubernetesBackend) ExecWritePipe(service string, command []string, opts *ExecOptions, stdin io.Reader) (func() error, error) {
	pod, err := k.getPodForService(service)
	if err != nil {
		return nil, err
	}
	finalCmd := k.prepareCommand(command, opts)
	args := []string{"exec", "-i", "-n", k.namespace, pod, "--"}
	args = append(args, finalCmd...)
	return k.executor.runCommandWritePipe(stdin, "kubectl", args...)
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

// KubernetesBackend implements the collect-side primitives (spec
// 003-collect-tool, research R3/R4) and the optional timeout-bounded and
// per-replica exec capabilities the diagnostics collectors prefer.
var (
	_ collectBackend = (*KubernetesBackend)(nil)
	_ contextExecer  = (*KubernetesBackend)(nil)
	_ replicaExecer  = (*KubernetesBackend)(nil)
)

// ExecContext is the timeout-bounded variant of Exec used by the bundle
// collectors (research R2: 60s per status dump).
func (k *KubernetesBackend) ExecContext(ctx context.Context, timeout time.Duration, service string, command []string, opts *ExecOptions) (string, error) {
	args, err := k.buildExecArgs(service, command, opts)
	if err != nil {
		return "", err
	}
	return k.executor.runCommandContext(ctx, timeout, "kubectl", args...)
}

// ExecReplica executes a command in one specific replica (pod container),
// unlike Exec which resolves a single pod per service. The bundle collectors
// use it to capture per-replica task-worker state.
func (k *KubernetesBackend) ExecReplica(ctx context.Context, timeout time.Duration, replica Replica, command []string) (string, error) {
	args := []string{"exec", "-n", k.namespace, replica.Pod, "-c", replica.Container, "--"}
	args = append(args, command...)
	return k.executor.runCommandContext(ctx, timeout, "kubectl", args...)
}

// kubectlContainerStatusArgs builds the kubectl arguments that list one
// "<container> <restartCount>" pair per line for a pod's containers.
func kubectlContainerStatusArgs(namespace, pod string) []string {
	return []string{
		"get", "pod", pod, "-n", namespace,
		"-o", "jsonpath={range .status.containerStatuses[*]}{.name}{\" \"}{.restartCount}{\"\\n\"}{end}",
	}
}

// parsePodContainerStatuses converts kubectlContainerStatusArgs output into
// one Replica per pod container, with Restarted derived from that container's
// restartCount (critique E2: restart counts live per container).
func parsePodContainerStatuses(service, pod, output string) ([]Replica, error) {
	replicas := []Replica{}
	for _, line := range nonEmptyLines(output) {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("unexpected container status line %q for pod %s", line, pod)
		}
		restarts, err := strconv.Atoi(fields[1])
		if err != nil {
			return nil, fmt.Errorf("invalid restart count %q for container %s/%s: %w", fields[1], pod, fields[0], err)
		}
		replicas = append(replicas, Replica{
			Service:   service,
			Pod:       pod,
			Container: fields[0],
			Restarted: restarts > 0,
		})
	}
	return replicas, nil
}

// ServiceReplicas enumerates one Replica per container of every pod backing a
// service, sorted by pod then container. A service with no matching pods is a
// valid empty enumeration (nil, nil) so callers can record it as skipped
// rather than failed.
func (k *KubernetesBackend) ServiceReplicas(service string) ([]Replica, error) {
	pods, err := k.GetAllPods(service)
	if err != nil {
		// GetAllPods only errors when no pods matched any selector: the
		// service is not deployed in this namespace.
		return nil, nil
	}

	replicas := []Replica{}
	for _, pod := range pods {
		args := kubectlContainerStatusArgs(k.namespace, pod)
		output, err := k.executor.runCommandContext(context.Background(), collectExecTimeout, "kubectl", args...)
		if err != nil {
			return nil, fmt.Errorf("failed to read container statuses for pod %s: %w", pod, err)
		}
		podReplicas, err := parsePodContainerStatuses(service, pod, output)
		if err != nil {
			return nil, err
		}
		replicas = append(replicas, podReplicas...)
	}

	sort.Slice(replicas, func(i, j int) bool {
		if replicas[i].Pod != replicas[j].Pod {
			return replicas[i].Pod < replicas[j].Pod
		}
		return replicas[i].Container < replicas[j].Container
	})
	return replicas, nil
}

// kubectlLogArgs builds the kubectl logs arguments for one replica.
func kubectlLogArgs(namespace string, replica Replica, tailLines int, previous bool) []string {
	args := []string{
		"logs", "-n", namespace, replica.Pod,
		"-c", replica.Container,
		fmt.Sprintf("--tail=%d", tailLines),
	}
	if previous {
		args = append(args, "--previous")
	}
	return args
}

// ReplicaLogs streams a replica's logs through kubectl, bounded by the
// collect transfer timeout (research R2) so a hung kubelet cannot stall the
// whole run.
func (k *KubernetesBackend) ReplicaLogs(replica Replica, tailLines int, previous bool) (io.ReadCloser, func() error, error) {
	args := kubectlLogArgs(k.namespace, replica, tailLines, previous)
	return k.executor.runCommandPipeContext(context.Background(), collectTransferTimeout, "kubectl", args...)
}

// Metrics captures one-shot pod resource metrics via kubectl top. A missing
// metrics-server surfaces as a normal error so the metrics collector records
// failed in the manifest without aborting the run (research R4).
func (k *KubernetesBackend) Metrics() (string, error) {
	output, err := k.executor.runCommandContext(context.Background(), collectExecTimeout, "kubectl", "top", "pods", "-n", k.namespace)
	if err != nil {
		if output != "" {
			return "", fmt.Errorf("kubectl top pods failed: %w: %s", err, output)
		}
		return "", fmt.Errorf("kubectl top pods failed: %w", err)
	}
	return output, nil
}
