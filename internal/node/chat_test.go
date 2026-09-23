package node

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"slices"
	"strconv"
	"testing"
	"time"
)

// trio starts three nodes that all know each other.
func trio(t *testing.T) (a, b, c *testNode) {
	t.Helper()
	lnA, lnB, lnC := listen(t), listen(t), listen(t)
	a = newTestNode(t, "a", testSecret, nil, t.TempDir(), lnA, map[string]net.Listener{"b": lnB, "c": lnC})
	b = newTestNode(t, "b", testSecret, nil, t.TempDir(), lnB, map[string]net.Listener{"a": lnA, "c": lnC})
	c = newTestNode(t, "c", testSecret, nil, t.TempDir(), lnC, map[string]net.Listener{"a": lnA, "b": lnB})
	for _, n := range []*testNode{a, b, c} {
		n.start(t)
	}
	eventually(t, "all connected", func() bool {
		return a.Connected("b") && a.Connected("c") && b.Connected("c") && b.Connected("a") && c.Connected("a") && c.Connected("b")
	})
	return a, b, c
}

// chatIDs lists the ids of a chat's messages (kind "") on n, sorted.
func chatIDs(n *testNode, chat string) []string {
	msgs, err := n.ChatMessages(chat, 0, 0, 1000)
	if err != nil {
		return nil
	}
	var ids []string
	for _, m := range msgs {
		if m.Kind == "" {
			ids = append(ids, m.ID)
		}
	}
	slices.Sort(ids)
	return ids
}

func waitChat(t *testing.T, tn *testNode, chat, timeout string) (int, []Message) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		"http://"+tn.apiLn.Addr().String()+"/wait?timeout="+timeout+"&chat="+chat, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var msgs []Message
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&msgs); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode, msgs
}

func TestGroupChatThreeNodes(t *testing.T) {
	a, b, c := trio(t)
	info, err := a.CreateChat([]string{"c,b"}, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(info.Participants, []string{"a", "b", "c"}) || info.Closed || info.Archived {
		t.Fatalf("created %+v", info)
	}
	for _, n := range []*testNode{b, c} {
		eventually(t, n.cfg.Node+" knows the chat", func() bool {
			got, ok := n.ChatOf(info.ID)
			return ok && got.Area == "dev" && slices.Equal(got.Participants, info.Participants)
		})
	}
	// A legacy message to b stays for b's plain wait; wait?chat takes only chat messages.
	legacy, err := a.Send("b", "outside the chat", "")
	if err != nil {
		t.Fatal(err)
	}
	q, err := a.SendChat(ChatSend{ChatID: info.ID, Body: "question for b", Ask: []string{"b"}})
	if err != nil {
		t.Fatal(err)
	}
	if q.RootID != q.ID || q.AutoDepth != 0 || !slices.Equal(q.Responders, []string{"b"}) {
		t.Fatalf("question %+v", q)
	}
	eventually(t, "b has the legacy message", func() bool {
		entries, _ := b.Recent(0)
		return slices.ContainsFunc(entries, func(e Entry) bool { return e.ID == legacy.ID })
	})
	code, msgs := waitChat(t, b, info.ID, "10s")
	if code != http.StatusOK || len(msgs) != 1 || msgs[0].ID != q.ID || !msgs[0].Asks("b") || msgs[0].Asks("c") {
		t.Fatalf("b wait?chat = %d %+v", code, msgs)
	}
	code, msgs = waitHTTP(t, b, "1s")
	if code != http.StatusOK || len(msgs) != 1 || msgs[0].ID != legacy.ID {
		t.Fatalf("b plain wait after chat wait = %d %+v", code, msgs)
	}
	// Both b and c answer the same request: their reply ids differ.
	var replies []string
	for _, n := range []*testNode{b, c} {
		r, err := n.SendMessage(Message{ID: DerivedID(q.ID, n.cfg.Node+"/reply"), ChatID: info.ID, ReplyTo: q.ID,
			Body: "answer from " + n.cfg.Node, JobStatus: JobCompleted})
		if err != nil {
			t.Fatal(err)
		}
		replies = append(replies, r.ID)
	}
	if replies[0] == replies[1] {
		t.Fatal("replies of two participants share an id")
	}
	want := []string{q.ID, replies[0], replies[1]}
	slices.Sort(want)
	for _, n := range []*testNode{a, b, c} {
		eventually(t, n.cfg.Node+" has the whole history", func() bool { return slices.Equal(chatIDs(n, info.ID), want) })
	}
	// A retry with the same id keeps the stored time and adds nothing.
	again, err := a.SendMessage(Message{ID: q.ID, ChatID: info.ID, Body: "question for b", Responders: []string{"b"}})
	if err != nil || !again.CreatedAt.Equal(q.CreatedAt) {
		t.Fatalf("retry = %+v, %v", again, err)
	}
	msgs2, _ := a.ChatMessages(info.ID, 0, 0, 0)
	for _, m := range msgs2 {
		if m.ID == q.ID && (m.Direction != "out" || len(m.Delivery) != 2) {
			t.Fatalf("own message listed as %+v", m)
		}
	}
	eventually(t, "a sees both deliveries sent", func() bool {
		got, _ := a.Chat(info.ID)
		return got.LastMessage != nil && got.Count == 3 && got.Members[1].Queued == 0 && got.Members[2].Queued == 0
	})
	// Paging: after returns what follows a seq, before what precedes it.
	all, _ := c.ChatMessages(info.ID, 0, 0, 0)
	if later, _ := c.ChatMessages(info.ID, 0, all[0].Seq, 0); len(later) != len(all)-1 {
		t.Fatalf("after %d: %d of %d", all[0].Seq, len(later), len(all))
	}
	if last, _ := c.ChatMessages(info.ID, 0, 0, 1); len(last) != 1 || last[0].Seq != all[len(all)-1].Seq {
		t.Fatalf("limit 1 = %+v", last)
	}
	if first, _ := c.ChatMessages(info.ID, all[1].Seq, 0, 0); len(first) != 1 || first[0].Seq != all[0].Seq {
		t.Fatalf("before = %+v", first)
	}
}

func TestChatStatusAndActivity(t *testing.T) {
	a, b, _ := trio(t)
	info, err := a.CreateChat([]string{"b"}, "")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "b knows the chat", func() bool { _, ok := b.ChatOf(info.ID); return ok })
	q, err := a.SendChat(ChatSend{ChatID: info.ID, Body: "do it", Ask: []string{"b"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, seq := range []uint64{2, 1} { // the older update arrives last
		_, err := b.SendMessage(Message{ID: DerivedID(q.ID, "b/running-"+strconv.FormatUint(seq, 10)), ChatID: info.ID, ReplyTo: q.ID,
			Kind: KindStatus, JobStatus: JobRunning, Activity: "step",
			ActivityInfo: &ActivityState{ID: "t1", Type: "command", Text: "go test", Phase: PhaseRunning, Seq: seq}})
		if err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := b.Chat(info.ID); !got.Active || !got.Members[1].Self || len(got.Members[1].Jobs) != 1 {
		t.Fatalf("b's own view of its job: %+v", got)
	}
	eventually(t, "a shows b running", func() bool {
		got, _ := a.Chat(info.ID)
		jobs := got.Members[1].Jobs
		return got.Active && len(jobs) == 1 && jobs[0].ActivityInfo != nil && jobs[0].ActivityInfo.Seq == 2
	})
	if _, err := b.SendMessage(Message{ID: DerivedID(q.ID, "b/reply"), ChatID: info.ID, ReplyTo: q.ID, Body: "done", JobStatus: JobCompleted}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a shows b idle", func() bool { got, _ := a.Chat(info.ID); return !got.Active })
	// A late running update does not bring the finished job back.
	if _, err := b.SendMessage(Message{ID: DerivedID(q.ID, "b/late"), ChatID: info.ID, ReplyTo: q.ID, Kind: KindStatus, JobStatus: JobRunning,
		ActivityInfo: &ActivityState{Seq: 9}}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if got, _ := a.Chat(info.ID); got.Active {
		t.Fatal("late running update revived a finished job")
	}
	for _, m := range chatIDs(a, info.ID) {
		if m != q.ID && m != DerivedID(q.ID, "b/reply") {
			t.Fatalf("status update stored as a chat message: %s", m)
		}
	}
}

func TestChatCloseRace(t *testing.T) {
	a, b, c := trio(t)
	info, err := a.CreateChat([]string{"b", "c"}, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []*testNode{b, c} {
		eventually(t, n.cfg.Node+" knows the chat", func() bool { _, ok := n.ChatOf(info.ID); return ok })
	}
	q, err := a.SendChat(ChatSend{ChatID: info.ID, Body: "long job", Ask: []string{"c"}})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "c has the question", func() bool { return slices.Contains(chatIDs(c, info.ID), q.ID) })
	// b and c close at the same time: everyone keeps the same winner.
	done := make(chan error, 2)
	for _, n := range []*testNode{b, c} {
		go func() { _, err := n.CloseChat(info.ID); done <- err }()
	}
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	winner := min(DerivedID(info.ID, "close/b"), DerivedID(info.ID, "close/c"))
	for _, n := range []*testNode{a, b, c} {
		eventually(t, n.cfg.Node+" agrees on the close", func() bool {
			got, _ := n.ChatOf(info.ID)
			return got.CloseID == winner
		})
		got, _ := n.Chat(info.ID)
		if !got.Closed || !got.Archived || got.ClosedBy == "" {
			t.Fatalf("%s sees %+v", n.cfg.Node, got)
		}
	}
	if _, err := a.SendMessage(Message{ChatID: info.ID, Body: "more", Responders: []string{"b"}}); !errors.Is(err, ErrChatClosed) {
		t.Fatalf("message into a closed chat: %v", err)
	}
	// Writing to the conversation goes on in its next generation.
	if m, err := a.SendChat(ChatSend{ChatID: info.ID, Body: "more", Ask: []string{"b"}}); err != nil || m.ChatID != KeyedChatID("", info.Participants, 1) {
		t.Fatalf("send to a closed chat = %+v, %v", m, err)
	}
	if ok, hold, _ := c.ClaimRun(Message{ID: q.ID, ChatID: info.ID, Responders: []string{"c"}, RootID: q.ID}); ok || hold != HoldChatClosed {
		t.Fatal("a closed chat still starts a job")
	}
	// The job that was already running still delivers its answer.
	reply, err := c.SendMessage(Message{ID: DerivedID(q.ID, "c/reply"), ChatID: info.ID, ReplyTo: q.ID, Body: "finished", JobStatus: JobCompleted})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "a has the late answer", func() bool { return slices.Contains(chatIDs(a, info.ID), reply.ID) })
	if got, _ := a.ChatOf(info.ID); got.CloseID != winner {
		t.Fatal("a late answer changed the close")
	}
}

// A chat is archived exactly when a participant closes it, on every node; a
// new message does not bring an open chat anywhere else. History stays.
func TestChatArchiveIsClose(t *testing.T) {
	a, b := pair(t, testSecret, testSecret)
	eventually(t, "connected", func() bool { return a.Connected("b") && b.Connected("a") })
	info, err := a.CreateChat([]string{"b"}, "")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "b knows the chat", func() bool { _, ok := b.ChatOf(info.ID); return ok })
	if _, err := b.SendChat(ChatSend{ChatID: info.ID, Body: "new message"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a has the message", func() bool { return len(chatIDs(a, info.ID)) == 1 })
	if main, _ := a.Chats(false, false); len(main) != 1 || main[0].Archived {
		t.Fatalf("open chat: %+v", main)
	}
	if _, err := b.CloseChat(info.ID); err != nil {
		t.Fatal(err)
	}
	for _, n := range []*testNode{a, b} {
		eventually(t, n.cfg.Node+" archives the closed chat", func() bool {
			main, _ := n.Chats(false, false)
			arch, _ := n.Chats(true, false)
			return len(main) == 0 && len(arch) == 1 && arch[0].ID == info.ID && arch[0].Closed
		})
	}
	if ids := chatIDs(a, info.ID); len(ids) != 1 {
		t.Fatalf("archive changed the history: %v", ids)
	}
}
func TestClaimRunLimitsAutomaticChains(t *testing.T) {
	a, b, c := trio(t)
	info, err := a.CreateChat([]string{"b", "c"}, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []*testNode{b, c} {
		eventually(t, n.cfg.Node+" knows the chat", func() bool { _, ok := n.ChatOf(info.ID); return ok })
	}
	root, err := a.SendChat(ChatSend{ChatID: info.ID, Body: "external", Ask: []string{"b"}})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "b has the root", func() bool { return slices.Contains(chatIDs(b, info.ID), root.ID) })
	if ok, hold, err := b.ClaimRun(root); !ok || hold != "" || err != nil {
		t.Fatalf("b claims the external request: %v %q %v", ok, hold, err)
	}
	if ok, _, _ := b.ClaimRun(root); !ok {
		t.Fatal("a resent duplicate is refused")
	}
	if ok, hold, _ := c.ClaimRun(root); ok || hold != "" {
		t.Fatal("c claims a request not asking it")
	}
	// b's job asks c: the chain continues at depth 1 with the same root.
	next, err := b.SendChat(ChatSend{ChatID: info.ID, Body: "c, check", Ask: []string{"c"}, Parent: root.ID})
	if err != nil {
		t.Fatal(err)
	}
	if next.RootID != root.ID || next.AutoDepth != 1 {
		t.Fatalf("continued request %+v", next)
	}
	eventually(t, "c has it", func() bool { return slices.Contains(chatIDs(c, info.ID), next.ID) })
	if ok, _, _ := c.ClaimRun(next); !ok {
		t.Fatal("c refuses its first run of the chain")
	}
	// c's job asks b back: b runs again for the same root, one hop deeper.
	back, err := c.SendChat(ChatSend{ChatID: info.ID, Body: "b, again", Ask: []string{"b"}, Parent: next.ID})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "b has it", func() bool { return slices.Contains(chatIDs(b, info.ID), back.ID) })
	if ok, hold, _ := b.ClaimRun(back); !ok || hold != "" || back.AutoDepth != 2 {
		t.Fatalf("b refused the next round (depth %d): %q", back.AutoDepth, hold)
	}
	// Past MaxAutoDepth a request is held.
	deep := Message{ID: newID(), ChatID: info.ID, From: "a", Responders: []string{"b"}, RootID: newID(), AutoDepth: MaxAutoDepth + 1}
	if !deep.Held() {
		t.Fatal("deep request not held")
	}
	if ok, hold, _ := b.ClaimRun(deep); ok || hold != HoldAutoLimit {
		t.Fatal("held request claimed")
	}
	// Asking oneself or an outsider is refused.
	if _, err := a.SendChat(ChatSend{ChatID: info.ID, Body: "x", Ask: []string{"a"}}); !errors.Is(err, ErrBadParticipants) {
		t.Fatalf("ask self: %v", err)
	}
	if _, err := a.SendChat(ChatSend{ChatID: info.ID, Body: "x", Ask: []string{"z"}}); !errors.Is(err, ErrBadParticipants) {
		t.Fatalf("ask outsider: %v", err)
	}
}

// A peer without chat-v1 cannot be added to a chat, still gets legacy
// messages, and never receives a chat message queued for it.
func TestChatLegacyPeer(t *testing.T) {
	a := openNode(t, time.Second, 5*time.Second)
	conn, sc := manualDial(t, a, "b", nil, "")
	eventually(t, "b connected", func() bool { return a.Connected("b") })
	if _, err := a.CreateChat([]string{"b"}, ""); !errors.Is(err, ErrNoChatSupport) {
		t.Fatalf("chat with a legacy peer: %v", err)
	}
	chatMsg := Message{ID: newID(), From: "a", To: "b", ChatID: newID(), Participants: []string{"a", "b"}, Body: "chat", CreatedAt: time.Now().UTC()}
	if err := a.store.enqueue("b", chatMsg); err != nil {
		t.Fatal(err)
	}
	legacy, err := a.Send("b", "plain", "")
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	gotLegacy := false
	for sc.Scan() {
		f, ok := decodeFrame(sc.Bytes())
		if !ok || f.Type != "msg" {
			continue
		}
		if f.Msg.ChatID != "" {
			t.Fatal("chat message sent to a legacy peer")
		}
		gotLegacy = gotLegacy || f.Msg.ID == legacy.ID
	}
	if !gotLegacy {
		t.Fatal("legacy message not delivered")
	}
	chats, _ := a.Chats(false, true)
	if len(chats) != 1 || !chats[0].Legacy || chats[0].Peer != "b" || chats[0].Count != 1 || chats[0].Members[1].Compatible {
		t.Fatalf("legacy history as chats: %+v", chats)
	}
	// Writing into it sends a plain message: b has no chats.
	if m, err := a.SendChat(ChatSend{ChatID: chats[0].ID, Body: "plain again", Ask: []string{"b"}}); err != nil || m.ChatID != "" || m.To != "b" {
		t.Fatalf("send to a legacy peer's legacy chat = %+v, %v", m, err)
	}
	// Closing it archives it here only: b is never told.
	if info, err := a.CloseChat(chats[0].ID); err != nil || !info.Archived || info.ClosedBy != "a" {
		t.Fatalf("close a legacy peer's legacy chat = %+v, %v", info, err)
	}
	pending, _ := a.store.pending("b")
	if slices.ContainsFunc(pending, func(m Message) bool { return m.Kind == KindChatClose }) {
		t.Fatal("legacy close queued for a peer without chats")
	}
}

// Closing a legacy chat archives it on both sides, whichever side closes, and
// a later message brings it back on both.
func TestLegacyChatClosePropagates(t *testing.T) {
	a, b := pair(t, testSecret, testSecret)
	eventually(t, "connected", func() bool { return a.Connected("b") && b.Connected("a") })
	q, err := a.Send("b", "question", "")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "b got it", func() bool { e, _ := b.Recent(0); return len(e) == 1 })
	aID, bID := legacyChatID(q.ID, "b"), legacyChatID(q.ID, "a")
	archived := func(tn *testNode, id, by string) bool {
		info, err := tn.Chat(id)
		return err == nil && info.Archived && info.ClosedBy == by && !info.ClosedAt.IsZero() && !info.Closed
	}
	open := func(tn *testNode, id string) bool { info, err := tn.Chat(id); return err == nil && !info.Archived }
	if _, err := b.CloseChat(bID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "closed by b on both", func() bool { return archived(b, bID, "b") && archived(a, aID, "b") })
	// The close is a control message: no inbox entry, no history entry.
	if e, _ := a.Recent(0); len(e) != 1 {
		t.Fatalf("a's history after the close: %+v", e)
	}
	time.Sleep(10 * time.Millisecond)
	if _, err := b.Send("a", "answer", q.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "reopened on both", func() bool { return open(a, aID) && open(b, bID) })
	if _, err := a.CloseChat(aID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "closed by a on both", func() bool { return archived(a, aID, "a") && archived(b, bID, "a") })
}

// The side that answers a plain request sees its own agent working on it in
// the legacy chat, until it replies.
func TestLegacyChatShowsOwnJob(t *testing.T) {
	a, b := pair(t, testSecret, testSecret)
	eventually(t, "connected", func() bool { return a.Connected("b") && b.Connected("a") })
	q, err := a.Send("b", "question", "")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "b got it", func() bool { e, _ := b.Recent(0); return len(e) == 1 })
	if _, err := b.SendMessage(Message{ID: DerivedID(q.ID, "running"), To: "a", ReplyTo: q.ID, Kind: KindStatus, JobStatus: JobRunning, Activity: "thinking"}); err != nil {
		t.Fatal(err)
	}
	id := legacyChatID(q.ID, "a")
	info, err := b.Chat(id)
	if err != nil || !info.Active || len(info.Members) != 2 || !info.Members[1].Self {
		t.Fatalf("b's legacy chat %+v, %v", info, err)
	}
	if jobs := info.Members[1].Jobs; len(jobs) != 1 || jobs[0].ReplyTo != q.ID || jobs[0].JobStatus != JobRunning || jobs[0].Activity != "thinking" {
		t.Fatalf("b's own job %+v", jobs)
	}
	if _, err := b.SendMessage(Message{ID: DerivedID(q.ID, "reply"), To: "a", ReplyTo: q.ID, Body: "done", JobStatus: JobCompleted}); err != nil {
		t.Fatal(err)
	}
	if info, _ := b.Chat(id); info.Active || len(info.Members[1].Jobs) != 0 {
		t.Fatalf("b's legacy chat after the reply %+v", info)
	}
}

func TestLegacyHistoryAsVirtualChats(t *testing.T) {
	a, b := pair(t, testSecret, testSecret)
	eventually(t, "connected", func() bool { return a.Connected("b") && b.Connected("a") })
	q1, _ := a.Send("b", "first question", "")
	q2, _ := a.Send("b", "second question", "")
	eventually(t, "b got both", func() bool { e, _ := b.Recent(0); return len(e) == 2 })
	if _, err := b.Send("a", "answer one", q1.ID); err != nil {
		t.Fatal(err)
	}
	// Every plain exchange with b is one conversation, in time order, named
	// after its oldest request.
	eventually(t, "a got the answer", func() bool {
		chats, _ := a.Chats(false, true)
		return len(chats) == 1 && chats[0].Count == 3
	})
	id := legacyChatID(q1.ID, "b")
	msgs, err := a.ChatMessages(id, 0, 0, 0)
	if err != nil || len(msgs) != 3 || msgs[0].ID != q1.ID || msgs[1].ID != q2.ID || msgs[2].Direction != "in" {
		t.Fatalf("legacy chat messages %+v %v", msgs, err)
	}
	if bc, _ := b.Chats(false, true); len(bc) != 1 || bc[0].ID != legacyChatID(q1.ID, "a") {
		t.Fatalf("b's legacy chats %+v", bc)
	}
	// Writing into a legacy chat continues it in a real chat of a and b, the
	// same one next time.
	m, err := a.SendChat(ChatSend{ChatID: id, Body: "next question", ReplyTo: q1.ID, Ask: []string{"b"}})
	if err != nil || m.ChatID == "" || m.ReplyTo != "" || !m.Asks("b") {
		t.Fatalf("send to legacy chat = %+v, %v", m, err)
	}
	if again, err := a.SendChat(ChatSend{ChatID: id, Body: "more"}); err != nil || again.ChatID != m.ChatID {
		t.Fatalf("second send to legacy chat = %+v, %v", again, err)
	}
	eventually(t, "b has the real chat", func() bool { return slices.Contains(chatIDs(b, m.ChatID), m.ID) })
	if info, _ := b.Chat(m.ChatID); info.Legacy || !slices.Equal(info.Participants, []string{"a", "b"}) {
		t.Fatalf("b sees %+v", info)
	}
	if _, err := a.CloseChat(id); err != nil { // a legacy chat: archived on both sides
		t.Fatal(err)
	}
	eventually(t, "b archived its legacy chat", func() bool {
		info, err := b.Chat(legacyChatID(q1.ID, "a"))
		return err == nil && info.Archived && info.ClosedBy == "a"
	})
	if arch, _ := a.Chats(true, true); len(arch) != 1 || arch[0].ID != id {
		t.Fatalf("archived legacy chat: %+v", arch)
	}
	if chats, _ := a.Chats(false, false); len(chats) != 1 || chats[0].Legacy {
		t.Fatalf("without legacy=1: %+v", chats)
	}
}

// A crash after storing a chat message but before queueing its copies: the
// next start queues them.
func TestChatFanoutRepairedAfterCrash(t *testing.T) {
	a, b := pair(t, testSecret, testSecret)
	eventually(t, "connected", func() bool { return a.Connected("b") && b.Connected("a") })
	info, err := a.CreateChat([]string{"b"}, "")
	if err != nil {
		t.Fatal(err)
	}
	a.stop()
	lost := Message{ID: newID(), From: "a", ChatID: info.ID, Participants: info.Participants, Body: "lost in the crash", CreatedAt: time.Now().UTC()}
	if _, _, _, err := a.chats.add(lost); err != nil {
		t.Fatal(err)
	}
	re, err := New(a.cfg, a.secret, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := re.store.delivery("b", lost.ID); got != "queued" {
		t.Fatalf("after restart the copy for b is %q", got)
	}
}

// A real reply answers a request for good: a failed reply that came later
// (a worker that ran the same request in parallel) does not turn it failed.
func TestFailedReplyDoesNotOverrideAnswer(t *testing.T) {
	a, b := pair(t, testSecret, testSecret)
	eventually(t, "connected", func() bool { return a.Connected("b") && b.Connected("a") })
	q, err := a.Send("b", "which version?", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Send("a", "Godot 4.7.1", q.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond) // the failed reply is newer
	if _, err := b.SendMessage(Message{ID: DerivedID(q.ID, "reply"), To: "a", ReplyTo: q.ID, Body: "agentlink: handler timed out", JobStatus: JobFailed}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a got both replies", func() bool { e, _ := a.Recent(0); return len(e) == 3 })
	entries, _ := a.Recent(0)
	for _, e := range entries {
		if e.ID == q.ID && (e.Answer != "Godot 4.7.1" || e.JobStatus == JobFailed) {
			t.Fatalf("request entry %+v", e)
		}
	}
	msgs, err := a.ChatMessages(legacyChatID(q.ID, "b"), 0, 0, 0)
	if err != nil || len(msgs) != 3 || msgs[0].JobStatus != "" || msgs[2].JobStatus != JobFailed {
		t.Fatalf("legacy chat %+v %v", msgs, err)
	}
}

// A reply sent through the control API reports the request it answers,
// unless it comes from the job that answers that very request.
func TestLocalReplyHook(t *testing.T) {
	a, b := pair(t, testSecret, testSecret)
	eventually(t, "connected", func() bool { return a.Connected("b") && b.Connected("a") })
	var answered []string
	b.SetLocalReplyHook(func(id string) { answered = append(answered, id) })
	q, err := a.Send("b", "question", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.SendRequest(SendRequest{To: "a", Body: "from the job", ReplyTo: q.ID, Parent: q.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.SendRequest(SendRequest{To: "a", Body: "by hand", ReplyTo: q.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.SendRequest(SendRequest{To: "a", Body: "a new question"}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(answered, []string{q.ID}) {
		t.Fatalf("answered = %q", answered)
	}
}

// A new question sent "to" a member with chats through the control API goes
// into the open chat of the two (created once); a reply to a plain request stays plain.
func TestSendToContinuesChat(t *testing.T) {
	a, b := pair(t, testSecret, testSecret)
	eventually(t, "chat support", func() bool { return a.PeerHas("b", CapChat) && b.PeerHas("a", CapChat) })
	q1, err := a.SendRequest(SendRequest{To: "b", Body: "first"})
	if err != nil || q1.ChatID == "" || !q1.Asks("b") {
		t.Fatalf("first = %+v, %v", q1, err)
	}
	q2, err := a.SendRequest(SendRequest{To: "b", Body: "second"})
	if err != nil || q2.ChatID != q1.ChatID {
		t.Fatalf("second = %+v, %v", q2, err)
	}
	if chats, _ := a.Chats(false, true); len(chats) != 1 || chats[0].Legacy || chats[0].Count != 2 {
		t.Fatalf("chats %+v", chats)
	}
	plain, _ := b.Send("a", "plain question", "")
	eventually(t, "a got it", func() bool { e, _ := a.Recent(0); return len(e) == 1 })
	if r, err := a.SendRequest(SendRequest{To: "b", Body: "plain answer", ReplyTo: plain.ID}); err != nil || r.ChatID != "" {
		t.Fatalf("reply = %+v, %v", r, err)
	}
	// The control API's inbox lists chat messages too, with the chat reply.
	if _, err := b.SendChat(ChatSend{ChatID: q1.ChatID, Body: "answer", ReplyTo: q1.ID}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "answer in a's inbox", func() bool {
		e, _ := a.store.recent(0, true)
		return slices.ContainsFunc(e, func(e Entry) bool { return e.ID == q1.ID && e.Answer == "answer" })
	})
}

// A job that ends with a completed status and no reply of its own (the request
// was answered by hand) no longer shows as running, on both sides.
func TestChatJobEndsByStatus(t *testing.T) {
	a, b, _ := trio(t)
	info, err := a.CreateChat([]string{"b"}, "")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "b knows the chat", func() bool { _, ok := b.ChatOf(info.ID); return ok })
	q, err := a.SendChat(ChatSend{ChatID: info.ID, Body: "do it", Ask: []string{"b"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.SendMessage(Message{ID: DerivedID(q.ID, "b/running"), ChatID: info.ID, ReplyTo: q.ID, Kind: KindStatus, JobStatus: JobRunning,
		ActivityInfo: &ActivityState{Text: "step", Seq: 99}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a shows b running", func() bool { got, _ := a.Chat(info.ID); return got.Active })
	if _, err := b.SendChat(ChatSend{ChatID: info.ID, Body: "by hand", ReplyTo: q.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.SendMessage(Message{ID: DerivedID(q.ID, "b/answered"), ChatID: info.ID, ReplyTo: q.ID, Kind: KindStatus, JobStatus: JobCompleted}); err != nil {
		t.Fatal(err)
	}
	for _, n := range []*testNode{a, b} {
		eventually(t, n.cfg.Node+" shows b idle", func() bool { got, _ := n.Chat(info.ID); return !got.Active })
	}
}

// A held status shows on the asked participant as held, never as a running
// job, on both sides; it survives a restart and ends when that participant
// answers (a reply to it, or any message a person there writes).
func TestChatHeldStatus(t *testing.T) {
	a, b, _ := trio(t)
	info, err := a.CreateChat([]string{"b"}, "")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "b knows the chat", func() bool { _, ok := b.ChatOf(info.ID); return ok })
	held := func(n *testNode) []JobActivity {
		got, _ := n.Chat(info.ID)
		for _, m := range got.Members {
			if m.Name == "b" {
				if len(m.Jobs) > 0 || got.Active {
					t.Fatalf("%s shows a held request as running: %+v", n.cfg.Node, got)
				}
				return m.Held
			}
		}
		return nil
	}
	hold := func(q Message) {
		t.Helper()
		if _, err := b.SendMessage(Message{ID: DerivedID(q.ID, "b/held"), ChatID: info.ID, ReplyTo: q.ID, Kind: KindStatus,
			JobStatus: JobHeld, HoldReason: HoldNoHandler, Activity: HoldText(HoldNoHandler)}); err != nil {
			t.Fatal(err)
		}
	}
	q1, err := a.SendChat(ChatSend{ChatID: info.ID, Body: "anyone?", Ask: []string{"b"}})
	if err != nil {
		t.Fatal(err)
	}
	hold(q1)
	for _, n := range []*testNode{a, b} {
		eventually(t, n.cfg.Node+" shows b holding q1", func() bool {
			h := held(n)
			return len(h) == 1 && h[0].ReplyTo == q1.ID && h[0].JobStatus == JobHeld && h[0].HoldReason == HoldNoHandler &&
				h[0].Activity == "никто не отвечает — ждёт человека"
		})
	}
	reopened, err := openChatStore(a.cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := reopened.snapshot(info.ID); len(s.jobs) != 1 || s.jobs[0].JobStatus != JobHeld {
		t.Fatalf("held status lost on restart: %+v", s.jobs)
	}
	if _, err := b.SendChat(ChatSend{ChatID: info.ID, Body: "answer by hand", ReplyTo: q1.ID}); err != nil {
		t.Fatal(err)
	}
	for _, n := range []*testNode{a, b} {
		eventually(t, n.cfg.Node+" drops the answered hold", func() bool { return len(held(n)) == 0 })
	}
	q2, err := a.SendChat(ChatSend{ChatID: info.ID, Body: "and this?", Ask: []string{"b"}})
	if err != nil {
		t.Fatal(err)
	}
	hold(q2)
	eventually(t, "a shows b holding q2", func() bool { return len(held(a)) == 1 })
	if _, err := b.SendChat(ChatSend{ChatID: info.ID, Body: "a person writes here"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a person's message ends the hold", func() bool { return len(held(a)) == 0 })
}
