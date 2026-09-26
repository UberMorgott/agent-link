package node

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// A chat message that only informs this node opens a session in full mode
// alone, and only within the hop limit (0: none).
func TestAutonomyFullLaunchesForInformation(t *testing.T) {
	l := &fakeLauncher{}
	a, b := deliveryPair(t, t.TempDir(), nil, l)
	ctx := context.Background()
	chat, err := b.CreateChat([]string{"a"}, "")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "a knows chat", func() bool { _, ok := a.ChatOf(chat.ID); return ok })
	fyi, err := b.SendMessage(Message{ChatID: chat.ID, Body: "fyi: deployed", RootID: newID(), AutoDepth: 5})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "a has the fyi", func() bool { p, _ := a.Unread("", "", 10); return p.Total == 1 })
	later := time.Now().Add(launchGrace + time.Second)

	a.launchDue(ctx, later) // asked: only a question opens a session
	a.SetAutonomy(Autonomy{Mode: AutonomyFull, MaxDepth: 3})
	a.launchDue(ctx, later) // full, past the hop limit
	if got := l.all(); len(got) != 0 {
		t.Fatalf("launched: %+v", got)
	}
	a.SetAutonomy(Autonomy{Mode: AutonomyFull, MaxDepth: 0})
	a.launchDue(ctx, later)
	if got := l.all(); len(got) != 1 {
		t.Fatalf("full mode without a hop limit did not launch for the fyi: %+v", got)
	}
	waitAttempts(t, b, fyi, AttemptLaunchRequested)
	if st := a.AutonomyStatus(); st.TurnsLastHour != 1 {
		t.Fatalf("the launch was not counted: %+v", st)
	}
}

// The hop limit is the project's: a request past 3 hops pauses with 3, and
// none does with 0.
func TestAutonomyHopLimit(t *testing.T) {
	a, b := deliveryPair(t, t.TempDir(), nil, nil)
	chat, err := b.CreateChat([]string{"a"}, "")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "a knows chat", func() bool { _, ok := a.ChatOf(chat.ID); return ok })
	deep, err := b.SendMessage(Message{ChatID: chat.ID, Body: "four hops", Responders: []string{"a"}, RootID: newID(), AutoDepth: 4})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "a has it", func() bool { p, _ := a.Unread("", "", 10); return p.Total == 1 })
	paused := func() bool { p, _ := a.Unread("", "", 10); return p.Messages[0].Paused }
	if paused() || a.AutoHeld(deep) {
		t.Fatal("paused within the default hop limit")
	}
	a.SetAutonomy(Autonomy{Mode: AutonomyAsked, MaxDepth: 3})
	if !paused() || !a.AutoHeld(deep) {
		t.Fatal("not paused past the project's hop limit")
	}
	if ok, hold, _ := a.ClaimRun(deep); ok || hold != HoldAutoLimit {
		t.Fatalf("ClaimRun past the limit = %v %q", ok, hold)
	}
	a.SetAutonomy(Autonomy{Mode: AutonomyAsked, MaxDepth: 0})
	if paused() || a.AutoHeld(deep) {
		t.Fatal("paused with no hop limit")
	}
}

// Full mode's budgets: the turns of any hour and the minutes of continuous
// work. An exhausted one pauses autonomous delivery, tells the hook once and
// survives a restart; ResumeAutonomy starts over. Other modes count nothing.
func TestAutonomyBudgets(t *testing.T) {
	a, _ := deliveryPair(t, t.TempDir(), nil, nil)
	var told atomic.Int32
	a.SetAutonomyPauseHook(func(reason string) {
		if reason == PauseTurns || reason == PauseRun {
			told.Add(1)
		}
	})
	t0 := time.Now()
	for i := range 50 {
		if !a.autoTake(t0.Add(time.Duration(i) * time.Second)) {
			t.Fatal("asked mode is budgeted")
		}
	}
	a.SetAutonomy(Autonomy{Mode: AutonomyFull, TurnsPerHour: 3, MaxRun: time.Hour})
	for i := range 3 {
		if !a.autoTake(t0.Add(time.Duration(i) * time.Minute)) {
			t.Fatalf("turn %d refused", i)
		}
	}
	if a.autoTake(t0.Add(3*time.Minute)) || a.autoTake(t0.Add(4*time.Minute)) || a.autoOK() {
		t.Fatal("a 4th turn in the hour was taken")
	}
	if st := a.AutonomyStatus(); !st.Paused || st.Reason != PauseTurns || told.Load() != 1 {
		t.Fatalf("status %+v, told %d", st, told.Load())
	}
	restarted := newAutonomy(a.cfg.DataDir)
	if err := restarted.load(); err != nil || !restarted.st.Paused || restarted.st.Reason != PauseTurns {
		t.Fatalf("pause not kept: %+v %v", restarted.st, err)
	}

	a.ResumeAutonomy()
	if !a.autoOK() || a.AutonomyStatus().TurnsLastHour != 0 {
		t.Fatal("resume did not start over")
	}
	// Continuous work: turns 10 minutes apart run on; past 30 minutes it pauses.
	a.SetAutonomy(Autonomy{Mode: AutonomyFull, TurnsPerHour: 100, MaxRun: 30 * time.Minute})
	t1 := t0.Add(2 * time.Hour)
	for _, m := range []int{0, 10, 20, 29} {
		if !a.autoTake(t1.Add(time.Duration(m) * time.Minute)) {
			t.Fatalf("turn at +%dm refused", m)
		}
	}
	if a.autoTake(t1.Add(31*time.Minute)) || a.AutonomyStatus().Reason != PauseRun || told.Load() != 2 {
		t.Fatalf("work past its minutes went on: %+v", a.AutonomyStatus())
	}
	a.ResumeAutonomy()
	// A quiet gap starts a new run.
	t2 := t1.Add(time.Hour)
	for _, m := range []int{0, 20, 40, 60} {
		if !a.autoTake(t2.Add(time.Duration(m) * time.Minute)) {
			t.Fatalf("turn after a quiet gap at +%dm refused", m)
		}
	}
}

// The emergency stop releases every active automatic lease without counting
// a failure, takes no automatic lease (a hook's still), opens nothing and
// runs no worker; switched off, all of it works again and a late proof of the
// released owner still counts.
func TestStopReleasesAndBlocks(t *testing.T) {
	l := &fakeLauncher{}
	a, b := deliveryPair(t, t.TempDir(), nil, l)
	ctx := context.Background()
	m := ask(t, a, b, "stop me")
	now := time.Now()
	if took, err := a.leases.take(m.ID, "", "s1", ViaInbox, "tok1", now.Add(time.Minute), now); err != nil || !took {
		t.Fatalf("take = %v %v", took, err)
	}
	a.SetStopped(true)
	if !a.Stopped() || a.autoOK() || a.autoTake(now) || !a.AutoHeld(m) {
		t.Fatal("not stopped")
	}
	le, _ := a.leases.get(m.ID)
	if le.State != LeaseRetry || le.Reason != "stopped" || le.Attempts != 0 || len(le.Fails) != 0 || le.Owner != "s1" {
		t.Fatalf("lease after the stop: %+v", le)
	}
	if took, _ := a.leases.take("other", "", "s2", ViaQueue, "tok2", now.Add(time.Minute), now); took {
		t.Fatal("an automatic lease was taken while stopped")
	}
	if took, _ := a.leases.take("other", "", "s2", ViaHook, "", now.Add(time.Minute), now); !took {
		t.Fatal("a hook's lease was refused while stopped")
	}
	a.launchDue(ctx, now.Add(launchGrace+time.Second))
	if len(l.all()) != 0 {
		t.Fatal("launched while stopped")
	}
	if p, _ := a.Unread("", "", 10); p.Total != 1 {
		t.Fatal("the message did not stay unread")
	}

	a.SetStopped(false)
	if got, _ := a.leases.start("s1", "", "tok1", []string{m.ID}, now); len(got) != 1 {
		t.Fatal("the released owner's late proof did not count")
	}
	if took, _ := a.leases.take("third", "", "s3", ViaQueue, "tok3", now.Add(time.Minute), now); !took {
		t.Fatal("no automatic lease after the stop ended")
	}
}
