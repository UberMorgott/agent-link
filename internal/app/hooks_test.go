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
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // no agent-link plugin
	root := t.TempDir()
	work, work2, dev := filepath.Join(root, "work"), filepath.Join(root, "work2"), filepath.Join(root, "dev")
	for _, d := range []string{work, work2, dev} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	exe := filepath.Join(root, "bin", "agentlink.exe") // never run: only written into the hook entries
	agent := fakeAgentFile(t)
	h := newHarness(t, func(a *App) { a.HookExe, a.Agents, a.s.Code = exe, settings.Finder{}, "" })
	apply := func(handler, workDir string) {
		t.Helper()
		agentPath := agent
		if handler == "none" {
			agentPath = ""
		}
		s := settings.Settings{Node: "alice", Handler: handler, AgentPath: agentPath, WorkDir: workDir,
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
	// Every event plus Claude's background waiter; a second sync changes nothing.
	file := agenthook.ProjectFile(work, "claude")
	before, _ := os.ReadFile(filepath.Clean(file))
	for _, want := range []string{agenthook.PreTool, agenthook.SessionEnd, `"asyncRewake": true`, `"--wait"`} {
		if !strings.Contains(string(before), want) {
			t.Fatalf("folder hook lacks %s:\n%s", want, before)
		}
	}
	apply("claude", work)
	if after, _ := os.ReadFile(filepath.Clean(file)); string(after) != string(before) {
		t.Fatalf("second sync changed the file:\n%s", after)
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

// With Claude Code's agent-link plugin enabled the plugin brings the hooks:
// the app writes no folder entries and takes out the ones it wrote before,
// keeping the folder's other hooks; turning the plugin off brings them back.
func TestNoFolderHooksWithPlugin(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	setPlugin := func(on bool) {
		t.Helper()
		settings := `{"enabledPlugins":{"agent-link@agent-link":false}}`
		if on {
			settings = `{"enabledPlugins":{"agent-link@agent-link":true}}`
		}
		files := map[string]string{
			filepath.Join(cfg, "settings.json"):                     settings,
			filepath.Join(cfg, "plugins", "installed_plugins.json"): `{"version":2,"plugins":{"agent-link@agent-link":[{"scope":"user"}]}}`,
		}
		for path, data := range files {
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	work := t.TempDir()
	file := agenthook.ProjectFile(work, "claude")
	exe := filepath.Join(t.TempDir(), "agentlink.exe")
	h := newHarness(t, func(a *App) { a.HookExe, a.Agents, a.s.Code = exe, settings.Finder{}, "" })
	s := settings.Settings{Node: "alice", Handler: "claude", AgentPath: fakeAgentFile(t), WorkDir: work}
	apply := func() {
		t.Helper()
		if _, err := h.app.Apply(t.Context(), s); err != nil {
			t.Fatal(err)
		}
	}
	read := func() string {
		data, _ := os.ReadFile(filepath.Clean(file))
		return string(data)
	}

	// Someone else's hook in the same file stays throughout.
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	other := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"notify"}]}]}}`
	if err := os.WriteFile(file, []byte(other), 0o600); err != nil {
		t.Fatal(err)
	}
	setPlugin(false)
	apply()
	if got := read(); !strings.Contains(got, "agentlink.exe") || !strings.Contains(got, `"notify"`) {
		t.Fatalf("plugin off: folder file\n%s", got)
	}

	setPlugin(true)
	apply()
	if got := read(); strings.Contains(got, "agentlink") || !strings.Contains(got, `"notify"`) {
		t.Fatalf("plugin on: folder file\n%s", got)
	}
	if h.app.HookStatus().Client != "" {
		t.Fatal("plugin on: status still reports folder hooks")
	}

	setPlugin(false)
	apply()
	if got := read(); !strings.Contains(got, "agentlink.exe") || !strings.Contains(got, `"notify"`) {
		t.Fatalf("plugin off again: folder file\n%s", got)
	}
}

func TestNoFolderHooksWithoutExe(t *testing.T) {
	work := t.TempDir()
	h := newHarness(t, func(a *App) { a.Agents, a.s.Code = settings.Finder{}, "" })
	s := settings.Settings{Node: "alice", Handler: "claude", AgentPath: fakeAgentFile(t), WorkDir: work}
	if _, err := h.app.Apply(t.Context(), s); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(work, ".claude")); err == nil {
		t.Fatal("hooks installed without HookExe")
	}
}

// fakeAgentFile is an agent program path that passes settings validation
// on machines (CI) without claude or codex on PATH; it is never run.
func fakeAgentFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "agent.exe")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
