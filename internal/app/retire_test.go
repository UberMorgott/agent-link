package app

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/UberMorgott/agent-link/internal/node"
)

// holdRunner runs seat turns at once, except a turn whose prompt says "hold":
// it runs until its context ends (a live agent process), then counts it.
type holdRunner struct {
	threadRunner
	holding, cancelled atomic.Int32
}

func (r *holdRunner) Run(ctx context.Context, spec node.LaunchSpec, started func(string)) error {
	if err := r.threadRunner.Run(ctx, spec, started); err != nil || !strings.Contains(spec.Prompt, "hold") {
		return err
	}
	r.holding.Add(1)
	<-ctx.Done()
	r.cancelled.Add(1)
	return ctx.Err()
}

func registerSession(t *testing.T, h *harness, sid, dir string, agents ...string) {
	t.Helper()
	req := node.SessionRequest{SessionID: sid, Provider: node.ProviderClaude, Folder: dir, Agents: agents}
	if code, raw := h.do(t, http.MethodPost, "/sessions", jsonOf(t, req), nil); code != http.StatusOK {
		t.Fatalf("register %s: %d %s", sid, code, raw)
	}
}

func endSession(t *testing.T, h *harness, sid string) {
	t.Helper()
	if code, raw := h.do(t, http.MethodDelete, "/sessions/"+sid, "", nil); code != http.StatusNoContent {
		t.Fatalf("end %s: %d %s", sid, code, raw)
	}
}

func hasBinding(h *harness, pid string) bool {
	h.app.mu.Lock()
	defer h.app.mu.Unlock()
	return h.app.bindingIndex(pid) >= 0
}

// A subagent's stop retires its chat alone after the grace, ending its running
// turn; a session back within the grace keeps its chats; its end for good
// retires the rest; the project chat stays.
func TestRetireOwnedChatsWhenOwnersEnd(t *testing.T) {
	runner := &holdRunner{}
	h := projectsHarness(t, "alice", "", func(a *App) { a.Launcher = runner })
	dir := repoDir(t)
	project := discussIn(t, h, map[string]any{"folder": dir})
	registerSession(t, h, "s1", dir, "agent-a", "agent-b")
	main := discussIn(t, h, map[string]any{"folder": dir, "session_id": "s1"})
	a := discussIn(t, h, map[string]any{"folder": dir, "session_id": "s1", "agent_id": "agent-a"})
	req := map[string]any{"folder": dir, "provider": node.ProviderCodex, "body": "hold this", "session_id": "s1",
		"source": node.ProviderClaude, "agent_id": "agent-b"}
	code, raw := h.do(t, http.MethodPost, "/discuss", jsonOf(t, req), nil)
	if code != http.StatusOK {
		t.Fatalf("discuss: %d %s", code, raw)
	}
	var b localChatResult
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		t.Fatal(err)
	}
	waitLong(t, "subagent b's turn runs", func() bool { return runner.holding.Load() == 1 })

	now := time.Now()
	h.app.retireOwnedChats(now)
	registerSession(t, h, "s1", dir, "agent-a") // SubagentStop of agent-b
	h.app.retireOwnedChats(now.Add(time.Second))
	h.app.retireOwnedChats(now.Add(RetireGrace))
	if !hasBinding(h, b.Project) {
		t.Fatal("retired before the grace passed")
	}
	h.app.retireOwnedChats(now.Add(RetireGrace + time.Second))
	if hasBinding(h, b.Project) || runner.cancelled.Load() != 1 {
		t.Fatalf("stopped subagent's chat kept (%v) or its turn not ended (%d)", hasBinding(h, b.Project), runner.cancelled.Load())
	}
	for _, pid := range []string{project.Project, main.Project, a.Project} {
		if !hasBinding(h, pid) {
			t.Fatalf("chat %s retired with another owner", pid)
		}
	}

	now = now.Add(time.Hour)
	endSession(t, h, "s1")
	h.app.retireOwnedChats(now)
	registerSession(t, h, "s1", dir, "agent-a") // resumed within the grace
	h.app.retireOwnedChats(now.Add(RetireGrace + time.Second))
	if !hasBinding(h, main.Project) || !hasBinding(h, a.Project) {
		h.app.mu.Lock()
		t.Logf("owners %+v live %+v", h.app.owners, h.app.liveAgentsLocked())
		h.app.mu.Unlock()
		t.Fatalf("a session back within the grace lost its chats: main %v a %v", hasBinding(h, main.Project), hasBinding(h, a.Project))
	}

	now = now.Add(time.Hour)
	endSession(t, h, "s1")
	h.app.retireOwnedChats(now)
	h.app.retireOwnedChats(now.Add(RetireGrace))
	if hasBinding(h, main.Project) || hasBinding(h, a.Project) || !hasBinding(h, project.Project) {
		t.Fatalf("session end: main %v, subagent %v, project chat %v", hasBinding(h, main.Project), hasBinding(h, a.Project),
			hasBinding(h, project.Project))
	}
}

// An unread reply keeps a retired chat, hidden and out of routing, until it
// is read; an owner never seen live (registered nowhere) is not taken for gone.
func TestRetiredChatKeepsUnreadReplyHidden(t *testing.T) {
	h := projectsHarness(t, "alice", "", func(a *App) { a.Launcher = &threadRunner{} })
	dir := repoDir(t)
	discussIn(t, h, map[string]any{"folder": dir})
	registerSession(t, h, "s1", dir)
	ask := discussIn(t, h, map[string]any{"folder": dir, "session_id": "s1"})
	seatThread(t, h, ask.Project)
	n := h.app.projects[ask.Project].n
	reply, err := n.SendRequest(node.SendRequest{ChatID: ask.Chat, ReplyTo: ask.ID, Body: "answer", Seat: ask.Seat, AuthorKind: node.AuthorAgent})
	if err != nil {
		t.Fatal(err)
	}
	unseen := discussIn(t, h, map[string]any{"folder": t.TempDir(), "session_id": "nowhere"})

	endSession(t, h, "s1")
	now := time.Now()
	h.app.retireOwnedChats(now)
	h.app.retireOwnedChats(now.Add(RetireGrace))
	h.app.retireOwnedChats(now.Add(24 * time.Hour))
	if !hasBinding(h, unseen.Project) {
		t.Fatal("a chat of a session registered nowhere was retired")
	}
	var view *LocalChatView
	for _, p := range h.app.Projects() {
		if p.ID == ask.Project {
			view = p.Chat
		}
	}
	h.app.mu.Lock()
	lc := h.app.s.Bindings[h.app.bindingIndex(ask.Project)].Chat
	h.app.mu.Unlock()
	if view == nil || view.Live || lc.Retired.IsZero() || len(n.Seats()) != 0 {
		t.Fatalf("retired chat with an unread reply: view %+v, chat %+v, seats %+v", view, lc, n.Seats())
	}
	registerSession(t, h, "s1", dir)
	if again := discussIn(t, h, map[string]any{"folder": dir, "session_id": "s1"}); again.Project == ask.Project {
		t.Fatal("discuss routed to a retired chat")
	}
	h.app.gcLocalChats(time.Now())
	if !hasBinding(h, ask.Project) {
		t.Fatal("unread reply dropped")
	}
	if _, err := n.Ack(ask.Chat, node.AckRequest{IDs: []string{reply.ID}}); err != nil {
		t.Fatal(err)
	}
	h.app.gcLocalChats(time.Now())
	if hasBinding(h, ask.Project) {
		t.Fatal("retired chat kept after its reply was read")
	}
}

// A temporary chat of no owner never becomes its first session's (Unowned);
// a retired chat continued by id becomes the caller's.
func TestUnownedAndAdoptedChats(t *testing.T) {
	h := projectsHarness(t, "alice", "", func(a *App) { a.Launcher = &threadRunner{} })
	dir := repoDir(t)
	human := discussIn(t, h, map[string]any{"folder": dir, "temporary": true})
	discussIn(t, h, map[string]any{"folder": dir, "chat": human.Chat, "session_id": "s1"})
	own := discussIn(t, h, map[string]any{"folder": dir, "session_id": "s1"})
	if own.Project == human.Project {
		t.Fatal("a person's temporary chat became the session's own")
	}
	h.app.mu.Lock()
	i := h.app.bindingIndex(own.Project)
	s := h.app.s
	s.Bindings = slices.Clone(s.Bindings)
	lc := *s.Bindings[i].Chat
	lc.Retired = time.Now().UTC()
	s.Bindings[i].Chat = &lc
	h.app.s = s
	h.app.mu.Unlock()
	if again := discussIn(t, h, map[string]any{"folder": dir, "chat": own.Chat, "session_id": "s2"}); again.Project != own.Project {
		t.Fatalf("retired chat not continued by id: %+v", again)
	}
	h.app.mu.Lock()
	got := h.app.s.Bindings[h.app.bindingIndex(own.Project)].Chat
	h.app.mu.Unlock()
	if !got.Retired.IsZero() || got.Owner == nil || got.Owner.Session != "s2" {
		t.Fatalf("adopted chat: %+v", got)
	}
}

// An owner that registers again while its chat's retirement waits for the
// owner's lock keeps its chat and seats.
func TestRetireRechecksOwnerUnderItsLock(t *testing.T) {
	h := projectsHarness(t, "alice", "", func(a *App) { a.Launcher = &threadRunner{} })
	dir := repoDir(t)
	project := discussIn(t, h, map[string]any{"folder": dir})
	registerSession(t, h, "s1", dir)
	own := discussIn(t, h, map[string]any{"folder": dir, "session_id": "s1"})
	seatThread(t, h, own.Project)
	endSession(t, h, "s1")
	now := time.Now()
	h.app.retireOwnedChats(now)
	mu := h.app.ownerLock("s1")
	mu.Lock()
	done := make(chan struct{})
	go func() { h.app.retireOwnedChats(now.Add(RetireGrace)); close(done) }()
	time.Sleep(50 * time.Millisecond)
	// Back meanwhile, registered where the hook would (the folder's local project).
	if _, err := h.app.projects[project.Project].n.RegisterSession(node.SessionRequest{SessionID: "s1", Provider: node.ProviderClaude, Folder: dir}); err != nil {
		mu.Unlock()
		t.Fatal(err)
	}
	mu.Unlock()
	<-done
	if !hasBinding(h, own.Project) || len(h.app.projects[own.Project].n.Seats()) != 1 {
		t.Fatal("a returning owner's chat was retired")
	}
}
