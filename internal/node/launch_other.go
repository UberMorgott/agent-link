//go:build !windows

package node

import (
	"context"
	"fmt"
)

// TerminalLauncher opens sessions in Windows Terminal; elsewhere it has none.
type TerminalLauncher struct{}

// Launch reports ErrNoTerminal: opening a visible session is Windows-only.
func (TerminalLauncher) Launch(context.Context, LaunchSpec) error {
	return fmt.Errorf("%w: windows only", ErrNoTerminal)
}
