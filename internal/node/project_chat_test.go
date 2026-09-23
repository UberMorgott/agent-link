package node

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// projectPair starts two connected nodes a and b of project p.
func projectPair(t *testing.T, p testProject) (a, b *testNode) {
	t.Helper()
	lnA, lnB := listen(t), listen(t)
	a = newProjectNode(t, "a", p, lnA, map[string]net.Listener{"b": lnB})
	b = newProjectNode(t, "b", p, lnB, nil)
	a.start(t)
	b.start(t)
	eventually(t, "a<->b connected", func() bool { return a.Connected("b") && b.Connected("a") })
	return a, b
}

// Equal names in two projects get different keyed chats, pinned to the node
// ids of each project's members; the envelope carries the ids.
func TestProjectChatIDsScoped(t *testing.T) {
	p1, p2 := newTestProject(t), newTestProject(t)
	a1, b1 := projectPair(t, p1)
	a2, _ := projectPair(t, p2)
	c1, err := a1.CreateChat([]string{"b"}, "")
	if err != nil {
		t.Fatal(err)
	}
	c2, err := a2.CreateChat([]string{"b"}, "")
	if err != nil {
		t.Fatal(err)
	}
	parts := []string{"a", "b"}
	if c1.ID == c2.ID || c1.ID == KeyedChatID("", parts, 0) || !c1.Keyed || c1.Project != p1.id {
		t.Fatalf("chat ids %s / %s, keyed %v, project %q", c1.ID, c2.ID, c1.Keyed, c1.Project)
	}
	if want := []string{a1.ID(), b1.ID()}; !slices.Equal(c1.ParticipantIDs, want) || c1.ID != ProjectChatID(p1.id, parts, want, 0) {
		t.Fatalf("pinned ids %v, want %v", c1.ParticipantIDs, want)
	}
	if _, err := a1.CreateChat([]string{"b"}, "dev"); err == nil {
		t.Fatal("a project chat took an area")
	}
	m, err := a1.SendChat(ChatSend{ChatID: c1.ID, Body: "hi", Ask: []string{"b"}})
	if err != nil || !slices.Equal(m.ParticipantIDs, c1.ParticipantIDs) {
		t.Fatalf("send: %+v, %v", m, err)
	}
	eventually(t, "b stores the message", func() bool { return slices.Contains(chatIDs(b1, c1.ID), m.ID) })
	got, ok := b1.ChatOf(c1.ID)
	if !ok || !slices.Equal(got.ParticipantIDs, c1.ParticipantIDs) || got.Project != p1.id || !got.Keyed() {
		t.Fatalf("b's chat %+v", got)
	}
	eventually(t, "delivered", func() bool { return deliveryOf(a1, c1.ID, m.ID, "b") == StateDelivered })
}

// seedMember puts name with node id into n's member table, as gossip would.
func seedMember(n *Node, name, id string, ver int64) {
	n.mergeMembers([]Member{{Name: name, ID: id, Ver: ver}})
}

func TestProjectEnvelopeValidation(t *testing.T) {
	p := newTestProject(t)
	a := newProjectNode(t, "a", p, listen(t), nil)
	bID := newID()
	seedMember(a.Node, "b", bID, 1)
	parts := []string{"a", "b"}
	valid := func() Message {
		ids := []string{a.ID(), bID}
		return Message{ID: newID(), From: "b", ChatID: ProjectChatID(p.id, parts, ids, 0), Participants: parts, ParticipantIDs: ids}
	}
	if m := valid(); !a.validChatEnvelope("b", bID, &m) {
		t.Fatal("valid envelope rejected")
	}
	for name, change := range map[string]func(m *Message){
		"no ids":     func(m *Message) { m.ParticipantIDs = nil },
		"misaligned": func(m *Message) { m.ParticipantIDs = []string{bID, a.ID()} },
		"short":      func(m *Message) { m.ParticipantIDs = m.ParticipantIDs[:1] },
		"invalid id": func(m *Message) { m.ParticipantIDs = []string{a.ID(), "x"} },
		"sender not pinned to its session": func(m *Message) {
			m.ParticipantIDs = []string{a.ID(), newID()}
			m.ChatID = ProjectChatID(p.id, parts, m.ParticipantIDs, 0)
		},
		"self not pinned": func(m *Message) {
			m.ParticipantIDs = []string{newID(), bID}
			m.ChatID = ProjectChatID(p.id, parts, m.ParticipantIDs, 0)
		},
		"area":           func(m *Message) { m.Area = "dev" },
		"legacy chat id": func(m *Message) { m.ChatID = KeyedChatID("", parts, 0) },
		"unknown mode":   func(m *Message) { m.ChatMode = "other" },
		"other project":  func(m *Message) { m.ChatID = ProjectChatID(newTestProject(t).id, parts, m.ParticipantIDs, 0) },
	} {
		m := valid()
		change(&m)
		if a.validChatEnvelope("b", bID, &m) {
			t.Errorf("%s: accepted", name)
		}
	}
	legacy := newTestNode(t, "a", testSecret, nil, t.TempDir(), listen(t), nil)
	m := Message{ID: newID(), From: "b", ChatID: KeyedChatID("", parts, 0), Participants: parts}
	if !legacy.validChatEnvelope("b", "", &m) {
		t.Fatal("legacy envelope rejected")
	}
	m.ParticipantIDs = []string{newID(), newID()}
	if legacy.validChatEnvelope("b", "", &m) {
		t.Fatal("legacy node took participant ids")
	}
}

// When a member's record moves to another node id (a re-join), its queue
// moves to dropped/, nothing more is queued for it in the chats pinned to the
// old id (delivery "left"), and a new chat pins the new id.
func TestProjectFanoutStopsAfterRejoin(t *testing.T) {
	p := newTestProject(t)
	a := newProjectNode(t, "a", p, listen(t), nil)
	oldID, newIDb := newID(), newID()
	seedMember(a.Node, "b", oldID, 1)
	c, err := a.EnsureOpenChat([]string{"a", "b"}, "")
	if err != nil {
		t.Fatal(err)
	}
	first, err := a.SendChat(ChatSend{ChatID: c.ID, Body: "one", Ask: []string{"b"}})
	if err != nil {
		t.Fatal(err)
	}
	if a.store.delivery("b", first.ID) != "queued" {
		t.Fatal("first message not queued for b")
	}
	seedMember(a.Node, "b", newIDb, 2)
	if _, err := os.Stat(filepath.Join(a.cfg.DataDir, "dropped", "b-"+oldID, first.ID+".json")); err != nil {
		t.Fatalf("old queue not in dropped/: %v", err)
	}
	second, err := a.SendChat(ChatSend{ChatID: c.ID, Body: "two", Ask: []string{"b"}})
	if err != nil {
		t.Fatal(err)
	}
	if msgs, _ := a.store.pending("b"); len(msgs) != 0 {
		t.Fatalf("queued for the new incarnation: %d", len(msgs))
	}
	for _, id := range []string{first.ID, second.ID} {
		if st := deliveryOf(a, c.ID, id, "b"); st != StateLeft {
			t.Fatalf("delivery of %s = %q, want left", id, st)
		}
	}
	next, err := a.EnsureOpenChat([]string{"a", "b"}, "")
	if err != nil || next.ID == c.ID || !slices.Equal(next.ParticipantIDs, []string{a.ID(), newIDb}) {
		t.Fatalf("new chat %+v, %v", next, err)
	}
	if err := a.repairChats(); err != nil {
		t.Fatal(err)
	}
	if msgs, _ := a.store.pending("b"); !slices.ContainsFunc(msgs, func(m Message) bool { return m.ChatID == next.ID }) ||
		slices.ContainsFunc(msgs, func(m Message) bool { return m.ChatID == c.ID }) {
		t.Fatal("repair queued the old chat or missed the new one")
	}
	if _, err := a.EnsureOpenChat([]string{"a", "zed"}, ""); !errors.Is(err, ErrUnknownPeer) {
		t.Fatalf("unknown member: %v", err)
	}
}
