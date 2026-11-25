package app

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/UberMorgott/agent-link/internal/node"
	"github.com/UberMorgott/agent-link/internal/settings"
)

type finishRunner struct {
	threadRunner
	archived chan string
}

func (r *finishRunner) ArchiveSessions(_ context.Context, _ string, ids []string) error {
	for _, id := range ids {
		r.archived <- id
	}
	return nil
}

func TestDiscussUntilDone(t *testing.T) {
	runner := &finishRunner{archived: make(chan string, 8)}
	h := projectsHarness(t, "alice", "", func(a *App) { a.Launcher = runner })
	dir := repoDir(t)
	project := discussIn(t, h, map[string]any{"folder": dir})
	registerSession(t, h, "owner", dir)
	body := map[string]any{"folder": dir, "session_id": "owner", "source": node.ProviderClaude, "provider": node.ProviderCodex, "body": "question"}
	code, raw := h.do(t, http.MethodPost, "/discuss/until-done", jsonOf(t, body), nil)
	var first localChatResult
	if code != http.StatusOK || json.Unmarshal([]byte(raw), &first) != nil || first.Chat == project.Chat || !first.ExpiresAt.IsZero() {
		t.Fatalf("start explicit discussion: %d %s", code, raw)
	}
	thread := seatThread(t, h, first.Project)
	if normal := discussIn(t, h, map[string]any{"folder": dir, "session_id": "owner"}); normal.Chat == first.Chat {
		t.Fatal("implicit routing picked the explicit discussion")
	}
	endSession(t, h, "owner")
	later := time.Now().Add(3 * TempChatIdle)
	h.app.retireOwnedChats(later)
	h.app.retireOwnedChats(later.Add(RetireGrace))
	h.app.gcLocalChats(later)
	if !hasBinding(h, first.Project) {
		t.Fatal("explicit discussion collected during a pause")
	}
	loadChat := func() *settings.LocalChat {
		t.Helper()
		s, ok, err := settings.Load(h.path)
		if err != nil || !ok {
			t.Fatalf("load settings: %v", err)
		}
		for _, b := range s.Bindings {
			if b.ID == first.Project {
				return b.Chat
			}
		}
		return nil
	}
	if lc := loadChat(); lc == nil || !lc.UntilDone || lc.FinishRequested {
		t.Fatal("open discussion was not persisted")
	}
	next := discussIn(t, h, map[string]any{"folder": dir, "chat": first.Chat, "session_id": "owner"})
	if next.Chat != first.Chat || seatThread(t, h, next.Project) != thread || !runner.resumed(thread) {
		t.Fatal("returning to the discussion lost the Codex session")
	}
	n := h.app.projects[first.Project].n
	reply, err := n.SendRequest(node.SendRequest{ChatID: next.Chat, ReplyTo: next.ID, Body: "answer", Seat: next.Seat, AuthorKind: node.AuthorAgent})
	if err != nil {
		t.Fatal(err)
	}
	finish := func(id string) int {
		t.Helper()
		status, result := h.do(t, http.MethodPost, "/discuss/finish", jsonOf(t, map[string]string{"chat": id}), nil)
		var v struct {
			Finished bool `json:"finished"`
		}
		if status == http.StatusOK && (json.Unmarshal([]byte(result), &v) != nil || !v.Finished) {
			t.Fatalf("finish result: %s", result)
		}
		return status
	}
	if finish("") != http.StatusBadRequest || finish("missing") != http.StatusNotFound || finish(project.Chat) != http.StatusConflict {
		t.Fatal("finish accepted an invalid or unrelated chat")
	}
	for attempt := range 2 {
		if finish(next.Chat) != http.StatusOK {
			t.Fatalf("finish failed on attempt %d", attempt+1)
		}
	}
	if lc := loadChat(); lc == nil || !lc.FinishRequested {
		t.Fatal("finish was not persisted")
	}
	h.app.gcLocalChats(later)
	if !hasBinding(h, first.Project) {
		t.Fatal("finished discussion with unread reply was collected")
	}
	body["chat"] = next.Chat
	if status, _ := h.do(t, http.MethodPost, "/discuss", jsonOf(t, body), nil); status != http.StatusConflict {
		t.Fatalf("new question after finish: %d", status)
	}
	if _, err := n.Ack(next.Chat, node.AckRequest{IDs: []string{reply.ID}}); err != nil {
		t.Fatal(err)
	}
	h.app.discussWaiting(first.Project, 1)
	h.app.gcLocalChats(later)
	if !hasBinding(h, first.Project) {
		t.Fatal("discussion collected while its caller was reading")
	}
	h.app.discussWaiting(first.Project, -1)
	h.app.gcLocalChats(later)
	if hasBinding(h, first.Project) || !hasBinding(h, project.Project) {
		t.Fatal("cleanup did not remove exactly the finished discussion")
	}
	paths, err := filepath.Glob(filepath.Join(h.app.projectsRoot(), ".left", first.Project+"-*"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("archived data: %v, %v", paths, err)
	}
	eventuallyApp(t, "archive the Codex thread", func() bool {
		select {
		case id := <-runner.archived:
			return id == thread
		default:
			return false
		}
	})
}

func TestDiscussUntilDoneSelector(t *testing.T) {
	h := projectsHarness(t, "alice", "")
	dir := t.TempDir()
	for k, v := range map[string]any{"chat": "id", "topic": "name", "temporary": true, "shared": true} {
		req := map[string]any{"folder": dir, "provider": node.ProviderCodex, "body": "question", "until_done": true, k: v}
		if code, _ := h.do(t, http.MethodPost, "/discuss", jsonOf(t, req), nil); code != http.StatusBadRequest {
			t.Fatalf("until_done with %s: %d", k, code)
		}
	}
}

// A restart while an explicitly finished question is paused must still run it
// once the owner releases the pause. A turn that is already running drains too.
func TestFinishedDiscussDrainsAfterRestart(t *testing.T) {
	h := projectsHarness(t, "alice", "", func(a *App) { a.Launcher = &threadRunner{} })
	if err := h.app.SetStopAll(true); err != nil {
		t.Fatal(err)
	}
	q := discussIn(t, h, map[string]any{"folder": t.TempDir(), "until_done": true})
	if !q.Queued {
		t.Fatal("question did not queue during the pause")
	}
	if code, raw := h.do(t, http.MethodPost, "/discuss/finish", jsonOf(t, map[string]string{"chat": q.Chat}), nil); code != http.StatusOK {
		t.Fatalf("finish while paused: %d %s", code, raw)
	}
	h.app.Stop()
	a, err := New(h.path, nil)
	if err != nil {
		t.Fatal(err)
	}
	run := &drainRunner{started: make(chan struct{}), release: make(chan struct{})}
	a.Discovery, a.Launcher = false, run
	t.Cleanup(a.Stop)
	if err := a.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	a.gcLocalChats(time.Now().Add(3 * TempChatIdle))
	active := &harness{app: a}
	if !hasBinding(active, q.Project) {
		t.Fatal("pending question lost during restart")
	}
	if err := a.SetStopAll(false); err != nil {
		t.Fatal(err)
	}
	select {
	case <-run.started:
	case <-time.After(10 * time.Second):
		t.Fatal("pending question did not run")
	}
	a.gcLocalChats(time.Now())
	if !hasBinding(active, q.Project) {
		t.Fatal("finish cancelled the running turn")
	}
	close(run.release)
	seatThread(t, active, q.Project)
	a.gcLocalChats(time.Now())
	if hasBinding(active, q.Project) {
		t.Fatal("finished discussion stayed after its turn drained")
	}
}

type drainRunner struct {
	threadRunner
	started, release chan struct{}
}

func (r *drainRunner) Run(ctx context.Context, spec node.LaunchSpec, started func(string)) error {
	if err := r.threadRunner.Run(ctx, spec, started); err != nil {
		return err
	}
	close(r.started)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-r.release:
		return nil
	}
}
