package node

import (
	"context"
	"net"
	"slices"
	"sync"
	"testing"
	"time"
)

// abandonNode is a seat node of project p (data in data, folder dir) whose
// Codex threads w wakes.
func abandonNode(t *testing.T, p testProject, data, dir string, w *fakeWaker) *testNode {
	t.Helper()
	l := &seatLauncher{}
	a := newProjectNodeAt(t, "morgott", p, data, listen(t), nil)
	a.SetFolders(dir, nil)
	a.SetLauncher(l, ProviderClaude)
	a.wakeEvery = time.Hour // the tests run seatsDue and wakeIdle themselves
	a.seatBusy = l.busy
	if w != nil {
		w.note = pointerFor(a.Node)
		a.SetSessionWaker(w)
	}
	a.start(t)
	return a
}

// sessionAsk is a question of session sid to seat in chat.
func sessionAsk(t *testing.T, n *testNode, chat, dir, sid, seat, body string) Message {
	t.Helper()
	m, err := n.SendRequest(SendRequest{ChatID: chat, Body: body, Folder: dir, SessionID: sid, AskSeats: []string{seat}, AuthorKind: AuthorAgent})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// seatReply posts seat's reply to ask.
func seatReply(t *testing.T, n *testNode, ask Message, seat, body string) Message {
	t.Helper()
	m, err := n.SendRequest(SendRequest{ChatID: ask.ChatID, ReplyTo: ask.ID, Body: body, Seat: seat, AuthorKind: AuthorAgent})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// A reply to a question whose discuss wait timed out (AbandonAsk) stays in
// history only: unread for no session, so no hook, no Codex wake and no
// orphan hand-over takes it; the mark survives a restart. A question nobody
// abandoned (async) is delivered and wakes its idle Codex thread; one whose
// asker waits again (ResumeAsk) is delivered too.
func TestAbandonedAskReplyReachesNoOne(t *testing.T) {
	w := &fakeWaker{ready: true}
	p, data, dir, home := newTestProject(t), t.TempDir(), t.TempDir(), t.TempDir()
	a := abandonNode(t, p, data, dir, w)
	_, codex := addSeats(t, a)
	if _, err := a.RegisterSession(SessionRequest{SessionID: "s-q", Provider: ProviderCodex, Folder: dir, Wake: WakeQueue,
		Idle: true, CodexHome: home}); err != nil {
		t.Fatal(err)
	}
	a.syncQueueWake()
	chat, err := a.NewProjectChat(nil)
	if err != nil {
		t.Fatal(err)
	}
	ask := sessionAsk(t, a, chat.ID, dir, "s-q", codex.ID, "timed-out question")
	if answered, err := a.AbandonAsk(chat.ID, ask.ID, codex.ID); err != nil || answered {
		t.Fatalf("abandon: %v %v", answered, err)
	}
	late := seatReply(t, a, ask, codex.ID, "late answer")
	if p, _ := a.UnreadFor("", "s-q", "", 10); p.Total != 0 || len(p.Woken) != 0 {
		t.Fatalf("a late reply reached the asking session: %+v", p)
	}
	a.wakeIdle(context.Background())
	if w.count() != 0 {
		t.Fatalf("a late reply woke the Codex thread: %v", w.calls)
	}
	// The session gone (a day later): nothing waits for it to hand over.
	a.orphanReplies("s-q", time.Now().Add(24*time.Hour))
	if rec, ok := a.chats.message(late.ID); !ok || rec.Unread || rec.Orphaned != "" {
		t.Fatalf("late reply not in history only: %+v %v", rec, ok)
	}

	// The mark is saved: a reply after a restart reaches no one either.
	ask2 := sessionAsk(t, a, chat.ID, dir, "s-q", codex.ID, "second question")
	if answered, err := a.AbandonAsk(chat.ID, ask2.ID, codex.ID); err != nil || answered {
		t.Fatalf("abandon: %v %v", answered, err)
	}
	a.stop()
	w2 := &fakeWaker{ready: true}
	b := abandonNode(t, p, data, dir, w2)
	if _, err := b.RegisterSession(SessionRequest{SessionID: "s-q", Provider: ProviderCodex, Folder: dir, Wake: WakeQueue,
		Idle: true, CodexHome: home}); err != nil {
		t.Fatal(err)
	}
	b.syncQueueWake()
	after := seatReply(t, b, ask2, codex.ID, "after restart")
	if rec, ok := b.chats.message(after.ID); !ok || rec.Unread {
		t.Fatalf("a reply after the restart is unread: %+v %v", rec, ok)
	}
	b.wakeIdle(context.Background())
	if w2.count() != 0 {
		t.Fatalf("a late reply woke the thread after a restart: %v", w2.calls)
	}

	// Asked async (never waited for): delivered, and it wakes the thread.
	async := sessionAsk(t, b, chat.ID, dir, "s-q", codex.ID, "async question")
	reply := seatReply(t, b, async, codex.ID, "async answer")
	if p, _ := b.UnreadFor("", "s-q", "", 10); p.Total != 1 || p.Messages[0].ID != reply.ID {
		t.Fatalf("an async ask's reply not delivered: %+v", p)
	}
	b.wakeIdle(context.Background())
	if w2.count() != 1 {
		t.Fatalf("an async ask's reply did not wake the thread: %v", w2.calls)
	}

	// Waited for again: delivered as usual.
	again := sessionAsk(t, b, chat.ID, dir, "s-q", codex.ID, "waited again")
	if answered, err := b.AbandonAsk(chat.ID, again.ID, codex.ID); err != nil || answered {
		t.Fatalf("abandon: %v %v", answered, err)
	}
	if err := b.ResumeAsk(again.ID); err != nil {
		t.Fatal(err)
	}
	resumed := seatReply(t, b, again, codex.ID, "resumed answer")
	if rec, _ := b.chats.message(resumed.ID); !rec.Unread {
		t.Fatalf("a resumed ask's reply is not delivered: %+v", rec)
	}
}

// A peer's seat reply to a question a seat of this node abandoned is queued
// for that seat no more (seatsForIncoming) and stored unread for no one; one
// to a question still waited for goes to the seat as before.
func TestAbandonedAskPeerReplyReachesNoSeat(t *testing.T) {
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
	peerSeat := &AgentRef{Seat: "seat-b", Label: "Codex", Provider: ProviderCodex}
	ask := func(body string) Message {
		t.Helper()
		q, err := a.SendRequest(SendRequest{ChatID: chat.ID, Body: body, Ask: []string{"b"}, Seat: codex.ID})
		if err != nil {
			t.Fatal(err)
		}
		eventually(t, "b has the question", func() bool { return slices.Contains(chatIDs(b, chat.ID), q.ID) })
		return q
	}
	reply := func(q Message, body string) Message {
		t.Helper()
		m, err := b.SendMessage(Message{ChatID: chat.ID, Body: body, ReplyTo: q.ID, Agent: peerSeat, RootID: q.ID, AutoDepth: 1})
		if err != nil {
			t.Fatal(err)
		}
		eventually(t, "a has the reply", func() bool { _, ok := a.chats.message(m.ID); return ok })
		return m
	}

	waited := reply(ask("still waited for"), "on time")
	if !slices.Contains(pendingIDs(seatByLabel(t, a, "Codex")), waited.ID) {
		t.Fatalf("a waited-for question's reply not queued for its seat: %+v", seatByLabel(t, a, "Codex").Pending)
	}

	q := ask("abandoned")
	if answered, err := a.AbandonAsk(chat.ID, q.ID, peerSeat.Seat); err != nil || answered {
		t.Fatalf("abandon: %v %v", answered, err)
	}
	late := reply(q, "late")
	if slices.Contains(pendingIDs(seatByLabel(t, a, "Codex")), late.ID) {
		t.Fatalf("an abandoned question's peer reply queued for the asking seat: %+v", seatByLabel(t, a, "Codex").Pending)
	}
	if rec, _ := a.chats.message(late.ID); rec.Unread || rec.Assigned != "" {
		t.Fatalf("an abandoned question's peer reply pushed: %+v", rec)
	}
}

// A timeout races the reply: either the reply was stored first and the wait
// hands it over (answered, delivered as usual), or the question is abandoned
// and the reply stored after it reaches no one. Never both, never neither.
func TestAbandonAskRacesReply(t *testing.T) {
	dir := t.TempDir()
	a := abandonNode(t, newTestProject(t), t.TempDir(), dir, nil)
	_, codex := addSeats(t, a)
	if _, err := a.RegisterSession(SessionRequest{SessionID: "s-r", Provider: ProviderClaude, Folder: dir}); err != nil {
		t.Fatal(err)
	}
	chat, err := a.NewProjectChat(nil)
	if err != nil {
		t.Fatal(err)
	}
	for range 40 {
		ask := sessionAsk(t, a, chat.ID, dir, "s-r", codex.ID, "racing question")
		var (
			wg       sync.WaitGroup
			answered bool
			abErr    error
			reply    Message
		)
		wg.Go(func() { answered, abErr = a.AbandonAsk(chat.ID, ask.ID, codex.ID) })
		wg.Go(func() {
			m, err := a.SendRequest(SendRequest{ChatID: chat.ID, ReplyTo: ask.ID, Body: "racing answer", Seat: codex.ID, AuthorKind: AuthorAgent})
			if err != nil {
				t.Error(err)
			}
			reply = m
		})
		wg.Wait()
		if abErr != nil {
			t.Fatal(abErr)
		}
		rec, ok := a.chats.message(reply.ID)
		if !ok || rec.Unread != answered || a.chats.abandoned(ask.ID) == answered {
			t.Fatalf("answered %v, reply unread %v, abandoned %v", answered, rec.Unread, a.chats.abandoned(ask.ID))
		}
	}
}
