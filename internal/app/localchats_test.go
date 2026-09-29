package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sync"
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
	// A topic of an agent is its own; shared names the one of every session.
	own := discussIn(t, h, map[string]any{"folder": home, "topic": "scratch", "session_id": "s1"})
	if again := discussIn(t, h, map[string]any{"folder": other, "topic": "Scratch", "session_id": "s1"}); own.Scope != ChatScopeFolderlessTemporary ||
		again.Chat != own.Chat || own.Chat == first.Chat {
		t.Fatalf("own folderless topic: %+v again %+v", own, again)
	}
	if theirs := discussIn(t, h, map[string]any{"folder": home, "topic": "scratch", "session_id": "s3"}); theirs.Chat == own.Chat {
		t.Fatal("two sessions share an own topic")
	}
	named := discussIn(t, h, map[string]any{"folder": home, "topic": "scratch", "session_id": "s1", "shared": true})
	if again := discussIn(t, h, map[string]any{"folder": other, "topic": "scratch", "session_id": "s3", "shared": true}); named.Scope != ChatScopeFolderless ||
		!named.ExpiresAt.IsZero() || again.Chat != named.Chat || named.Chat == own.Chat {
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

// chatViewOf is the local chat view of binding pid in the project list.
func chatViewOf(t *testing.T, h *harness, pid string) LocalChatView {
	t.Helper()
	for _, p := range h.app.Projects() {
		if p.ID == pid {
			if p.Chat == nil {
				t.Fatalf("project %s has no local chat view", pid)
			}
			return *p.Chat
		}
	}
	t.Fatalf("project %s not listed", pid)
	return LocalChatView{}
}

// A temporary chat is live while its owner's session is open, a caller waits
// for a reply or a turn runs; an unread reply after the owner ended does not
// keep it live (it is for a person, not the sidebar). Its owner is the stored
// one (settings.LocalChat.Owner).
func TestLocalChatLiveFollowsOwnerWaitersAndTurns(t *testing.T) {
	h := projectsHarness(t, "alice", "", func(a *App) { a.Launcher = &seatRunner{} })
	dir := repoDir(t)
	chat := discussIn(t, h, map[string]any{"folder": dir, "temporary": true, "session_id": "owner-1"})
	n := h.app.projects[chat.Project].n
	eventuallyApp(t, "seat's first turn", func() bool {
		seats := n.Seats()
		return len(seats) == 1 && seats[0].Status != node.SeatRunning && len(seats[0].Pending) == 0
	})
	v := chatViewOf(t, h, chat.Project)
	if v.Live || v.Waiting || v.Owner == nil || v.Owner.Session != "owner-1" || v.Owner.Provider != node.ProviderClaude || v.LastActive.IsZero() {
		t.Fatalf("idle chat of an ended owner: %+v owner %+v", v, v.Owner)
	}

	owner := node.SessionRequest{SessionID: "owner-1", Provider: node.ProviderClaude, Folder: dir}
	if code, raw := h.do(t, http.MethodPost, "/sessions", jsonOf(t, owner), nil); code != http.StatusOK {
		t.Fatalf("register session: %d %s", code, raw)
	}
	if v := chatViewOf(t, h, chat.Project); !v.Live || v.Waiting || v.Owner == nil || v.Owner.Session != "owner-1" {
		t.Fatalf("owner live: %+v owner %+v", v, v.Owner)
	}
	if code, raw := h.do(t, http.MethodDelete, "/sessions/owner-1", "", nil); code != http.StatusNoContent {
		t.Fatalf("end session: %d %s", code, raw)
	}
	if v := chatViewOf(t, h, chat.Project); v.Live {
		t.Fatalf("owner ended, still live: %+v", v)
	}

	h.app.discussWaiting(chat.Project, 1)
	if v := chatViewOf(t, h, chat.Project); !v.Live || !v.Waiting {
		t.Fatalf("caller waits: %+v", v)
	}
	h.app.discussWaiting(chat.Project, -1)
	if v := chatViewOf(t, h, chat.Project); v.Live || v.Waiting {
		t.Fatalf("wait ended: %+v", v)
	}

	if _, err := n.SendRequest(node.SendRequest{ChatID: chat.Chat, ReplyTo: chat.ID, Body: "answer",
		Seat: chat.Seat, AuthorKind: node.AuthorAgent}); err != nil {
		t.Fatal(err)
	}
	if v := chatViewOf(t, h, chat.Project); v.Live {
		t.Fatalf("unread reply of an ended owner keeps the chat live: %+v", v)
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

// threadRunner runs seat turns at once and gives each new session its own
// thread id; resumes are the threads its turns resumed.
type threadRunner struct {
	mu      sync.Mutex
	n       int
	resumes []string
}

func (*threadRunner) Launch(context.Context, node.LaunchSpec) error { return nil }
func (*threadRunner) Direct(string) bool                            { return true }
func (r *threadRunner) Run(_ context.Context, spec node.LaunchSpec, started func(string)) error {
	r.mu.Lock()
	id := spec.ResumeID
	if id == "" {
		r.n++
		id = fmt.Sprintf("%s-thread-%d", spec.Provider, r.n)
	} else {
		r.resumes = append(r.resumes, id)
	}
	r.mu.Unlock()
	started(id)
	return nil
}

func (r *threadRunner) resumed(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Contains(r.resumes, id)
}

// waitFor polls fn for up to 10 s (a seat turn waits for the node's 2 s poll).
func waitLong(t *testing.T, what string, fn func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if fn() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// seatThread is the thread of the only seat of local chat project pid once
// its turn ended.
func seatThread(t *testing.T, h *harness, pid string) string {
	t.Helper()
	var id string
	waitLong(t, "seat thread of "+pid, func() bool {
		h.app.mu.Lock()
		c := h.app.projects[pid]
		h.app.mu.Unlock()
		if c == nil {
			return false
		}
		seats := c.n.Seats()
		if len(seats) != 1 || seats[0].SessionID == "" || seats[0].Status == node.SeatRunning || len(seats[0].Pending) > 0 {
			return false
		}
		id = seats[0].SessionID
		return true
	})
	return id
}

// Q1: an agent session in a project folder asks in its own chat, one per
// (project, session, topic), and its thread with the asked agent continues
// there; a person and shared keep the folder's project chat.
func TestDiscussAgentSessionsOwnTheirChats(t *testing.T) {
	runner := &threadRunner{}
	h := projectsHarness(t, "alice", "", func(a *App) { a.Launcher = runner })
	dir := repoDir(t)
	human := discussIn(t, h, map[string]any{"folder": dir})
	first := discussIn(t, h, map[string]any{"folder": dir, "session_id": "s1"})
	if human.Scope != ChatScopeProject || first.Scope != ChatScopeProjectTemporary || first.Project == human.Project {
		t.Fatalf("own chat: %+v project chat: %+v", first, human)
	}
	thread := seatThread(t, h, first.Project)
	again := discussIn(t, h, map[string]any{"folder": dir, "session_id": "s1"})
	if again.Project != first.Project || again.Chat != first.Chat || again.Seat != first.Seat {
		t.Fatalf("second ask left the session's chat: %+v first %+v", again, first)
	}
	waitLong(t, "second ask resumes the thread", func() bool { return runner.resumed(thread) })
	other := discussIn(t, h, map[string]any{"folder": dir, "session_id": "s2"})
	if other.Project == first.Project || other.Project == human.Project || seatThread(t, h, other.Project) == thread {
		t.Fatalf("two sessions share a chat or thread: %+v %+v", other, first)
	}
	api := discussIn(t, h, map[string]any{"folder": dir, "session_id": "s1", "topic": "api"})
	db := discussIn(t, h, map[string]any{"folder": dir, "session_id": "s1", "topic": "db"})
	if api.Project == db.Project || api.Project == first.Project || api.Topic != "api" {
		t.Fatalf("topics of one session: %+v %+v", api, db)
	}
	if again := discussIn(t, h, map[string]any{"folder": dir, "session_id": "s1", "topic": "API"}); again.Project != api.Project {
		t.Fatalf("topic not continued: %+v", again)
	}
	if shared := discussIn(t, h, map[string]any{"folder": dir, "session_id": "s1", "shared": true}); shared.Project != human.Project {
		t.Fatalf("shared left the project chat: %+v", shared)
	}
	h.app.mu.Lock()
	i := h.app.bindingIndex(first.Project)
	lc := h.app.s.Bindings[i].Chat
	if o := lc.Owner; o == nil || o.Session != "s1" || o.Provider != node.ProviderClaude || o.Agent != "" || !lc.Temporary {
		h.app.mu.Unlock()
		t.Fatalf("owner of the session's chat: %+v", lc)
	}
	// A temporary chat of an older build (no owner) is its creator's.
	s := h.app.s
	s.Bindings = slices.Clone(s.Bindings)
	legacy := *lc
	legacy.Owner = nil
	s.Bindings[i].Chat = &legacy
	h.app.s = s
	h.app.mu.Unlock()
	if again := discussIn(t, h, map[string]any{"folder": dir, "session_id": "s1"}); again.Project != first.Project {
		t.Fatalf("legacy chat of the session not reused: %+v", again)
	}
	code, raw := h.do(t, http.MethodPost, "/discuss", jsonOf(t, map[string]any{"folder": dir, "provider": node.ProviderCodex,
		"body": "x", "session_id": "s1", "source": node.ProviderClaude, "shared": true, "temporary": true}), nil)
	if code != http.StatusBadRequest {
		t.Fatalf("shared temporary: %d %s", code, raw)
	}
}

// Each subagent of a session owns its chat and thread, reused on its next
// ask; an ask with no agent id is the session's own.
func TestDiscussSubagentsOwnTheirThreads(t *testing.T) {
	runner := &threadRunner{}
	h := projectsHarness(t, "alice", "", func(a *App) { a.Launcher = runner })
	dir := repoDir(t)
	ask := func(agent string) localChatResult {
		req := map[string]any{"folder": dir, "session_id": "s1"}
		if agent != "" {
			req["agent_id"], req["agent_type"] = agent, "Explore"
		}
		return discussIn(t, h, req)
	}
	main, a, b := ask(""), ask("agent-a"), ask("agent-b")
	if main.Project == a.Project || a.Project == b.Project || main.Project == b.Project {
		t.Fatalf("subagents share a chat: main %+v a %+v b %+v", main, a, b)
	}
	threads := map[string]string{}
	for name, r := range map[string]localChatResult{"main": main, "a": a, "b": b} {
		threads[name] = seatThread(t, h, r.Project)
	}
	if threads["a"] == threads["b"] || threads["a"] == threads["main"] {
		t.Fatalf("subagents share a thread: %v", threads)
	}
	for name, r := range map[string]localChatResult{"a": ask("agent-a"), "b": ask("agent-b"), "main": ask("")} {
		want := map[string]localChatResult{"main": main, "a": a, "b": b}[name]
		if r.Project != want.Project || r.Seat != want.Seat {
			t.Fatalf("%s's second ask left its chat: %+v, first %+v", name, r, want)
		}
		waitLong(t, name+"'s thread resumed", func() bool { return runner.resumed(threads[name]) })
	}
	h.app.mu.Lock()
	o := h.app.s.Bindings[h.app.bindingIndex(a.Project)].Chat.Owner
	h.app.mu.Unlock()
	if o == nil || o.Session != "s1" || o.Agent != "agent-a" || o.AgentType != "Explore" {
		t.Fatalf("subagent owner: %+v", o)
	}
	code, raw := h.do(t, http.MethodPost, "/discuss", jsonOf(t, map[string]any{"folder": dir, "provider": node.ProviderCodex,
		"body": "x", "session_id": "s1", "source": node.ProviderClaude, "agent_id": "bad id"}), nil)
	if code != http.StatusBadRequest {
		t.Fatalf("invalid agent id: %d %s", code, raw)
	}
}
