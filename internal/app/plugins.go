package app

import (
	"context"

	"github.com/UberMorgott/agent-link/internal/agenthook"
	"github.com/UberMorgott/agent-link/internal/selfupdate"
	"github.com/UberMorgott/agent-link/internal/settings"
)

// pluginRunner runs the host CLIs' plugin commands; tests replace it.
var pluginRunner agenthook.Runner = agenthook.RunCommand

// RefreshPlugins brings the installed agent-link plugins of Claude Code and
// Codex up to date with this executable, once per start: an update of
// agentlink leaves them at the copy of their install, whose hooks may name
// what this executable no longer has. It runs the clients' own plugin
// commands (never touching their plugin cache) and only for a released build
// of the default settings (HookExe set); a plugin still stale afterwards is
// told to the person by the SessionStart hook with the exact commands.
func (a *App) RefreshPlugins(ctx context.Context) {
	if a.HookExe == "" || !selfupdate.Valid(a.Version) {
		return
	}
	s := a.Settings()
	for _, client := range []string{agenthook.Claude, agenthook.Codex} {
		if !agenthook.CheckPlugin(client).Stale() {
			continue
		}
		program, source := "", ""
		if a.Agents.LookPath != nil {
			program, source = a.Agents.Resolve(client, s.ProgramPath(client))
		}
		if source == settings.AgentMissing {
			program = ""
		}
		c, err := agenthook.RefreshPlugin(ctx, client, program, pluginRunner)
		if err != nil {
			a.log.Warn("plugin refresh", "client", client, "installed", c.Installed, "want", c.Want, "err", err)
			continue
		}
		a.log.Info("plugin refreshed", "client", client, "plugin", c.ID, "installed", c.Installed, "stale", c.Stale())
	}
}
