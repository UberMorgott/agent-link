package node

import (
	"testing"
	"time"
)

// dormantNode is a running local chat's node with a waker, so its wake loop
// runs every every, or every dormantEvery while dormant; it is dormant from the start.
func dormantNode(t *testing.T, every, dormantEvery time.Duration) (*testNode, string) {
	t.Helper()
	tn := newTestNode(t, "a", testSecret, nil, t.TempDir(), listen(t), nil)
	tn.cfg.LocalOnly = true
	dir := t.TempDir()
	tn.SetFolders(dir, nil)
	tn.SetSessionWaker(&fakeWaker{})
	tn.wakeEvery, tn.dormantEvery = every, dormantEvery
	tn.start(t)
	eventually(t, "first pass", func() bool { return tn.wakeTicks.Load() >= 1 })
	if !tn.dormant(time.Now()) {
		t.Fatal("an empty local chat must be dormant")
	}
	return tn, dir
}

// ticksWithin is how many passes the wake loop runs within d.
func ticksWithin(tn *testNode, d time.Duration) int64 {
	before := tn.wakeTicks.Load()
	time.Sleep(d)
	return tn.wakeTicks.Load() - before
}

// A local chat's node with nothing to do polls every wakeDormant; a live
// session puts it back on the fast cadence, and its end makes it dormant again.
func TestWakeLoopDormantCadence(t *testing.T) {
	tn, dir := dormantNode(t, 10*time.Millisecond, time.Hour)
	if n := ticksWithin(tn, 150*time.Millisecond); n > 1 {
		t.Fatalf("dormant loop ran %d passes in 150ms", n)
	}
	if _, err := tn.RegisterSession(SessionRequest{SessionID: "s1", Provider: ProviderCodex, Folder: dir, Idle: true}); err != nil {
		t.Fatal(err)
	}
	if tn.dormant(time.Now()) {
		t.Fatal("a live session must wake the loop")
	}
	if n := ticksWithin(tn, 150*time.Millisecond); n < 5 {
		t.Fatalf("awake loop ran only %d passes in 150ms (every 10ms)", n)
	}
	if err := tn.EndSession("s1"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "dormant again", func() bool { return tn.dormant(time.Now()) })
	time.Sleep(30 * time.Millisecond) // the pass after the end sees it dormant
	if n := ticksWithin(tn, 150*time.Millisecond); n > 1 {
		t.Fatalf("loop still ran %d passes in 150ms after the session ended", n)
	}
}

// Any change of a dormant node runs its wake loop at once, not after
// wakeDormant: a message's delivery does not wait for the dormant period.
func TestWakeLoopKickedByActivity(t *testing.T) {
	tn, dir := dormantNode(t, time.Hour, time.Hour)
	before := tn.wakeTicks.Load()
	if _, err := tn.RegisterSession(SessionRequest{SessionID: "s1", Provider: ProviderCodex, Folder: dir, Idle: true}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a pass right after the change", func() bool { return tn.wakeTicks.Load() > before })
}

// A network project's node is never dormant: it polls every wakePoll as before.
func TestNetworkNodeNeverDormant(t *testing.T) {
	tn := newTestNode(t, "a", testSecret, nil, t.TempDir(), listen(t), nil)
	if tn.dormant(time.Now()) {
		t.Fatal("a network node must not be dormant")
	}
}
