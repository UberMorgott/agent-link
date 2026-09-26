package node

import (
	"context"
	"fmt"
	"os"
	"os/exec"
)

// TerminalLauncher opens sessions in a new Windows Terminal window
// (LaunchCommand).
type TerminalLauncher struct {
	ProgramPath func(provider string) string
}

// Launch starts wt.exe with spec's command and does not wait for it.
func (l TerminalLauncher) Launch(ctx context.Context, spec LaunchSpec) error {
	if spec.ProgramPath == "" && l.ProgramPath != nil {
		spec.ProgramPath = l.ProgramPath(spec.Provider)
	}
	args := LaunchCommand(spec)
	wt, err := exec.LookPath(args[0])
	if err != nil {
		return fmt.Errorf("%w: %w", ErrNoTerminal, err)
	}
	if _, err := exec.LookPath(args[5]); err != nil {
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
