package node

import (
	"context"
	"testing"
	"time"
)

// Hold kinds as the pinning tests name them.
const (
	pinNone   = ""
	pinHook   = "hook"
	pinWake   = "wake"
	pinLaunch = "launch"
	pinAck    = "ack"
	pinTurn   = "turn"
)

// pinClaim is a session claim of kind, age old.
func pinClaim(kind, session string, age time.Duration) sessionClaim {
	return sessionClaim{session: session, at: time.Now().Add(-age), wake: kind == pinWake, launch: kind == pinLaunch,
		ackOnly: kind == pinAck, token: "tok"}
}

// pinMark sets (or with pinNone drops) the hold of message id for seat.
func pinMark(a *testNode, seat, id, kind string, age time.Duration) {
	st := a.seats
	st.mu.Lock()
	defer st.mu.Unlock()
	k := seatKey(seat, id)
	if kind == pinNone {
		delete(st.marks, k)
		return
	}
	st.marks[k] = seatMark{at: time.Now().Add(-age), wake: kind == pinWake, turn: kind == pinTurn, token: "tok"}
}

// A session's claim holds by kind: a hook's for claimTTL and a wake's for
// inboxWakeGrace while the session lives, a launch's for launchHold live or
// not, an ack-pending one until it is released.
func TestHoldRuleSessionClaims(t *testing.T) {
	const s = "s-1"
	live, gone := map[string]bool{s: true}, map[string]bool{}
	for _, tc := range []struct {
		kind string
		age  time.Duration
		live map[string]bool
		want bool
	}{
		{pinHook, 0, live, true},
		{pinHook, 0, gone, false},
		{pinHook, claimTTL + time.Second, live, false},
		{pinWake, claimTTL + time.Second, live, true},
		{pinWake, 0, gone, false},
		{pinWake, inboxWakeGrace + time.Second, live, false},
		{pinLaunch, 0, gone, true},
		{pinLaunch, launchHold + time.Second, gone, false},
		{pinAck, 0, gone, true},
		{pinAck, 48 * time.Hour, gone, true},
	} {
		if got := pinClaim(tc.kind, s, tc.age).held(tc.live); got != tc.want {
			t.Errorf("%s age %v live %v: held %v, want %v", tc.kind, tc.age, tc.live[s], got, tc.want)
		}
	}
}

// A seat's hold decides, by kind and age, what its session's unread shows
// (woken, or hidden while the node's turn has it), what a hook's claim and a
// wake may take, and what the node's turn takes.
func TestHoldRuleSeatMarks(t *testing.T) {
	l := &seatLauncher{}
	dir := t.TempDir()
	a := seatNode(t, t.TempDir(), dir, l)
	_, codex := addSeats(t, a)
	if _, err := a.RegisterSession(SessionRequest{SessionID: codex.SessionID, Provider: codex.Provider, Folder: dir}); err != nil {
		t.Fatal(err)
	}
	chat, _ := a.NewProjectChat(nil)
	m, err := a.SendRequest(SendRequest{ChatID: chat.ID, Body: "review", AuthorKind: AuthorHuman, AskSeats: []string{codex.ID}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		kind            string
		age             time.Duration
		woke, turn      bool // what the unread view sees
		hook, wake, run bool // granted to a hook's claim, a wake, the node's turn
	}{
		{pinNone, 0, false, false, true, true, true},
		{pinHook, 0, false, false, true, false, false},
		{pinHook, claimTTL + time.Second, false, false, true, true, true},
		{pinWake, claimTTL + time.Second, true, false, false, false, false},
		{pinWake, inboxWakeGrace + time.Second, false, false, true, true, true},
		{pinTurn, 48 * time.Hour, false, true, false, false, false},
	} {
		pinMark(a, codex.ID, m.ID, tc.kind, tc.age)
		_, woke, turn := a.seats.held(codex.ID, m.ID)
		pinMark(a, codex.ID, m.ID, tc.kind, tc.age)
		_, hook := a.seatClaim(codex.SessionID, m.ID, false, "")
		pinMark(a, codex.ID, m.ID, tc.kind, tc.age)
		_, wake := a.seatClaim(codex.SessionID, m.ID, true, "tok2")
		pinMark(a, codex.ID, m.ID, tc.kind, tc.age)
		var um UnreadMessage
		um.ID = m.ID
		out, ok := a.claimTurn(context.Background(), codex.ID, []UnreadMessage{um}, false)
		run := ok && len(out) == 1
		if woke != tc.woke || turn != tc.turn || hook != tc.hook || wake != tc.wake || run != tc.run {
			t.Errorf("%s age %v: woke %v turn %v hook %v wake %v run %v; want %v %v %v %v %v",
				tc.kind, tc.age, woke, turn, hook, wake, run, tc.woke, tc.turn, tc.hook, tc.wake, tc.run)
		}
	}
	pinMark(a, codex.ID, m.ID, pinNone, 0)
}
