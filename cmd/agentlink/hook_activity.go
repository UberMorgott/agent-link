package main

// Live activity of a session working on a batch it accepted: short texts
// («читает rel/path», «правит rel/path», «запускает go», «думает») posted to
// the chats of that batch (POST /chats/{id}/activity). Never raw arguments,
// file contents or command lines: only a tool kind, a relative path and a
// program name.

import (
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/UberMorgott/agent-link/internal/node"
)

// Activity coalescing: the same text again within hookSameActivity is not
// posted, nor anything within hookMinActivity of the last post.
const (
	hookSameActivity = 20 * time.Second
	hookMinActivity  = 300 * time.Millisecond
)

// report posts activity typ/text to every chat this session works on,
// coalesced; phase "" is running.
func (h *hookSession) report(typ, text, phase string) {
	if len(h.st.Active) == 0 || text == "" {
		return
	}
	now := h.env.clock()
	if phase != node.PhaseIdle {
		since := now.Sub(h.st.ActivityAt)
		if since < hookMinActivity || text == h.st.Activity && since < hookSameActivity {
			return
		}
	}
	posted := false
	for chat, replyTo := range h.st.Active {
		req := node.ActivityRequest{SessionID: h.sid, ReplyTo: replyTo, Type: typ, Text: text, Phase: phase}
		err := hookCall(h.env.api, http.MethodPost, "/chats/"+url.PathEscape(chat)+"/activity", nil, req, nil, hookHTTPTimeout)
		var se *statusError
		switch {
		case err == nil:
			posted = true
		case errors.As(err, &se) && (se.code == http.StatusNotFound || se.code == http.StatusBadRequest):
			delete(h.st.Active, chat) // chat or session gone: nothing to report there
		}
	}
	if posted {
		h.st.Activity, h.st.ActivityAt = text, now
	}
}

// reportMain reports only what hooks can honestly attribute to the main
// agent. Codex tool hooks do not identify the agent that ran them, so while
// children are active their tool calls must not be presented as the parent's.
func (h *hookSession) reportMain(typ, text, phase string) {
	if h.client == hookCodex && phase != node.PhaseIdle && len(h.st.Subagents) > 0 {
		typ, text = "thinking", "работает с субагентами"
	}
	h.report(typ, text, phase)
}

// reportCodexAgentState replaces stale parent activity immediately when a
// child starts or the last child stops. Codex exposes child lifecycle, but not
// child ownership on tool hooks.
func (h *hookSession) reportCodexAgentState() {
	if h.client != hookCodex || len(h.st.Active) == 0 {
		return
	}
	h.st.ActivityAt = time.Time{}
	h.reportMain("thinking", "думает", "")
}

// agentActivity is a subagent's last activity posted (hookState.AgentActivity).
type agentActivity struct {
	Text string    `json:"text"`
	At   time.Time `json:"at"`
}

// subagent reports a child's lifecycle under its parent's registered session.
func (h *hookSession) subagent(id, label string, stopped bool) {
	if id == "" || len(id) > 128 {
		return
	}
	var chats map[string]string
	if stopped {
		chats = h.st.Subagents[id]
		delete(h.st.Subagents, id)
		delete(h.st.AgentActivity, id)
	} else {
		chats = h.agentChats(id)
	}
	req := node.ActivityRequest{Type: "thinking", Text: "агент работает"}
	if stopped {
		req.Phase, req.Text = node.PhaseIdle, "готово"
	}
	h.postAgent(chats, id, label, req)
}

// reportAgent posts what subagent id (of type label) is doing, coalesced per
// agent like report is for the main agent.
func (h *hookSession) reportAgent(id, label, typ, text string) {
	if id == "" || len(id) > 128 || text == "" {
		return
	}
	now := h.env.clock()
	last := h.st.AgentActivity[id]
	if since := now.Sub(last.At); since < hookMinActivity || text == last.Text && since < hookSameActivity {
		return
	}
	if h.postAgent(h.agentChats(id), id, label, node.ActivityRequest{Type: typ, Text: text}) {
		if h.st.AgentActivity == nil {
			h.st.AgentActivity = map[string]agentActivity{}
		}
		h.st.AgentActivity[id] = agentActivity{Text: text, At: now}
	}
}

// agentChats are the chats (chat -> request) subagent id works for: those the
// session worked on when it started, kept even if the parent moves on. A
// subagent first heard of now (its start missed) takes the current ones.
func (h *hookSession) agentChats(id string) map[string]string {
	if chats, ok := h.st.Subagents[id]; ok {
		return chats
	}
	if len(h.st.Active) == 0 {
		return nil
	}
	if h.st.Subagents == nil {
		h.st.Subagents = map[string]map[string]string{}
	}
	chats := maps.Clone(h.st.Active)
	h.st.Subagents[id] = chats
	return chats
}

// postAgent posts req as subagent id's activity to chats; it reports whether
// any post went through.
func (h *hookSession) postAgent(chats map[string]string, id, label string, req node.ActivityRequest) bool {
	if len(label) > 128 {
		label = ""
	}
	req.SessionID, req.AgentID, req.Label = h.sid, id, label
	posted := false
	for chat, replyTo := range chats {
		req.ReplyTo = replyTo
		if hookCall(h.env.api, http.MethodPost, "/chats/"+url.PathEscape(chat)+"/activity", nil, req, nil, hookHTTPTimeout) == nil {
			posted = true
		}
	}
	return posted
}

// noteSent makes chatID, where session sid just wrote message msgID
// (agentlink send inside the session), a chat the session reports its
// activity to until its turn ends: everyone in the chat sees what the agent
// that wrote does next. Only for a session its hooks registered here.
func noteSent(env hookEnv, sid, chatID, msgID string) {
	for _, client := range []string{hookClaude, hookCodex} {
		path := hookStatePath(env.dir, client, sid)
		if _, err := os.Stat(path); err != nil {
			continue
		}
		unlock, err := lockFile(path + ".lock")
		if err != nil {
			return
		}
		defer unlock()
		st := loadHookState(path)
		if st.Ended || st.Unbound || st.Folder == "" {
			return
		}
		if st.Active == nil {
			st.Active = map[string]string{}
		}
		st.Active[chatID] = msgID
		st.Activity, st.ActivityAt = "", time.Time{} // shows at once
		(&hookSession{env: env, st: &st, sid: sid, folder: st.Folder, client: client}).reportMain("thinking", "думает", "")
		_ = saveHookState(path, st)
		return
	}
}

// idle ends the activity of every chat this session worked on: its turn is over.
func (h *hookSession) idle() {
	for id := range h.st.Subagents {
		h.subagent(id, "", true)
	}
	if len(h.st.Active) == 0 {
		return
	}
	h.report("thinking", "готово", node.PhaseIdle)
	h.st.Active, h.st.Activity = nil, ""
}

// toolActivity is what a tool call of a session is doing, as the activity
// type and a short text; folder is the session's folder paths are shown
// relative to.
func toolActivity(folder, tool string, input json.RawMessage) (string, string) {
	var in struct {
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
		Path         string `json:"path"`
		Command      any    `json:"command"`
	}
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
