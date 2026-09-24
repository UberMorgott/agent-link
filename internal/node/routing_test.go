package node

import (
	"slices"
	"testing"
	"time"
)

// A reply goes to the session that sent the request, never to another session
// of the same folder; a message for no session in particular is claimed by
// exactly one; a message for a session that ended is anyone's again.
func TestUnreadRoutesRepliesToTheSendingSession(t *testing.T) {
	a, b := pair(t, testSecret, testSecret)
	dir := t.TempDir()
	for _, id := range []string{"s-test", "s-other"} {
		if _, err := a.RegisterSession(SessionRequest{SessionID: id, Provider: "claude", Folder: dir, Wake: WakeRewake}); err != nil {
			t.Fatal(err)
		}
	}
	q, err := a.SendRequest(SendRequest{To: "b", Body: "please check the PR", AuthorKind: AuthorAgent, SessionID: "s-test"})
	if err != nil {
		t.Fatal(err)
	}
	if rec, _ := a.chats.message(q.ID); rec.Session != "s-test" {
		t.Fatalf("sending session not recorded: %+v", rec)
	}
	eventually(t, "b has the request", func() bool { p, _ := b.Unread("", "", 10); return p.Total == 1 })
	r, err := b.SendRequest(SendRequest{ChatID: q.ChatID, ReplyTo: q.ID, Body: "done, merge it", AuthorKind: AuthorAgent})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "a has the reply", func() bool { p, _ := a.Unread("", "", 10); return p.Total == 1 })

	ids := func(p UnreadPage) []string {
		var out []string
		for _, m := range p.Messages {
			out = append(out, m.ID)
		}
		return out
	}
	if p, err := a.UnreadFor(dir, "s-other", "", 10); err != nil || p.Total != 0 {
		t.Fatalf("an unrelated session sees the reply: %+v, %v", p, err)
	}
	if p, _ := a.UnreadFor(dir, "s-test", "", 10); p.Total != 1 || !slices.Equal(ids(p), []string{r.ID}) {
		t.Fatalf("the sending session's unread: %+v", p)
	}
	if g, _ := a.Claim(ClaimRequest{IDs: []string{r.ID}, SessionID: "s-other"}); len(g) != 0 {
		t.Fatalf("another session claimed the reply: %v", g)
	}
	if g, _ := a.Claim(ClaimRequest{IDs: []string{r.ID}, SessionID: "s-test"}); !slices.Equal(g, []string{r.ID}) {
		t.Fatalf("the sending session's claim: %v", g)
	}

	// A new message of the chat with no reply_to still goes to the chat's
	// session (chat affinity), not to the first session that claims it.
	u, err := b.SendRequest(SendRequest{ChatID: q.ChatID, Body: "new topic", Ask: []string{"a"}, AuthorKind: AuthorAgent})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "a has the new message", func() bool { p, _ := a.Unread("", "", 10); return p.Total == 2 })
	if p, _ := a.UnreadFor(dir, "s-other", "", 10); p.Total != 0 {
		t.Fatalf("a session outside the chat sees its new message: %+v", p)
	}
	if g, _ := a.Claim(ClaimRequest{IDs: []string{u.ID}, SessionID: "s-other"}); len(g) != 0 {
		t.Fatalf("a session outside the chat claimed its new message: %v", g)
	}
	if p, _ := a.UnreadFor(dir, "s-test", "", 10); !slices.Equal(ids(p), []string{r.ID, u.ID}) {
		t.Fatalf("the chat's session: %+v", p)
	}

	// The sender ends: its chat is anyone's again. An ack ends the claim.
	if err := a.EndSession("s-test"); err != nil {
		t.Fatal(err)
	}
	if p, _ := a.UnreadFor(dir, "s-other", "", 10); !slices.Equal(ids(p), []string{r.ID, u.ID}) {
		t.Fatalf("after the sender ended: %+v", p)
	}
	// A session that is not registered (a headless run, an ended one) takes
	// nothing, not even a message for nobody in particular.
	for _, s := range []string{"s-headless", "s-test"} {
		if g, err := a.Claim(ClaimRequest{IDs: []string{r.ID, u.ID}, SessionID: s}); err != nil || len(g) != 0 {
			t.Fatalf("unregistered %s claimed: %v, %v", s, g, err)
		}
	}
	if g, _ := a.Claim(ClaimRequest{IDs: []string{u.ID}, SessionID: "s-other"}); !slices.Equal(g, []string{u.ID}) {
		t.Fatalf("first claim once nobody holds the chat: %v", g)
	}
	if _, err := a.Ack("", AckRequest{IDs: []string{u.ID}, SessionID: "s-other"}); err != nil {
		t.Fatal(err)
	}
	if g, _ := a.Claim(ClaimRequest{IDs: []string{u.ID}, SessionID: "s-other"}); len(g) != 0 {
		t.Fatalf("a read message claimed: %v", g)
	}
}

// The reported case: the owner session wrote in a project chat earlier; the
// peer starts a new root there (no reply_to) and then an FYI; another session
// of the folder polls and claims first. Neither may go to it, nor become read
// for the peer: both are the owner session's. A claim never acknowledged
// lapses back to the chat's session.
func TestChatAffinityKeepsNewRootsWithTheChatsSession(t *testing.T) {
	a, b := pair(t, testSecret, testSecret)
	dir := t.TempDir()
	for _, id := range []string{"s-stray", "s-owner", "s-idle"} {
		if _, err := a.RegisterSession(SessionRequest{SessionID: id, Provider: "claude", Folder: dir, Wake: WakeRewake}); err != nil {
			t.Fatal(err)
		}
	}
	q, err := a.SendRequest(SendRequest{To: "b", Body: "review PR #8", AuthorKind: AuthorAgent, SessionID: "s-owner"})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "b has the request", func() bool { p, _ := b.Unread("", "", 10); return p.Total == 1 })

	root, err := b.SendRequest(SendRequest{ChatID: q.ChatID, Body: "new: gate results", Ask: []string{"a"}, AuthorKind: AuthorAgent})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "a has the new root", func() bool { p, _ := a.Unread("", "", 10); return p.Total == 1 })
	for _, s := range []string{"s-stray", "s-idle"} {
		if p, _ := a.UnreadFor(dir, s, "", 10); p.Total != 0 {
			t.Fatalf("%s sees the owner's chat: %+v", s, p)
		}
		if g, _ := a.Claim(ClaimRequest{IDs: []string{root.ID}, SessionID: s}); len(g) != 0 {
			t.Fatalf("%s claimed the owner's new root: %v", s, g)
		}
	}
	if g, _ := a.Claim(ClaimRequest{IDs: []string{root.ID}, SessionID: "s-owner"}); len(g) != 1 {
		t.Fatalf("the owner's claim: %v", g)
	}

	// The owner took the root but never acknowledged it: after claimTTL the
	// claim lapses; the root still goes to the chat's session, not the stray.
	a.sess.claimMu.Lock()
	a.sess.claims[root.ID] = sessionClaim{session: "s-owner", at: time.Now().Add(-2 * claimTTL)}
	a.sess.claimMu.Unlock()
	if g, _ := a.Claim(ClaimRequest{IDs: []string{root.ID}, SessionID: "s-stray"}); len(g) != 0 {
		t.Fatalf("a lapsed claim went to the stray: %v", g)
	}

	fyi, err := b.SendChat(ChatSend{ChatID: q.ChatID, Body: "FYI: hold the merge"})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "a has the FYI", func() bool { p, _ := a.Unread("", "", 10); return p.Total == 2 })
	if p, _ := a.UnreadFor(dir, "s-stray", "", 10); p.Total != 0 {
		t.Fatalf("the stray sees the FYI: %+v", p)
	}
	if p, _ := a.UnreadFor(dir, "s-owner", "", 10); p.Total != 2 || p.Messages[1].ID != fyi.ID {
		t.Fatalf("the owner's unread: %+v", p)
	}
	if rec, _ := a.chats.message(fyi.ID); !rec.Unread || !rec.ReadAt.IsZero() {
		t.Fatalf("the FYI is read before the owner got it: %+v", rec)
	}
}
