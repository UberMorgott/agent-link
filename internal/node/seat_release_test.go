package node

import (
	"errors"
	"net"
	"os"
	"slices"
	"testing"
	"time"
)

// seatReplied is a project node a whose Codex seat asked peer b, and b's reply
// assigned to that seat.
type seatReplied struct {
	p        testProject
	a, b     *testNode
	codex    SeatView
	chat     string
	q, reply Message
}

func newSeatReplied(t *testing.T) seatReplied {
	t.Helper()
	p := newTestProject(t)
	lnA, lnB := listen(t), listen(t)
	l := &seatLauncher{}
	a := newProjectNode(t, "a", p, lnA, map[string]net.Listener{"b": lnB})
	a.SetFolders(t.TempDir(), nil)
	a.SetLauncher(l, ProviderClaude)
	a.wakeEvery, a.seatBusy = time.Hour, l.busy
	b := newProjectNode(t, "b", p, lnB, nil)
	a.start(t)
	b.start(t)
	eventually(t, "a<->b connected", func() bool { return a.Connected("b") && b.Connected("a") })
	_, codex := addSeats(t, a)
	l.open.Store(true) // no turn runs: the reply stays pending for the seat
	chat, err := a.NewProjectChat([]string{"b"})
	if err != nil {
		t.Fatal(err)
	}
	q, err := a.SendRequest(SendRequest{ChatID: chat.ID, Body: "b, which port?", Ask: []string{"b"}, Seat: codex.ID})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "b has the question", func() bool { return slices.Contains(chatIDs(b, chat.ID), q.ID) })
	ans, err := b.SendChat(ChatSend{ChatID: chat.ID, Body: "8080", ReplyTo: q.ID, Ask: []string{"a"}})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the seat has the reply", func() bool { return slices.Contains(pendingIDs(seatByLabel(t, a, "Codex")), ans.ID) })
	return seatReplied{p: p, a: a, b: b, codex: codex, chat: chat.ID, q: q, reply: ans}
}

func unreadIDs(t *testing.T, n *testNode) []string {
	t.Helper()
	u, err := n.Unread("", "", 50)
	if err != nil {
		t.Fatal(err)
	}
	return ids(u.Messages)
}

// A removed seat's unanswered messages go back to the node's sessions: unread
// again, no seat assigned, no lease of the seat left.
func TestRemoveSeatReturnsPendingToRouting(t *testing.T) {
	s := newSeatReplied(t)
	a := s.a
	now := time.Now()
	if _, err := a.leases.take(s.reply.ID, s.codex.ID, seatOwner(s.codex.ID), ViaSeat, "", now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(unreadIDs(t, a), s.reply.ID) {
		t.Fatal("the seat's message is unread for every session")
	}
	if err := a.RemoveSeat(s.codex.ID); err != nil {
		t.Fatal(err)
	}
	rec, _ := a.chats.message(s.reply.ID)
	if rec.Assigned != "" || !rec.ReadAt.IsZero() || !rec.Unread {
		t.Fatalf("record after the seat left %+v", rec)
	}
	if !slices.Contains(unreadIDs(t, a), s.reply.ID) {
		t.Fatal("the removed seat's message is not back for the sessions")
	}
	if _, ok := a.leases.get(leaseKey(s.codex.ID, s.reply.ID)); ok {
		t.Fatal("the removed seat's lease is kept")
	}
}

// A network project (DisableSeats) keeps no seat: at start its leftover seats
// go and their messages return to the sessions; a reply to an old seat's
// message is the sessions' too, and no seat can be added or started.
func TestDisableSeatsDropsLeftoverSeats(t *testing.T) {
	s := newSeatReplied(t)
	s.a.stop()
	cfg := s.a.cfg
	cfg.DisableSeats, cfg.Listen = true, listen(t).Addr().String()
	n, err := New(cfg, s.p.key, nil)
	if err != nil {
		t.Fatal(err)
	}
	a := &testNode{Node: n}
	if got := a.Seats(); len(got) != 0 {
		t.Fatalf("seats kept %+v", got)
	}
	if !slices.Contains(unreadIDs(t, a), s.reply.ID) {
		t.Fatal("the old seat's message is not back for the sessions")
	}
	later := Message{ID: newID(), From: "b", ChatID: s.chat, ReplyTo: s.q.ID, Body: "and 8081", CreatedAt: time.Now().UTC()}
	c, _ := a.chats.get(s.chat)
	later.Participants, later.ParticipantIDs, later.ChatGen, later.ChatMode = c.Participants, c.ParticipantIDs, c.Gen, c.Mode
	if v := a.receiveChat("b", s.b.ID(), later); v != chatStored {
		t.Fatalf("reply rejected: %v", v)
	}
	if !slices.Contains(unreadIDs(t, a), later.ID) {
		t.Fatal("a reply to an old seat's message is not the sessions'")
	}
	before, err := os.ReadFile(a.seats.path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.AddSeat(SeatRequest{Provider: ProviderCodex}); !errors.Is(err, ErrSeatsDisabled) {
		t.Fatalf("add: %v", err)
	}
	if _, err := a.StartSeat(s.codex.ID, false); !errors.Is(err, ErrSeatsDisabled) {
		t.Fatalf("start: %v", err)
	}
	if after, _ := os.ReadFile(a.seats.path); string(after) != string(before) {
		t.Fatal("a refused seat changed seats.json")
	}
}
