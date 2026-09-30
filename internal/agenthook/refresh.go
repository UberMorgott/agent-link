package agenthook

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// refreshTimeout bounds each host CLI command of a plugin refresh (it fetches
// the marketplace from GitHub).
const refreshTimeout = 3 * time.Minute

// Runner runs program with args and returns its combined output; RunCommand
// in production, a fake in tests.
type Runner func(ctx context.Context, program string, args ...string) ([]byte, error)

// RunCommand runs a host CLI without a console window (the tray app has none).
func RunCommand(ctx context.Context, program string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, program, args...) // #nosec G204 -- the client's own CLI with fixed plugin commands
	hide(cmd)
	return cmd.CombinedOutput()
}

// RefreshPlugin brings client's stale installed plugin up to date with the
// client's own commands (RefreshCommands), run through program (the resolved
// claude or codex executable); agentlink never edits the plugin cache itself.
// It returns the plugin's state afterwards; an up-to-date or unused plugin
// runs nothing. The client loads the refreshed copy in its next session.
func RefreshPlugin(ctx context.Context, client, program string, run Runner) (PluginCheck, error) {
	c := CheckPlugin(client)
	if !c.Stale() {
		return c, nil
	}
	if program == "" {
		return c, errors.New(client + " not found")
	}
	for _, cmd := range c.RefreshCommands() {
		cctx, cancel := context.WithTimeout(ctx, refreshTimeout)
		out, err := run(cctx, program, cmd[1:]...)
		cancel()
		if err != nil {
			return c, fmt.Errorf("%s: %w: %s", strings.Join(cmd, " "), err, strings.TrimSpace(string(out)))
		}
	}
	return CheckPlugin(client), nil
}
