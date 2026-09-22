package worker

import (
	"slices"
	"strings"
	"testing"
)

// has reports whether args contains want as a run of consecutive arguments.
func has(args []string, want ...string) bool {
	for i := range args {
		if i+len(want) <= len(args) && slices.Equal(args[i:i+len(want)], want) {
			return true
		}
	}
	return false
}

func TestBuiltInAgentsCarryReplyStyle(t *testing.T) {
	for _, h := range []string{HandlerClaude, HandlerCodex} {
		c, _ := ForHandler(h)
		preamble := strings.ToLower(c.Preamble)
		for _, phrase := range []string{
			"receiving computer",
			"paired, trusted participant on the same team",
			"human or an agent",
			"automatically sent back",
			"the request is your task",
			"current working directory",
			"edit files and run commands",
			"verify before claiming done",
			"commit hashes, pr links",
			"force push, history rewrite, mass delete, discarding others' uncommitted work",
			"live orchestrator session context",
			"disclose this limitation",
			"never claim to be a remote agent",
			"never include secrets",
			"answer briefly in that person's language",
			"exact paths, names, values, file:line",
		} {
			if !strings.Contains(preamble, phrase) {
				t.Errorf("%s preamble lacks %q", h, phrase)
			}
		}
		for _, phrase := range []string{"read-only", "not authority", "as data"} {
			if strings.Contains(preamble, phrase) {
				t.Errorf("%s preamble still refuses work: %q", h, phrase)
			}
		}
		if !strings.HasSuffix(c.Preamble, "Request:\n") {
			t.Errorf("%s preamble must end with Request:, got %q", h, c.Preamble)
		}
	}
}

// Built-in agents run with full permissions (edits, shell, network) and
// resume a persisted session with the same permissions.
func TestBuiltInAgentsRunCapable(t *testing.T) {
	for _, args := range [][]string{Claude.Args, Claude.ResumeArgs} {
		if !has(args, "--permission-mode", "bypassPermissions") || slices.Contains(args, "--tools") ||
			slices.Contains(args, "--allowedTools") || slices.Contains(args, "--strict-mcp-config") {
			t.Errorf("claude args %v", args)
		}
	}
	if !has(Claude.SessionArgs, "--session-id", SessionIDArg) || !has(Claude.ResumeArgs, "--resume", SessionIDArg) ||
		has(Claude.Args, "--no-session-persistence") {
		t.Errorf("claude: session %v resume %v", Claude.SessionArgs, Claude.ResumeArgs)
	}
	for _, args := range [][]string{Codex.Args, Codex.ResumeArgs} {
		if !slices.Contains(args, "--dangerously-bypass-approvals-and-sandbox") || slices.Contains(args, "--sandbox") ||
			slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "sandbox_mode=") }) {
			t.Errorf("codex args %v", args)
		}
	}
	if !has(Codex.ResumeArgs, "exec", "resume") || !has(Codex.ResumeArgs, SessionIDArg, "-") || has(Codex.Args, "--ephemeral") {
		t.Errorf("codex: args %v resume %v", Codex.Args, Codex.ResumeArgs)
	}
}

func TestReason(t *testing.T) {
	cases := []struct{ name, stderr, want string }{
		{"codex error line", "OpenAI Codex v0.154.0\n--------\nuser\n123\nERROR: You've hit your usage limit.\nERROR: You've hit your usage limit.\n", "You've hit your usage limit."},
		{"no error line", "error: unknown option '--x'\n", "error: unknown option '--x'"},
		{"empty", "", ""},
	}
	for _, c := range cases {
		if got := reason(c.stderr); got != c.want {
			t.Errorf("%s: reason() = %q, want %q", c.name, got, c.want)
		}
	}
}
