//go:build windows

package platform

import "os"

// terminateProcess stops the child immediately. Windows does not implement
// SIGTERM for os.Process, and a console control event would also hit
// dissident itself, so Stop escalates straight to Kill.
func terminateProcess(p *os.Process) error {
	return p.Kill()
}
