package agenthook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// PluginName is the name of the agent-link plugin (plugins/agent-link), which
// brings the same hooks as the folder entries through the client's plugin
// system.
const PluginName = "agent-link"

// PluginEnabled reports whether client's own state has the agent-link plugin
// installed and enabled; its hooks then already run in every session, so
// folder entries would make each hook fire twice.
func PluginEnabled(client string) bool {
	switch client {
	case Claude:
		return claudePluginEnabled()
	case Codex:
		return codexPluginEnabled()
	}
	return false
}

// configDir is the client's config folder: env when set, else name in the
// home folder; "" when there is none.
func configDir(env, name string) string {
	if dir := strings.TrimSpace(os.Getenv(env)); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, name)
}

// codexPluginEnabled: Codex's home (CODEX_HOME, else ~/.codex) turns a plugin
// on in config.toml ([plugins."agent-link@<marketplace>"] enabled = true) and
// installs it under plugins/cache/<marketplace>/agent-link.
func codexPluginEnabled() bool {
	home := configDir("CODEX_HOME", ".codex")
	if home == "" {
		return false
	}
	var cfg struct {
		Plugins map[string]struct {
			Enabled bool `toml:"enabled"`
		} `toml:"plugins"`
	}
	if _, err := toml.DecodeFile(filepath.Join(home, "config.toml"), &cfg); err != nil {
		return false
	}
	for id, p := range cfg.Plugins {
		name, market, _ := strings.Cut(id, "@")
		if p.Enabled && name == PluginName && market != "" && IsDir(filepath.Join(home, "plugins", "cache", market, name)) {
			return true
		}
	}
	return false
}

// claudePluginEnabled: Claude Code's config folder (CLAUDE_CONFIG_DIR, else
// ~/.claude) lists the plugin under "plugins" in
// plugins/installed_plugins.json and turns it on with
// "enabledPlugins": {"agent-link@<marketplace>": true} in settings.json.
func claudePluginEnabled() bool {
	dir := configDir("CLAUDE_CONFIG_DIR", ".claude")
	if dir == "" {
		return false
	}
	var settings struct {
		EnabledPlugins map[string]bool `json:"enabledPlugins"`
	}
	var installed struct {
		Plugins map[string]json.RawMessage `json:"plugins"`
	}
	if !readJSON(filepath.Join(dir, "settings.json"), &settings) ||
		!readJSON(filepath.Join(dir, "plugins", "installed_plugins.json"), &installed) {
		return false
	}
	for id, on := range settings.EnabledPlugins {
		name, _, _ := strings.Cut(id, "@")
		if on && name == PluginName && installed.Plugins[id] != nil {
			return true
		}
	}
	return false
}

// readJSON decodes the JSON file at path into v; false when it is missing or
// unreadable.
func readJSON(path string, v any) bool {
	data, err := os.ReadFile(filepath.Clean(path))
	return err == nil && json.Unmarshal(data, v) == nil
}
