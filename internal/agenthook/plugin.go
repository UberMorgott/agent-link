package agenthook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// PluginName is the name of the agent-link plugin (plugins/agent-link), which
// brings the same hooks as the folder entries through the client's plugin
// system.
const PluginName = "agent-link"

// PluginEnabled reports whether client's own state has the agent-link plugin
// installed and enabled; its hooks then already run in every session, so
// folder entries would make each hook fire twice. Only Claude Code is known:
// its config folder (CLAUDE_CONFIG_DIR, else ~/.claude) lists the plugin under
// "plugins" in plugins/installed_plugins.json and turns it on with
// "enabledPlugins": {"agent-link@<marketplace>": true} in settings.json.
func PluginEnabled(client string) bool {
	if client != Claude {
		return false // TODO: Codex once agent-link ships a Codex plugin
	}
	dir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR"))
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return false
		}
		dir = filepath.Join(home, ".claude")
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
