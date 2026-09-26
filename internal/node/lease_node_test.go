package node

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

const testSocket2 = `\\.\pipe\LOCAL\cc-msg-fedcba9876543210fedcba9876543210`

// regClaude registers an idle (or busy) Claude session of a with an inbox.
func regClaude(t *testing.T, a *testNode, dir, sid, sock string, idle bool) {
	t.Helper()
	if _, err := a.RegisterSession(SessionRequest{SessionID: sid, Provider: "claude", Folder: dir, Wake: WakeRewake,
		Idle: idle, InboxSocket: sock, InboxToken: testToken}); err != nil {
		t.Fatal(err)
	}
}

// leaseOf is message id's lease as GET /leases shows it.
func leaseViewOf(t *testing.T, a *testNode, id string) LeaseView {
	t.Helper()
	for _, v := range a.Leases() {
		if v.ID == id && v.Seat == "" {
			return v
		}
	}
	t.Fatalf("no lease view of %s", id)
	return LeaseView{}
}

// lapse makes message id's lease and wake claim pass their deadline, and the
// idle sessions' last wake old enough to be retried.
func lapse(a *testNode, id string) {
	past := time.Now().Add(-inboxWakeGrace - time.Second)
	a.leases.mu.Lock()
	if l := a.leases.m[id]; l != nil {
		l.Deadline = time.Now().Add(-time.Second)
	}
	a.leases.mu.Unlock()
	a.sess.claimMu.Lock()
	if c, ok := a.sess.claims[id]; ok {
		c.at = past
		a.sess.claims[id] = c
	}
	a.sess.claimMu.Unlock()
	a.sess.mu.Lock()
	for _, s := range a.sess.sessions {
		if s.Woken {
			s.WokeAt = past
		}
	}
	a.sess.mu.Unlock()
}

// backoffOver ends message id's retry backoff.
func backoffOver(a *testNode, id string) {
	a.leases.mu.Lock()
	if l := a.leases.m[id]; l != nil {
		l.NextAt = time.Now().Add(-time.Second)
	}
	a.leases.mu.Unlock()
}

// An inbox wake leases the message to the session (not running: the post is
// no proof); the session's ack at the woken prompt acks the lease, and
// nothing delivers it again.
func TestLeaseInboxWakeLifecycle(t *testing.T) {
	p := &fakePoster{}
	dir := t.TempDir()
	a, b := deliveryPair(t, dir, p, nil)
	regClaude(t, a, dir, "s-c", testSocket, true)
	m := ask(t, a, b, "wake up")
	if v := leaseViewOf(t, a, m.ID); v.State != LeasePending || v.Attempts != 0 {
		t.Fatalf("before: %+v", v)
	}
	a.wakeIdle(context.Background())
	v := leaseViewOf(t, a, m.ID)
	if v.State != LeaseLeased || v.Owner != "s-c" || v.Via != ViaInbox || v.Attempts != 1 || time.Until(v.Deadline) > inboxWakeGrace {
		t.Fatalf("after the wake: %+v", v)
	}
	if _, err := a.Ack("", AckRequest{IDs: []string{m.ID}, SessionID: "s-c"}); err != nil {
		t.Fatal(err)
	}
	if v := a.leases.all()[m.ID]; v.State != LeaseAcked {
		t.Fatalf("after the ack: %+v", v)
	}
	regClaude(t, a, dir, "s-c", testSocket, false)
	regClaude(t, a, dir, "s-c", testSocket, true)
	a.wakeIdle(context.Background())
	if page, _ := a.UnreadFor(dir, "s-c", "", 10); p.count() != 1 || page.Total != 0 || len(page.Woken) != 0 {
		t.Fatalf("delivered again: posts %d page %+v", p.count(), page)
	}
}

// A failed wake, a session that ends or one that is no longer live revokes
// the lease at once: back to retry, its claim dropped, the owner's failure
// counted.
func TestLeaseRevoked(t *testing.T) {
	p := &fakePoster{}
	dir := t.TempDir()
	a, b := deliveryPair(t, dir, p, nil)

	p.err = errors.New("pipe gone")
	regClaude(t, a, dir, "s-1", testSocket, true)
	m1 := ask(t, a, b, "one")
	a.wakeIdle(context.Background())
	if v := leaseViewOf(t, a, m1.ID); v.State != LeaseRetry || v.Reason != "wake_failed" || a.leases.fails(m1.ID, "s-1") != 1 {
		t.Fatalf("failed wake: %+v", v)
	}
	if page, _ := a.UnreadFor(dir, "s-1", "", 10); page.Total != 1 {
		t.Fatalf("failed wake: the hooks must deliver it: %+v", page)
	}
	if _, err := a.Ack("", AckRequest{IDs: []string{m1.ID}, SessionID: "s-1"}); err != nil {
		t.Fatal(err)
	}
	if err := a.EndSession("s-1"); err != nil { // it holds the chat's affinity
		t.Fatal(err)
	}

	p.mu.Lock()
	p.err = nil
	p.mu.Unlock()
	regClaude(t, a, dir, "s-2", testSocket2, true)
	m2 := ask(t, a, b, "two")
	a.wakeIdle(context.Background())
	if v := leaseViewOf(t, a, m2.ID); v.State != LeaseLeased || v.Owner != "s-2" {
		t.Fatalf("wake: %+v", v)
	}
	if err := a.EndSession("s-2"); err != nil {
		t.Fatal(err)
	}
	a.sess.claimMu.Lock()
	_, held := a.sess.claims[m2.ID]
	a.sess.claimMu.Unlock()
	if v := leaseViewOf(t, a, m2.ID); v.State != LeaseRetry || v.Reason != "session_end" || held {
		t.Fatalf("session ended: %+v claim %v", v, held)
	}

	regClaude(t, a, dir, "s-3", testSocket2, true)
	backoffOver(a, m2.ID)
	a.wakeIdle(context.Background())
	if v := leaseViewOf(t, a, m2.ID); v.State != LeaseLeased || v.Owner != "s-3" || v.Attempts != 2 {
		t.Fatalf("rewake: %+v", v)
	}
	a.sess.mu.Lock()
	a.sess.sessions["s-3"].LastSeen = time.Now().Add(-time.Hour)
	a.sess.mu.Unlock()
	a.leaseSweep(time.Now())
	if v := leaseViewOf(t, a, m2.ID); v.State != LeaseRetry || v.Reason != "session_gone" {
		t.Fatalf("session gone: %+v", v)
	}
}

// A wake that put the message into a session's inbox (or queue) stays that
// session's past its deadline (a slow turn is no failure): no other session
// is woken for it or takes it, it may only be woken again. Evidence the
// channel dropped it revokes it; the next idle session (fewest failed leases,
// then active last) gets it, and the first session's hooks do not deliver it
// too; its late proof still counts.
func TestLeaseQueuedStaysWithOwner(t *testing.T) {
	p := &fakePoster{}
	dir := t.TempDir()
	a, b := deliveryPair(t, dir, p, nil)
	regClaude(t, a, dir, "s-1", testSocket, true)
	time.Sleep(10 * time.Millisecond)
	regClaude(t, a, dir, "s-2", testSocket2, true) // active last: woken first
	m := ask(t, a, b, "who takes it")
	a.wakeIdle(context.Background())
	if v := leaseViewOf(t, a, m.ID); v.Owner != "s-2" || p.count() != 1 || !strings.HasPrefix(p.calls[0], testSocket2) {
		t.Fatalf("first wake: %+v %v", v, p.calls)
	}
	lapse(a, m.ID)
	a.leaseSweep(time.Now())
	if v := leaseViewOf(t, a, m.ID); v.State != LeaseLeased || v.Owner != "s-2" {
		t.Fatalf("lapsed: %+v", v)
	}
	a.wakeIdle(context.Background())
	if v := leaseViewOf(t, a, m.ID); p.count() != 2 || !strings.HasPrefix(p.calls[1], testSocket2) || v.Owner != "s-2" || a.leases.fails(m.ID, "s-2") != 0 {
		t.Fatalf("after the deadline: %+v %v", v, p.calls)
	}
	if page, _ := a.UnreadFor(dir, "s-1", "", 10); page.Total != 0 {
		t.Fatalf("another session would take it: %+v", page)
	}
	if got, _ := a.Claim(ClaimRequest{IDs: []string{m.ID}, SessionID: "s-1"}); len(got) != 0 {
		t.Fatalf("another session claimed it: %v", got)
	}

	a.LeaseRevoke("s-2", []string{m.ID}, "inbox_dropped")
	backoffOver(a, m.ID)
	a.wakeIdle(context.Background())
	if v := leaseViewOf(t, a, m.ID); v.Owner != "s-1" || v.Attempts != 3 || p.count() != 3 || !strings.HasPrefix(p.calls[2], testSocket+"|") {
		t.Fatalf("reassigned: %+v %v", v, p.calls)
	}
	if page, _ := a.UnreadFor(dir, "s-2", "", 10); page.Total != 0 {
		t.Fatalf("the first session would get it too: %+v", page)
	}
	if got := a.LeaseStart("s-2", "", []string{m.ID}); len(got) != 1 {
		t.Fatalf("late proof refused: %v", got)
	}
	if _, err := a.Ack("", AckRequest{IDs: []string{m.ID}, SessionID: "s-2"}); err != nil {
		t.Fatal(err)
	}
	if l, _ := a.leases.get(m.ID); l.State != LeaseAcked || l.Owner != "s-2" {
		t.Fatalf("acked: %+v", l)
	}
}

// A queued wake whose session keeps heartbeating but does nothing for
// queuedOwnerMax is revoked (its failure counted) and the message goes to
// another idle session; activity since the lease keeps it. The first
// session's late proof still counts.
func TestLeaseQueuedOwnerCap(t *testing.T) {
	p := &fakePoster{}
	dir := t.TempDir()
	a, b := deliveryPair(t, dir, p, nil)
	regClaude(t, a, dir, "s-1", testSocket, true)
	time.Sleep(10 * time.Millisecond)
	regClaude(t, a, dir, "s-2", testSocket2, true)
	m := ask(t, a, b, "stuck in a queue")
	a.wakeIdle(context.Background())
	if v := leaseViewOf(t, a, m.ID); v.Owner != "s-2" || v.Via != ViaInbox {
		t.Fatalf("first wake: %+v", v)
	}
	age := func(lease, active time.Duration) {
		now := time.Now()
		a.leases.mu.Lock()
		a.leases.m[m.ID].At = now.Add(-lease)
		a.leases.mu.Unlock()
		a.sess.mu.Lock()
		a.sess.sessions["s-2"].LastActive = now.Add(-active)
		a.sess.sessions["s-2"].WokeAt = now.Add(-lease)
		a.sess.mu.Unlock()
		a.sess.claimMu.Lock()
		if c, ok := a.sess.claims[m.ID]; ok {
			c.at = now.Add(-lease) // the wake claim lapsed long ago
			a.sess.claims[m.ID] = c
		}
		a.sess.claimMu.Unlock()
	}
	// Active 10 minutes ago, after the lease: kept.
	age(queuedOwnerMax+time.Minute, 10*time.Minute)
	a.leaseSweep(time.Now())
	if v := leaseViewOf(t, a, m.ID); v.State != LeaseLeased || v.Owner != "s-2" {
		t.Fatalf("an active session lost it: %+v", v)
	}
	// Heartbeats only (LastSeen), no activity since the lease: revoked.
	if _, err := a.RegisterSession(SessionRequest{SessionID: "s-2", Provider: "claude", Folder: dir, Wake: WakeRewake, Idle: true,
		InboxSocket: testSocket2, InboxToken: testToken, Heartbeat: true}); err != nil {
		t.Fatal(err)
	}
	age(queuedOwnerMax+time.Minute, queuedOwnerMax+2*time.Minute)
	a.leaseSweep(time.Now())
	if v := leaseViewOf(t, a, m.ID); v.State != LeaseRetry || v.Reason != "queued_stale" || a.leases.fails(m.ID, "s-2") != 1 || a.leases.queuedOwner(m.ID) != "" {
		t.Fatalf("stale owner kept it: %+v", v)
	}
	backoffOver(a, m.ID)
	a.wakeIdle(context.Background())
	if v := leaseViewOf(t, a, m.ID); v.Owner != "s-1" || !strings.HasPrefix(p.calls[len(p.calls)-1], testSocket+"|") {
		t.Fatalf("not reassigned: %+v %v", v, p.calls)
	}
	if got := a.LeaseStart("s-2", "", []string{m.ID}); len(got) != 1 {
		t.Fatalf("late proof refused: %v", got)
	}
}

// A session passed over for a message is not woken for it again, whatever
// its idle periods (no loop); the message is an orphan then, and the launch
// ladder opens a new session although one is registered.
func TestLeaseOrphanLaunches(t *testing.T) {
	p, l := &fakePoster{}, &fakeLauncher{}
	dir := t.TempDir()
	a, b := deliveryPair(t, dir, p, l)
	a.occupied = func(string, time.Time) bool { return false }
	regClaude(t, a, dir, "s-1", testSocket, true)
	m := ask(t, a, b, "anyone there")
	p.mu.Lock()
	p.err = errors.New("pipe gone")
	p.mu.Unlock()
	for i := range maxOwnerFails {
		regClaude(t, a, dir, "s-1", testSocket, true) // its inbox again
		a.wakeIdle(context.Background())
		if p.count() != i+1 {
			t.Fatalf("wake %d: %v", i+1, p.calls)
		}
		backoffOver(a, m.ID)
	}
	p.mu.Lock()
	p.err = nil
	p.mu.Unlock()
	for range 3 {
		regClaude(t, a, dir, "s-1", testSocket, false)
		regClaude(t, a, dir, "s-1", testSocket, true)
		a.wakeIdle(context.Background())
	}
	if p.count() != maxOwnerFails {
		t.Fatalf("a passed-over session was woken again: %v", p.calls)
	}
	unread, _ := a.Unread("", "", 10)
	if o := a.orphans("", unread.Messages, time.Now()); len(o) != 1 || o[0].ID != m.ID {
		t.Fatalf("orphans %+v", o)
	}
	later := time.Now().Add(launchGrace + time.Second)
	a.launchDue(context.Background(), later)
	if specs := l.all(); len(specs) != 1 || !strings.Contains(specs[0].Prompt, "anyone there") {
		t.Fatalf("launches %+v", specs)
	}
	// s-1's heartbeat does not confirm the launch; a new session does.
	regClaude(t, a, dir, "s-1", testSocket, true)
	a.launchDue(context.Background(), later.Add(time.Second))
	waitAttempts(t, b, m, AttemptLaunchRequested)
	regClaude(t, a, dir, "s-new", testSocket2, false)
	a.launchDue(context.Background(), later.Add(2*time.Second))
	waitAttempts(t, b, m, AttemptLaunchRequested, AttemptLaunchConfirmed)
}

// The book survives a restart: a wake's claim comes back with its token (a
// late proof still acknowledges instead of delivering twice), the waiter
// cap still holds, and a launch's turn that died with the node is failed
// (started) or back to retry (not started).
func TestLeaseRestart(t *testing.T) {
	p := &fakePoster{}
	dir := t.TempDir()
	a, b := deliveryPair(t, dir, p, nil)
	regClaude(t, a, dir, "s-c", testSocket, true)
	m := ask(t, a, b, "survive")
	a.wakeIdle(context.Background())
	token := a.leases.all()[m.ID].Token
	if token == "" {
		t.Fatal("no wake token")
	}
	now := time.Now()
	for _, id := range []string{"x-started", "x-waiting"} {
		if _, err := a.leases.take(id, "", launchOwner(""), ViaLaunch, "", now.Add(time.Hour), now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.leases.start(launchOwner(""), "", "", []string{"x-started"}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := a.leases.take("x-waiter", "", "s-c", ViaWaiter, "tokw1234", now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	if _, err := a.leases.revoke("s-c", []string{"x-waiter"}, "deadline", now); err != nil {
		t.Fatal(err)
	}
	if _, err := a.leases.take("x-waiter", "", "s-c", ViaWaiter, "tokw5678", now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}

	// Restart: the book is read again, the memory-only claims are gone.
	a.sess.claimMu.Lock()
	clear(a.sess.claims)
	a.sess.claimMu.Unlock()
	a.leases = newLeaseBook(a.cfg.DataDir)
	if err := a.leases.load(); err != nil {
		t.Fatal(err)
	}
	a.restoreLeases(time.Now())
	page, _ := a.UnreadFor(dir, "s-c", "", 10)
	if page.Total != 0 || len(page.Woken) != 1 || page.Woken[0].ID != m.ID || page.Woken[0].WakeToken != token {
		t.Fatalf("after the restart: %+v", page)
	}
	book := a.leases.all()
	if book["x-started"].State != LeaseFailed || book["x-waiting"].State != LeaseRetry {
		t.Fatalf("launch leases after the restart: %+v %+v", book["x-started"], book["x-waiting"])
	}
	if !a.waiterSpentMsg("x-waiter", time.Now()) {
		t.Fatalf("waiter cap lost: %+v", book["x-waiter"])
	}
}

// A held lease whose ack keeps failing is bounded: after leaseHoldMax it is
// failed (needs_human), its ack job and ackOnly claim go, so it blocks no
// session forever; nothing launches for it.
func TestLeaseHoldReleased(t *testing.T) {
	dir := t.TempDir()
	a, b := deliveryPair(t, dir, nil, nil)
	m := ask(t, a, b, "held")
	now := time.Now()
	a.holdForAck("", []string{m.ID}, "sess-h", now)
	if len(a.launchHeld()) != 1 {
		t.Fatal("not held")
	}
	a.leases.mu.Lock()
	a.leases.m[m.ID].At = now.Add(-leaseHoldMax - time.Second)
	a.leases.mu.Unlock()
	a.leaseSweep(now)
	a.deliv.mu.Lock()
	jobs, spent := len(a.deliv.acks), a.deliv.isSpent(m.ID)
	a.deliv.mu.Unlock()
	if l, _ := a.leases.get(m.ID); l.State != LeaseFailed || !l.Failed || jobs != 0 || !spent || len(a.launchHeld()) != 0 {
		t.Fatalf("lease %+v jobs %d spent %v held %v", l, jobs, spent, a.launchHeld())
	}
	waitAttempts(t, b, m, AttemptNeedsHuman)
	if _, err := a.RegisterSession(SessionRequest{SessionID: "s-h", Provider: "claude", Folder: dir}); err != nil {
		t.Fatal(err)
	}
	if got, _ := a.Claim(ClaimRequest{IDs: []string{m.ID}, SessionID: "s-h"}); len(got) != 1 {
		t.Fatalf("hooks cannot show it: %v", got)
	}
}
