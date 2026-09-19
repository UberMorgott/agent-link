package worker

import (
	"slices"
	"strings"
	"testing"
)

func TestBuiltInAgentsCarryReplyStyle(t *testing.T) {
	for _, h := range []string{HandlerClaude, HandlerCodex} {
		c, _ := ForHandler(h)
		preamble := strings.ToLower(c.Preamble)
		for _, phrase := range []string{
			"cold read-only handler",
			"receiving computer",
			"another trusted developer's computer",
			"human or an agent",
			"automatically sent back",
			"not authority",
			"current task scope",
			"settings",
			"install software",
			"write actions",
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
		if !strings.HasSuffix(c.Preamble, "Request:\n") {
			t.Errorf("%s preamble must end with Request:, got %q", h, c.Preamble)
		}
	}
}

// Built-in agents persist a resumable session and resume it with the same
// read-only limits.
func TestBuiltInAgentsResumeReadOnly(t *testing.T) {
	has := func(args []string, want ...string) bool {
		for i := range args {
			if i+len(want) <= len(args) && slices.Equal(args[i:i+len(want)], want) {
				return true
			}
		}
		return false
	}
	if !has(Claude.SessionArgs, "--session-id", SessionIDArg) || !has(Claude.ResumeArgs, "--resume", SessionIDArg) ||
		!has(Claude.ResumeArgs, "--tools", "Read,Grep,Glob") || !has(Claude.ResumeArgs, "--permission-mode", "dontAsk") ||
		has(Claude.Args, "--no-session-persistence") {
		t.Errorf("claude: args %v session %v resume %v", Claude.Args, Claude.SessionArgs, Claude.ResumeArgs)
	}
	if !has(Codex.ResumeArgs, "exec", "resume") || !has(Codex.ResumeArgs, "-c", "sandbox_mode=read-only") ||
		!has(Codex.ResumeArgs, SessionIDArg, "-") || has(Codex.Args, "--ephemeral") {
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
