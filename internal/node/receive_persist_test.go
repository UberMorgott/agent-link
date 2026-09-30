package node

import (
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// A peer's chat message this node cannot store (a disk error) is not ACKed:
// the peer resends it, and once the disk works it is stored. A reply to a
// seat's message is not left pending for the seat without its record.
func TestReceiveChatPersistFailureNotAcked(t *testing.T) {
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
	chat, err := a.NewProjectChat([]string{"b"})
	if err != nil {
		t.Fatal(err)
	}
	q, err := a.SendRequest(SendRequest{ChatID: chat.ID, Body: "b, which port?", Ask: []string{"b"}, Seat: codex.ID})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "b has the question", func() bool { return slices.Contains(chatIDs(b, chat.ID), q.ID) })
	// A file where the messages directory was: storing a message fails.
	dir := filepath.Join(a.chats.chatDir(chat.ID), "messages")
	if err := os.Rename(dir, dir+".away"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ans, err := b.SendChat(ChatSend{ChatID: chat.ID, Body: "8080", ReplyTo: q.ID, Ask: []string{"a"}})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * b.resendAfter)
	if _, ok := a.chats.message(ans.ID); ok {
		t.Fatal("stored despite the disk error")
	}
	if slices.Contains(pendingIDs(seatByLabel(t, a, "Codex")), ans.ID) {
		t.Fatal("the seat keeps a message that was not stored")
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dir+".away", dir); err != nil {
		t.Fatal(err)
	}
	eventually(t, "resent and stored", func() bool { _, ok := a.chats.message(ans.ID); return ok })
	eventually(t, "the seat has the reply", func() bool { return slices.Contains(pendingIDs(seatByLabel(t, a, "Codex")), ans.ID) })
}

// A seat's pending message whose record is gone is dropped once it is past
// orphanGrace; a message just queued (its store follows) is kept.
func TestSeatsDueDropsPendingWithoutRecord(t *testing.T) {
	l := &seatLauncher{}
	a := seatNode(t, t.TempDir(), t.TempDir(), l)
	_, codex := addSeats(t, a)
	now := time.Now()
	old, fresh := newID(), newID()
	st := a.seats
	st.mu.Lock()
	s := st.getLocked(codex.ID)
	s.Pending = append(s.Pending, SeatPending{ID: old, Ask: true, At: now.Add(-2 * orphanGrace)}, SeatPending{ID: fresh, Ask: true, At: now})
	st.mu.Unlock()
	a.dropOrphanPending(now)
	if got := pendingIDs(seatByLabel(t, a, "Codex")); !slices.Equal(got, []string{fresh}) {
		t.Fatalf("pending %v, want only the fresh one", got)
	}
}
