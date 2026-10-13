package node

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

// Full mode has no budget of autonomous turns: more wakes in one hour than the
// removed limit allowed (30) all run, and nothing pauses the project or writes
// budget state. The owner's stop is the only brake: while it is on a message
// waits (held:stopped) and no session is woken; off, the wakes go on.
func TestAutonomyManyTurnsNeverPause(t *testing.T) {
	p := &fakePoster{}
	dir := t.TempDir()
	a, b := deliveryPair(t, dir, p, nil)
	a.SetAutonomy(Autonomy{Mode: AutonomyFull})
	ctx := context.Background()
	turn := func(body string) Message {
		t.Helper()
		regClaude(t, a, dir, "s-c", testSocket, false) // the session's turn ...
		regClaude(t, a, dir, "s-c", testSocket, true)  // ... ended: idle again
		m := ask(t, a, b, body)
		a.wakeIdle(ctx)
		return m
	}
	done := func(m Message) {
		t.Helper()
		if _, err := a.Ack("", AckRequest{IDs: []string{m.ID}, SessionID: "s-c"}); err != nil {
			t.Fatal(err)
		}
	}
	const turns = 45
	for i := range turns {
		m := turn(fmt.Sprintf("turn %d", i))
		if p.count() != i+1 || a.holdReason() != "" {
			t.Fatalf("turn %d: %d wakes, hold %q", i, p.count(), a.holdReason())
		}
		done(m)
	}
	if _, err := os.Stat(filepath.Join(a.cfg.DataDir, autonomyFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the turns wrote %s: %v", autonomyFile, err)
	}

	a.SetStopped(true)
	held := turn("while stopped")
	if p.count() != turns || a.holdReason() != HoldStopped {
		t.Fatalf("stopped: %d wakes, hold %q", p.count(), a.holdReason())
	}
	if page, err := a.UnreadFor(dir, "s-c", "", 10); err != nil || page.Total != 0 {
		t.Fatalf("a stopped node delivered to its session: %+v %v", page, err)
	}
	if page, _ := a.Unread("", "", 10); page.Total != 1 || page.Messages[0].ID != held.ID {
		t.Fatalf("the held message is not kept unread: %+v", page)
	}
	a.SetStopped(false)
	a.wakeIdle(ctx)
	if p.count() != turns+1 || !strings.Contains(p.calls[turns], "while stopped") {
		t.Fatalf("after the stop: %d wakes", p.count())
	}
}

// An autonomy.json of a version with budgets (paused by "turns" or "run", old
// counters, unknown fields, or not JSON at all) holds nothing: the node starts
// free and drops the file. The owner's stop is not kept in it and stays.
func TestStaleBudgetPauseDropped(t *testing.T) {
	for name, content := range map[string]string{
		"turns": `{"paused":true,"reason":"turns","paused_at":"2026-09-30T10:00:00Z","turns":["2026-09-30T09:59:00Z"],"run_start":"2026-09-30T09:00:00Z","last_turn":"2026-09-30T09:59:00Z","told_at":"2026-09-30T10:00:00Z"}`,
		"run":   `{"paused":true,"reason":"run","paused_at":"2026-09-30T10:00:00Z","turns_per_hour":30,"max_run":14400000000000}`,
		"junk":  `{"paused":`,
	} {
		t.Run(name, func(t *testing.T) {
			data := t.TempDir()
			path := filepath.Join(data, autonomyFile)
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			n := newTestNode(t, "a", testSecret, nil, data, listen(t), nil)
			if n.holdReason() != "" || n.Stopped() || n.AutonomyStatus().Stopped {
				t.Fatalf("held by a stale budget pause: %q", n.holdReason())
			}
			if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("%s kept: %v", autonomyFile, err)
			}
			n.SetStopped(true)
			if n.holdReason() != HoldStopped {
				t.Fatal("the owner's stop does not hold")
			}
		})
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
	if !a.Stopped() || a.holdReason() != HoldStopped || !a.AutoHeld(m) {
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
