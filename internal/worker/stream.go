package worker

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/UberMorgott/agent-link/internal/node"
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
	ActivityPlanning = "planning"
)

// Activity types (node.ActivityState.Type).
const (
	TypeThinking = "thinking"
	TypeWriting  = "writing"
	TypeEdit     = "edit"
	TypeCommand  = "command"
	TypeRead     = "read"
	TypeSearch   = "search"
	TypePlan     = "plan"
	TypeTool     = "tool"
)

// stream parses an agent's stdout line by line as it arrives. It is an
// io.Writer fed by the process's stdout copier; onLine gets the activity of
// every complete line (a zero one, Text "", when the line says nothing new but
// proves the agent is alive). An activity is one operation of the agent: a
// tool call, a Codex item, a thinking or answer block; it starts running and
// may end (PhaseDone) when the stream reports its result. Reasoning text and
// full tool inputs never leave the stream: only a short description.
type stream struct {
	format string
	dir    string // the agent's working folder, to shorten paths
	onLine func(a node.ActivityState)

	cur       node.ActivityState            // the latest operation
	open      map[string]node.ActivityState // running operations by id
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
	var a node.ActivityState
	ok := false
	if s.format != FormatText {
		a, ok = s.event(bytes.TrimSpace(raw))
	}
	if !ok {
		s.plain.Write(raw)
	}
	if s.onLine != nil {
		s.onLine(a)
	}
}

// begin makes operation id the current one, running; an operation already
// running keeps its start time.
func (s *stream) begin(id, typ, text string) node.ActivityState {
	a := node.ActivityState{ID: id, Type: typ, Text: clip(text), Phase: node.PhaseRunning, StartedAt: time.Now().UTC()}
	if old, ok := s.open[id]; ok {
		a.StartedAt = old.StartedAt
	}
	if s.open == nil {
		s.open = map[string]node.ActivityState{}
	}
	s.open[id], s.cur = a, a
	return a
}

// end marks operation id done. It reports the change only when id is the
// current operation: a parallel one that ends does not replace it.
func (s *stream) end(id string) node.ActivityState {
	a, ok := s.open[id]
	delete(s.open, id)
	if !ok || s.cur.ID != id {
		return node.ActivityState{}
	}
	a.Phase = node.PhaseDone
	s.cur = a
	return a
}

// event parses one JSON event and reports whether the line was one.
func (s *stream) event(line []byte) (node.ActivityState, bool) {
	var none node.ActivityState
	if len(line) == 0 || line[0] != '{' {
		return none, false
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
		return none, false
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
		return none, true
	}
	return none, false
}

func (s *stream) claude(typ string, message json.RawMessage, result string, isError bool) node.ActivityState {
	var a node.ActivityState
	switch typ {
	case "result":
		s.hasResult = true
		if isError {
			s.failure = result
		} else {
			s.result = result
		}
	case "assistant":
		var m struct {
			ID      string `json:"id"`
			Content []struct {
				Type  string         `json:"type"`
				ID    string         `json:"id"`
				Name  string         `json:"name"`
				Input map[string]any `json:"input"`
				Text  string         `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(message, &m) != nil {
			return a
		}
		for _, c := range m.Content {
			switch c.Type {
			case "tool_use":
				typ, text := s.tool(c.Name, c.Input)
				a = s.begin(cmp.Or(c.ID, c.Name), typ, text)
			case "thinking", "redacted_thinking":
				a = s.begin(m.ID+"/thinking", TypeThinking, ActivityThinking)
			case "text":
				if strings.TrimSpace(c.Text) != "" {
					a = s.begin(m.ID+"/text", TypeWriting, ActivityWriting)
				}
			}
		}
	case "user":
		// Tool results: {"content":[{"type":"tool_result","tool_use_id"}]}; a
		// prompt echo has a string content, which does not parse here.
		var m struct {
			Content []struct {
				Type      string `json:"type"`
				ToolUseID string `json:"tool_use_id"`
			} `json:"content"`
		}
		if json.Unmarshal(message, &m) != nil {
			return a
		}
		for _, c := range m.Content {
			if c.Type == "tool_result" && c.ToolUseID != "" {
				if done := s.end(c.ToolUseID); done.Text != "" {
					a = done
				}
			}
		}
	}
	return a
}

// tool describes a Claude tool call: "Read docs/index.md", "Grep 'Worker'
// internal", "Edit internal/x.go". A shell command is described by the
// description Claude gives it, else by its program: its arguments may hold
// secrets.
func (s *stream) tool(name string, in map[string]any) (typ, text string) {
	str := func(k string) string { v, _ := in[k].(string); return v }
	withPath := func(out string) string {
		if p := s.rel(str("path")); p != "" {
			out += " " + p
		}
		return out
	}
	switch name {
	case "Read":
		return TypeRead, "Read " + s.rel(str("file_path"))
	case "Grep":
		return TypeSearch, withPath("Grep '" + str("pattern") + "'")
	case "Glob":
		return TypeSearch, withPath("Glob " + str("pattern"))
	case "Edit", "MultiEdit", "Write", "NotebookEdit":
		return TypeEdit, name + " " + s.rel(cmp.Or(str("file_path"), str("notebook_path")))
	case "Bash", "PowerShell":
		if d := strings.TrimSpace(str("description")); d != "" {
			return TypeCommand, redact(d)
		}
		return TypeCommand, "Run " + program(str("command"))
	case "TodoWrite":
		return TypePlan, ActivityPlanning
	case "WebSearch":
		return TypeSearch, "Search " + redact(str("query"))
	case "WebFetch":
		host := str("url")
		if u, err := url.Parse(host); err == nil && u.Host != "" {
			host = u.Host
		}
		return TypeRead, "Fetch " + host
	case "Task", "Agent":
		if d := strings.TrimSpace(str("description")); d != "" {
			return TypeTool, name + ": " + redact(d)
		}
	}
	return TypeTool, name
}

func (s *stream) codexItem(evType string, raw json.RawMessage) node.ActivityState {
	var it struct {
		ID      string `json:"id"`
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
		return node.ActivityState{}
	}
	typ, text := "", ""
	switch it.Type {
	case "command_execution":
		typ, text = TypeCommand, "Run "+redact(shellBody(it.Command))
	case "reasoning":
		typ, text = TypeThinking, ActivityThinking // never the reasoning text
	case "agent_message":
		if evType == "item.completed" && strings.TrimSpace(it.Text) != "" {
			s.result, s.hasResult = it.Text, true
		}
		typ, text = TypeWriting, ActivityWriting
	case "web_search":
		typ, text = TypeSearch, "Search "+redact(it.Query)
	case "mcp_tool_call":
		typ, text = TypeTool, "Tool "+it.Server+"."+it.Tool
	case "file_change":
		if len(it.Changes) > 0 {
			typ, text = TypeEdit, "Edit "+s.rel(it.Changes[0].Path)
			if more := len(it.Changes) - 1; more > 0 {
				text += " (+" + strconv.Itoa(more) + ")"
			}
		}
	case "todo_list":
		typ, text = TypePlan, ActivityPlanning
	case "error":
		s.failure = it.Message
	}
	if text == "" {
		return node.ActivityState{}
	}
	id := cmp.Or(it.ID, it.Type)
	a := s.begin(id, typ, text)
	if evType == "item.completed" {
		delete(s.open, id)
		a.Phase = node.PhaseDone
		s.cur = a
	}
	return a
}

// secretFlag and secretValue find secrets in a command line: a flag named
// like one followed by its value, a NAME=value or name: value pair, a bearer
// token.
var (
	secretFlag  = regexp.MustCompile(`(?i)(--?[\w-]*(?:token|secret|passw|pwd|api[_-]?key|auth)[\w-]*)\s+("[^"]*"|'[^']*'|[^\s'"]+)`)
	secretValue = regexp.MustCompile(`(?i)([\w-]*(?:token|secret|passw|pwd|api[_-]?key|auth)[\w-]*\s*[=:]\s*)(?:bearer\s+)?("[^"]*"|'[^']*'|[^\s'"]+)`)
	bearer      = regexp.MustCompile(`(?i)(bearer\s+)[^\s'"]+`)
)

// redact masks the values of what looks like a secret in s.
func redact(s string) string {
	s = secretFlag.ReplaceAllString(s, "$1 ***")
	s = secretValue.ReplaceAllString(s, "$1***")
	return bearer.ReplaceAllString(s, "$1***")
}

// program names what a shell command runs, without its arguments: "go test",
// "git commit", "npm".
func program(cmd string) string {
	f := strings.Fields(shellBody(cmd))
	if len(f) == 0 {
		return "command"
	}
	out := strings.Trim(f[0], `"'`)
	if i := strings.LastIndexAny(out, `/\`); i >= 0 {
		out = out[i+1:]
	}
	if len(f) > 1 && subcommand.MatchString(f[1]) {
		out += " " + f[1]
	}
	return out
}

var subcommand = regexp.MustCompile(`^[a-z][a-z-]*$`)

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
