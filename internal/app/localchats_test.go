package app

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/UberMorgott/agent-link/internal/node"
	"github.com/UberMorgott/agent-link/internal/settings"
)

// repoDir is a new folder inside a git work tree: a project folder.
func repoDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

type localChatResult struct {
	discussResultAPI
	Scope     string    `json:"scope"`
	Topic     string    `json:"topic"`
	ExpiresAt time.Time `json:"expires_at"`
}

func discussIn(t *testing.T, h *harness, req map[string]any) localChatResult {
	t.Helper()
	req["provider"], req["body"] = node.ProviderCodex, "question"
	if req["session_id"] != nil {
		req["source"] = node.ProviderClaude
	}
	code, raw := h.do(t, http.MethodPost, "/discuss", jsonOf(t, req), nil)
	if code != http.StatusOK {
		t.Fatalf("discuss %v: %d %s", req, code, raw)
	}
	var got localChatResult
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestDiscussTemporaryAndTopicChatsBesideProjectChat(t *testing.T) {
	h := projectsHarness(t, "alice", "", func(a *App) { a.Launcher = &seatRunner{} })
	dir := repoDir(t)
	var network ProjectView
	if code, raw := h.api(t, http.MethodPost, "projects", map[string]any{"name": "Shared", "dir": dir}, &network); code != http.StatusOK {
		t.Fatalf("network project: %d %s", code, raw)
	}
	project := discussIn(t, h, map[string]any{"folder": dir})
	temp := discussIn(t, h, map[string]any{"folder": dir, "temporary": true, "session_id": "s1"})
	if project.Scope != ChatScopeProject || temp.Scope != ChatScopeProjectTemporary || temp.ExpiresAt.IsZero() ||
		temp.Chat == project.Chat || temp.Project == project.Project {
		t.Fatalf("temporary chat: %+v project chat: %+v", temp, project)
	}
	if again := discussIn(t, h, map[string]any{"folder": dir, "chat": temp.Chat, "session_id": "s2"}); again.Chat != temp.Chat || again.Project != temp.Project {
		t.Fatalf("temporary chat not continued by id: %+v", again)
	}
	if other := discussIn(t, h, map[string]any{"folder": dir, "temporary": true}); other.Chat == temp.Chat {
		t.Fatal("temporary did not start a new chat")
	}
	if again := discussIn(t, h, map[string]any{"folder": filepath.Join(dir, ".")}); again.Chat != project.Chat {
		t.Fatalf("default discuss left the project chat: %+v", again)
	}
	topic := discussIn(t, h, map[string]any{"folder": dir, "topic": "Design"})
	if again := discussIn(t, h, map[string]any{"folder": dir, "topic": "design"}); topic.Scope != ChatScopeProject ||
		topic.Topic != "Design" || !topic.ExpiresAt.IsZero() || topic.Chat == project.Chat || again.Chat != topic.Chat {
		t.Fatalf("topic chat: %+v again %+v", topic, again)
	}
	code, raw := h.do(t, http.MethodPost, "/discuss", jsonOf(t, map[string]any{"folder": dir, "provider": node.ProviderCodex,
		"body": "x", "chat": "0123456789abcdef0123456789abcdef"}), nil)
	if code != http.StatusNotFound {
		t.Fatalf("unknown chat: %d %s", code, raw)
	}
	s, _, err := settings.Load(h.app.path)
	if err != nil {
		t.Fatal(err)
	}
	if i := h.app.bindingIndex(temp.Project); i < 0 || s.Bindings[i].Chat == nil || s.Bindings[i].Chat.Project != project.Project ||
		s.Bindings[i].Dir != "" || len(s.Bindings[i].Chat.Sessions) != 2 {
		t.Fatalf("temporary binding: %+v", s.Bindings)
	}
	if got, err := route(h.app.routeContexts(), selector{folder: dir}); err != nil || got.id != network.ID {
		t.Fatalf("folder routing changed: %+v, %v", got, err)
	}
	if chats, err := h.app.projects[network.ID].n.Chats(false, false); err != nil || len(chats) != 0 {
		t.Fatalf("network project got chats: %+v, %v", chats, err)
	}
}

func TestDiscussOutsideProjectFolderUsesSessionChat(t *testing.T) {
	h := projectsHarness(t, "alice", "", func(a *App) { a.Launcher = &seatRunner{} })
	home, other := t.TempDir(), t.TempDir()
	first := discussIn(t, h, map[string]any{"folder": home, "session_id": "s1"})
	if first.Scope != ChatScopeFolderlessTemporary || first.ExpiresAt.IsZero() {
		t.Fatalf("folderless chat: %+v", first)
	}
	if again := discussIn(t, h, map[string]any{"folder": other, "session_id": "s1"}); again.Chat != first.Chat {
		t.Fatalf("session's chat not reused from another folder: %+v", again)
	}
	if second := discussIn(t, h, map[string]any{"folder": home, "session_id": "s2"}); second.Chat == first.Chat {
		t.Fatal("two sessions share a temporary chat")
	}
	named := discussIn(t, h, map[string]any{"folder": home, "topic": "scratch", "session_id": "s1"})
	if again := discussIn(t, h, map[string]any{"folder": other, "topic": "scratch", "session_id": "s3"}); named.Scope != ChatScopeFolderless ||
		!named.ExpiresAt.IsZero() || again.Chat != named.Chat {
		t.Fatalf("named folderless chat: %+v again %+v", named, again)
	}
	for _, b := range h.app.s.Bindings {
		if b.Dir != "" || b.Chat == nil || b.Chat.Project != "" {
			t.Fatalf("a folderless session bound a folder or project: %+v", b)
		}
	}
	// Its hooks read the replies to its own messages in its chats.
	q := url.Values{"folder": {home}, "session": {"s1"}}
	if code, raw := h.do(t, http.MethodGet, "/unread?"+q.Encode(), "", nil); code != http.StatusOK {
		t.Fatalf("folderless session unread: %d %s", code, raw)
	}
}

func TestGCRemovesOnlyEndedIdleTemporaryChats(t *testing.T) {
	h := projectsHarness(t, "alice", "", func(a *App) { a.Launcher = &seatRunner{} })
	dir := repoDir(t)
	live := node.SessionRequest{SessionID: "live-1", Provider: node.ProviderClaude, Folder: dir}
	project := discussIn(t, h, map[string]any{"folder": dir})
	if code, raw := h.do(t, http.MethodPost, "/sessions", jsonOf(t, live), nil); code != http.StatusOK {
		t.Fatalf("register session: %d %s", code, raw)
	}
	ended := discussIn(t, h, map[string]any{"folder": t.TempDir(), "session_id": "gone-1"})
	living := discussIn(t, h, map[string]any{"folder": dir, "temporary": true, "session_id": live.SessionID})
	pending := discussIn(t, h, map[string]any{"folder": t.TempDir(), "session_id": "gone-2"})
	named := discussIn(t, h, map[string]any{"folder": t.TempDir(), "topic": "keep"})
	for _, r := range []localChatResult{ended, living, named} {
		n := h.app.projects[r.Project].n
		for _, s := range n.Seats() {
			if err := n.RemoveSeat(s.ID); err != nil { // nothing pending for a seat
				t.Fatal(err)
			}
		}
	}
	// The pending chat has a reply its asker has not read.
	pendingNode := h.app.projects[pending.Project].n
	eventuallyApp(t, "seat's first turn", func() bool {
		seats := pendingNode.Seats()
		return len(seats) == 1 && seats[0].Status != node.SeatRunning && len(seats[0].Pending) == 0
	})
	if _, err := pendingNode.SendRequest(node.SendRequest{ChatID: pending.Chat, ReplyTo: pending.ID, Body: "answer",
		Seat: pending.Seat, AuthorKind: node.AuthorAgent}); err != nil {
		t.Fatal(err)
	}
	h.app.gcLocalChats(time.Now())
	if len(h.app.s.Bindings) != 5 {
		t.Fatalf("removed a chat before it was idle: %d bindings", len(h.app.s.Bindings))
	}
	h.app.gcLocalChats(time.Now().Add(TempChatIdle + time.Hour))
	h.app.mu.Lock()
	defer h.app.mu.Unlock()
	for what, keep := range map[string]string{"project": project.Project, "live session's": living.Project,
		"pending": pending.Project, "named": named.Project} {
		if h.app.bindingIndex(keep) < 0 {
			t.Fatalf("removed the %s chat", what)
		}
	}
	if h.app.bindingIndex(ended.Project) >= 0 || h.app.projects[ended.Project] != nil {
		t.Fatal("ended idle temporary chat kept")
	}
}
