package node

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// staleQueue is an idle Codex thread of a whose hooks never fire: only time
// passing (age), and what a test writes to its rollout, changes it.
type staleQueue struct {
	t    *testing.T
	a, b *testNode
	w    *fakeWaker
	dir  string
	home string
}

func newStaleQueue(t *testing.T) *staleQueue {
	t.Helper()
	q := &staleQueue{t: t, w: &fakeWaker{ready: true}, dir: t.TempDir(), home: t.TempDir()}
	q.a, q.b = wakePair(t, q.w)
	q.reg(true)
	return q
}

func (q *staleQueue) reg(idle bool) {
	q.t.Helper()
	if _, err := q.a.RegisterSession(SessionRequest{SessionID: "s-q", Provider: ProviderCodex, Folder: q.dir, Wake: WakeQueue,
		Idle: idle, CodexHome: q.home, TTLSec: 86400}); err != nil {
		q.t.Fatal(err)
	}
}

// send is a new request of b for a, received.
func (q *staleQueue) send(body string) Message {
	q.t.Helper()
	return ask(q.t, q.a, q.b, body)
}

// age makes the session's last wake, its activity, its leases and its wake
// claims d old, then runs the lease sweep and the wake pass as the loop does.
func (q *staleQueue) age(d time.Duration) {
	q.t.Helper()
	a, past := q.a, time.Now().Add(-d)
	a.leases.mu.Lock()
	for _, l := range a.leases.m {
		if l.Owner == "s-q" && l.State == LeaseLeased {
			l.At = past
		}
	}
	a.leases.mu.Unlock()
	a.sess.claimMu.Lock()
	for id, c := range a.sess.claims {
		c.at = past
		a.sess.claims[id] = c
	}
	a.sess.claimMu.Unlock()
	a.sess.mu.Lock()
	s := a.sess.sessions["s-q"]
	s.WokeAt, s.LastActive = past, past
	a.sess.mu.Unlock()
	q.pass()
}

func (q *staleQueue) pass() {
	q.a.leaseSweep(time.Now())
	q.a.wakeIdle(q.t.Context()) // ends the wake's rollout observer with the test
}

func (q *staleQueue) session() Session {
	q.t.Helper()
	s, ok := q.a.SessionLive("s-q")
	if !ok {
		q.t.Fatal("session gone")
	}
	return s
}

// lastWake is the text of the last queued prompt.
func (q *staleQueue) lastWake() string {
	q.w.mu.Lock()
	defer q.w.mu.Unlock()
	if len(q.w.calls) == 0 {
		return ""
	}
	return q.w.calls[len(q.w.calls)-1]
}

// wakesWith counts the queued prompts that carry body.
func (q *staleQueue) wakesWith(body string) int {
	q.w.mu.Lock()
	defer q.w.mu.Unlock()
	n := 0
	for _, c := range q.w.calls {
		if strings.Contains(c, body) {
			n++
		}
	}
	return n
}

// spend uses up the wakes of the idle period before the stale ones
// (maxIdleWakes), each for a request of its own, none proven. It returns the
// first request.
func (q *staleQueue) spend() Message {
	q.t.Helper()
	first := q.send("spent request 0")
	q.pass()
	for i := 1; i < maxIdleWakes; i++ {
		q.send(fmt.Sprintf("spent request %d", i))
		q.age(inboxWakeGrace + time.Second)
	}
	if s := q.session(); q.w.count() != maxIdleWakes || !s.Woken || s.Wakes != maxIdleWakes {
		q.t.Fatalf("wakes of the idle period: %d %+v", q.w.count(), s)
	}
	if v := leaseViewOf(q.t, q.a, first.ID); v.State != LeaseLeased || v.Owner != "s-q" || v.Via != ViaQueue || v.Attempts != 1 {
		q.t.Fatalf("lease of the first woken message: %+v", v)
	}
	if got := q.wakesWith("spent request 0"); got != 1 {
		q.t.Fatalf("the first request was queued %d times", got)
	}
	return first
}

// A Codex thread woken through its queue that never proves the wake (its app
// is closed: no turn, no hook event) is not latched for the rest of its
// registration: once the wake is stale its unproven message fails (a person
// decides, it is never queued again) and a newer message gets a wake of its
// own.
func TestStaleQueueWakeReleasesNewerMessages(t *testing.T) {
	q := newStaleQueue(t)
	old := q.spend()

	// Unproven, but not stale yet: nothing more is queued.
	early := q.send("early request")
	q.age(queuedOwnerMax - time.Minute)
	if q.w.count() != maxIdleWakes {
		t.Fatalf("woken before the wake was stale: %v", q.w.calls)
	}

	q.age(queuedOwnerMax + time.Second)
	if v := leaseViewOf(t, q.a, old.ID); v.State != LeaseFailed || v.Reason != "queued_stale" {
		t.Fatalf("stale unproven message: %+v", v)
	}
	text := q.lastWake()
	if q.w.count() != maxIdleWakes+1 || !strings.Contains(text, "early request") || strings.Contains(text, "spent request") {
		t.Fatalf("no wake of its own for the newer message: %v", q.w.calls)
	}
	if v := leaseViewOf(t, q.a, early.ID); v.State != LeaseLeased || v.Owner != "s-q" || v.Attempts != 1 {
		t.Fatalf("lease of the newer message: %+v", v)
	}
	waitAttempts(t, q.b, old, AttemptWakeRequested, AttemptNeedsHuman)

	// The next one waits for the backoff of the last wake, not longer.
	late := q.send("late request")
	q.age(staleWakeWait(maxIdleWakes+1) - time.Minute)
	if q.w.count() != maxIdleWakes+1 {
		t.Fatalf("woken inside the backoff: %v", q.w.calls)
	}
	q.age(staleWakeWait(maxIdleWakes+1) + time.Second)
	text = q.lastWake()
	if q.w.count() != maxIdleWakes+2 || !strings.Contains(text, "late request") || strings.Contains(text, "early request") || strings.Contains(text, "spent request") {
		t.Fatalf("stale wake: %v", q.w.calls)
	}
	if v := leaseViewOf(t, q.a, late.ID); v.State != LeaseLeased || v.Owner != "s-q" {
		t.Fatalf("lease of the late message: %+v", v)
	}
	// No message id was queued twice, and the unproven one stays failed.
	if v := leaseViewOf(t, q.a, old.ID); v.State != LeaseFailed || v.Attempts != 1 {
		t.Fatalf("the unproven message was leased again: %+v", v)
	}
	for _, body := range []string{"spent request 0", "spent request 1", "early request", "late request"} {
		if got := q.wakesWith(body); got != 1 {
			t.Fatalf("%q was queued %d times: %v", body, got, q.w.calls)
		}
	}
	// Its late proof still counts.
	if got := q.a.LeaseStart("s-q", "", []string{old.ID}); len(got) != 1 {
		t.Fatalf("late proof refused: %v", got)
	}
}

// A queue keeps the prompts it accepted: the retry of the idle period does not
// queue the same message again (one turn per message id), whatever lapsed.
func TestQueueWakeNeverRepeatsMessage(t *testing.T) {
	q := newStaleQueue(t)
	m := q.send("only request")
	q.pass()
	q.age(inboxWakeGrace + time.Second)
	q.age(queuedOwnerMax - time.Minute)
	if s := q.session(); q.w.count() != 1 || s.Wakes != 1 {
		t.Fatalf("queued again while unproven: %v %+v", q.w.calls, s)
	}
	if v := leaseViewOf(t, q.a, m.ID); v.State != LeaseLeased || v.Attempts != 1 {
		t.Fatalf("lease: %+v", v)
	}
	// Stale, failed, a new idle period: still not queued again.
	q.age(queuedOwnerMax + time.Second)
	q.reg(false)
	q.reg(true)
	q.pass()
	if v := leaseViewOf(t, q.a, m.ID); q.w.count() != 1 || v.State != LeaseFailed || v.Attempts != 1 {
		t.Fatalf("queued again after it failed: %v %+v", q.w.calls, v)
	}
}

// What this node's own person wrote to the others is information for its
// sessions' next event: it wakes no idle session, takes no lease and spends
// no wake of the idle period, so a request that comes later is woken for.
func TestOwnHumanMessageIsNoWakeReason(t *testing.T) {
	q := newStaleQueue(t)
	own, err := q.a.SendRequest(SendRequest{To: "b", Body: "decide on the schema", AuthorKind: AuthorHuman})
	if err != nil {
		t.Fatal(err)
	}
	page, _ := q.a.UnreadFor(q.dir, "s-q", "", 10)
	if page.Total != 1 || !page.Messages[0].OwnHuman || page.Messages[0].ID != own.ID {
		t.Fatalf("the session's unread: %+v", page)
	}
	q.pass()
	q.age(inboxWakeGrace + time.Second)
	q.age(queuedOwnerMax + time.Second)
	if s := q.session(); q.w.count() != 0 || s.Woken || s.Wakes != 0 {
		t.Fatalf("woken for its own person's message: %v %+v", q.w.calls, s)
	}
	if l, ok := q.a.leases.get(own.ID); ok {
		t.Fatalf("lease of its own person's message: %+v", l)
	}
	in := q.send("inbound request")
	q.pass()
	text := q.lastWake()
	if s := q.session(); q.w.count() != 1 || s.Wakes != 1 || !strings.Contains(text, "inbound request") || strings.Contains(text, "decide on the schema") {
		t.Fatalf("wake for the request: %v %+v", q.w.calls, s)
	}
	if v := leaseViewOf(t, q.a, in.ID); v.State != LeaseLeased || v.Owner != "s-q" {
		t.Fatalf("lease of the request: %+v", v)
	}
	// Still unread, for the session's hooks.
	if page, _ := q.a.UnreadFor(q.dir, "s-q", "", 10); !slices.ContainsFunc(page.Messages, func(m UnreadMessage) bool { return m.ID == own.ID }) {
		t.Fatalf("its own person's message is gone: %+v", page)
	}
}

// The state an older node left on disk (v0.6.69: both wakes of the idle
// period spent on the node's own person's message, queued twice, its lease
// back in retry after the turn's deadline; no hook event since) does not keep
// a later request from waking the session.
func TestExhaustedWakeStateOnDisk(t *testing.T) {
	q := newStaleQueue(t)
	own, err := q.a.SendRequest(SendRequest{To: "b", Body: "note of the person", AuthorKind: AuthorHuman})
	if err != nil {
		t.Fatal(err)
	}
	var saved Session
	if err := json.Unmarshal([]byte(`{"session_id":"s-q","woken":true,"wakes":2,"woke_at":"2026-10-12T19:52:21Z"}`), &saved); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-13 * time.Hour)
	q.a.sess.mu.Lock()
	s := q.a.sess.sessions["s-q"]
	s.Woken, s.Wakes, s.WokeAt, s.WakeCapped = saved.Woken, saved.Wakes, past, saved.WakeCapped
	q.a.sess.mu.Unlock()
	q.a.leases.mu.Lock()
	q.a.leases.m[own.ID] = &Lease{ID: own.ID, State: LeaseRetry, Owner: "s-q", Via: ViaQueue, Token: "0123456789abcdef", Attempts: 2,
		Fails: map[string]int{"s-q": 1}, Reason: "deadline", At: past}
	q.a.leases.mu.Unlock()
	q.pass()
	if q.w.count() != 0 {
		t.Fatalf("woken with nothing new: %v", q.w.calls)
	}
	in := q.send("ask after the upgrade")
	q.pass()
	text := q.lastWake()
	if q.w.count() != 1 || !strings.Contains(text, "ask after the upgrade") || strings.Contains(text, "note of the person") {
		t.Fatalf("the request after the upgrade got no wake: %v", q.w.calls)
	}
	if v := leaseViewOf(t, q.a, in.ID); v.State != LeaseLeased || v.Owner != "s-q" || v.Attempts != 1 {
		t.Fatalf("lease of the request: %+v", v)
	}
	if l, _ := q.a.leases.get(own.ID); l.State != LeaseRetry || l.Attempts != 2 {
		t.Fatalf("the old lease was taken again: %+v", l)
	}
}

// A queued turn the rollout shows started is proof the session took the wake:
// the wakes of that idle period are over though no hook event said so, its
// messages are its turn's (not queued again), and a request that comes later
// wakes it at once.
func TestProvenQueueWakeReleasesLatch(t *testing.T) {
	q := newStaleQueue(t)
	dir := filepath.Join(q.home, "sessions", "2026", "10", "13")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	rollout := filepath.Join(dir, "rollout-x-s-q.jsonl")
	if err := os.WriteFile(rollout, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	first := q.send("first request")
	q.pass()
	page, _ := q.a.UnreadFor(q.dir, "s-q", "", 10)
	if q.w.count() != 1 || len(page.Woken) != 1 || !q.session().Woken {
		t.Fatalf("first wake: %v %+v", q.w.calls, page)
	}
	if err := os.WriteFile(rollout, []byte(WakeMarker(page.Woken[0].WakeToken)), 0o600); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the proven wake ends the wake count", func() bool { s := q.session(); return !s.Woken && s.Wakes == 0 })
	// No hook ran: the rollout's proof alone acknowledged the message.
	if l, _ := q.a.leases.get(first.ID); l.State != LeaseAcked {
		t.Fatalf("lease after the proof: %+v", l)
	}
	if page, _ := q.a.UnreadFor(q.dir, "s-q", "", 10); page.Total != 0 || len(page.Woken) != 0 {
		t.Fatalf("the proven message is still unread: %+v", page)
	}
	waitAttempts(t, q.b, first, AttemptWakeRequested, AttemptWokenConfirmed)
	q.pass()
	if q.w.count() != 1 {
		t.Fatalf("the proven message was queued again: %v", q.w.calls)
	}
	q.send("second request")
	q.pass()
	if text := q.lastWake(); q.w.count() != 2 || !strings.Contains(text, "second request") || strings.Contains(text, "first request") {
		t.Fatalf("no wake after the proven turn: %v", q.w.calls)
	}
}

// A message a session's turn has from a queue wake (running: proven by
// LeaseStart, not acknowledged) is not queued again, neither while it runs
// nor after the lease lapsed unacknowledged.
func TestRunningQueueMessageNotQueuedAgain(t *testing.T) {
	q := newStaleQueue(t)
	m := q.send("running request")
	q.pass()
	if got := q.a.LeaseStart("s-q", "", []string{m.ID}); len(got) != 1 {
		t.Fatalf("proof refused: %v", got)
	}
	q.reg(false)
	q.reg(true)
	q.age(inboxWakeGrace + time.Second)
	if q.w.count() != 1 {
		t.Fatalf("queued again while its turn has it: %v", q.w.calls)
	}
	q.a.leases.mu.Lock()
	q.a.leases.m[m.ID].Deadline = time.Now().Add(-time.Second)
	q.a.leases.mu.Unlock()
	q.pass()
	backoffOver(q.a, m.ID)
	q.reg(false)
	q.reg(true)
	q.age(inboxWakeGrace + time.Second)
	if l, _ := q.a.leases.get(m.ID); l.State != LeaseRetry || l.Reason != "deadline" || q.w.count() != 1 {
		t.Fatalf("a turn's message was queued again: %+v %v", l, q.w.calls)
	}
}

// A woken session whose hooks do not fire (it stays Idle and woken) but that
// reports activity on a request took a turn: its wakes of that idle period
// are over, and a message that came since wakes it at once.
func TestActivityReleasesWakeLatch(t *testing.T) {
	q := newStaleQueue(t)
	old := q.spend()
	newer := q.send("newer request")
	q.pass()
	if q.w.count() != maxIdleWakes {
		t.Fatalf("woken past the cap: %v", q.w.calls)
	}
	if _, err := q.a.SessionActivity(old.ChatID, ActivityRequest{SessionID: "s-q", ReplyTo: old.ID, Type: "command", Text: "go test"}); err != nil {
		t.Fatal(err)
	}
	if s := q.session(); s.Woken || s.Wakes != 0 || !s.WokeAt.IsZero() {
		t.Fatalf("activity kept the latch: %+v", s)
	}
	q.pass()
	if text := q.lastWake(); q.w.count() != maxIdleWakes+1 || !strings.Contains(text, "newer request") || strings.Contains(text, "spent request") {
		t.Fatalf("no wake after activity: %v", q.w.calls)
	}
	if v := leaseViewOf(t, q.a, newer.ID); v.State != LeaseLeased || v.Owner != "s-q" {
		t.Fatalf("lease of the newer message: %+v", v)
	}
}

// A message whose wake failed for good does not keep the newer ones out of the
// prompt: it is left out before the prompt is fitted.
func TestWakeFitSkipsFailedMessages(t *testing.T) {
	q := newStaleQueue(t)
	long := strings.Repeat("x", 1500)
	m0, m1 := q.send("m0 "+long), q.send("m1 "+long)
	q.send("m2 " + long)
	q.pass()
	if text := q.lastWake(); q.w.count() != 1 || !strings.Contains(text, "m0 ") || !strings.Contains(text, "m1 ") || strings.Contains(text, "m2 ") {
		t.Fatalf("first wake: %v", q.w.calls)
	}
	if err := q.a.leases.fail("s-q", []string{m0.ID, m1.ID}, "queued_stale", time.Now()); err != nil {
		t.Fatal(err)
	}
	// A new idle period: the failed ones are the oldest unread and would fill
	// the prompt.
	q.reg(false)
	q.reg(true)
	q.pass()
	if text := q.lastWake(); q.w.count() != 2 || !strings.Contains(text, "m2 ") || strings.Contains(text, "m0 ") || strings.Contains(text, "m1 ") {
		t.Fatalf("the failed messages kept the newer one out: %v", q.w.calls)
	}
}

// The stale wakes are bounded, each for its own messages after a growing
// wait; then nothing is queued into the dead thread any more and the authors
// of what waits for it hear needs_human once, which a restart does not repeat
// (saved with the session). A new idle period starts over.
func TestStaleQueueWakeCap(t *testing.T) {
	q := newStaleQueue(t)
	q.spend()
	limit := maxIdleWakes + maxStaleWakes
	for i := maxIdleWakes; i < limit; i++ {
		q.send(fmt.Sprintf("stale request %d", i))
		q.age(staleWakeWait(i) - time.Minute)
		if q.w.count() != i {
			t.Fatalf("wake %d inside its backoff: %d", i+1, q.w.count())
		}
		q.age(staleWakeWait(i) + time.Second)
		if s := q.session(); q.w.count() != i+1 || s.Wakes != i+1 || s.WakeCapped {
			t.Fatalf("stale wake %d: %d %+v", i+1, q.w.count(), s)
		}
	}
	if d := staleWakeWait(limit - 1); d != queuedOwnerMax<<(maxStaleWakes-1) {
		t.Fatalf("last backoff %v", d)
	}

	waiting := q.send("request past the cap")
	q.pass()
	if s := q.session(); s.WakeCapped || slices.Contains(attemptsOf(q.b, waiting), AttemptNeedsHuman) {
		t.Fatalf("needs_human before the last wake was stale: %+v", s)
	}
	q.age(24 * time.Hour)
	q.age(24 * time.Hour)
	if s := q.session(); q.w.count() != limit || !s.WakeCapped {
		t.Fatalf("past the cap: wakes %d %+v", q.w.count(), s)
	}
	waitAttempts(t, q.b, waiting, AttemptNeedsHuman)
	if l, ok := q.a.leases.get(waiting.ID); ok {
		t.Fatalf("the waiting message is not the next event's any more: %+v", l)
	}
	var saved []Session
	if err := readJSON(q.a.sess.path, &saved); err != nil || len(saved) != 1 || !saved[0].WakeCapped || saved[0].Wakes != limit {
		t.Fatalf("the cap is not saved: %v %+v", err, saved)
	}

	// Once: neither a later message nor a later pass reports again.
	later := q.send("request after the notice")
	q.age(24 * time.Hour)
	time.Sleep(200 * time.Millisecond)
	if got := attemptsOf(q.b, later); slices.Contains(got, AttemptNeedsHuman) || q.w.count() != limit {
		t.Fatalf("after the notice: %v wakes %d", got, q.w.count())
	}
	if got := attemptsOf(q.b, waiting); !slices.Equal(got, []string{AttemptNeedsHuman}) {
		t.Fatalf("needs_human repeated: %v", got)
	}

	// A new idle period (the thread took a turn) is woken again.
	q.reg(false)
	q.reg(true)
	q.pass()
	text := q.lastWake()
	if s := q.session(); q.w.count() != limit+1 || s.Wakes != 1 || s.WakeCapped ||
		!strings.Contains(text, "request past the cap") || !strings.Contains(text, "request after the notice") || strings.Contains(text, "spent request") {
		t.Fatalf("new idle period: %d %+v %q", q.w.count(), s, text)
	}
}

// A Claude session keeps its two wakes per idle period: its waiter takes over
// after them, the node queues nothing more.
func TestStaleWakeOnlyForQueueSessions(t *testing.T) {
	now := time.Now()
	s := Session{Wake: WakeRewake, Idle: true, Woken: true, Wakes: maxIdleWakes, WokeAt: now.Add(-48 * time.Hour)}
	if s.wakeDue(now) || s.wakeSpent(now) {
		t.Fatalf("a waiter's session got a stale wake: %+v", s)
	}
	s.Wake = WakeQueue
	if !s.wakeDue(now) || s.wakeSpent(now) {
		t.Fatalf("queue session past the stale limit: %+v", s)
	}
	s.Wakes = maxIdleWakes + maxStaleWakes
	if s.wakeDue(now) || !s.wakeSpent(now) {
		t.Fatalf("queue session past the cap: %+v", s)
	}
}
