package node

import (
	"errors"
	"slices"
	"testing"
)

// A standalone project chat starts with its owner alone; the owner adds
// members later, an offline one too, and removes them. An added member gets
// the chat from then on, never older messages; a removed one keeps its copy
// archived and gets nothing more.
func TestProjectChatMembers(t *testing.T) {
	p := newTestProject(t)
	a, b := projectPair(t, p)
	solo, err := a.NewProjectChat(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(solo.Participants, []string{"a"}) || solo.Owner != "a" || solo.Archived || solo.Removed {
		t.Fatalf("solo chat %+v", solo.Chat)
	}
	before, err := a.SendChat(ChatSend{ChatID: solo.ID, Body: "note to self"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetChatMembers(solo.ID, nil, []string{"a"}); !errors.Is(err, ErrBadParticipants) {
		t.Fatalf("owner removed itself: %v", err)
	}
	if _, err := a.SetChatMembers(solo.ID, []string{"zed"}, nil); !errors.Is(err, ErrUnknownPeer) {
		t.Fatalf("unknown member: %v", err)
	}

	info, err := a.SetChatMembers(solo.ID, []string{"b"}, nil)
	if err != nil || !slices.Equal(info.Participants, []string{"a", "b"}) || info.Rev != 1 {
		t.Fatalf("add b: %+v, %v", info.Chat, err)
	}
	eventually(t, "b has the chat", func() bool {
		c, ok := b.ChatOf(solo.ID)
		return ok && slices.Equal(c.Participants, []string{"a", "b"}) && c.Owner == "a" && c.Rev == 1
	})
	if slices.Contains(chatIDs(b, solo.ID), before.ID) || deliveryOf(a, solo.ID, before.ID, "b") != "" {
		t.Fatal("b got or is owed a message from before it joined")
	}
	if _, err := b.SetChatMembers(solo.ID, nil, []string{"a"}); !errors.Is(err, ErrNotChatOwner) {
		t.Fatalf("b changed a's chat: %v", err)
	}
	hi, err := b.SendChat(ChatSend{ChatID: solo.ID, Body: "hi", Ask: []string{"a"}})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "a gets b's message", func() bool { return slices.Contains(chatIDs(a, solo.ID), hi.ID) })

	// c is a member that is not online: the change waits in its queue.
	seedMember(a.Node, "c", newID(), 1)
	if _, err := a.SetChatMembers(solo.ID, []string{"c"}, nil); err != nil {
		t.Fatal(err)
	}
	if msgs, _ := a.store.pending("c"); !slices.ContainsFunc(msgs, func(m Message) bool {
		return m.Kind == KindChatMembers && slices.Equal(m.Participants, []string{"a", "b", "c"}) && m.ChatRev == 2
	}) {
		t.Fatalf("no change queued for the offline member: %+v", msgs)
	}
	eventually(t, "b sees c", func() bool {
		c, _ := b.ChatOf(solo.ID)
		return slices.Equal(c.Participants, []string{"a", "b", "c"})
	})

	if _, err := a.SetChatMembers(solo.ID, nil, []string{"b"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "b removed", func() bool {
		info, err := b.Chat(solo.ID)
		return err == nil && info.Removed && info.Archived && !info.Closed
	})
	if _, err := b.SendChat(ChatSend{ChatID: solo.ID, Body: "still here?"}); !errors.Is(err, ErrChatClosed) {
		t.Fatalf("a removed member sent: %v", err)
	}
	after, err := a.SendChat(ChatSend{ChatID: solo.ID, Body: "after", Ask: []string{"c"}})
	if err != nil {
		t.Fatal(err)
	}
	if a.store.delivery("b", after.ID) != "" || a.store.delivery("c", after.ID) != "queued" {
		t.Fatal("after the removal: b got a copy or c did not")
	}
	if err := a.repairChats(); err != nil {
		t.Fatal(err)
	}
	if a.store.delivery("c", before.ID) != "" || a.store.delivery("c", hi.ID) != "" {
		t.Fatal("repair queued older messages for a member that joined later")
	}
	keyed, err := a.EnsureOpenChat([]string{"a", "b"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetChatMembers(keyed.ID, []string{"c"}, nil); !errors.Is(err, ErrBadParticipants) {
		t.Fatalf("a keyed chat changed members: %v", err)
	}
}

// Only the owner changes members: a change from another participant, or a
// message of a chat this node was removed from, is dropped.
func TestProjectChatMembersReceive(t *testing.T) {
	p := newTestProject(t)
	a := newProjectNode(t, "a", p, listen(t), nil)
	bID, cID := newID(), newID()
	seedMember(a.Node, "b", bID, 1)
	seedMember(a.Node, "c", cID, 1)
	id := newID()
	env := func(from, kind string, parts, ids []string, rev uint32) Message {
		return Message{ID: newID(), From: from, Kind: kind, ChatID: id, Participants: parts, ParticipantIDs: ids,
			ChatMode: ChatModeProject, ChatOwner: "b", ChatRev: rev}
	}
	abc, abcIDs := []string{"a", "b", "c"}, []string{a.ID(), bID, cID}
	if !a.receiveChat("b", bID, env("b", KindChatMembers, abc, abcIDs, 1)) {
		t.Fatal("the owner's change rejected")
	}
	if a.receiveChat("c", cID, env("c", KindChatMembers, []string{"a", "c"}, []string{a.ID(), cID}, 2)) {
		t.Fatal("a non-owner's change accepted")
	}
	if !a.receiveChat("b", bID, env("b", KindChatMembers, []string{"b", "c"}, []string{bID, cID}, 2)) {
		t.Fatal("the removal rejected")
	}
	if info, _ := a.Chat(id); !info.Removed || !info.Archived {
		t.Fatalf("not removed: %+v", info)
	}
	if a.receiveChat("c", cID, env("c", "", abc, abcIDs, 1)) {
		t.Fatal("a stale message of a chat a was removed from accepted")
	}
}
