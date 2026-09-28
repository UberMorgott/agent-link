package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/UberMorgott/agent-link/internal/node"
)

// A network-registered session asks the folder's local seat: when it ends,
// the seat's reply in the local project needs a person although the session
// was never registered there; it coming back takes the reply back; only a
// reassign (to a session live in any context, by force while its session
// lives) hands it on, and that session then claims it in the local project.
func TestOrphanedLocalSeatReplyOfNetworkSession(t *testing.T) {
	p := newBinding(t, t.TempDir())
	alice, _ := pairApps(t, p)
	alice.Launcher = &seatRunner{}
	srv := httptest.NewServer(alice.Handler())
	defer srv.Close()
	register := func(sid string) {
		t.Helper()
		session := node.SessionRequest{SessionID: sid, Provider: node.ProviderClaude, Folder: p.Dir}
		if code, body := call(t, srv, http.MethodPost, "/sessions", jsonOf(t, session)); code != http.StatusOK {
			t.Fatalf("register %s: %d %s", sid, code, body)
		}
	}
	register("claude-one")
	register("claude-two")
	code, body := call(t, srv, http.MethodPost, "/discuss", jsonOf(t, map[string]string{
		"folder": p.Dir, "provider": node.ProviderCodex, "source": node.ProviderClaude, "session_id": "claude-one", "body": "question",
	}))
	var ask discussResultAPI
	if code != http.StatusOK || json.Unmarshal([]byte(body), &ask) != nil || ask.Project == p.ID {
		t.Fatalf("discuss: %d %s", code, body)
	}
	local := alice.projects[ask.Project].n
	eventuallyApp(t, "seat setup", func() bool {
		for _, s := range local.Seats() {
			if s.SessionID == "" || s.LastTurn.IsZero() || s.Status == node.SeatRunning {
				return false
			}
		}
		return true
	})
	reply, err := local.SendRequest(node.SendRequest{ChatID: ask.Chat, ReplyTo: ask.ID, Body: "answer", Seat: ask.Seat, AuthorKind: node.AuthorAgent})
	if err != nil {
		t.Fatal(err)
	}
	if len(local.Sessions()) != 0 {
		t.Fatal("the network session was registered in the local project")
	}
	msg := func() node.ChatMessage {
		t.Helper()
		msgs, err := local.ChatMessages(ask.Chat, 0, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		i := slices.IndexFunc(msgs, func(m node.ChatMessage) bool { return m.ID == reply.ID })
		if i < 0 {
			t.Fatalf("no reply in %+v", msgs)
		}
		return msgs[i]
	}
	reassign := func(sid string, force bool) (int, string) {
		return call(t, srv, http.MethodPost, "/reassign", jsonOf(t, node.ReassignRequest{ID: reply.ID, SessionID: sid, Force: force}))
	}
	if code, body := reassign("claude-two", false); code != http.StatusConflict {
		t.Fatalf("reassign of a live session's reply without force: %d %s", code, body)
	}
	end := func() {
		t.Helper()
		if code, body := call(t, srv, http.MethodDelete, "/sessions/claude-one", ""); code != http.StatusNoContent {
			t.Fatalf("end session: %d %s", code, body)
		}
	}
	end()
	eventuallyApp(t, "local reply needs a person", func() bool { m := msg(); return m.NeedsHuman && m.OrphanedSession == "claude-one" })
	register("claude-one")
	eventuallyApp(t, "the session back takes its reply", func() bool { return !msg().NeedsHuman })
	end()
	eventuallyApp(t, "needs a person again", func() bool { return msg().NeedsHuman })
	// Events are hints: a late "live" run (reordered after the end) keeps
	// the mark; a "gone" run that comes after the session registered again
	// does not orphan it.
	alice.reconcileSession("claude-one")
	if !msg().NeedsHuman {
		t.Fatal("a late run cleared the mark of a session that is gone")
	}
	mu := alice.sessionLock("claude-one")
	mu.Lock()
	register("claude-one") // its own run waits for the lock
	mu.Unlock()
	alice.reconcileSession("claude-one") // the end's run, late
	eventuallyApp(t, "registered again: its reply", func() bool { return !msg().NeedsHuman })
	end()
	eventuallyApp(t, "gone again", func() bool { return msg().NeedsHuman })

	// A reassign whose target ends meanwhile: under the target's lock it
	// sees the end (404), and the reply stays the owner's to decide.
	two := alice.sessionLock("claude-two")
	two.Lock()
	type result struct {
		code int
		body string
	}
	done := make(chan result, 1)
	go func() { c, b := reassign("claude-two", false); done <- result{c, b} }()
	if code, body := call(t, srv, http.MethodDelete, "/sessions/claude-two", ""); code != http.StatusNoContent {
		t.Fatalf("end claude-two: %d %s", code, body)
	}
	two.Unlock()
	if r := <-done; r.code != http.StatusNotFound || !msg().NeedsHuman || msg().OrphanedSession != "claude-one" {
		t.Fatalf("reassign racing its target's end: %d %s, %+v", r.code, r.body, msg())
	}
	register("claude-two")

	if code, body := reassign("claude-gone", false); code != http.StatusNotFound {
		t.Fatalf("reassign to no live session: %d %s", code, body)
	}
	if code, body := reassign("claude-two", false); code != http.StatusOK || !strings.Contains(body, `"assigned":"session:claude-two"`) {
		t.Fatalf("reassign to the other network session: %d %s", code, body)
	}
	code, body = call(t, srv, http.MethodGet, "/unread?"+url.Values{"folder": {p.Dir}, "session": {"claude-two"}}.Encode(), "")
	if code != http.StatusOK || !strings.Contains(body, reply.ID) {
		t.Fatalf("reassigned reply not in the new session's unread: %d %s", code, body)
	}
	code, body = call(t, srv, http.MethodPost, "/claim", jsonOf(t, node.ClaimRequest{IDs: []string{reply.ID}, SessionID: "claude-two", Folder: p.Dir}))
	var granted []string
	if code != http.StatusOK || json.Unmarshal([]byte(body), &granted) != nil || !slices.Equal(granted, []string{reply.ID}) {
		t.Fatalf("claim by the new session: %d %s", code, body)
	}
	// The session it was handed to ends: the reply needs a person again.
	if code, body := call(t, srv, http.MethodDelete, "/sessions/claude-two", ""); code != http.StatusNoContent {
		t.Fatalf("end claude-two: %d %s", code, body)
	}
	eventuallyApp(t, "reassigned reply orphaned by its new session's end", func() bool {
		m := msg()
		return m.NeedsHuman && m.OrphanedSession == "claude-two"
	})
}
