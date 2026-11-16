//go:build windows

package app

import "os/exec"

// killProcessGroupOnCancel keeps exec.CommandContext's default on Windows,
// which kills only cmd: Windows has no POSIX process groups to signal.
func killProcessGroupOnCancel(cmd *exec.Cmd) {}
