package node

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// histNode is an unstarted node "a" keeping days of history and its keep
// newest, with a chat with "b".
func histNode(t *testing.T, dataDir string, days, keep int) (*testNode, Chat) {
	t.Helper()
	a := newTestNode(t, "a", testSecret, nil, dataDir, listen(t), nil)
	a.cfg.HistoryDays, a.cfg.HistoryKeep = days, keep
	c := Chat{ID: newID(), Participants: []string{"a", "b"}, CreatedAt: time.Now().UTC()}
	if _, err := a.chats.ensure(c); err != nil {
		t.Fatal(err)
	}
	return a, c
}

// histPut stores a read message of from in chat c, received age ago.
func histPut(t *testing.T, a *testNode, c Chat, from string, age time.Duration) Message {
	t.Helper()
	m := Message{ID: newID(), From: from, To: "a", ChatID: c.ID, Body: "hello", CreatedAt: time.Now().UTC()}
	if _, _, _, err := a.chats.put(m, false); err != nil {
		t.Fatal(err)
	}
	a.chats.mu.Lock()
	st := a.chats.chats[c.ID]
	st.msgs[st.index[m.ID]].ReceivedAt = time.Now().Add(-age)
	a.chats.mu.Unlock()
	return m
}

// kept lists which of msgs the chat store still has.
func kept(a *testNode, msgs ...Message) []bool {
	var out []bool
	for _, m := range msgs {
		_, ok := a.chats.message(m.ID)
		out = append(out, ok)
	}
	return out
}

const histOld = 100 * 24 * time.Hour

// A message older than the retention goes, with its file; a newer one stays.
func TestHistoryPrunesOld(t *testing.T) {
	a, c := histNode(t, t.TempDir(), 90, 1)
	m1, m2 := histPut(t, a, c, "b", histOld), histPut(t, a, c, "b", histOld)
	m3 := histPut(t, a, c, "b", time.Hour)
	a.pruneHistory(time.Now())
	if got := kept(a, m1, m2, m3); !slices.Equal(got, []bool{false, false, true}) {
		t.Fatalf("kept %v", got)
	}
	if _, err := os.Stat(filepath.Join(a.chats.chatDir(c.ID), "messages", m1.ID+".json")); !os.IsNotExist(err) {
		t.Fatalf("file of a pruned message: %v", err)
	}
	// What is left loads again.
	cs, err := openChatStore(a.cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := cs.snapshot(c.ID); len(s.msgs) != 1 || s.msgs[0].Message.ID != m3.ID {
		t.Fatalf("reopened %+v", s.msgs)
	}
}

// A chat's newest HistoryKeep messages stay however histOld they are.
func TestHistoryKeepsNewest(t *testing.T) {
	a, c := histNode(t, t.TempDir(), 90, 3)
	var msgs []Message
	for range 5 {
		msgs = append(msgs, histPut(t, a, c, "b", histOld))
	}
	a.pruneHistory(time.Now())
	if got := kept(a, msgs...); !slices.Equal(got, []bool{false, false, true, true, true}) {
		t.Fatalf("kept %v", got)
	}
	// The next message numbers on from the newest.
	next := histPut(t, a, c, "b", 0)
	if r, _ := a.chats.message(next.ID); r.Seq != 6 {
		t.Fatalf("next seq %d", r.Seq)
	}
}

// HistoryDays 0 keeps everything.
func TestHistoryOff(t *testing.T) {
	a, c := histNode(t, t.TempDir(), 0, 0)
	m := histPut(t, a, c, "b", 10*histOld)
	histPut(t, a, c, "b", 0)
	a.pruneHistory(time.Now())
	if got := kept(a, m); !got[0] {
		t.Fatal("pruned with retention off")
	}
}

// A message that still waits for someone is never pruned: unread, pending
// for a seat, leased, failed (needs a person), held by a claim, or
// this node's own message still queued for a peer.
func TestHistoryNeverPrunesWaiting(t *testing.T) {
	a, c := histNode(t, t.TempDir(), 90, 0)
	unread := Message{ID: newID(), From: "b", ChatID: c.ID, Body: "unread"}
	if _, _, _, err := a.chats.put(unread, true); err != nil {
		t.Fatal(err)
	}
	a.chats.mu.Lock()
	st := a.chats.chats[c.ID]
	st.msgs[st.index[unread.ID]].ReceivedAt = time.Now().Add(-histOld)
	a.chats.mu.Unlock()
	pending, leased, failed := histPut(t, a, c, "b", histOld), histPut(t, a, c, "b", histOld), histPut(t, a, c, "b", histOld)
	claimed, queued := histPut(t, a, c, "b", histOld), histPut(t, a, c, "a", histOld)
	plain := histPut(t, a, c, "b", histOld)
	histPut(t, a, c, "b", 0) // the newest

	a.seats.mu.Lock()
	a.seats.seats = append(a.seats.seats, &Seat{ID: "seat-1", Provider: ProviderCodex, Pending: []SeatPending{{ID: pending.ID, Ask: true}}})
	a.seats.mu.Unlock()
	now := time.Now()
	if ok, err := a.leases.take(leased.ID, "", "s-1", ViaInbox, "tok", now.Add(time.Minute), now); !ok || err != nil {
		t.Fatalf("lease %v %v", ok, err)
	}
	a.leases.mu.Lock()
	a.leases.m[failed.ID] = &Lease{ID: failed.ID, State: LeaseFailed, Failed: true, At: now}
	a.leases.mu.Unlock()
	a.sess.claimMu.Lock()
	a.sess.claims[claimed.ID] = hold{owner: "s-1", kind: holdAck, at: now}
	a.sess.claimMu.Unlock()
	cp := queued
	cp.To = "b"
	if err := a.store.enqueue("b", cp); err != nil {
		t.Fatal(err)
	}

	a.pruneHistory(time.Now())
	if got := kept(a, pending, leased, failed, claimed, queued, plain); !slices.Equal(got, []bool{true, true, true, true, true, false}) {
		t.Fatalf("kept %v", got)
	}
	if got := kept(a, unread); !got[0] {
		t.Fatal("an unread message was pruned")
	}
}

// A read message that a person must still look at (orphaned) stays.
func TestHistoryKeepsOrphaned(t *testing.T) {
	a, c := histNode(t, t.TempDir(), 90, 0)
	m := Message{ID: newID(), From: "b", ChatID: c.ID, Body: "for a gone session"}
	if _, _, _, err := a.chats.put(m, true); err != nil {
		t.Fatal(err)
	}
	if _, err := a.chats.markOrphaned([]string{m.ID}, "s-gone"); err != nil {
		t.Fatal(err)
	}
	a.chats.mu.Lock()
	st := a.chats.chats[c.ID]
	r := &st.msgs[st.index[m.ID]]
	r.ReceivedAt, r.ReadAt = time.Now().Add(-histOld), time.Now() // read, still marked orphaned
	a.chats.mu.Unlock()
	histPut(t, a, c, "b", 0)
	a.pruneHistory(time.Now())
	if got := kept(a, m); !got[0] {
		t.Fatal("an orphaned message was pruned")
	}
}

// The inbox records and sent copies of what the chat store no longer has go
// by age; an unread plain message stays, and so do the copies of a kept
// message.
func TestHistoryPrunesCopies(t *testing.T) {
	a, c := histNode(t, t.TempDir(), 90, 0)
	gone := histPut(t, a, c, "b", histOld)
	keep := histPut(t, a, c, "b", 0)
	for _, m := range []Message{gone, keep} {
		if _, err := a.store.saveInbound(m); err != nil {
			t.Fatal(err)
		}
	}
	plain := Message{ID: newID(), From: "b", To: "a", Body: "plain, unread"}
	status := Message{ID: newID(), From: "b", To: "a", ChatID: c.ID, Kind: KindStatus, ReplyTo: keep.ID}
	for _, m := range []Message{plain, status} {
		if _, err := a.store.saveInbound(m); err != nil {
			t.Fatal(err)
		}
	}
	a.store.mu.Lock()
	for _, id := range []string{gone.ID, keep.ID, plain.ID, status.ID} {
		a.store.inbox[id].ReceivedAt = time.Now().Add(-histOld)
	}
	a.store.mu.Unlock()
	sentDir := filepath.Join(a.cfg.DataDir, "sent", "b")
	if err := os.MkdirAll(sentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	control := Message{ID: newID(), From: "a", To: "b", ChatID: c.ID, Kind: KindReceipt}
	for _, m := range []Message{control, keep} {
		p := filepath.Join(sentDir, m.ID+".json")
		if err := writeJSON(p, m); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, time.Now().Add(-histOld), time.Now().Add(-histOld)); err != nil {
			t.Fatal(err)
		}
	}

	a.pruneHistory(time.Now())
	a.store.mu.Lock()
	_, hasGone := a.store.inbox[gone.ID]
	_, hasKeep := a.store.inbox[keep.ID]
	_, hasPlain := a.store.inbox[plain.ID]
	_, hasStatus := a.store.inbox[status.ID]
	a.store.mu.Unlock()
	if hasGone || !hasKeep || !hasPlain || hasStatus {
		t.Fatalf("inbox gone %v keep %v plain %v status %v", hasGone, hasKeep, hasPlain, hasStatus)
	}
	if _, err := os.Stat(a.store.inboxPath(gone.ID)); !os.IsNotExist(err) {
		t.Fatalf("inbox file of a pruned message: %v", err)
	}
	if a.store.delivery("b", control.ID) != "" || a.store.delivery("b", keep.ID) != "sent" {
		t.Fatal("sent copies: the control message's must go, the kept message's stay")
	}
}

// The node prunes once at start (and on the slow GC tick after).
func TestHistoryPrunedAtStart(t *testing.T) {
	data := t.TempDir()
	a, c := histNode(t, data, 0, 0)
	m := histPut(t, a, c, "b", 0)
	histPut(t, a, c, "b", 0)
	a.chats.mu.Lock()
	st := a.chats.chats[c.ID]
	r := st.msgs[st.index[m.ID]]
	a.chats.mu.Unlock()
	r.ReceivedAt = time.Now().Add(-histOld)
	if err := writeJSON(filepath.Join(a.chats.chatDir(c.ID), "messages", m.ID+".json"), r); err != nil {
		t.Fatal(err)
	}
	b := newTestNode(t, "a", testSecret, nil, data, listen(t), nil)
	b.cfg.HistoryDays, b.cfg.HistoryKeep = 90, 1
	b.start(t)
	eventually(t, "pruned at start", func() bool { return !kept(b, m)[0] })
}

// A request a held status waits on (a person decides) stays, and a dropped
// reply leaves the reply index as a reopen rebuilds it.
func TestHistoryKeepsHeldAndReindexesReplies(t *testing.T) {
	a, c := histNode(t, t.TempDir(), 90, 0)
	held := histPut(t, a, c, "a", histOld)
	asked := histPut(t, a, c, "a", histOld)
	reply := Message{ID: newID(), From: "b", ChatID: c.ID, ReplyTo: asked.ID, Body: "answer", CreatedAt: time.Now().UTC()}
	if _, _, _, err := a.chats.put(reply, false); err != nil {
		t.Fatal(err)
	}
	histPut(t, a, c, "b", 0)
	a.chats.mu.Lock()
	st := a.chats.chats[c.ID]
	st.msgs[st.index[reply.ID]].ReceivedAt = time.Now().Add(-histOld)
	st.jobs["b/"+held.ID] = Message{ID: newID(), From: "b", ChatID: c.ID, ReplyTo: held.ID, Kind: KindStatus, JobStatus: JobHeld}
	a.chats.mu.Unlock()
	if by := a.chats.repliedBy(asked.ID); len(by) != 1 {
		t.Fatalf("replied before %v", by)
	}
	a.pruneHistory(time.Now())
	if got := kept(a, held, asked, reply); !slices.Equal(got, []bool{true, false, false}) {
		t.Fatalf("kept %v", got)
	}
	if by := a.chats.repliedBy(asked.ID); len(by) != 0 {
		t.Fatalf("replied after %v", by)
	}
}

// An own message with a copy still queued for a peer stays, even when an
// earlier copy was sent already.
func TestHistoryKeepsQueuedBesideSent(t *testing.T) {
	a, c := histNode(t, t.TempDir(), 90, 0)
	m := histPut(t, a, c, "a", histOld)
	histPut(t, a, c, "b", 0)
	cp := m
	cp.To = "b"
	if err := a.store.enqueue("b", cp); err != nil {
		t.Fatal(err)
	}
	sent := filepath.Join(a.cfg.DataDir, "sent", "b")
	if err := os.MkdirAll(sent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(sent, m.ID+".json"), slimMessage(cp)); err != nil {
		t.Fatal(err)
	}
	a.pruneHistory(time.Now())
	if got := kept(a, m); !got[0] {
		t.Fatal("a message still queued for a peer was pruned")
	}
}
