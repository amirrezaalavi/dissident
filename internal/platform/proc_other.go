//go:build !linux

package platform

import "os/exec"

// setChildAttrs is a no-op on platforms without Pdeathsig.
func setChildAttrs(cmd *exec.Cmd) {}
