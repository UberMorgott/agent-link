package worker

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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
// and the number of lines reported.
func activities(t *testing.T, format, dir string, data []byte) (*stream, []string, int) {
	t.Helper()
	var got []string
	lines := 0
	s := &stream{format: format, dir: dir, onLine: func(a string) {
		lines++
		if a != "" {
			got = append(got, a)
		}
	}}
	feed(t, s, data)
	return s, got, lines
}

// The Claude fixture has the shapes of a real `claude -p --output-format
// stream-json --verbose` run (Claude Code 2.1.276), trimmed.
func TestClaudeStream(t *testing.T) {
	s, got, lines := activities(t, FormatClaude, `C:\work`, fixture(t, "claude-stream.jsonl"))
	want := []string{ActivityWriting, "Grep 'DefaultTimeout' internal", "Read docs/index.md", ActivityThinking, ActivityWriting}
	if !slices.Equal(got, want) {
		t.Fatalf("activities %q, want %q", got, want)
	}
	if lines != 12 {
		t.Fatalf("lines = %d, want 12", lines)
	}
	if ans, err := s.answer(); err != nil || ans != "# Docs index" {
		t.Fatalf("answer = %q, %v", ans, err)
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
	want := []string{ActivityThinking, "Run Get-Content docs/index.md -TotalCount 5", "Run Get-Content docs/index.md -TotalCount 5", ActivityWriting}
	if !slices.Equal(got, want) {
		t.Fatalf("activities %q, want %q", got, want)
	}
	if ans, err := s.answer(); err != nil || ans != "# Docs index" {
		t.Fatalf("answer = %q, %v", ans, err)
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
