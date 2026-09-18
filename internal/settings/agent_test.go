package settings

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

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

// fakeFile is a regular file (or, with dir, a directory) in a fakeFS.
type fakeFile struct {
	mod time.Time
	dir bool
}

func (f fakeFile) Name() string       { return "" }
func (f fakeFile) Size() int64        { return 0 }
func (f fakeFile) ModTime() time.Time { return f.mod }
func (f fakeFile) IsDir() bool        { return f.dir }
func (f fakeFile) Sys() any           { return nil }
func (f fakeFile) Mode() fs.FileMode {
	if f.dir {
		return fs.ModeDir
	}
	return 0
}

// fakeFS is a Finder over an in-memory disk: files maps a path to its age in
// hours, versions maps a path to its --version output (absent: the program fails).
type fakeFS struct {
	files    map[string]int
	dirs     map[string]bool
	versions map[string]string
	onPath   map[string]string
	ran      []string
}

var errNotFound = errors.New("not found")

var fakeEnv = map[string]string{
	"APPDATA": `C:\U\AppData\Roaming`, "LOCALAPPDATA": `C:\U\AppData\Local`, "USERPROFILE": `C:\U`,
}

func (x *fakeFS) finder() Finder {
	epoch := time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC)
	stat := func(p string) (fs.FileInfo, error) {
		if x.dirs[p] {
			return fakeFile{dir: true}, nil
		}
		if age, ok := x.files[p]; ok {
			return fakeFile{mod: epoch.Add(-time.Duration(age) * time.Hour)}, nil
		}
		return nil, fs.ErrNotExist
	}
	return Finder{
		LookPath: func(name string) (string, error) {
			if p, ok := x.onPath[name]; ok {
				return p, nil
			}
			return "", errNotFound
		},
		Stat:   stat,
		Getenv: func(k string) string { return fakeEnv[k] },
		Glob: func(pattern string) ([]string, error) {
			var out []string
			for p := range x.files {
				if ok, _ := filepath.Match(pattern, p); ok {
					out = append(out, p)
				}
			}
			slices.Sort(out)
			return out, nil
		},
		Version: func(p string) (string, error) {
			x.ran = append(x.ran, p)
			if v, ok := x.versions[p]; ok {
				return v, nil
			}
			return "", errNotFound
		},
	}
}

const (
	codexOut  = "codex-cli 0.154.0"
	claudeOut = "2.1.276 (Claude Code)"
)

func TestDiscoverInstallKinds(t *testing.T) {
	codexApp1 := `C:\U\AppData\Local\OpenAI\Codex\bin\12219cbfbcbddde7\codex.exe`
	codexApp2 := `C:\U\AppData\Local\OpenAI\Codex\bin\9a0b1c2d3e4f5a6b\codex.exe`
	ext := func(editor, ver string) string {
		return `C:\U\` + editor + `\extensions\openai.chatgpt-` + ver + `-win32-x64\bin\windows-x86_64\codex.exe`
	}
	claudeExt := func(editor, ver string) string {
		return `C:\U\` + editor + `\extensions\anthropic.claude-code-` + ver + `-win32-x64\resources\native-binary\claude.exe`
	}
	claudeApp := func(ver string) string { return `C:\U\AppData\Roaming\Claude\claude-code\` + ver + `\claude.exe` }
	cases := []struct {
		name     string
		handler  string
		files    map[string]int    // path -> age in hours
		versions map[string]string // path -> --version output; absent fails
		onPath   map[string]string
		want     Found
		ok       bool
	}{
		{name: "nothing", handler: worker.HandlerCodex},
		{name: "no handler", handler: worker.HandlerNone, files: map[string]int{`C:\U\AppData\Roaming\npm\codex.cmd`: 1}},
		{
			name: "codex on PATH", handler: worker.HandlerCodex,
			onPath:   map[string]string{"codex": `C:\bin\codex.exe`},
			files:    map[string]int{`C:\U\AppData\Roaming\npm\codex.cmd`: 1},
			versions: map[string]string{`C:\bin\codex.exe`: codexOut, `C:\U\AppData\Roaming\npm\codex.cmd`: codexOut},
			want:     Found{`C:\bin\codex.exe`, KindPath, codexOut}, ok: true,
		},
		{
			name: "codex standalone installer", handler: worker.HandlerCodex,
			files:    map[string]int{`C:\U\AppData\Local\Programs\OpenAI\Codex\bin\codex.exe`: 1, `C:\U\AppData\Roaming\npm\codex.cmd`: 1},
			versions: map[string]string{`C:\U\AppData\Local\Programs\OpenAI\Codex\bin\codex.exe`: codexOut, `C:\U\AppData\Roaming\npm\codex.cmd`: codexOut},
			want:     Found{`C:\U\AppData\Local\Programs\OpenAI\Codex\bin\codex.exe`, KindStandalone, codexOut}, ok: true,
		},
		{
			name: "codex npm", handler: worker.HandlerCodex,
			files:    map[string]int{`C:\U\AppData\Roaming\npm\codex.cmd`: 1, codexApp1: 1},
			versions: map[string]string{`C:\U\AppData\Roaming\npm\codex.cmd`: codexOut, codexApp1: codexOut},
			want:     Found{`C:\U\AppData\Roaming\npm\codex.cmd`, KindNpm, codexOut}, ok: true,
		},
		{
			name: "codex winget", handler: worker.HandlerCodex,
			files:    map[string]int{`C:\U\AppData\Local\Microsoft\WinGet\Links\codex.exe`: 1},
			versions: map[string]string{`C:\U\AppData\Local\Microsoft\WinGet\Links\codex.exe`: codexOut},
			want:     Found{`C:\U\AppData\Local\Microsoft\WinGet\Links\codex.exe`, KindWinget, codexOut}, ok: true,
		},
		{
			name: "codex app, newest hash folder by time", handler: worker.HandlerCodex,
			files:    map[string]int{codexApp1: 48, codexApp2: 2, ext(".vscode", "26.5.1"): 1},
			versions: map[string]string{codexApp1: codexOut, codexApp2: codexOut, ext(".vscode", "26.5.1"): codexOut},
			want:     Found{codexApp2, KindCodexApp, codexOut}, ok: true,
		},
		{
			name: "codex VS Code extension, newest version", handler: worker.HandlerCodex,
			files:    map[string]int{ext(".vscode", "26.5.9"): 1, ext(".vscode", "26.5.10"): 90},
			versions: map[string]string{ext(".vscode", "26.5.9"): codexOut, ext(".vscode", "26.5.10"): codexOut},
			want:     Found{ext(".vscode", "26.5.10"), KindVSCode, codexOut}, ok: true,
		},
		{
			name: "codex Cursor extension", handler: worker.HandlerCodex,
			files:    map[string]int{ext(".cursor", "26.5.1"): 1},
			versions: map[string]string{ext(".cursor", "26.5.1"): codexOut},
			want:     Found{ext(".cursor", "26.5.1"), KindCursor, codexOut}, ok: true,
		},
		{
			name: "codex --version fails, next one runs", handler: worker.HandlerCodex,
			files:    map[string]int{`C:\U\AppData\Roaming\npm\codex.cmd`: 1, codexApp1: 1},
			versions: map[string]string{codexApp1: codexOut},
			want:     Found{codexApp1, KindCodexApp, codexOut}, ok: true,
		},
		{
			name: "codex --version names another program", handler: worker.HandlerCodex,
			files:    map[string]int{`C:\U\AppData\Roaming\npm\codex.cmd`: 1},
			versions: map[string]string{`C:\U\AppData\Roaming\npm\codex.cmd`: "Claude 2.2553.1"},
		},
		{
			name: "claude native before npm", handler: worker.HandlerClaude,
			files:    map[string]int{`C:\U\.local\bin\claude.exe`: 1, `C:\U\AppData\Roaming\npm\claude.cmd`: 1},
			versions: map[string]string{`C:\U\.local\bin\claude.exe`: claudeOut, `C:\U\AppData\Roaming\npm\claude.cmd`: claudeOut},
			want:     Found{`C:\U\.local\bin\claude.exe`, KindStandalone, claudeOut}, ok: true,
		},
		{
			name: "claude npm", handler: worker.HandlerClaude,
			files:    map[string]int{`C:\U\AppData\Roaming\npm\claude.cmd`: 1},
			versions: map[string]string{`C:\U\AppData\Roaming\npm\claude.cmd`: claudeOut},
			want:     Found{`C:\U\AppData\Roaming\npm\claude.cmd`, KindNpm, claudeOut}, ok: true,
		},
		{
			name: "claude winget", handler: worker.HandlerClaude,
			files:    map[string]int{`C:\U\AppData\Local\Microsoft\WinGet\Links\claude.exe`: 1},
			versions: map[string]string{`C:\U\AppData\Local\Microsoft\WinGet\Links\claude.exe`: claudeOut},
			want:     Found{`C:\U\AppData\Local\Microsoft\WinGet\Links\claude.exe`, KindWinget, claudeOut}, ok: true,
		},
		{
			name: "claude desktop app, newest version before extension", handler: worker.HandlerClaude,
			files:    map[string]int{claudeApp("2.1.274"): 1, claudeApp("2.1.275"): 50, claudeExt(".vscode", "2.1.276"): 1},
			versions: map[string]string{claudeApp("2.1.274"): claudeOut, claudeApp("2.1.275"): claudeOut, claudeExt(".vscode", "2.1.276"): claudeOut},
			want:     Found{claudeApp("2.1.275"), KindClaudeApp, claudeOut}, ok: true,
		},
		{
			name: "claude VS Code extension", handler: worker.HandlerClaude,
			files:    map[string]int{claudeExt(".vscode", "2.1.276"): 1},
			versions: map[string]string{claudeExt(".vscode", "2.1.276"): claudeOut},
			want:     Found{claudeExt(".vscode", "2.1.276"), KindVSCode, claudeOut}, ok: true,
		},
		{
			name: "claude desktop app alias on PATH is skipped", handler: worker.HandlerClaude,
			onPath:   map[string]string{"claude": `C:\U\AppData\Local\Microsoft\WindowsApps\claude.exe`},
			versions: map[string]string{`C:\U\AppData\Local\Microsoft\WindowsApps\claude.exe`: claudeOut},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			x := &fakeFS{files: c.files, versions: c.versions, onPath: c.onPath}
			got, ok := x.finder().Discover(c.handler)
			if ok != c.ok || got != c.want {
				t.Fatalf("Discover = %+v %v, want %+v %v (ran %v)", got, ok, c.want, c.ok, x.ran)
			}
			if ok && got.Kind != KindPath {
				if k := x.finder().Kind(c.handler, got.Path); k != got.Kind {
					t.Fatalf("Kind(%s) = %q, want %q", got.Path, k, got.Kind)
				}
			}
		})
	}
}

func TestDiscoverSkipsDirectories(t *testing.T) {
	x := &fakeFS{
		dirs:     map[string]bool{`C:\U\AppData\Roaming\npm\codex.cmd`: true},
		versions: map[string]string{`C:\U\AppData\Roaming\npm\codex.cmd`: codexOut},
	}
	if got, ok := x.finder().Discover(worker.HandlerCodex); ok {
		t.Fatalf("directory taken for a program: %+v", got)
	}
	if c := fakeFinder(nil, nil).Candidates(worker.HandlerClaude); len(c) != 0 {
		t.Fatalf("candidates without environment: %v", c)
	}
}

func TestKindUnknownPath(t *testing.T) {
	if k := (&fakeFS{}).finder().Kind(worker.HandlerCodex, `D:\tools\codex.exe`); k != "" {
		t.Fatalf("Kind = %q", k)
	}
}

// The real probe: a program that fails --version is rejected.
func TestSystemVersionRejectsFailure(t *testing.T) {
	if _, err := SystemFinder.Version(filepath.Join(t.TempDir(), "absent.exe")); err == nil {
		t.Fatal("missing program passed --version")
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
