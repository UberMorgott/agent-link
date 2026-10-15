package node

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

// chatMsg is message id of chat as the chat API lists it.
func chatMsg(t *testing.T, n *testNode, chat, id string) ChatMessage {
	t.Helper()
	msgs, err := n.ChatMessages(chat, 0, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(msgs, func(m ChatMessage) bool { return m.ID == id })
	if i < 0 {
		t.Fatalf("no message %s in %+v", id, msgs)
	}
	return msgs[i]
}

func chatNeedsHuman(t *testing.T, n *testNode, chat string) int {
	t.Helper()
	info, err := n.Chat(chat)
	if err != nil {
		t.Fatal(err)
	}
	return info.NeedsHuman
}

// A local seat's reply to a session that ended needs a person: it is marked
// (message, chat, unread API), stays unread for no other session, and only an
// explicit Reassign hands it to a live one, whose claim and ack then take it.
func TestOrphanedSeatReplyNeedsHumanAndReassign(t *testing.T) {
	l := &seatLauncher{}
	dir := t.TempDir()
	a := seatNode(t, t.TempDir(), dir, l)
	_, codex := addSeats(t, a)
	for _, id := range []string{"s-owner", "s-other"} {
		if _, err := a.RegisterSession(SessionRequest{SessionID: id, Provider: ProviderClaude, Folder: dir}); err != nil {
			t.Fatal(err)
		}
	}
	chat, err := a.NewProjectChat(nil)
	if err != nil {
		t.Fatal(err)
	}
	q, err := a.SendRequest(SendRequest{ChatID: chat.ID, Body: "codex, check this", AuthorKind: AuthorAgent, SessionID: "s-owner", AskSeats: []string{codex.ID}})
	if err != nil {
		t.Fatal(err)
	}
	r, err := a.SendRequest(SendRequest{ChatID: chat.ID, ReplyTo: q.ID, Body: "checked", Seat: codex.ID})
	if err != nil {
		t.Fatal(err)
	}
	if m := chatMsg(t, a, chat.ID, r.ID); !m.Unread || m.NeedsHuman {
		t.Fatalf("seat reply before the end: %+v", m)
	}
	// A live session's reply moves only by force; the orphan sweep of a live
	// session marks nothing.
	if _, err := a.Reassign(ReassignRequest{ID: r.ID, SessionID: "s-other"}); !errors.Is(err, ErrNotOrphaned) {
		t.Fatalf("reassign of a live session's reply without force: %v", err)
	}
	a.OrphanReplies("s-owner")
	if m := chatMsg(t, a, chat.ID, r.ID); m.NeedsHuman {
		t.Fatalf("orphaned while its session lives: %+v", m)
	}
	if err := a.EndSession("s-owner"); err != nil {
		t.Fatal(err)
	}
	if m := chatMsg(t, a, chat.ID, r.ID); !m.Unread || !m.NeedsHuman || m.OrphanedSession != "s-owner" {
		t.Fatalf("orphaned seat reply: %+v", m)
	}
	if got := chatNeedsHuman(t, a, chat.ID); got != 1 {
		t.Fatalf("chat needs_human %d", got)
	}
	if p, _ := a.Unread("", "", 10); !slices.ContainsFunc(p.Messages, func(m UnreadMessage) bool { return m.ID == r.ID && m.NeedsHuman }) {
		t.Fatalf("unread API lacks needs_human: %+v", p.Messages)
	}
	// No auto reroute: the other live session neither sees nor claims it.
	if p, _ := a.UnreadFor(dir, "s-other", "", 10); slices.ContainsFunc(p.Messages, func(m UnreadMessage) bool { return m.ID == r.ID }) {
		t.Fatalf("orphaned reply rerouted: %+v", p.Messages)
	}
	if g, _ := a.Claim(ClaimRequest{IDs: []string{r.ID}, SessionID: "s-other"}); len(g) != 0 {
		t.Fatalf("orphaned reply claimed by another session: %v", g)
	}

	// Reassign: refused to a session that is not live, for an unknown id.
	if _, err := a.Reassign(ReassignRequest{ID: r.ID, SessionID: "s-owner"}); !errors.Is(err, ErrUnknownSession) {
		t.Fatalf("reassign to an ended session: %v", err)
	}
	if _, err := a.Reassign(ReassignRequest{ID: newID(), SessionID: "s-other"}); !errors.Is(err, ErrUnknownMessage) {
		t.Fatalf("reassign of an unknown message: %v", err)
	}
	// A session whose folder does not read the chat would never get it.
	if _, err := a.ReassignTo(r.ID, Session{SessionID: "s-far", Folder: t.TempDir()}, false); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("reassign to a session of another folder: %v", err)
	}
	m, err := a.Reassign(ReassignRequest{ID: r.ID, SessionID: "s-other"})
	if err != nil || m.NeedsHuman || m.Assigned != "session:s-other" {
		t.Fatalf("reassign: %+v, %v", m, err)
	}
	if got := chatNeedsHuman(t, a, chat.ID); got != 0 {
		t.Fatalf("chat needs_human after reassign %d", got)
	}
	if p, _ := a.UnreadFor(dir, "s-other", "", 10); !slices.ContainsFunc(p.Messages, func(m UnreadMessage) bool { return m.ID == r.ID }) {
		t.Fatalf("reassigned reply not for its new session: %+v", p.Messages)
	}
	// A late sweep for the old session does not take it back from the new one.
	a.OrphanReplies("s-owner")
	if m := chatMsg(t, a, chat.ID, r.ID); m.NeedsHuman {
		t.Fatalf("reassigned reply orphaned again: %+v", m)
	}
	if g, _ := a.Claim(ClaimRequest{IDs: []string{r.ID}, SessionID: "s-other"}); !slices.Equal(g, []string{r.ID}) {
		t.Fatalf("new session's claim: %v", g)
	}
	if _, err := a.Ack("", AckRequest{IDs: []string{r.ID}, SessionID: "s-other"}); err != nil {
		t.Fatal(err)
	}
	// A read message is not reassigned.
	if _, err := a.Reassign(ReassignRequest{ID: r.ID, SessionID: "s-other"}); !errors.Is(err, ErrBadRequest) {
		t.Fatalf("reassign of a read message: %v", err)
	}
}

// A session whose TTL passes without SessionEnd orphans its replies like an
// ended one: marked for the owner, the peer author told needs_human. The
// session coming back takes them again.
func TestExpiredSessionOrphansReplies(t *testing.T) {
	a, b := pair(t, testSecret, testSecret)
	dir := t.TempDir()
	if _, err := a.RegisterSession(SessionRequest{SessionID: "s-ttl", Provider: ProviderClaude, Folder: dir}); err != nil {
		t.Fatal(err)
	}
	q, err := a.SendRequest(SendRequest{To: "b", Body: "question", AuthorKind: AuthorAgent, SessionID: "s-ttl"})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "b has the request", func() bool { p, _ := b.Unread("", "", 10); return p.Total == 1 })
	r, err := b.SendRequest(SendRequest{ChatID: q.ChatID, ReplyTo: q.ID, Body: "answer", AuthorKind: AuthorAgent})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "a has the reply", func() bool { p, _ := a.Unread("", "", 10); return p.Total == 1 })

	// Still live: nothing happens.
	a.expireSessions(time.Now())
	if m := chatMsg(t, a, q.ChatID, r.ID); m.NeedsHuman {
		t.Fatalf("needs_human while live: %+v", m)
	}
	// Its heartbeats stop past its TTL.
	a.sess.mu.Lock()
	a.sess.sessions["s-ttl"].LastSeen = time.Now().Add(-SessionTTL - time.Minute)
	a.sess.mu.Unlock()
	a.expireSessions(time.Now())
	if m := chatMsg(t, a, q.ChatID, r.ID); !m.NeedsHuman || m.OrphanedSession != "s-ttl" {
		t.Fatalf("expired session's reply: %+v", m)
	}
	eventually(t, "needs_human reaches the author", func() bool { return slices.Contains(attemptsOf(b, r), AttemptNeedsHuman) })
	// Reported once: a second sweep finds nothing new.
	a.sess.mu.Lock()
	gone := len(a.sess.gone)
	a.sess.mu.Unlock()
	if gone != 0 {
		t.Fatalf("gone sessions kept: %d", gone)
	}

	// The session is resumed: its reply is its own again.
	if _, err := a.RegisterSession(SessionRequest{SessionID: "s-ttl", Provider: ProviderClaude, Folder: dir}); err != nil {
		t.Fatal(err)
	}
	if m := chatMsg(t, a, q.ChatID, r.ID); m.NeedsHuman || !m.Unread {
		t.Fatalf("after the session came back: %+v", m)
	}
	// Its live reply moves to another session only by force.
	if _, err := a.RegisterSession(SessionRequest{SessionID: "s-two", Provider: ProviderClaude, Folder: dir}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Reassign(ReassignRequest{ID: r.ID, SessionID: "s-two"}); !errors.Is(err, ErrNotOrphaned) {
		t.Fatalf("reassign without force: %v", err)
	}
	if m, err := a.Reassign(ReassignRequest{ID: r.ID, SessionID: "s-two", Force: true}); err != nil || m.Assigned != "session:s-two" {
		t.Fatalf("reassign with force: %+v, %v", m, err)
	}
	if p, _ := a.UnreadFor(dir, "s-ttl", "", 10); slices.ContainsFunc(p.Messages, func(m UnreadMessage) bool { return m.ID == r.ID }) {
		t.Fatalf("forced reply still for its old session: %+v", p.Messages)
	}
	// The session it was handed to ends: the reply needs a person again.
	if err := a.EndSession("s-two"); err != nil {
		t.Fatal(err)
	}
	if m := chatMsg(t, a, q.ChatID, r.ID); !m.NeedsHuman || m.OrphanedSession != "s-two" {
		t.Fatalf("reassigned reply after its new session ended: %+v", m)
	}
	// A reassign to a session that ended is refused, even from a stale check.
	if _, err := a.reassignTo(r.ID, Session{SessionID: "s-two", Folder: dir}, false, true); !errors.Is(err, ErrUnknownSession) {
		t.Fatalf("reassign to an ended session past the first check: %v", err)
	}
}

// An expired session another session's heartbeat dropped (gone) that
// registers again before the sweep keeps its replies: the sweep skips it.
func TestExpiredSessionBackBeforeSweep(t *testing.T) {
	a, b := pair(t, testSecret, testSecret)
	dir := t.TempDir()
	for _, id := range []string{"s-a", "s-b"} {
		if _, err := a.RegisterSession(SessionRequest{SessionID: id, Provider: ProviderClaude, Folder: dir}); err != nil {
			t.Fatal(err)
		}
	}
	q, err := a.SendRequest(SendRequest{To: "b", Body: "question", AuthorKind: AuthorAgent, SessionID: "s-a"})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "b has the request", func() bool { p, _ := b.Unread("", "", 10); return p.Total == 1 })
	r, err := b.SendRequest(SendRequest{ChatID: q.ChatID, ReplyTo: q.ID, Body: "answer", AuthorKind: AuthorAgent})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "a has the reply", func() bool { p, _ := a.Unread("", "", 10); return p.Total == 1 })
	a.sess.mu.Lock()
	a.sess.sessions["s-a"].LastSeen = time.Now().Add(-SessionTTL - time.Minute)
	a.sess.mu.Unlock()
	// s-b's heartbeat drops the expired s-a; s-a registers again at once.
	for _, id := range []string{"s-b", "s-a"} {
		if _, err := a.RegisterSession(SessionRequest{SessionID: id, Provider: ProviderClaude, Folder: dir}); err != nil {
			t.Fatal(err)
		}
	}
	a.expireSessions(time.Now())
	if m := chatMsg(t, a, q.ChatID, r.ID); m.NeedsHuman {
		t.Fatalf("a returned session's reply orphaned: %+v", m)
	}
	if slices.Contains(attemptsOf(b, r), AttemptNeedsHuman) {
		t.Fatal("author told needs_human for a returned session")
	}
}

// A request a session read and took (its ack) but never answered goes back
// when that session ends: unread again, nobody's, shown as needing a person,
// and a live session takes it. It is handed back once; an answered request,
// and one read by a session that still lives, stay read.
func TestStrandedRequestReopened(t *testing.T) {
	dir := t.TempDir()
	a, b := deliveryPair(t, dir, nil, nil)
	reg := func(id string) {
		t.Helper()
		if _, err := a.RegisterSession(SessionRequest{SessionID: id, Provider: ProviderClaude, Folder: dir}); err != nil {
			t.Fatal(err)
		}
	}
	take := func(id, sid string) {
		t.Helper()
		if g, err := a.Claim(ClaimRequest{IDs: []string{id}, SessionID: sid}); err != nil || !slices.Equal(g, []string{id}) {
			t.Fatalf("claim of %s by %s: %v %v", id, sid, g, err)
		}
		if res, err := a.Ack("", AckRequest{IDs: []string{id}, SessionID: sid}); err != nil || res[0].Assigned != "session:"+sid {
			t.Fatalf("ack of %s by %s: %+v %v", id, sid, res, err)
		}
	}
	reg("s-1")
	m := ask(t, a, b, "do this")
	done := ask(t, a, b, "and this")
	take(done.ID, "s-1")
	if _, err := a.SendRequest(SendRequest{ChatID: done.ChatID, ReplyTo: done.ID, Body: "done", AuthorKind: AuthorAgent, SessionID: "s-1"}); err != nil {
		t.Fatal(err)
	}
	take(m.ID, "s-1") // read after that reply: nothing answers it
	// Its session lives: the sweep leaves it, however long ago it was read.
	a.orphanSweep(time.Now().Add(30 * time.Minute))
	if cm := chatMsg(t, a, m.ChatID, m.ID); cm.Unread || cm.NeedsHuman {
		t.Fatalf("a live session's request handed back: %+v", cm)
	}
	reg("s-2")
	if err := a.EndSession("s-1"); err != nil {
		t.Fatal(err)
	}
	if cm := chatMsg(t, a, m.ChatID, m.ID); !cm.Unread || !cm.NeedsHuman || cm.OrphanedSession != "s-1" || cm.Assigned != "" {
		t.Fatalf("unanswered request after its session ended: %+v", cm)
	}
	if cm := chatMsg(t, a, done.ChatID, done.ID); cm.Unread {
		t.Fatalf("an answered request handed back: %+v", cm)
	}
	if v := leaseViewOf(t, a, m.ID); v.State == LeaseAcked || v.Owner != "" {
		t.Fatalf("lease of the handed back request: %+v", v)
	}
	if p, _ := a.UnreadFor(dir, "s-2", "", 10); !slices.ContainsFunc(p.Messages, func(u UnreadMessage) bool { return u.ID == m.ID && u.AsksYou }) {
		t.Fatalf("not offered to the live session: %+v", p.Messages)
	}
	take(m.ID, "s-2")
	// Handed back once: the second session's end leaves it read.
	if err := a.EndSession("s-2"); err != nil {
		t.Fatal(err)
	}
	if cm := chatMsg(t, a, m.ChatID, m.ID); cm.Unread || cm.Assigned != "session:s-2" {
		t.Fatalf("handed back twice: %+v", cm)
	}

	// A reader that went without SessionEnd (never registered here, or its
	// end was missed): the periodic sweep finds it, past its grace.
	lost := ask(t, a, b, "read by a ghost")
	if _, err := a.Ack("", AckRequest{IDs: []string{lost.ID}, SessionID: "s-ghost"}); err != nil {
		t.Fatal(err)
	}
	a.orphanSweep(time.Now())
	if cm := chatMsg(t, a, lost.ChatID, lost.ID); cm.Unread {
		t.Fatalf("handed back within its grace: %+v", cm)
	}
	// Past its grace, unseen for less than sessionGoneAfter: it may be back.
	a.orphanSweep(time.Now().Add(strandedGrace + time.Second))
	if cm := chatMsg(t, a, lost.ChatID, lost.ID); cm.Unread {
		t.Fatalf("handed back before its reader is gone for good: %+v", cm)
	}
	a.orphanSweep(time.Now().Add(sessionGoneAfter + time.Second))
	if cm := chatMsg(t, a, lost.ChatID, lost.ID); !cm.Unread || cm.OrphanedSession != "s-ghost" {
		t.Fatalf("the sweep left a gone reader's request: %+v", cm)
	}
	// Old news is not replayed.
	old := ask(t, a, b, "read long ago")
	if _, err := a.Ack("", AckRequest{IDs: []string{old.ID}, SessionID: "s-ghost"}); err != nil {
		t.Fatal(err)
	}
	a.orphanSweep(time.Now().Add(strandedMax + time.Minute))
	if cm := chatMsg(t, a, old.ChatID, old.ID); cm.Unread {
		t.Fatalf("a request read long ago handed back: %+v", cm)
	}
}

// A request handed back by a gone session opens no session at once
// (v0.6.70 launched one 1.4 s after the hand-back while a desktop session was
// starting in the folder): not within reopenGrace, then only when nobody took
// it meanwhile and no session started that would take it (#37).
func TestReopenedRequestLaunchWaitsGrace(t *testing.T) {
	for _, tc := range []string{"none", "live", "claimed"} {
		t.Run(tc, func(t *testing.T) {
			dir := t.TempDir()
			l := &fakeLauncher{}
			a, b := deliveryPair(t, dir, nil, l)
			reg := func(id string) {
				t.Helper()
				if _, err := a.RegisterSession(SessionRequest{SessionID: id, Provider: ProviderClaude, Folder: dir, TTLSec: 3600}); err != nil {
					t.Fatal(err)
				}
			}
			take := func(id, sid string) {
				t.Helper()
				if g, err := a.Claim(ClaimRequest{IDs: []string{id}, SessionID: sid}); err != nil || !slices.Equal(g, []string{id}) {
					t.Fatalf("claim of %s by %s: %v %v", id, sid, g, err)
				}
				if _, err := a.Ack("", AckRequest{IDs: []string{id}, SessionID: sid}); err != nil {
					t.Fatal(err)
				}
			}
			reg("s-1")
			m := ask(t, a, b, "do this")
			take(m.ID, "s-1")
			if err := a.EndSession("s-1"); err != nil {
				t.Fatal(err)
			}
			if cm := chatMsg(t, a, m.ChatID, m.ID); !cm.Unread || cm.OrphanedSession != "s-1" {
				t.Fatalf("not handed back: %+v", cm)
			}
			ctx := context.Background()
			now := time.Now()
			a.launchDue(ctx, now.Add(launchGrace+time.Second))
			a.launchDue(ctx, now.Add(reopenGrace-time.Second))
			if got := l.all(); len(got) != 0 {
				t.Fatalf("launched within the grace: %+v", got)
			}
			switch tc {
			case "live":
				reg("s-2") // a session starts in the folder during the grace
			case "claimed":
				reg("s-2")
				take(m.ID, "s-2")
				if err := a.EndSession("s-2"); err != nil {
					t.Fatal(err)
				}
			}
			a.launchDue(ctx, now.Add(reopenGrace+time.Second))
			got := l.all()
			if tc == "none" {
				if len(got) != 1 {
					t.Fatalf("no launch after the grace with nobody to take it: %+v", got)
				}
				return
			}
			if len(got) != 0 {
				t.Fatalf("launched although %s: %+v", tc, got)
			}
		})
	}
}

// A request its session read, took and answered in the chat without reply_to
// (a plain message later than its read, or a reply to a later ask), or that
// another session of this node took up there, is answered: its session's end
// does not hand it back for a second answer (v0.6.70 reopened requests
// answered 13 hours before, and a launched session answered them again).
func TestAnsweredWithoutReplyToNotReopened(t *testing.T) {
	for _, by := range []string{"s-1", "s-2"} {
		t.Run(by, func(t *testing.T) {
			dir := t.TempDir()
			a, b := deliveryPair(t, dir, nil, nil)
			for _, sid := range []string{"s-1", "s-2"} {
				if _, err := a.RegisterSession(SessionRequest{SessionID: sid, Provider: ProviderClaude, Folder: dir}); err != nil {
					t.Fatal(err)
				}
			}
			m := ask(t, a, b, "do this")
			if g, err := a.Claim(ClaimRequest{IDs: []string{m.ID}, SessionID: "s-1"}); err != nil || !slices.Equal(g, []string{m.ID}) {
				t.Fatalf("claim: %v %v", g, err)
			}
			if _, err := a.Ack("", AckRequest{IDs: []string{m.ID}, SessionID: "s-1"}); err != nil {
				t.Fatal(err)
			}
			if _, err := a.SendRequest(SendRequest{ChatID: m.ChatID, Body: "done, see the branch", AuthorKind: AuthorAgent, SessionID: by}); err != nil {
				t.Fatal(err)
			}
			if err := a.EndSession("s-1"); err != nil {
				t.Fatal(err)
			}
			a.orphanSweep(time.Now().Add(sessionGoneAfter + time.Minute))
			if cm := chatMsg(t, a, m.ChatID, m.ID); cm.Unread || cm.NeedsHuman {
				t.Fatalf("a request answered with no reply_to was handed back: unread=%v needs_human=%v orphaned=%q", cm.Unread, cm.NeedsHuman, cm.OrphanedSession)
			}
		})
	}
}

// A session that only expired (a sleep, the node down: no heartbeat for a TTL)
// and comes back keeps the request it works on: nothing hands it back, the
// session is not given it a second time. One that ended is, once: the mark of
// that outlives its return (ReclaimReplies), so its next end leaves the
// request read.
func TestExpiredSessionKeepsItsRequest(t *testing.T) {
	dir := t.TempDir()
	a, b := deliveryPair(t, dir, nil, nil)
	reg := func() {
		t.Helper()
		if _, err := a.RegisterSession(SessionRequest{SessionID: "s-1", Provider: ProviderClaude, Folder: dir}); err != nil {
			t.Fatal(err)
		}
	}
	take := func(id string) {
		t.Helper()
		if g, err := a.Claim(ClaimRequest{IDs: []string{id}, SessionID: "s-1"}); err != nil || !slices.Equal(g, []string{id}) {
			t.Fatalf("claim: %v %v", g, err)
		}
		if _, err := a.Ack("", AckRequest{IDs: []string{id}, SessionID: "s-1"}); err != nil {
			t.Fatal(err)
		}
	}
	offered := func(id string) bool {
		t.Helper()
		p, err := a.UnreadFor(dir, "s-1", "", 10)
		if err != nil {
			t.Fatal(err)
		}
		return slices.ContainsFunc(p.Messages, func(u UnreadMessage) bool { return u.ID == id })
	}
	expire := func() {
		a.sess.mu.Lock()
		a.sess.sessions["s-1"].LastSeen = time.Now().Add(-2 * time.Hour)
		a.sess.mu.Unlock()
		a.expireSessions(time.Now())
	}
	reg()
	m := ask(t, a, b, "do this")
	take(m.ID)
	// Its heartbeats stopped two hours ago, as the clock has it; the node has
	// been awake to hear one for a moment only.
	expire()
	a.orphanSweep(time.Now().Add(2 * strandedGrace))
	if cm := chatMsg(t, a, m.ChatID, m.ID); cm.Unread || cm.NeedsHuman || cm.Assigned != "session:s-1" {
		t.Fatalf("an expired session's request handed back: %+v", cm)
	}
	reg() // it is back
	if offered(m.ID) {
		t.Fatal("the session got its request a second time")
	}
	// Unseen for sessionGoneAfter with the node awake: gone for good.
	other := ask(t, a, b, "and this")
	take(other.ID)
	a.orphanSweep(time.Now().Add(sessionGoneAfter + time.Minute))
	for _, id := range []string{m.ID, other.ID} {
		if cm := chatMsg(t, a, m.ChatID, id); !cm.Unread || cm.OrphanedSession != "s-1" || cm.Assigned != "" {
			t.Fatalf("request of a session unseen for an hour: %+v", cm)
		}
	}
	// It registers after all: the requests are its own again, taken again, and
	// handed back no second time, whether it expires or ends.
	expire()
	reg()
	if cm := chatMsg(t, a, m.ChatID, m.ID); cm.NeedsHuman || !cm.Unread || !offered(m.ID) {
		t.Fatalf("after the session came back: %+v", cm)
	}
	take(m.ID)
	take(other.ID)
	a.orphanSweep(time.Now().Add(sessionGoneAfter + time.Minute))
	if err := a.EndSession("s-1"); err != nil {
		t.Fatal(err)
	}
	a.orphanSweep(time.Now().Add(2 * strandedGrace))
	for _, id := range []string{m.ID, other.ID} {
		if cm := chatMsg(t, a, m.ChatID, id); cm.Unread || cm.Assigned != "session:s-1" {
			t.Fatalf("handed back twice: %+v", cm)
		}
	}
}

// A session live in another context of the process (the session hook finds
// it there: ReclaimReplies) is reconciled once, not at every sweep, until the
// next reconciliation says otherwise.
func TestOrphanSweepReconcilesLiveElsewhereOnce(t *testing.T) {
	a, b := deliveryPair(t, t.TempDir(), nil, nil)
	var calls atomic.Int32
	a.onSession = func(sid string, _ bool) {
		calls.Add(1)
		a.ReclaimReplies(sid)
	}
	m := ask(t, a, b, "read from another context")
	if _, err := a.Ack("", AckRequest{IDs: []string{m.ID}, SessionID: "s-there"}); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(sessionGoneAfter + time.Minute)
	for range 3 {
		a.orphanSweep(at)
	}
	if cm := chatMsg(t, a, m.ChatID, m.ID); calls.Load() != 1 || cm.Unread {
		t.Fatalf("reconciled %d times, message %+v", calls.Load(), cm)
	}
	// It ends there: reconciled as live nowhere, the sweep looks again.
	a.onSession = func(string, bool) { calls.Add(1) }
	a.OrphanReplies("s-there")
	a.orphanSweep(at)
	if calls.Load() != 2 {
		t.Fatalf("reconciled %d times after it ended elsewhere", calls.Load())
	}
}

// A reply that arrives after the session that asked ended has no session end
// left to mark it: its arrival asks for a sweep, which marks it as needing a
// person and tells its author.
func TestLateReplyOrphanedBySweep(t *testing.T) {
	a, b := pair(t, testSecret, testSecret)
	dir := t.TempDir()
	if _, err := a.RegisterSession(SessionRequest{SessionID: "s-ask", Provider: ProviderClaude, Folder: dir}); err != nil {
		t.Fatal(err)
	}
	q, err := a.SendRequest(SendRequest{To: "b", Body: "question", AuthorKind: AuthorAgent, SessionID: "s-ask"})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "b has the request", func() bool { p, _ := b.Unread("", "", 10); return p.Total == 1 })
	if err := a.EndSession("s-ask"); err != nil {
		t.Fatal(err)
	}
	a.orphanDue.Store(false)
	r, err := b.SendRequest(SendRequest{ChatID: q.ChatID, ReplyTo: q.ID, Body: "answer", AuthorKind: AuthorAgent})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "a has the reply", func() bool { p, _ := a.Unread("", "", 10); return p.Total == 1 })
	eventually(t, "the reply asks for a sweep or got one", func() bool {
		return a.orphanDue.Load() || chatMsg(t, a, q.ChatID, r.ID).NeedsHuman
	})
	a.orphanSweep(time.Now())
	if m := chatMsg(t, a, q.ChatID, r.ID); !m.Unread || !m.NeedsHuman || m.OrphanedSession != "s-ask" {
		t.Fatalf("late reply for an ended session: %+v", m)
	}
	eventually(t, "needs_human reaches the author", func() bool { return slices.Contains(attemptsOf(b, r), AttemptNeedsHuman) })
	// The asker comes back: the reply is its own again, and the sweep leaves it.
	if _, err := a.RegisterSession(SessionRequest{SessionID: "s-ask", Provider: ProviderClaude, Folder: dir}); err != nil {
		t.Fatal(err)
	}
	a.orphanSweep(time.Now())
	if m := chatMsg(t, a, q.ChatID, r.ID); m.NeedsHuman || !m.Unread {
		t.Fatalf("after the asker came back: %+v", m)
	}
}
