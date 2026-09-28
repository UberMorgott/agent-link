package node

import (
	"slices"
	"testing"
)

// Two unrelated threads of one chat keep their own hop counts: an agent
// message without a reply does not take the depth of another thread's
// request that asks only other members, while agents asking each other
// without replies still reach the hop limit.
func TestChainDepthPerThread(t *testing.T) {
	a, b, c := trio(t)
	info, err := a.CreateChat([]string{"b,c"}, "dev")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []*testNode{b, c} {
		eventually(t, n.cfg.Node+" knows the chat", func() bool { _, ok := n.ChatOf(info.ID); return ok })
	}
	// Thread 1: b's agents and c's agents deep in their own exchange.
	deep, err := b.SendMessage(Message{ChatID: info.ID, Body: "c, round seven", Responders: []string{"c"}, RootID: newID(), AutoDepth: MaxAutoDepth - 1})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "a has thread 1", func() bool { return slices.Contains(chatIDs(a, info.ID), deep.ID) })
	// Thread 2: a's agent starts its own request without a reply.
	q, err := a.SendChat(ChatSend{ChatID: info.ID, Body: "b, unrelated question", Ask: []string{"b"}})
	if err != nil {
		t.Fatal(err)
	}
	if q.AutoDepth != 0 || q.RootID != q.ID || q.Held() {
		t.Fatalf("thread 2 took thread 1's depth: depth %d root %s", q.AutoDepth, q.RootID)
	}
	eventually(t, "b has thread 2", func() bool { return slices.Contains(chatIDs(b, info.ID), q.ID) })

	// Runaway: a and b ask each other without replies; the depth still grows
	// and the request past the limit is held.
	prev := q
	for i := 0; prev.AutoDepth <= MaxAutoDepth; i++ {
		if i > 2*MaxAutoDepth {
			t.Fatalf("runaway loop not bounded: depth %d", prev.AutoDepth)
		}
		from, to := b, a
		if prev.From == "b" {
			from, to = a, b
		}
		next, err := from.SendChat(ChatSend{ChatID: info.ID, Body: "again", Ask: []string{to.cfg.Node}})
		if err != nil {
			t.Fatal(err)
		}
		if next.AutoDepth != prev.AutoDepth+1 || next.RootID != q.ID {
			t.Fatalf("hop after %d: depth %d root %s", prev.AutoDepth, next.AutoDepth, next.RootID)
		}
		eventually(t, to.cfg.Node+" has the hop", func() bool { return slices.Contains(chatIDs(to, info.ID), next.ID) })
		prev = next
	}
	if !prev.Held() {
		t.Fatalf("request past the limit not held: %+v", prev)
	}

	// An unrelated inform arriving after b's held request does not let a's
	// answer escape the hop limit: the request stays its base.
	fyi, err := c.SendMessage(Message{ChatID: info.ID, Body: "fyi: deployed", RootID: newID(), AutoDepth: 5})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "a has the fyi", func() bool { return slices.Contains(chatIDs(a, info.ID), fyi.ID) })
	held, err := a.SendChat(ChatSend{ChatID: info.ID, Body: "still on it", Ask: []string{"c"}})
	if err != nil {
		t.Fatal(err)
	}
	if held.RootID != q.ID || held.AutoDepth != prev.AutoDepth+1 || !held.Held() {
		t.Fatalf("answer to a held request after an fyi: depth %d root %s", held.AutoDepth, held.RootID)
	}

	// Information to everyone, alone since a last wrote, continues its chain.
	fyi, err = c.SendMessage(Message{ChatID: info.ID, Body: "fyi: deployed again", RootID: newID(), AutoDepth: 5})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "a has the fyi", func() bool { return slices.Contains(chatIDs(a, info.ID), fyi.ID) })
	after, err := a.SendChat(ChatSend{ChatID: info.ID, Body: "noted", Ask: []string{"c"}})
	if err != nil {
		t.Fatal(err)
	}
	if after.RootID != fyi.RootID || after.AutoDepth != 6 {
		t.Fatalf("after an fyi: depth %d root %s", after.AutoDepth, after.RootID)
	}

	// A handoff: b replies to a's message but asks only c. That is c's
	// thread; a's next message without a reply does not take its depth.
	handoff, err := b.SendMessage(Message{ChatID: info.ID, Body: "c, take it over", ReplyTo: after.ID, Responders: []string{"c"},
		RootID: newID(), AutoDepth: MaxAutoDepth})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "a has the handoff", func() bool { return slices.Contains(chatIDs(a, info.ID), handoff.ID) })
	own, err := a.SendChat(ChatSend{ChatID: info.ID, Body: "b, one more thing", Ask: []string{"b"}})
	if err != nil {
		t.Fatal(err)
	}
	if own.RootID == handoff.RootID || own.AutoDepth != 6 || own.Held() {
		t.Fatalf("a took the handoff's depth: depth %d root %s", own.AutoDepth, own.RootID)
	}
}

// Since a node's agents last wrote, a request for them outranks information
// of an unrelated thread (it lends no depth), and of several requests the
// deepest is the base: sessions of one node are not told apart, so a
// shallower thread must not lower another's count.
func TestChainBaseInformAndDeepest(t *testing.T) {
	a, b, c := trio(t)
	info, err := a.CreateChat([]string{"b,c"}, "dev")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []*testNode{b, c} {
		eventually(t, n.cfg.Node+" knows the chat", func() bool { _, ok := n.ChatOf(info.ID); return ok })
	}
	send := func(from *testNode, m Message) Message {
		t.Helper()
		m.ChatID = info.ID
		out, err := from.SendMessage(m)
		if err != nil {
			t.Fatal(err)
		}
		eventually(t, "a has "+m.Body, func() bool { return slices.Contains(chatIDs(a, info.ID), out.ID) })
		return out
	}
	ask := send(b, Message{Body: "a, check this", Responders: []string{"a"}, RootID: newID(), AutoDepth: 1})
	fyi := send(c, Message{Body: "fyi: unrelated deploy", RootID: newID(), AutoDepth: 5})
	got, err := a.SendChat(ChatSend{ChatID: info.ID, Body: "checked", Ask: []string{"b"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.RootID != ask.RootID || got.AutoDepth != 2 {
		t.Fatalf("answer took the fyi's chain: depth %d root %s (ask %s, fyi %s)", got.AutoDepth, got.RootID, ask.RootID, fyi.RootID)
	}

	deep := send(b, Message{Body: "a, round six", Responders: []string{"a"}, RootID: newID(), AutoDepth: 6})
	send(c, Message{Body: "a, new question", Responders: []string{"a"}, RootID: newID(), AutoDepth: 1})
	got, err = a.SendChat(ChatSend{ChatID: info.ID, Body: "on it", Ask: []string{"b"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.RootID != deep.RootID || got.AutoDepth != 7 {
		t.Fatalf("interleaved threads under-counted: depth %d root %s (deep %s)", got.AutoDepth, got.RootID, deep.RootID)
	}

	// An inform of a requesting thread counts even when another thread's
	// request is deeper than that thread's request.
	x := send(b, Message{Body: "a, thread x", Responders: []string{"a"}, RootID: newID(), AutoDepth: 1})
	send(c, Message{Body: "fyi: thread x moved on", RootID: x.RootID, AutoDepth: 8})
	send(c, Message{Body: "a, thread y", Responders: []string{"a"}, RootID: newID(), AutoDepth: 2})
	got, err = a.SendChat(ChatSend{ChatID: info.ID, Body: "both", Ask: []string{"b"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.RootID != x.RootID || got.AutoDepth != 9 {
		t.Fatalf("thread x's inform dropped: depth %d root %s (x %s)", got.AutoDepth, got.RootID, x.RootID)
	}
}

// A person's prompt a seat's turn handles in one chat resets nothing in
// another chat: the seat's message there continues that chat's deep loop.
func TestSeatTurnPromptOtherChatNoReset(t *testing.T) {
	a, b := pair(t, testSecret, testSecret)
	h, err := a.SendRequest(SendRequest{To: "b", Body: "prompt", AuthorKind: AuthorHuman})
	if err != nil {
		t.Fatal(err)
	}
	other, err := a.CreateChat([]string{"b"}, "other")
	if err != nil || other.ID == h.ChatID {
		t.Fatalf("other chat %+v %v", other, err)
	}
	eventually(t, "b knows the chat", func() bool { _, ok := b.ChatOf(other.ID); return ok })
	deep, err := b.SendMessage(Message{ChatID: other.ID, Body: "a, deep", Responders: []string{"a"}, RootID: newID(), AutoDepth: MaxAutoDepth - 1})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "a has the deep ask", func() bool { return slices.Contains(chatIDs(a, other.ID), deep.ID) })
	stored, ok := a.chats.message(h.ID)
	if !ok {
		t.Fatal("prompt not stored")
	}
	a.seats.setHandling("seat-x", []UnreadMessage{{Message: stored.Message}})
	defer a.seats.setHandling("seat-x", nil)
	got, err := a.SendChat(ChatSend{ChatID: other.ID, Body: "answer", Ask: []string{"b"}, Agent: &AgentRef{Seat: "seat-x"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.RootID != deep.RootID || got.AutoDepth != MaxAutoDepth {
		t.Fatalf("another chat's prompt reset the loop: depth %d root %s (deep %s)", got.AutoDepth, got.RootID, deep.RootID)
	}
}

// Agents replying again and again to the same old message of a person still
// reach the hop limit: only the first answer to the prompt starts over.
func TestChainStaleReplyLoopHeld(t *testing.T) {
	a, b := pair(t, testSecret, testSecret)
	h, err := a.SendRequest(SendRequest{To: "b", Body: "decide", AuthorKind: AuthorHuman})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "b has the prompt", func() bool { return slices.Contains(chatIDs(b, h.ChatID), h.ID) })
	from, to := b, a
	var last Message
	for i := 0; !last.Held(); i++ {
		if i > 2*MaxAutoDepth+4 {
			t.Fatalf("stale reply_to loop not bounded: depth %d", last.AutoDepth)
		}
		next, err := from.SendChat(ChatSend{ChatID: h.ChatID, Body: "again", ReplyTo: h.ID, Ask: []string{to.cfg.Node}})
		if err != nil {
			t.Fatal(err)
		}
		if next.RootID != h.ID || (i > 1 && next.AutoDepth != last.AutoDepth+1) {
			t.Fatalf("hop %d: depth %d (last %d) root %s", i, next.AutoDepth, last.AutoDepth, next.RootID)
		}
		eventually(t, to.cfg.Node+" has the hop", func() bool { return slices.Contains(chatIDs(to, h.ChatID), next.ID) })
		last, from, to = next, to, from
	}
}
