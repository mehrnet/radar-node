//go:build !linux

package module

import "os/exec"

// setParentDeathSignal is Linux-only (Pdeathsig); on other platforms
// engines are still killed by context cancellation, they just cannot
// be protected against the parent's own ungraceful death the way
// Linux can.
func setParentDeathSignal(cmd *exec.Cmd) {}
