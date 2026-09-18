package worker

import (
	"slices"
	"testing"
)

func TestBuiltInAgentsCarryReplyStyle(t *testing.T) {
	for _, h := range []string{HandlerClaude, HandlerCodex} {
		if c, _ := ForHandler(h); c.Preamble != ReplyStyle {
			t.Errorf("%s: preamble missing", h)
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
