package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/UberMorgott/agent-link/internal/node"
)

// hookAPI is a node API with chats and an inbox the test changes between hooks.
type hookAPI struct {
	mu     sync.Mutex
	chats  []node.ChatInfo
	msgs   map[string][]node.ChatMessage
	inbox  []node.Entry
	writes int // requests other than GET: a hook must make none
	api    string
}

func newHookAPI(t *testing.T) *hookAPI {
	h := &hookAPI{msgs: map[string][]node.ChatMessage{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if r.Method != http.MethodGet || r.URL.Path == "/wait" {
			h.writes++
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/chats":
			_ = json.NewEncoder(w).Encode(h.chats)
		case r.URL.Path == "/inbox":
			_ = json.NewEncoder(w).Encode(h.inbox)
		case strings.HasSuffix(r.URL.Path, "/messages"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/chats/"), "/messages")
			after, _ := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
			out := []node.ChatMessage{}
			for _, m := range h.msgs[id] {
				if m.Seq > after {
					out = append(out, m)
				}
			}
			_ = json.NewEncoder(w).Encode(out)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	h.api = strings.TrimPrefix(srv.URL, "http://")
	return h
}

// addChatMessage appends a message to chat id (created on first use).
func (h *hookAPI) addChatMessage(id, dir, from, kind, body string, responders ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := len(h.msgs[id]) + 1
	seq := uint64(n)
	h.msgs[id] = append(h.msgs[id], node.ChatMessage{Seq: seq, Direction: dir,
		ID: "m" + id + "-" + strconv.Itoa(n), From: from, Body: body, Kind: kind, ChatID: id,
		Responders: responders, CreatedAt: time.Date(2026, 9, 23, 10, n, 0, 0, time.UTC),
	})
	for i := range h.chats {
		if h.chats[i].ID == id {
			if kind == "" {
				h.chats[i].LastSeq = seq
			}
			return
		}
	}
	info := node.ChatInfo{ID: id, Participants: []string{"alice", "bob", "me"},
		Members: []node.ParticipantState{{Name: "me", Self: true}, {Name: "alice"}, {Name: "bob"}}}
	if kind == "" {
		info.LastSeq = seq
	}
	h.chats = append(h.chats, info)
}

func (h *hookAPI) addInbox(id, from, status, body string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	e := node.Entry{Direction: "in", Status: status, Peer: from,
		ID: id, From: from, Body: body, CreatedAt: time.Now().Add(time.Duration(len(h.inbox)-30) * time.Minute)}
	h.inbox = append([]node.Entry{e}, h.inbox...) // newest first, like the node
}

// addReply records this node's reply to inbox message id.
func (h *hookAPI) addReply(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	e := node.Entry{Direction: "out", Status: "sent", ID: "r" + id, From: "me", ReplyTo: id, Body: "done", CreatedAt: time.Now()}
	h.inbox = append([]node.Entry{e}, h.inbox...)
}

type hookCase struct {
	t   *testing.T
	env hookEnv
}

func (c hookCase) run(client, event, input string) string {
	c.t.Helper()
	out, err := hookRun(client, event, strings.NewReader(input), c.env)
	if err != nil {
		c.t.Fatalf("hook %s %s: %v", client, input, err)
	}
	return out
}

func stdin(event string, extra ...string) string {
	return `{"session_id":"s-1","hook_event_name":"` + event + `","cwd":"C:/x"` + strings.Join(extra, "") + `}`
}

func contextOf(t *testing.T, out, event string) string {
	t.Helper()
	var v struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	if v.HookSpecificOutput.HookEventName != event {
		t.Fatalf("hookEventName = %q, want %q", v.HookSpecificOutput.HookEventName, event)
	}
	return v.HookSpecificOutput.AdditionalContext
}

func TestHookReportsOnlyNewMessages(t *testing.T) {
	h := newHookAPI(t)
	h.addChatMessage("c1", "in", "alice", "", "old chat message")
	h.addInbox("i1", "bob", "delivered", "old delivered")
	h.addInbox("i2", "bob", "pending", "waiting for you")
	h.addInbox("i3", "bob", "pending", "old answered")
	h.addReply("i3")
	h.mu.Lock()
	h.inbox = append(h.inbox, node.Entry{Direction: "in", Status: "pending",
		ID: "i0", From: "bob", Body: "old pending from last week", CreatedAt: time.Now().Add(-7 * 24 * time.Hour)})
	h.mu.Unlock()
	c := hookCase{t, hookEnv{api: h.api, dir: t.TempDir()}}

	// First hook of the session: chats are a baseline, pending inbox messages show.
	ctx := contextOf(t, c.run(hookClaude, "auto", stdin(evSessionStart)), evSessionStart)
	if !strings.Contains(ctx, "waiting for you") || strings.Contains(ctx, "old") {
		t.Fatalf("session start context:\n%s", ctx)
	}
	if !strings.Contains(ctx, "agentlink send --to bob --reply-to i2") {
		t.Fatalf("no reply hint:\n%s", ctx)
	}
	if out := c.run(hookClaude, "auto", stdin(evPrompt)); out != "" {
		t.Fatalf("nothing new, got %q", out)
	}

	// Own messages and control/status messages are not news.
	h.addChatMessage("c1", "out", "me", "", "my own words")
	h.addChatMessage("c1", "in", "alice", node.KindStatus, "")
	h.addChatMessage("c1", "in", "bob", "", "please check the build", "me")
	ctx = contextOf(t, c.run(hookClaude, "auto", stdin(evPrompt)), evPrompt)
	for _, want := range []string{"Пришло сообщение от bob в чате c1", "участники: alice, bob, me", "просит ответа от вас", "please check the build", "agentlink send --chat c1"} {
		if !strings.Contains(ctx, want) {
			t.Fatalf("missing %q in:\n%s", want, ctx)
		}
	}
	if strings.Contains(ctx, "my own words") || strings.Contains(ctx, "old chat") {
		t.Fatalf("stale or own message shown:\n%s", ctx)
	}

	// A chat created after the session started shows in full.
	h.addChatMessage("c2", "in", "alice", "", "new chat hello")
	ctx = contextOf(t, c.run(hookClaude, evPostTool, stdin("")), evPostTool)
	if !strings.Contains(ctx, "new chat hello") {
		t.Fatalf("new chat:\n%s", ctx)
	}
	if h.writes != 0 {
		t.Fatalf("hook made %d non-GET requests: it must never claim messages", h.writes)
	}
}

func TestHookStop(t *testing.T) {
	h := newHookAPI(t)
	c := hookCase{t, hookEnv{api: h.api, dir: t.TempDir()}}
	for _, client := range []string{hookClaude, hookCodex} {
		c.run(client, "auto", stdin(evSessionStart))
	}
	h.addChatMessage("c1", "in", "alice", "", "stop and read this")
	for _, client := range []string{hookClaude, hookCodex} {
		var v struct {
			Decision string `json:"decision"`
			Reason   string `json:"reason"`
		}
		if err := json.Unmarshal([]byte(c.run(client, "auto", stdin(evStop, `,"stop_hook_active":false`))), &v); err != nil {
			t.Fatal(err)
		}
		if v.Decision != "block" || !strings.Contains(v.Reason, "stop and read this") {
			t.Fatalf("%s stop: %+v", client, v)
		}
	}
	// Continued by the block, nothing newer: let the agent stop.
	if out := c.run(hookClaude, "auto", stdin(evStop, `,"stop_hook_active":true`)); out != "" {
		t.Fatalf("claude: %q", out)
	}
	if out := c.run(hookCodex, "auto", stdin(evStop, `,"stop_hook_active":true`)); out != "{}" {
		t.Fatalf("codex wants JSON on Stop: %q", out)
	}
	// A genuinely new message blocks again even while stop_hook_active.
	h.addChatMessage("c1", "in", "bob", "", "one more")
	if out := c.run(hookClaude, "auto", stdin(evStop, `,"stop_hook_active":true`)); !strings.Contains(out, "one more") {
		t.Fatalf("new message on active stop: %q", out)
	}
}

func TestHookCapsAndIgnores(t *testing.T) {
	h := newHookAPI(t)
	dir := t.TempDir()
	c := hookCase{t, hookEnv{api: h.api, dir: dir}}
	c.run(hookClaude, "auto", stdin(evSessionStart))
	for i := range hookMaxMessages + 2 {
		h.addChatMessage("c1", "in", "bob", "", "msg"+string(rune('a'+i)))
	}
	h.addChatMessage("c1", "in", "alice", "", strings.Repeat("я", hookMaxBody+100))
	ctx := contextOf(t, c.run(hookClaude, "auto", stdin(evPrompt)), evPrompt)
	if !strings.Contains(ctx, strings.Repeat("я", hookMaxBody)) || strings.Contains(ctx, strings.Repeat("я", hookMaxBody+1)) ||
		!strings.Contains(ctx, "обрезано") || !strings.Contains(ctx, "И ещё 3 более ранних") {
		t.Fatalf("not capped:\n%.300s", ctx)
	}
	// Events the hook does not answer print nothing and leave the cursor alone.
	h.addChatMessage("c1", "in", "bob", "", "later")
	if out := c.run(hookClaude, "auto", stdin("Notification")); out != "" {
		t.Fatalf("notification: %q", out)
	}
	if ctx := contextOf(t, c.run(hookClaude, "auto", stdin(evPrompt)), evPrompt); !strings.Contains(ctx, "later") {
		t.Fatalf("message lost after an ignored event:\n%s", ctx)
	}
	if out := c.run(hookClaude, "auto", `{"hook_event_name":"UserPromptSubmit"}`); out != "" {
		t.Fatalf("no session id: %q", out)
	}
}

func TestHookUnreachableNodeIsSilent(t *testing.T) {
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	dir := t.TempDir()
	t.Setenv(envAPI, addr)
	t.Setenv(envJobID, "")
	t.Setenv("AppData", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	for _, ev := range hookEvents {
		for _, client := range []string{hookClaude, hookCodex} {
			var out, errw bytes.Buffer
			if code := runHook([]string{client}, strings.NewReader(stdin(ev)), &out, &errw); code != 0 || errw.Len() != 0 {
				t.Fatalf("%s %s: code %d stderr %q", client, ev, code, errw.String())
			}
			if client == hookClaude && out.Len() != 0 {
				t.Fatalf("%s: output %q", ev, out.String())
			}
		}
	}
	// Garbage on stdin must not break the session either.
	var out, errw bytes.Buffer
	if code := runHook([]string{hookClaude}, strings.NewReader("not json"), &out, &errw); code != 0 || out.Len() != 0 {
		t.Fatalf("garbage: code %d out %q", code, out.String())
	}
}

func TestHookInsideJobIsSilent(t *testing.T) {
	h := newHookAPI(t)
	h.addInbox("i1", "bob", "pending", "for the job")
	dir := t.TempDir()
	t.Setenv(envAPI, h.api)
	t.Setenv(envJobID, "j1")
	t.Setenv("AppData", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	var out, errw bytes.Buffer
	if code := runHook([]string{hookClaude}, strings.NewReader(stdin(evSessionStart)), &out, &errw); code != 0 || out.Len() != 0 {
		t.Fatalf("inside a job: code %d out %q", code, out.String())
	}
	t.Setenv(envJobID, "")
	if runHook([]string{hookClaude}, strings.NewReader(stdin(evSessionStart)), &out, &errw); !strings.Contains(out.String(), "for the job") {
		t.Fatalf("outside a job: %q %q", out.String(), errw.String())
	}
}

func TestHookAreaFilter(t *testing.T) {
	root := t.TempDir()
	work, dev, devSub := filepath.Join(root, "work"), filepath.Join(root, "dev"), filepath.Join(root, "dev", "ui")
	projects := map[string]string{"dev": dev, "ui": devSub}
	h := newHookAPI(t)
	c := hookCase{t, hookEnv{api: h.api, dir: t.TempDir(), projects: projects}}
	session := func(id, cwd string) string {
		b, _ := json.Marshal(map[string]string{"session_id": id, "hook_event_name": evPrompt, "cwd": cwd})
		return string(b)
	}
	for _, id := range []string{"w", "d", "u", "o"} {
		c.run(hookClaude, evSessionStart, `{"session_id":"`+id+`","hook_event_name":"SessionStart"}`)
	}
	h.addChatMessage("plain", "in", "alice", "", "plain chat")
	h.addChatMessage("devchat", "in", "alice", "", "dev chat")
	h.addChatMessage("uichat", "in", "alice", "", "ui chat")
	h.addChatMessage("docs", "in", "alice", "", "docs chat") // an area without a project folder
	h.mu.Lock()
	for i, area := range map[int]string{1: "dev", 2: "ui", 3: "docs"} {
		h.chats[i].Area = area
	}
	h.mu.Unlock()
	h.addInbox("d1", "bob", "pending", "direct request")
	h.mu.Lock()
	e := node.Entry{Direction: "in", Status: "pending"}
	e.ID, e.From, e.To, e.Area, e.Body, e.CreatedAt = "a1", "bob", "area:dev", "dev", "legacy dev request", time.Now()
	h.inbox = append([]node.Entry{e}, h.inbox...)
	h.mu.Unlock()

	all := []string{"plain chat", "dev chat", "ui chat", "docs chat", "direct request", "legacy dev request"}
	for _, tc := range []struct {
		id, cwd string
		want    []string
	}{
		{"w", work, []string{"plain chat", "docs chat", "direct request"}},
		{"o", "", []string{"plain chat", "docs chat", "direct request"}},
		{"d", strings.ToUpper(dev), []string{"dev chat", "legacy dev request"}},
		{"u", filepath.Join(devSub, "src"), []string{"ui chat"}},
	} {
		ctx := contextOf(t, c.run(hookClaude, "auto", session(tc.id, tc.cwd)), evPrompt)
		for _, s := range all {
			if strings.Contains(ctx, s) != slices.Contains(tc.want, s) {
				t.Fatalf("session in %q: %q shown=%v, want %v:\n%s", tc.cwd, s, strings.Contains(ctx, s), tc.want, ctx)
			}
		}
	}
}
