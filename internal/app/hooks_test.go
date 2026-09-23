package app

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/UberMorgott/agent-link/internal/agenthook"
	"github.com/UberMorgott/agent-link/internal/settings"
)

func TestFolderHooksFollowSettings(t *testing.T) {
	root := t.TempDir()
	work, work2, dev := filepath.Join(root, "work"), filepath.Join(root, "work2"), filepath.Join(root, "dev")
	for _, d := range []string{work, work2, dev} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	exe := filepath.Join(root, "bin", "agentlink.exe") // never run: only written into the hook entries
	h := newHarness(t, func(a *App) { a.HookExe, a.Agents = exe, settings.Finder{} })
	apply := func(handler, workDir string) {
		t.Helper()
		s := settings.Settings{Node: "alice", Handler: handler, WorkDir: workDir,
			Projects: map[string]settings.Project{"dev": {Dir: dev}, "gone": {Dir: filepath.Join(root, "missing")}}}
		if _, err := h.app.Apply(t.Context(), s); err != nil {
			t.Fatal(err)
		}
	}
	has := func(dir, client string) bool {
		data, _ := os.ReadFile(filepath.Clean(agenthook.ProjectFile(dir, client)))
		return strings.Contains(string(data), "agentlink.exe") // each client has its own file
	}

	apply("claude", work)
	if !has(work, "claude") || !has(dev, "claude") || has(work, "codex") {
		t.Fatal("claude hooks not installed in the working and project folders")
	}
	if _, err := os.Stat(filepath.Join(root, "missing")); err == nil {
		t.Fatal("a missing project folder was created")
	}
	_, raw := h.do(t, http.MethodGet, "/ui/api/hooks", "", h.tokenHdr())
	var st HookStatus
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		t.Fatal(err)
	}
	if st.Client != "claude" || st.WorkDir != HookOK || st.Projects["dev"] != HookOK || st.Projects["gone"] != HookNoFolder {
		t.Fatalf("status %s", raw)
	}

	// A new working folder: the old one loses the entries, the new one gets them.
	apply("claude", work2)
	if has(work, "claude") || !has(work2, "claude") || !has(dev, "claude") {
		t.Fatal("working folder change not followed")
	}
	// Another agent: Claude's entries go, Codex's come.
	apply("codex", work2)
	if has(work2, "claude") || has(dev, "claude") || !has(work2, "codex") || !has(dev, "codex") {
		t.Fatal("agent change not followed")
	}
	// No agent answers: no hooks anywhere.
	apply("none", work2)
	if has(work2, "codex") || has(dev, "codex") || h.app.HookStatus().Client != "" {
		t.Fatal("hooks left without an agent")
	}

	// A start with saved settings installs them too.
	apply("claude", work)
	if err := os.Remove(agenthook.ProjectFile(work, "claude")); err != nil {
		t.Fatal(err)
	}
	h.app.Stop()
	a, err := New(h.path, nil)
	if err != nil {
		t.Fatal(err)
	}
	a.HookExe, a.Agents, a.Discovery = exe, settings.Finder{}, false
	if err := a.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Stop)
	if !has(work, "claude") {
		t.Fatal("start did not install the hook")
	}
}

func TestNoFolderHooksWithoutExe(t *testing.T) {
	work := t.TempDir()
	h := newHarness(t, func(a *App) { a.Agents = settings.Finder{} })
	if _, err := h.app.Apply(t.Context(), settings.Settings{Node: "alice", Handler: "claude", WorkDir: work}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(work, ".claude")); err == nil {
		t.Fatal("hooks installed without HookExe")
	}
}
