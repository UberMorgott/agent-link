package agenthook

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/UberMorgott/agent-link/plugins"
)

// makeCopy writes a plugin copy at dir whose manifests name name and whose
// Codex version is version, unused since at.
func makeCopy(t *testing.T, dir, name, version string, at time.Time) {
	t.Helper()
	for _, m := range []string{".claude-plugin", ".codex-plugin"} {
		if err := os.MkdirAll(filepath.Join(dir, m), 0o700); err != nil {
			t.Fatal(err)
		}
		data := `{"name":` + strconv.Quote(name) + `,"version":` + strconv.Quote(version) + `}`
		if err := os.WriteFile(filepath.Join(dir, m, "plugin.json"), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "hooks.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(dir, "hooks.json"), filepath.Join(dir, ".claude-plugin", "plugin.json"), filepath.Join(dir, ".codex-plugin", "plugin.json"), filepath.Join(dir, ".claude-plugin"), filepath.Join(dir, ".codex-plugin"), dir} {
		if err := os.Chtimes(p, at, at); err != nil {
			t.Fatal(err)
		}
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Claude Code: only agent-link copies that installed_plugins.json no longer
// names, unused past the grace period, are removed; the installed copy, a
// recent orphan, another plugin and a person's own folder stay.
func TestPruneClaudeCache(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("CODEX_HOME", t.TempDir())
	now := time.Now()
	old := now.Add(-PluginGrace - time.Hour)
	cache := filepath.Join(dir, "plugins", "cache", "agent-link", PluginName)
	installed := filepath.Join(cache, "a0b1dde438ec")
	orphan := filepath.Join(cache, "96dc3b643097")
	recent := filepath.Join(cache, "662d53eb9ee1")
	other := filepath.Join(dir, "plugins", "cache", "agent-link", "other", "1.0.0")
	userDir := filepath.Join(cache, "my-notes")
	makeCopy(t, installed, PluginName, "x", old)
	makeCopy(t, orphan, PluginName, "x", old)
	makeCopy(t, recent, PluginName, "x", now)
	makeCopy(t, other, "other", "x", old)
	if err := os.MkdirAll(userDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(userDir, "notes.txt"), []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(userDir, old, old); err != nil {
		t.Fatal(err)
	}
	// A deletion cut short earlier is finished.
	half := filepath.Join(cache, trashPrefix+"0000")
	if err := os.MkdirAll(half, 0o700); err != nil {
		t.Fatal(err)
	}
	writeClaudeConfig(t, dir, `{}`, `{"plugins":{"agent-link@agent-link":[{"installPath":`+strconv.Quote(installed)+`}]}}`)
	removed, err := PrunePluginCaches(now)
	if err != nil || !slices.Equal(removed, []string{orphan}) {
		t.Fatalf("removed %q, %v", removed, err)
	}
	for _, keep := range []string{installed, recent, other, filepath.Join(userDir, "notes.txt")} {
		if !exists(keep) {
			t.Fatalf("%s removed", keep)
		}
	}
	if exists(orphan) || exists(half) {
		t.Fatal("orphan or half-removed copy left")
	}
	// Without the install record nothing is known to be unused.
	if err := os.Remove(filepath.Join(dir, "plugins", "installed_plugins.json")); err != nil {
		t.Fatal(err)
	}
	if removed, _ := PrunePluginCaches(now.Add(365 * 24 * time.Hour)); removed != nil {
		t.Fatalf("no record: removed %q", removed)
	}
}

// A copy with a file open (a live session's hook running from it) is kept on
// Windows, whose rename of the folder then fails, and goes once it is closed.
func TestPruneKeepsCopyInUse(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("an open file locks its folder only on Windows")
	}
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("CODEX_HOME", t.TempDir())
	now := time.Now()
	orphan := filepath.Join(dir, "plugins", "cache", "agent-link", PluginName, "96dc3b643097")
	makeCopy(t, orphan, PluginName, "x", now.Add(-PluginGrace-time.Hour))
	writeClaudeConfig(t, dir, `{}`, `{"plugins":{}}`)
	f, err := os.Open(filepath.Clean(filepath.Join(orphan, "hooks.json")))
	if err != nil {
		t.Fatal(err)
	}
	if removed, err := PrunePluginCaches(now); removed != nil || err == nil || !exists(filepath.Join(orphan, "hooks.json")) {
		_ = f.Close()
		t.Fatalf("in use: removed %q, %v", removed, err)
	}
	_ = f.Close()
	if removed, err := PrunePluginCaches(now); err != nil || len(removed) != 1 || exists(orphan) {
		t.Fatalf("closed: removed %q, %v", removed, err)
	}
}

// Codex has no install record: an old copy goes only once the expected
// version's copy is there, and after the grace period.
func TestPruneCodexCache(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	now := time.Now()
	old := now.Add(-PluginGrace - time.Hour)
	root := filepath.Join(home, "plugins", "cache", "mkt", PluginName)
	stale := filepath.Join(root, "0.6.40")
	makeCopy(t, stale, PluginName, "0.6.40+codex.aaaaaaaaaaaa", old)
	if removed, _ := PrunePluginCaches(now); removed != nil {
		t.Fatalf("no current copy: removed %q", removed)
	}
	current := filepath.Join(root, "current")
	makeCopy(t, current, PluginName, plugins.Version(), old)
	removed, err := PrunePluginCaches(now)
	if err != nil || !slices.Equal(removed, []string{stale}) || !exists(current) {
		t.Fatalf("removed %q, %v", removed, err)
	}
}
