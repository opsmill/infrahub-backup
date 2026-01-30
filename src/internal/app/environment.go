package app

import (
	"errors"
	"sort"
	"strings"
)

var ErrEnvironmentNotFound = errors.New("environment not found")
var ErrCLIUnavailable = errors.New("CLI not available")

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
