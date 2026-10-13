package node

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

// What a tool call of an agent session is doing, for the people who watch
// it: an activity line («читает rel/path», «запускает go») and the agent's
// state. Never raw arguments, file contents or command lines: only a tool
// kind, a relative path and a program name. The hooks (agentlink hook) and
// the node itself (ActivityRequest.Tool, SessionDoingRequest.Tool) read a
// call the same way.

// Activity events a session reports without a tool (ActivityRequest.Event).
const (
	ActThinking = "thinking" // the model works
	ActReading  = "reading"  // it reads the messages delivered to it
	ActDone     = "done"     // its turn, or a subagent's run, ended (with PhaseIdle)
	ActAgent    = "agent"    // a subagent works
)

// EventActivity is the activity type and text of event; ok is false for an
// unknown one.
func EventActivity(event string) (typ, text string, ok bool) {
	switch event {
	case ActThinking:
		return "thinking", "думает", true
	case ActReading:
		return "read", "читает сообщения", true
	case ActDone:
		return "thinking", "готово", true
	case ActAgent:
		return "thinking", "агент работает", true
	}
	return "", "", false
}

// toolInput is the part of a tool call's input the activity reads.
type toolInput struct {
	FilePath     string `json:"file_path"`
	NotebookPath string `json:"notebook_path"`
	Path         string `json:"path"`
	Command      any    `json:"command"`
}

// ToolActivity is what a tool call of a session is doing, as the activity
// type and a short text; folder is the session's folder paths are shown
// relative to.
func ToolActivity(folder, tool string, input json.RawMessage) (string, string) {
	var in toolInput
	_ = json.Unmarshal(input, &in)
	command := ""
	switch c := in.Command.(type) {
	case string:
		command = c
	case []any: // Codex exec-style argv
		if len(c) > 0 {
			command, _ = c[0].(string)
		}
	}
	switch tool {
	case "Read", "NotebookRead":
		return "read", "читает " + showPath(folder, in.FilePath)
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		return "edit", "правит " + showPath(folder, firstNonEmpty(in.FilePath, in.NotebookPath))
	case "apply_patch":
		return "edit", "правит " + showPath(folder, patchFile(command))
	case "Bash", "PowerShell", "shell", "local_shell", "exec_command", "unified_exec":
		if p := program(command); p != "" {
			return "command", "запускает " + p
		}
		return "command", "запускает команду"
	case "Grep", "Glob", "LS", "ToolSearch":
		return "search", "ищет в коде"
	case "WebFetch", "WebSearch", "web_search":
		return "search", "ищет в сети"
	case "Task", "Agent":
		return "tool", "запускает субагента"
	case "":
		return "thinking", "думает"
	}
	return "tool", "работает: " + safeName(tool, 48)
}

// agentlinkAsk matches a shell command line that runs agentlink send or discuss.
var agentlinkAsk = regexp.MustCompile(`(?i)agentlink(\.exe|\.cmd)?["']?\s+(send|discuss)\b`)

// ShellAsk reports whether a shell command line runs agentlink send or
// discuss, and which of the two.
func ShellAsk(command string) (verb string, ok bool) {
	m := agentlinkAsk.FindStringSubmatch(command)
	if len(m) < 3 {
		return "", false
	}
	return strings.ToLower(m[2]), true
}

// DoingOf is the state of a main-agent tool call: its kind by name
// (ToolKind), and waiting for an answer when a shell runs agentlink discuss.
func DoingOf(tool string, input json.RawMessage) string {
	kind := ToolKind(tool)
	if kind != AgentCommand {
		return kind
	}
	var in toolInput
	_ = json.Unmarshal(input, &in)
	command := ""
	switch c := in.Command.(type) {
	case string:
		command = c
	case []any:
		for _, a := range c {
			if s, ok := a.(string); ok {
				command += s + " "
			}
		}
	}
	if verb, ok := ShellAsk(command); ok && verb == "discuss" {
		return AgentWaiting
	}
	return kind
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

// showPath is p as the activity shows it: relative to folder when inside it,
// else only its file name; "файл" when there is none.
func showPath(folder, p string) string {
	if p == "" {
		return "файл"
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(folder, p)
	}
	rel, err := filepath.Rel(folder, filepath.Clean(p))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		rel = filepath.Base(p)
	}
	rel = filepath.ToSlash(rel)
	if r := []rune(rel); len(r) > 120 {
		rel = "…" + string(r[len(r)-119:])
	}
	return rel
}

var patchFileLine = regexp.MustCompile(`(?m)^\*\*\* (?:Update|Add|Delete) File: (.+)$`)

// patchFile is the first file an apply_patch patch changes.
func patchFile(patch string) string {
	if m := patchFileLine.FindStringSubmatch(patch); m != nil {
		return strings.TrimSpace(m[1])
	}
	return ""
}

// program is the program a command line runs, without its path, extension
// and arguments: `FOO=1 & "C:\Go\bin\go.exe" test ./...` -> "go".
func program(command string) string {
	for _, tok := range splitCommand(command) {
		tok = strings.Trim(tok, "`()")
		switch {
		case tok == "", tok == "&", tok == ".", tok == "sudo", tok == "env", tok == "time", tok == "nohup", tok == "exec":
			continue
		case strings.Contains(tok, "=") && !strings.ContainsAny(tok, `/\`):
			continue // VAR=value
		}
		base := filepath.Base(strings.ReplaceAll(tok, `\`, "/"))
		if ext := filepath.Ext(base); ext != "" && len(ext) <= 5 {
			base = strings.TrimSuffix(base, ext)
		}
		return safeName(base, 40)
	}
	return ""
}

// splitCommand splits a command line into words, keeping quoted parts ("..."
// or '...') together without their quotes. Enough to find the program.
func splitCommand(s string) []string {
	var out []string
	var cur strings.Builder
	quote, in := rune(0), false
	for _, r := range s {
		switch {
		case quote != 0 && r == quote:
			quote = 0
		case quote != 0:
			cur.WriteRune(r)
		case r == '"' || r == '\'':
			quote, in = r, true
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			if in {
				out = append(out, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteRune(r)
			in = true
		}
	}
	if in {
		out = append(out, cur.String())
	}
	return out
}

var nameJunk = regexp.MustCompile(`[^A-Za-z0-9._+-]`)

// safeName keeps only harmless name characters, at most n.
func safeName(s string, n int) string {
	s = nameJunk.ReplaceAllString(s, "")
	if utf8.RuneCountInString(s) > n {
		s = s[:n]
	}
	return s
}
