package node

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

func TestObserveQueueStartDrivesLease(t *testing.T) {
	n := newTestNode(t, "a", testSecret, nil, t.TempDir(), listen(t), nil)
	now := time.Now()
	if ok, err := n.leases.take("m1", "", "s1", ViaQueue, "tok12345", now.Add(time.Minute), now); err != nil || !ok {
		t.Fatalf("take lease: %v %v", ok, err)
	}
	home := t.TempDir()
	dir := filepath.Join(home, "sessions", "2026", "09", "26")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rollout-x-s1.jsonl"), []byte(WakeMarker("tok12345")), 0o600); err != nil {
		t.Fatal(err)
	}
	n.observeQueueStart(context.Background(), Session{SessionID: "s1", CodexHome: home}, "tok12345", []string{"m1"})
	if lease, _ := n.leases.get("m1"); lease.State != LeaseRunning {
		t.Fatalf("lease after proof: %+v", lease)
	}

	if ok, err := n.leases.take("m2", "", "s2", ViaQueue, "tok23456", now.Add(time.Minute), now); err != nil || !ok {
		t.Fatalf("take second lease: %v %v", ok, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	n.observeQueueStart(ctx, Session{SessionID: "s2", CodexHome: home}, "tok23456", []string{"m2"})
	if lease, _ := n.leases.get("m2"); lease.State != LeaseLeased {
		t.Fatalf("missing proof revoked content-carrying wake: %+v", lease)
	}
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
	first, _ := a.Unread("", "", 10)
	if s := wake(); !s.Woken || w.count() != 1 || !strings.HasPrefix(w.calls[0], home+"|s-q|agent-link: новые сообщения (1).") ||
		!strings.Contains(w.calls[0], "wake up") || !strings.Contains(w.calls[0], "id "+first.Messages[0].ID) ||
		!strings.Contains(w.calls[0], "Просит ответа от вас") || strings.Contains(w.calls[0], "прочитай их") {
		t.Fatalf("idle session: %+v %v", s, w.calls)
	}
	reg(true) // a heartbeat of the same idle period
	wake()
	if w.count() != 1 {
		t.Fatalf("woken twice in one idle period: %v", w.calls)
	}
	// Its next turn did not acknowledge it yet, and a new message came: the
	// next idle period's wake carries the new one only.
	if _, err := b.SendRequest(SendRequest{To: "a", Body: "second one", AuthorKind: AuthorAgent}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a has both", func() bool { p, _ := a.Unread("", "", 10); return p.Total == 2 })
	reg(false)
	reg(true)
	wake()
	if w.count() != 2 || !strings.Contains(w.calls[1], "second one") || strings.Contains(w.calls[1], "wake up") {
		t.Fatalf("new idle period: %v", w.calls)
	}
	if page, _ := a.UnreadFor(dir, "s-q", "", 10); page.Total != 0 || len(page.Woken) != 2 {
		t.Fatalf("hooks would deliver the woken again: %+v", page)
	}
	// The session acknowledges them at the prompt (its hook): read.
	var all []string
	for _, m := range first.Messages {
		all = append(all, m.ID)
	}
	p2, _ := a.UnreadFor(dir, "s-q", "", 10)
	for _, m := range p2.Woken {
		if !slices.Contains(all, m.ID) {
			all = append(all, m.ID)
		}
	}
	if _, err := a.Ack("", AckRequest{IDs: all, SessionID: "s-q"}); err != nil {
		t.Fatal(err)
	}
	if page, _ := a.UnreadFor(dir, "s-q", "", 10); page.Total != 0 || len(page.Woken) != 0 {
		t.Fatalf("after the ack: %+v", page)
	}
	if _, err := b.SendRequest(SendRequest{To: "a", Body: "third", AuthorKind: AuthorAgent}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a has the third", func() bool { p, _ := a.Unread("", "", 10); return p.Total == 1 })

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
	if page, _ := a.UnreadFor(dir, "s-q", "", 10); page.Total != 1 || len(page.Woken) != 0 {
		t.Fatalf("a failed wake kept its claim: %+v", page)
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

// A wake carries as many messages as fit FormatBudget and points at the rest;
// only those it carries are claimed. Its claim lapses after inboxWakeGrace
// (the session never took the prompt) and the hooks get them again.
func TestWakePromptCapAndLapse(t *testing.T) {
	w := &fakeWaker{ready: true}
	a, b := wakePair(t, w)
	dir := t.TempDir()
	if _, err := a.RegisterSession(SessionRequest{SessionID: "s-q", Provider: "codex", Folder: dir, Wake: WakeQueue, Idle: true}); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("x", 1500)
	for i := range 4 {
		if _, err := b.SendRequest(SendRequest{To: "a", Body: fmt.Sprintf("m%d %s", i, long), AuthorKind: AuthorAgent}); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "a has 4", func() bool { p, _ := a.Unread("", "", 10); return p.Total == 4 })
	a.wakeIdle(context.Background())
	if w.count() != 1 {
		t.Fatalf("wakes %v", w.calls)
	}
	text := w.calls[0]
	if !strings.Contains(text, "новые сообщения (2)") || !strings.Contains(text, "m0 ") || !strings.Contains(text, "m1 ") ||
		strings.Contains(text, "m2 ") || !strings.Contains(text, "Ещё 2 непрочитанных: agentlink chat unread --folder") {
		t.Fatalf("text %q", text)
	}
	page, _ := a.UnreadFor(dir, "s-q", "", 10)
	if page.Total != 2 || len(page.Woken) != 2 {
		t.Fatalf("claimed %+v", page)
	}
	// Another session cannot take the woken ones either.
	if granted, _ := a.Claim(ClaimRequest{SessionID: "s-q", IDs: ids(page.Woken)}); len(granted) != 0 {
		t.Fatalf("hook re-claimed woken: %v", granted)
	}
	for _, m := range page.Woken {
		if m.WakeToken == "" || !strings.Contains(text, WakeMarker(m.WakeToken)) {
			t.Fatalf("woken %s without the prompt's token %q", m.ID, m.WakeToken)
		}
	}
	lapse := func() {
		t.Helper()
		past := time.Now().Add(-inboxWakeGrace - time.Second)
		a.sess.claimMu.Lock()
		for id, c := range a.sess.claims {
			c.at = past
			a.sess.claims[id] = c
		}
		a.sess.claimMu.Unlock()
		a.sess.mu.Lock()
		a.sess.sessions["s-q"].WokeAt = past
		a.sess.mu.Unlock()
	}
	lapse()
	// Unread again; still listed as woken, for a hook that sees the prompt
	// arrived after all.
	if page, _ := a.UnreadFor(dir, "s-q", "", 10); page.Total != 4 || len(page.Woken) != 2 {
		t.Fatalf("after the lapse: %+v", page)
	}
	// Still idle, never took it: one retry in this idle period, then no more.
	a.wakeIdle(context.Background())
	if w.count() != 2 || !strings.Contains(w.calls[1], "m0 ") {
		t.Fatalf("no retry after the lapse: %v", w.calls)
	}
	a.wakeIdle(context.Background())
	lapse()
	a.wakeIdle(context.Background())
	a.wakeIdle(context.Background())
	if w.count() != 2 {
		t.Fatalf("more than %d wakes in one idle period: %d", maxIdleWakes, w.count())
	}
	// A new idle period may be woken again.
	for _, idle := range []bool{false, true} {
		if _, err := a.RegisterSession(SessionRequest{SessionID: "s-q", Provider: "codex", Folder: dir, Wake: WakeQueue, Idle: idle}); err != nil {
			t.Fatal(err)
		}
	}
	a.wakeIdle(context.Background())
	if w.count() != 3 {
		t.Fatalf("new idle period not woken: %d", w.count())
	}
}

func TestWakePromptFormat(t *testing.T) {
	m := UnreadMessage{AsksYou: true}
	m.ID, m.ChatID, m.From, m.AuthorKind, m.Body, m.Participants = "id1", "c1", "KPECTIK", "agent", "hello", []string{"KPECTIK", "me"}
	got := WakePrompt([]UnreadMessage{m}, 0, `C:\p`, "tok1")
	want := "agent-link: новые сообщения (1). Вся переписка остаётся в истории чата. [agent-link wake tok1]\n\n" + FormatUnread(m)
	if got != strings.TrimRight(want, "\n") || !strings.Contains(got, "agentlink send --chat c1 --reply-to id1") {
		t.Fatalf("got %q", got)
	}
	// Only the prompt of that wake acknowledges the message.
	m.WakeToken = "tok1"
	other := m
	other.WakeToken = "tok2"
	for _, c := range []struct {
		prompt string
		m      UnreadMessage
		want   bool
	}{
		{got, m, true},
		{"", m, false},                                          // the agent did not say
		{"see message id id1 please", m, false},                 // a foreign prompt naming the id
		{got, other, false},                                     // an earlier or later wake
		{"[agent-link wake tok1] id id2", m, false},             // that wake, but not this message
		{got, UnreadMessage{ChatMessage: m.ChatMessage}, false}, // no token
	} {
		if WokenBy(c.prompt, c.m) != c.want {
			t.Errorf("WokenBy(%q, token %q) != %v", c.prompt, c.m.WakeToken, c.want)
		}
	}
}

// A wake never takes a message a hook claimed (it is being delivered), even
// for the same session: no double delivery.
func TestWakeSkipsHookClaimed(t *testing.T) {
	w := &fakeWaker{ready: true}
	a, b := wakePair(t, w)
	dir := t.TempDir()
	if _, err := a.RegisterSession(SessionRequest{SessionID: "s-q", Provider: "codex", Folder: dir, Wake: WakeQueue, Idle: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.SendRequest(SendRequest{To: "a", Body: "claimed", AuthorKind: AuthorAgent}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a has it", func() bool { p, _ := a.Unread("", "", 10); return p.Total == 1 })
	page, _ := a.UnreadFor(dir, "s-q", "", 10)
	if granted, _ := a.Claim(ClaimRequest{SessionID: "s-q", IDs: ids(page.Messages)}); len(granted) != 1 {
		t.Fatalf("hook claim %v", granted)
	}
	a.wakeIdle(context.Background())
	if w.count() != 0 {
		t.Fatalf("woke with a hook-claimed message: %v", w.calls)
	}
	if msgs, _ := a.wakeClaim("s-q", ViaQueue, page.Messages); len(msgs) != 0 {
		t.Fatalf("wake claim over a hook claim: %v", msgs)
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
