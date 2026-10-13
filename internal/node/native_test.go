package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/UberMorgott/agent-link/internal/config"
)

// unreadOf is a chat message for Render's tests.
func unreadOf(id, from, chat, body string) UnreadMessage {
	var m UnreadMessage
	m.ID, m.From, m.ChatID, m.Body, m.AuthorKind = id, from, chat, body, "agent"
	m.Participants = []string{"a", from}
	m.Cursor = "c-" + id
	return m
}

// The text of a delivery is the node's for every client: a batch reads as the
// hooks' (FormatUnread per message), a wake carries its token, and both name
// what they carry.
func TestRenderBatchAndWake(t *testing.T) {
	ask := unreadOf("m-1", "b", "chat-1", "review this")
	ask.AsksYou = true
	fyi := unreadOf("m-2", "c", "chat-2", "for your information")
	held := unreadOf("m-3", "b", "chat-3", "past the hop limit")
	held.AsksYou, held.Paused = true, true
	mine := unreadOf("m-4", "a", "chat-4", "to everyone")
	mine.OwnHuman = true
	mine.Project = "p-2"
	msgs := []UnreadMessage{ask, fyi, held, mine}

	res, err := Render(RenderRequest{Messages: msgs, Total: 6, Folder: `C:\work`, SessionID: "s-1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"agent-link: непрочитанные сообщения для этой папки (6).",
		FormatUnread(ask), FormatUnread(fyi), FormatUnread(held), FormatUnread(mine),
		"Просит ответа от вас.", "К сведению, ответ не обязателен.", "К сведению, ответ не требуется.",
		"Ещё 2 непрочитанных", `--after c-m-4`, "--session s-1",
	} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("batch lacks %q:\n%s", want, res.Text)
		}
	}
	wantShown := []RenderedMessage{{ID: "m-1"}, {ID: "m-2"}, {ID: "m-3"}, {ID: "m-4", Project: "p-2"}}
	if !slices.Equal(res.Shown, wantShown) || res.Rest != 2 {
		t.Fatalf("shown %+v rest %d", res.Shown, res.Rest)
	}
	// Activity goes to the chats it may answer in: not the held request's, not
	// its own person's.
	if len(res.Chats) != 2 || res.Chats["chat-1"] != "m-1" || res.Chats["chat-2"] != "m-2" {
		t.Fatalf("chats %+v", res.Chats)
	}
	if res.Notice != "agent-link: 3 сообщения от b, c — беру в работу" {
		t.Fatalf("notice %q", res.Notice)
	}

	wake, err := Render(RenderRequest{Messages: msgs[:2], Mode: RenderWake, Token: "tok12345", Folder: `C:\work`})
	if err != nil {
		t.Fatal(err)
	}
	if wake.Text != WakePrompt(msgs[:2], 0, `C:\work`, "tok12345") {
		t.Fatalf("wake text:\n%s", wake.Text)
	}
	woken := ask
	woken.WakeToken = "tok12345"
	if !WokenBy(wake.Text, woken) {
		t.Fatal("the wake prompt does not prove its message")
	}

	stop, err := Render(RenderRequest{Messages: msgs[:1], Mode: RenderStop})
	if err != nil || !strings.HasPrefix(stop.Text, "Пока вы работали, пришли сообщения agent-link.") {
		t.Fatalf("stop text %q %v", stop.Text, err)
	}
	// What a turn's end left unread in the conversation is pointed at, not
	// repeated: the nudge names the count and its token alone.
	nudge, err := Render(RenderRequest{Mode: RenderNudge, Token: "tok23456", Total: 2})
	if err != nil || !strings.Contains(nudge.Text, "(2)") || !strings.Contains(nudge.Text, WakeMarker("tok23456")) ||
		strings.Contains(nudge.Text, "review this") || len(nudge.Shown) != 0 {
		t.Fatalf("nudge %q %v", nudge.Text, err)
	}
	for _, bad := range []RenderRequest{{Mode: "other"}, {Mode: RenderWake, Token: "x"}, {Mode: RenderNudge}, {Messages: make([]UnreadMessage, maxRenderMessages+1)}} {
		if _, err := Render(bad); !errors.Is(err, ErrBadRequest) {
			t.Errorf("render %+v: %v", bad.Mode, err)
		}
	}
}

// A message too many for one text stays for the next delivery, and is not
// named as shown: the client claims and acknowledges only what the text has.
func TestRenderFitsBudget(t *testing.T) {
	var msgs []UnreadMessage
	for _, id := range []string{"m-1", "m-2", "m-3"} {
		msgs = append(msgs, unreadOf(id, "b", "chat-1", id+" "+strings.Repeat("x", 2000)))
	}
	res, err := Render(RenderRequest{Messages: msgs})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Shown) != FitUnread(msgs) || len(res.Shown) >= len(msgs) || res.Rest != len(msgs)-len(res.Shown) {
		t.Fatalf("shown %d of %d, rest %d", len(res.Shown), len(msgs), res.Rest)
	}
	if strings.Contains(res.Text, "m-3 ") || res.Chats["chat-1"] != res.Shown[len(res.Shown)-1].ID {
		t.Fatalf("text carries more than shown; chats %+v", res.Chats)
	}
}

// A reply for a subagent is its run's alone while it lives; the main agent
// reads it, marked, once the subagent ended. Notes lead the text, the CLI's
// path ends a session's first one, and both stand without any message.
func TestRenderForAgentsNotesAndCLI(t *testing.T) {
	plain := unreadOf("m-1", "b", "chat-1", "for the session")
	sub := unreadOf("m-2", "b", "chat-1", "for the subagent")
	sub.ForAgent, sub.ForAgentType = "agent-1", "Explore"
	msgs := []UnreadMessage{plain, sub}

	own, _ := Render(RenderRequest{Messages: msgs, Agent: "agent-1"})
	if !slices.Equal(own.Shown, []RenderedMessage{{ID: "m-2"}}) || strings.Contains(own.Text, "завершившегося") {
		t.Fatalf("the subagent's run: %+v\n%s", own.Shown, own.Text)
	}
	live, _ := Render(RenderRequest{Messages: msgs, LiveAgents: []string{"agent-1"}})
	if !slices.Equal(live.Shown, []RenderedMessage{{ID: "m-1"}}) || strings.Contains(live.Text, "for the subagent") {
		t.Fatalf("the main agent's run beside a live subagent: %+v", live.Shown)
	}
	ended, _ := Render(RenderRequest{Messages: msgs})
	if len(ended.Shown) != 2 || !strings.Contains(ended.Text, EndedAgentNote("Explore", "agent-1")+"\nfor the subagent") {
		t.Fatalf("an ended subagent's reply:\n%s", ended.Text)
	}

	old := cliExecutable
	cliExecutable = func() string { return `C:\apps\agentlink.exe` }
	t.Cleanup(func() { cliExecutable = old })
	note := WorkerNote("m-9", "chat-9")
	res, _ := Render(RenderRequest{Messages: msgs[:1], Notes: []string{note}, CLI: true})
	if !strings.HasPrefix(res.Text, note+"\n\nagent-link: непрочитанные") || !strings.HasSuffix(res.Text, "\n\n"+CLIHint(`C:\apps\agentlink.exe`)) {
		t.Fatalf("notes and the CLI line:\n%s", res.Text)
	}
	none, _ := Render(RenderRequest{Notes: []string{note}, CLI: true})
	if none.Text != note+"\n\n"+CLIHint(`C:\apps\agentlink.exe`) || len(none.Shown) != 0 || none.Notice != "" {
		t.Fatalf("no messages: %q", none.Text)
	}
	if empty, _ := Render(RenderRequest{}); empty.Text != "" {
		t.Fatalf("nothing to say: %q", empty.Text)
	}
}

func nativeQuery(dir string, more ...string) url.Values {
	q := url.Values{"folder": {dir}, "session": {"s-n"}}
	for i := 0; i+1 < len(more); i += 2 {
		q.Set(more[i], more[i+1])
	}
	return q
}

// A session Claude Code's in-process module registered is delivered to by the
// module alone: the shell hooks of the same session (their registration,
// their unread queries, their waiter, their claims) and the node's inbox wake
// stand down, so nothing reaches the session twice.
func TestNativeSessionShellHooksStandDown(t *testing.T) {
	p := &fakePoster{}
	dir := t.TempDir()
	a, b := deliveryPair(t, dir, p, nil)
	classic := SessionRequest{SessionID: "s-n", Provider: ProviderClaude, Folder: dir, Wake: WakeRewake, Idle: true,
		InboxSocket: testSocket, InboxToken: testToken, Agents: []string{"agent-1"}}
	if _, err := a.RegisterSession(classic); err != nil || !a.InboxWakes("s-n") {
		t.Fatalf("the shell hooks' registration: %v", err)
	}
	native := SessionRequest{SessionID: "s-n", Provider: ProviderClaude, Folder: dir, Wake: WakeRewake, Idle: true, Native: true,
		InboxSocket: testSocket, InboxToken: testToken}
	s, err := a.RegisterSession(native)
	if err != nil || !s.Native || a.InboxWakes("s-n") {
		t.Fatalf("the module's registration: %+v %v (inbox %v)", s, err, a.InboxWakes("s-n"))
	}
	// The hooks register again (every event does): it stays the module's.
	classic.Idle = false
	if s, err = a.RegisterSession(classic); err != nil || !s.Native || !s.Idle || len(s.Agents) != 0 || a.InboxWakes("s-n") {
		t.Fatalf("the hooks' registration changed a native session: %+v %v", s, err)
	}

	m := ask(t, a, b, "who delivers this")
	for name, q := range map[string]url.Values{
		"hook":   nativeQuery(dir),
		"stop":   nativeQuery(dir, "actionable", "1"),
		"waiter": nativeQuery(dir, "actionable", "1", "waiter", "1", "limit", "1"),
	} {
		if page, err := a.UnreadQuery(q); err != nil || page.Total != 0 || len(page.Messages) != 0 {
			t.Errorf("the shell %s got %+v %v", name, page, err)
		}
	}
	if got, err := a.Claim(ClaimRequest{IDs: []string{m.ID}, SessionID: "s-n"}); err != nil || len(got) != 0 {
		t.Fatalf("the hooks' claim was granted: %v %v", got, err)
	}
	a.wakeIdle(t.Context())
	if p.count() != 0 {
		t.Fatalf("the node posted a wake to a native session's inbox: %d", p.count())
	}
	// The module's own, and a tool's listing, see it.
	for name, q := range map[string]url.Values{
		"module": nativeQuery(dir, "native", "1", "actionable", "1", "waiter", "1"),
		"tool":   nativeQuery(dir, "agent", "1"),
	} {
		if page, err := a.UnreadQuery(q); err != nil || page.Total != 1 || page.Messages[0].ID != m.ID {
			t.Errorf("the %s got %+v %v", name, page, err)
		}
	}
	if got, err := a.Claim(ClaimRequest{IDs: []string{m.ID}, SessionID: "s-n", Native: true}); err != nil || !slices.Equal(got, []string{m.ID}) {
		t.Fatalf("the module's claim: %v %v", got, err)
	}
	// The session ended: a later one of the hooks alone is theirs again.
	if err := a.EndSession("s-n"); err != nil {
		t.Fatal(err)
	}
	if s, err = a.RegisterSession(classic); err != nil || s.Native || !a.InboxWakes("s-n") {
		t.Fatalf("a new session of the hooks: %+v %v", s, err)
	}
}

// What a wake's turn never read is given back: unread again at once, its
// lease back in retry, so the next delivery takes it (no loss), while the
// claim that held it is gone (no second copy from a stale claim).
func TestReleaseGivesBackWhatNoTurnRead(t *testing.T) {
	dir := t.TempDir()
	a, b := deliveryPair(t, dir, nil, nil)
	if _, err := a.RegisterSession(SessionRequest{SessionID: "s-n", Provider: ProviderClaude, Folder: dir, Wake: WakeRewake, Idle: true, Native: true}); err != nil {
		t.Fatal(err)
	}
	m := ask(t, a, b, "woken, then aborted")
	wake := nativeQuery(dir, "native", "1", "actionable", "1", "waiter", "1")
	if got, err := a.Claim(ClaimRequest{IDs: []string{m.ID}, SessionID: "s-n", Native: true, WakeToken: "tok12345"}); err != nil || len(got) != 1 {
		t.Fatalf("wake claim: %v %v", got, err)
	}
	if page, _ := a.UnreadQuery(wake); page.Total != 0 || len(page.Woken) != 1 || page.Woken[0].WakeToken != "tok12345" {
		t.Fatalf("while the wake holds: %+v", page)
	}
	if err := a.Release(ReleaseRequest{IDs: []string{m.ID}, SessionID: "s-n"}); err != nil {
		t.Fatal(err)
	}
	if page, _ := a.UnreadQuery(wake); page.Total != 1 || len(page.Woken) != 0 {
		t.Fatalf("after the release: %+v", page)
	}
	if v := leaseViewOf(t, a, m.ID); v.State != LeaseRetry || v.Reason != "turn_aborted" {
		t.Fatalf("lease after the release: %+v", v)
	}
	// It may wake the session once more (maxWaiterWakes), never in a loop:
	// after that it waits for the session's own turn, which gets it.
	if got, _ := a.Claim(ClaimRequest{IDs: []string{m.ID}, SessionID: "s-n", Native: true, WakeToken: "tok23456"}); len(got) != 1 {
		t.Fatalf("not offered again after the release: %v", got)
	}
	if err := a.Release(ReleaseRequest{IDs: []string{m.ID}, SessionID: "s-n"}); err != nil {
		t.Fatal(err)
	}
	if page, _ := a.UnreadQuery(wake); page.Total != 0 {
		t.Fatalf("a third wake is offered: %+v", page)
	}
	if got, _ := a.Claim(ClaimRequest{IDs: []string{m.ID}, SessionID: "s-n", Native: true, WakeToken: "tok34567"}); len(got) != 0 {
		t.Fatalf("woken a third time: %v", got)
	}
	if page, _ := a.UnreadQuery(nativeQuery(dir, "native", "1")); page.Total != 1 {
		t.Fatalf("not unread for the session's own turn: %+v", page)
	}
	if got, _ := a.Claim(ClaimRequest{IDs: []string{m.ID}, SessionID: "s-n", Native: true}); len(got) != 1 {
		t.Fatalf("the session's turn does not get it: %v", got)
	}
	for _, bad := range []ReleaseRequest{{SessionID: "s-n"}, {IDs: []string{m.ID}, SessionID: "bad id"}} {
		if err := a.Release(bad); !errors.Is(err, ErrBadRequest) {
			t.Errorf("release %+v: %v", bad, err)
		}
	}
}

// GET /unread with wait is held until a message is there, and answered empty
// when the time is up or the client left.
func TestUnreadLongPoll(t *testing.T) {
	oldTick := unreadWaitTick
	unreadWaitTick = 10 * time.Millisecond
	t.Cleanup(func() { unreadWaitTick = oldTick })
	dir := t.TempDir()
	a, b := deliveryPair(t, dir, nil, nil)
	if _, err := a.RegisterSession(SessionRequest{SessionID: "s-n", Provider: ProviderClaude, Folder: dir, Wake: WakeRewake, Idle: true, Native: true}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a.APIHandler())
	t.Cleanup(srv.Close)
	get := func(ctx context.Context, wait string) (UnreadPage, time.Duration) {
		t.Helper()
		q := nativeQuery(dir, "native", "1", "actionable", "1", "waiter", "1", "wait", wait)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/unread?"+q.Encode(), nil)
		start := time.Now()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return UnreadPage{}, time.Since(start)
		}
		defer func() { _ = resp.Body.Close() }()
		var page UnreadPage
		if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&page) != nil {
			t.Fatalf("GET /unread: %s", resp.Status)
		}
		return page, time.Since(start)
	}
	if page, took := get(t.Context(), "1"); page.Total != 0 || took < 900*time.Millisecond || took > 5*time.Second {
		t.Fatalf("nothing unread: %+v after %v", page, took)
	}
	type answer struct {
		page UnreadPage
		took time.Duration
	}
	done := make(chan answer, 1)
	go func() {
		page, took := get(t.Context(), "30")
		done <- answer{page, took}
	}()
	time.Sleep(200 * time.Millisecond)
	m := ask(t, a, b, "arrives while it waits")
	select {
	case got := <-done:
		if got.page.Total != 1 || got.page.Messages[0].ID != m.ID || got.took > 10*time.Second {
			t.Fatalf("held request: %+v after %v", got.page, got.took)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the held request was not answered when the message came")
	}
	// The client left: the node stops looking.
	if _, err := a.Ack("", AckRequest{IDs: []string{m.ID}, SessionID: "s-n"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if _, took := get(ctx, "30"); took > 5*time.Second {
		t.Fatalf("a request whose client left was held %v", took)
	}
	for _, bad := range []string{"x", "-1"} {
		if _, err := WaitUnread(t.Context(), url.Values{"wait": {bad}}, func() (UnreadPage, error) { return UnreadPage{}, nil }); !errors.Is(err, ErrBadRequest) {
			t.Errorf("wait=%s: %v", bad, err)
		}
	}
	if _, err := WaitUnread(t.Context(), url.Values{"wait": {"9999"}}, func() (UnreadPage, error) { return UnreadPage{Total: 1}, nil }); err != nil {
		t.Fatal(err)
	}
}

// POST /render and POST /release are served by the node's API.
func TestRenderAndReleaseRoutes(t *testing.T) {
	a := newTestNode(t, "a", testSecret, nil, t.TempDir(), listen(t), nil)
	srv := httptest.NewServer(a.APIHandler())
	t.Cleanup(srv.Close)
	post := func(path string, body any) (int, []byte) {
		t.Helper()
		data, _ := json.Marshal(body)
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+path, bytes.NewReader(data))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, out
	}
	var res RenderResult
	code, out := post("/render", RenderRequest{Messages: []UnreadMessage{unreadOf("m-1", "b", "chat-1", "hello")}})
	if code != http.StatusOK || json.Unmarshal(out, &res) != nil || len(res.Shown) != 1 || !strings.Contains(res.Text, "hello") {
		t.Fatalf("POST /render: %d %s", code, out)
	}
	if code, _ := post("/render", RenderRequest{Mode: "other"}); code != http.StatusBadRequest {
		t.Fatalf("POST /render with a bad mode: %d", code)
	}
	if code, _ := post("/release", ReleaseRequest{IDs: []string{"m-1"}, SessionID: "s-n"}); code != http.StatusNoContent {
		t.Fatalf("POST /release: %d", code)
	}
	if code, _ := post("/release", ReleaseRequest{SessionID: "s-n"}); code != http.StatusBadRequest {
		t.Fatalf("POST /release without ids: %d", code)
	}
}

// A client that sends a tool call's name and input, or an event, gets the
// activity worded and the agent's state read by the node, as the shell hooks
// word them.
func TestActivityAndDoingFromTool(t *testing.T) {
	a, b, _ := trio(t)
	info, err := a.CreateChat([]string{"b"}, "")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "b knows the chat", func() bool { _, ok := b.ChatOf(info.ID); return ok })
	dir := t.TempDir()
	b.SetFolders(dir, nil)
	if _, err := b.RegisterSession(SessionRequest{SessionID: "s-n", Provider: ProviderClaude, Folder: dir, Native: true}); err != nil {
		t.Fatal(err)
	}
	q, err := a.SendChat(ChatSend{ChatID: info.ID, Body: "do it", Ask: []string{"b"}})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "b has the request", func() bool { return slices.Contains(chatIDs(b, info.ID), q.ID) })
	text := func() string {
		got, _ := b.Chat(info.ID)
		for _, m := range got.Members {
			for _, j := range m.Jobs {
				if m.Name == "b" && j.ActivityInfo != nil {
					return j.ActivityInfo.Type + ":" + j.ActivityInfo.Text
				}
			}
		}
		return ""
	}
	input, _ := json.Marshal(map[string]string{"file_path": filepath.Join(dir, "pkg", "main.go"), "command": "go test ./..."})
	for _, c := range []struct {
		req  ActivityRequest
		want string
	}{
		{ActivityRequest{Tool: "Read", Input: input}, "read:читает pkg/main.go"},
		{ActivityRequest{Tool: "Bash", Input: input}, "command:запускает go"},
		{ActivityRequest{Event: ActReading}, "read:читает сообщения"},
		{ActivityRequest{Event: ActThinking}, "thinking:думает"},
		{ActivityRequest{Type: "edit", Text: "its own words", Tool: "Read"}, "edit:its own words"},
	} {
		c.req.SessionID, c.req.ReplyTo = "s-n", q.ID
		if _, err := b.SessionActivity(info.ID, c.req); err != nil {
			t.Fatal(err)
		}
		if got := text(); got != c.want {
			t.Errorf("activity of %+v: %q, want %q", c.req, got, c.want)
		}
	}
	if _, err := b.SessionActivity(info.ID, ActivityRequest{SessionID: "s-n", Event: "other"}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("an unknown event: %v", err)
	}

	doing := func() string {
		s, _ := b.SessionLive("s-n")
		return s.Doing
	}
	discuss, _ := json.Marshal(map[string]string{"command": `agentlink discuss --with codex --body-file q.md`})
	for _, c := range []struct {
		req  SessionDoingRequest
		want string
	}{
		{SessionDoingRequest{Tool: "Edit"}, AgentEditing},
		{SessionDoingRequest{Tool: "Bash", Input: input}, AgentCommand},
		{SessionDoingRequest{Tool: "Bash", Input: discuss}, AgentWaiting},
		{SessionDoingRequest{Tool: "mcp__agent-link__discuss"}, AgentWaiting},
		{SessionDoingRequest{Tool: "mcp__plugin_agent-link_agentlink__discuss"}, AgentWaiting},
		{SessionDoingRequest{Tool: "mcp__agent-link__send"}, AgentThinking},
		{SessionDoingRequest{Doing: AgentReading, Tool: "Edit"}, AgentReading},
	} {
		if err := b.SessionDoing("s-n", c.req); err != nil || doing() != c.want {
			t.Errorf("doing of %+v: %q %v, want %q", c.req, doing(), err, c.want)
		}
	}
	if err := b.SessionDoing("s-n", SessionDoingRequest{}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("neither a state nor a tool: %v", err)
	}
}

// GET /native tells the in-process module whether it may deliver: the node's
// native_claude, off by default, unless the node's environment overrides it.
func TestNativeSwitchEndpoint(t *testing.T) {
	t.Setenv(config.EnvNativeClaude, "")
	a, _ := deliveryPair(t, t.TempDir(), nil, nil)
	srv := httptest.NewServer(a.APIHandler())
	t.Cleanup(srv.Close)
	get := func() bool {
		t.Helper()
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/native", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var v config.NativeView
		if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&v) != nil {
			t.Fatalf("GET /native: %d", resp.StatusCode)
		}
		return v.Claude
	}
	if get() {
		t.Fatal("on by default")
	}
	a.cfg.NativeClaude = true
	if !get() {
		t.Fatal("native_claude: true is not served")
	}
	t.Setenv(config.EnvNativeClaude, "0")
	if get() {
		t.Fatal("the environment's 0 does not override the setting")
	}
	a.cfg.NativeClaude = false
	t.Setenv(config.EnvNativeClaude, "1")
	if !get() {
		t.Fatal("the environment's 1 does not override the setting")
	}
}
