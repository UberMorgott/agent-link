package app

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/UberMorgott/agent-link/internal/settings"
)

// offPathFinder sees no agent on PATH and codex at the npm location under appdata.
func offPathFinder(appdata string) settings.Finder {
	return settings.Finder{
		LookPath: func(string) (string, error) { return "", errors.New("not found") },
		Stat:     os.Stat,
		Getenv: func(k string) string {
			if k == "APPDATA" {
				return appdata
			}
			return ""
		},
	}
}

func codexJSON(t *testing.T, agentPath string) string {
	t.Helper()
	b, _ := json.Marshal(map[string]any{
		"node": "alice", "code": "ABC123", "listen": "127.0.0.1:0", "handler": "codex",
		"work_dir": t.TempDir(), "agent_path": agentPath,
	})
	return string(b)
}

func TestSaveDiscoversAgentOffPath(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // Validate's own PATH check must not find the real codex
	appdata := t.TempDir()
	h := newHarness(t, func(a *App) { a.Agents = offPathFinder(appdata) })

	code, body := h.do(t, http.MethodPost, "/ui/api/settings", codexJSON(t, ""), h.tokenHdr())
	var r saveResult
	_ = json.Unmarshal([]byte(body), &r)
	if code != http.StatusBadRequest || r.Error != msg("error.handler_missing", nil) {
		t.Fatalf("nothing installed: %d %s", code, body)
	}

	exe := filepath.Join(appdata, `npm\codex.cmd`)
	if err := os.MkdirAll(filepath.Dir(exe), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("@echo off\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, body = h.do(t, http.MethodPost, "/ui/api/settings", codexJSON(t, ""), h.tokenHdr())
	r = saveResult{}
	_ = json.Unmarshal([]byte(body), &r)
	if code != http.StatusOK || !r.Saved || r.Found != msg("settings.agent.found", map[string]string{"path": exe}) {
		t.Fatalf("discovered: %d %s", code, body)
	}
	if s, _, _ := settings.Load(h.path); s.AgentPath != exe {
		t.Fatalf("agent path stored as %q", s.AgentPath)
	}
	if c, _ := h.app.Settings().Command(); c.Name != exe {
		t.Fatalf("worker command %q", c.Name)
	}

	// A path the user chose is kept as is and nothing is searched.
	chosen := filepath.Join(t.TempDir(), "codex.exe")
	if err := os.WriteFile(chosen, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	code, body = h.do(t, http.MethodPost, "/ui/api/settings", codexJSON(t, chosen), h.tokenHdr())
	r = saveResult{}
	_ = json.Unmarshal([]byte(body), &r)
	if code != http.StatusOK || r.Found != "" || h.app.Settings().AgentPath != chosen {
		t.Fatalf("chosen: %d %s", code, body)
	}
	code, body = h.do(t, http.MethodPost, "/ui/api/settings", codexJSON(t, chosen+".gone"), h.tokenHdr())
	r = saveResult{}
	_ = json.Unmarshal([]byte(body), &r)
	if code != http.StatusBadRequest || r.Error != msg("error.agent_path", nil) {
		t.Fatalf("gone: %d %s", code, body)
	}
}

func TestAgentInfo(t *testing.T) {
	onPath := filepath.Join(t.TempDir(), "claude.exe")
	chosen := filepath.Join(t.TempDir(), "codex.cmd")
	if err := os.WriteFile(chosen, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, func(a *App) {
		a.Agents = settings.Finder{
			LookPath: func(name string) (string, error) {
				if name == "claude" {
					return onPath, nil
				}
				return "", errors.New("not found")
			},
			Stat: os.Stat, Getenv: func(string) string { return "" },
		}
	})
	cases := []struct {
		handler, agentPath string
		want               agentInfo
	}{
		{"none", "", agentInfo{}},
		{"claude", "", agentInfo{onPath, settings.AgentFromPath, msg("settings.agent.from_path", map[string]string{"path": onPath})}},
		{"codex", chosen, agentInfo{chosen, settings.AgentFromSetting, msg("settings.agent.from_setting", map[string]string{"path": chosen})}},
		{"codex", chosen + "x", agentInfo{chosen + "x", settings.AgentMissing, msg("settings.agent.gone", map[string]string{"path": chosen + "x"})}},
		{"codex", "", agentInfo{"", settings.AgentMissing, msg("settings.agent.missing", nil)}},
	}
	for _, c := range cases {
		b, _ := json.Marshal(map[string]string{"handler": c.handler, "agent_path": c.agentPath})
		code, body := h.do(t, http.MethodPost, "/ui/api/agent", string(b), h.tokenHdr())
		var got agentInfo
		_ = json.Unmarshal([]byte(body), &got)
		if code != http.StatusOK || got != c.want {
			t.Errorf("%s %q: %d %+v, want %+v", c.handler, c.agentPath, code, got, c.want)
		}
	}
	if code, _ := h.do(t, http.MethodPost, "/ui/api/agent", `{"handler":"codex"}`, nil); code != http.StatusForbidden {
		t.Fatalf("no token: %d", code)
	}
}

func pickFileHarness(t *testing.T, pick func(start, title, name, spec string) (string, error)) *harness {
	t.Helper()
	return newHarness(t, func(a *App) { a.PickFile = pick })
}

func TestPickAgentGuard(t *testing.T) {
	called := false
	h := pickFileHarness(t, func(string, string, string, string) (string, error) { called = true; return `C:\x.exe`, nil })
	for name, hdr := range map[string]map[string]string{
		"no token":     nil,
		"wrong token":  {TokenHeader: "nope"},
		"cross origin": {TokenHeader: h.app.token, "Origin": "http://evil.example"},
		"cross site":   {TokenHeader: h.app.token, "Sec-Fetch-Site": "cross-site"},
	} {
		if code, _ := h.do(t, http.MethodPost, "/ui/api/pick-agent", `{}`, hdr); code != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403", name, code)
		}
	}
	if code, _ := h.do(t, http.MethodGet, "/ui/api/pick-agent", "", h.tokenHdr()); code != http.StatusMethodNotAllowed {
		t.Errorf("GET: status %d, want 405", code)
	}
	if called {
		t.Fatal("dialog opened for a rejected request")
	}
}

func TestPickAgentReturnsProgram(t *testing.T) {
	var got [4]string
	h := pickFileHarness(t, func(start, title, name, spec string) (string, error) {
		got = [4]string{start, title, name, spec}
		return `C:\Users\n\AppData\Roaming\npm\codex.cmd`, nil
	})
	code, body := h.do(t, http.MethodPost, "/ui/api/pick-agent", `{"start":" C:\\tools\\codex.exe "}`, h.tokenHdr())
	if code != http.StatusOK || decodePick(t, body).Path != `C:\Users\n\AppData\Roaming\npm\codex.cmd` {
		t.Fatalf("pick: %d %s", code, body)
	}
	want := [4]string{filepath.Dir(`C:\tools\codex.exe`), msg("settings.agent.pick_title", nil), msg("settings.agent.filter", nil), "*.exe;*.cmd;*.bat"}
	if got != want {
		t.Fatalf("dialog args %q, want %q", got, want)
	}
}

func TestPickAgentCancelAndFailures(t *testing.T) {
	h := pickFileHarness(t, func(string, string, string, string) (string, error) { return "", ErrPickCancelled })
	code, body := h.do(t, http.MethodPost, "/ui/api/pick-agent", `{}`, h.tokenHdr())
	if r := decodePick(t, body); code != http.StatusOK || !r.Cancelled || r.Message != msg("settings.agent.cancelled", nil) {
		t.Fatalf("cancel: %d %s", code, body)
	}
	for _, c := range []struct {
		pick func(string, string, string, string) (string, error)
		code int
		key  string
	}{
		{nil, http.StatusNotImplemented, "error.pick_agent_unsupported"},
		{func(string, string, string, string) (string, error) { return "", ErrPickUnsupported }, http.StatusNotImplemented, "error.pick_agent_unsupported"},
		{func(string, string, string, string) (string, error) { return "", errors.New("COM broke") }, http.StatusInternalServerError, "error.pick_agent"},
	} {
		h := pickFileHarness(t, c.pick)
		code, body := h.do(t, http.MethodPost, "/ui/api/pick-agent", `{}`, h.tokenHdr())
		var e map[string]string
		_ = json.Unmarshal([]byte(body), &e)
		if code != c.code || e["error"] != msg(c.key, nil) {
			t.Errorf("%s: %d %s", c.key, code, body)
		}
	}
}

// The folder and the program dialogs share the one-at-a-time guard.
func TestPickAgentBusyWhileFolderDialogOpen(t *testing.T) {
	opened, release := make(chan struct{}), make(chan struct{})
	h := newHarness(t, func(a *App) {
		a.PickFolder = func(string, string) (string, error) { opened <- struct{}{}; <-release; return `C:\p`, nil }
		a.PickFile = func(string, string, string, string) (string, error) { return `C:\x.exe`, nil }
	})
	done := make(chan int, 1)
	go func() {
		code, _ := h.do(t, http.MethodPost, "/ui/api/pick-folder", `{}`, h.tokenHdr())
		done <- code
	}()
	<-opened
	code, body := h.do(t, http.MethodPost, "/ui/api/pick-agent", `{}`, h.tokenHdr())
	if code != http.StatusConflict || !strings.Contains(body, jsonText(msg("error.pick_busy", nil))) {
		t.Fatalf("program dialog during folder dialog: %d %s", code, body)
	}
	close(release)
	if c := <-done; c != http.StatusOK {
		t.Fatalf("folder dialog: %d", c)
	}
	if code, _ := h.do(t, http.MethodPost, "/ui/api/pick-agent", `{}`, h.tokenHdr()); code != http.StatusOK {
		t.Fatalf("after close: %d", code)
	}
}

func jsonText(s string) string {
	b, _ := json.Marshal(s)
	return strings.Trim(string(b), `"`)
}
