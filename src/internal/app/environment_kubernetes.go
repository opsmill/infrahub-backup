package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// errNoPodsMatched is returned by GetAllPods when kubectl calls succeeded but
// no pod matched the service. It is distinct from a real kubectl/cluster
// failure so callers (ServiceReplicas) can record "service not deployed"
// (skipped) only for a genuine empty match, and surface API/RBAC/cluster
// errors as collector failures instead of masking them (FIX-2).
var errNoPodsMatched = errors.New("no pods matched the service")

// podRunner executes a kubectl command and returns its trimmed output. The
// shared pod-resolution helpers (getPodForService, GetAllPods — used by the
// backup tool) pass the unbounded executor.runCommand; the collect primitives
// pass a timeout-bounded runner so a wedged API server cannot hang the bundle
// run before the exec/copy timeout even applies (research R2, FIX-5).
type podRunner func(name string, args ...string) (string, error)

// boundedRunner returns a podRunner that bounds every kubectl call by timeout.
func (k *KubernetesBackend) boundedRunner(ctx context.Context, timeout time.Duration) podRunner {
	return func(name string, args ...string) (string, error) {
		return k.executor.runCommandContext(ctx, timeout, name, args...)
	}
}

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

// getPodForService resolves a single pod for a service using the unbounded
// executor. It is the shared helper used by the backup tool; its behavior is
// unchanged.
func (k *KubernetesBackend) getPodForService(service string) (string, error) {
	return k.getPodForServiceWith(k.executor.runCommand, service)
}

// getPodForServiceContext is the timeout-bounded pod resolver the collect
// primitives use so a hung API server cannot stall the run before the exec/copy
// timeout applies (research R2, FIX-5). It shares getPodForServiceWith with the
// unbounded getPodForService, differing only in the runner.
func (k *KubernetesBackend) getPodForServiceContext(ctx context.Context, timeout time.Duration, service string) (string, error) {
	return k.getPodForServiceWith(k.boundedRunner(ctx, timeout), service)
}

// getPodForServiceWith resolves a single pod for a service via the supplied
// runner, caching the result. HA clusters resolve to the primary pod.
func (k *KubernetesBackend) getPodForServiceWith(run podRunner, service string) (string, error) {
	if pod, ok := k.podCache[service]; ok && pod != "" {
		return pod, nil
	}

	selectors := k.podSelectors(service)
	for _, selector := range selectors {
		output, err := run("kubectl", "get", "pods", "-n", k.namespace, "-l", selector, "-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
		if err != nil {
			continue
		}
		pods := nonEmptyLines(output)
		if len(pods) > 0 {
			// If multiple pods found, try to find the primary (for HA clusters like CloudNativePG)
			if len(pods) > 1 {
				if primary := k.findPrimaryPod(run, pods); primary != "" {
					k.podCache[service] = primary
					return primary, nil
				}
			}
			k.podCache[service] = pods[0]
			return pods[0], nil
		}
	}

	output, err := run("kubectl", "get", "pods", "-n", k.namespace, "-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
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

// GetAllPods returns all pod names for a given service using the unbounded
// executor (shared helper used by the backup tool). It returns errNoPodsMatched
// only when kubectl calls succeeded but nothing matched, and the wrapped
// kubectl error when the cluster/API itself is unreachable (FIX-2).
func (k *KubernetesBackend) GetAllPods(service string) ([]string, error) {
	return k.getAllPodsWith(k.executor.runCommand, service)
}

// getAllPodsContext is the timeout-bounded pod enumerator the collect primitives
// use (research R2, FIX-5).
func (k *KubernetesBackend) getAllPodsContext(ctx context.Context, timeout time.Duration, service string) ([]string, error) {
	return k.getAllPodsWith(k.boundedRunner(ctx, timeout), service)
}

// getAllPodsWith enumerates every pod backing a service via the supplied
// runner. A successful-but-empty match returns errNoPodsMatched; a failed
// fallback kubectl call returns the wrapped kubectl error so cluster failures
// are never mistaken for an undeployed service (FIX-2).
func (k *KubernetesBackend) getAllPodsWith(run podRunner, service string) ([]string, error) {
	selectors := k.podSelectors(service)
	for _, selector := range selectors {
		output, err := run("kubectl", "get", "pods", "-n", k.namespace, "-l", selector, "-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
		if err != nil {
			continue
		}
		pods := nonEmptyLines(output)
		if len(pods) > 0 {
			return pods, nil
		}
	}

	// Fallback: substring match on pod names, mirroring getPodForService. The
	// Helm chart labels some pods with a different service value than the
	// canonical name (e.g. infrahub-server pods carry infrahub/service=server),
	// so label selectors alone would miss them. A failure here means the
	// cluster/API is unreachable, not that the service is absent.
	output, err := run("kubectl", "get", "pods", "-n", k.namespace, "-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
	if err != nil {
		return nil, fmt.Errorf("failed to list pods in namespace %s: %w", k.namespace, err)
	}
	pods := []string{}
	for _, name := range nonEmptyLines(output) {
		if strings.Contains(name, service) {
			pods = append(pods, name)
		}
	}
	if len(pods) > 0 {
		return pods, nil
	}

	return nil, fmt.Errorf("no pods found for service %s in namespace %s: %w", service, k.namespace, errNoPodsMatched)
}

// KubernetesBackend implements the collect-side primitives (spec
// 003-collect-tool, research R3/R4) and the optional timeout-bounded and
// per-replica exec capabilities the diagnostics collectors prefer.
var (
	_ collectBackend        = (*KubernetesBackend)(nil)
	_ contextExecer         = (*KubernetesBackend)(nil)
	_ replicaExecer         = (*KubernetesBackend)(nil)
	_ separateExecer        = (*KubernetesBackend)(nil)
	_ separateReplicaExecer = (*KubernetesBackend)(nil)
	_ contextCopier         = (*KubernetesBackend)(nil)
	_ deploymentDescriber   = (*KubernetesBackend)(nil)
)

// buildExecArgsContext resolves the pod under a bounded runner and constructs
// kubectl exec arguments, so the pod-resolution kubectl call is bounded too
// (FIX-5) rather than only the exec that follows.
func (k *KubernetesBackend) buildExecArgsContext(ctx context.Context, timeout time.Duration, service string, command []string, opts *ExecOptions) ([]string, error) {
	pod, err := k.getPodForServiceContext(ctx, timeout, service)
	if err != nil {
		return nil, err
	}
	finalCmd := k.prepareCommand(command, opts)
	args := []string{"exec", "-n", k.namespace, pod, "--"}
	args = append(args, finalCmd...)
	return args, nil
}

// ExecContext is the timeout-bounded variant of Exec used by the bundle
// collectors (research R2: 60s per status dump). Both the pod resolution and
// the exec itself are bounded (FIX-5).
func (k *KubernetesBackend) ExecContext(ctx context.Context, timeout time.Duration, service string, command []string, opts *ExecOptions) (string, error) {
	args, err := k.buildExecArgsContext(ctx, timeout, service, command, opts)
	if err != nil {
		return "", err
	}
	return k.executor.runCommandContext(ctx, timeout, "kubectl", args...)
}

// ExecSeparateContext is ExecContext with the command's stdout and stderr kept
// apart, so a collector can write the payload (stdout) to the bundle without
// kubectl's own notices — most notably `Defaulted container "x" out of: …` on a
// multi-container pod — or the command's diagnostics being mixed into it.
func (k *KubernetesBackend) ExecSeparateContext(ctx context.Context, timeout time.Duration, service string, command []string, opts *ExecOptions) (string, string, error) {
	args, err := k.buildExecArgsContext(ctx, timeout, service, command, opts)
	if err != nil {
		return "", "", err
	}
	return k.executor.runCommandSeparateContext(ctx, timeout, "kubectl", args...)
}

// ExecReplicaSeparate is ExecReplica with stdout and stderr kept apart (see
// ExecSeparateContext).
func (k *KubernetesBackend) ExecReplicaSeparate(ctx context.Context, timeout time.Duration, replica Replica, command []string) (string, string, error) {
	args := []string{"exec", "-n", k.namespace, replica.Pod, "-c", replica.Container, "--"}
	args = append(args, command...)
	return k.executor.runCommandSeparateContext(ctx, timeout, "kubectl", args...)
}

// CopyFromContext is the timeout-bounded variant of CopyFrom used by the bundle
// collectors (research R2, FIX-1). Pod resolution is bounded by the exec
// timeout (a quick metadata lookup) while the copy itself gets the supplied
// transfer timeout. The shared CopyFrom is left untouched for the backup tool.
func (k *KubernetesBackend) CopyFromContext(ctx context.Context, timeout time.Duration, service, src, dest string) error {
	pod, err := k.getPodForServiceContext(ctx, collectExecTimeout, service)
	if err != nil {
		return err
	}
	source := fmt.Sprintf("%s/%s:%s", k.namespace, pod, src)
	if _, err := k.executor.runCommandContext(ctx, timeout, "kubectl", "cp", source, dest); err != nil {
		return err
	}
	return nil
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
	pods, err := k.getAllPodsContext(context.Background(), collectExecTimeout, service)
	if err != nil {
		if errors.Is(err, errNoPodsMatched) {
			// kubectl succeeded but nothing matched: the service is genuinely
			// not deployed in this namespace, so callers record it as skipped.
			return nil, nil
		}
		// A real kubectl/cluster failure (API/RBAC/unreachable) must surface so
		// the collector records failed with the reason instead of masquerading
		// as "service not deployed" (FIX-2).
		return nil, fmt.Errorf("failed to enumerate %s pods: %w", service, err)
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

// kubectlBenchmarkRunArgs builds the kubectl run arguments for the one-off
// benchmark pod (research R11): attached so its output is captured, never
// restarted, removed after the attach session completes, and bounded by
// --pod-running-timeout so a failing image pull surfaces as an error instead
// of waiting out the whole benchmark bound.
func kubectlBenchmarkRunArgs(namespace, name, image string) []string {
	return []string{
		"run", name,
		"-n", namespace,
		"--image=" + image,
		"--image-pull-policy=Always",
		"--restart=Never",
		"--attach",
		"--rm",
		"--quiet",
		"--pod-running-timeout=5m",
	}
}

// RunBenchmark runs the opt-in benchmark image as a one-off attached pod in
// the namespace (research R11) and returns its combined output. The pod is
// deleted afterwards even when the run timed out — kubectl's --rm only covers
// a completed attach session. Deleting it is permitted: FR-010 protects the
// deployment's workloads and this pod is the tool's own transient resource.
func (k *KubernetesBackend) RunBenchmark(ctx context.Context, image, podName string) (string, error) {
	defer func() {
		// context.Background(): the pod must be deleted even when ctx was
		// cancelled by a timeout or an interrupt.
		if _, err := k.executor.runCommandContext(context.Background(), collectExecTimeout,
			"kubectl", "delete", "pod", podName, "-n", k.namespace, "--ignore-not-found=true", "--now"); err != nil {
			logrus.Debugf("Failed to delete benchmark pod %s: %v", podName, err)
		}
	}()

	args := kubectlBenchmarkRunArgs(k.namespace, podName, image)
	output, err := k.executor.runCommandContext(ctx, collectBenchmarkTimeout, "kubectl", args...)
	if err != nil {
		if output != "" {
			return "", fmt.Errorf("kubectl run failed: %w: %s", err, commandErrorLine(output))
		}
		return "", fmt.Errorf("kubectl run failed: %w", err)
	}
	return output, nil
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

// helmChartJSONPath reads, per workload in the namespace, the standard Helm
// stamps: the helm.sh/chart label ("<name>-<version>") and the
// meta.helm.sh/release-name annotation, tab-separated, one workload per line.
// The dotted keys are escaped for kubectl jsonpath; the "/" needs no escaping.
const helmChartJSONPath = `jsonpath={range .items[*]}` +
	`{.metadata.labels.helm\.sh/chart}{"\t"}` +
	`{.metadata.annotations.meta\.helm\.sh/release-name}{"\n"}{end}`

// HelmRelease reports the Helm chart and version a Kubernetes install was
// templated from, read from the labels/annotations Helm stamps on its
// resources. It needs only kubectl — the helm CLI is not required — and
// returns (nil, nil) when the workloads carry no Helm metadata (e.g. an
// install not managed by Helm).
func (k *KubernetesBackend) HelmRelease() (*HelmRelease, error) {
	output, err := k.executor.runCommandContext(context.Background(), collectExecTimeout,
		"kubectl", "get", "deployments,statefulsets", "-n", k.namespace, "-o", helmChartJSONPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read Helm metadata from namespace %s: %w", k.namespace, err)
	}
	return parseHelmChartLabels(output), nil
}

// infrahubProductCharts are the Helm chart names for the Infrahub product
// itself. A namespace commonly co-hosts auxiliary Infrahub charts — most
// notably infrahub-observability — whose workloads carry their own
// helm.sh/chart stamp. The manifest must report the product chart's version,
// so parseHelmChartLabels prefers a workload templated from one of these.
var infrahubProductCharts = map[string]bool{
	"infrahub":            true,
	"infrahub-enterprise": true,
}

// parseHelmChartLabels extracts the release/chart/version from the
// tab-separated "<chart-label>\t<release-name>" lines HelmRelease's jsonpath
// query produces. It prefers the first workload templated from an Infrahub
// product chart (infrahub / infrahub-enterprise), so a co-hosted chart such as
// infrahub-observability never shadows the product's version regardless of the
// order kubectl lists workloads. When no product chart is present it falls back
// to the first workload carrying any chart label, and returns nil when none do
// (kubectl emits empty fields for resources without the stamps).
func parseHelmChartLabels(output string) *HelmRelease {
	var fallback *HelmRelease
	for _, line := range nonEmptyLines(output) {
		fields := strings.SplitN(line, "\t", 2)
		chart := strings.TrimSpace(fields[0])
		if chart == "" {
			continue
		}
		release := &HelmRelease{}
		if len(fields) == 2 {
			release.ReleaseName = strings.TrimSpace(fields[1])
		}
		if name, version, ok := splitHelmChartLabel(chart); ok {
			release.Chart = name
			release.ChartVersion = version
		} else {
			release.Chart = chart
		}
		if infrahubProductCharts[release.Chart] {
			return release
		}
		if fallback == nil {
			fallback = release
		}
	}
	return fallback
}

// splitHelmChartLabel splits a Helm "<name>-<version>" chart label at the
// first hyphen that begins the version — a hyphen followed by a digit, since
// chart versions are semver. This preserves chart names with embedded hyphens
// (infrahub-enterprise-1.2.3 → "infrahub-enterprise", "1.2.3") and versions
// with prerelease suffixes (infrahub-1.2.3-alpha.1 → "infrahub",
// "1.2.3-alpha.1"). It returns ok=false when no version segment is found.
func splitHelmChartLabel(chart string) (name, version string, ok bool) {
	for i := 0; i < len(chart)-1; i++ {
		if chart[i] == '-' && chart[i+1] >= '0' && chart[i+1] <= '9' {
			return chart[:i], chart[i+1:], true
		}
	}
	return "", "", false
}
