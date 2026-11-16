//go:build !windows

package app

import (
	"os/exec"
	"syscall"
)

// killProcessGroupOnCancel runs cmd in a process group of its own and makes
// cancelling it kill that whole group, not only cmd: the docker CLI runs
// `compose` as a plugin child, and killing the CLI alone leaves that child to
// carry on with the operation the caller has already reported as timed out.
func killProcessGroupOnCancel(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error {
		// With Setpgid the group id is the child's pid; a negative pid
		// signals every process in the group.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
