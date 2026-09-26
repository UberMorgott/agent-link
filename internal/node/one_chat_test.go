package node

import (
	"errors"
	"slices"
	"strings"
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

// «Очистить чат» (ArchiveChat) from any member clears the chat for everyone:
// the history becomes a dated, read-only snapshot on every node, the chat goes
// on empty with the same members, a message to the old chat lands in the
// fresh one, the project never gets a second active chat, and a request the
// clear caught unread stays deliverable (listed unread, ackable) on the node
// it asks.
func TestClearChatForAll(t *testing.T) {
	p := newTestProject(t)
	a, b := projectPair(t, p)
	old, err := a.NewProjectChat([]string{"b"})
	if err != nil {
		t.Fatal(err)
	}
	q, err := a.SendChat(ChatSend{ChatID: old.ID, Body: "please look", Ask: []string{"b"}})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "b has the request", func() bool { return slices.Contains(chatIDs(b, old.ID), q.ID) })

	// b is not the chat's owner and clears it all the same.
	fresh, err := b.ArchiveChat("")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []*testNode{a, b} {
		eventually(t, n.cfg.Node+": the snapshot is history, the fresh chat active", func() bool {
			hist, err := n.Chats(true, false)
			ids := activeIDs(n)
			return err == nil && len(hist) == 1 && hist[0].ID == old.ID && !hist[0].ClosedAt.IsZero() &&
				hist[0].Count == 1 && len(ids) == 1 && ids[0] == fresh.ID
		})
	}
	// The unread request of the cleared chat is still b's to read.
	page, err := b.Unread("", "", 0)
	if err != nil || !slices.ContainsFunc(page.Messages, func(m UnreadMessage) bool { return m.ID == q.ID }) {
		t.Fatalf("b's unread after the clear: %+v, %v", page.Messages, err)
	}
	if res, err := b.Ack("", AckRequest{IDs: []string{q.ID}}); err != nil || !res[0].Found || !res[0].WasUnread {
		t.Fatalf("ack of the cleared request: %+v, %v", res, err)
	}
	// Answering it goes on in the fresh chat.
	r, err := b.SendChat(ChatSend{ChatID: old.ID, ReplyTo: q.ID, Body: "done"})
	if err != nil || r.ChatID != fresh.ID {
		t.Fatalf("reply to the cleared chat: %+v, %v", r, err)
	}
	eventually(t, "a gets the reply in the fresh chat", func() bool { return slices.Contains(chatIDs(a, fresh.ID), r.ID) })
	// One chat per project: asking for a new one returns the fresh chat.
	if again, err := a.NewProjectChat([]string{"b"}); err != nil || again.ID != fresh.ID {
		t.Fatalf("new chat after a clear: %+v, %v", again.Chat, err)
	}
	if ids := activeIDs(a); len(ids) != 1 {
		t.Fatalf("active chats %v", ids)
	}
	// The snapshot is read-only history: its messages are kept as they were.
	if msgs, _ := a.ChatMessages(old.ID, 0, 0, 0); !slices.ContainsFunc(msgs, func(cm ChatMessage) bool { return cm.ID == q.ID }) ||
		slices.ContainsFunc(msgs, func(cm ChatMessage) bool { return cm.ID == r.ID }) {
		t.Fatal("the snapshot changed")
	}
}

// A nickname travels in the member record; the member's name stays its
// identity, and send, ask and chat members resolve the nickname or an earlier
// one (ignoring case) to it. Another member's name or nickname is taken.
func TestNicknameResolves(t *testing.T) {
	p := newTestProject(t)
	a, b := projectPair(t, p)
	displayOf := func(n *testNode, name string) string {
		for _, m := range n.Members() {
			if m.Name == name {
				return m.Display
			}
		}
		return "?"
	}
	b.SetDisplay("Nikita", nil)
	eventually(t, "a sees b's nickname", func() bool { return displayOf(a, "b") == "Nikita" })
	b.SetDisplay("Kpectik", []string{"Nikita"})
	eventually(t, "a sees the new nickname", func() bool { return displayOf(a, "b") == "Kpectik" })
	for _, s := range []string{"b", "B", "kpectik", "nikita", " Nikita "} {
		if got := a.ResolveMember(s); got != "b" {
			t.Fatalf("ResolveMember(%q) = %q", s, got)
		}
	}
	if got := a.ResolveMember("nobody"); got != "nobody" {
		t.Fatalf("unknown name resolved to %q", got)
	}
	if !a.NameTaken("KPECTIK") || !a.NameTaken("nikita") || !a.NameTaken("B") || a.NameTaken("a") || a.NameTaken("olga") {
		t.Fatal("NameTaken")
	}
	// An old nickname asks the member in a send.
	m, err := a.SendRequest(SendRequest{To: "nikita", Body: "hi", Ask: []string{"Kpectik"}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(m.Responders, []string{"b"}) || !slices.Contains(m.Participants, "b") {
		t.Fatalf("sent %+v", m)
	}
	eventually(t, "b has it", func() bool { return slices.Contains(chatIDs(b, m.ChatID), m.ID) })
	if !ValidDisplay("Морготт") || ValidDisplay("") || ValidDisplay(" x") || ValidDisplay("a,b") || ValidDisplay(strings.Repeat("я", MaxDisplayLen+1)) {
		t.Fatal("ValidDisplay")
	}
	// A member that takes another's earlier nickname makes it ambiguous: it
	// never takes over the asks meant for the other one.
	a.SetDisplay("NIKITA", nil)
	if got := a.ResolveMember("nikita"); got != "nikita" {
		t.Fatalf("taken earlier nickname resolved to %q", got)
	}
}
