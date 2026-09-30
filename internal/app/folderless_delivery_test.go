package app

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"testing"

	"github.com/UberMorgott/agent-link/internal/node"
)

// A Claude session in a folder of no project asks a local seat (discuss,
// async): the seat's reply reaches that session. Its hooks register it (the
// folder's only contexts are its own local chats, sessionContexts), else they
// took the folder for unbound and delivered nothing; its waiter wakes it with
// the reply, and its next prompt gets it (owner report 2026-09-30: replies
// seen only in history).
func TestFolderlessDiscussReplyReachesAskingSession(t *testing.T) {
	h := projectsHarness(t, "alice", "", func(a *App) { a.Launcher = &seatRunner{} })
	home := t.TempDir()
	chat := discussIn(t, h, map[string]any{"folder": home, "session_id": "s1", "topic": "towns", "async": true})
	n := h.app.projects[chat.Project].n
	eventuallyApp(t, "seat's first turn", func() bool {
		seats := n.Seats()
		return len(seats) == 1 && seats[0].Status != node.SeatRunning && len(seats[0].Pending) == 0
	})
	reply, err := n.SendRequest(node.SendRequest{ChatID: chat.Chat, ReplyTo: chat.ID, Body: "answer",
		Seat: chat.Seat, AuthorKind: node.AuthorAgent})
	if err != nil {
		t.Fatal(err)
	}

	// The hooks' heartbeat: registered, not refused as an unbound folder.
	s1 := node.SessionRequest{SessionID: "s1", Provider: node.ProviderClaude, Folder: home, Wake: node.WakeRewake, Idle: true}
	if code, raw := h.do(t, http.MethodPost, "/sessions", jsonOf(t, s1), nil); code != http.StatusOK {
		t.Fatalf("register a session of a folderless chat: %d %s", code, raw)
	}
	if _, live := h.app.liveSession(h.app.routeContexts(), "s1"); !live {
		t.Fatal("session not live in its chat's context")
	}

	// The waiter sees the reply and claims it as a wake.
	unread := func(extra url.Values) node.UnreadPage {
		t.Helper()
		q := url.Values{"folder": {home}, "session": {"s1"}, "actionable": {"1"}}
		maps.Copy(q, extra)
		code, raw := h.do(t, http.MethodGet, "/unread?"+q.Encode(), "", nil)
		if code != http.StatusOK {
			t.Fatalf("unread: %d %s", code, raw)
		}
		var page node.UnreadPage
		if err := json.Unmarshal([]byte(raw), &page); err != nil {
			t.Fatal(err)
		}
		return page
	}
	if page := unread(url.Values{"waiter": {"1"}, "limit": {"1"}}); page.Total != 1 {
		t.Fatalf("waiter's unread: %+v", page)
	}
	page := unread(nil)
	if len(page.Messages) != 1 || page.Messages[0].ID != reply.ID {
		t.Fatalf("hook's unread: %+v", page)
	}
	claim := node.ClaimRequest{IDs: []string{reply.ID}, SessionID: "s1", Folder: home, WakeToken: "wake-token-1"}
	code, raw := h.do(t, http.MethodPost, "/claim?project="+chat.Project, jsonOf(t, claim), nil)
	var granted []string
	if code != http.StatusOK || json.Unmarshal([]byte(raw), &granted) != nil || !slices.Contains(granted, reply.ID) {
		t.Fatalf("waiter's claim: %d %s", code, raw)
	}
}
