package node

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

// A member's request for its own seat and that seat's answer are visible in
// the shared project chat, but neither invites another member's Full agent.
// Explicitly asking that member still launches its agent.
func TestAutonomyFullIgnoresPeerSeatConversation(t *testing.T) {
	p := newTestProject(t)
	lnA, lnB := listen(t), listen(t)
	a := newProjectNode(t, "a", p, lnA, map[string]net.Listener{"b": lnB})
	b := newProjectNode(t, "b", p, lnB, nil)
	seatRuns, remoteRuns := &seatLauncher{}, &fakeLauncher{}
	a.SetFolders(t.TempDir(), nil)
	a.SetLauncher(seatRuns, ProviderCodex)
	a.seatBusy = seatRuns.busy
	b.SetFolders(t.TempDir(), nil)
	b.SetLauncher(remoteRuns, ProviderCodex)
	b.SetAutonomy(Autonomy{Mode: AutonomyFull, MaxDepth: -1})
	a.start(t)
	b.start(t)
	eventually(t, "a<->b connected", func() bool { return a.Connected("b") && b.Connected("a") })
	seat, err := a.AddSeat(SeatRequest{Provider: ProviderCodex})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "local seat ready", func() bool { return seatByLabel(t, a, "Codex").SessionID != "" })
	chat, err := a.NewProjectChat([]string{"b"})
	if err != nil {
		t.Fatal(err)
	}
	question, err := a.SendRequest(SendRequest{ChatID: chat.ID, Body: "Codex, inspect this", AuthorKind: AuthorHuman, AskSeats: []string{seat.ID}})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "remote sees seat question", func() bool { return slices.Contains(chatIDs(b, chat.ID), question.ID) })
	if eligible, _ := b.launchable(b.folders.work, time.Now().Add(launchGrace+time.Second)); len(eligible) != 0 {
		t.Fatalf("peer seat request made remote agent eligible: %+v", eligible)
	}
	reply, err := a.SendRequest(SendRequest{ChatID: chat.ID, Body: "Inspection complete", ReplyTo: question.ID, Seat: seat.ID})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "remote sees seat answer", func() bool { return slices.Contains(chatIDs(b, chat.ID), reply.ID) })
	b.launchDue(context.Background(), time.Now().Add(launchGrace+time.Second))
	if got := remoteRuns.all(); len(got) != 0 {
		t.Fatalf("remote agent launched for local seat conversation: %+v", got)
	}
	msgs, err := b.ChatMessages(chat.ID, 0, 0, 100)
	if err != nil || !slices.ContainsFunc(msgs, func(m ChatMessage) bool { return m.ID == question.ID }) ||
		!slices.ContainsFunc(msgs, func(m ChatMessage) bool { return m.ID == reply.ID }) {
		t.Fatalf("shared history lost seat conversation: %+v, %v", msgs, err)
	}
	asked, err := a.SendRequest(SendRequest{ChatID: chat.ID, Body: "b, review this", Ask: []string{"b"}, Seat: seat.ID})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "remote sees explicit ask", func() bool { return slices.Contains(chatIDs(b, chat.ID), asked.ID) })
	b.launchDue(context.Background(), time.Now().Add(launchGrace+time.Second))
	if got := remoteRuns.all(); len(got) != 1 {
		t.Fatalf("explicit ask did not launch remote agent: %+v", got)
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

// A global pause keeps messages visible to the owner but does not deliver
// them to a live agent session until the owner resumes all agents.
func TestStopHoldsSessionDelivery(t *testing.T) {
	a, b := pair(t, testSecret, testSecret)
	dir := t.TempDir()
	if _, err := a.RegisterSession(SessionRequest{SessionID: "s-paused", Provider: "claude", Folder: dir, Wake: WakeRewake}); err != nil {
		t.Fatal(err)
	}
	m, err := b.SendRequest(SendRequest{To: "a", Body: "wait for resume", AuthorKind: AuthorAgent})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "a has the message", func() bool { p, _ := a.Unread("", "", 10); return p.Total == 1 })
	a.SetStopped(true)
	queued, err := b.SendRequest(SendRequest{To: "a", Body: "sent during pause", AuthorKind: AuthorHuman})
	if err != nil {
		t.Fatalf("human chat send during pause: %v", err)
	}
	eventually(t, "human message reached paused node", func() bool { p, _ := a.Unread("", "", 10); return p.Total == 2 })
	if p, err := a.UnreadFor(dir, "s-paused", "", 10); err != nil || p.Total != 0 || len(p.Woken) != 0 {
		t.Fatalf("paused session received messages: %+v, %v", p, err)
	}
	if got, err := a.Claim(ClaimRequest{IDs: []string{m.ID}, SessionID: "s-paused"}); err != nil || len(got) != 0 {
		t.Fatalf("paused session claimed messages: %v, %v", got, err)
	}
	if p, _ := a.Unread("", "", 10); p.Total != 2 {
		t.Fatalf("owner lost the pending message: %+v", p)
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "http://127.0.0.1/unread?agent=1", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	a.APIHandler().ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"messages":[]`) {
		t.Fatalf("paused MCP agent read = %d %s", w.Code, w.Body.String())
	}
	a.SetStopped(false)
	if p, err := a.UnreadFor(dir, "s-paused", "", 10); err != nil || p.Total != 2 ||
		(p.Messages[0].ID != m.ID && p.Messages[1].ID != m.ID) ||
		(p.Messages[0].ID != queued.ID && p.Messages[1].ID != queued.ID) {
		t.Fatalf("resumed session did not receive message: %+v, %v", p, err)
	}
}

func TestStopHoldsWaitUntilResume(t *testing.T) {
	a, b := pair(t, testSecret, testSecret)
	m, err := b.SendRequest(SendRequest{To: "a", Body: "deferred", AuthorKind: AuthorHuman})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "a has the message", func() bool { p, _ := a.Unread("", "", 10); return p.Total == 1 })
	a.SetStopped(true)
	w := httptest.NewRecorder()
	a.handleWait(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/wait?timeout=10ms", nil))
	if w.Code != http.StatusNoContent {
		t.Fatalf("paused wait = %d %s", w.Code, w.Body.String())
	}
	a.SetStopped(false)
	w = httptest.NewRecorder()
	a.handleWait(w, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/wait?timeout=10ms", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), m.ID) {
		t.Fatalf("resumed wait = %d %s", w.Code, w.Body.String())
	}
}

func TestSeatAddedDuringStopStartsAfterResume(t *testing.T) {
	l := &seatLauncher{}
	a := seatNode(t, t.TempDir(), t.TempDir(), l)
	a.SetStopped(true)
	seat, err := a.AddSeat(SeatRequest{Provider: ProviderCodex})
	if err != nil || seat.ID == "" || len(l.all()) != 0 {
		t.Fatalf("paused seat started: %+v, runs=%d, err=%v", seat, len(l.all()), err)
	}
	a.SetStopped(false)
	a.seatsDue(context.Background(), time.Now())
	eventually(t, "deferred seat introduction", func() bool {
		return seatByLabel(t, a, "Codex").SessionID != ""
	})
	if len(l.all()) != 1 {
		t.Fatalf("resumed seat runs = %d", len(l.all()))
	}
}

// A turn is spent only by work: a wake whose claim comes back empty (the
// message's lease is in its backoff, another handler has it) gives its turn
// back, so a loop that finds nothing to hand over never pauses the project.
func TestAutonomyEmptyClaimCostsNoTurn(t *testing.T) {
	p := &fakePoster{}
	dir := t.TempDir()
	a, b := deliveryPair(t, dir, p, nil)
	a.SetAutonomy(Autonomy{Mode: AutonomyFull, TurnsPerHour: 3})
	regClaude(t, a, dir, "s-c", testSocket, true)
	m := ask(t, a, b, "in its backoff")
	now := time.Now()
	if took, err := a.leases.take(m.ID, "", "s-c", ViaInbox, "tok12345", now.Add(time.Minute), now); err != nil || !took {
		t.Fatalf("take = %v %v", took, err)
	}
	if _, err := a.leases.revoke("s-c", []string{m.ID}, "wake_failed", now); err != nil {
		t.Fatal(err)
	}
	for range 10 {
		a.wakeIdle(context.Background())
	}
	if st := a.AutonomyStatus(); st.Paused || st.TurnsLastHour != 0 || st.Run != 0 || len(p.calls) != 0 {
		t.Fatalf("empty claims spent turns: %+v, posts %v", st, p.calls)
	}
	// Nor does a pass over a message in its backoff write the budget state.
	if _, err := os.Stat(filepath.Join(a.cfg.DataDir, autonomyFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("passes with nothing to claim wrote %s: %v", autonomyFile, err)
	}
	backoffOver(a, m.ID)
	a.wakeIdle(context.Background())
	if st := a.AutonomyStatus(); st.TurnsLastHour != 1 || len(p.calls) != 1 {
		t.Fatalf("the wake was not counted once: %+v, posts %v", st, p.calls)
	}

	// The refund takes back exactly its own turn and the run it started.
	a.ResumeAutonomy()
	t0 := time.Now().Add(-10 * time.Minute)
	if !a.autoTake(t0) || !a.autoTake(t0.Add(time.Minute)) {
		t.Fatal("turns refused")
	}
	a.autoRefund(t0.Add(time.Minute))
	a.autoRefund(t0.Add(time.Minute)) // twice: nothing more to give back
	a.auto.mu.Lock()
	st := a.auto.st
	a.auto.mu.Unlock()
	if len(st.Turns) != 1 || !st.LastTurn.Equal(t0) || !st.RunStart.Equal(t0) {
		t.Fatalf("after the refund: %+v", st)
	}
	a.autoRefund(t0)
	a.auto.mu.Lock()
	st = a.auto.st
	a.auto.mu.Unlock()
	if len(st.Turns) != 0 || !st.LastTurn.IsZero() || !st.RunStart.IsZero() {
		t.Fatalf("after the last refund: %+v", st)
	}
}

// Two turns counted at one instant (seatsDue takes every seat's with one now):
// the refund of one leaves the other, its LastTurn and the run it started, so
// the next turn does not pause for a run without a start.
func TestAutonomyRefundKeepsTurnOfSameInstant(t *testing.T) {
	a, _ := deliveryPair(t, t.TempDir(), nil, nil)
	a.SetAutonomy(Autonomy{Mode: AutonomyFull, TurnsPerHour: 30})
	now := time.Now()
	for range 2 {
		if !a.autoTake(now) {
			t.Fatal("turn refused")
		}
	}
	a.autoRefund(now) // one seat's claim came back empty
	a.auto.mu.Lock()
	st := a.auto.st
	a.auto.mu.Unlock()
	if len(st.Turns) != 1 || !st.LastTurn.Equal(now) || !st.RunStart.Equal(now) {
		t.Fatalf("after the refund of one of two: %+v", st)
	}
	if !a.autoTake(now.Add(time.Minute)) {
		t.Fatalf("the next turn paused: %+v", a.AutonomyStatus())
	}
	if s := a.AutonomyStatus(); s.Paused || s.TurnsLastHour != 2 {
		t.Fatalf("status %+v", s)
	}
}

// A turns pause is a rate limit: it ends by itself once the hour has room for
// a turn again and pauses again when it fills up, which at the budget is every
// freed turn: the owner is told of it once an hour, not each time. A run pause
// waits for the owner.
func TestAutonomyTurnsPauseEndsByItself(t *testing.T) {
	a, _ := deliveryPair(t, t.TempDir(), nil, nil)
	var told atomic.Int32
	a.SetAutonomyPauseHook(func(string) { told.Add(1) })
	a.SetAutonomy(Autonomy{Mode: AutonomyFull, TurnsPerHour: 3})
	now := time.Now()
	for _, ago := range []time.Duration{58 * time.Minute, 30 * time.Minute, 10 * time.Minute} {
		if !a.autoTake(now.Add(-ago)) {
			t.Fatalf("turn %v ago refused", ago)
		}
	}
	if a.autoTake(now) || a.autoOK() || a.holdReason() != HoldPaused+PauseTurns || told.Load() != 1 {
		t.Fatalf("not paused at the budget: %+v", a.AutonomyStatus())
	}
	// Three minutes on the oldest turn left the hour: room for one.
	later := now.Add(3 * time.Minute)
	if paused, _ := a.autoPaused(later); paused {
		t.Fatal("still paused with room in the hour")
	}
	if st := a.AutonomyStatus(); st.Paused || st.Reason != "" || !st.PausedAt.IsZero() || !a.autoOK() || a.holdReason() != "" {
		t.Fatalf("after the pause ran out: %+v", st)
	}
	restarted := newAutonomy(a.cfg.DataDir)
	if err := restarted.load(); err != nil || restarted.st.Paused {
		t.Fatalf("the ended pause was kept: %+v %v", restarted.st, err)
	}
	if !a.autoTake(later) || a.autoTake(later.Add(time.Minute)) || !a.AutonomyStatus().Paused || told.Load() != 1 {
		t.Fatalf("the budget does not hold after the pause, or it was told again within the hour: %+v, told %d", a.AutonomyStatus(), told.Load())
	}
	// When it was told survives a restart, and an hour after it a pause is told again.
	if restarted = newAutonomy(a.cfg.DataDir); restarted.load() != nil || !restarted.st.ToldAt.Equal(now) {
		t.Fatalf("told at %v, want %v", restarted.st.ToldAt, now)
	}
	hour := now.Add(pauseToldEvery + 2*time.Minute)
	if !a.autoTake(hour) || !a.autoTake(hour.Add(time.Second)) || a.autoTake(hour.Add(2*time.Second)) || told.Load() != 2 {
		t.Fatalf("a pause an hour after the last one told: %+v, told %d", a.AutonomyStatus(), told.Load())
	}
	// A pause for continuous work stays until the owner resumes.
	a.ResumeAutonomy()
	a.SetAutonomy(Autonomy{Mode: AutonomyFull, TurnsPerHour: 100, MaxRun: 20 * time.Minute})
	for _, m := range []int{0, 10, 19} {
		if !a.autoTake(now.Add(time.Duration(m) * time.Minute)) {
			t.Fatalf("turn at +%dm refused", m)
		}
	}
	if a.autoTake(now.Add(21 * time.Minute)) {
		t.Fatal("work past its minutes went on")
	}
	if paused, reason := a.autoPaused(now.Add(10 * time.Hour)); !paused || reason != PauseRun {
		t.Fatalf("a run pause ended by itself: %v %q", paused, reason)
	}
}

// While a budget pauses autonomous delivery the node wakes nobody, so it does
// not hold a live Claude session's waiter off: the waiter sees the message and
// wakes the session with it.
func TestPausedAutonomyLeavesWakeToWaiter(t *testing.T) {
	p := &fakePoster{}
	dir := t.TempDir()
	a, b := deliveryPair(t, dir, p, nil)
	a.SetAutonomy(Autonomy{Mode: AutonomyFull, TurnsPerHour: 100, MaxRun: time.Minute})
	regClaude(t, a, dir, "s-c", testSocket, true)
	m := ask(t, a, b, "while paused")
	q := url.Values{"waiter": {"1"}, "session": {"s-c"}, "folder": {dir}, "actionable": {"1"}}
	if page, err := a.UnreadQuery(q); err != nil || len(page.Messages) != 0 || !a.InboxWakes("s-c") {
		t.Fatalf("the waiter is not held off while the node wakes: %+v %v", page, err)
	}
	// A run pause: it never ends by itself.
	t0 := time.Now().Add(-10 * time.Minute)
	if !a.autoTake(t0) || a.autoTake(t0.Add(5*time.Minute)) || a.autoOK() || a.AutonomyStatus().Reason != PauseRun {
		t.Fatalf("not paused: %+v", a.AutonomyStatus())
	}
	if a.InboxWakes("s-c") {
		t.Fatal("the node holds the inbox while it wakes nobody")
	}
	page, err := a.UnreadQuery(q)
	if err != nil || len(page.Messages) != 1 || page.Messages[0].ID != m.ID {
		t.Fatalf("the waiter sees nothing while paused: %+v %v", page, err)
	}
	if g, err := a.Claim(ClaimRequest{IDs: []string{m.ID}, SessionID: "s-c", WakeToken: "0123456789abcdef"}); err != nil || !slices.Equal(g, []string{m.ID}) {
		t.Fatalf("the waiter's claim while paused: %v %v", g, err)
	}
	if v := leaseViewOf(t, a, m.ID); v.Owner != "s-c" || v.Via != ViaWaiter || p.count() != 0 {
		t.Fatalf("lease %+v, posts %v", v, p.calls)
	}
}
