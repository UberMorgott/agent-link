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
	if _, err := a.SendChat(ChatSend{ChatID: info.ID, Body: "more", Ask: []string{"b"}}); !errors.Is(err, ErrChatClosed) {
		t.Fatalf("send to a closed chat: %v", err)
	}
	if ok, _ := c.ClaimRun(Message{ID: q.ID, ChatID: info.ID, Responders: []string{"c"}, RootID: q.ID}); ok {
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

func TestChatArchive(t *testing.T) {
	a, b := pair(t, testSecret, testSecret)
	eventually(t, "connected", func() bool { return a.Connected("b") && b.Connected("a") })
	info, err := a.CreateChat([]string{"b"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.ArchiveChat(info.ID, true); err != nil {
		t.Fatal(err)
	}
	main, _ := a.Chats(false, false)
	arch, _ := a.Chats(true, false)
	if len(main) != 0 || len(arch) != 1 || arch[0].ID != info.ID {
		t.Fatalf("after archive: main %d, archive %d", len(main), len(arch))
	}
	eventually(t, "b knows the chat", func() bool { _, ok := b.ChatOf(info.ID); return ok })
	time.Sleep(10 * time.Millisecond)
	m, err := b.SendChat(ChatSend{ChatID: info.ID, Body: "new message"})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "a new message brings the chat back", func() bool {
		main, _ := a.Chats(false, false)
		return len(main) == 1 && main[0].LastMessage != nil && main[0].LastMessage.ID == m.ID
	})
	if _, err := a.CloseChat(info.ID); err != nil {
		t.Fatal(err)
	}
	if main, _ := a.Chats(false, false); len(main) != 0 {
		t.Fatal("a closed chat stays in the main list")
	}
	if got, _ := a.ArchiveChat(info.ID, false); got.Archived || !got.Closed {
		t.Fatalf("unarchived closed chat: %+v", got)
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
	if ok, err := b.ClaimRun(root); !ok || err != nil {
		t.Fatalf("b claims the external request: %v %v", ok, err)
	}
	if ok, _ := b.ClaimRun(root); !ok {
		t.Fatal("a resent duplicate is refused")
	}
	if ok, _ := c.ClaimRun(root); ok {
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
	if ok, _ := c.ClaimRun(next); !ok {
		t.Fatal("c refuses its first run of the chain")
	}
	// c's job asks b back: b already ran for this root.
	back, err := c.SendChat(ChatSend{ChatID: info.ID, Body: "b, again", Ask: []string{"b"}, Parent: next.ID})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "b has it", func() bool { return slices.Contains(chatIDs(b, info.ID), back.ID) })
	if ok, _ := b.ClaimRun(back); ok || back.AutoDepth != 2 {
		t.Fatalf("b ran twice for one root (depth %d)", back.AutoDepth)
	}
	// Past MaxAutoDepth a request is held.
	deep := Message{ID: newID(), ChatID: info.ID, From: "a", Responders: []string{"b"}, RootID: newID(), AutoDepth: MaxAutoDepth + 1}
	if !deep.Held() {
		t.Fatal("deep request not held")
	}
	if ok, _ := b.ClaimRun(deep); ok {
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
	if chats, _ := a.Chats(false, true); len(chats) != 1 || !chats[0].Legacy || chats[0].Peer != "b" || chats[0].Count != 1 {
		t.Fatalf("legacy history as chats: %+v", chats)
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
	eventually(t, "a got the answer", func() bool {
		chats, _ := a.Chats(false, true)
		return len(chats) == 2 && slices.ContainsFunc(chats, func(c ChatInfo) bool { return c.Count == 2 })
	})
	id := legacyChatID(q1.ID, "b")
	msgs, err := a.ChatMessages(id, 0, 0, 0)
	if err != nil || len(msgs) != 2 || msgs[0].ID != q1.ID || msgs[1].Direction != "in" {
		t.Fatalf("legacy chat messages %+v %v", msgs, err)
	}
	if _, err := a.SendChat(ChatSend{ChatID: id, Body: "x"}); !errors.Is(err, ErrLegacyChat) {
		t.Fatalf("send to legacy chat: %v", err)
	}
	if _, err := a.ArchiveChat(legacyChatID(q2.ID, "b"), true); err != nil {
		t.Fatal(err)
	}
	if arch, _ := a.Chats(true, true); len(arch) != 1 || arch[0].ID != legacyChatID(q2.ID, "b") {
		t.Fatalf("archived legacy chat: %+v", arch)
	}
	if chats, _ := a.Chats(false, false); len(chats) != 0 {
		t.Fatal("legacy chats listed without legacy=1")
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
