package settings

import (
	"cmp"
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/UberMorgott/agent-link/internal/worker"
)

// Agent program sources reported by Finder.Resolve.
const (
	AgentFromPath    = "path"       // found on PATH under the handler's name
	AgentFromSetting = "agent_path" // the AgentPath setting, and the file exists
	AgentMissing     = "missing"    // neither
)

// Install kinds of an agent program, from the most to the least preferred
// after PATH. The web UI names each in plain words.
const (
	KindPath       = "path"       // on PATH
	KindEnv        = "env"        // CODEX_CLI_PATH
	KindStandalone = "standalone" // the vendor's own CLI installer
	KindNpm        = "npm"
	KindWinget     = "winget"
	KindCodexApp   = "codex_app"  // the Codex desktop app's copy of its CLI
	KindClaudeApp  = "claude_app" // the Claude desktop app's copy of Claude Code
	KindVSCode     = "vscode"     // an editor extension's bundled CLI
	KindVSCodeIns  = "vscode_insiders"
	KindCursor     = "cursor"
	KindWindsurf   = "windsurf"
)

// Found is an agent program that answered --version.
type Found struct {
	Path, Kind, Version string
}

// Finder locates agent programs. The functions are replaceable in tests.
type Finder struct {
	LookPath func(string) (string, error)
	Stat     func(string) (fs.FileInfo, error)
	Getenv   func(string) string
	// Glob expands a pattern with one "*" path segment; nil uses filepath.Glob.
	Glob func(string) ([]string, error)
	// Version runs "<path> --version" and returns its output; nil accepts
	// every existing file (tests that do not care).
	Version func(path string) (string, error)
}

// SystemFinder looks at the real PATH, disk and environment and runs the
// candidates to check them.
var SystemFinder = Finder{
	LookPath: exec.LookPath, Stat: os.Stat, Getenv: os.Getenv, Glob: filepath.Glob,
	Version: func(path string) (string, error) { return worker.Version(context.Background(), path) },
}

// location is one install location: Rel under the directory in Env (Rel empty:
// Env holds the program's own path). One segment of Rel may be a "*" pattern
// for a versioned folder; the newest match is tried first.
type location struct{ Kind, Env, Rel string }

// locations lists where each install method puts the agent on Windows, in
// order of preference after PATH (standalone CLI, app bundle, editor extension).
// Sources are in README "Handler agent"; every entry was seen on a real
// install or in the vendor's installer or docs. The tray started from Explorer
// or autostart may not see a PATH entry an installer added later.
var locations = map[string][]location{
	worker.HandlerCodex: {
		{KindEnv, "CODEX_CLI_PATH", ""},
		{KindStandalone, "CODEX_INSTALL_DIR", `codex.exe`},
		{KindStandalone, "LOCALAPPDATA", `Programs\OpenAI\Codex\bin\codex.exe`},
		{KindNpm, "APPDATA", `npm\codex.cmd`},
		{KindWinget, "LOCALAPPDATA", `Microsoft\WinGet\Links\codex.exe`},
		{KindCodexApp, "LOCALAPPDATA", `OpenAI\Codex\bin\*\codex.exe`},
		{KindVSCode, "USERPROFILE", `.vscode\extensions\openai.chatgpt-*\bin\windows-x86_64\codex.exe`},
		{KindVSCodeIns, "USERPROFILE", `.vscode-insiders\extensions\openai.chatgpt-*\bin\windows-x86_64\codex.exe`},
		{KindCursor, "USERPROFILE", `.cursor\extensions\openai.chatgpt-*\bin\windows-x86_64\codex.exe`},
		{KindWindsurf, "USERPROFILE", `.windsurf\extensions\openai.chatgpt-*\bin\windows-x86_64\codex.exe`},
	},
	worker.HandlerClaude: {
		{KindStandalone, "USERPROFILE", `.local\bin\claude.exe`},
		{KindNpm, "APPDATA", `npm\claude.cmd`},
		{KindWinget, "LOCALAPPDATA", `Microsoft\WinGet\Links\claude.exe`},
		{KindClaudeApp, "APPDATA", `Claude\claude-code\*\claude.exe`},
		{KindVSCode, "USERPROFILE", `.vscode\extensions\anthropic.claude-code-*\resources\native-binary\claude.exe`},
		{KindVSCodeIns, "USERPROFILE", `.vscode-insiders\extensions\anthropic.claude-code-*\resources\native-binary\claude.exe`},
		{KindCursor, "USERPROFILE", `.cursor\extensions\anthropic.claude-code-*\resources\native-binary\claude.exe`},
		{KindWindsurf, "USERPROFILE", `.windsurf\extensions\anthropic.claude-code-*\resources\native-binary\claude.exe`},
	},
}

// versionMarks is what each agent's --version output contains; anything else
// on the same name (the Claude desktop app's own claude.exe) is not the CLI.
var versionMarks = map[string]string{
	worker.HandlerCodex:  "codex",
	worker.HandlerClaude: "claude code",
}

// Candidates returns every program of the handler's agent that exists, in
// order of preference: PATH, then the install locations, newest version first
// within a versioned folder. Nothing is run.
func (f Finder) Candidates(handler string) []Found {
	var out []Found
	if p, err := f.LookPath(handler); err == nil && !appAlias(f, p) {
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		out = append(out, Found{Path: p, Kind: KindPath})
	}
	for _, l := range locations[handler] {
		base := f.Getenv(l.Env)
		if base == "" {
			continue
		}
		pattern := filepath.Join(base, l.Rel)
		if !strings.Contains(l.Rel, "*") {
			if f.isFile(pattern) {
				out = append(out, Found{Path: pattern, Kind: l.Kind})
			}
			continue
		}
		for _, p := range f.newestFirst(base, l.Rel, pattern) {
			out = append(out, Found{Path: p, Kind: l.Kind})
		}
	}
	return out
}

// appAlias reports a PATH hit in %LOCALAPPDATA%\Microsoft\WindowsApps: a Store
// app's execution alias (the Claude desktop app registers claude.exe there),
// which would start the app, not the CLI.
func appAlias(f Finder, p string) bool {
	la := f.Getenv("LOCALAPPDATA")
	return la != "" && strings.EqualFold(filepath.Dir(p), filepath.Join(la, `Microsoft\WindowsApps`))
}

func (f Finder) isFile(p string) bool {
	st, err := f.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

var versionRe = regexp.MustCompile(`\d+(?:\.\d+)+`)

// newestFirst expands pattern and orders the files by the version in the
// "*" segment (extensions, Claude app), else by modification time (the Codex
// app's hash-named folders).
func (f Finder) newestFirst(base, rel, pattern string) []string {
	glob := f.Glob
	if glob == nil {
		glob = filepath.Glob
	}
	matches, _ := glob(pattern)
	star := slices.IndexFunc(strings.Split(rel, `\`), func(s string) bool { return strings.Contains(s, "*") })
	type hit struct {
		path string
		ver  []int
		mod  int64
	}
	var hits []hit
	for _, m := range matches {
		st, err := f.Stat(m)
		if err != nil || !st.Mode().IsRegular() {
			continue
		}
		h := hit{path: m, mod: st.ModTime().UnixNano()}
		if r, err := filepath.Rel(base, m); err == nil {
			if segs := strings.Split(r, `\`); star >= 0 && star < len(segs) {
				h.ver = parseVersion(versionRe.FindString(segs[star]))
			}
		}
		hits = append(hits, h)
	}
	slices.SortStableFunc(hits, func(a, b hit) int {
		if c := slices.Compare(b.ver, a.ver); c != 0 {
			return c
		}
		return cmp.Compare(b.mod, a.mod)
	})
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.path
	}
	return out
}

func parseVersion(s string) []int {
	var v []int
	for p := range strings.SplitSeq(s, ".") {
		if n, err := strconv.Atoi(p); err == nil {
			v = append(v, n)
		}
	}
	return v
}

// Discover returns the first candidate that runs: "<path> --version" must
// succeed and name the agent. ok is false when none does.
func (f Finder) Discover(handler string) (Found, bool) {
	if _, ok := worker.ForHandler(handler); !ok {
		return Found{}, false
	}
	for _, c := range f.Candidates(handler) {
		if f.Version == nil {
			return c, true
		}
		out, err := f.Version(c.Path)
		if err != nil || !strings.Contains(strings.ToLower(out), versionMarks[handler]) {
			continue
		}
		c.Version = out
		return c, true
	}
	return Found{}, false
}

// Kind names the install method of a program path of the handler's agent, or
// "" when the path is not in a known location.
func (f Finder) Kind(handler, path string) string {
	for _, l := range locations[handler] {
		base := f.Getenv(l.Env)
		if base == "" {
			continue
		}
		pattern := strings.ToLower(filepath.Join(base, l.Rel))
		if ok, _ := filepath.Match(pattern, strings.ToLower(filepath.Clean(path))); ok {
			return l.Kind
		}
	}
	return ""
}

// Resolve reports which program the handler would run: agentPath when set and
// present, else the handler's name on PATH. Source is one of the Agent* constants.
func (f Finder) Resolve(handler, agentPath string) (path, source string) {
	if agentPath != "" {
		if f.isFile(agentPath) {
			return agentPath, AgentFromSetting
		}
		return agentPath, AgentMissing
	}
	if p, err := f.LookPath(handler); err == nil {
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		return p, AgentFromPath
	}
	return "", AgentMissing
}
