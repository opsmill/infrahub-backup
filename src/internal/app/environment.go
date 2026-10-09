package app

import (
	"errors"
	"io"
	"sort"
	"strings"
)

var ErrEnvironmentNotFound = errors.New("environment not found")
var ErrCLIUnavailable = errors.New("CLI not available")

type ExecOptions struct {
	User string
	Env  map[string]string

	// Pod names the exact pod to run in, instead of resolving one from the
	// service. It is empty for every path that has a deployment to resolve
	// against, which is every path that exists today.
	//
	// It exists for a workload this run created and therefore already knows the
	// name of. The alternative — registering that pod under the service's name
	// so the ordinary resolver finds it — is what the transient
	// external-database workload used to do, and Start and scaleServices
	// replace the resolver's cache wholesale. A restore calls StartServices by
	// design, so the execution target de-registered itself mid-run and the next
	// copy resolved to nothing, against a pod that was still ready.
	//
	// Kubernetes-only. The Docker backend addresses a Compose service and has
	// no pod to name, so it ignores this field; nothing outside the Kubernetes
	// external-database path sets it.
	Pod string
}

type EnvironmentBackend interface {
	Name() string
	Detect() error
	Info() string
	Exec(service string, command []string, opts *ExecOptions) (string, error)
	ExecStream(service string, command []string, opts *ExecOptions) (string, error)
	ExecStreamPipe(service string, command []string, opts *ExecOptions) (io.ReadCloser, func() error, error)
	ExecWritePipe(service string, command []string, opts *ExecOptions, stdin io.Reader) (func() error, error)
	CopyTo(service, src, dest string) error
	CopyFrom(service, src, dest string) error
	Start(services ...string) error
	Stop(services ...string) error
	IsRunning(service string) (bool, error)
}

// runtimeNamer is a backend that can name the command-line tool it shells out
// to.
//
// It is an optional capability rather than a method on EnvironmentBackend so
// that a test double is not obliged to answer it — the same idiom serviceLocator
// and separateExecer follow. Both real backends implement it, which is what
// makes FR-018's "name what is missing" answerable from the detection loop.
type runtimeNamer interface {
	RuntimeCommand() string
}

// runtimeCommandOf names the tool a backend needs, falling back to the
// backend's own name where it does not say. The fallback is a less precise
// message, never a missing one.
func runtimeCommandOf(backend EnvironmentBackend) string {
	if namer, ok := backend.(runtimeNamer); ok {
		return namer.RuntimeCommand()
	}

	return backend.Name()
}

// Shared utility functions

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

// commaFields is nonEmptyLines for a comma-separated operator-supplied list:
// the entries, trimmed, with empty ones dropped so a trailing comma is not an
// entry. It exists because the flag parsers each opened with the same loop and
// a change to that rule had to land in every one of them.
func commaFields(text string) []string {
	fields := []string{}
	for _, field := range strings.Split(text, ",") {
		if trimmed := strings.TrimSpace(field); trimmed != "" {
			fields = append(fields, trimmed)
		}
	}

	return fields
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
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
