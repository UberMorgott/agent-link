package node

import (
	"testing"
	"time"
)

// hookTake is the session's hook delivering ms in a turn: claimed and
// acknowledged.
func (q *staleQueue) hookTake(ms ...Message) {
	q.t.Helper()
	var want []string
	for _, m := range ms {
		want = append(want, m.ID)
	}
	got, err := q.a.Claim(ClaimRequest{IDs: want, SessionID: "s-q"})
	if err != nil || len(got) != len(want) {
		q.t.Fatalf("hook claim: %v %v", got, err)
	}
	if _, err := q.a.Ack("", AckRequest{IDs: got, SessionID: "s-q"}); err != nil {
		q.t.Fatal(err)
	}
}

// retryFor makes the leases that went back to retry d old: d passed since.
func retryFor(a *testNode, d time.Duration) {
	a.leases.mu.Lock()
	defer a.leases.mu.Unlock()
	for _, l := range a.leases.m {
		if l.State == LeaseRetry {
			l.At = l.At.Add(-d)
		}
	}
}

// A thread whose hooks fire and that ran its pointer (the marker is in its
// rollout) after a hook delivered the message is woken for the next message
// at once: nothing of the consumed pointer holds the wake.
func TestPointerRanAfterHookNextWakeAtOnce(t *testing.T) {
	q := newStaleQueue(t)
	path := q.rollout("")
	a := q.send("a")
	q.pass()
	tok := wakeTokenOf(q.lastWake())
	q.reg(false)
	q.hookTake(a)
	q.wrote(path, WakeMarker(tok)+"\n")
	q.reg(true)
	q.send("b")
	q.pass()
	if q.w.count() != 2 {
		t.Fatalf("the next wake waits: %d prompts, pending %q", q.w.count(), q.pending())
	}
}

// A user's turn whose hook took the message before the pointer ran leaves no
// pointer pending (nothing is leased under it any more): the next message is
// woken for at once, with or without a pointer before it.
func TestHookTookBeforePointerNextWakeAtOnce(t *testing.T) {
	q := newStaleQueue(t)
	q.rollout("")
	a := q.send("a")
	q.pass()
	q.reg(false)
	q.hookTake(a)
	q.reg(true)
	q.send("b")
	q.pass()
	if q.w.count() != 2 {
		t.Fatalf("the next wake waits: %d prompts, pending %q", q.w.count(), q.pending())
	}

	q = newStaleQueue(t)
	q.reg(false)
	q.reg(true)
	q.send("b")
	q.pass()
	if q.w.count() != 1 {
		t.Fatalf("a turn without a pointer holds the wake: %d prompts", q.w.count())
	}
}

// A pointer is not lost while the thread's rollout shows a turn running that
// its hooks did not report: it is late behind that turn. Its lease stays, no
// attempt or failure is counted, and its fetch after the turn delivers.
func TestPointerNotLostWhileRolloutShowsATurn(t *testing.T) {
	q := newStaleQueue(t)
	path := q.rollout("")
	a := q.send("a")
	q.pass()
	tok := wakeTokenOf(q.lastWake())
	q.reg(false)
	q.reg(true)
	q.wrote(path, turnLine("task_started", time.Now().Add(time.Second)))
	q.idleFor(pointerLostWait + time.Second)
	at := time.Now().Add(2 * time.Second)
	if held, busy := q.a.queueHeld(q.session(), at); !held || !busy {
		t.Fatalf("setup: not busy by its rollout: %v %v", held, busy)
	}
	q.a.leaseSweep(at)
	if l, _ := q.a.leases.get(a.ID); l.State != LeaseLeased || l.Token != tok || l.Attempts != 1 || len(l.Fails) != 0 || q.pending() != tok {
		t.Fatalf("the pointer was lost behind a running turn: %+v pending %q", l, q.pending())
	}
	// The turn ends; the pointer runs and fetches.
	q.wrote(path, turnLine("task_complete", time.Now().Add(3*time.Second)))
	q.wrote(path, WakeMarker(tok)+"\n")
	if page := q.fetch(); len(page.Messages) != 1 || !page.Delivered || q.w.count() != 1 {
		t.Fatalf("the pointer's fetch: %d delivered=%v prompts=%d", len(page.Messages), page.Delivered, q.w.count())
	}
}

// A lost pointer that runs after the new one was queued fetches what waits
// then; the new one's fetch finds nothing, and no third prompt is queued.
func TestLostPointerRunsAfterTheNewOne(t *testing.T) {
	q := newStaleQueue(t)
	path := q.rollout("")
	a := q.send("a")
	q.pass()
	old := wakeTokenOf(q.lastWake())
	q.reg(false)
	q.reg(true)
	q.idleFor(pointerLostWait + time.Second)
	q.pass()
	backoffOver(q.a, a.ID)
	q.pass()
	next := wakeTokenOf(q.lastWake())
	if q.w.count() != 2 || next == old {
		t.Fatalf("no new pointer: %d %v", q.w.count(), q.w.calls)
	}
	q.wrote(path, WakeMarker(old)+"\n")
	p1 := q.fetch()
	q.wrote(path, WakeMarker(next)+"\n")
	p2 := q.fetch()
	if len(p1.Messages) != 1 || len(p2.Messages) != 0 || p2.Delivered {
		t.Fatalf("fetches: %d then %d", len(p1.Messages), len(p2.Messages))
	}
	q.pass()
	if q.w.count() != 2 {
		t.Fatalf("a third prompt: %v", q.w.calls)
	}
}

// The emergency stop takes no pointer out of a queue: after it the message is
// in retry and its pointer still pending, and what comes meanwhile is leased
// under it. A thread whose hooks stay silent never ends that: after
// queuedOwnerMax the pointer is lost, its message is handed no more, and a new
// pointer is queued for it; what was leased under it goes back to retry with
// it instead of failing unproven.
func TestStoppedPointerLostAfterQueuedOwnerMax(t *testing.T) {
	q := newStaleQueue(t)
	q.rollout("")
	m := q.send("first")
	q.pass()
	old := wakeTokenOf(q.lastWake())
	q.a.SetStopped(true)
	q.a.SetStopped(false)
	if l, _ := q.a.leases.get(m.ID); l.State != LeaseRetry || l.Reason != "stopped" || q.pending() != old {
		t.Fatalf("after the stop: %+v pending %q", l, q.pending())
	}
	n := q.send("second")
	q.pass()
	q.age(inboxWakeGrace + time.Second)
	if l, _ := q.a.leases.get(n.ID); l.State != LeaseLeased || l.Token != old || q.w.count() != 1 {
		t.Fatalf("the second message is not under the pending pointer: %+v prompts %d", l, q.w.count())
	}
	// Inside the bound nothing changes.
	retryFor(q.a, queuedOwnerMax-time.Minute)
	q.pass()
	if l, _ := q.a.leases.get(m.ID); l.Reason != "stopped" || q.pending() != old || q.w.count() != 1 {
		t.Fatalf("lost inside queuedOwnerMax: %+v pending %q prompts %d", l, q.pending(), q.w.count())
	}
	retryFor(q.a, 2*time.Minute)
	q.age(queuedOwnerMax + time.Second)
	l, _ := q.a.leases.get(m.ID)
	l2, _ := q.a.leases.get(n.ID)
	next := wakeTokenOf(q.lastWake())
	if q.w.count() != 2 || next == old || l.State != LeaseLeased || l.Token != next || l.Failed {
		t.Fatalf("no new pointer for the first message: prompts %d first %+v pending %q", q.w.count(), l, q.pending())
	}
	if l2.Failed || l2.State != LeaseRetry || l2.Reason != reasonLost {
		t.Fatalf("the second message did not go back with the lost pointer: %+v", l2)
	}
}

// A registration that lapsed (the app was closed) takes no pointer out of the
// queue either. Back, with hooks that stay silent, the session's pointer is
// lost queuedOwnerMax after its lease went back, and a new one is queued.
func TestGoneSessionPointerLostAfterQueuedOwnerMax(t *testing.T) {
	w := &fakeWaker{ready: true}
	a, b := wakePair(t, w)
	dir, home := t.TempDir(), t.TempDir()
	reg := func() {
		t.Helper()
		if _, err := a.RegisterSession(SessionRequest{SessionID: "s-g", Provider: ProviderCodex, Folder: dir, Wake: WakeQueue, Idle: true, CodexHome: home, TTLSec: 3600}); err != nil {
			t.Fatal(err)
		}
	}
	reg()
	m := ask(t, a, b, "first")
	a.leaseSweep(time.Now())
	a.wakeIdle(t.Context())
	if w.count() != 1 {
		t.Fatalf("no pointer: %d", w.count())
	}
	old, _ := a.leases.pendingPointer("s-g")
	a.sess.mu.Lock()
	a.sess.sessions["s-g"].LastSeen = time.Now().Add(-2 * time.Hour)
	a.sess.mu.Unlock()
	a.leaseSweep(time.Now())
	if l, _ := a.leases.get(m.ID); l.State != LeaseRetry || l.Reason != "session_gone" {
		t.Fatalf("the gone session's lease: %+v", l)
	}
	reg()
	backoffOver(a, m.ID)
	n := ask(t, a, b, "second")
	a.leaseSweep(time.Now())
	a.wakeIdle(t.Context())
	if l2, _ := a.leases.get(n.ID); w.count() != 1 || l2.State != LeaseLeased || l2.Token != old {
		t.Fatalf("back: prompts %d, second %+v, pending %q", w.count(), l2, old)
	}
	// queuedOwnerMax of silence.
	a.leases.mu.Lock()
	for _, x := range a.leases.m {
		x.At = x.At.Add(-queuedOwnerMax - time.Minute)
	}
	a.leases.mu.Unlock()
	a.sess.mu.Lock()
	s := a.sess.sessions["s-g"]
	s.LastActive, s.WokeAt = s.LastActive.Add(-queuedOwnerMax-time.Minute), s.WokeAt.Add(-queuedOwnerMax-time.Minute)
	a.sess.mu.Unlock()
	a.leaseSweep(time.Now())
	a.wakeIdle(t.Context())
	l, _ := a.leases.get(m.ID)
	l2, _ := a.leases.get(n.ID)
	next, _ := a.leases.pendingPointer("s-g")
	if w.count() != 2 || next == "" || next == old || l.State != LeaseLeased || l.Token != next || l.Failed {
		t.Fatalf("no new pointer for the first message: prompts %d first %+v pending %q", w.count(), l, next)
	}
	if l2.Failed || l2.State != LeaseRetry || l2.Reason != reasonLost {
		t.Fatalf("the second message did not go back with the lost pointer: %+v", l2)
	}
}
