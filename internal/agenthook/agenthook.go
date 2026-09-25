// Package agenthook edits the hook settings of Claude Code and Codex so their
// sessions run `agentlink hook <client>`: Install adds (or updates) the
// agentlink entries, Remove takes out only those. A settings file keeps its
// other content and key order; the previous version is saved next to it as
// *.agentlink.bak.
package agenthook

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Clients.
const (
	Claude = "claude"
	Codex  = "codex"
)

// Hook events agentlink installs and answers.
const (
	SessionStart  = "SessionStart"
	Prompt        = "UserPromptSubmit"
	PreTool       = "PreToolUse"
	PostTool      = "PostToolUse"
	Stop          = "Stop"
	SessionEnd    = "SessionEnd"
	SubagentStart = "SubagentStart"
	SubagentStop  = "SubagentStop"
)

// Events are installed in this order. SubagentStart/SubagentStop show a
// session's subagents under it (both clients send agent_id and agent_type).
var Events = []string{SessionStart, Prompt, PreTool, PostTool, Stop, SessionEnd, SubagentStart, SubagentStop}

// Hook entry timeouts, in seconds.
const (
	// Timeout is the timeout of an installed hook entry.
	Timeout = 10
	// EndTimeout is SessionEnd's: Codex allows at most 3 seconds there
	// (https://learn.chatgpt.com/docs/hooks), Claude Code 1.5 by default.
	EndTimeout = 3
	// WaitTimeout is the timeout of Claude Code's background wait entry
	// (`agentlink hook claude --wait`, asyncRewake). Claude Code enforces it on
	// an asyncRewake hook; the docs name no maximum. The waiter ends itself a
	// little earlier and is armed again at the next Stop.
	WaitTimeout = 86400
)

// WaitArg is the flag of the background wait entry.
const WaitArg = "--wait"

// ProjectFile is the file of a project folder the hooks of client go to:
// Claude Code's personal .claude/settings.local.json (not the shared
// settings.json) and Codex's .codex/hooks.json, which has no local variant.
func ProjectFile(dir, client string) string {
	if client == Codex {
		return filepath.Join(dir, ".codex", "hooks.json")
	}
	return filepath.Join(dir, ".claude", "settings.local.json")
}

// Handler is one installed hook handler, in the key order people write.
type Handler struct {
	Type    string   `json:"type"`
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
	// AsyncRewake: Claude Code runs it in the background and wakes the session
	// (even an idle one) when it exits with code 2, showing Claude its stderr
	// (https://code.claude.com/docs/en/hooks#command-hook-fields).
	AsyncRewake bool `json:"asyncRewake,omitempty"`
	Timeout     int  `json:"timeout"`
}

// Entry is the hook handler agentlink installs for client. Claude Code spawns
// an exec-form entry (command + args) without a shell, so the path needs no
// quoting; Codex hands command to a shell.
func Entry(client, exe string) Handler {
	exe = filepath.ToSlash(exe)
	if client == Claude {
		return Handler{Type: "command", Command: exe, Args: []string{"hook", client}, Timeout: Timeout}
	}
	if strings.ContainsAny(exe, " \t") {
		exe = `"` + exe + `"`
	}
	return Handler{Type: "command", Command: exe + " hook " + client, Timeout: Timeout}
}

// Handlers are the handlers of event ev for client: the hook itself and, for
// Claude Code on Stop, the background waiter that wakes an idle session when a
// message arrives (armed again at every Stop). Not on SessionStart: Claude
// Code in stream-json mode (the desktop app, the SDK, -p) holds the session's
// start until every SessionStart hook ends, asyncRewake ones included, so a
// waiter there kept the session at "Starting session…" until its timeout.
func Handlers(client, exe, ev string) []Handler {
	h := Entry(client, exe)
	if ev == SessionEnd {
		h.Timeout = EndTimeout
	}
	out := []Handler{h}
	if client == Claude && ev == Stop {
		w := h
		w.Args = append(slices.Clone(h.Args), WaitArg)
		w.AsyncRewake, w.Timeout = true, WaitTimeout
		out = append(out, w)
	}
	return out
}

// isAgentlink reports whether a matcher group runs `agentlink hook client`.
func isAgentlink(group json.RawMessage, client string) bool {
	var g struct {
		Hooks []struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"hooks"`
	}
	if json.Unmarshal(group, &g) != nil {
		return false
	}
	for _, h := range g.Hooks {
		line := strings.ToLower(h.Command + " " + strings.Join(h.Args, " "))
		if strings.Contains(line, "agentlink") && strings.Contains(line, "hook "+client) {
			return true
		}
	}
	return false
}

// Install adds (or updates) the agentlink matcher group of every event in the
// settings file at path, creating it and its folder when missing; an older
// agentlink group (fewer events, no waiter) is replaced in place. It reports
// whether the file changed.
func Install(path, client, exe string) (bool, error) {
	return edit(path, func(ev string, groups []json.RawMessage) []json.RawMessage {
		group := struct {
			Matcher string    `json:"matcher,omitempty"`
			Hooks   []Handler `json:"hooks"`
		}{Hooks: Handlers(client, exe, ev)}
		if ev == PreTool || ev == PostTool {
			group.Matcher = "*"
		}
		want := json.RawMessage(bytes.TrimSpace(mustJSON(group)))
		for i, g := range groups {
			if isAgentlink(g, client) {
				groups[i] = want
				return groups
			}
		}
		return append(groups, want)
	})
}

// Remove takes the agentlink matcher groups of client out of the settings file
// at path; other hooks stay. A missing file is nothing to remove.
func Remove(path, client string) (bool, error) {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return edit(path, func(_ string, groups []json.RawMessage) []json.RawMessage {
		return slices.DeleteFunc(groups, func(g json.RawMessage) bool { return isAgentlink(g, client) })
	})
}

// edit rewrites the matcher groups of every agentlink event with change. An
// event left without groups is dropped, and so is an empty "hooks".
func edit(path string, change func(ev string, groups []json.RawMessage) []json.RawMessage) (bool, error) {
	old, err := os.ReadFile(filepath.Clean(path))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	src := bytes.TrimSpace(old)
	if len(src) == 0 {
		src = []byte("{}")
	}
	top, err := decodeObject(src)
	if err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	hooksRaw, _ := top.get("hooks")
	if hooksRaw == nil {
		hooksRaw = json.RawMessage("{}")
	}
	hooks, err := decodeObject(hooksRaw)
	if err != nil {
		return false, fmt.Errorf("%s: hooks: %w", path, err)
	}
	for _, ev := range Events {
		var groups []json.RawMessage
		if raw, ok := hooks.get(ev); ok {
			if err := json.Unmarshal(raw, &groups); err != nil {
				return false, fmt.Errorf("%s: hooks.%s: %w", path, ev, err)
			}
		}
		if groups = change(ev, groups); len(groups) > 0 {
			hooks.set(ev, mustJSON(groups))
		} else {
			hooks.del(ev)
		}
	}
	if len(hooks) > 0 {
		top.set("hooks", hooks.encode())
	} else {
		top.del("hooks")
	}
	var out bytes.Buffer
	if err := json.Indent(&out, top.encode(), "", "  "); err != nil {
		return false, err
	}
	out.WriteByte('\n')
	if len(src) > 0 && jsonEqual(src, out.Bytes()) {
		return false, nil
	}
	if len(old) > 0 {
		if err := os.WriteFile(path+".agentlink.bak", old, 0o600); err != nil {
			return false, err
		}
	}
	return true, writeFileAtomic(path, out.Bytes())
}

// ExcludeFromGit lists file (inside the git work tree dir) in the repository's
// own .git/info/exclude, so a personal settings file the hooks went to is not
// committed by accident. A folder that is not a git repository is left alone.
func ExcludeFromGit(dir, file string) error {
	if !IsDir(filepath.Join(dir, ".git")) { // no repository, or a linked work tree
		return nil
	}
	rel, err := filepath.Rel(dir, file)
	if err != nil {
		return err
	}
	line := "/" + filepath.ToSlash(rel)
	path := filepath.Join(dir, ".git", "info", "exclude")
	old, err := os.ReadFile(filepath.Clean(path))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for l := range strings.Lines(string(old)) {
		if strings.TrimSpace(l) == line {
			return nil
		}
	}
	if len(old) > 0 && !bytes.HasSuffix(old, []byte("\n")) {
		old = append(old, '\n')
	}
	return writeFileAtomic(path, append(old, line+"\n"...))
}

// IsDir reports whether path is an existing folder.
func IsDir(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

// jsonEqual reports whether a and b hold the same JSON, whitespace aside.
func jsonEqual(a, b []byte) bool {
	var ca, cb bytes.Buffer
	return json.Compact(&ca, a) == nil && json.Compact(&cb, b) == nil && bytes.Equal(ca.Bytes(), cb.Bytes())
}

// object is a JSON object that keeps its key order and raw values.
type object []struct {
	key string
	val json.RawMessage
}

func decodeObject(data []byte) (object, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, errors.New("not a JSON object")
	}
	var o object
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := tok.(string)
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return nil, err
		}
		o.set(key, val)
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return o, nil
}

func (o object) get(key string) (json.RawMessage, bool) {
	for _, kv := range o {
		if kv.key == key {
			return kv.val, true
		}
	}
	return nil, false
}

func (o *object) set(key string, val []byte) {
	val = bytes.TrimSpace(val)
	for i := range *o {
		if (*o)[i].key == key {
			(*o)[i].val = val
			return
		}
	}
	*o = append(*o, struct {
		key string
		val json.RawMessage
	}{key, val})
}

func (o *object) del(key string) {
	*o = slices.DeleteFunc(*o, func(kv struct {
		key string
		val json.RawMessage
	}) bool {
		return kv.key == key
	})
}

func (o object) encode() []byte {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, kv := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(bytes.TrimSpace(mustJSON(kv.key)))
		b.WriteByte(':')
		b.Write(kv.val)
	}
	b.WriteByte('}')
	return b.Bytes()
}

// writeFileAtomic replaces path with data through a temporary file.
func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// mustJSON encodes v without HTML escaping; v is always encodable here.
func mustJSON(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return buf.Bytes()
}
