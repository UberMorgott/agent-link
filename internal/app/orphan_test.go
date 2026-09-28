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
}
