// Package agenthook edits the hook settings of Claude Code and Codex so their
// sessions run `agentlink hook <client>`: Install adds (or updates) the
// agentlink handlers, Remove takes out only those. A settings file keeps its
// other content and key order; the previous version is saved next to it as
// *.agentlink.bak.
package agenthook

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/UberMorgott/agent-link/internal/fileutil"
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
	// PostToolFailure (Claude Code only) ends an ask call that failed: its
	// stamp is dropped like at PostToolUse.
	PostToolFailure = "PostToolUseFailure"
)

// Events are installed in this order. SubagentStart/SubagentStop show a
// session's subagents under it (both clients send agent_id and agent_type).
var Events = []string{SessionStart, Prompt, PreTool, PostTool, Stop, SessionEnd, SubagentStart, SubagentStop}

// ClaudeEvents are the events installed for Claude Code: Events and
// PostToolFailure, which Codex does not have.
var ClaudeEvents = append(slices.Clone(Events), PostToolFailure)

// EventsFor are the events installed for client.
func EventsFor(client string) []string {
	if client == Claude {
		return ClaudeEvents
	}
	return Events
}

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

// isAgentlink reports whether a hook handler runs `agentlink hook client`.
func isAgentlink(handler json.RawMessage, client string) bool {
	var h struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
	}
	if json.Unmarshal(handler, &h) != nil {
		return false
	}
	line := strings.ToLower(h.Command + " " + strings.Join(h.Args, " "))
	return strings.Contains(line, "agentlink") && strings.Contains(line, "hook "+client)
}

// withoutAgentlink is matcher group g without its agentlink handlers of
// client (had: it held some); nil when none of its handlers is left. The
// group keeps its other keys and handlers as they were.
func withoutAgentlink(g json.RawMessage, client string) (rest json.RawMessage, had bool) {
	obj, err := decodeObject(g)
	if err != nil {
		return g, false
	}
	raw, _ := obj.get("hooks")
	var handlers []json.RawMessage
	if json.Unmarshal(raw, &handlers) != nil {
		return g, false
	}
	kept := slices.DeleteFunc(slices.Clone(handlers), func(h json.RawMessage) bool { return isAgentlink(h, client) })
	if len(kept) == len(handlers) {
		return g, false
	}
	if len(kept) == 0 {
		return nil, true
	}
	obj.set("hooks", mustJSON(kept))
	return obj.encode(), true
}

// Install adds (or updates) agentlink's matcher group of every event in the
// settings file at path, creating it and its folder when missing. Only
// agentlink's own handlers change: an older agentlink group (fewer events, no
// waiter) is replaced in place, and an agentlink handler inside a group of
// the user's is taken out of it, the user's handlers staying. It reports
// whether the file changed.
func Install(path, client, exe string) (bool, error) {
	return edit(path, EventsFor(client), func(ev string, groups []json.RawMessage) []json.RawMessage {
		group := struct {
			Matcher string    `json:"matcher,omitempty"`
			Hooks   []Handler `json:"hooks"`
		}{Hooks: Handlers(client, exe, ev)}
		if ev == PreTool || ev == PostTool || ev == PostToolFailure {
			group.Matcher = "*"
		}
		want := json.RawMessage(bytes.TrimSpace(mustJSON(group)))
		out, placed := make([]json.RawMessage, 0, len(groups)+1), false
		for _, g := range groups {
			rest, had := withoutAgentlink(g, client)
			switch {
			case !had:
				out = append(out, g)
			case rest != nil:
				out = append(out, rest) // the user's handlers of a shared group
			case !placed:
				out, placed = append(out, want), true // agentlink's own group, in place
			}
		}
		if !placed {
			out = append(out, want)
		}
		return out
	})
}

// Remove takes the agentlink handlers of client out of the settings file at
// path, and a matcher group they leave empty; other hooks stay. A missing
// file is nothing to remove.
func Remove(path, client string) (bool, error) {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return edit(path, EventsFor(client), func(_ string, groups []json.RawMessage) []json.RawMessage {
		var out []json.RawMessage
		for _, g := range groups {
			if rest, _ := withoutAgentlink(g, client); rest != nil {
				out = append(out, rest)
			}
		}
		return out
	})
}

// editLockWait bounds the wait for another edit of the same settings file.
const editLockWait = 10 * time.Second

// edit rewrites the matcher groups of each of events with change. An event
// left without groups is dropped, and so is an empty "hooks". Edits of one
// file are serialized across processes (the app's folder hooks, `agentlink
// hook install`): the read-modify-write never loses another edit. The lock
// file lives in the temporary folder, never next to the user's settings.
func edit(path string, events []string, change func(ev string, groups []json.RawMessage) []json.RawMessage) (bool, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false, err
	}
	sum := sha256.Sum256([]byte(strings.ToLower(abs)))
	unlock, err := fileutil.Lock(filepath.Join(os.TempDir(), "agentlink-hooks-"+hex.EncodeToString(sum[:8])+".lock"), editLockWait)
	if err != nil {
		return false, fmt.Errorf("%s: another edit holds it: %w", path, err)
	}
	defer unlock()
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
	for _, ev := range events {
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
	return true, fileutil.WriteAtomic(path, out.Bytes())
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
	return fileutil.WriteAtomic(path, append(old, line+"\n"...))
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

// mustJSON encodes v without HTML escaping; v is always encodable here.
func mustJSON(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return buf.Bytes()
}
