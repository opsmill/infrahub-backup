//go:build !linux && !darwin

package app

import (
	"fmt"
	"runtime"
)

// localFreeSpace has no implementation off Linux and macOS. Direct mode is
// reached only inside a Kubernetes pod, which is Linux, so this answer is the
// refusal of a check rather than a path anyone takes.
func localFreeSpace(string) (uint64, error) {
	return 0, fmt.Errorf("reading free disk space is not supported on %s", runtime.GOOS)
}
