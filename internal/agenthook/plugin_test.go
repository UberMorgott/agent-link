package agenthook

import (
	"os"
	"path/filepath"
	"testing"
)

// writeClaudeConfig makes dir a Claude Code config folder with the given
// settings.json and plugins/installed_plugins.json ("" leaves a file out).
func writeClaudeConfig(t *testing.T, dir, settings, installed string) {
	t.Helper()
	for path, data := range map[string]string{
		filepath.Join(dir, "settings.json"):                     settings,
		filepath.Join(dir, "plugins", "installed_plugins.json"): installed,
	} {
		if data == "" {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPluginEnabled(t *testing.T) {
	const installed = `{"version":2,"plugins":{"agent-link@agent-link":[{"scope":"user","version":"662d53eb9ee1"}]}}`
	for name, tc := range map[string]struct {
		settings, installed string
		want                bool
	}{
		"enabled":         {`{"enabledPlugins":{"agent-link@agent-link":true}}`, installed, true},
		"other market":    {`{"enabledPlugins":{"agent-link@mine":true}}`, `{"plugins":{"agent-link@mine":[{}]}}`, true},
		"disabled":        {`{"enabledPlugins":{"agent-link@agent-link":false}}`, installed, false},
		"not installed":   {`{"enabledPlugins":{"agent-link@agent-link":true}}`, `{"plugins":{}}`, false},
		"no install list": {`{"enabledPlugins":{"agent-link@agent-link":true}}`, "", false},
		"no settings":     {"", installed, false},
		"other plugin":    {`{"enabledPlugins":{"agent-link-x@agent-link":true}}`, `{"plugins":{"agent-link-x@agent-link":[{}]}}`, false},
		"broken settings": {`{"enabledPlugins":`, installed, false},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeClaudeConfig(t, dir, tc.settings, tc.installed)
			t.Setenv("CLAUDE_CONFIG_DIR", dir)
			if got := PluginEnabled(Claude); got != tc.want {
				t.Fatalf("PluginEnabled(claude) = %v, want %v", got, tc.want)
			}
		})
	}
	dir := t.TempDir()
	writeClaudeConfig(t, dir, `{"enabledPlugins":{"agent-link@agent-link":true}}`, installed)
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	if PluginEnabled(Codex) {
		t.Fatal("codex has no agent-link plugin")
	}
}
