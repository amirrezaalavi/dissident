//go:build !windows

package platform

import (
	"os"
	"syscall"
)

// terminateProcess asks the child to exit gracefully. Windows has no SIGTERM,
// so it uses the Windows implementation instead.
func terminateProcess(p *os.Process) error {
	return p.Signal(syscall.SIGTERM)
}
