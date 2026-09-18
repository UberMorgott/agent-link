package worker

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// OutputFileArg in Command.Args is replaced by a temporary file path; the
// agent's final text is then read from that file instead of stdout.
const OutputFileArg = "{output_file}"

// Command describes how to run an agent CLI. The prompt is always written to
// stdin, never interpolated into arguments or a shell.
type Command struct {
	Name string
	Args []string
	// Preamble, when set, is written to stdin before the request body.
	Preamble string
}

// ReplyStyle frames a request for the built-in agents: agent-to-agent traffic
// stays compact English, a human's question gets an answer in their language.
const ReplyStyle = `You answer a request that arrived over agent-link from the other developer's machine. You are read-only: read and search only.
Reply rules:
- Agent-written request (English, terse bullets or key: value lines) -> reply the same way: English, terse bullets, no preamble, no recap, no pleasantries; exact paths, names, values, file:line.
- Human-written request (another language or plain prose) -> answer briefly in that person's language.
- Never include secrets, tokens or config contents.
Request:
`

// Claude runs Claude Code headless with read-only built-in tools only: no

// Handler names accepted in settings.
const (
	HandlerNone   = "none"
	HandlerClaude = "claude"
	HandlerCodex  = "codex"
)

// Claude runs Claude Code headless with read-only built-in tools only: no
// shell, no edits, no web, no MCP servers, and every other permission denied.
// dontAsk already denies anything not pre-approved without prompting, so the
// newer --permission-prompts flag is left out: older CLIs reject it.
var Claude = Command{Name: "claude", Args: []string{
	"-p",
	"--output-format", "text",
	"--tools", "Read,Grep,Glob",
	"--allowedTools", "Read,Grep,Glob",
	"--permission-mode", "dontAsk",
	"--strict-mcp-config",
	"--no-session-persistence",
}, Preamble: ReplyStyle}

// Codex runs Codex non-interactively in its read-only sandbox; "-" reads the
// prompt from stdin.
var Codex = Command{Name: "codex", Args: []string{
	"exec",
	"--sandbox", "read-only",
	"--skip-git-repo-check",
	"--ephemeral",
	"--color", "never",
	"--output-last-message", OutputFileArg,
	"-",
}, Preamble: ReplyStyle}

// ForHandler returns the command for a handler name.
func ForHandler(h string) (Command, bool) {
	switch h {
	case HandlerClaude:
		return Claude, true
	case HandlerCodex:
		return Codex, true
	}
	return Command{}, false
}

// Runner returns a Runner that executes c.
func (c Command) Runner() Runner {
	return func(ctx context.Context, dir, prompt string) (string, error) {
		args := append([]string(nil), c.Args...)
		outFile := ""
		for i, a := range args {
			if a != OutputFileArg {
				continue
			}
			if outFile == "" {
				f, err := os.CreateTemp("", "agentlink-reply-*.txt")
				if err != nil {
					return "", err
				}
				outFile = f.Name()
				_ = f.Close()
				defer func() { _ = os.Remove(outFile) }()
			}
			args[i] = outFile
		}
		cmd := exec.CommandContext(ctx, c.Name, args...)
		cmd.Dir = filepath.Clean(dir)
		cmd.Stdin = strings.NewReader(c.Preamble + prompt)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		cmd.WaitDelay = 5 * time.Second
		prepare(cmd)
		if err := cmd.Run(); err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			return "", fmt.Errorf("%s: %w: %s", c.Name, err, reason(stderr.String()))
		}
		if outFile != "" {
			data, err := os.ReadFile(outFile)
			if err != nil {
				return "", err
			}
			return string(data), nil
		}
		return stdout.String(), nil
	}
}

// VersionTimeout bounds one "--version" probe; a large agent binary can take a
// few seconds on its first start while an antivirus scans it.
const VersionTimeout = 15 * time.Second

// Version runs "<path> --version" without a console window and returns its
// trimmed output. A program that fails, prints nothing or hangs is an error.
func Version(ctx context.Context, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, VersionTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.WaitDelay = 2 * time.Second
	prepare(cmd)
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s --version: %w: %s", path, err, tail(out.String(), 300))
	}
	v := tail(out.String(), 300)
	if v == "" {
		return "", fmt.Errorf("%s --version printed nothing", path)
	}
	return v, nil
}

// reason keeps the CLI's own "ERROR:" line when there is one (Codex prints its
// whole session banner to stderr), otherwise the tail of stderr.
func reason(stderr string) string {
	lines := strings.Split(stderr, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); strings.HasPrefix(line, "ERROR:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "ERROR:"))
		}
	}
	return tail(stderr, 2000)
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		s = "..." + s[len(s)-n:]
	}
	return s
}
