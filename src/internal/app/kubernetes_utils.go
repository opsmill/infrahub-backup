package app

import (
	"fmt"
	"sort"
	"strings"

	"github.com/sirupsen/logrus"
)

// cnpgInstanceExclusion is the label requirement that rules out CloudNativePG
// instance pods, which all carry the cnpg.io/cluster label.
const cnpgInstanceExclusion = "!cnpg.io/cluster"

// neo4jInstanceSelector matches the server pods the Neo4j Helm chart deploys.
const neo4jInstanceSelector = "helm.neo4j.com/pod_category=neo4j-instance"

// excludesCNPGInstances reports whether a CloudNativePG instance pod must be
// kept out of the pods resolved for a service. Only task-manager-db runs on
// PostgreSQL, so any other service that resolves to a CNPG pod has matched the
// wrong workload: CNPG labels every instance pod
// app.kubernetes.io/component=database, the same value as the Neo4j service.
func excludesCNPGInstances(service string) bool {
	return service != "task-manager-db"
}

// podSelectors returns the label selectors tried, in order, to find a
// service's pods. infrahub/service comes first because the Infrahub Helm chart
// and the HA deployment examples set it on every component; the generic labels
// after it are also set by other software in the same namespace.
func (k *KubernetesBackend) podSelectors(service string) []string {
	selectors := []string{fmt.Sprintf("infrahub/service=%s", service)}
	if service == "database" {
		// A Neo4j installed from its own chart without the infrahub/service label.
		selectors = append(selectors, neo4jInstanceSelector)
	}

	exclusion := ""
	if excludesCNPGInstances(service) {
		exclusion = "," + cnpgInstanceExclusion
	}
	for _, key := range []string{"app.kubernetes.io/component", "app", "component"} {
		selectors = append(selectors, fmt.Sprintf("%s=%s%s", key, service, exclusion))
	}
	return selectors
}

// podNameFallbackArgs returns the kubectl arguments that list the pod names a
// service falls back to matching by substring when no label selector matched.
// CloudNativePG instance pods are left out for the same reason as in
// podSelectors.
func (k *KubernetesBackend) podNameFallbackArgs(service, jsonpath string) []string {
	args := []string{"get", "pods", "-n", k.namespace}
	if excludesCNPGInstances(service) {
		args = append(args, "-l", cnpgInstanceExclusion)
	}
	return append(args, "-o", jsonpath)
}

// findPrimaryPod searches for a pod with primary role label (for HA PostgreSQL
// clusters like CloudNativePG). The runner is supplied by the caller so the
// collect path can bound these lookups while the backup path keeps the
// unbounded executor (FIX-5).
func (k *KubernetesBackend) findPrimaryPod(run podRunner, pods []string) string {
	for _, pod := range pods {
		output, err := run("kubectl", "get", "pod", pod, "-n", k.namespace, "-o", "jsonpath={.metadata.labels.cnpg\\.io/instanceRole}")
		if err == nil && output == "primary" {
			logrus.Debugf("Found primary pod via cnpg.io/instanceRole: %s", pod)
			return pod
		}
		// Fallback to legacy role label
		output, err = run("kubectl", "get", "pod", pod, "-n", k.namespace, "-o", "jsonpath={.metadata.labels.role}")
		if err == nil && output == "primary" {
			logrus.Debugf("Found primary pod via role label: %s", pod)
			return pod
		}
	}
	return ""
}

func ListKubernetesNamespaces(executor *CommandExecutor) ([]string, error) {
	output, err := executor.runCommand("kubectl", "get", "pods", "-A", "-l", "app.kubernetes.io/name=infrahub", "-o", "jsonpath={range .items[*]}{.metadata.namespace}{\"\\n\"}{end}")
	if err != nil {
		// Check if this is a permission/RBAC issue
		outputLower := strings.ToLower(output)
		if strings.Contains(outputLower, "forbidden") || strings.Contains(outputLower, "cannot list") {
			return nil, fmt.Errorf("insufficient permissions to list pods across namespaces; try specifying --k8s-namespace explicitly: %w", err)
		}
		// Generic kubectl failure during auto-detect is treated as "not found"
		return nil, ErrEnvironmentNotFound
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

// selectorMatchesLabels evaluates a kubectl label selector against a label set.
// It supports key=value, key (the label is set) and !key (the label is not set).
func selectorMatchesLabels(selector string, labels map[string]string) bool {
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
		if key, ok := strings.CutPrefix(condition, "!"); ok {
			if _, present := labels[key]; present {
				return false
			}
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
