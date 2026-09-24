package node

import (
	"slices"
	"testing"
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

	// A new message for nobody in particular: the first claim takes it.
	u, err := b.SendRequest(SendRequest{ChatID: q.ChatID, Body: "new topic", Ask: []string{"a"}, AuthorKind: AuthorAgent})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "a has the new message", func() bool { p, _ := a.Unread("", "", 10); return p.Total == 2 })
	if p, _ := a.UnreadFor(dir, "s-other", "", 10); !slices.Equal(ids(p), []string{u.ID}) {
		t.Fatalf("before the claim every session may take it: %+v", p)
	}
	if g, _ := a.Claim(ClaimRequest{IDs: []string{u.ID}, SessionID: "s-other"}); !slices.Equal(g, []string{u.ID}) {
		t.Fatalf("first claim: %v", g)
	}
	if g, _ := a.Claim(ClaimRequest{IDs: []string{u.ID}, SessionID: "s-test"}); len(g) != 0 {
		t.Fatalf("second claim granted too: %v", g)
	}
	if p, _ := a.UnreadFor(dir, "s-test", "", 10); !slices.Equal(ids(p), []string{r.ID}) {
		t.Fatalf("claimed by another session and still listed: %+v", p)
	}

	// The sender ends: its reply is anyone's again. An ack ends the claim.
	if err := a.EndSession("s-test"); err != nil {
		t.Fatal(err)
	}
	if p, _ := a.UnreadFor(dir, "s-other", "", 10); !slices.Equal(ids(p), []string{r.ID, u.ID}) {
		t.Fatalf("after the sender ended: %+v", p)
	}
	if _, err := a.Ack("", AckRequest{IDs: []string{u.ID}, SessionID: "s-other"}); err != nil {
		t.Fatal(err)
	}
	if g, _ := a.Claim(ClaimRequest{IDs: []string{u.ID}, SessionID: "s-other"}); len(g) != 0 {
		t.Fatalf("a read message claimed: %v", g)
	}
}
