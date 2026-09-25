package node

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

// fakeWaker records the wakes a node queues.
type fakeWaker struct {
	mu    sync.Mutex
	ready bool
	err   error
	calls []string // "home|session|text"
}

func (w *fakeWaker) Check(context.Context, bool) {}

func (w *fakeWaker) Ready() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ready
}

func (w *fakeWaker) Wake(_ context.Context, home, session, text string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls = append(w.calls, home+"|"+session+"|"+text)
	return w.err
}

func (w *fakeWaker) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.calls)
}

// wakePair is pair with w as a's waker; the test drives the wakes itself.
func wakePair(t *testing.T, w SessionWaker) (*testNode, *testNode) {
	t.Helper()
	lnA, lnB := listen(t), listen(t)
	a := newTestNode(t, "a", testSecret, nil, t.TempDir(), lnA, map[string]net.Listener{"b": lnB})
	b := newTestNode(t, "b", testSecret, []string{"dev"}, t.TempDir(), lnB, map[string]net.Listener{"a": lnA})
	a.SetSessionWaker(w)
	a.wakeEvery = time.Hour
	a.start(t)
	b.start(t)
	return a, b
}

// An idle WakeQueue session with unread messages is woken once per idle
// period; a busy one is not; without a ready waker, or after a failed wake,
// it reads at its next event.
func TestWakeIdleQueueSession(t *testing.T) {
	w := &fakeWaker{}
	a, b := wakePair(t, w)
	dir, home := t.TempDir(), t.TempDir()
	reg := func(idle bool) Session {
		t.Helper()
		s, err := a.RegisterSession(SessionRequest{SessionID: "s-q", Provider: "codex", Folder: dir, Wake: WakeQueue, Idle: idle, CodexHome: home})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	wake := func() Session {
		t.Helper()
		a.syncQueueWake()
		a.wakeIdle(context.Background())
		for _, s := range a.Sessions() {
			if s.SessionID == "s-q" {
				return s
			}
		}
		t.Fatal("session gone")
		return Session{}
	}
	if s := reg(false); s.Wake != WakeNextEvent || s.Asked != WakeQueue {
		t.Fatalf("no waker ready: %+v", s)
	}
	w.mu.Lock()
	w.ready = true
	w.mu.Unlock()
	if s := wake(); s.Wake != WakeQueue {
		t.Fatalf("waker ready: %+v", s)
	}
	if _, err := b.SendRequest(SendRequest{To: "a", Body: "wake up", AuthorKind: AuthorAgent}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a has the request", func() bool { p, _ := a.Unread("", "", 10); return p.Total == 1 })

	wake()
	if w.count() != 0 {
		t.Fatalf("a busy session was woken: %v", w.calls)
	}
	reg(true)
	if s := wake(); !s.Woken || w.count() != 1 || w.calls[0] != home+"|s-q|agent-link: 1 новое сообщение — прочитай их" {
		t.Fatalf("idle session: %+v %v", s, w.calls)
	}
	reg(true) // a heartbeat of the same idle period
	wake()
	if w.count() != 1 {
		t.Fatalf("woken twice in one idle period: %v", w.calls)
	}
	reg(false) // its next turn (the hooks deliver), then idle again
	reg(true)
	wake()
	if w.count() != 2 {
		t.Fatalf("new idle period not woken: %v", w.calls)
	}

	// A failed wake falls back to the next event.
	w.mu.Lock()
	w.err = errors.New("boom")
	w.mu.Unlock()
	reg(false)
	reg(true)
	a.wakeIdle(context.Background())
	for _, s := range a.Sessions() {
		if s.SessionID == "s-q" && s.Wake != WakeNextEvent {
			t.Fatalf("after a failed wake: %+v", s)
		}
	}

	// An ended session is never woken.
	w.mu.Lock()
	w.err = nil
	w.mu.Unlock()
	reg(false)
	reg(true)
	if err := a.EndSession("s-q"); err != nil {
		t.Fatal(err)
	}
	n := w.count()
	a.syncQueueWake()
	a.wakeIdle(context.Background())
	if w.count() != n {
		t.Fatalf("an ended session was woken: %v", w.calls)
	}
}

func TestWakeIgnoresGuardedMessage(t *testing.T) {
	w := &fakeWaker{ready: true}
	a, b := wakePair(t, w)
	chat, err := b.CreateChat([]string{"a"}, "")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "a knows chat", func() bool { _, ok := a.ChatOf(chat.ID); return ok })
	if _, err := a.RegisterSession(SessionRequest{SessionID: "s-q", Provider: "codex", Folder: t.TempDir(), Wake: WakeQueue, Idle: true}); err != nil {
		t.Fatal(err)
	}
	deep, err := b.SendMessage(Message{ChatID: chat.ID, Body: "guarded", Responders: []string{"a"}, RootID: newID(), AutoDepth: MaxAutoDepth + 1})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "guarded message is unread", func() bool {
		page, _ := a.Unread("", "", 10)
		return len(page.Messages) == 1 && page.Messages[0].ID == deep.ID && page.Messages[0].Paused
	})
	a.wakeIdle(context.Background())
	if w.count() != 0 {
		t.Fatalf("guarded message woke idle session: %v", w.calls)
	}
	if page, _ := a.Unread("", "", 10); page.Total != 1 {
		t.Fatalf("guarded message disappeared: %+v", page)
	}
}

func TestWakeText(t *testing.T) {
	for n, want := range map[int]string{
		1: "agent-link: 1 новое сообщение — прочитай их", 2: "agent-link: 2 новых сообщения — прочитай их",
		5: "agent-link: 5 новых сообщений — прочитай их", 11: "agent-link: 11 новых сообщений — прочитай их",
		21: "agent-link: 21 новое сообщение — прочитай их", 22: "agent-link: 22 новых сообщения — прочитай их",
	} {
		if got := WakeText(n); got != want {
			t.Errorf("WakeText(%d) = %q, want %q", n, got, want)
		}
	}
}

// Presence names a queue session unless a rewake one is there too.
func TestPresenceQueueSession(t *testing.T) {
	a, _ := wakePair(t, &fakeWaker{ready: true})
	dir := t.TempDir()
	if _, err := a.RegisterSession(SessionRequest{SessionID: "s1", Provider: "codex", Folder: dir, Wake: WakeNextEvent}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.RegisterSession(SessionRequest{SessionID: "s2", Provider: "codex", Folder: dir, Wake: WakeQueue}); err != nil {
		t.Fatal(err)
	}
	if p := a.presenceFor(nil); p[0].Session != WakeQueue {
		t.Fatalf("presence %+v", p)
	}
	if _, err := a.RegisterSession(SessionRequest{SessionID: "s3", Provider: "claude", Folder: dir, Wake: WakeRewake}); err != nil {
		t.Fatal(err)
	}
	if p := a.presenceFor(nil); p[0].Session != WakeRewake {
		t.Fatalf("presence %+v", p)
	}
}
