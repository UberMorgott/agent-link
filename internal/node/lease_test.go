package node

import (
	"testing"
	"time"
)

func mustTake(t *testing.T, b *leaseBook, id, seat, owner, via, token string, deadline, now time.Time) {
	t.Helper()
	ok, err := b.take(id, seat, owner, via, token, deadline, now)
	if err != nil || !ok {
		t.Fatalf("take %s by %s via %s: ok %v err %v", id, owner, via, ok, err)
	}
}

func leaseState(t *testing.T, b *leaseBook, key string) Lease {
	t.Helper()
	l, ok := b.get(key)
	if !ok {
		t.Fatalf("no lease of %s", key)
	}
	return l
}

// pending -> leased -> running -> acked, idempotent by message id.
func TestLeaseLifecycle(t *testing.T) {
	b := newLeaseBook(t.TempDir())
	now := time.Now()
	mustTake(t, b, "m1", "", "s1", ViaInbox, "tok1", now.Add(time.Minute), now)
	mustTake(t, b, "m1", "", "s1", ViaInbox, "tok1", now.Add(2*time.Minute), now) // same wake again: refresh
	if l := leaseState(t, b, "m1"); l.State != LeaseLeased || l.Attempts != 1 || !l.Deadline.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("leased: %+v", l)
	}
	if ok, _ := b.take("m1", "", "s2", ViaInbox, "tok2", now.Add(time.Minute), now); ok {
		t.Fatal("another owner took a held lease")
	}
	if got, _ := b.start("s1", "", "other", []string{"m1"}, now); len(got) != 0 {
		t.Fatalf("started with a wrong token: %v", got)
	}
	if got, _ := b.start("s1", "", "tok1", []string{"m1"}, now); len(got) != 1 {
		t.Fatalf("start: %v", got)
	}
	if l := leaseState(t, b, "m1"); l.State != LeaseRunning || !l.Deadline.Equal(now.Add(runningHold)) {
		t.Fatalf("running: %+v", l)
	}
	if err := b.ack("", []string{"m1"}, nil, now); err != nil {
		t.Fatal(err)
	}
	if err := b.ack("", []string{"m1"}, nil, now); err != nil {
		t.Fatal(err)
	}
	if l := leaseState(t, b, "m1"); l.State != LeaseAcked {
		t.Fatalf("acked: %+v", l)
	}
	if ok, _ := b.take("m1", "", "s2", ViaInbox, "tok3", now.Add(time.Minute), now); ok {
		t.Fatal("an acked message was leased again")
	}
}

// A revoked lease goes back to retry with a growing, bounded backoff and one
// failure of its owner; after maxLeaseAttempts it is failed.
func TestLeaseRevokeBackoffAndFail(t *testing.T) {
	b := newLeaseBook("")
	now := time.Now()
	var prev time.Duration
	for i := 1; i <= maxLeaseAttempts; i++ {
		owner := "s1"
		if i%2 == 0 {
			owner = "s2"
		}
		if b.blocked("m1", owner, ViaQueue, now) {
			t.Fatalf("attempt %d blocked", i)
		}
		mustTake(t, b, "m1", "", owner, ViaQueue, "t", now.Add(time.Minute), now)
		failed, err := b.revoke(owner, []string{"m1"}, "wake_failed", now)
		if err != nil {
			t.Fatal(err)
		}
		l := leaseState(t, b, "m1")
		if i < maxLeaseAttempts {
			wait := l.NextAt.Sub(now)
			if l.State != LeaseRetry || len(failed) != 0 || wait < prev || wait > leaseBackoffMax {
				t.Fatalf("attempt %d: %+v failed %v", i, l, failed)
			}
			prev = wait
			if !b.blocked("m1", owner, ViaQueue, now) || b.blocked("m1", owner, ViaWaiter, now) {
				t.Fatalf("attempt %d: backoff blocks node wakes only", i)
			}
			now = l.NextAt
			continue
		}
		if l.State != LeaseFailed || len(failed) != 1 || l.Fails["s1"]+l.Fails["s2"] != maxLeaseAttempts {
			t.Fatalf("last attempt: %+v failed %v", l, failed)
		}
	}
	if !b.blocked("m1", "s3", ViaInbox, now) || b.blocked("m1", "s3", ViaHook, now) {
		t.Fatal("a failed message blocks automatic delivery only")
	}
	if !b.passedOver("m1", "s1") || b.passedOver("m1", "s3") {
		t.Fatal("passedOver")
	}
}

// A hook's lease counts no attempt and no owner failure.
func TestLeaseHookNotCounted(t *testing.T) {
	b := newLeaseBook("")
	now := time.Now()
	mustTake(t, b, "m1", "", "s1", ViaHook, "", now.Add(claimTTL), now)
	if _, err := b.revoke("s1", nil, "deadline", now); err != nil {
		t.Fatal(err)
	}
	if l := leaseState(t, b, "m1"); l.State != LeasePending || l.Attempts != 0 || len(l.Fails) != 0 {
		t.Fatalf("hook lease: %+v", l)
	}
}

// A lease past its deadline, or of a gone owner, is due to end; another
// owner may take it over past its deadline. A wake that put the message into
// a session's queue or inbox is never due by time, nor taken over.
func TestLeaseDueAndTakeOver(t *testing.T) {
	b := newLeaseBook("")
	now := time.Now()
	mustTake(t, b, "m1", "", launchOwner("x"), ViaLaunch, "", now.Add(time.Minute), now)
	mustTake(t, b, "m2", "", "s2", ViaQueue, "t2", now.Add(time.Hour), now)
	mustTake(t, b, "m3", "", "s1", ViaInbox, "t3", now.Add(time.Minute), now)
	later := now.Add(2 * time.Minute)
	ends := b.due(later, func(o string) bool { return o == "s2" })
	if len(ends) != 2 {
		t.Fatalf("due: %+v", ends)
	}
	for _, e := range ends {
		if (e.id == "m1" && e.reason != "deadline") || (e.id == "m2" && e.reason != "session_gone") || e.id == "m3" {
			t.Fatalf("due: %+v", ends)
		}
	}
	mustTake(t, b, "m1", "", launchOwner("y"), ViaLaunch, "", later.Add(time.Minute), later)
	if l := leaseState(t, b, "m1"); l.Owner != launchOwner("y") || l.Fails[launchOwner("x")] != 1 || l.Attempts != 2 {
		t.Fatalf("take over: %+v", l)
	}
	if ok, _ := b.take("m3", "", "s9", ViaInbox, "t9", later.Add(time.Minute), later); ok || b.queuedOwner("m3") != "s1" {
		t.Fatal("a queued wake was taken over by time")
	}
	// The same session woken again (its slow turn) is no failure of it.
	mustTake(t, b, "m3", "", "s1", ViaInbox, "t4", later.Add(time.Minute), later)
	if l := leaseState(t, b, "m3"); l.Fails["s1"] != 0 || l.Attempts != 2 || l.Token != "t4" {
		t.Fatalf("rewake: %+v", l)
	}
}

// A late proof is accepted after a revoke while no one acked the message,
// also when another owner leased it since.
func TestLeaseLateProof(t *testing.T) {
	b := newLeaseBook("")
	now := time.Now()
	mustTake(t, b, "m1", "", "s1", ViaQueue, "t1", now.Add(time.Minute), now)
	if _, err := b.revoke("s1", []string{"m1"}, "queue_error", now); err != nil {
		t.Fatal(err)
	}
	if got, _ := b.start("s1", "", "t1", []string{"m1"}, now); len(got) != 1 {
		t.Fatalf("late proof after a revoke: %v", got)
	}
	if l := leaseState(t, b, "m1"); l.State != LeaseRunning || l.Owner != "s1" || !l.NextAt.IsZero() {
		t.Fatalf("running: %+v", l)
	}
	mustTake(t, b, "m2", "", "s1", ViaQueue, "t1", now.Add(time.Minute), now)
	_, _ = b.revoke("s1", []string{"m2"}, "queue_error", now)
	mustTake(t, b, "m2", "", "s2", ViaQueue, "t2", now.Add(time.Minute), now.Add(time.Hour))
	if got, _ := b.start("s1", "", "", []string{"m2"}, now); len(got) != 1 || leaseState(t, b, "m2").Owner != "s1" {
		t.Fatalf("late proof over a new lease: %v %+v", got, leaseState(t, b, "m2"))
	}
	_ = b.ack("", []string{"m2"}, nil, now)
	if got, _ := b.start("s2", "", "", []string{"m2"}, now); len(got) != 0 {
		t.Fatalf("proof after the ack: %v", got)
	}
	if got, _ := b.start("s3", "", "", []string{"m1"}, now); len(got) != 0 {
		t.Fatalf("a stranger's proof: %v", got)
	}
}

// Failed is terminal for automatic paths: a hook may still deliver it, but
// its lapse leaves it failed, not in retry.
func TestLeaseFailedTerminal(t *testing.T) {
	b := newLeaseBook("")
	now := time.Now()
	mustTake(t, b, "m1", "", launchOwner(""), ViaLaunch, "", now.Add(time.Minute), now)
	if err := b.fail(launchOwner(""), []string{"m1"}, "launch_turn_error", now); err != nil {
		t.Fatal(err)
	}
	mustTake(t, b, "m1", "", "s1", ViaHook, "", now.Add(claimTTL), now)
	if failed, _ := b.revoke("s1", []string{"m1"}, "deadline", now); len(failed) != 0 {
		t.Fatalf("reported twice: %v", failed)
	}
	if l := leaseState(t, b, "m1"); l.State != LeaseFailed || !b.blocked("m1", "s2", ViaInbox, now.Add(time.Hour)) {
		t.Fatalf("resurrected: %+v", l)
	}
	if ok, _ := b.take("m1", "", launchOwner(""), ViaLaunch, "", now.Add(time.Minute), now.Add(time.Hour)); ok {
		t.Fatal("relaunched a failed message")
	}
}

// A held lease (its ack pending) is stale after leaseHoldMax.
func TestLeaseHoldBounded(t *testing.T) {
	b := newLeaseBook("")
	now := time.Now()
	if err := b.hold("s1", []string{"m1"}, now); err != nil {
		t.Fatal(err)
	}
	if s := b.staleHolds(now.Add(leaseHoldMax / 2)); len(s) != 0 {
		t.Fatalf("stale early: %+v", s)
	}
	if s := b.staleHolds(now.Add(leaseHoldMax + time.Second)); len(s) != 1 || s[0].owner != "s1" {
		t.Fatalf("stale: %+v", s)
	}
}

// The book survives a restart; a seat's lease is kept apart from the node's.
func TestLeasePersisted(t *testing.T) {
	dir := t.TempDir()
	b := newLeaseBook(dir)
	now := time.Now().UTC().Truncate(time.Second)
	mustTake(t, b, "m1", "", "s1", ViaWaiter, "tok", now.Add(time.Minute), now)
	mustTake(t, b, "m1", "seat1", seatOwner("seat1"), ViaSeat, "", now.Add(time.Minute), now)
	c := newLeaseBook(dir)
	if err := c.load(); err != nil {
		t.Fatal(err)
	}
	if l := leaseState(t, c, "m1"); l.State != LeaseLeased || l.Owner != "s1" || l.Token != "tok" || l.WaiterWakes != 1 || !l.Deadline.Equal(now.Add(time.Minute)) {
		t.Fatalf("restored: %+v", l)
	}
	if l := leaseState(t, c, seatKey("seat1", "m1")); l.Owner != seatOwner("seat1") || l.Seat != "seat1" {
		t.Fatalf("restored seat lease: %+v", l)
	}
	if err := c.ack("seat1", []string{"m1"}, map[string]bool{"m1": true}, now); err != nil {
		t.Fatal(err)
	}
	if leaseState(t, c, "m1").State != LeaseAcked || leaseState(t, c, seatKey("seat1", "m1")).State != LeaseAcked {
		t.Fatal("ack")
	}
}
