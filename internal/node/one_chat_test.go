package node

import (
	"errors"
	"slices"
	"testing"
	"time"
)

// activeIDs lists n's active (main list) chat ids.
func activeIDs(n *testNode) []string {
	var out []string
	for _, c := range openChats(n) {
		out = append(out, c.ID)
	}
	return out
}

// Archiving the history closes the project's chat for both sides and opens a
// fresh, empty one with the same members at once; it takes over the old one.
func TestProjectArchiveChat(t *testing.T) {
	p := newTestProject(t)
	a, b := projectPair(t, p)
	old, err := a.NewProjectChat([]string{"b"})
	if err != nil {
		t.Fatal(err)
	}
	m, err := a.SendChat(ChatSend{ChatID: old.ID, Body: "before", Ask: []string{"b"}})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "b has the message", func() bool { return slices.Contains(chatIDs(b, old.ID), m.ID) })

	fresh, err := a.ArchiveChat("")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.ID == old.ID || fresh.Prev != old.ID || fresh.Count != 0 || fresh.Archived ||
		!slices.Equal(fresh.Participants, []string{"a", "b"}) {
		t.Fatalf("fresh chat %+v", fresh)
	}
	for _, n := range []*testNode{a, b} {
		eventually(t, n.cfg.Node+" has one active chat, the fresh one", func() bool {
			info, err := n.Chat(old.ID)
			ids := activeIDs(n)
			return err == nil && info.Archived && info.Closed && len(ids) == 1 && ids[0] == fresh.ID
		})
		if msgs, _ := n.ChatMessages(old.ID, 0, 0, 0); !slices.ContainsFunc(msgs, func(cm ChatMessage) bool { return cm.ID == m.ID }) {
			t.Fatalf("%s lost the archived history", n.cfg.Node)
		}
	}
	eventually(t, "b: the fresh chat takes over from the old one", func() bool { c, _ := b.ChatOf(fresh.ID); return c.Prev == old.ID })
	// Archiving an archived chat changes nothing: the active one stays.
	if again, err := b.ArchiveChat(old.ID); err != nil || again.ID != fresh.ID {
		t.Fatalf("archive of an archived chat: %+v, %v", again.Chat, err)
	}
	if _, err := a.ArchiveChat("ffffffffffffffffffffffffffffffff"); !errors.Is(err, ErrUnknownChat) {
		t.Fatalf("unknown chat: %v", err)
	}
	if _, err := newTestNode(t, "x", testSecret, nil, t.TempDir(), listen(t), nil).ArchiveChat(""); !errors.Is(err, ErrNotProject) {
		t.Fatalf("legacy: %v", err)
	}
}

// A chat an (older) peer starts while this node has an active one: the newest
// wins, the other goes to the archive; a late, older chat is archived itself.
func TestProjectNewestChatWins(t *testing.T) {
	p := newTestProject(t)
	a := newProjectNode(t, "a", p, listen(t), nil)
	bID := newID()
	seedMember(a.Node, "b", bID, 1)
	mine, err := a.NewProjectChat([]string{"b"})
	if err != nil {
		t.Fatal(err)
	}
	parts, ids := []string{"a", "b"}, []string{a.ID(), bID}
	open := func(at time.Time) Message {
		return Message{ID: newID(), From: "b", Kind: KindChatOpen, ChatID: newID(), Participants: parts, ParticipantIDs: ids,
			ChatMode: ChatModeProject, ChatOwner: "b", CreatedAt: at}
	}
	newer := open(time.Now().Add(time.Minute).UTC())
	if !a.receiveChat("b", bID, newer) {
		t.Fatal("newer chat rejected")
	}
	if got := activeIDs(a); !slices.Equal(got, []string{newer.ChatID}) {
		t.Fatalf("active %v, want the newer chat", got)
	}
	if info, _ := a.Chat(mine.ID); !info.Archived || !info.Closed {
		t.Fatal("the older chat is not archived")
	}
	if c, _ := a.ChatOf(newer.ChatID); c.Prev != mine.ID {
		t.Fatalf("newer chat takes over from %q", c.Prev)
	}
	late := open(time.Now().Add(-time.Hour).UTC())
	if !a.receiveChat("b", bID, late) {
		t.Fatal("late chat rejected")
	}
	if got := activeIDs(a); !slices.Equal(got, []string{newer.ChatID}) {
		t.Fatalf("active %v after a late chat", got)
	}
	if info, _ := a.Chat(late.ChatID); !info.Archived {
		t.Fatal("the late chat is not archived")
	}
}

// On start a project node with several active chats (data from before the
// rule) keeps the one with the newest message and archives the others.
func TestProjectChatsMigration(t *testing.T) {
	p := newTestProject(t)
	a := newProjectNode(t, "a", p, listen(t), nil)
	seedMember(a.Node, "b", newID(), 1)
	var made []Chat
	for range 3 {
		a.ensureMu.Lock()
		c, err := a.startProjectChatLocked([]string{"a", "b"}, "")
		a.ensureMu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		made = append(made, c)
	}
	if got := activeIDs(a); len(got) != 3 {
		t.Fatalf("setup: %d active chats", len(got))
	}
	time.Sleep(10 * time.Millisecond)
	m, err := a.SendChat(ChatSend{ChatID: made[0].ID, Body: "latest here"})
	if err != nil {
		t.Fatal(err)
	}
	a.stop()
	a2 := newProjectNodeAt(t, "a", p, a.cfg.DataDir, listen(t), nil)
	if got := activeIDs(a2); !slices.Equal(got, []string{made[0].ID}) {
		t.Fatalf("active after migration %v, want %s", got, made[0].ID)
	}
	arch, _ := a2.Chats(true, false)
	if len(arch) != 2 {
		t.Fatalf("archive %d chats, want 2", len(arch))
	}
	if !slices.Contains(chatIDs(a2, made[0].ID), m.ID) {
		t.Fatal("kept chat lost its message")
	}
}

// Sessions of an archived chat follow its history into the chat that took
// over, until a session of its own takes part.
func TestAffinityFollowsPrev(t *testing.T) {
	cs, err := openChatStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	old := Chat{ID: newID(), Participants: []string{"a", "b"}, Project: "p", Mode: ChatModeProject}
	next := Chat{ID: newID(), Participants: []string{"a", "b"}, Project: "p", Mode: ChatModeProject, Prev: old.ID}
	for _, c := range []Chat{old, next} {
		if _, err := cs.ensure(c); err != nil {
			t.Fatal(err)
		}
	}
	m := Message{ID: newID(), From: "a", ChatID: old.ID, Body: "x"}
	if _, _, _, err := cs.add(m); err != nil {
		t.Fatal(err)
	}
	if err := cs.setSession(m.ID, "s1"); err != nil {
		t.Fatal(err)
	}
	live := map[string]bool{"s1": true}
	if got := cs.affinity(next.ID, "a", live); got != "s1" {
		t.Fatalf("affinity %q, want s1", got)
	}
	if got := cs.affinity(next.ID, "a", map[string]bool{}); got != "" {
		t.Fatalf("affinity to a dead session %q", got)
	}
}
