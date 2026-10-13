package node

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// turnLine is a rollout's turn record of kind (task_started, task_complete,
// turn_aborted) at at, as Codex writes it.
func turnLine(kind string, at time.Time) string {
	return fmt.Sprintf(`{"timestamp":%q,"ordinal":7,"type":"event_msg","payload":{"type":%q,"turn_id":"t-1"}}`+"\n",
		at.UTC().Format("2006-01-02T15:04:05.000Z"), kind)
}

// rollout creates the thread's rollout with text and returns its path.
func (q *staleQueue) rollout(text string) string {
	q.t.Helper()
	dir := filepath.Join(q.home, "sessions", "2026", "10", "13")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		q.t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-x-s-q.jsonl")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		q.t.Fatal(err)
	}
	return path
}

// wrote appends text to the rollout at path, as the thread does.
func (q *staleQueue) wrote(path, text string) {
	q.t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // a test's own file
	if err != nil {
		q.t.Fatal(err)
	}
	if _, err := f.WriteString(text); err != nil {
		q.t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		q.t.Fatal(err)
	}
}

// A thread whose hooks say nothing is in a turn by its rollout alone: nothing
// is queued behind that turn, and what came during it goes into one wake once
// the rollout shows the turn's end.
func TestRolloutTurnHoldsQueueWake(t *testing.T) {
	for _, end := range []string{"task_complete", "turn_aborted"} {
		t.Run(end, func(t *testing.T) {
			q := newStaleQueue(t)
			// A record of megabytes before it (tool output) hides nothing.
			path := q.rollout(`{"timestamp":"2026-10-13T10:00:00.000Z","type":"response_item","payload":{"type":"message","text":"` +
				strings.Repeat("x", 200<<10) + `"}}` + "\n" + turnLine("task_started", time.Now().Add(time.Second)))
			bodies := []string{"first during the turn", "second during the turn", "third during the turn"}
			var sent []Message
			for _, body := range bodies {
				sent = append(sent, q.send(body))
				q.pass()
				if s := q.session(); q.w.count() != 0 || s.Woken || s.Wakes != 0 {
					t.Fatalf("queued behind a running turn: %v %+v", q.w.calls, s)
				}
			}
			q.age(staleWakeWait(maxIdleWakes) + time.Second) // waiting spends no wake
			if q.w.count() != 0 {
				t.Fatalf("queued behind a running turn after a wait: %v", q.w.calls)
			}
			// Their authors hear why they wait, once, also after a restart (the
			// node's own memory of what it reported is gone, the session's is not).
			for _, m := range sent {
				waitAttempts(t, q.b, m, AttemptHeld+":"+HoldBusyTurn)
			}
			var saved []Session
			if err := readJSON(q.a.sess.path, &saved); err != nil || len(saved) != 1 || len(saved[0].HeldNoted) != len(sent) {
				t.Fatalf("the notice is not saved: %v %+v", err, saved)
			}
			q.a.deliv.mu.Lock()
			clear(q.a.deliv.sent)
			q.a.deliv.mu.Unlock()
			q.pass()
			q.wrote(path, turnLine(end, time.Now().Add(2*time.Second)))
			q.pass()
			text := q.lastWake()
			if s := q.session(); q.w.count() != 1 || s.Wakes != 1 {
				t.Fatalf("wakes after the turn: %v %+v", q.w.calls, s)
			}
			for i, body := range bodies {
				if !strings.Contains(text, body) {
					t.Fatalf("the one wake lacks %q: %s", body, text)
				}
				if v := leaseViewOf(t, q.a, sent[i].ID); v.State != LeaseLeased || v.Owner != "s-q" || v.Via != ViaQueue || v.Attempts != 1 {
					t.Fatalf("lease of %q: %+v", body, v)
				}
				waitAttempts(t, q.b, sent[i], AttemptHeld+":"+HoldBusyTurn, AttemptWakeRequested)
			}
			q.pass()
			if q.w.count() != 1 {
				t.Fatalf("queued again with nothing new: %v", q.w.calls)
			}
		})
	}
}

// A prompt of the node still in the thread's queue is its pending wake: once
// the turn it waited behind ends, the thread runs it, so the wait before a
// wake for a newer message counts from that end.
func TestQueuedPromptHoldsNextWake(t *testing.T) {
	q := newStaleQueue(t)
	path := q.rollout("")
	q.send("request the prompt carries")
	q.pass()
	if q.w.count() != 1 || !q.session().Woken {
		t.Fatalf("first wake: %v", q.w.calls)
	}
	q.send("request behind the prompt")
	q.wrote(path, turnLine("task_complete", time.Now().Add(-time.Minute)))
	q.age(5 * time.Minute) // the wake is older than its grace, the turn's end is not
	if q.w.count() != 1 {
		t.Fatalf("a second prompt beside the unconsumed one: %v", q.w.calls)
	}
}

// A thread that ends turn after turn and never runs the pending prompt holds
// the wake of a newer message for one turn end only: the wake comes after the
// waits of wakeDue and one grace, however many turns end meanwhile.
func TestTurnEndsDoNotStarveNewerMessage(t *testing.T) {
	q := newStaleQueue(t)
	path := q.rollout("")
	q.send("request the prompt carries")
	q.pass()
	token := wakeTokenOf(q.lastWake())
	if q.w.count() != 1 {
		t.Fatalf("first wake: %v", q.w.calls)
	}
	behind := q.send("request behind the prompt")
	q.wrote(path, turnLine("task_complete", time.Now().Add(-90*time.Second)))
	q.age(3 * time.Minute) // the first end after the wake: its grace is waited for
	if _, ok := q.a.leases.get(behind.ID); ok || q.w.count() != 1 {
		t.Fatalf("a second wake inside the grace of the turn's end: %v", q.w.calls)
	}
	q.wrote(path, turnLine("task_complete", time.Now().Add(-90*time.Second)))
	q.age(3 * time.Minute) // a later end holds nothing
	// The wake is a lease under the pointer still in the queue (one at a time).
	if s := q.session(); q.w.count() != 1 || s.Wakes != 2 || !q.covers(token, behind) {
		t.Fatalf("the newer message waits behind a second turn end: %v %+v", q.w.calls, s)
	}
	// The wakes of a thread that goes on like that run out as a silent one's.
	limit := maxIdleWakes + maxStaleWakes
	for i := 2; i < limit; i++ {
		q.send(fmt.Sprintf("later request %d", i))
		for range 3 {
			q.wrote(path, turnLine("task_complete", time.Now().Add(-90*time.Second)))
			q.age(staleWakeWait(i) + time.Second)
		}
		if s := q.session(); q.w.count() != 1 || s.Wakes != i+1 {
			t.Fatalf("wake %d of the thread: %d %+v", i+1, q.w.count(), s)
		}
	}
}

// A prompt queued behind a turn the hooks do not report is no silent session's:
// while the rollout shows that turn running its lease stays, and it fails
// (queued_stale) only queuedOwnerMax after the thread fell silent.
func TestQueuedPromptBehindLiveTurnIsNotStale(t *testing.T) {
	q := newStaleQueue(t)
	path := q.rollout("")
	m := q.send("request the prompt carries")
	q.pass()
	q.wrote(path, turnLine("task_started", time.Now().Add(time.Second)))
	q.age(queuedOwnerMax + time.Second)
	if v := leaseViewOf(t, q.a, m.ID); v.State != LeaseLeased {
		t.Fatalf("lease behind a running turn: %+v", v)
	}
	old := time.Now().Add(-rolloutTurnQuiet - time.Minute)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	q.age(queuedOwnerMax + time.Second)
	if v := leaseViewOf(t, q.a, m.ID); v.State != LeaseFailed || v.Reason != "queued_stale" {
		t.Fatalf("lease of a thread silent inside its turn: %+v", v)
	}
}

// A pointer whose lease failed unproven still starts once the thread gets to
// it: while the rollout shows a turn, the wake is observed again, and its
// prompt in the rollout ends the wake count; its fetch delivers the message,
// failed or not.
func TestLateStartOfStalePromptIsProven(t *testing.T) {
	q := newStaleQueue(t)
	path := q.rollout("")
	m := q.send("request the prompt carries")
	q.pass()
	q.age(queuedOwnerMax + time.Second)
	l, _ := q.a.leases.get(m.ID)
	if l.State != LeaseFailed || l.Reason != "queued_stale" || l.Token == "" {
		t.Fatalf("lease of the silent thread: %+v", l)
	}
	s := q.session()
	if q.a.queueLateStart(s, l.Token, []string{m.ID}, time.Now()) {
		t.Fatal("a late start expected of a thread in no turn")
	}
	q.wrote(path, turnLine("task_started", time.Now())+WakeMarker(l.Token)+"\n")
	if !q.a.queueLateStart(s, l.Token, []string{m.ID}, time.Now()) {
		t.Fatal("no late start expected of a thread in a turn")
	}
	q.a.observeLateStarts(t.Context(), time.Now())
	// The proof ends the wake count, and the observer with it.
	eventually(t, "the observer ended", func() bool {
		_, on := q.a.observed.Load("s-q\x00" + l.Token)
		return !on && !q.session().Woken
	})
	// The pointer's prompt read nothing; the turn's fetch does.
	if got, _ := q.a.leases.get(m.ID); got.State != LeaseFailed {
		t.Fatalf("lease after the late pointer's prompt: %+v", got)
	}
	if page := q.fetch(); !page.Delivered || len(page.Messages) != 1 || page.Messages[0].ID != m.ID {
		t.Fatalf("the late pointer's fetch: %+v", page)
	}
	got, _ := q.a.leases.get(m.ID)
	if p, _ := q.a.Unread("", "", 100); got.State != LeaseAcked || slices.ContainsFunc(p.Messages, func(u UnreadMessage) bool { return u.ID == m.ID }) {
		t.Fatalf("after the late fetch: %+v", got)
	}
}

// The same for a prompt of a node before pointers, which carried its message:
// its late start still starts and acknowledges it.
func TestLateStartOfOldPromptAcknowledges(t *testing.T) {
	q := newStaleQueue(t)
	path := q.rollout("")
	m := q.send("request the old prompt carries")
	const token = "0123456789abcdef"
	now := time.Now()
	if ok, err := q.a.leases.take(m.ID, "", "s-q", ViaQueue, token, now.Add(inboxWakeGrace), now); err != nil || !ok {
		t.Fatalf("take: %v %v", ok, err)
	}
	if err := q.a.leases.fail("s-q", []string{m.ID}, "queued_stale", now); err != nil {
		t.Fatal(err)
	}
	q.wrote(path, turnLine("task_started", time.Now())+WakeMarker(token)+"\n")
	q.a.observeLateStarts(t.Context(), time.Now())
	eventually(t, "the late start acknowledged", func() bool {
		got, _ := q.a.leases.get(m.ID)
		p, _ := q.a.Unread("", "", 100)
		return got.State == LeaseAcked && !slices.ContainsFunc(p.Messages, func(u UnreadMessage) bool { return u.ID == m.ID })
	})
}

// Only a hook event that says idle moves IdleAt: after a heartbeat and an
// activity report later than the turn's start (which move LastSeen and
// LastActive), the turn is still one the hooks did not report.
func TestIdleAtIsTheLastIdleHookEvent(t *testing.T) {
	q := newStaleQueue(t)
	idleAt := q.session().IdleAt
	if idleAt.IsZero() {
		t.Fatal("no IdleAt after an idle registration")
	}
	m := q.send("request during the turn")
	start := idleAt.Add(5 * time.Millisecond)
	q.rollout(turnLine("task_started", start))
	for !time.Now().After(start.Add(5 * time.Millisecond)) {
		time.Sleep(time.Millisecond)
	}
	if _, err := q.a.RegisterSession(SessionRequest{SessionID: "s-q", Provider: ProviderCodex, Folder: q.dir, Wake: WakeQueue,
		Idle: true, Heartbeat: true, CodexHome: q.home, TTLSec: staleQueueTTL}); err != nil {
		t.Fatal(err)
	}
	if s := q.session(); !s.IdleAt.Equal(idleAt) || !s.LastSeen.After(start) || s.LastActive.After(start) {
		t.Fatalf("after a heartbeat: %+v", s)
	}
	if _, err := q.a.SessionActivity(m.ChatID, ActivityRequest{SessionID: "s-q", Type: "edit", Text: "Edit schema.sql"}); err != nil {
		t.Fatal(err)
	}
	if s := q.session(); !s.IdleAt.Equal(idleAt) || !s.LastActive.After(start) || !s.Idle {
		t.Fatalf("after an activity report inside the turn: %+v", s)
	}
	q.pass()
	if q.w.count() != 0 {
		t.Fatalf("queued behind a running turn: %v", q.w.calls)
	}
	q.reg(true) // its Stop hook
	if s := q.session(); !s.IdleAt.After(start) {
		t.Fatalf("after an idle hook event: %+v", s)
	}
	q.pass()
	if q.w.count() != 1 {
		t.Fatalf("not woken after its hook said idle: %v", q.w.calls)
	}
}

// The thread did not run the pending prompt after its turn (its app is
// closed): the newer message gets its wake once the grace passed since the
// turn's end, without the prompt's own message.
func TestQueuedPromptStaleAfterTurn(t *testing.T) {
	q := newStaleQueue(t)
	path := q.rollout("")
	first := q.send("request the prompt carries")
	q.pass()
	behind := q.send("request behind the prompt")
	q.wrote(path, turnLine("task_complete", time.Now().Add(-inboxWakeGrace-time.Minute)))
	q.age(inboxWakeGrace + 3*time.Minute)
	// No second prompt beside the pending one: a lease under it.
	if s := q.session(); q.w.count() != 1 || s.Wakes != 2 || !q.covers(wakeTokenOf(q.lastWake()), behind) {
		t.Fatalf("wake after the silent thread's grace: %v %+v", q.w.calls, s)
	}
	if v := leaseViewOf(t, q.a, first.ID); v.Attempts != 1 {
		t.Fatalf("the pending prompt's message was leased again: %+v", v)
	}
}

// A hook event that said idle after the rollout's last turn start wins: the
// session is woken as ever.
func TestHookIdleAfterTurnStartWakes(t *testing.T) {
	q := newStaleQueue(t)
	q.rollout(turnLine("task_started", time.Now().Add(-time.Minute)))
	q.reg(true) // its Stop hook; the rollout's end record is not written yet
	q.send("request for the idle session")
	q.pass()
	if q.w.count() != 1 || !q.session().Woken {
		t.Fatalf("a session its hook reported idle was not woken: %v", q.w.calls)
	}
}

// A rollout that is missing, unreadable as one, or silent for rolloutTurnQuiet
// inside a turn is no busy signal: the session is woken.
func TestRolloutWithoutSignalWakes(t *testing.T) {
	soon := time.Now().Add(time.Second)
	old := time.Now().Add(-rolloutTurnQuiet - time.Minute)
	cases := []struct {
		name string
		prep func(q *staleQueue)
	}{
		{"missing", func(*staleQueue) {}},
		{"no codex home", func(q *staleQueue) {
			q.rollout(turnLine("task_started", soon))
			q.home = filepath.Join(q.home, "none")
			q.reg(true)
		}},
		{"no turn record", func(q *staleQueue) { q.rollout("not a rollout\n{}\n") }},
		{"no timestamp", func(q *staleQueue) {
			q.rollout(`{"type":"event_msg","payload":{"type":"task_started"}}` + "\n")
		}},
		{"start inside a message", func(q *staleQueue) {
			q.rollout(`{"timestamp":"2026-10-13T10:00:00.000Z","type":"response_item","payload":{"type":"message","text":"{\"type\":\"event_msg\",\"payload\":{\"type\":\"task_started\"}}"}}` + "\n")
		}},
		{"line still being written", func(q *staleQueue) { q.rollout(strings.TrimSuffix(turnLine("task_started", soon), "\n")) }},
		{"turn silent for too long", func(q *staleQueue) {
			path := q.rollout(turnLine("task_started", soon))
			if err := os.Chtimes(path, old, old); err != nil {
				q.t.Fatal(err)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q := newStaleQueue(t)
			c.prep(q)
			q.send("request for the thread")
			q.pass()
			if q.w.count() != 1 || !q.session().Woken {
				t.Fatalf("not woken: %v", q.w.calls)
			}
		})
	}
}

// A thread killed inside a turn (a start, no end, then silence) is no busy
// thread for good: its wakes run out as a dead thread's do and the authors
// hear needs_human.
func TestDeadTurnReachesNeedsHuman(t *testing.T) {
	q := newStaleQueue(t)
	path := q.rollout(turnLine("task_started", time.Now().Add(time.Second)))
	held := q.send("request during the turn")
	q.pass()
	if q.w.count() != 0 {
		t.Fatalf("queued behind a running turn: %v", q.w.calls)
	}
	old := time.Now().Add(-rolloutTurnQuiet - time.Minute)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	q.pass()
	if q.w.count() != 1 || q.wakesWith("request during the turn") != 1 {
		t.Fatalf("no wake once the turn fell silent: %v", q.w.calls)
	}
	limit := maxIdleWakes + maxStaleWakes
	for i := 1; i < limit; i++ {
		q.send(fmt.Sprintf("later request %d", i))
		wait := inboxWakeGrace
		if i >= maxIdleWakes {
			wait = staleWakeWait(i)
		}
		q.age(wait + time.Second)
		if s := q.session(); q.w.count() != 1 || s.Wakes != i+1 {
			t.Fatalf("wake %d of the dead thread: %d %+v", i+1, q.w.count(), s)
		}
	}
	waiting := q.send("request past the cap")
	q.age(queuedOwnerMax + time.Second)
	if s := q.session(); q.w.count() != 1 || s.Wakes != limit || !s.WakeCapped {
		t.Fatalf("past the cap: prompts %d %+v", q.w.count(), s)
	}
	waitAttempts(t, q.b, waiting, AttemptNeedsHuman)
	if got := q.wakesWith("request during the turn"); got != 1 {
		t.Fatalf("the held request was queued %d times", got)
	}
	if v := leaseViewOf(t, q.a, held.ID); v.Attempts != 1 {
		t.Fatalf("the held request was leased again: %+v", v)
	}
}

// The book reads a rollout once and continues where it ended.
func TestRolloutBookContinues(t *testing.T) {
	q := newStaleQueue(t)
	at := time.Date(2026, 10, 13, 10, 0, 0, 0, time.UTC)
	path := q.rollout(turnLine("task_started", at))
	var b rolloutBook
	now := time.Now()
	if got, ok := b.turn(q.home, "s-q", now); !ok || got.state != turnRunning || !got.at.Equal(at) {
		t.Fatalf("start: %+v %v", got, ok)
	}
	q.wrote(path, turnLine("task_complete", at.Add(time.Minute)))
	if got, ok := b.turn(q.home, "s-q", now); !ok || got.state != turnEnded || !got.at.Equal(at.Add(time.Minute)) {
		t.Fatalf("end: %+v %v", got, ok)
	}
	q.wrote(path, turnLine("token_count", at.Add(2*time.Minute))+turnLine("task_started", at.Add(3*time.Minute)))
	if got, ok := b.turn(q.home, "s-q", now); !ok || got.state != turnRunning || !got.at.Equal(at.Add(3*time.Minute)) {
		t.Fatalf("next start: %+v %v", got, ok)
	}
	// A start with no end before it (the turn before was killed): the newer one.
	q.wrote(path, turnLine("task_started", at.Add(4*time.Minute)))
	if got, ok := b.turn(q.home, "s-q", now); !ok || got.state != turnRunning || !got.at.Equal(at.Add(4*time.Minute)) {
		t.Fatalf("start after a start: %+v %v", got, ok)
	}
	// A shorter file is not the one that was read: it is read from its start.
	if err := os.WriteFile(path, []byte(turnLine("task_complete", at.Add(5*time.Minute))), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, ok := b.turn(q.home, "s-q", now); !ok || got.state != turnEnded || !got.at.Equal(at.Add(5*time.Minute)) {
		t.Fatalf("a truncated rollout: %+v %v", got, ok)
	}
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, ok := b.turn(q.home, "s-q", now); !ok || got.state != turnNone {
		t.Fatalf("a truncated rollout with no turn record keeps the old turn: %+v %v", got, ok)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got, ok := b.turn(q.home, "s-q", now); ok {
		t.Fatalf("a removed rollout: %+v", got)
	}
}
