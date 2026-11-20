package app

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/UberMorgott/agent-link/internal/node"
)

// A Claude session in a folder of no project asks a local seat about another
// folder (discuss folder: a git repository, async): its hooks register it in
// its own chat, whose folder is the repository's, and the seat's reply is its
// own, not orphaned while it lives (issue #44: refused as an unbound folder,
// it was live nowhere and every reply needed a person).
func TestForeignFolderDiscussReplyReachesAskingSession(t *testing.T) {
	h := projectsHarness(t, "alice", "", func(a *App) { a.Launcher = &seatRunner{} })
	home := t.TempDir()
	repo, _ := gitWorktree(t)
	chat := discussIn(t, h, map[string]any{"folder": repo, "session_id": "s1", "topic": "broker-audit", "async": true})
	n := h.app.projects[chat.Project].n
	eventuallyApp(t, "seat's first turn", func() bool {
		seats := n.Seats()
		return len(seats) == 1 && seats[0].Status != node.SeatRunning && len(seats[0].Pending) == 0
	})

	s1 := node.SessionRequest{SessionID: "s1", Provider: node.ProviderClaude, Folder: home, Wake: node.WakeRewake, Idle: true}
	if code, raw := h.do(t, http.MethodPost, "/sessions", jsonOf(t, s1), nil); code != http.StatusOK {
		t.Fatalf("register the asking session: %d %s", code, raw)
	}
	if _, live := h.app.liveSession(h.app.routeContexts(), "s1"); !live {
		t.Fatal("asking session not live in any context")
	}
	reply, err := n.SendRequest(node.SendRequest{ChatID: chat.Chat, ReplyTo: chat.ID, Body: "answer",
		Seat: chat.Seat, AuthorKind: node.AuthorAgent})
	if err != nil {
		t.Fatal(err)
	}
	h.app.reconcileSession("s1") // what the orphan sweep asks for a reply's target

	code, raw := h.do(t, http.MethodGet, "/unread?"+url.Values{"folder": {home}, "session": {"s1"}}.Encode(), "", nil)
	if code != http.StatusOK {
		t.Fatalf("unread: %d %s", code, raw)
	}
	var page node.UnreadPage
	if err := json.Unmarshal([]byte(raw), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 1 || page.Messages[0].ID != reply.ID || page.Messages[0].NeedsHuman {
		t.Fatalf("hook's unread: %+v", page)
	}
}
