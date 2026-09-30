package agenthook

import (
	"encoding/json"
	"errors"
	"io/fs"
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

// codexConfig is what agentlink reads of Codex's config.toml: the plugins
// ([plugins."name@marketplace"] enabled), the marketplaces and the hook trust
// ([hooks.state."<source>:<event>:<i>:<j>"] trusted_hash).
type codexConfig struct {
	Plugins map[string]struct {
		Enabled bool `toml:"enabled"`
	} `toml:"plugins"`
	Marketplaces map[string]struct{} `toml:"marketplaces"`
	Hooks        struct {
		State map[string]struct {
			TrustedHash string `toml:"trusted_hash"`
		} `toml:"state"`
	} `toml:"hooks"`
}

// readCodexConfig reads config.toml in Codex's home (CODEX_HOME, else
// ~/.codex); a missing file is an empty config, false when there is no home
// or the file does not parse.
func readCodexConfig() (home string, cfg codexConfig, ok bool) {
	home = configDir("CODEX_HOME", ".codex")
	if home == "" {
		return "", cfg, false
	}
	_, err := toml.DecodeFile(filepath.Join(home, "config.toml"), &cfg)
	return home, cfg, err == nil || errors.Is(err, fs.ErrNotExist)
}

// hooksTrusted reports whether the person trusted a hook of plugin id: Codex
// runs a plugin's hooks only after review (keys "<id>:<hooks file>:...").
func (c codexConfig) hooksTrusted(id string) bool {
	for key, s := range c.Hooks.State {
		if strings.HasPrefix(key, id+":") && s.TrustedHash != "" {
			return true
		}
	}
	return false
}

// codexPluginEnabled: Codex turns a plugin on in config.toml
// ([plugins."agent-link@<marketplace>"] enabled = true), installs it under
// plugins/cache/<marketplace>/agent-link and runs its hooks only once the
// person trusted them; until then the folder hooks deliver.
func codexPluginEnabled() bool {
	home, cfg, ok := readCodexConfig()
	if !ok {
		return false
	}
	for id, p := range cfg.Plugins {
		name, market, _ := strings.Cut(id, "@")
		if p.Enabled && name == PluginName && market != "" && IsDir(filepath.Join(home, "plugins", "cache", market, name)) && cfg.hooksTrusted(id) {
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
