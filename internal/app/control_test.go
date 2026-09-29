package app

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/UberMorgott/agent-link/internal/config"
	"github.com/UberMorgott/agent-link/internal/node"
	"github.com/UberMorgott/agent-link/internal/settings"
)

// freeAddr is a loopback address whose port was free a moment ago.
func freeAddr(t *testing.T) string {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// pairApps starts alice and bob, both in the legacy network and in project
// p; bob dials alice. Alice keeps p's folder, bob binds one of his own.
func pairApps(t *testing.T, p settings.ProjectBinding, more ...settings.ProjectBinding) (alice, bob *App) {
	t.Helper()
	const code = "K7Q2-MXPA-4RTB"
	addr := freeAddr(t)
	alice = startApp(t, settings.Settings{Code: code, Listen: addr, Bindings: append([]settings.ProjectBinding{p}, more...)})
	pb := p
	pb.Dir, pb.Peers = t.TempDir(), []string{addr}
	bob = startApp(t, settings.Settings{Node: "bob", Code: code, Peers: []config.Peer{{Addr: addr}}, Bindings: []settings.ProjectBinding{pb}})
	eventuallyLong(t, "alice and bob connected", func() bool {
		alice.mu.Lock()
		defer alice.mu.Unlock()
		return alice.n.Connected("bob") && alice.projects[p.ID].n.Connected("bob")
	})
	return alice, bob
}

func TestNetworkHookUnreadSurvivesLocalDiscussionInSameFolder(t *testing.T) {
	p := newBinding(t, t.TempDir())
	alice, bob := pairApps(t, p)
	alice.Launcher = &seatRunner{}
	srv := httptest.NewServer(alice.Handler())
	defer srv.Close()
	session := node.SessionRequest{SessionID: "claude-interactive", Provider: node.ProviderClaude, Folder: p.Dir}
	if code, body := call(t, srv, http.MethodPost, "/sessions", jsonOf(t, session)); code != http.StatusOK {
		t.Fatalf("register network session: %d %s", code, body)
	}
	if code, body := call(t, srv, http.MethodPost, "/discuss", jsonOf(t, map[string]string{
		"folder": p.Dir, "provider": node.ProviderCodex, "source": node.ProviderClaude,
		"session_id": session.SessionID, "body": "Ask private Codex",
	})); code != http.StatusOK {
		t.Fatalf("discuss: %d %s", code, body)
	}
	if code, body := call(t, srv, http.MethodPost, "/sessions", jsonOf(t, session)); code != http.StatusOK {
		t.Fatalf("network hook heartbeat after discuss: %d %s", code, body)
	}
	chat, err := bob.projects[p.ID].n.NewProjectChat([]string{"alice"})
	if err != nil {
		t.Fatal(err)
	}
	message, err := bob.projects[p.ID].n.SendRequest(node.SendRequest{ChatID: chat.ID, Body: "Network question",
		Ask: []string{"alice"}, AuthorKind: node.AuthorHuman})
	if err != nil {
		t.Fatal(err)
	}
	arrived := false
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		page, _ := alice.projects[p.ID].n.UnreadFor("", session.SessionID, "", 10)
		if slices.ContainsFunc(page.Messages, func(m node.UnreadMessage) bool { return m.ID == message.ID }) {
			arrived = true
			break
		}
	}
	if !arrived {
		page, _ := alice.projects[p.ID].n.UnreadFor("", session.SessionID, "", 10)
		msgs, _ := alice.projects[p.ID].n.ChatMessages(chat.ID, 0, 0, 10)
		t.Fatalf("network question not in session unread: page=%+v chat=%+v sessions=%+v", page, msgs, alice.projects[p.ID].n.Sessions())
	}
	path := "/unread?folder=" + url.QueryEscape(p.Dir) + "&session=" + session.SessionID + "&agent=1"
	code, body := call(t, srv, http.MethodGet, path, "")
	if code != http.StatusOK || !strings.Contains(body, message.ID) {
		t.Fatalf("network unread routed to local chat: %d %s", code, body)
	}
	for _, b := range alice.Settings().Bindings {
		if b.ScopeOf() == settings.ProjectScopeLocal && len(alice.projects[b.ID].n.Sessions()) != 0 {
			t.Fatal("external network hook session was registered in the local project")
		}
	}
}

// One folder bound to a network project and to its private local chat, two
// interactive sessions registered in the network project, both asking the
// same local seat: each gets its own seat reply (never the other's), both
// hear the network message but only one takes it, and every message is
// claimed and acknowledged once, in its own project.
func TestOriginRoutingAcrossNetworkAndLocalProject(t *testing.T) {
	p := newBinding(t, t.TempDir())
	alice, bob := pairApps(t, p)
	alice.Launcher = &seatRunner{}
	srv := httptest.NewServer(alice.Handler())
	defer srv.Close()
	sids := []string{"claude-one", "claude-two"}
	var asks []discussResultAPI
	for _, sid := range sids {
		session := node.SessionRequest{SessionID: sid, Provider: node.ProviderClaude, Folder: p.Dir}
		if code, body := call(t, srv, http.MethodPost, "/sessions", jsonOf(t, session)); code != http.StatusOK {
			t.Fatalf("register %s: %d %s", sid, code, body)
		}
		// shared: both ask in the folder's project chat (by default each asks in its own).
		code, body := call(t, srv, http.MethodPost, "/discuss", jsonOf(t, map[string]any{
			"folder": p.Dir, "provider": node.ProviderCodex, "source": node.ProviderClaude, "session_id": sid, "body": "question of " + sid,
			"shared": true,
		}))
		var ask discussResultAPI
		if code != http.StatusOK || json.Unmarshal([]byte(body), &ask) != nil {
			t.Fatalf("discuss %s: %d %s", sid, code, body)
		}
		asks = append(asks, ask)
	}
	local := alice.projects[asks[0].Project].n
	if asks[0].Project == p.ID || asks[1].Chat != asks[0].Chat || asks[1].Seat != asks[0].Seat {
		t.Fatalf("both asks not in one local chat and seat: %+v", asks)
	}
	eventuallyApp(t, "seat setup", func() bool {
		for _, s := range local.Seats() {
			if s.SessionID == "" || s.LastTurn.IsZero() || s.Status == node.SeatRunning {
				return false
			}
		}
		return true
	})
	var replies []node.Message
	for _, ask := range asks {
		m, err := local.SendRequest(node.SendRequest{ChatID: ask.Chat, ReplyTo: ask.ID, Body: "answer to " + ask.ID,
			Seat: ask.Seat, AuthorKind: node.AuthorAgent})
		if err != nil {
			t.Fatal(err)
		}
		replies = append(replies, m)
	}
	chat, err := bob.projects[p.ID].n.NewProjectChat([]string{"alice"})
	if err != nil {
		t.Fatal(err)
	}
	network, err := bob.projects[p.ID].n.SendRequest(node.SendRequest{ChatID: chat.ID, Body: "Network question",
		Ask: []string{"alice"}, AuthorKind: node.AuthorHuman})
	if err != nil {
		t.Fatal(err)
	}
	unread := func(sid string) map[string]string {
		t.Helper()
		code, body := call(t, srv, http.MethodGet, "/unread?"+url.Values{"folder": {p.Dir}, "session": {sid}}.Encode(), "")
		var page node.UnreadPage
		if code != http.StatusOK || json.Unmarshal([]byte(body), &page) != nil {
			t.Fatalf("unread %s: %d %s", sid, code, body)
		}
		out := map[string]string{}
		for _, m := range page.Messages {
			out[m.ID] = m.Project
		}
		return out
	}
	eventuallyLong(t, "network question at alice", func() bool { _, ok := unread(sids[0])[network.ID]; return ok })
	for i, sid := range sids {
		got := unread(sid)
		if got[replies[i].ID] != asks[i].Project || got[network.ID] != p.ID {
			t.Fatalf("%s unread: %v", sid, got)
		}
		if _, ok := got[replies[1-i].ID]; ok {
			t.Fatalf("%s sees the other session's reply: %v", sid, got)
		}
	}
	claim := func(project, sid string, ids ...string) []string {
		t.Helper()
		path := "/claim"
		if project != "" {
			path += "?project=" + project
		}
		code, body := call(t, srv, http.MethodPost, path, jsonOf(t, node.ClaimRequest{IDs: ids, SessionID: sid, Folder: p.Dir}))
		var granted []string
		if code != http.StatusOK || json.Unmarshal([]byte(body), &granted) != nil {
			t.Fatalf("claim %v by %s: %d %s", ids, sid, code, body)
		}
		return granted
	}
	if g := claim(asks[0].Project, sids[1], replies[0].ID); len(g) != 0 {
		t.Fatalf("the other session claimed a reply: %v", g)
	}
	if g := claim(asks[0].Project, sids[0], replies[0].ID); !slices.Equal(g, []string{replies[0].ID}) {
		t.Fatalf("own reply claim: %v", g)
	}
	// An older hook claims without a project: each id in its own context.
	if g := claim("", sids[1], replies[1].ID, network.ID); !slices.Equal(slices.Sorted(slices.Values(g)), slices.Sorted(slices.Values([]string{replies[1].ID, network.ID}))) {
		t.Fatalf("claim across contexts: %v", g)
	}
	if g := claim(p.ID, sids[0], network.ID); len(g) != 0 {
		t.Fatalf("network message granted twice: %v", g)
	}
	ack := func(project, sid string, ids ...string) []node.AckResult {
		t.Helper()
		path := "/ack"
		if project != "" {
			path += "?project=" + project
		}
		code, body := call(t, srv, http.MethodPost, path, jsonOf(t, node.AckRequest{IDs: ids, SessionID: sid}))
		var res []node.AckResult
		if code != http.StatusOK || json.Unmarshal([]byte(body), &res) != nil {
			t.Fatalf("ack %v: %d %s", ids, code, body)
		}
		return res
	}
	if res := ack(asks[0].Project, sids[0], replies[0].ID); len(res) != 1 || !res[0].Found || !res[0].WasUnread {
		t.Fatalf("ack in the local project: %+v", res)
	}
	if res := ack("", sids[1], replies[1].ID, network.ID); len(res) != 2 || !res[0].WasUnread || !res[1].WasUnread {
		t.Fatalf("id-only ack across contexts: %+v", res)
	}
	if res := ack(asks[0].Project, sids[0], replies[0].ID); len(res) != 1 || res[0].WasUnread {
		t.Fatalf("acknowledged twice: %+v", res)
	}
	for _, sid := range sids {
		if got := unread(sid); len(got) != 0 {
			t.Fatalf("%s still has unread: %v", sid, got)
		}
	}
	// The asking session reports its work on the reply in the local chat,
	// though registered in the network project; a stranger may not.
	activity := func(sid string) int {
		code, _ := call(t, srv, http.MethodPost, "/chats/"+asks[0].Chat+"/activity",
			jsonOf(t, node.ActivityRequest{SessionID: sid, ReplyTo: replies[0].ID, Type: "read", Text: "читает"}))
		return code
	}
	if code := activity(sids[0]); code != http.StatusOK {
		t.Fatalf("origin session activity in the local chat: %d", code)
	}
	if code := activity("claude-stranger"); code != http.StatusNotFound {
		t.Fatalf("stranger activity in the local chat: %d", code)
	}
	for _, b := range alice.Settings().Bindings {
		if b.ScopeOf() == settings.ProjectScopeLocal && len(alice.projects[b.ID].n.Sessions()) != 0 {
			t.Fatal("external network hook session was registered in the local project")
		}
	}
}

// A direct discuss wait hands the seat's reply over without reading it: only
// the caller that decoded it acknowledges it (discussMessage), so a reply a
// timed-out or failed caller never got stays unread for the session's hooks.
func TestDiscussReplyWaitKeepsReplyUnread(t *testing.T) {
	h, n, ask, reply := discussReplyFixture(t)
	path := "/discuss/reply?" + url.Values{"project": {ask.Project}, "chat": {ask.Chat}, "id": {ask.ID}, "seat": {ask.Seat},
		"timeout": {"2s"}, "client_ack": {"1"}}.Encode()
	if code, body := h.do(t, http.MethodGet, path, "", nil); code != http.StatusOK || !strings.Contains(body, reply.ID) {
		t.Fatalf("reply wait: %d %s", code, body)
	}
	if p, _ := n.UnreadFor("", "claude-ext", "", 10); p.Total != 1 {
		t.Fatalf("reply read before the caller got it: %+v", p)
	}
	// The caller's ack (its project, as discussMessage sends it) reads it.
	if code, body := h.do(t, http.MethodPost, "/ack?project="+ask.Project,
		jsonOf(t, node.AckRequest{IDs: []string{reply.ID}, SessionID: "claude-ext"}), nil); code != http.StatusOK || !strings.Contains(body, `"was_unread":true`) {
		t.Fatalf("caller ack: %d %s", code, body)
	}
	if p, _ := n.UnreadFor("", "claude-ext", "", 10); p.Total != 0 {
		t.Fatalf("reply unread after the caller's ack: %+v", p)
	}
}

// A caller without client_ack (a client before v0.6.30, whose MCP server
// outlives an update) never acknowledges the reply itself: the wait reads it,
// so the session's hooks do not deliver it a second time.
func TestDiscussReplyWaitReadsReplyForLegacyCaller(t *testing.T) {
	h, n, ask, reply := discussReplyFixture(t)
	path := "/discuss/reply?" + url.Values{"project": {ask.Project}, "chat": {ask.Chat}, "id": {ask.ID}, "seat": {ask.Seat}, "timeout": {"2s"}}.Encode()
	if code, body := h.do(t, http.MethodGet, path, "", nil); code != http.StatusOK || !strings.Contains(body, reply.ID) {
		t.Fatalf("reply wait: %d %s", code, body)
	}
	if p, _ := n.UnreadFor("", "claude-ext", "", 10); p.Total != 0 {
		t.Fatalf("legacy caller's reply delivered again: %+v", p)
	}
}

// discussReplyFixture asks the codex seat from an external claude session and
// posts the seat's reply, unread for that session.
func discussReplyFixture(t *testing.T) (*harness, *node.Node, discussResultAPI, node.Message) {
	t.Helper()
	h := projectsHarness(t, "alice", "", func(a *App) { a.Launcher = &seatRunner{} })
	dir := t.TempDir()
	code, raw := h.do(t, http.MethodPost, "/discuss", jsonOf(t, map[string]string{
		"folder": dir, "provider": node.ProviderCodex, "source": node.ProviderClaude, "session_id": "claude-ext", "body": "question",
	}), nil)
	var ask discussResultAPI
	if code != http.StatusOK || json.Unmarshal([]byte(raw), &ask) != nil {
		t.Fatalf("discuss: %d %s", code, raw)
	}
	n := h.app.projects[ask.Project].n
	reply, err := n.SendRequest(node.SendRequest{ChatID: ask.Chat, ReplyTo: ask.ID, Body: "42", Seat: ask.Seat, AuthorKind: node.AuthorAgent})
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := n.UnreadFor("", "claude-ext", "", 10); p.Total != 1 || p.Messages[0].ID != reply.ID {
		t.Fatalf("reply not unread for the asking session: %+v", p)
	}
	return h, n, ask, reply
}

// call sends one control API request to srv.
func call(t *testing.T, srv *httptest.Server, method, path, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data)
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// The control API routes by project, chat, message and folder (§6.1).
func TestControlAPIRoutes(t *testing.T) {
	p, noDir := newBinding(t, t.TempDir()), newBinding(t, "")
	alice, bob := pairApps(t, p, noDir)
	srv := httptest.NewServer(alice.Handler())
	defer srv.Close()
	alice.mu.Lock()
	legacyN, projN := alice.n, alice.projects[p.ID].n
	alice.mu.Unlock()
	bob.mu.Lock()
	bobProj := bob.projects[p.ID].n
	bob.mu.Unlock()

	legacyChat, err := legacyN.CreateChat([]string{"bob"}, "")
	if err != nil {
		t.Fatal(err)
	}
	projChat, err := projN.NewProjectChat([]string{"bob"})
	if err != nil {
		t.Fatal(err)
	}
	unknown := strings.Repeat("ab", 16)

	// Chat lists: every context without a selector, each naming its project.
	code, body := call(t, srv, http.MethodGet, "/chats", "")
	var all []ChatInfoView
	if err := json.Unmarshal([]byte(body), &all); code != http.StatusOK || err != nil {
		t.Fatalf("GET /chats: %d %s", code, body)
	}
	projects := map[string]string{}
	for _, c := range all {
		projects[c.ID] = c.Project
	}
	if projects[legacyChat.ID] != LegacyProjectID || projects[projChat.ID] != p.ID {
		t.Fatalf("chat projects %v", projects)
	}
	if code, body := call(t, srv, http.MethodGet, "/projects", ""); code != http.StatusOK ||
		!strings.Contains(body, `"id":"`+p.ID+`"`) || !strings.Contains(body, `"id":"`+LegacyProjectID+`"`) {
		t.Fatalf("GET /projects: %d %s", code, body)
	}
	if code, body := call(t, srv, http.MethodGet, "/chats?project="+p.ID, ""); code != http.StatusOK ||
		!strings.Contains(body, projChat.ID) || strings.Contains(body, legacyChat.ID) {
		t.Fatalf("GET /chats?project: %d %s", code, body)
	}
	for path, want := range map[string]int{
		"/chats?project=" + unknown:                          http.StatusNotFound,
		"/chats/" + projChat.ID:                              http.StatusOK,
		"/chats/" + projChat.ID + "/messages":                http.StatusOK,
		"/chats/" + projChat.ID + "?project=legacy":          http.StatusConflict,
		"/chats/" + legacyChat.ID + "?project=" + p.ID:       http.StatusConflict,
		"/chats/" + unknown:                                  http.StatusNotFound,
		"/members?project=" + p.ID:                           http.StatusOK,
		"/inbox":                                             http.StatusOK,
		"/wait?timeout=10ms&chat=" + projChat.ID:             http.StatusNoContent,
		"/wait?timeout=10ms&project=" + noDir.ID:             http.StatusConflict,
		"/unread?project=" + noDir.ID:                        http.StatusConflict,
		"/unread?project=" + p.ID + "&folder=" + t.TempDir(): http.StatusConflict,
		"/unread?folder=" + p.Dir:                            http.StatusOK,
	} {
		if code, body := call(t, srv, http.MethodGet, path, ""); code != want {
			t.Errorf("GET %s: %d %s, want %d", path, code, body, want)
		}
	}

	// Send: chat owner, explicit project, folder, legacy.
	send := func(req map[string]any) (int, node.Message) {
		t.Helper()
		code, body := call(t, srv, http.MethodPost, "/send", jsonOf(t, req))
		var m node.Message
		if code == http.StatusOK {
			if err := json.Unmarshal([]byte(body), &m); err != nil {
				t.Fatal(err)
			}
		}
		return code, m
	}
	if code, m := send(map[string]any{"chat_id": projChat.ID, "body": "hi"}); code != http.StatusOK || m.ChatID != projChat.ID {
		t.Fatalf("send into the project chat: %d %+v", code, m)
	}
	if code, _ := send(map[string]any{"chat_id": projChat.ID, "body": "hi", "project": LegacyProjectID}); code != http.StatusConflict {
		t.Fatalf("send with a foreign project: %d", code)
	}
	if code, _ := send(map[string]any{"to": "bob", "body": "hi", "project": unknown}); code != http.StatusNotFound {
		t.Fatalf("send to an unknown project: %d", code)
	}
	if code, m := send(map[string]any{"to": "bob", "body": "hi", "folder": p.Dir}); code != http.StatusOK || !projN.OwnsChat(m.ChatID) {
		t.Fatalf("send from the project folder: %d %+v", code, m)
	}
	if code, m := send(map[string]any{"to": "bob", "body": "hi", "folder": t.TempDir()}); code != http.StatusOK || !legacyN.OwnsChat(m.ChatID) {
		t.Fatalf("send from another folder: %d %+v", code, m)
	}
	// A reply goes to the context of the message it answers.
	eventuallyLong(t, "bob has the project chat", func() bool { return bobProj.OwnsChat(projChat.ID) })
	fromBob, err := bobProj.SendRequest(node.SendRequest{ChatID: projChat.ID, Body: "from bob"})
	if err != nil {
		t.Fatal(err)
	}
	eventuallyLong(t, "alice got bob's message", func() bool { return projN.OwnsMessage(fromBob.ID) })
	if code, m := send(map[string]any{"reply_to": fromBob.ID, "body": "thanks"}); code != http.StatusOK || m.ChatID != projChat.ID {
		t.Fatalf("reply: %d %+v", code, m)
	}
	// Ack without a chat: each id to its owner; an unknown one is not found.
	code, body = call(t, srv, http.MethodPost, "/ack", jsonOf(t, node.AckRequest{IDs: []string{fromBob.ID, unknown}}))
	var acks []node.AckResult
	if err := json.Unmarshal([]byte(body), &acks); code != http.StatusOK || err != nil || len(acks) != 2 || !acks[0].Found || acks[1].Found {
		t.Fatalf("ack: %d %s", code, body)
	}

	// Sessions: by folder, remembered for the end, listed with their project.
	sess := func(req map[string]any) int {
		t.Helper()
		code, _ := call(t, srv, http.MethodPost, "/sessions", jsonOf(t, req))
		return code
	}
	if code := sess(map[string]any{"session_id": "s1", "provider": "claude", "folder": p.Dir}); code != http.StatusOK {
		t.Fatalf("register in the project folder: %d", code)
	}
	if len(projN.Sessions()) != 1 || len(legacyN.Sessions()) != 0 {
		t.Fatal("session registered in the wrong context")
	}
	if code := sess(map[string]any{"session_id": "s2", "provider": "claude", "folder": p.Dir, "project": noDir.ID}); code != http.StatusConflict {
		t.Fatalf("register in a project without a folder: %d", code)
	}
	if code := sess(map[string]any{"session_id": "s3", "provider": "claude", "folder": t.TempDir(), "project": p.ID}); code != http.StatusConflict {
		t.Fatalf("register outside the project folder: %d", code)
	}
	code, body = call(t, srv, http.MethodGet, "/sessions", "")
	if code != http.StatusOK || !strings.Contains(body, `"project":"`+p.ID+`"`) {
		t.Fatalf("GET /sessions: %d %s", code, body)
	}
	// Claims go to the context of the session's folder, like its unread.
	claim := func(folder string) (int, string) {
		return call(t, srv, http.MethodPost, "/claim", jsonOf(t, node.ClaimRequest{IDs: []string{fromBob.ID}, SessionID: "s1", Folder: folder}))
	}
	if code, body := claim(p.Dir); code != http.StatusOK || strings.TrimSpace(body) != "[]" { // acked above: nothing to claim
		t.Fatalf("claim in the project folder: %d %s", code, body)
	}
	if code, body := claim(""); code != http.StatusOK {
		t.Fatalf("claim without a folder: %d %s", code, body)
	}
	// Reassign goes to the context of the message: a read one is refused, an
	// unread one no session waits for moves only by force (it needs no person).
	reassign := func(id string, force bool) (int, string) {
		return call(t, srv, http.MethodPost, "/reassign", jsonOf(t, node.ReassignRequest{ID: id, SessionID: "s1", Force: force}))
	}
	if code, body := reassign(fromBob.ID, true); code != http.StatusBadRequest || !strings.Contains(body, "not unread") {
		t.Fatalf("reassign a read message: %d %s", code, body)
	}
	again, err := bobProj.SendRequest(node.SendRequest{ChatID: projChat.ID, Body: "from bob again"})
	if err != nil {
		t.Fatal(err)
	}
	eventuallyLong(t, "alice got bob's second message", func() bool { return projN.OwnsMessage(again.ID) })
	if code, body := reassign(again.ID, false); code != http.StatusConflict {
		t.Fatalf("reassign without force: %d %s", code, body)
	}
	if code, body := reassign(again.ID, true); code != http.StatusOK || !strings.Contains(body, `"assigned":"session:s1"`) {
		t.Fatalf("reassign an unread message: %d %s", code, body)
	}
	if code, _ := call(t, srv, http.MethodDelete, "/sessions/s1", ""); code != http.StatusNoContent {
		t.Fatalf("end session: %d", code)
	}
	if code, _ := call(t, srv, http.MethodDelete, "/sessions/s1", ""); code != http.StatusNotFound {
		t.Fatalf("end an unknown session: %d", code)
	}
	if code, _ := call(t, srv, http.MethodGet, "/chats", ""); code != http.StatusOK {
		t.Fatal("browsers are refused, the CLI is not")
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/chats", nil)
	req.Header.Set("Origin", srv.URL)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("browser request: %v %v", resp, err)
	} else {
		_ = resp.Body.Close()
	}
}

// Without the legacy network a folder outside every project reaches no
// context.
func TestControlAPINoContext(t *testing.T) {
	p := newBinding(t, t.TempDir())
	a := startApp(t, settings.Settings{Bindings: []settings.ProjectBinding{p}})
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	if code, body := call(t, srv, http.MethodPost, "/send", `{"to":"bob","body":"hi","folder":`+jsonOf(t, t.TempDir())+`}`); code != http.StatusBadRequest ||
		!strings.Contains(body, "folder is not in a project") {
		t.Fatalf("send outside every project: %d %s", code, body)
	}
	if code, body := call(t, srv, http.MethodGet, "/unread?folder="+p.Dir, ""); code != http.StatusOK {
		t.Fatalf("unread in the project folder: %d %s", code, body)
	}
	// The caller's folder picks the project; without any folder the only
	// project answers; a folder outside it is refused naming the project.
	sub := filepath.Join(p.Dir, "sub")
	for path, want := range map[string]int{
		"/members?cwd=" + url.QueryEscape(sub):         http.StatusOK,
		"/inbox?cwd=" + url.QueryEscape(p.Dir):         http.StatusOK,
		"/unread?cwd=" + url.QueryEscape(p.Dir):        http.StatusOK,
		"/members":                                     http.StatusOK,
		"/members?cwd=" + url.QueryEscape(t.TempDir()): http.StatusBadRequest,
		"/unread?cwd=" + url.QueryEscape(t.TempDir()):  http.StatusBadRequest,
	} {
		code, body := call(t, srv, http.MethodGet, path, "")
		if code != want || (code == http.StatusBadRequest && !strings.Contains(body, "pass --project <id>; known: "+p.ID)) {
			t.Errorf("GET %s: %d %s, want %d", path, code, body, want)
		}
	}
}

// Without the legacy network and with several projects a request that names
// none is refused with the known projects.
func TestControlAPINoContextSeveral(t *testing.T) {
	p, q := newBinding(t, t.TempDir()), newBinding(t, t.TempDir())
	a := startApp(t, settings.Settings{Bindings: []settings.ProjectBinding{p, q}})
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	if code, body := call(t, srv, http.MethodGet, "/members", ""); code != http.StatusBadRequest ||
		!strings.Contains(body, p.ID) || !strings.Contains(body, q.ID) {
		t.Fatalf("members naming no project: %d %s", code, body)
	}
	if code, body := call(t, srv, http.MethodGet, "/members?cwd="+url.QueryEscape(q.Dir), ""); code != http.StatusOK {
		t.Fatalf("members in q's folder: %d %s", code, body)
	}
}

func eventuallyLong(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out: %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
