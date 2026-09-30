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

// Marketplace is the marketplace this repository publishes the plugin in
// (.claude-plugin/marketplace.json) and MarketplaceSource where Codex gets it.
const (
	Marketplace       = "agent-link"
	MarketplaceSource = "UberMorgott/agent-link"
)

// CodexInstallCommands are Codex's own commands that install and enable the
// plugin (program first): the marketplace is added unless configured. nil
// when Codex already lists an agent-link plugin in any marketplace, enabled
// or not: a disabled one is the person's choice and stays so.
func CodexInstallCommands() [][]string {
	_, cfg, ok := readCodexConfig()
	if !ok {
		return nil
	}
	for id := range cfg.Plugins {
		if name, _, _ := strings.Cut(id, "@"); name == PluginName {
			return nil
		}
	}
	var cmds [][]string
	if _, ok := cfg.Marketplaces[Marketplace]; !ok {
		cmds = append(cmds, []string{"codex", "plugin", "marketplace", "add", MarketplaceSource})
	}
	return append(cmds, []string{"codex", "plugin", "add", PluginName + "@" + Marketplace})
}

// InstallCodexPlugin installs and enables the agent-link plugin in Codex with
// its own commands (CodexInstallCommands) run through program (the resolved
// codex executable); it touches no other plugin and never the plugin cache.
// It returns whether it ran anything.
func InstallCodexPlugin(ctx context.Context, program string, run Runner) (bool, error) {
	cmds := CodexInstallCommands()
	if len(cmds) == 0 {
		return false, nil
	}
	if program == "" {
		return false, errors.New(Codex + " not found")
	}
	for _, cmd := range cmds {
		cctx, cancel := context.WithTimeout(ctx, refreshTimeout)
		out, err := run(cctx, program, cmd[1:]...)
		cancel()
		if err != nil {
			return true, fmt.Errorf("%s: %w: %s", strings.Join(cmd, " "), err, strings.TrimSpace(string(out)))
		}
	}
	return true, nil
}
