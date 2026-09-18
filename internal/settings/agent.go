package settings

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/UberMorgott/agent-link/internal/worker"
)

// Agent program sources reported by Finder.Resolve.
const (
	AgentFromPath    = "path"       // found on PATH under the handler's name
	AgentFromSetting = "agent_path" // the AgentPath setting, and the file exists
	AgentMissing     = "missing"    // neither
)

// Finder locates agent programs. The functions are replaceable in tests.
type Finder struct {
	LookPath func(string) (string, error)
	Stat     func(string) (fs.FileInfo, error)
	Getenv   func(string) string
}

// SystemFinder looks at the real PATH, disk and environment.
var SystemFinder = Finder{LookPath: exec.LookPath, Stat: os.Stat, Getenv: os.Getenv}

// installDirs lists where the official installers put each agent on Windows,
// relative to an environment variable:
//   - npm i -g @openai/codex (and @anthropic-ai/claude-code) writes a .cmd
//     shim into npm's default global prefix, %APPDATA%\npm;
//   - Claude Code's native installer puts claude.exe into %USERPROFILE%\.local\bin.
//
// The tray started from Explorer or autostart may not see a PATH entry the
// installer added later, or one set only in a terminal profile.
var installDirs = map[string][][2]string{
	worker.HandlerCodex:  {{"APPDATA", `npm\codex.cmd`}},
	worker.HandlerClaude: {{"USERPROFILE", `.local\bin\claude.exe`}, {"APPDATA", `npm\claude.cmd`}},
}

// Candidates returns the well-known install locations of the handler's agent.
func (f Finder) Candidates(handler string) []string {
	var out []string
	for _, c := range installDirs[handler] {
		if base := f.Getenv(c[0]); base != "" {
			out = append(out, filepath.Join(base, c[1]))
		}
	}
	return out
}

// Discover returns the first well-known install location of the handler's
// agent that exists, or "" when the agent is on PATH (nothing to remember) or
// nowhere to be found.
func (f Finder) Discover(handler string) string {
	if _, ok := worker.ForHandler(handler); !ok {
		return ""
	}
	if _, err := f.LookPath(handler); err == nil {
		return ""
	}
	for _, p := range f.Candidates(handler) {
		if st, err := f.Stat(p); err == nil && st.Mode().IsRegular() {
			return p
		}
	}
	return ""
}

// Resolve reports which program the handler would run: agentPath when set and
// present, else the handler's name on PATH. Source is one of the Agent* constants.
func (f Finder) Resolve(handler, agentPath string) (path, source string) {
	if agentPath != "" {
		if st, err := f.Stat(agentPath); err == nil && st.Mode().IsRegular() {
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
