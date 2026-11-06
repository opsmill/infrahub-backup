package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strings"
	"time"
)

// Where the collector finds the deployment's Tempo (research R2 of INFP-744).
// Both are part of the deployment contract of infrahub-collect.
const (
	// tempoComposeService and tempoComposePort are the Tempo service of the
	// Docker Compose observability stack and its HTTP port.
	tempoComposeService = "infrahub-tempo"
	tempoComposePort    = "3200"
	// tempoServiceSelector and tempoServicePort find the Tempo service of the
	// infrahub-observability Helm chart, in the Infrahub namespace.
	tempoServiceSelector = "app.kubernetes.io/name=tempo"
	tempoServicePort     = "3100"
)

var (
	_ tempoLocator = (*DockerBackend)(nil)
	_ tempoLocator = (*KubernetesBackend)(nil)
	_ stdinExecer  = (*DockerBackend)(nil)
	_ stdinExecer  = (*KubernetesBackend)(nil)
)

// LocateTempo returns the host address of the published Tempo port of the
// Compose project. A Tempo service that is absent, or whose port is not
// published, returns errTempoNotFound.
func (d *DockerBackend) LocateTempo(ctx context.Context) (string, func(), error) {
	output, err := d.executor.runCommandContext(ctx, collectExecTimeout, "docker",
		d.composeArgs("port", tempoComposeService, tempoComposePort)...)
	if err != nil {
		var timeout *timeoutError
		if !errors.As(err, &timeout) && composePortNotFound(output) {
			return "", nil, errTempoNotFound
		}
		return "", nil, fmt.Errorf("docker compose port %s %s: %w", tempoComposeService, tempoComposePort, commandError(err, output))
	}
	address, err := publishedAddress(output)
	if err != nil {
		return "", nil, err
	}
	return "http://" + address, func() {}, nil
}

// composePortNotFound reports whether the output of a failed
// `docker compose port` says that the service or its port does not exist.
func composePortNotFound(output string) bool {
	lower := strings.ToLower(output)
	for _, marker := range []string{"no such service", "no port", "no container", "not running", "is not published"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// publishedAddress turns the output of `docker compose port` into an address
// the host can reach. A port published on every interface ("0.0.0.0" or
// "::") is reached on 127.0.0.1. An empty answer or port 0 means not
// published.
func publishedAddress(output string) (string, error) {
	line := lastLine(output)
	if line == "" {
		return "", errTempoNotFound
	}
	host, port, err := net.SplitHostPort(line)
	if err != nil {
		return "", fmt.Errorf("unexpected docker compose port output %q: %w", line, err)
	}
	if port == "" || port == "0" {
		return "", errTempoNotFound
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port), nil
}

// commandError adds a command's output to its error.
func commandError(err error, output string) error {
	if line := commandErrorLine(output); line != "" {
		return fmt.Errorf("%w: %s", err, line)
	}
	return err
}

// ExecStdinContext runs a command in a service container with stdin, limited
// by timeout, and returns stdout and stderr apart. It is the collect-only,
// time-limited variant of ExecWritePipe.
func (d *DockerBackend) ExecStdinContext(ctx context.Context, timeout time.Duration, service string, command []string, stdin []byte) (string, string, error) {
	return d.executor.runCommandStdinSeparateContext(ctx, timeout, stdin, "docker", d.buildExecArgs(service, command, nil)...)
}

// portForwardLine matches the line kubectl port-forward prints when it is
// ready, for example "Forwarding from 127.0.0.1:54321 -> 3100".
var portForwardLine = regexp.MustCompile(`^Forwarding from 127\.0\.0\.1:(\d+) ->`)

// LocateTempo finds the Tempo service in the Infrahub namespace by label and
// starts `kubectl port-forward` to it for the duration of the collector. The
// returned stop function ends the port-forward.
func (k *KubernetesBackend) LocateTempo(ctx context.Context) (string, func(), error) {
	output, stderr, err := k.executor.runCommandSeparateContext(ctx, collectExecTimeout, "kubectl",
		"get", "service", "-n", k.namespace, "-l", tempoServiceSelector,
		"-o", `jsonpath={range .items[*]}{.metadata.name}{"\n"}{end}`)
	if err != nil {
		if isForbidden(stderr) {
			return "", nil, &tempoPermissionError{action: "list services", grant: "list on services"}
		}
		return "", nil, fmt.Errorf("kubectl get service -l %s: %w", tempoServiceSelector, commandError(err, stderr))
	}
	service := pickTempoService(nonEmptyLines(output))
	if service == "" {
		return "", nil, errTempoNotFound
	}

	port, stop, err := k.executor.startBackgroundCommand(ctx, collectExecTimeout, func(line string) (string, bool) {
		if match := portForwardLine.FindStringSubmatch(line); match != nil {
			return match[1], true
		}
		return "", false
	}, "kubectl", "port-forward", "-n", k.namespace, "service/"+service, ":"+tempoServicePort)
	if err != nil {
		if isForbidden(err.Error()) {
			return "", nil, &tempoPermissionError{action: "forward a port to the Tempo pod", grant: "get on services, get and list on pods, and create on pods/portforward"}
		}
		return "", nil, fmt.Errorf("kubectl port-forward service/%s: %w", service, err)
	}
	return "http://127.0.0.1:" + port, stop, nil
}

// pickTempoService chooses the Tempo service among the services with the
// Tempo label: the first one that is not a headless or member-list service.
func pickTempoService(services []string) string {
	for _, service := range services {
		if strings.Contains(service, "headless") || strings.Contains(service, "memberlist") || strings.Contains(service, "gossip") {
			continue
		}
		return service
	}
	return ""
}

// isForbidden reports whether kubectl answered that the role lacks a
// permission.
func isForbidden(output string) bool {
	return strings.Contains(strings.ToLower(output), "forbidden")
}

// ExecStdinContext runs a command in the service's pod with stdin, limited by
// timeout, and returns stdout and stderr apart. Pod resolution is limited too.
// It is the collect-only, time-limited variant of ExecWritePipe.
func (k *KubernetesBackend) ExecStdinContext(ctx context.Context, timeout time.Duration, service string, command []string, stdin []byte) (string, string, error) {
	pod, err := k.getPodForServiceContext(ctx, timeout, service)
	if err != nil {
		return "", "", err
	}
	args := []string{"exec", "-i", "-n", k.namespace, pod, "--"}
	args = append(args, command...)
	return k.executor.runCommandStdinSeparateContext(ctx, timeout, stdin, "kubectl", args...)
}
