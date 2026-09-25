package node

import (
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// openChats lists n's open chats (the main list, without legacy history).
func openChats(n *testNode) []ChatInfo {
	chats, _ := n.Chats(false, false)
	return chats
}

// Two nodes that start the conversation at the same moment end up in one
// chat, with both first messages; so do three nodes opening a group chat.
func TestConcurrentFirstSendOneChat(t *testing.T) {
	a, b, c := trio(t)
	var wg sync.WaitGroup
	var ma, mb Message
	var ea, eb error
	wg.Go(func() { ma, ea = a.SendRequest(SendRequest{To: "b", Body: "from a"}) })
	wg.Go(func() { mb, eb = b.SendRequest(SendRequest{To: "a", Body: "from b"}) })
	wg.Wait()
	if ea != nil || eb != nil {
		t.Fatal(ea, eb)
	}
	want := KeyedChatID("", []string{"a", "b"}, 0)
	if ma.ChatID != want || mb.ChatID != want || !ma.Asks("b") || !mb.Asks("a") {
		t.Fatalf("first sends went to %s and %s, want %s", ma.ChatID, mb.ChatID, want)
	}
	ids := []string{ma.ID, mb.ID}
	slices.Sort(ids)
	for _, n := range []*testNode{a, b} {
		eventually(t, n.cfg.Node+" has one chat with both messages", func() bool {
			chats := openChats(n)
			return len(chats) == 1 && chats[0].ID == want && chats[0].Keyed && slices.Equal(chatIDs(n, want), ids)
		})
	}
	// A group chat opened on three nodes at once.
	var group [3]ChatInfo
	var errs [3]error
	for i, n := range []*testNode{a, b, c} {
		others := slices.DeleteFunc([]string{"a", "b", "c"}, func(s string) bool { return s == n.cfg.Node })
		wg.Go(func() { group[i], errs[i] = n.CreateChat(others, "dev") })
	}
	wg.Wait()
	for i := range group {
		if errs[i] != nil || group[i].ID != KeyedChatID("dev", []string{"a", "b", "c"}, 0) || group[i].Area != "dev" {
			t.Fatalf("group chat %d: %+v, %v", i, group[i], errs[i])
		}
	}
	if len(openChats(c)) != 1 {
		t.Fatalf("c: %+v", openChats(c))
	}
}

// A person's close moves the conversation to its next generation on both
// sides; a message sent into the old one by a side that did not know of the
// close yet is kept there, not lost.
func TestCloseStartsNextGeneration(t *testing.T) {
	lnA, lnB := listen(t), listen(t)
	a := newTestNode(t, "a", testSecret, nil, t.TempDir(), lnA, map[string]net.Listener{"b": lnB})
	b := newTestNode(t, "b", testSecret, nil, t.TempDir(), lnB, map[string]net.Listener{"a": lnA})
	a.start(t)
	gen0 := KeyedChatID("", []string{"a", "b"}, 0)
	gen1 := KeyedChatID("", []string{"a", "b"}, 1)
	m1, err := a.SendRequest(SendRequest{To: "b", Body: "question"})
	if err != nil || m1.ChatID != gen0 {
		t.Fatalf("first send %+v, %v", m1, err)
	}
	if info, err := a.CloseChat(gen0); err != nil || !info.Closed || !info.Archived {
		t.Fatalf("close %+v, %v", info, err)
	}
	// b is offline and does not know of the close: its message goes to gen 0.
	m2, err := b.SendRequest(SendRequest{To: "a", Body: "racing the close"})
	if err != nil || m2.ChatID != gen0 {
		t.Fatalf("b's racing send %+v, %v", m2, err)
	}
	b.start(t)
	eventually(t, "a keeps the racing message in the closed chat", func() bool { return slices.Contains(chatIDs(a, gen0), m2.ID) })
	eventually(t, "b gets the close", func() bool { c, _ := b.ChatOf(gen0); return c.Closed() })
	if page, _ := a.Unread("", "", 10); page.Total != 1 || page.Messages[0].ID != m2.ID {
		t.Fatalf("a's unread after the race: %+v", page)
	}
	// The next message from either side opens generation 1, the same chat.
	m3, err := b.SendRequest(SendRequest{To: "a", Body: "after the close"})
	if err != nil || m3.ChatID != gen1 {
		t.Fatalf("b after the close %+v, %v", m3, err)
	}
	m4, err := a.SendRequest(SendRequest{ChatID: gen0, Body: "continuing the old chat id"})
	if err != nil || m4.ChatID != gen1 {
		t.Fatalf("a to the closed chat %+v, %v", m4, err)
	}
	for _, n := range []*testNode{a, b} {
		eventually(t, n.cfg.Node+" has generation 1 open", func() bool {
			chats := openChats(n)
			return len(chats) == 1 && chats[0].ID == gen1 && chats[0].Gen == 1 && len(chatIDs(n, gen1)) == 2
		})
		if arch, _ := n.Chats(true, false); len(arch) != 1 || arch[0].ID != gen0 {
			t.Fatalf("%s's archive %+v", n.cfg.Node, arch)
		}
	}
	// A restart rebuilds the generation from the chats.
	cs, err := openChatStore(b.cfg.DataDir)
	if err != nil || cs.gen(chatKey("", []string{"a", "b"})) != 1 {
		t.Fatalf("reopened generation: %v", err)
	}
}

// deliveryOf is the delivery state of message id to peer on n.
func deliveryOf(n *testNode, chat, id, peer string) string {
	msgs, _ := n.ChatMessages(chat, 0, 0, 100)
	for _, m := range msgs {
		if m.ID == id {
			for _, d := range m.Delivery {
				if d.Peer == peer {
					return d.State
				}
			}
		}
	}
	return ""
}

// restart stops tn and starts a new node on its data directory, on a new
// peer port (it dials its peers; they accept it inbound).
func restart(t *testing.T, tn *testNode, peers map[string]net.Listener) *testNode {
	t.Helper()
	tn.stop()
	next := newTestNode(t, tn.cfg.Node, testSecret, tn.cfg.Areas, tn.cfg.DataDir, listen(t), peers)
	next.start(t)
	return next
}

// Receipts: queued while the peer is offline, delivered once it stores the
// message, read when a session there acks it (the receipt waits in the
// reader's outbox while the author is offline), answered by a reply. Unread
// survives a restart, and a browser listing the chat reads nothing.
func TestReceiptsAndUnread(t *testing.T) {
	lnA, lnB := listen(t), listen(t)
	a := newTestNode(t, "a", testSecret, nil, t.TempDir(), lnA, map[string]net.Listener{"b": lnB})
	b := newTestNode(t, "b", testSecret, nil, t.TempDir(), lnB, map[string]net.Listener{"a": lnA})
	a.start(t)
	q, err := a.SendRequest(SendRequest{To: "b", Body: "please review", AuthorKind: AuthorAgent})
	if err != nil {
		t.Fatal(err)
	}
	chat := q.ChatID
	if got := deliveryOf(a, chat, q.ID, "b"); got != StateQueued {
		t.Fatalf("offline peer: %q", got)
	}
	b.start(t)
	eventually(t, "delivered", func() bool { return deliveryOf(a, chat, q.ID, "b") == StateDelivered })
	if _, err := b.ChatMessages(chat, 0, 0, 0); err != nil { // what a browser does
		t.Fatal(err)
	}
	if info, _ := b.Chat(chat); info.Unread != 1 {
		t.Fatalf("b's chat unread = %d", info.Unread)
	}
	b = restart(t, b, map[string]net.Listener{"a": a.peerLn})
	page, err := b.Unread("", "", 10)
	if err != nil || page.Total != 1 || page.Messages[0].ID != q.ID || !page.Messages[0].AsksYou || page.Messages[0].AuthorKind != AuthorAgent {
		t.Fatalf("b's unread after a restart: %+v, %v", page, err)
	}
	eventually(t, "b reconnected", func() bool { return a.Connected("b") && b.Connected("a") })
	a.stop() // the author goes offline before the receipt
	res, err := b.Ack(chat, AckRequest{IDs: []string{q.ID, strings.Repeat("0", 32)}, SessionID: "s1"})
	if err != nil || len(res) != 2 || !res[0].Found || !res[0].WasUnread || res[0].Assigned != "session:s1" || res[1].Found {
		t.Fatalf("ack %+v, %v", res, err)
	}
	if again, _ := b.Ack(chat, AckRequest{IDs: []string{q.ID}}); again[0].WasUnread {
		t.Fatal("a second ack read it again")
	}
	if page, _ := b.Unread("", "", 10); page.Total != 0 {
		t.Fatalf("unread after the ack: %+v", page)
	}
	a = restart(t, a, map[string]net.Listener{"b": b.peerLn})
	eventually(t, "read receipt after reconnect", func() bool { return deliveryOf(a, chat, q.ID, "b") == StateRead })
	if _, err := b.SendRequest(SendRequest{To: "a", ReplyTo: q.ID, Body: "looks good"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "answered", func() bool { return deliveryOf(a, chat, q.ID, "b") == StateAnswered })
	if page, _ := a.Unread("", "", 10); page.Total != 1 || page.Messages[0].ReplyTo != q.ID {
		t.Fatalf("a's unread reply: %+v", page)
	}
}

// A message a person writes goes to the others as usual and is also unread
// for this node's own sessions (own_human, never a request); an agent's own
// message is not echoed back. A person's message starts a new chain; agents
// writing after it count hops.
func TestAuthorKindAndOwnHuman(t *testing.T) {
	a, b := pair(t, testSecret, testSecret)
	h, err := a.SendRequest(SendRequest{To: "b", Body: "decide on the schema", AuthorKind: AuthorHuman})
	if err != nil || h.AuthorKind != AuthorHuman || h.AutoDepth != 0 || h.RootID != h.ID {
		t.Fatalf("human message %+v, %v", h, err)
	}
	if _, err := a.SendRequest(SendRequest{ChatID: h.ChatID, Body: "fyi from the agent"}); err != nil {
		t.Fatal(err)
	}
	page, _ := a.Unread("", "", 10)
	if page.Total != 1 || page.Messages[0].ID != h.ID || !page.Messages[0].OwnHuman || page.Messages[0].AsksYou || page.Messages[0].Direction != "out" {
		t.Fatalf("a's own unread: %+v", page)
	}
	eventually(t, "b has both", func() bool { p, _ := b.Unread("", "", 10); return p.Total == 2 })
	bp, _ := b.Unread("", "", 10)
	if m := bp.Messages[0]; m.ID != h.ID || m.AuthorKind != AuthorHuman || m.OwnHuman || !m.AsksYou {
		t.Fatalf("b's view of a's human message: %+v", m)
	}
	if m := bp.Messages[1]; m.AuthorKind != AuthorAgent || m.AsksYou {
		t.Fatalf("b's view of a's agent fyi: %+v", m)
	}
	r, err := b.SendRequest(SendRequest{ChatID: h.ChatID, Body: "agent answer", Ask: []string{"a"}})
	if err != nil || r.RootID != h.ID || r.AutoDepth != 2 { // a's agent fyi was hop 1
		t.Fatalf("agent after a person: %+v, %v", r, err)
	}
	if res, _ := a.Ack("", AckRequest{IDs: []string{h.ID}}); !res[0].WasUnread || res[0].Assigned != "" {
		t.Fatalf("ack of own human message: %+v", res)
	}
	// Past MaxAutoDepth a request is paused for a person.
	deep := Message{ChatID: h.ChatID, Body: "one more round", Responders: []string{"a"}, RootID: h.ID, AutoDepth: MaxAutoDepth + 1}
	if deep, err = b.SendMessage(deep); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a sees it paused", func() bool {
		p, _ := a.Unread("", "", 10)
		return slices.ContainsFunc(p.Messages, func(m UnreadMessage) bool { return m.Body == "one more round" && m.Paused })
	})
	if _, err := a.Ack("", AckRequest{IDs: []string{deep.ID}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "sender sees a read receipt without a hold", func() bool {
		msgs, _ := b.ChatMessages(h.ChatID, 0, 0, 20)
		for _, m := range msgs {
			if m.ID == deep.ID {
				return !m.Held && len(m.Delivery) == 1 && m.Delivery[0].State == StateRead
			}
		}
		return false
	})
}

// Live sessions bind to areas by folder; while one is registered the worker
// does not get the area's requests, and a session's ack and the worker's claim
// never both take one request.
func TestSessionsGateWorker(t *testing.T) {
	a, b := pair(t, testSecret, testSecret)
	work, proj := t.TempDir(), t.TempDir()
	b.SetFolders(work, map[string]string{"dev": proj})
	if _, err := b.RegisterSession(SessionRequest{SessionID: "x", Provider: "claude", Folder: t.TempDir()}); !errors.Is(err, ErrFolderUnbound) {
		t.Fatalf("session outside the folders: %v", err)
	}
	s, err := b.RegisterSession(SessionRequest{SessionID: "s1", Provider: "claude", Folder: filepath.Join(proj, "src"), Wake: WakeRewake})
	if err != nil || s.Area != "dev" || !s.Primary || s.Wake != WakeRewake {
		t.Fatalf("register %+v, %v", s, err)
	}
	s2, err := b.RegisterSession(SessionRequest{SessionID: "s2", Provider: "codex", Folder: proj})
	if err != nil || s2.Primary || s2.Wake != WakeNextEvent {
		t.Fatalf("second session %+v, %v", s2, err)
	}
	if got := b.Sessions(); len(got) != 2 || !got[0].Primary || got[1].Primary {
		t.Fatalf("sessions %+v", got)
	}
	if !b.LiveSession("dev") || b.LiveSession("") {
		t.Fatal("live session areas")
	}
	q, err := a.SendRequest(SendRequest{To: "b", Area: "dev", Body: "dev question"})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "b has it", func() bool { _, ok := b.chats.message(q.ID); return ok })
	if ok, hold, _ := b.ClaimRun(q); ok || hold != "" {
		t.Fatalf("worker claimed a request of a live session's area: %v %q", ok, hold)
	}
	if page, _ := b.Unread(filepath.Join(proj, "src"), "", 10); page.Total != 1 {
		t.Fatalf("unread in the project folder: %+v", page)
	}
	if page, _ := b.Unread(work, "", 10); page.Total != 0 {
		t.Fatalf("unread in the working folder: %+v", page)
	}
	// Session activity reaches the asker as a running job, until idle.
	if _, err := b.SessionActivity(q.ChatID, ActivityRequest{SessionID: "s1", Type: "edit", Text: "Edit schema.sql"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a sees b working", func() bool {
		info, _ := a.Chat(q.ChatID)
		j := info.Members[1].Jobs
		return info.Active && len(j) == 1 && j[0].ReplyTo == q.ID && j[0].ActivityInfo != nil && j[0].ActivityInfo.Text == "Edit schema.sql"
	})
	if _, err := b.SessionActivity(q.ChatID, ActivityRequest{SessionID: "s1", Phase: PhaseIdle}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a sees b idle", func() bool { info, _ := a.Chat(q.ChatID); return !info.Active })
	if _, err := b.SessionActivity(q.ChatID, ActivityRequest{SessionID: "nobody"}); !errors.Is(err, ErrUnknownSession) {
		t.Fatalf("activity of an unknown session: %v", err)
	}
	// Without sessions the worker takes the next request, and then a session's
	// ack learns the worker has it.
	for _, id := range []string{"s1", "s2"} {
		if err := b.EndSession(id); err != nil {
			t.Fatal(err)
		}
	}
	q2, err := a.SendRequest(SendRequest{ChatID: q.ChatID, Body: "another", Ask: []string{"b"}})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "b has q2", func() bool { _, ok := b.chats.message(q2.ID); return ok })
	if ok, _, err := b.ClaimRun(q2); !ok || err != nil {
		t.Fatalf("worker claim: %v %v", ok, err)
	}
	if res, _ := b.Ack(q.ChatID, AckRequest{IDs: []string{q2.ID}, SessionID: "late"}); res[0].Assigned != WorkerOwner || res[0].WasUnread {
		t.Fatalf("ack after the worker: %+v", res)
	}
	eventually(t, "the worker's claim reads it", func() bool { return deliveryOf(a, q.ChatID, q2.ID, "b") == StateRead })
	// The session that acked first keeps q: the worker never takes it.
	if _, err := b.Ack(q.ChatID, AckRequest{IDs: []string{q.ID}, SessionID: "s1"}); err != nil {
		t.Fatal(err)
	}
	if ok, _, _ := b.ClaimRun(q); ok {
		t.Fatal("the worker took a request a session acked")
	}
}

// The control API is for agents: no close, and whatever author_kind a client
// claims, its messages are an agent's.
func TestControlAPIIsForAgents(t *testing.T) {
	a, _ := pair(t, testSecret, testSecret)
	c, err := a.CreateChat([]string{"b"}, "")
	if err != nil {
		t.Fatal(err)
	}
	base := "http://" + a.apiLn.Addr().String()
	resp, err := postJSON(t, base+"/chats/"+c.ID+"/close", "{}")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("the control API closed a chat")
	}
	resp, err = postJSON(t, base+"/send", `{"chat_id":"`+c.ID+`","body":"hi","author_kind":"human"}`)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	msgs, _ := a.ChatMessages(c.ID, 0, 0, 10)
	if last := msgs[len(msgs)-1]; last.Body != "hi" || last.AuthorKind != AuthorAgent {
		t.Fatalf("sent through the API: %+v", last)
	}
}

func postJSON(t *testing.T, url, body string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return http.DefaultClient.Do(req)
}
