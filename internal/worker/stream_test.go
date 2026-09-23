package worker

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/UberMorgott/agent-link/internal/node"
)

// feed writes data to s in awkward chunks, as a pipe would deliver it.
func feed(t *testing.T, s *stream, data []byte) {
	t.Helper()
	for len(data) > 0 {
		n := min(7, len(data))
		if _, err := s.Write(data[:n]); err != nil {
			t.Fatal(err)
		}
		data = data[n:]
	}
	s.flush()
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", filepath.Clean(name)))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// activities feeds a fixture and returns the non-empty activities in order
// as "id type phase text", and the number of lines reported. It checks that
// an operation keeps its start time from running to done.
func activities(t *testing.T, format, dir string, data []byte) (*stream, []string, int) {
	t.Helper()
	var got []string
	lines := 0
	started := map[string]time.Time{}
	s := &stream{format: format, dir: dir, onLine: func(a node.ActivityState) {
		lines++
		if a.Text == "" {
			return
		}
		if a.StartedAt.IsZero() {
			t.Fatalf("activity %+v without a start time", a)
		}
		if at, ok := started[a.ID]; ok && !at.Equal(a.StartedAt) {
			t.Fatalf("activity %+v restarted (was %s)", a, at)
		}
		started[a.ID] = a.StartedAt
		got = append(got, a.ID+" "+a.Type+" "+a.Phase+" "+a.Text)
	}}
	feed(t, s, data)
	return s, got, lines
}

// The Claude fixture has the shapes of a real `claude -p --output-format
// stream-json --verbose` run (Claude Code 2.1.276), trimmed. A tool call runs
// until its tool_result; one that ends while a later call runs changes
// nothing. A shell command shows its description, else only its program.
func TestClaudeStream(t *testing.T) {
	s, got, lines := activities(t, FormatClaude, `C:\work`, fixture(t, "claude-stream.jsonl"))
	want := []string{
		"m1/text writing running " + ActivityWriting,
		"t1 search running Grep 'DefaultTimeout' internal",
		"t2 read running Read docs/index.md",
		"t2 read done Read docs/index.md",
		"t3 edit running Edit internal/x.go",
		"t3 edit done Edit internal/x.go",
		"t4 command running Run worker tests",
		"t5 command running Run curl",
		"t5 command done Run curl",
		"m3/thinking thinking running " + ActivityThinking,
		"m3/text writing running " + ActivityWriting,
	}
	if !slices.Equal(got, want) {
		t.Fatalf("activities\n%q\nwant\n%q", got, want)
	}
	if lines != 19 {
		t.Fatalf("lines = %d, want 19", lines)
	}
	if ans, err := s.answer(); err != nil || ans != "# Docs index" {
		t.Fatalf("answer = %q, %v", ans, err)
	}
	if s.session != "s1" {
		t.Fatalf("session = %q", s.session)
	}
}

func TestRedact(t *testing.T) {
	for in, want := range map[string]string{ //nolint:gosec // G101: fake secrets to mask
		"gh auth login --with-token ghp_x":           "gh auth login --with-token ***",
		"curl -H 'Authorization: Bearer abc' x":      "curl -H 'Authorization: ***' x",
		"curl -H \"X: Bearer abc\" x":                "curl -H \"X: Bearer ***\" x",
		"set API_KEY=sk-1 && run --password \"a b\"": "set API_KEY=*** && run --password ***",
		"go test ./...":                              "go test ./...",
	} {
		if got := redact(in); got != want {
			t.Errorf("redact(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"go test ./...":                  "go test",
		`"C:\bin\git.exe" commit -m "x"`: "git.exe commit",
		"bash -lc 'npm ci'":              "npm ci",
		"curl -H x":                      "curl",
		"":                               "command",
	} {
		if got := program(in); got != want {
			t.Errorf("program(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClaudeStreamError(t *testing.T) {
	s, _, _ := activities(t, FormatClaude, "", []byte(`{"type":"result","subtype":"success","is_error":true,"result":"API Error: overloaded"}`+"\n"))
	if _, err := s.answer(); err == nil || err.Error() != "API Error: overloaded" {
		t.Fatalf("err = %v", err)
	}
}

// The Codex fixture follows the `codex exec --json` event schema
// (openai/codex codex-rs/exec/src/exec_events.rs); its first two lines and
// the failure lines below were printed by codex-cli 0.154.0.
func TestCodexStream(t *testing.T) {
	s, got, _ := activities(t, FormatCodex, `C:\work`, fixture(t, "codex-stream.jsonl"))
	want := []string{
		"item_0 thinking done " + ActivityThinking,
		"item_1 command running Run Get-Content docs/index.md -TotalCount 5",
		"item_1 command done Run Get-Content docs/index.md -TotalCount 5",
		"item_2 edit running Edit a.go (+1)",
		"item_2 edit done Edit a.go (+1)",
		"item_3 command running Run curl --api-key *** https://example.test && export GITHUB_TOKEN=***",
		"item_3 command done Run curl --api-key *** https://example.test && export GITHUB_TOKEN=***",
		"item_4 writing done " + ActivityWriting,
	}
	if !slices.Equal(got, want) {
		t.Fatalf("activities\n%q\nwant\n%q", got, want)
	}
	if ans, err := s.answer(); err != nil || ans != "# Docs index" {
		t.Fatalf("answer = %q, %v", ans, err)
	}
	if s.session != "01a0b5a6-bc3e-7df0-949d-957cc214fe5a" {
		t.Fatalf("session = %q", s.session)
	}
}

func TestCodexStreamFailure(t *testing.T) {
	data := `{"type":"thread.started","thread_id":"x"}
{"type":"turn.started"}
{"type":"error","message":"You've hit your usage limit."}
{"type":"turn.failed","error":{"message":"You've hit your usage limit."}}
`
	s, got, lines := activities(t, FormatCodex, "", []byte(data))
	if len(got) != 0 || lines != 4 {
		t.Fatalf("activities %q, lines %d", got, lines)
	}
	if _, err := s.answer(); err == nil || err.Error() != "You've hit your usage limit." {
		t.Fatalf("err = %v", err)
	}
}

// Plain text agents (and non-JSON lines of streaming ones) are the answer;
// each line still proves the agent is alive.
func TestTextStream(t *testing.T) {
	s, got, lines := activities(t, FormatText, "", []byte("line one\n{\"type\":\"x\"}\nlast"))
	if len(got) != 0 || lines != 3 {
		t.Fatalf("activities %q, lines %d", got, lines)
	}
	if ans, _ := s.answer(); ans != "line one\n{\"type\":\"x\"}\nlast" {
		t.Fatalf("answer = %q", ans)
	}
	s, _, _ = activities(t, FormatClaude, "", []byte("echo: hi\n"))
	if ans, _ := s.answer(); ans != "echo: hi\n" {
		t.Fatalf("claude fallback answer = %q", ans)
	}
}

func TestClip(t *testing.T) {
	long := strings.Repeat("x", 300)
	if got := clip("Read  a\nb " + long); len([]rune(got)) != maxActivity || !strings.HasPrefix(got, "Read a b x") {
		t.Fatalf("clip = %q", got)
	}
}
