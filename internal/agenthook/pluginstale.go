package agenthook

import (
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/UberMorgott/agent-link/plugins"
)

// PluginCheck compares the agent-link plugin a client has installed with the
// plugin this executable was built with. Claude Code and Codex keep serving
// the copy of the install (Claude Code does not refresh a third-party
// marketplace by default), so after an update of agentlink the installed
// hooks can name what this executable no longer has.
type PluginCheck struct {
	Client      string
	ID          string // "agent-link@<marketplace>"; "" when the plugin is not in use
	Marketplace string
	// Installed is the installed copy's version ("" for a copy older than the
	// version in its Codex manifest); Want the version this executable expects.
	Installed, Want string
}

// Stale reports an enabled plugin whose installed copy is not the expected one.
func (c PluginCheck) Stale() bool {
	return c.ID != "" && c.Want != "" && c.Installed != c.Want
}

// RefreshCommands are the client's own commands that bring the installed
// plugin up to date (program first); agentlink never edits the plugin cache.
func (c PluginCheck) RefreshCommands() [][]string {
	switch c.Client {
	case Claude:
		return [][]string{
			{"claude", "plugin", "marketplace", "update", c.Marketplace},
			{"claude", "plugin", "update", c.ID},
		}
	case Codex:
		return [][]string{
			{"codex", "plugin", "marketplace", "upgrade", c.Marketplace},
			{"codex", "plugin", "add", c.ID},
		}
	}
	return nil
}

// Notice is the line for the person at a session of a stale plugin: what is
// wrong and the exact commands; "" when the plugin is up to date.
func (c PluginCheck) Notice() string {
	if !c.Stale() {
		return ""
	}
	var cmds []string
	for _, cmd := range c.RefreshCommands() {
		cmds = append(cmds, strings.Join(cmd, " "))
	}
	app := "Claude Code"
	if c.Client == Codex {
		app = "Codex"
	}
	installed := c.Installed
	if installed == "" {
		installed = "старая"
	}
	return "agent-link: плагин устарел (установлен " + installed + ", нужен " + c.Want + "), хуки могут не работать. Обновите: " +
		strings.Join(cmds, " && ") + ", затем перезапустите " + app + "."
}

// CheckPlugin compares client's installed agent-link plugin with the one
// this executable was built with.
func CheckPlugin(client string) PluginCheck {
	c := PluginCheck{Client: client, Want: plugins.Version()}
	switch client {
	case Claude:
		c.checkClaude()
	case Codex:
		c.checkCodex()
	}
	return c
}

// manifestVersion is the "version" of the Codex manifest in an installed
// plugin copy; "" when it has none.
func manifestVersion(root string) string {
	var m struct {
		Version string `json:"version"`
	}
	if !readJSON(filepath.Join(root, ".codex-plugin", "plugin.json"), &m) {
		return ""
	}
	return m.Version
}

// checkClaude: an enabled agent-link@<marketplace> in settings.json and its
// installs (one per scope) in plugins/installed_plugins.json; the first
// install that is not the expected copy makes the plugin stale.
func (c *PluginCheck) checkClaude() {
	dir := configDir("CLAUDE_CONFIG_DIR", ".claude")
	if dir == "" {
		return
	}
	var settings struct {
		EnabledPlugins map[string]bool `json:"enabledPlugins"`
	}
	var installed struct {
		Plugins map[string][]struct {
			InstallPath string `json:"installPath"`
		} `json:"plugins"`
	}
	if !readJSON(filepath.Join(dir, "settings.json"), &settings) ||
		!readJSON(filepath.Join(dir, "plugins", "installed_plugins.json"), &installed) {
		return
	}
	ids := make([]string, 0, len(settings.EnabledPlugins))
	for id := range settings.EnabledPlugins {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		name, market, _ := strings.Cut(id, "@")
		if !settings.EnabledPlugins[id] || name != PluginName || len(installed.Plugins[id]) == 0 {
			continue
		}
		c.ID, c.Marketplace = id, market
		for _, in := range installed.Plugins[id] {
			if c.Installed = manifestVersion(in.InstallPath); c.Installed != c.Want {
				return
			}
		}
	}
}

// checkCodex: an enabled [plugins."agent-link@<marketplace>"] in config.toml
// and its copies under plugins/cache/<marketplace>/agent-link/<version>;
// Codex loads the copy of the marketplace's current version, so the plugin is
// up to date when one copy is the expected one.
func (c *PluginCheck) checkCodex() {
	home := configDir("CODEX_HOME", ".codex")
	if home == "" {
		return
	}
	var cfg struct {
		Plugins map[string]struct {
			Enabled bool `toml:"enabled"`
		} `toml:"plugins"`
	}
	if _, err := toml.DecodeFile(filepath.Join(home, "config.toml"), &cfg); err != nil {
		return
	}
	ids := make([]string, 0, len(cfg.Plugins))
	for id := range cfg.Plugins {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		name, market, _ := strings.Cut(id, "@")
		cache := filepath.Join(home, "plugins", "cache", market, name)
		if !cfg.Plugins[id].Enabled || name != PluginName || market == "" || !IsDir(cache) {
			continue
		}
		c.ID, c.Marketplace, c.Installed = id, market, ""
		entries, _ := os.ReadDir(cache)
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			if v := manifestVersion(filepath.Join(cache, e.Name())); v == c.Want {
				c.Installed = v
				return
			} else if v > c.Installed {
				c.Installed = v
			}
		}
		return
	}
}
