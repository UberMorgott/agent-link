package app

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

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
