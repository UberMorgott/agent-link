package node

import (
	"context"
	"fmt"
	"os"
	"os/exec"
)

// TerminalLauncher opens sessions in a new Windows Terminal window
// (LaunchCommand).
type TerminalLauncher struct{}

// Launch starts wt.exe with spec's command and does not wait for it.
func (TerminalLauncher) Launch(ctx context.Context, spec LaunchSpec) error {
	args := LaunchCommand(spec)
	wt, err := exec.LookPath(args[0])
	if err != nil {
		return fmt.Errorf("%w: %w", ErrNoTerminal, err)
	}
	if _, err := exec.LookPath(spec.Provider); err != nil {
		return fmt.Errorf("%w: %w", ErrNoAgent, err)
	}
	// The opened window outlives the node's context: it is the person's now.
	cmd := exec.CommandContext(context.WithoutCancel(ctx), wt, args[1:]...) //nolint:gosec // G204: fixed program, arguments built by LaunchCommand
	cmd.Dir = spec.Folder
	cmd.Env = LaunchEnv(os.Environ())
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
