package agenthook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	orig := `{
  "model": "opus",
  "hooks": {
    "Stop": [{"hooks": [{"type": "command", "command": "other-tool <&>"}]}]
  },
  "zeta": 1
}`
	if err := os.WriteFile(path, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	exe := `C:\Program Files\agentlink\agentlink.exe`
	changed, err := Install(path, Claude, exe)
	if err != nil || !changed {
		t.Fatalf("install: %v %v", changed, err)
	}
	data, _ := os.ReadFile(filepath.Clean(path))
	got := string(data)
	if strings.Index(got, `"model"`) > strings.Index(got, `"hooks"`) || strings.Index(got, `"hooks"`) > strings.Index(got, `"zeta"`) {
		t.Fatalf("key order lost:\n%s", got)
	}
	if !strings.Contains(got, "other-tool <&>") {
		t.Fatalf("existing hook lost or escaped:\n%s", got)
	}
	var cfg struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Command     string   `json:"command"`
				Args        []string `json:"args"`
				AsyncRewake bool     `json:"asyncRewake"`
				Timeout     int      `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	for _, ev := range Events {
		groups := cfg.Hooks[ev]
		hs := groups[len(groups)-1].Hooks
		last, timeout := hs[0], Timeout
		if ev == SessionEnd {
			timeout = EndTimeout
		}
		if last.Command != "C:/Program Files/agentlink/agentlink.exe" || strings.Join(last.Args, " ") != "hook claude" || last.Timeout != timeout || last.AsyncRewake {
			t.Fatalf("%s: %+v", ev, last)
		}
		// The background waiter: Stop only. On SessionStart it would hold the
		// session's start (Claude Code waits for every SessionStart hook).
		if wantWait := ev == Stop; wantWait != (len(hs) == 2) {
			t.Fatalf("%s: handlers %+v", ev, hs)
		} else if wantWait && (strings.Join(hs[1].Args, " ") != "hook claude --wait" || !hs[1].AsyncRewake || hs[1].Timeout != WaitTimeout) {
			t.Fatalf("%s waiter: %+v", ev, hs[1])
		}
	}
	if len(cfg.Hooks[Stop]) != 2 || cfg.Hooks[PostTool][0].Matcher != "*" || cfg.Hooks[PreTool][0].Matcher != "*" {
		t.Fatalf("groups: %+v", cfg.Hooks)
	}
	if bak, _ := os.ReadFile(filepath.Clean(path + ".agentlink.bak")); string(bak) != orig {
		t.Fatalf("backup: %q", bak)
	}
	// Idempotent: a second install changes nothing.
	if changed, err := Install(path, Claude, exe); err != nil || changed {
		t.Fatalf("second install: %v %v", changed, err)
	}
	// A moved executable replaces the old entry instead of adding one.
	if changed, err := Install(path, Claude, `D:\bin\agentlink.exe`); err != nil || !changed {
		t.Fatalf("moved: %v %v", changed, err)
	}
	data, _ = os.ReadFile(filepath.Clean(path))
	if strings.Count(string(data), "agentlink.exe") != len(Events)+1 {
		t.Fatalf("duplicate entries:\n%s", data)
	}

	// Remove takes out only agentlink's groups; other hooks and keys stay.
	if changed, err := Remove(path, Claude); err != nil || !changed {
		t.Fatalf("remove: %v %v", changed, err)
	}
	data, _ = os.ReadFile(filepath.Clean(path))
	if strings.Contains(string(data), "agentlink") || !strings.Contains(string(data), "other-tool <&>") ||
		!strings.Contains(string(data), `"zeta"`) || strings.Contains(string(data), SessionStart) {
		t.Fatalf("after remove:\n%s", data)
	}
	if changed, err := Remove(path, Claude); err != nil || changed {
		t.Fatalf("second remove: %v %v", changed, err)
	}
	if changed, err := Remove(filepath.Join(dir, "missing.json"), Claude); err != nil || changed {
		t.Fatalf("remove from a missing file: %v %v", changed, err)
	}

	// Codex: a new file and folder, shell-form command, quoted when the path has spaces.
	cpath := ProjectFile(dir, Codex)
	if changed, err := Install(cpath, Codex, exe); err != nil || !changed {
		t.Fatalf("codex: %v %v", changed, err)
	}
	data, _ = os.ReadFile(filepath.Clean(cpath))
	if !strings.Contains(string(data), `"command": "\"C:/Program Files/agentlink/agentlink.exe\" hook codex"`) {
		t.Fatalf("codex entry:\n%s", data)
	}
	if _, err := os.Stat(cpath + ".agentlink.bak"); err == nil {
		t.Fatal("backup of a file that did not exist")
	}
	// Removing Claude's hook leaves Codex's alone; removing Codex's empties "hooks".
	if changed, err := Remove(cpath, Claude); err != nil || changed {
		t.Fatalf("remove claude from codex file: %v %v", changed, err)
	}
	if _, err := Remove(cpath, Codex); err != nil {
		t.Fatal(err)
	}
	if data, _ = os.ReadFile(filepath.Clean(cpath)); strings.TrimSpace(string(data)) != "{}" {
		t.Fatalf("codex after remove:\n%s", data)
	}
	if err := os.WriteFile(cpath, []byte("[1]"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(cpath, Codex, exe); err == nil {
		t.Fatal("a non-object file must be refused, not overwritten")
	}
}

// An install of an older agentlink (four events; or a waiter on SessionStart,
// which held the session's start) is migrated in place: every agentlink group
// is replaced, none is duplicated, other hooks stay.
func TestInstallMigratesOldEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.local.json")
	old := `{"hooks": {
  "SessionStart": [{"hooks": [{"type": "command", "command": "C:/old/agentlink.exe", "args": ["hook", "claude"], "timeout": 10}, {"type": "command", "command": "C:/old/agentlink.exe", "args": ["hook", "claude", "--wait"], "asyncRewake": true, "timeout": 86400}]}],
  "UserPromptSubmit": [{"hooks": [{"type": "command", "command": "C:/old/agentlink.exe", "args": ["hook", "claude"], "timeout": 10}]}],
  "PostToolUse": [{"matcher": "*", "hooks": [{"type": "command", "command": "C:/old/agentlink.exe", "args": ["hook", "claude"], "timeout": 10}]}],
  "Stop": [{"hooks": [{"type": "command", "command": "mine"}]}, {"hooks": [{"type": "command", "command": "C:/old/agentlink.exe", "args": ["hook", "claude"], "timeout": 10}]}]
}}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		changed, err := Install(path, Claude, `C:\new\agentlink.exe`)
		if err != nil || changed != (i == 0) {
			t.Fatalf("install %d: %v %v", i, changed, err)
		}
	}
	data, _ := os.ReadFile(filepath.Clean(path))
	got := string(data)
	if strings.Contains(got, "C:/old") || strings.Count(got, "C:/new/agentlink.exe") != len(Events)+1 || strings.Count(got, `"--wait"`) != 1 ||
		!strings.Contains(got, `"mine"`) || !strings.Contains(got, `"asyncRewake": true`) || !strings.Contains(got, SessionEnd) {
		t.Fatalf("migrated:\n%s", got)
	}
}

func TestProjectFile(t *testing.T) {
	if got := ProjectFile(`C:\p`, Claude); got != filepath.Join(`C:\p`, ".claude", "settings.local.json") {
		t.Fatal(got)
	}
	if got := ProjectFile(`C:\p`, Codex); got != filepath.Join(`C:\p`, ".codex", "hooks.json") {
		t.Fatal(got)
	}
}

func TestExcludeFromGit(t *testing.T) {
	dir := t.TempDir()
	file := ProjectFile(dir, Claude)
	// Not a repository: nothing is written.
	if err := ExcludeFromGit(dir, file); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		t.Fatal(".git created in a folder that is no repository")
	}
	exclude := filepath.Join(dir, ".git", "info", "exclude")
	if err := os.MkdirAll(filepath.Dir(exclude), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exclude, []byte("# git ls-files --others\n*.log"), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := ExcludeFromGit(dir, file); err != nil {
			t.Fatal(err)
		}
	}
	data, _ := os.ReadFile(filepath.Clean(exclude))
	if string(data) != "# git ls-files --others\n*.log\n/.claude/settings.local.json\n" {
		t.Fatalf("exclude:\n%s", data)
	}
}
