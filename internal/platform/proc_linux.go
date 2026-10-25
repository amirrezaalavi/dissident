//go:build linux

package platform

import (
	"os/exec"
	"syscall"
)

// setChildAttrs makes the kernel deliver SIGKILL to the child if dissident
// dies without reaping it.
func setChildAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
