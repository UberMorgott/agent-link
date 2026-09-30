package app

import (
	"context"
	"time"

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
// told to the person by the SessionStart hook with the exact commands. Then it
// removes old copies the clients no longer use (prunePlugins).
func (a *App) RefreshPlugins(ctx context.Context) {
	if a.HookExe == "" || !selfupdate.Valid(a.Version) {
		return
	}
	s := a.Settings()
	a.installCodexPlugin(ctx, s)
	for _, client := range []string{agenthook.Claude, agenthook.Codex} {
		if !agenthook.CheckPlugin(client).Stale() {
			continue
		}
		program, _ := a.agentProgram(s, client)
		c, err := agenthook.RefreshPlugin(ctx, client, program, pluginRunner)
		if err != nil {
			a.log.Warn("plugin refresh", "client", client, "installed", c.Installed, "want", c.Want, "err", err)
			continue
		}
		a.log.Info("plugin refreshed", "client", client, "plugin", c.ID, "installed", c.Installed, "stale", c.Stale())
	}
	a.prunePlugins(time.Now())
}

// agentProgram is the resolved executable of provider ("" when it is not
// found) and whether this machine has it: picked as the handler, given a
// path, or found by the finder.
func (a *App) agentProgram(s settings.Settings, provider string) (string, bool) {
	available := s.Handler == provider || s.ProgramPath(provider) != ""
	if a.Agents.LookPath == nil {
		return "", available
	}
	program, source := a.Agents.Resolve(provider, s.ProgramPath(provider))
	if source == "" || source == settings.AgentMissing {
		return "", available
	}
	return program, true
}

// installCodexPlugin installs and enables the agent-link plugin in Codex when
// this machine has Codex and Codex lists no agent-link plugin yet: the plugin
// brings the MCP tools to interactive Codex sessions. It runs Codex's own
// commands (agenthook.InstallCodexPlugin) and touches no other plugin; a
// failure is shown on the settings page (HookStatus) and in the log. The
// plugin's hooks run once the person trusted them in Codex; the folder hooks
// deliver until then (agenthook.PluginEnabled).
func (a *App) installCodexPlugin(ctx context.Context, s settings.Settings) {
	program, available := a.agentProgram(s, agenthook.Codex)
	if !available {
		return
	}
	ran, err := agenthook.InstallCodexPlugin(ctx, program, pluginRunner)
	msg := ""
	if err != nil {
		msg = err.Error()
		a.log.Warn("codex plugin install", "err", err)
	} else if ran {
		a.log.Info("codex plugin installed", "plugin", agenthook.PluginName+"@"+agenthook.Marketplace)
	}
	a.mu.Lock()
	changed := a.codexPluginErr != msg
	a.codexPluginErr = msg
	a.mu.Unlock()
	if ran || changed {
		a.events.publish("settings")
	}
}

// prunePlugins removes the clients' old agent-link plugin copies
// (agenthook.PrunePluginCaches); a copy still in use stays for the next start.
func (a *App) prunePlugins(now time.Time) {
	removed, err := agenthook.PrunePluginCaches(now)
	if len(removed) > 0 {
		a.log.Info("old plugin copies removed", "dirs", removed)
	}
	if err != nil {
		a.log.Warn("old plugin copies kept", "err", err)
	}
}
