package settings

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/UberMorgott/agent-link/internal/worker"
)

func touch(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("@echo off\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCommandAgentPath(t *testing.T) {
	s := valid(t)
	s.Handler, s.AgentPath = worker.HandlerCodex, `C:\tools\codex.cmd`
	c, ok := s.Command()
	if !ok || c.Name != `C:\tools\codex.cmd` || !slices.Equal(c.Args, worker.Codex.Args) {
		t.Fatalf("agent path: %+v %v", c, ok)
	}
	if worker.Codex.Name != "codex" {
		t.Fatal("the built-in command was modified")
	}
	s.HandlerCommand = []string{"fake.exe", "--echo"}
	if c, _ := s.Command(); c.Name != "fake.exe" {
		t.Fatalf("handler command must win over agent path: %+v", c)
	}
}

func TestValidateAgentPath(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	dir := t.TempDir()
	exe := touch(t, filepath.Join(dir, "codex.cmd"))
	s := valid(t)
	s.Handler, s.AgentPath = worker.HandlerCodex, exe
	if err := s.Validate(); err != nil {
		t.Fatalf("existing agent path off PATH: %v", err)
	}
	for name, p := range map[string]string{
		"missing":   filepath.Join(dir, "absent.exe"),
		"directory": dir,
		"relative":  "codex.cmd",
	} {
		s.AgentPath = p
		var pr *Problem
		if err := s.Validate(); !errors.As(err, &pr) || pr.Key != "agent_path" {
			t.Errorf("%s: Validate() = %v, want agent_path", name, err)
		}
	}
	s.AgentPath, s.HandlerCommand = filepath.Join(dir, "absent.exe"), []string{"fake"}
	if err := s.Validate(); err != nil {
		t.Fatalf("handler command must skip the agent path check: %v", err)
	}
	if n := (Settings{AgentPath: "  " + exe + " "}).Normalize(); n.AgentPath != exe {
		t.Fatalf("agent path not trimmed: %q", n.AgentPath)
	}
}

// fakeFinder has nothing on PATH and the given environment.
func fakeFinder(onPath map[string]string, env map[string]string) Finder {
	return Finder{
		LookPath: func(name string) (string, error) {
			if p, ok := onPath[name]; ok {
				return p, nil
			}
			return "", errors.New("not found")
		},
		Stat:   os.Stat,
		Getenv: func(k string) string { return env[k] },
	}
}

func TestDiscover(t *testing.T) {
	appdata, profile := t.TempDir(), t.TempDir()
	env := map[string]string{"APPDATA": appdata, "USERPROFILE": profile}
	f := fakeFinder(nil, env)
	if got := f.Discover(worker.HandlerCodex); got != "" {
		t.Fatalf("nothing installed: %q", got)
	}
	codex := touch(t, filepath.Join(appdata, `npm\codex.cmd`))
	if got := f.Discover(worker.HandlerCodex); got != codex {
		t.Fatalf("npm codex: %q, want %q", got, codex)
	}
	npmClaude := touch(t, filepath.Join(appdata, `npm\claude.cmd`))
	if got := f.Discover(worker.HandlerClaude); got != npmClaude {
		t.Fatalf("npm claude: %q", got)
	}
	native := touch(t, filepath.Join(profile, `.local\bin\claude.exe`))
	if got := f.Discover(worker.HandlerClaude); got != native {
		t.Fatalf("native claude must come first: %q", got)
	}
	if got := fakeFinder(map[string]string{"codex": "codex.cmd"}, env).Discover(worker.HandlerCodex); got != "" {
		t.Fatalf("on PATH needs no agent path: %q", got)
	}
	if got := f.Discover(worker.HandlerNone); got != "" {
		t.Fatalf("no handler: %q", got)
	}
	if c := fakeFinder(nil, nil).Candidates(worker.HandlerClaude); len(c) != 0 {
		t.Fatalf("candidates without environment: %v", c)
	}
	// A directory at the candidate path is not a program.
	dirOnly := fakeFinder(nil, map[string]string{"APPDATA": t.TempDir()})
	if err := os.MkdirAll(filepath.Join(dirOnly.Getenv("APPDATA"), `npm\codex.cmd`), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := dirOnly.Discover(worker.HandlerCodex); got != "" {
		t.Fatalf("directory taken for a program: %q", got)
	}
}

func TestResolve(t *testing.T) {
	exe := touch(t, filepath.Join(t.TempDir(), "codex.exe"))
	onPath := filepath.Join(t.TempDir(), "codex.cmd")
	f := fakeFinder(map[string]string{"codex": onPath}, nil)
	cases := []struct {
		name, handler, agentPath, path, source string
	}{
		{"setting wins", worker.HandlerCodex, exe, exe, AgentFromSetting},
		{"setting gone", worker.HandlerCodex, exe + ".old", exe + ".old", AgentMissing},
		{"on path", worker.HandlerCodex, "", onPath, AgentFromPath},
		{"missing", worker.HandlerClaude, "", "", AgentMissing},
	}
	for _, c := range cases {
		if p, src := f.Resolve(c.handler, c.agentPath); p != c.path || src != c.source {
			t.Errorf("%s: Resolve = %q %q, want %q %q", c.name, p, src, c.path, c.source)
		}
	}
}
