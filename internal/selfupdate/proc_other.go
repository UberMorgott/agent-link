//go:build !windows

package selfupdate

import (
	"context"
	"os/exec"
	"syscall"
)

// hideFile is a no-op off Windows: there the replaced executable is unlinked
// right away, so nothing is left to hide.
func hideFile(string) error { return nil }

// Start runs exe with args in its own session, so the relaunched app survives
// this one exiting.
func Start(exe string, args []string) error {
	// The relaunched app outlives this one, so no context can end it.
	cmd := exec.CommandContext(context.Background(), exe, args...) // #nosec G204 -- this program's own path and argv
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
