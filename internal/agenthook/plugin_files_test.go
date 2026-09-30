package agenthook

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// pluginDir is the agent-link plugin of this repository.
const pluginDir = "../../plugins/agent-link"

// pluginHooks is a plugin hook file: matcher groups of handlers per event.
type pluginHooks struct {
	Hooks map[string][]struct {
		Matcher string `json:"matcher"`
		Hooks   []struct {
			Type           string   `json:"type"`
			Command        string   `json:"command"`
			CommandWindows string   `json:"commandWindows"`
			Args           []string `json:"args"`
			AsyncRewake    bool     `json:"asyncRewake"`
			Timeout        int      `json:"timeout"`
		} `json:"hooks"`
	} `json:"hooks"`
}

// manifestHooks reads the hook file the plugin manifest in dir (.claude-plugin,
// .codex-plugin) names.
func manifestHooks(t *testing.T, dir string) (string, pluginHooks) {
	t.Helper()
	var m struct {
		Hooks string `json:"hooks"`
	}
	data, err := os.ReadFile(filepath.Join(pluginDir, dir, "plugin.json")) //nolint:gosec // G304: the repository's plugin files
	if err != nil || json.Unmarshal(data, &m) != nil || m.Hooks == "" {
		t.Fatalf("%s/plugin.json names no hook file: %v", dir, err)
	}
	var h pluginHooks
	data, err = os.ReadFile(filepath.Join(pluginDir, filepath.FromSlash(m.Hooks)))
	if err != nil || json.Unmarshal(data, &h) != nil || len(h.Hooks) == 0 {
		t.Fatalf("%s: %s: %v", dir, m.Hooks, err)
	}
	return m.Hooks, h
}

// Claude Code loads a plugin's hooks/hooks.json besides the file its manifest
// names, and on Windows runs hook commands through bash: the Codex hook file
// there ran `"${PLUGIN_ROOT}/bin/agentlink" hook codex` in every Claude Code
// session, where PLUGIN_ROOT is unset, and each event failed with
// "/bin/agentlink: No such file or directory". Each client's manifest names
// its own file, and no file sits at Claude Code's default path.
func TestPluginHookFilesPerClient(t *testing.T) {
	if _, err := os.Stat(filepath.Join(pluginDir, "hooks", "hooks.json")); err == nil {
		t.Fatal("hooks/hooks.json is loaded by Claude Code whatever the manifest says: keep each client's hooks in the file its manifest names")
	}
	claudeFile, claude := manifestHooks(t, ".claude-plugin")
	codexFile, codex := manifestHooks(t, ".codex-plugin")
	if claudeFile == codexFile {
		t.Fatalf("one hook file for both clients: %s", claudeFile)
	}
	for ev, groups := range claude.Hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				line := h.Command + " " + strings.Join(h.Args, " ")
				if !strings.Contains(line, "${CLAUDE_PLUGIN_ROOT}") || !strings.Contains(line, "hook claude") {
					t.Errorf("claude %s: %q does not run `hook claude` from ${CLAUDE_PLUGIN_ROOT}", ev, line)
				}
			}
		}
	}
	for ev, groups := range codex.Hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				for _, line := range []string{h.Command, h.CommandWindows} {
					if !strings.Contains(line, "${PLUGIN_ROOT}") || !strings.HasSuffix(line, " hook codex") {
						t.Errorf("codex %s: %q does not run `hook codex` from ${PLUGIN_ROOT}", ev, line)
					}
				}
			}
		}
	}
}

// The plugin's hook files carry the hooks `agentlink hook install` writes
// (agenthook's events, matchers, timeouts and Claude Code's waiter): a hook
// added there and not here (PostToolUseFailure was) fails this test.
func TestPluginHooksMatchInstall(t *testing.T) {
	for _, c := range []struct{ dir, client string }{{".claude-plugin", Claude}, {".codex-plugin", Codex}} {
		_, h := manifestHooks(t, c.dir)
		if got, want := slices.Sorted(maps.Keys(h.Hooks)), slices.Sorted(slices.Values(EventsFor(c.client))); !slices.Equal(got, want) {
			t.Errorf("%s events %v, want %v", c.client, got, want)
		}
		for _, ev := range EventsFor(c.client) {
			groups := h.Hooks[ev]
			if len(groups) != 1 {
				t.Errorf("%s %s: %d groups, want 1", c.client, ev, len(groups))
				continue
			}
			wantMatcher := ""
			if ev == PreTool || ev == PostTool || ev == PostToolFailure {
				wantMatcher = "*"
			}
			want := Handlers(c.client, "agentlink", ev)
			if g := groups[0]; g.Matcher != wantMatcher || len(g.Hooks) != len(want) {
				t.Errorf("%s %s: matcher %q, %d handlers; want %q, %d", c.client, ev, g.Matcher, len(g.Hooks), wantMatcher, len(want))
				continue
			}
			for i, w := range want {
				got := groups[0].Hooks[i]
				runs := strings.HasSuffix(got.Command, strings.TrimPrefix(w.Command, "agentlink"))
				if c.client == Claude {
					runs = len(got.Args) >= len(w.Args) && slices.Equal(got.Args[len(got.Args)-len(w.Args):], w.Args)
				}
				if !runs || got.Type != w.Type || got.Timeout != w.Timeout || got.AsyncRewake != w.AsyncRewake {
					t.Errorf("%s %s handler %d: %+v, want the arguments, timeout and asyncRewake of %+v", c.client, ev, i, got, w)
				}
			}
		}
	}
}
