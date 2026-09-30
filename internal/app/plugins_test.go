package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/UberMorgott/agent-link/internal/agenthook"
	"github.com/UberMorgott/agent-link/internal/settings"
	"github.com/UberMorgott/agent-link/plugins"
)

// A released build refreshes a stale Claude Code plugin at start through the
// resolved claude program; a dev build or another settings file (no HookExe)
// runs nothing, nor does an up-to-date plugin.
func TestRefreshPlugins(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	t.Setenv("CODEX_HOME", t.TempDir())
	root := filepath.Join(cfg, "plugins", "cache", "agent-link", "agent-link", "96dc3b643097")
	setCopy := func(version string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(root, ".codex-plugin"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, ".codex-plugin", "plugin.json"), []byte(`{"version":`+strconv.Quote(version)+`}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	setCopy("0.6.40+codex.aaaaaaaaaaaa")
	for path, data := range map[string]string{
		filepath.Join(cfg, "settings.json"):                     `{"enabledPlugins":{"agent-link@agent-link":true}}`,
		filepath.Join(cfg, "plugins", "installed_plugins.json"): `{"version":2,"plugins":{"agent-link@agent-link":[{"scope":"user","installPath":` + strconv.Quote(root) + `}]}}`,
	} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var ran [][]string
	old := pluginRunner
	t.Cleanup(func() { pluginRunner = old })
	pluginRunner = func(_ context.Context, program string, args ...string) ([]byte, error) {
		ran = append(ran, append([]string{program}, args...))
		if args[1] == "update" && len(args) == 3 {
			setCopy(plugins.Version())
		}
		return nil, nil
	}
	claude := filepath.Join(t.TempDir(), "claude.exe")
	h := newHarness(t, func(a *App) {
		a.HookExe, a.Version = filepath.Join(t.TempDir(), "agentlink.exe"), "dev"
		a.Agents = settings.Finder{LookPath: func(name string) (string, error) {
			if name != "claude" {
				return "", os.ErrNotExist
			}
			return claude, nil
		}}
	})
	h.app.RefreshPlugins(t.Context())
	if ran != nil {
		t.Fatalf("dev build ran %q", ran)
	}
	h.app.Version = "0.6.57"
	h.app.RefreshPlugins(t.Context())
	want := [][]string{{claude, "plugin", "marketplace", "update", "agent-link"}, {claude, "plugin", "update", "agent-link@agent-link"}}
	if !slices.EqualFunc(ran, want, slices.Equal) {
		t.Fatalf("ran %q", ran)
	}
	ran = nil
	h.app.RefreshPlugins(t.Context())
	if ran != nil {
		t.Fatalf("up-to-date plugin ran %q", ran)
	}
	setCopy("0.6.40+codex.aaaaaaaaaaaa")
	h.app.HookExe = ""
	h.app.RefreshPlugins(t.Context())
	if ran != nil {
		t.Fatalf("no HookExe ran %q", ran)
	}
}

// With Codex on this machine and no agent-link plugin in it, a released build
// installs the plugin with Codex's own commands at start; the settings page
// shows a failed install, then the installed plugin awaiting the person's
// trust of its hooks (the folder hooks keep delivering), then the plugin on,
// which replaces the folder hooks. A plugin already listed runs nothing.
func TestInstallCodexPluginAtStart(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	work := t.TempDir()
	codex := filepath.Join(t.TempDir(), "codex.exe")
	var ran [][]string
	fail := true
	old := pluginRunner
	t.Cleanup(func() { pluginRunner = old })
	pluginRunner = func(_ context.Context, program string, args ...string) ([]byte, error) {
		ran = append(ran, append([]string{program}, args...))
		if fail {
			return []byte("no network"), errors.New("exit status 1")
		}
		if slices.Equal(args, []string{"plugin", "add", "agent-link@agent-link"}) {
			// what codex plugin add leaves: the entry and the cache copy
			root := filepath.Join(home, "plugins", "cache", "agent-link", "agent-link", "1", ".codex-plugin")
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "plugin.json"), []byte(`{"version":`+strconv.Quote(plugins.Version())+`}`), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[marketplaces.agent-link]\nsource_type = \"git\"\n[plugins.\"agent-link@agent-link\"]\nenabled = true\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return nil, nil
	}
	h := newHarness(t, func(a *App) {
		a.HookExe, a.Version = filepath.Join(t.TempDir(), "agentlink.exe"), "0.6.64"
		a.Agents = settings.Finder{Stat: os.Stat, Getenv: func(string) string { return "" }, LookPath: func(name string) (string, error) {
			if name != "codex" {
				return "", os.ErrNotExist
			}
			return codex, nil
		}}
	})
	if _, err := h.app.Apply(t.Context(), settings.Settings{Node: "alice", Listen: "127.0.0.1:0", Handler: "none", WorkDir: work}); err != nil {
		t.Fatal(err)
	}
	folderHook := func() bool {
		data, _ := os.ReadFile(filepath.Clean(agenthook.ProjectFile(work, "codex")))
		return strings.Contains(string(data), "agentlink.exe")
	}

	h.app.RefreshPlugins(t.Context())
	want := [][]string{{codex, "plugin", "marketplace", "add", "UberMorgott/agent-link"}}
	if st := h.app.HookStatus(); !slices.EqualFunc(ran, want, slices.Equal) || st.CodexPlugin != PluginError || !strings.Contains(st.CodexPluginError, "no network") {
		t.Fatalf("failed install: ran %q, status %+v", ran, st)
	}

	fail, ran = false, nil
	h.app.RefreshPlugins(t.Context())
	want = append(want, []string{codex, "plugin", "add", "agent-link@agent-link"})
	if st := h.app.HookStatus(); !slices.EqualFunc(ran, want, slices.Equal) || st.CodexPlugin != PluginUntrusted || st.CodexPluginError != "" || !folderHook() {
		t.Fatalf("install: ran %q, status %+v, folder hook %v", ran, st, folderHook())
	}

	ran = nil
	h.app.RefreshPlugins(t.Context())
	if ran != nil {
		t.Fatalf("installed plugin ran %q", ran)
	}

	trusted := "[plugins.\"agent-link@agent-link\"]\nenabled = true\n[hooks.state.\"agent-link@agent-link:hooks/codex-hooks.json:session_start:0:0\"]\ntrusted_hash = \"sha256:1\"\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(trusted), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.app.Apply(t.Context(), settings.Settings{Node: "alice", Listen: "127.0.0.1:0", Handler: "none", WorkDir: work}); err != nil {
		t.Fatal(err)
	}
	if st := h.app.HookStatus(); st.CodexPlugin != PluginOn || folderHook() {
		t.Fatalf("trusted plugin: status %+v, folder hook %v", st, folderHook())
	}
}
