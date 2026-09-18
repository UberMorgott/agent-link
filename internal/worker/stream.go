package worker

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// Output formats of an agent CLI's stdout (Command.Format).
const (
	// FormatText: stdout is the answer; every line still counts as activity.
	FormatText = ""
	// FormatClaude: `claude -p --output-format stream-json --verbose`, one JSON
	// event per line: {"type":"assistant","message":{"content":[{"type":
	// "tool_use","name","input"} | {"type":"text"} | {"type":"thinking"}]}},
	// ... and a final {"type":"result","is_error","result"}.
	FormatClaude = "claude-stream-json"
	// FormatCodex: `codex exec --json`, one JSON event per line:
	// {"type":"item.started|item.updated|item.completed","item":{"type":
	// "command_execution","command"} | {"type":"reasoning"} | {"type":
	// "agent_message","text"} | ...}, {"type":"turn.failed","error":{"message"}},
	// {"type":"error","message"} (openai/codex codex-rs/exec/src/exec_events.rs).
	FormatCodex = "codex-jsonl"
)

// maxActivity bounds one activity line in runes.
const maxActivity = 120

// Activity lines that are not a tool call.
const (
	ActivityThinking = "thinking"
	ActivityWriting  = "writing answer"
)

// stream parses an agent's stdout line by line as it arrives. It is an
// io.Writer fed by the process's stdout copier; onLine gets the activity of
// every complete line ("" when the line says nothing new but proves the agent
// is alive).
type stream struct {
	format string
	dir    string // the agent's working folder, to shorten paths
	onLine func(activity string)

	partial   []byte
	plain     strings.Builder // non-JSON lines: the answer in FormatText
	result    string          // the final answer the stream reported
	hasResult bool
	failure   string // an error the stream reported
	session   string // the agent session id the stream announced
	finished  bool   // the stream reported the end of the run
}

func (s *stream) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			s.partial = append(s.partial, p...)
			break
		}
		line := p[:i+1]
		if len(s.partial) > 0 {
			line = append(s.partial, line...)
			s.partial = nil
		}
		s.line(line)
		p = p[i+1:]
	}
	return n, nil
}

// flush handles a last line without a newline.
func (s *stream) flush() {
	if len(s.partial) > 0 {
		line := s.partial
		s.partial = nil
		s.line(line)
	}
}

func (s *stream) line(raw []byte) {
	activity, ok := "", false
	if s.format != FormatText {
		activity, ok = s.event(bytes.TrimSpace(raw))
	}
	if !ok {
		s.plain.Write(raw)
	}
	if s.onLine != nil {
		s.onLine(activity)
	}
}

// event parses one JSON event and reports whether the line was one.
func (s *stream) event(line []byte) (string, bool) {
	if len(line) == 0 || line[0] != '{' {
		return "", false
	}
	var ev struct {
		Type    string          `json:"type"`
		Message json.RawMessage `json:"message"`
		// Claude result.
		Result  string `json:"result"`
		IsError bool   `json:"is_error"`
		// Codex.
		Item  json.RawMessage `json:"item"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		// Session ids: Claude's on every event, Codex's on thread.started.
		SessionID string `json:"session_id"`
		ThreadID  string `json:"thread_id"`
	}
	if json.Unmarshal(line, &ev) != nil || ev.Type == "" {
		return "", false
	}
	if id := cmp.Or(ev.SessionID, ev.ThreadID); id != "" && s.session == "" {
		s.session = id
	}
	switch ev.Type {
	case "result", "turn.completed", "turn.failed":
		s.finished = true
	}
	switch s.format {
	case FormatClaude:
		return s.claude(ev.Type, ev.Message, ev.Result, ev.IsError), true
	case FormatCodex:
		switch ev.Type {
		case "item.started", "item.updated", "item.completed":
			return s.codexItem(ev.Type, ev.Item), true
		case "turn.failed":
			s.failure = ev.Error.Message
		case "error":
			var e struct {
				Message string `json:"message"`
			}
			_ = json.Unmarshal(line, &e)
			s.failure = e.Message
		}
		return "", true
	}
	return "", false
}

func (s *stream) claude(typ string, message json.RawMessage, result string, isError bool) string {
	switch typ {
	case "result":
		s.hasResult = true
		if isError {
			s.failure = result
		} else {
			s.result = result
		}
		return ""
	case "assistant":
		var m struct {
			Content []struct {
				Type  string         `json:"type"`
				Name  string         `json:"name"`
				Input map[string]any `json:"input"`
				Text  string         `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(message, &m) != nil {
			return ""
		}
		activity := ""
		for _, c := range m.Content {
			switch c.Type {
			case "tool_use":
				activity = s.tool(c.Name, c.Input)
			case "thinking", "redacted_thinking":
				activity = ActivityThinking
			case "text":
				if strings.TrimSpace(c.Text) != "" {
					activity = ActivityWriting
				}
			}
		}
		return activity
	}
	return ""
}

// tool describes a Claude tool call: "Read docs/index.md", "Grep 'Worker' internal".
func (s *stream) tool(name string, in map[string]any) string {
	str := func(k string) string { v, _ := in[k].(string); return v }
	switch name {
	case "Read":
		return clip("Read " + s.rel(str("file_path")))
	case "Grep":
		out := "Grep '" + str("pattern") + "'"
		if p := s.rel(str("path")); p != "" {
			out += " " + p
		}
		return clip(out)
	case "Glob":
		out := "Glob " + str("pattern")
		if p := s.rel(str("path")); p != "" {
			out += " " + p
		}
		return clip(out)
	}
	return clip(name)
}

func (s *stream) codexItem(evType string, raw json.RawMessage) string {
	var it struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Command string `json:"command"`
		Query   string `json:"query"`
		Server  string `json:"server"`
		Tool    string `json:"tool"`
		Message string `json:"message"`
		Changes []struct {
			Path string `json:"path"`
		} `json:"changes"`
	}
	if json.Unmarshal(raw, &it) != nil {
		return ""
	}
	switch it.Type {
	case "command_execution":
		return clip("Run " + shellBody(it.Command))
	case "reasoning":
		return ActivityThinking
	case "agent_message":
		if evType == "item.completed" && strings.TrimSpace(it.Text) != "" {
			s.result, s.hasResult = it.Text, true
		}
		return ActivityWriting
	case "web_search":
		return clip("Search " + it.Query)
	case "mcp_tool_call":
		return clip("Tool " + it.Server + "." + it.Tool)
	case "file_change":
		if len(it.Changes) > 0 {
			return clip("Edit " + s.rel(it.Changes[0].Path))
		}
	case "todo_list":
		return "planning"
	case "error":
		s.failure = it.Message
	}
	return ""
}

// answer returns the final text: what the stream reported, else the plain
// output. A failure the stream reported without an answer is an error.
func (s *stream) answer() (string, error) {
	switch {
	case s.hasResult && s.result != "":
		return s.result, nil
	case s.failure != "":
		return "", errors.New(s.failure)
	}
	return s.plain.String(), nil
}

// shellBody drops the shell wrapper Codex puts around a command
// (`"...pwsh.exe" -Command '...'`, `bash -lc '...'`), keeping what it runs.
func shellBody(cmd string) string {
	for _, sep := range []string{" -Command ", " -lc ", " -c "} {
		if _, after, ok := strings.Cut(cmd, sep); ok {
			body := strings.TrimSpace(after)
			if len(body) >= 2 && (body[0] == '\'' || body[0] == '"') && body[len(body)-1] == body[0] {
				body = body[1 : len(body)-1]
			}
			return body
		}
	}
	return cmd
}

// rel shortens a path inside the working folder to a relative one with forward slashes.
func (s *stream) rel(p string) string {
	if p == "" {
		return ""
	}
	if s.dir != "" && filepath.IsAbs(p) {
		if r, err := filepath.Rel(s.dir, p); err == nil && !strings.HasPrefix(r, "..") {
			p = r
		}
	}
	if p == "." {
		return "."
	}
	return filepath.ToSlash(p)
}

// clip makes an activity one short line.
func clip(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= maxActivity {
		return s
	}
	r := []rune(s)
	return string(r[:maxActivity-1]) + "…"
}
