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

	// Information to everyone still continues its chain.
	fyi, err := c.SendMessage(Message{ChatID: info.ID, Body: "fyi: deployed", RootID: newID(), AutoDepth: 5})
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
