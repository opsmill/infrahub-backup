package app

import (
	"encoding/json"
	"fmt"
	"strings"
)

type kubernetesWorkload struct {
	Name           string
	SelectorLabels map[string]string
	TemplateLabels map[string]string
}

// findWorkloadResource locates a workload (deployment or statefulset) for a service
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
