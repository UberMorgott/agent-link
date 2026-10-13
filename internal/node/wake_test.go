package node

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
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
	// calls: "home|session|text", then what the pointer was queued for (note);
	// texts: the prompts alone, as the thread gets them.
	calls []string
	texts []string
	// note, when set, names what a queued prompt is for (pointerFor).
	note func(text string) string
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
	call := home + "|" + session + "|" + text
	if w.note != nil {
		call += w.note(text)
	}
	w.calls = append(w.calls, call)
	w.texts = append(w.texts, text)
	return w.err
}

// wakeTokenOf is the wake token of a queued prompt ("" when it has none).
func wakeTokenOf(text string) string {
	_, rest, ok := strings.Cut(text, "[agent-link wake ")
	if !ok {
		return ""
	}
	token, _, _ := strings.Cut(rest, "]")
	return token
}

// pointerFor notes, for a test's waker, the messages a pointer of n was queued
// for: a "[pointer for] <body>" line for each lease of the prompt's token. The
// prompt names none of them, and the tests tell the wakes apart by them.
func pointerFor(n *Node) func(string) string {
	return func(text string) string {
		token := wakeTokenOf(text)
		if !pointerToken(token) {
			return ""
		}
		var bodies []string
		for _, l := range n.leases.all() {
			if l.Token != token {
				continue
			}
			if rec, ok := n.chats.message(l.ID); ok {
				bodies = append(bodies, rec.Message.Body)
				continue
			}
			for _, p := range n.store.unreadPlain() {
				if p.Message.ID == l.ID {
					bodies = append(bodies, p.Message.Body)
				}
			}
		}
		slices.Sort(bodies)
		var b strings.Builder
		for _, body := range bodies {
			b.WriteString("\n[pointer for] " + body)
		}
		return b.String()
	}
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
	if fw, ok := w.(*fakeWaker); ok {
		fw.note = pointerFor(a.Node)
	}
	a.SetSessionWaker(w)
	a.wakeEvery = time.Hour
	a.start(t)
	b.start(t)
	return a, b
}

// A pinned idle Codex thread stays registered while the node can wake it
// (Codex sends no heartbeat): an unpinned one, a pinned one in a turn, or one
// that let its wakes lapse still expires by its TTL (#27).
func TestKeepPinnedCodexThread(t *testing.T) {
	w := &fakeWaker{}
	a, _ := wakePair(t, w)
	dir, home := t.TempDir(), t.TempDir()
	for _, id := range []string{"s-chef", "s-free"} {
		if _, err := a.RegisterSession(SessionRequest{SessionID: id, Provider: ProviderCodex, Folder: dir, Wake: WakeQueue,
			Idle: true, CodexHome: home, TTLSec: 120}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.PinSession(PinSessionRequest{SessionID: "s-chef"}); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	live := func(at time.Time) map[string]bool { return a.sess.liveIDs(at) }
	a.keepPinned(t0.Add(90 * time.Second)) // no waker ready: nothing kept
	if l := live(t0.Add(150 * time.Second)); l["s-chef"] || l["s-free"] {
		t.Fatalf("kept without a ready waker: %v", l)
	}
	w.mu.Lock()
	w.ready = true
	w.mu.Unlock()
	a.syncQueueWake()
	a.keepPinned(t0.Add(90 * time.Second))
	if l := live(t0.Add(150 * time.Second)); !l["s-chef"] || l["s-free"] {
		t.Fatalf("pinned idle thread not kept, or the unpinned one kept: %v", l)
	}
	if got := a.Sessions(); len(got) != 2 {
		t.Fatalf("keeping must not drop anyone now: %+v", got)
	}
	// The wakes of its idle period lapsed untaken: still kept, for the stale
	// wakes (their waits are longer than a Codex session's TTL).
	a.sess.mu.Lock()
	s := a.sess.sessions["s-chef"]
	s.Woken, s.Wakes, s.WokeAt = true, maxIdleWakes, t0
	a.sess.mu.Unlock()
	a.keepPinned(t0.Add(160 * time.Second))
	if !live(t0.Add(215 * time.Second))["s-chef"] {
		t.Fatal("thread with stale wakes left is not kept")
	}
	// Every wake spent and the last one stale: left to its TTL.
	a.sess.mu.Lock()
	s.Wakes, s.WokeAt = maxIdleWakes+maxStaleWakes, t0.Add(-queuedOwnerMax)
	a.sess.mu.Unlock()
	a.keepPinned(t0.Add(230 * time.Second))
	if live(t0.Add(285 * time.Second))["s-chef"] {
		t.Fatal("thread whose wakes are spent still kept")
	}
	// In a turn: its own events keep it.
	a.sess.mu.Lock()
	s.Woken, s.Wakes, s.Idle = false, 0, false
	a.sess.mu.Unlock()
	a.keepPinned(t0.Add(230 * time.Second))
	if live(t0.Add(285 * time.Second))["s-chef"] {
		t.Fatal("busy thread kept")
	}
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
	// The prompt is a pointer: where to fetch, no message of its own.
	if s := wake(); !s.Woken || w.count() != 1 || !strings.HasPrefix(w.calls[0], home+"|s-q|agent-link: для этой сессии есть новые сообщения (1); их текста здесь нет. [agent-link wake p-") ||
		!strings.Contains(w.calls[0], "[pointer for] wake up") || strings.Contains(w.texts[0], "wake up") || strings.Contains(w.texts[0], first.Messages[0].ID) ||
		!strings.Contains(w.texts[0], "инструмент unread MCP-сервера agentlink") || !strings.Contains(w.texts[0], " chat unread --session s-q\n") || strings.Contains(w.texts[0], dir) {
		t.Fatalf("idle session: %+v %v", s, w.calls)
	}
	reg(true) // a heartbeat of the same idle period
	wake()
	if w.count() != 1 {
		t.Fatalf("woken twice in one idle period: %v", w.calls)
	}
	// Its next turn did not acknowledge it yet, and a new message came: the
	// next idle period's wake is for the new one only, and no second prompt:
	// the pointer in the queue, not shown yet, covers it (one at a time).
	if _, err := b.SendRequest(SendRequest{To: "a", Body: "second one", AuthorKind: AuthorAgent}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a has both", func() bool { p, _ := a.Unread("", "", 10); return p.Total == 2 })
	reg(false)
	reg(true)
	wake()
	both, _ := a.Unread("", "", 10)
	l1, _ := a.leases.get(both.Messages[0].ID)
	l2, _ := a.leases.get(both.Messages[1].ID)
	if w.count() != 1 || l2.State != LeaseLeased || l2.Token != wakeTokenOf(w.texts[0]) || l2.Attempts != 1 || l1.Attempts != 1 {
		t.Fatalf("new idle period: %v %+v %+v", w.calls, l1, l2)
	}
	// A pointer carried none of them: they are the session's hooks' to deliver
	// (or its fetch's), not woken with.
	p2, _ := a.UnreadFor(dir, "s-q", "", 10)
	if p2.Total != 2 || len(p2.Woken) != 0 {
		t.Fatalf("a pointer's messages are held back from the hooks: %+v", p2)
	}
	// The session's hook delivers and acknowledges them: read.
	if _, err := a.Ack("", AckRequest{IDs: ids(p2.Messages), SessionID: "s-q"}); err != nil {
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

// The node wakes nobody for a request past the hop limit, so it does not hold
// a live idle session's waiter off it: the waiter gets it (and, while the node
// wakes the session through its inbox, nothing else), wakes the session with
// it as information, and the session's hook then finds it woken.
func TestWaiterGetsGuardedMessage(t *testing.T) {
	p := &fakePoster{}
	dir := t.TempDir()
	a, b := deliveryPair(t, dir, p, nil)
	regClaude(t, a, dir, "s-c", testSocket, true)
	m := ask(t, a, b, "within the limit")
	deep, err := b.SendMessage(Message{ChatID: m.ChatID, Body: "guarded", Responders: []string{a.cfg.Node}, RootID: newID(), AutoDepth: MaxAutoDepth + 1})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "guarded message is unread", func() bool {
		page, _ := a.Unread("", "", 10)
		return slices.ContainsFunc(page.Messages, func(u UnreadMessage) bool { return u.ID == deep.ID && u.Paused })
	})
	if !a.InboxWakes("s-c") {
		t.Fatal("the node does not wake the session through its inbox")
	}
	q := url.Values{"waiter": {"1"}, "session": {"s-c"}, "folder": {dir}, "actionable": {"1"}, "limit": {"1"}}
	page, err := a.UnreadQuery(q)
	if err != nil || page.Total != 1 || len(page.Messages) != 1 || page.Messages[0].ID != deep.ID || !page.Messages[0].Paused {
		t.Fatalf("the waiter of a live idle session does not get the guarded message alone: %+v %v", page, err)
	}
	// A Stop, an automatic wake: still not theirs.
	stop := url.Values{"session": {"s-c"}, "folder": {dir}, "actionable": {"1"}}
	if page, err := a.UnreadQuery(stop); err != nil || slices.ContainsFunc(page.Messages, func(u UnreadMessage) bool { return u.ID == deep.ID }) || len(page.Woken) != 0 {
		t.Fatalf("guarded message is actionable: %+v %v", page, err)
	}
	const token = "0123456789abcdef"
	if g, err := a.Claim(ClaimRequest{IDs: []string{deep.ID}, SessionID: "s-c", WakeToken: token}); err != nil || !slices.Equal(g, []string{deep.ID}) {
		t.Fatalf("the waiter's claim: %v %v", g, err)
	}
	if page, err := a.UnreadQuery(q); err != nil || len(page.Messages) != 0 || page.Total != 0 {
		t.Fatalf("the waiter gets the message it woke with again: %+v %v", page, err)
	}
	page, err = a.UnreadQuery(stop)
	if err != nil || len(page.Woken) != 1 || page.Woken[0].ID != deep.ID || page.Woken[0].WakeToken != token ||
		slices.ContainsFunc(page.Messages, func(u UnreadMessage) bool { return u.ID == deep.ID }) {
		t.Fatalf("the session's hook does not find the guarded message woken: %+v %v", page, err)
	}
}

// A pointer is for every message that waits for the session, whatever their
// size (it carries none): they are leased to it, another session cannot take
// them, its own hooks can. A wake it never took is retried only for what no
// pointer was queued for yet.
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
	if !strings.Contains(text, "новые сообщения (4)") || strings.Contains(w.texts[0], long) {
		t.Fatalf("text %q", text)
	}
	for i := range 4 {
		if !strings.Contains(text, fmt.Sprintf("[pointer for] m%d ", i)) {
			t.Fatalf("the pointer is not for m%d: %q", i, text)
		}
	}
	page, _ := a.UnreadFor(dir, "s-q", "", 10)
	if page.Total != 4 || len(page.Woken) != 0 {
		t.Fatalf("listed for the session %+v", page)
	}
	token := wakeTokenOf(w.texts[0])
	for _, m := range page.Messages {
		if l, _ := a.leases.get(m.ID); l.State != LeaseLeased || l.Owner != "s-q" || l.Via != ViaQueue || l.Token != token || !pointerToken(token) {
			t.Fatalf("lease of %s: %+v (prompt token %q)", m.ID, l, token)
		}
	}
	// Another session cannot take them.
	if _, err := a.RegisterSession(SessionRequest{SessionID: "s-other", Provider: "codex", Folder: dir, Wake: WakeNextEvent, Idle: true}); err != nil {
		t.Fatal(err)
	}
	if granted, _ := a.Claim(ClaimRequest{SessionID: "s-other", IDs: ids(page.Messages)}); len(granted) != 0 {
		t.Fatalf("another session took a pointer's messages: %v", granted)
	}
	if err := a.EndSession("s-other"); err != nil {
		t.Fatal(err)
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
	if page, _ := a.UnreadFor(dir, "s-q", "", 10); page.Total != 4 || len(page.Woken) != 0 {
		t.Fatalf("after the lapse: %+v", page)
	}
	// Still idle, never took it: no second pointer for the same messages.
	a.wakeIdle(context.Background())
	if w.count() != 1 {
		t.Fatalf("a second pointer for the same messages: %v", w.calls)
	}
	// One more wake in this idle period, for a message no pointer was queued
	// for yet (none has two), then no more.
	if _, err := b.SendRequest(SendRequest{To: "a", Body: "m4 " + long, AuthorKind: AuthorAgent}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a has 5", func() bool { p, _ := a.Unread("", "", 10); return p.Total == 5 })
	a.wakeIdle(context.Background())
	leasedTo := func(body string) Lease {
		t.Helper()
		p, _ := a.Unread("", "", 10)
		for _, m := range p.Messages {
			if strings.HasPrefix(m.Body, body) {
				l, _ := a.leases.get(m.ID)
				return l
			}
		}
		t.Fatalf("no unread message %q", body)
		return Lease{}
	}
	// The pointer is in the queue still: the wake is a lease under it, no
	// second prompt.
	if l := leasedTo("m4 "); w.count() != 1 || l.State != LeaseLeased || l.Token != token || l.Attempts != 1 {
		t.Fatalf("no second wake after the lapse: %v %+v", w.calls, l)
	}
	a.wakeIdle(context.Background())
	lapse()
	a.wakeIdle(context.Background())
	a.wakeIdle(context.Background())
	if s, _ := a.SessionLive("s-q"); w.count() != 1 || s.Wakes != maxIdleWakes {
		t.Fatalf("more than %d wakes in one idle period: %+v", maxIdleWakes, s)
	}
	// A new idle period may be woken again, for what no pointer is queued for.
	if _, err := b.SendRequest(SendRequest{To: "a", Body: "m5 " + long, AuthorKind: AuthorAgent}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a has 6", func() bool { p, _ := a.Unread("", "", 10); return p.Total == 6 })
	// Inside the wait of the first stale wake (staleWakeWait): nothing yet.
	a.sess.mu.Lock()
	a.sess.sessions["s-q"].WokeAt = time.Now().Add(time.Minute - staleWakeWait(maxIdleWakes))
	a.sess.mu.Unlock()
	a.wakeIdle(context.Background())
	if _, ok := a.leases.get(func() string { p, _ := a.Unread("", "", 10); return p.Messages[len(p.Messages)-1].ID }()); ok {
		t.Fatalf("more than %d wakes in one idle period", maxIdleWakes)
	}
	for _, idle := range []bool{false, true} {
		if _, err := a.RegisterSession(SessionRequest{SessionID: "s-q", Provider: "codex", Folder: dir, Wake: WakeQueue, Idle: idle}); err != nil {
			t.Fatal(err)
		}
	}
	a.wakeIdle(context.Background())
	if l := leasedTo("m5 "); w.count() != 1 || l.State != LeaseLeased || l.Token != token || l.Attempts != 1 {
		t.Fatalf("new idle period not woken: %d %+v", w.count(), l)
	}
	// Its own hooks deliver what a pointer was queued for.
	page, _ = a.UnreadFor(dir, "s-q", "", 10)
	if granted, _ := a.Claim(ClaimRequest{SessionID: "s-q", IDs: ids(page.Messages)}); len(granted) != 6 {
		t.Fatalf("its hook cannot take a pointer's messages: %v", granted)
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
	if msgs, _ := a.wakeClaim("s-q", ViaQueue, page.Messages, ""); len(msgs) != 0 {
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
