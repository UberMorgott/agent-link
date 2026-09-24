package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/UberMorgott/agent-link/internal/agenthook"
	"github.com/UberMorgott/agent-link/internal/node"
)

// fakeNode is the part of a node's control API the hook uses: sessions,
// unread, ack and activity.
type fakeNode struct {
	mu       sync.Mutex
	folder   string // the one folder bound to this node
	sessions map[string]node.SessionRequest
	ended    []string
	unread   []node.UnreadMessage
	acked    map[string]string // id -> session_id
	worker   map[string]bool   // ids the worker takes (ack answers assigned:"worker")
	activity []node.ActivityRequest
	actChat  []string
	gets     int // GET /unread calls
	api      string
	// claims: with claimOn, POST /claim grants a message to the first session
	// that claims it (id -> session); without, /claim is 404 like an older node.
	claimOn bool
	claims  map[string]string
	// unreadSession: the session the last GET /unread asked for.
	unreadSession string
}

func newFakeNode(t *testing.T, folder string) *fakeNode {
	f := &fakeNode{folder: folder, sessions: map[string]node.SessionRequest{}, acked: map[string]string{}, worker: map[string]bool{},
		claims: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	f.api = strings.TrimPrefix(srv.URL, "http://")
	return f
}

func (f *fakeNode) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/sessions":
		var req node.SessionRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if !strings.EqualFold(filepath.Clean(req.Folder), filepath.Clean(f.folder)) {
			http.Error(w, "folder is not the working folder or a project folder of this node", http.StatusBadRequest)
			return
		}
		f.sessions[req.SessionID] = req
		_ = json.NewEncoder(w).Encode(node.Session{SessionID: req.SessionID})
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/sessions/"):
		id := strings.TrimPrefix(r.URL.Path, "/sessions/")
		delete(f.sessions, id)
		f.ended = append(f.ended, id)
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && r.URL.Path == "/unread":
		f.gets++
		q := r.URL.Query()
		f.unreadSession = q.Get("session")
		if !strings.EqualFold(filepath.Clean(q.Get("folder")), filepath.Clean(f.folder)) {
			http.Error(w, "folder is not the working folder", http.StatusBadRequest)
			return
		}
		limit, _ := strconv.Atoi(q.Get("limit"))
		page := node.UnreadPage{Messages: []node.UnreadMessage{}}
		for _, m := range f.unread {
			if _, done := f.acked[m.ID]; done {
				continue
			}
			page.Total++
			if q.Get("after") != "" && m.Cursor <= q.Get("after") {
				continue
			}
			if len(page.Messages) == limit {
				page.Next = page.Messages[limit-1].Cursor
				continue
			}
			page.Messages = append(page.Messages, m)
		}
		_ = json.NewEncoder(w).Encode(page)
	case r.Method == http.MethodPost && r.URL.Path == "/ack":
		var req node.AckRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		res := []node.AckResult{}
		for _, id := range req.IDs {
			_, was := f.acked[id]
			f.acked[id] = req.SessionID
			a := "session:" + req.SessionID
			if f.worker[id] {
				a = "worker"
			}
			res = append(res, node.AckResult{ID: id, Found: true, WasUnread: !was, Assigned: a})
		}
		_ = json.NewEncoder(w).Encode(res)
	case r.Method == http.MethodPost && r.URL.Path == "/claim" && f.claimOn:
		var req node.ClaimRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		granted := []string{}
		for _, id := range req.IDs {
			if s := f.claims[id]; s == "" || s == req.SessionID {
				f.claims[id] = req.SessionID
				granted = append(granted, id)
			}
		}
		_ = json.NewEncoder(w).Encode(granted)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/activity"):
		var req node.ActivityRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if _, ok := f.sessions[req.SessionID]; !ok {
			http.Error(w, "unknown session", http.StatusNotFound)
			return
		}
		f.activity = append(f.activity, req)
		f.actChat = append(f.actChat, strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/chats/"), "/activity"))
		_ = json.NewEncoder(w).Encode(node.Message{})
	default:
		http.NotFound(w, r)
	}
}

// add queues an unread message.
func (f *fakeNode) add(m node.UnreadMessage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := len(f.unread) + 1
	if m.ID == "" {
		m.ID = "m" + strconv.Itoa(n)
	}
	m.Cursor = fmt.Sprintf("%05d-%s", n, m.ID)
	m.Direction, m.Unread = "in", true
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Date(2026, 9, 23, 10, n, 0, 0, time.UTC)
	}
	f.unread = append(f.unread, m)
}

func chatMsg(chat, from, kind, body string, asks bool) node.UnreadMessage {
	m := node.UnreadMessage{AsksYou: asks}
	m.ChatID, m.From, m.AuthorKind, m.Body, m.Participants = chat, from, kind, body, []string{"KPECTIK", "me"}
	return m
}

func (f *fakeNode) texts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, a := range f.activity {
		out = append(out, a.Text+"|"+a.Phase)
	}
	return out
}

func (f *fakeNode) ackedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []string
	for id := range f.acked {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// hookCase runs hook events of one session against a fake node, with a clock
// the test moves.
type hookCase struct {
	t      *testing.T
	f      *fakeNode
	env    hookEnv
	folder string
	now    time.Time
	sid    string
}

func newHookCase(t *testing.T) *hookCase {
	folder := t.TempDir()
	c := &hookCase{t: t, f: newFakeNode(t, folder), folder: folder, now: time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC), sid: "s-1"}
	c.env = hookEnv{api: c.f.api, dir: t.TempDir(), now: func() time.Time { return c.now }}
	return c
}

func (c *hookCase) input(event string, extra ...string) string {
	return `{"session_id":"` + c.sid + `","hook_event_name":"` + event + `","cwd":` + strconv.Quote(c.folder) + strings.Join(extra, "") + `}`
}

// run runs event (moving the clock a second first) and returns its output.
func (c *hookCase) run(client, event string, extra ...string) string {
	c.t.Helper()
	c.now = c.now.Add(time.Second)
	var out bytes.Buffer
	if err := hookRun(client, event, strings.NewReader(c.input(event, extra...)), &out, c.env); err != nil {
		c.t.Fatalf("hook %s %s: %v", client, event, err)
	}
	return strings.TrimSpace(out.String())
}

func (c *hookCase) state(client string) hookState {
	return loadHookState(hookStatePath(c.env.dir, client, c.sid))
}

// hookOut is a hook's JSON output.
type hookOut struct {
	SystemMessage      string `json:"systemMessage"`
	Decision           string `json:"decision"`
	Reason             string `json:"reason"`
	HookSpecificOutput struct {
		HookEventName     string `json:"hookEventName"`
		AdditionalContext string `json:"additionalContext"`
	} `json:"hookSpecificOutput"`
}

func parseOut(t *testing.T, out string) hookOut {
	t.Helper()
	var v hookOut
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("output %q: %v", out, err)
	}
	return v
}

func TestHookSessionLifecycle(t *testing.T) {
	for _, client := range []string{hookClaude, hookCodex} {
		c := newHookCase(t)
		c.run(client, evSessionStart)
		s, ok := c.f.sessions[c.sid]
		wantWake := map[string]string{hookClaude: node.WakeRewake, hookCodex: node.WakeNextEvent}[client]
		if !ok || s.Provider != client || s.Wake != wantWake || !strings.EqualFold(s.Folder, c.folder) || s.TTLSec == 0 {
			t.Fatalf("%s: registered %+v %v", client, s, ok)
		}
		// Events within a minute are no heartbeats; later ones are.
		delete(c.f.sessions, c.sid)
		c.run(client, evPreTool, `,"tool_name":"Read"`)
		if _, ok := c.f.sessions[c.sid]; ok {
			t.Fatalf("%s: heartbeat too early", client)
		}
		c.now = c.now.Add(2 * time.Minute)
		c.run(client, evPostTool, `,"tool_name":"Read"`)
		if _, ok := c.f.sessions[c.sid]; !ok {
			t.Fatalf("%s: no heartbeat", client)
		}
		c.run(client, evSessionEnd)
		if !slices.Contains(c.f.ended, c.sid) || !c.state(client).Ended {
			t.Fatalf("%s: not ended: %v %+v", client, c.f.ended, c.state(client))
		}
	}
}

func TestHookUnboundFolderIsSilent(t *testing.T) {
	c := newHookCase(t)
	c.f.add(chatMsg("c1", "KPECTIK", "agent", "hello", true))
	c.folder = t.TempDir() // not the node's folder
	if out := c.run(hookClaude, evSessionStart); out != "" {
		t.Fatalf("output %q", out)
	}
	if out := c.run(hookCodex, evStop); out != "{}" {
		t.Fatalf("codex stop %q", out)
	}
	if c.f.gets != 0 || len(c.f.acked) != 0 {
		t.Fatalf("unread fetched for an unbound folder: %d %v", c.f.gets, c.f.acked)
	}
}

func TestHookDeliversUnreadOnceAndAcks(t *testing.T) {
	c := newHookCase(t)
	c.f.add(chatMsg("c1", "KPECTIK", "human", "please review PR 12", true))
	c.f.add(chatMsg("c1", "KPECTIK", "agent", "and the tests", true))
	v := parseOut(t, c.run(hookClaude, evSessionStart))
	ctx := v.HookSpecificOutput.AdditionalContext
	for _, want := range []string{"please review PR 12", "and the tests", "KPECTIK (человек)", "KPECTIK (агент)", "чате c1",
		"agentlink send --chat c1 --reply-to m1", "Просит ответа от вас"} {
		if !strings.Contains(ctx, want) {
			t.Fatalf("context lacks %q:\n%s", want, ctx)
		}
	}
	if v.HookSpecificOutput.HookEventName != evSessionStart {
		t.Fatalf("event name %q", v.HookSpecificOutput.HookEventName)
	}
	if v.SystemMessage != "agent-link: 2 сообщения от KPECTIK — беру в работу" {
		t.Fatalf("systemMessage %q", v.SystemMessage)
	}
	if got := c.f.ackedIDs(); !slices.Equal(got, []string{"m1", "m2"}) || c.f.acked["m1"] != c.sid {
		t.Fatalf("acked %v %v", got, c.f.acked)
	}
	if c.state(hookClaude).Active["c1"] != "m2" || !slices.Equal(c.f.texts(), []string{"читает сообщения|"}) {
		t.Fatalf("active %v activity %v", c.state(hookClaude).Active, c.f.texts())
	}
	// Delivered once: the next events have nothing.
	if out := c.run(hookClaude, evPostTool, `,"tool_name":"Read"`); out != "" {
		t.Fatalf("again: %q", out)
	}
	// A new one arrives: UserPromptSubmit gets it, and so does Codex's shape.
	c.f.add(chatMsg("c1", "bob", "", "one more", false))
	v = parseOut(t, c.run(hookClaude, evPrompt))
	if !strings.Contains(v.HookSpecificOutput.AdditionalContext, "one more") || !strings.Contains(v.HookSpecificOutput.AdditionalContext, "К сведению") ||
		v.SystemMessage != "agent-link: 1 сообщение от bob — к сведению" {
		t.Fatalf("prompt: %+v", v)
	}
}

// Two sessions of one folder poll the same unread messages: a message goes to
// the one session the node grants it (POST /claim), never to both. The
// CodeDungeon case: a reply another session asked for was injected into an
// unrelated session too, which acted on it.
func TestHookDeliversOnlyClaimedMessages(t *testing.T) {
	c := newHookCase(t)
	c.f.claimOn = true
	c.f.add(chatMsg("c1", "KPECTIK", "agent", "PR is fine, merge it", false))
	c.f.claims["m1"] = "s-asker" // the session that asked (or that won the race)
	c.run(hookClaude, evSessionStart)
	if out := c.run(hookClaude, evPostTool, `,"tool_name":"Read"`); out != "" {
		t.Fatalf("an unrelated session got the reply: %s", out)
	}
	if c.f.unreadSession != c.sid || len(c.f.acked) != 0 {
		t.Fatalf("unread asked for %q; acked %v", c.f.unreadSession, c.f.acked)
	}
	c.sid = "s-asker"
	v := parseOut(t, c.run(hookClaude, evSessionStart))
	if !strings.Contains(v.HookSpecificOutput.AdditionalContext, "PR is fine") || c.f.acked["m1"] != "s-asker" {
		t.Fatalf("the asking session: %+v, acked %v", v, c.f.acked)
	}
}

// The chat shows what this node's own agent does too: a session that wrote in
// a chat (agentlink send) or read a message of it (a reply, not only a request
// to it) reports its activity there until its turn ends.
func TestHookActivityOfTheWritingSession(t *testing.T) {
	c := newHookCase(t)
	c.run(hookClaude, evSessionStart)
	noteSent(c.env, c.sid, "c9", "own1")
	if st := c.state(hookClaude); st.Active["c9"] != "own1" {
		t.Fatalf("active after send: %+v", st.Active)
	}
	c.run(hookClaude, evPreTool, `,"tool_name":"Edit","tool_input":{"file_path":"x.go"}`)
	c.run(hookClaude, evStop)
	if got := c.f.texts(); !slices.Equal(got, []string{"думает|", "правит x.go|", "готово|idle"}) {
		t.Fatalf("activity %v", got)
	}
	for i, a := range c.f.activity {
		if c.f.actChat[i] != "c9" || a.ReplyTo != "own1" || a.SessionID != c.sid {
			t.Fatalf("activity %d: %s %+v", i, c.f.actChat[i], a)
		}
	}
	// A session unknown to the hooks here (or ended) notes nothing.
	noteSent(c.env, "someone-else", "c9", "own2")
	c.run(hookClaude, evSessionEnd)
	noteSent(c.env, c.sid, "c9", "own3")
	if n := len(c.f.activity); n != 3 {
		t.Fatalf("activity after an unknown or ended session's send: %v", c.f.texts())
	}

	// A reply that asks nothing still makes its chat show the reader's work.
	c = newHookCase(t)
	c.f.add(chatMsg("c1", "KPECTIK", "agent", "done, see PR 7", false))
	c.run(hookClaude, evSessionStart)
	if st := c.state(hookClaude); st.Active["c1"] != "m1" || !slices.Equal(c.f.texts(), []string{"читает сообщения|"}) {
		t.Fatalf("reading a reply: active %v activity %v", st.Active, c.f.texts())
	}
}

func TestHookOwnHumanIsInformation(t *testing.T) {
	c := newHookCase(t)
	m := chatMsg("c1", "me", "human", "I told everyone: ship on Friday", false)
	m.OwnHuman = true
	c.f.add(m)
	v := parseOut(t, c.run(hookCodex, evSessionStart))
	ctx := v.HookSpecificOutput.AdditionalContext
	if !strings.Contains(ctx, "Ваш человек написал всем") || !strings.Contains(ctx, "ship on Friday") || strings.Contains(ctx, "agentlink send") {
		t.Fatalf("context:\n%s", ctx)
	}
	if v.SystemMessage != "" || len(c.state(hookCodex).Active) != 0 || !slices.Equal(c.f.ackedIDs(), []string{"m1"}) {
		t.Fatalf("notice %q active %v acked %v", v.SystemMessage, c.state(hookCodex).Active, c.f.ackedIDs())
	}
}

func TestHookWorkerAssigned(t *testing.T) {
	c := newHookCase(t)
	m := chatMsg("c1", "KPECTIK", "agent", "the worker has this", true)
	m.Assigned = "worker"
	c.f.add(m)
	v := parseOut(t, c.run(hookClaude, evSessionStart))
	if !strings.Contains(v.HookSpecificOutput.AdditionalContext, "не отвечайте") || v.SystemMessage != "agent-link: 1 сообщение от KPECTIK — отвечает агент-обработчик" {
		t.Fatalf("worker message: %+v", v)
	}
	// The worker takes one between listing and ack: the session hears so next.
	c.f.add(chatMsg("c2", "KPECTIK", "agent", "raced", true))
	c.f.worker["m2"] = true
	c.run(hookClaude, evPostTool, `,"tool_name":"Read"`)
	if _, ok := c.state(hookClaude).Active["c2"]; ok {
		t.Fatal("a chat the worker took is active")
	}
	v = parseOut(t, c.run(hookClaude, evPostTool, `,"tool_name":"Read"`))
	if !strings.Contains(v.HookSpecificOutput.AdditionalContext, "m2") || !strings.Contains(v.HookSpecificOutput.AdditionalContext, "агент-обработчик") {
		t.Fatalf("race note: %+v", v)
	}
}

func TestHookStopNoLoop(t *testing.T) {
	c := newHookCase(t)
	if out := c.run(hookClaude, evStop); out != "" {
		t.Fatalf("claude stop without news: %q", out)
	}
	if out := c.run(hookCodex, evStop); out != "{}" {
		t.Fatalf("codex stop without news: %q", out)
	}
	c.f.add(chatMsg("c1", "KPECTIK", "agent", "while you worked", true))
	v := parseOut(t, c.run(hookClaude, evStop))
	if v.Decision != "block" || !strings.Contains(v.Reason, "while you worked") || v.SystemMessage == "" {
		t.Fatalf("stop with news: %+v", v)
	}
	// Continuing because of the block, nothing new: it may stop, and the
	// activity of the chat it worked on ends.
	if out := c.run(hookClaude, evStop, `,"stop_hook_active":true`); out != "" {
		t.Fatalf("stop after block: %q", out)
	}
	if texts := c.f.texts(); texts[len(texts)-1] != "готово|idle" || len(c.state(hookClaude).Active) != 0 {
		t.Fatalf("idle: %v %v", texts, c.state(hookClaude).Active)
	}
	// Agents answering each other forever: after hookMaxStopBlocks the stop wins.
	blocks := 0
	for i := range hookMaxStopBlocks + 2 {
		c.f.add(chatMsg("c1", "KPECTIK", "agent", "ping "+strconv.Itoa(i), true))
		if out := c.run(hookClaude, evStop, `,"stop_hook_active":`+strconv.FormatBool(i > 0)); out != "" && parseOut(t, out).Decision == "block" {
			blocks++
		}
	}
	if blocks != hookMaxStopBlocks {
		t.Fatalf("blocks %d, want %d", blocks, hookMaxStopBlocks)
	}
	st := c.state(hookClaude)
	if st.Blocks != hookMaxStopBlocks {
		t.Fatalf("state blocks %d", st.Blocks)
	}
	// The stop that wins ends the turn's activity too.
	if texts := c.f.texts(); texts[len(texts)-1] != "готово|idle" || len(st.Active) != 0 {
		t.Fatalf("idle after the last block: %v %v", texts, st.Active)
	}
}

// A session that ends while working on a chat ends its activity there.
func TestHookSessionEndEndsActivity(t *testing.T) {
	c := newHookCase(t)
	c.f.add(chatMsg("c1", "KPECTIK", "agent", "do it", true))
	c.run(hookClaude, evPrompt)
	if len(c.state(hookClaude).Active) != 1 {
		t.Fatalf("not working on c1: %+v", c.state(hookClaude))
	}
	c.run(hookClaude, evSessionEnd)
	if texts := c.f.texts(); texts[len(texts)-1] != "готово|idle" || len(c.state(hookClaude).Active) != 0 {
		t.Fatalf("session end: %v %+v", texts, c.state(hookClaude))
	}
}

func TestHookPagesLargeBatches(t *testing.T) {
	c := newHookCase(t)
	c.f.add(chatMsg("c1", "KPECTIK", "agent", strings.Repeat("я", hookMaxBody+100), true))
	for range 3 {
		c.f.add(chatMsg("c1", "KPECTIK", "agent", strings.Repeat("ы", 1500), true))
	}
	ctx := parseOut(t, c.run(hookClaude, evSessionStart)).HookSpecificOutput.AdditionalContext
	if !strings.Contains(ctx, "обрезано") || !strings.Contains(ctx, "agentlink chat history --chat c1") {
		t.Fatalf("long body not capped:\n%.300s", ctx)
	}
	if !strings.Contains(ctx, "agentlink chat unread --folder") || !strings.Contains(ctx, "--after 00002-m2") || utf8Len(ctx) > 10000 {
		t.Fatalf("paging (%d runes):\n%s", utf8Len(ctx), ctx[len(ctx)-400:])
	}
	if got := c.f.ackedIDs(); !slices.Equal(got, []string{"m1", "m2"}) {
		t.Fatalf("acked %v", got)
	}
	ctx = parseOut(t, c.run(hookClaude, evPostTool, `,"tool_name":"Read"`)).HookSpecificOutput.AdditionalContext
	if !strings.Contains(ctx, "id m3") || !strings.Contains(ctx, "id m4") || len(c.f.ackedIDs()) != 4 {
		t.Fatalf("rest: %v", c.f.ackedIDs())
	}
}

func utf8Len(s string) int { return len([]rune(s)) }

func TestHookTelemetry(t *testing.T) {
	c := newHookCase(t)
	file := filepath.Join(c.folder, "src", "main.go")
	pre := func(tool, input string) {
		c.now = c.now.Add(time.Second)
		c.run(hookClaude, evPreTool, `,"tool_name":"`+tool+`","tool_input":`+input)
	}
	// No accepted batch: nothing is reported.
	pre("Read", `{"file_path":`+strconv.Quote(file)+`}`)
	if len(c.f.texts()) != 0 {
		t.Fatalf("activity without a batch: %v", c.f.texts())
	}
	c.f.add(chatMsg("c1", "KPECTIK", "agent", "fix it", true))
	c.run(hookClaude, evPrompt)
	pre("Read", `{"file_path":`+strconv.Quote(file)+`}`)
	pre("Read", `{"file_path":`+strconv.Quote(file)+`}`) // same again: coalesced
	pre("Edit", `{"file_path":"C:/elsewhere/secret/notes.txt","old_string":"token=abc"}`)
	pre("Bash", `{"command":"API_KEY=xyz \"C:\\Program Files\\Go\\bin\\go.exe\" test ./... --token=abc"}`)
	pre("apply_patch", `{"command":"*** Begin Patch\n*** Update File: pkg/a.go\n@@\n-x\n+y\n*** End Patch"}`)
	pre("mcp__srv__do", `{"q":"private"}`)
	c.run(hookClaude, evStop)
	want := []string{"читает сообщения|", "читает src/main.go|", "правит notes.txt|", "запускает go|", "правит pkg/a.go|",
		"работает: mcp__srv__do|", "готово|idle"}
	if got := c.f.texts(); !slices.Equal(got, want) {
		t.Fatalf("activity\n got %q\nwant %q", got, want)
	}
	for _, a := range c.f.activity {
		if a.ReplyTo != "m1" || a.SessionID != c.sid || strings.Contains(a.Text, "abc") || strings.Contains(a.Text, "xyz") {
			t.Fatalf("activity %+v", a)
		}
	}
	// A new batch after the turn ended starts reporting again.
	c.f.add(chatMsg("c1", "KPECTIK", "agent", "again", true))
	c.run(hookClaude, evPostTool, `,"tool_name":"Read"`)
	n := len(c.f.texts())
	c.run(hookClaude, evPreTool, `,"tool_name":"Grep"`)
	if len(c.f.texts()) != n+1 {
		t.Fatalf("search not reported: %v", c.f.texts())
	}
}

func TestToolActivity(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "p")
	for _, tc := range []struct{ tool, input, typ, text string }{
		{"Read", `{"file_path":"a/b.go"}`, "read", "читает a/b.go"},
		{"Write", `{}`, "edit", "правит файл"},
		{"NotebookEdit", `{"notebook_path":"n.ipynb"}`, "edit", "правит n.ipynb"},
		{"PowerShell", `{"command":"& 'C:\\x y\\git.exe' push --force"}`, "command", "запускает git"},
		{"Bash", `{"command":"sudo FOO=1 ./scripts/run.sh --secret s"}`, "command", "запускает run"},
		{"Bash", `{"command":""}`, "command", "запускает команду"},
		{"shell", `{"command":["bash","-lc","ls"]}`, "command", "запускает bash"},
		{"Grep", `{"pattern":"password"}`, "search", "ищет в коде"},
		{"WebFetch", `{"url":"https://x/?k=1"}`, "search", "ищет в сети"},
		{"Task", `{}`, "tool", "запускает субагента"},
	} {
		typ, text := toolActivity(dir, tc.tool, json.RawMessage(tc.input))
		if typ != tc.typ || text != tc.text {
			t.Errorf("%s %s: %q %q, want %q %q", tc.tool, tc.input, typ, text, tc.typ, tc.text)
		}
	}
	if got := plural(1, "a", "b", "c") + plural(3, "a", "b", "c") + plural(11, "a", "b", "c") + plural(22, "a", "b", "c"); got != "abcb" {
		t.Fatal(got)
	}
}

func TestHookWaitWakesIdleSession(t *testing.T) {
	c := newHookCase(t)
	c.run(hookClaude, evSessionStart)
	c.run(hookClaude, evStop) // idle now
	o := waitOpts{poll: 10 * time.Millisecond, heartbeat: time.Hour, life: 5 * time.Second, busyFor: time.Hour, stale: time.Minute}
	go func() {
		time.Sleep(100 * time.Millisecond)
		c.f.add(chatMsg("c1", "KPECTIK", "human", "wake up", true))
	}()
	var errw bytes.Buffer
	env := c.env
	env.now = time.Now
	if code := hookWait(hookClaude, strings.NewReader(c.input(evStop)), &errw, env, o); code != 2 {
		t.Fatalf("code %d", code)
	}
	if !strings.Contains(errw.String(), "wake up") || !strings.Contains(errw.String(), "agentlink send --chat c1") {
		t.Fatalf("stderr %q", errw.String())
	}
	if !slices.Equal(c.f.ackedIDs(), []string{"m1"}) {
		t.Fatalf("acked %v", c.f.ackedIDs())
	}
	// The person sees the line at the session's next event.
	if v := parseOut(t, c.run(hookClaude, evPreTool, `,"tool_name":"Read"`)); v.SystemMessage != "agent-link: 1 сообщение от KPECTIK — беру в работу" {
		t.Fatalf("notice %+v", v)
	}
	if c.state(hookClaude).Notice != "" {
		t.Fatal("notice shown twice")
	}
}

func TestHookWaitLeavesBusySessionAndEnds(t *testing.T) {
	c := newHookCase(t)
	c.run(hookClaude, evSessionStart)
	c.run(hookClaude, evPreTool, `,"tool_name":"Read"`) // busy
	c.f.add(chatMsg("c1", "KPECTIK", "agent", "later", true))
	env := c.env
	env.now = func() time.Time { return c.now }
	o := waitOpts{poll: 10 * time.Millisecond, heartbeat: time.Hour, life: 200 * time.Millisecond, busyFor: time.Hour, stale: time.Minute}
	var errw bytes.Buffer
	if code := hookWait(hookClaude, strings.NewReader(c.input(evStop)), &errw, env, o); code != 0 || errw.Len() != 0 || len(c.f.ackedIDs()) != 0 {
		t.Fatalf("busy: code %d stderr %q acked %v", code, errw.String(), c.f.ackedIDs())
	}
	// One waiter per session.
	path := hookStatePath(c.env.dir, hookClaude, c.sid) + ".wait"
	release, ok := takeWaitLock(path, time.Minute)
	if !ok {
		t.Fatal("lock")
	}
	if code := hookWait(hookClaude, strings.NewReader(c.input(evStop)), &errw, env, o); code != 0 || len(c.f.ackedIDs()) != 0 {
		t.Fatalf("second waiter: %d", code)
	}
	release()
	// A gone parent ends the session.
	o.alive = func() bool { return false }
	if code := hookWait(hookClaude, strings.NewReader(c.input(evStop)), &errw, env, o); code != 0 || !slices.Contains(c.f.ended, c.sid) {
		t.Fatalf("parent gone: %d %v", code, c.f.ended)
	}
	// SessionEnd stops a waiter.
	o.alive, o.life = nil, 5*time.Second
	c.run(hookClaude, evSessionEnd)
	start := time.Now()
	if code := hookWait(hookClaude, strings.NewReader(c.input(evStop)), &errw, env, o); code != 0 || time.Since(start) > time.Second {
		t.Fatalf("ended: %d after %v", code, time.Since(start))
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
	in := func(ev string) string {
		return `{"session_id":"s-1","hook_event_name":"` + ev + `","cwd":` + strconv.Quote(dir) + `,"tool_name":"Read"}`
	}
	for _, ev := range agenthook.Events {
		for _, client := range []string{hookClaude, hookCodex} {
			var out, errw bytes.Buffer
			start := time.Now()
			if code := runHook([]string{client}, strings.NewReader(in(ev)), &out, &errw); code != 0 || errw.Len() != 0 {
				t.Fatalf("%s %s: code %d stderr %q", client, ev, code, errw.String())
			}
			if d := time.Since(start); d > time.Second {
				t.Fatalf("%s %s took %v", client, ev, d)
			}
			want := ""
			if client == hookCodex && ev == evStop {
				want = "{}"
			}
			if strings.TrimSpace(out.String()) != want {
				t.Fatalf("%s %s: output %q", client, ev, out.String())
			}
		}
	}
	// Garbage on stdin must not break the session either.
	var out, errw bytes.Buffer
	if code := runHook([]string{hookClaude}, strings.NewReader("not json"), &out, &errw); code != 0 || out.Len() != 0 {
		t.Fatalf("garbage: code %d out %q", code, out.String())
	}
	if code := runHook([]string{hookClaude, "--wait"}, strings.NewReader("not json"), &out, &errw); code != 0 {
		t.Fatalf("wait garbage: code %d", code)
	}
}

func TestHookHeadlessIsSilent(t *testing.T) {
	dir := t.TempDir()
	f := newFakeNode(t, dir)
	f.claimOn = true
	f.add(chatMsg("c1", "KPECTIK", "agent", "for the session", true))
	t.Setenv(envAPI, f.api)
	t.Setenv(envJobID, "")
	t.Setenv("AppData", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	t.Cleanup(func() { hookHeadless = func(string) bool { return false } })
	hookHeadless = func(string) bool { return true }
	for _, client := range []string{hookClaude, hookCodex} {
		for _, ev := range []string{evSessionStart, evPrompt, evPreTool, evPostTool, evStop, evSessionEnd} {
			in := `{"session_id":"s-1","hook_event_name":"` + ev + `","cwd":` + strconv.Quote(dir) + `}`
			for _, args := range [][]string{{client}, {client, "--wait"}} {
				var out, errw bytes.Buffer
				if code := runHook(args, strings.NewReader(in), &out, &errw); code != 0 || out.Len() != 0 {
					t.Fatalf("headless %v %s: code %d out %q", args, ev, code, out.String())
				}
			}
		}
	}
	f.mu.Lock()
	calls := len(f.sessions) + len(f.ended) + f.gets + len(f.claims) + len(f.acked) + len(f.activity)
	f.mu.Unlock()
	if calls != 0 {
		t.Fatalf("headless run reached the node: sessions %v ended %v gets %d claims %v", f.sessions, f.ended, f.gets, f.claims)
	}
}

func TestHeadlessDiscriminator(t *testing.T) {
	t.Setenv(envAttended, "0")
	if !headless(hookClaude) {
		t.Fatal("claude with CLAUDE_CODE_SESSION_ATTENDED=0 is headless")
	}
	t.Setenv(envAttended, "1")
	if headless(hookClaude) {
		t.Fatal("claude with CLAUDE_CODE_SESSION_ATTENDED=1 is interactive")
	}
	for _, c := range []struct {
		client string
		args   []string
		want   bool
	}{
		{hookClaude, []string{"claude.exe", "-p", "--model", "claude-opus-4-6", "--output-format", "text"}, true},
		{hookClaude, []string{"claude", "--print", "hi"}, true},
		{hookClaude, []string{"node", "cli.js", "-p", "hi"}, true},
		{hookClaude, []string{"claude.exe", "--output-format", "stream-json", "--input-format", "stream-json", "--resume=x"}, false},
		{hookClaude, []string{"claude"}, false},
		{hookClaude, []string{"claude", "--", "-p"}, false},
		{hookCodex, []string{"codex.exe", "exec", "--skip-git-repo-check", "hi"}, true},
		{hookCodex, []string{"codex", "-c", "k=v", "exec", "hi"}, true},
		{hookCodex, []string{"codex", "e", "hi"}, true},
		{hookCodex, []string{"codex"}, false},
		{hookCodex, []string{"codex", "resume", "--last"}, false},
		{hookCodex, []string{"codex", "app-server"}, false},
		{hookCodex, nil, false},
	} {
		if got := headlessArgs(c.client, c.args); got != c.want {
			t.Errorf("headlessArgs(%s, %q) = %v, want %v", c.client, c.args, got, c.want)
		}
	}
}

// agentCommandLine finds the ancestor by name and reads its real command
// line: a child of this test binary looks for it.
func TestAgentCommandLine(t *testing.T) {
	const envName = "AGENTLINK_TEST_AGENT_NAME"
	if name := os.Getenv(envName); name != "" {
		fmt.Print(strings.Join(agentCommandLine(name), "\n"))
		os.Exit(0)
	}
	self := strings.TrimSuffix(strings.ToLower(filepath.Base(os.Args[0])), ".exe")
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestAgentCommandLine$") // #nosec G204 -- the test binary itself
	cmd.Env = append(os.Environ(), envName+"="+self)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(string(out), "\n")
	if base := strings.TrimSuffix(strings.ToLower(filepath.Base(args[0])), ".exe"); base != self || !slices.ContainsFunc(args[1:], func(a string) bool { return strings.HasPrefix(a, "-test.") }) {
		t.Fatalf("agentCommandLine(%q) = %q", self, args)
	}
	if got := agentCommandLine("no-such-agent"); got != nil {
		t.Fatalf("no such ancestor: %q", got)
	}
}

func TestHookInsideJobIsSilent(t *testing.T) {
	dir := t.TempDir()
	f := newFakeNode(t, dir)
	f.add(chatMsg("c1", "KPECTIK", "agent", "for the session", true))
	t.Setenv(envAPI, f.api)
	t.Setenv(envJobID, "j1")
	t.Setenv("AppData", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("HOME", dir)
	in := `{"session_id":"s-1","hook_event_name":"SessionStart","cwd":` + strconv.Quote(dir) + `}`
	var out, errw bytes.Buffer
	if code := runHook([]string{hookClaude}, strings.NewReader(in), &out, &errw); code != 0 || out.Len() != 0 || len(f.sessions) != 0 {
		t.Fatalf("inside a job: code %d out %q", code, out.String())
	}
	t.Setenv(envJobID, "")
	if runHook([]string{hookClaude}, strings.NewReader(in), &out, &errw); !strings.Contains(out.String(), "for the session") {
		t.Fatalf("outside a job: %q %q", out.String(), errw.String())
	}
}
