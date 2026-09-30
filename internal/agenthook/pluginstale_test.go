package agenthook

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/UberMorgott/agent-link/plugins"
)

// writeCopy makes root an installed plugin copy whose Codex manifest has
// version ("" leaves the manifest out, like a copy from before it had one).
func writeCopy(t *testing.T, root, version string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, ".codex-plugin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if version == "" {
		return
	}
	if err := os.WriteFile(filepath.Join(root, ".codex-plugin", "plugin.json"), []byte(`{"version":`+strconv.Quote(version)+`}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The executable knows the plugin version it was built with: the checked-in
// Codex manifest's, which tracks the plugin's content.
func TestPluginsVersionIsManifest(t *testing.T) {
	if v := plugins.Version(); v == "" || !strings.Contains(v, "+codex.") {
		t.Fatalf("plugins.Version() = %q", v)
	}
}

func TestCheckPluginClaude(t *testing.T) {
	want := plugins.Version()
	for name, tc := range map[string]struct {
		enabled   bool
		installed string // version of the installed copy; "-" no manifest
		stale     bool
		id        string
	}{
		"current":      {true, want, false, "agent-link@mkt"},
		"old":          {true, "0.6.40+codex.aaaaaaaaaaaa", true, "agent-link@mkt"},
		"pre-manifest": {true, "-", true, "agent-link@mkt"},
		"disabled":     {false, "0.6.40+codex.aaaaaaaaaaaa", false, ""},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			root := filepath.Join(dir, "plugins", "cache", "mkt", "agent-link", "abc")
			writeCopy(t, root, strings.TrimPrefix(tc.installed, "-"))
			settings := `{"enabledPlugins":{"agent-link@mkt":` + strconv.FormatBool(tc.enabled) + `}}`
			installed := `{"version":2,"plugins":{"agent-link@mkt":[{"scope":"user","installPath":` + strconv.Quote(root) + `}]}}`
			writeClaudeConfig(t, dir, settings, installed)
			t.Setenv("CLAUDE_CONFIG_DIR", dir)
			c := CheckPlugin(Claude)
			if c.Stale() != tc.stale || c.ID != tc.id || c.Want != want {
				t.Fatalf("CheckPlugin = %+v, stale %v", c, c.Stale())
			}
			if !tc.stale {
				if c.Notice() != "" {
					t.Fatalf("notice for an up-to-date plugin: %q", c.Notice())
				}
				return
			}
			n := c.Notice()
			for _, part := range []string{"claude plugin marketplace update mkt && claude plugin update agent-link@mkt", want, "Claude Code"} {
				if !strings.Contains(n, part) {
					t.Fatalf("notice %q lacks %q", n, part)
				}
			}
		})
	}
}

func TestCheckPluginClaudeNotInstalled(t *testing.T) {
	dir := t.TempDir()
	writeClaudeConfig(t, dir, `{"enabledPlugins":{"other@mkt":true}}`, `{"plugins":{"other@mkt":[{}]}}`)
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	if c := CheckPlugin(Claude); c.Stale() || c.ID != "" || c.Notice() != "" {
		t.Fatalf("CheckPlugin = %+v", c)
	}
}

// Codex keeps one copy per version under plugins/cache/<marketplace>/<name>;
// the plugin is current when one of them is the expected version.
func TestCheckPluginCodex(t *testing.T) {
	want := plugins.Version()
	for name, tc := range map[string]struct {
		copies []string
		stale  bool
	}{
		"current":         {[]string{want}, false},
		"current and old": {[]string{"0.6.40+codex.aaaaaaaaaaaa", want}, false},
		"old":             {[]string{"0.6.40+codex.aaaaaaaaaaaa"}, true},
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			cfg := "[plugins.\"agent-link@mkt\"]\nenabled = true\n"
			if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(cfg), 0o600); err != nil {
				t.Fatal(err)
			}
			for i, v := range tc.copies {
				writeCopy(t, filepath.Join(home, "plugins", "cache", "mkt", PluginName, strconv.Itoa(i)), v)
			}
			t.Setenv("CODEX_HOME", home)
			c := CheckPlugin(Codex)
			if c.Stale() != tc.stale || c.ID != "agent-link@mkt" {
				t.Fatalf("CheckPlugin = %+v", c)
			}
			if tc.stale && !strings.Contains(c.Notice(), "codex plugin marketplace upgrade mkt && codex plugin add agent-link@mkt") {
				t.Fatalf("notice %q", c.Notice())
			}
		})
	}
}

// RefreshPlugin runs the client's own update commands through the resolved
// program, only for a stale plugin, and reports the state afterwards.
func TestRefreshPlugin(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "plugins", "cache", "mkt", "agent-link", "abc")
	writeCopy(t, root, "0.6.40+codex.aaaaaaaaaaaa")
	writeClaudeConfig(t, dir, `{"enabledPlugins":{"agent-link@mkt":true}}`,
		`{"plugins":{"agent-link@mkt":[{"installPath":`+strconv.Quote(root)+`}]}}`)
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	var ran [][]string
	run := func(_ context.Context, program string, args ...string) ([]byte, error) {
		ran = append(ran, append([]string{program}, args...))
		if slices.Equal(args, []string{"plugin", "update", "agent-link@mkt"}) {
			writeCopy(t, root, plugins.Version()) // the client installs the new copy
		}
		return nil, nil
	}
	c, err := RefreshPlugin(context.Background(), Claude, `C:\bin\claude.exe`, run)
	if err != nil || c.Stale() {
		t.Fatalf("RefreshPlugin = %+v, %v", c, err)
	}
	want := [][]string{{`C:\bin\claude.exe`, "plugin", "marketplace", "update", "mkt"}, {`C:\bin\claude.exe`, "plugin", "update", "agent-link@mkt"}}
	if !slices.EqualFunc(ran, want, slices.Equal) {
		t.Fatalf("ran %q", ran)
	}
	ran = nil
	if _, err := RefreshPlugin(context.Background(), Claude, `C:\bin\claude.exe`, run); err != nil || ran != nil {
		t.Fatalf("up to date: ran %q, %v", ran, err)
	}
	// A failing command stops the refresh and names itself.
	writeCopy(t, root, "0.6.40+codex.aaaaaaaaaaaa")
	fail := func(context.Context, string, ...string) ([]byte, error) {
		return []byte("offline"), errors.New("exit status 1")
	}
	if c, err := RefreshPlugin(context.Background(), Claude, "claude", fail); err == nil || !c.Stale() || !strings.Contains(err.Error(), "marketplace update mkt") {
		t.Fatalf("failed refresh = %+v, %v", c, err)
	}
	if _, err := RefreshPlugin(context.Background(), Claude, "", run); err == nil {
		t.Fatal("no program: want an error")
	}
}

func TestRefreshCommands(t *testing.T) {
	c := PluginCheck{Client: Claude, ID: "agent-link@m", Marketplace: "m"}
	if got := c.RefreshCommands(); len(got) != 2 || !slices.Equal(got[1], []string{"claude", "plugin", "update", "agent-link@m"}) {
		t.Fatalf("claude %q", got)
	}
	if got := (PluginCheck{Client: "x"}).RefreshCommands(); got != nil {
		t.Fatalf("unknown client %q", got)
	}
}
