package worker

import "testing"

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
