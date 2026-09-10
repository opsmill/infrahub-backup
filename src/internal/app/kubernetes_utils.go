package app

import (
	"fmt"
	"sort"
	"strings"

	"github.com/sirupsen/logrus"
)

func (k *KubernetesBackend) podSelectors(service string) []string {
	selectors := make([]string, 0, len(serviceLabelKeys))
	for _, key := range serviceLabelKeys {
		selectors = append(selectors, fmt.Sprintf("%s=%s", key, service))
	}

	return selectors
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

func selectorMatchesLabels(selector string, labels map[string]string) bool {
	if len(labels) == 0 {
		return false
	}
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return false
	}

	for _, condition := range commaFields(selector) {
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
