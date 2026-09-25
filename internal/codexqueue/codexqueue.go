// Package codexqueue wakes an idle Codex session from outside. `codex queue
// --thread <id> --message <text>` (Codex 0.149 and later) stores the message
// in Codex's SQLite queue under CODEX_HOME, and every running Codex app-server
// (the TUI, the desktop app) that holds that thread loaded and idle starts a
// turn with it within about 10 seconds. A thread no app has open keeps the
// message until one opens it: nothing runs headless.
package codexqueue

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
)

// MinVersion is the first Codex CLI with `codex queue`.
var MinVersion = [3]int{0, 149, 0}

// MaxMessage is the longest text Wake queues, in UTF-16 units: escaped, it
// still fits a Windows command line (32767) with the rest of the arguments.
const MaxMessage = 12000

// Timings.
const (
	// recheck: how long a lookup (found or not) is trusted.
	recheck = 10 * time.Minute
	// downFor: after a failed wake no session is woken this way for this long.
	downFor = 10 * time.Minute
	// runTimeout bounds one codex run.
	runTimeout = 30 * time.Second
)

// Queue finds the codex binary and queues messages with it. The zero value is
// not usable: use New.
type Queue struct {
	// candidates lists the binaries to try, best first.
	candidates func() []string
	now        func() time.Time

	mu      sync.Mutex
	exe     string    // the binary found, "" when none
	version string    // its version
	checked time.Time // when the lookup ran
	down    time.Time // after a failed wake: not Ready until then
}

// New returns a Queue that looks for codex on PATH and where the Codex CLI
// and the Codex desktop app install it.
func New() *Queue { return &Queue{candidates: Candidates, now: time.Now} }

// NewWith returns a Queue that tries only the binaries of candidates (tests).
func NewWith(candidates func() []string) *Queue { return &Queue{candidates: candidates, now: time.Now} }

// Check looks for a usable codex (MinVersion or later) when the last lookup is
// older than recheck, or always when force. It runs the candidates, so it
// may take a moment: call it off the request path.
func (q *Queue) Check(ctx context.Context, force bool) {
	q.mu.Lock()
	due := force || q.checked.IsZero() || q.now().Sub(q.checked) >= recheck
	q.mu.Unlock()
	if !due {
		return
	}
	exe, ver := "", ""
	for _, c := range q.candidates() {
		v, err := versionOf(ctx, c)
		if err == nil && AtLeast(v, MinVersion) {
			exe, ver = c, v
			break
		}
	}
	q.mu.Lock()
	q.exe, q.version, q.checked = exe, ver, q.now()
	q.mu.Unlock()
}

// Ready reports whether the last lookup found a usable codex and no wake
// failed recently. It never runs anything.
func (q *Queue) Ready() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.exe != "" && !q.now().Before(q.down)
}

// Binary is the codex found and its version ("" when none).
func (q *Queue) Binary() (exe, version string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.exe, q.version
}

// Wake queues text for thread (the Codex session id) in the Codex home home
// ("": Codex's default, ~/.codex). A failure makes the queue not Ready for a
// while; the caller falls back to delivery at the session's next event.
func (q *Queue) Wake(ctx context.Context, home, thread, text string) error {
	q.mu.Lock()
	exe := q.exe
	q.mu.Unlock()
	if exe == "" {
		return errors.New("codex queue: no codex " + versionString(MinVersion) + " or later found")
	}
	if thread == "" || strings.HasPrefix(thread, "-") {
		return fmt.Errorf("codex queue: bad thread %q", thread)
	}
	// The text goes as one argv entry (quoted by exec for CommandLineToArgvW
	// on Windows, whose whole command line is at most 32767 UTF-16 units, and
	// escaping may double it): refuse, without marking codex down, what may not fit.
	if n := len(utf16.Encode([]rune(text))); n > MaxMessage {
		return fmt.Errorf("codex queue: message of %d UTF-16 units, at most %d", n, MaxMessage)
	}
	ctx, cancel := context.WithTimeout(ctx, runTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "queue", "--thread", thread, "--message", text) //nolint:gosec // G204: the codex binary found by Check; the thread id is a registered session id
	cmd.Env = os.Environ()
	if home != "" {
		cmd.Env = append(cmd.Env, "CODEX_HOME="+home)
	}
	hide(cmd)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		q.mu.Lock()
		q.down = q.now().Add(downFor)
		q.mu.Unlock()
		msg := strings.TrimSpace(out.String())
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return fmt.Errorf("codex queue: %w: %s", err, msg)
	}
	return nil
}

// versionOf runs `exe --version` ("codex-cli 0.155.1") and returns the version.
func versionOf(ctx context.Context, exe string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "--version")
	hide(cmd)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	f := strings.Fields(string(out))
	if len(f) == 0 {
		return "", errors.New("no version")
	}
	return f[len(f)-1], nil
}

// AtLeast reports whether version v ("0.155.1", "v0.150.0-alpha.1") is want
// or later.
func AtLeast(v string, want [3]int) bool {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		return false
	}
	for i := range 3 {
		n := 0
		if i < len(parts) {
			var err error
			if n, err = strconv.Atoi(parts[i]); err != nil {
				return false
			}
		}
		if n != want[i] {
			return n > want[i]
		}
	}
	return true
}

func versionString(v [3]int) string { return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2]) }

// Candidates lists the codex binaries to try, best first: codex on PATH (for
// the npm package, the native binary its shim runs, since a .cmd shim mangles
// arguments), then the standalone Codex CLI installs, then the codex the Codex
// desktop app runs.
func Candidates() []string {
	var out []string
	add := func(paths ...string) {
		for _, p := range paths {
			if p == "" {
				continue
			}
			if st, err := os.Stat(p); err == nil && !st.IsDir() && !contains(out, p) {
				out = append(out, p)
			}
		}
	}
	home, _ := os.UserHomeDir()
	if runtime.GOOS != "windows" {
		if p, err := exec.LookPath("codex"); err == nil {
			add(p)
		}
		add(filepath.Join(home, ".codex", "packages", "standalone", "current", "bin", "codex"),
			"/Applications/Codex.app/Contents/Resources/codex")
		return out
	}
	if p, err := exec.LookPath("codex"); err == nil {
		if strings.EqualFold(filepath.Ext(p), ".exe") {
			add(p)
		}
		add(npmNative(filepath.Dir(p))...)
	}
	if appdata := os.Getenv("APPDATA"); appdata != "" {
		add(npmNative(filepath.Join(appdata, "npm"))...)
	}
	add(filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", "OpenAI", "Codex", "bin", "codex.exe"),
		filepath.Join(home, ".codex", "packages", "standalone", "current", "bin", "codex.exe"))
	// The desktop app's package copy (WindowsApps\OpenAI.Codex_*) cannot be run
	// from outside the package; the app runs its app-server from a copy under
	// %LOCALAPPDATA%\OpenAI\Codex\bin\<hash>, which can: newest first.
	add(newestFirst(filepath.Join(os.Getenv("LOCALAPPDATA"), "OpenAI", "Codex", "bin", "*", "codex.exe"))...)
	return out
}

// npmNative is the native codex.exe of an npm install whose shims are in dir.
func npmNative(dir string) []string {
	var out []string
	for _, pat := range []string{
		filepath.Join(dir, "node_modules", "@openai", "codex", "node_modules", "@openai", "codex-win32-*", "vendor", "*", "bin", "codex.exe"),
		filepath.Join(dir, "node_modules", "@openai", "codex", "node_modules", "@openai", "codex-win32-*", "vendor", "*", "codex", "codex.exe"),
		filepath.Join(dir, "node_modules", "@openai", "codex", "vendor", "*", "codex", "codex.exe"),
	} {
		m, _ := filepath.Glob(pat)
		out = append(out, m...)
	}
	return out
}

// newestFirst is the files matching pattern, the most recently modified first.
func newestFirst(pattern string) []string {
	m, _ := filepath.Glob(pattern)
	mod := make(map[string]time.Time, len(m))
	for _, p := range m {
		if st, err := os.Stat(p); err == nil {
			mod[p] = st.ModTime()
		}
	}
	sort.SliceStable(m, func(i, j int) bool { return mod[m[i]].After(mod[m[j]]) })
	return m
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}
