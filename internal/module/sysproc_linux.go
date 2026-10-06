//go:build linux

package module

import (
	"os/exec"
	"syscall"
)

// setParentDeathSignal ensures a long-lived engine process can never
// outlive radar-node itself: if the parent dies for any reason --
// SIGKILL, a crash, an OOM kill -- the kernel sends the engine
// SIGKILL too. Essential now that pooled engines live across ticks
// rather than dying with the request context that spawned them; on
// the per-check engine path it is a bonus the old design already
// implied (ctx cancellation) but could not guarantee against a
// parent's own ungraceful death.
func setParentDeathSignal(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
}
