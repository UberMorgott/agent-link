package node

import (
	"errors"
	"slices"
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
