package app

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/UberMorgott/agent-link/internal/config"
	"github.com/UberMorgott/agent-link/internal/node"
	"github.com/UberMorgott/agent-link/internal/settings"
	"github.com/UberMorgott/agent-link/internal/worker"
)

// newBinding is a fresh project binding with folder dir ("" = none).
func newBinding(t *testing.T, dir string) settings.ProjectBinding {
	t.Helper()
	id, err := config.NewProjectID()
	if err != nil {
		t.Fatal(err)
	}
	secret, err := config.NewProjectSecret()
	if err != nil {
		t.Fatal(err)
	}
	return settings.ProjectBinding{ID: id, Epoch: config.ProjectEpoch, Secret: secret, Dir: dir}
}

// startApp saves s (a loopback listener, no handler unless set) and starts an
// App on it without discovery.
func startApp(t *testing.T, s settings.Settings, setup ...func(*App)) *App {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if s.Node == "" {
		s.Node = "alice"
	}
	if s.Handler == "" {
		s.Handler = worker.HandlerNone
	}
	if s.Listen == "" {
		s.Listen = "127.0.0.1:0"
	}
	if err := settings.Save(path, s); err != nil {
		t.Fatal(err)
	}
	a, err := New(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	a.SetAutostart, a.AutostartState, a.Discovery = nil, nil, false
	for _, f := range setup {
		f(a)
	}
	t.Cleanup(a.Stop)
	if err := a.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	return a
}

// waitTopic waits for an event that carries topic.
func waitTopic(t *testing.T, s *eventSubscription, topic string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-s.wake:
			if slices.Contains(s.snapshot().Topics, topic) {
				return
			}
		case <-deadline:
			t.Fatalf("no event %q", topic)
		}
	}
}

// The legacy network and every project run on one Hub; a project without a
// folder has no worker and no folder at all; project data without a binding
// moves to .left; every node and worker change carries its project topic.
func TestContextsRunOnHub(t *testing.T) {
	withDir, noDir := newBinding(t, t.TempDir()), newBinding(t, "")
	var orphan string
	a := startApp(t, settings.Settings{Code: "K7Q2-MXPA-4RTB", Bindings: []settings.ProjectBinding{withDir, noDir}},
		func(a *App) {
			orphan = a.projectDir(newBinding(t, "").ID)
			if err := os.MkdirAll(orphan, 0o700); err != nil {
				t.Fatal(err)
			}
		})
	a.mu.Lock()
	legacy, p1, p2, hub := a.legacy, a.projects[withDir.ID], a.projects[noDir.ID], a.hub
	a.mu.Unlock()
	if legacy == nil || legacy.w == nil || hub.Node("") != legacy.n {
		t.Fatalf("legacy context %+v", legacy)
	}
	if p1 == nil || p1.w == nil || hub.Node(withDir.ID) != p1.n || p1.n.NeedsFolder() {
		t.Fatalf("project with a folder %+v", p1)
	}
	if _, ok := p1.n.FolderArea(withDir.Dir); !ok {
		t.Fatal("the bound folder is not the project's")
	}
	if p2 == nil || p2.w != nil || hub.Node(noDir.ID) != p2.n || !p2.n.NeedsFolder() {
		t.Fatalf("project without a folder %+v", p2)
	}
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphaned project data not moved: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(a.projectsRoot(), ".left", "*")); len(left) != 1 {
		t.Fatalf(".left holds %v", left)
	}
	if st := a.Status(); !st.Running {
		t.Fatalf("status %+v", st)
	}

	sub := a.events.subscribe()
	defer sub.close()
	<-sub.wake
	sub.snapshot() // the initial "all"
	if _, err := legacy.n.RegisterSession(node.SessionRequest{SessionID: "s1", Provider: "claude", Folder: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	waitTopic(t, sub, "project:legacy")
	if err := p1.w.Accept(node.Message{ID: "0123456789abcdef0123456789abcdef", From: "bob", To: "alice", Body: "hi", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	waitTopic(t, sub, "project:"+withDir.ID)
}

// Leaving a project removes its binding, stops its context and moves its
// data to .left; with unfinished jobs it refuses and keeps everything.
func TestLeaveProject(t *testing.T) {
	b := newBinding(t, t.TempDir())
	on := true
	a := startApp(t, settings.Settings{Handler: worker.HandlerClaude, HandlerCommand: []string{"agentlink-test-no-such-agent"},
		AutoAnswer: &on, Bindings: []settings.ProjectBinding{b}})
	a.mu.Lock()
	c, slots := a.projects[b.ID], a.slots
	a.mu.Unlock()
	// Every shared slot is taken: a new job stays queued, so the worker is busy.
	for range worker.DefaultMaxJobs {
		defer slots.Hold()()
	}
	if err := c.w.Accept(node.Message{ID: "0123456789abcdef0123456789abcdef", From: "bob", To: "alice", Body: "hi", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := a.LeaveProject(b.ID); !errors.Is(err, worker.ErrBusy) {
		t.Fatalf("leave while busy: %v", err)
	}
	if s, _, _ := settings.Load(a.path); len(s.Bindings) != 1 {
		t.Fatal("a refused leave dropped the binding")
	}
	if err := c.w.Accept(node.Message{ID: "fedcba9876543210fedcba9876543210", From: "bob", To: "alice", Body: "more", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("intake closed after a refused leave: %v", err)
	}
	c.w.Cancel("0123456789abcdef0123456789abcdef")
	c.w.Cancel("fedcba9876543210fedcba9876543210")
	eventuallyApp(t, "worker idle", func() bool { return !c.w.Busy() })

	if err := a.LeaveProject(b.ID); err != nil {
		t.Fatal(err)
	}
	if s, _, _ := settings.Load(a.path); len(s.Bindings) != 0 {
		t.Fatalf("binding kept: %+v", s.Bindings)
	}
	a.mu.Lock()
	gone := a.projects[b.ID] == nil && a.hub.Node(b.ID) == nil
	a.mu.Unlock()
	if !gone {
		t.Fatal("context still runs")
	}
	if _, err := os.Stat(a.projectDir(b.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("data not moved: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(a.projectsRoot(), ".left", b.ID+"-*")); len(left) != 1 {
		t.Fatalf(".left holds %v", left)
	}
	if err := a.LeaveProject(b.ID); !errors.Is(err, ErrUnknownProject) {
		t.Fatalf("second leave: %v", err)
	}
}

// Leaving the legacy network clears its code and secret and keeps its data.
func TestLeaveLegacy(t *testing.T) {
	a := startApp(t, settings.Settings{Code: "K7Q2-MXPA-4RTB"})
	if err := a.LeaveProject(LegacyProjectID); err != nil {
		t.Fatal(err)
	}
	if s, _, _ := settings.Load(a.path); s.Code != "" || s.Secret != "" {
		t.Fatalf("code kept: %+v", s)
	}
	if a.node() != nil {
		t.Fatal("legacy context still runs")
	}
	if _, err := os.Stat(filepath.Join(a.dataRoot(), "node_id")); err != nil {
		t.Fatalf("legacy data gone: %v", err)
	}
}

// A folder change restarts the context in the new folder, starts a worker
// for a project that had none and deletes the agent sessions of the old one.
func TestSetProjectDir(t *testing.T) {
	b := newBinding(t, "")
	a := startApp(t, settings.Settings{Bindings: []settings.ProjectBinding{b}})
	stale := filepath.Join(a.projectDir(b.ID), "sessions", "chat.json")
	if err := os.MkdirAll(filepath.Dir(stale), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	a.mu.Lock()
	err := a.setProjectDirLocked(b.ID, dir)
	c := a.projects[b.ID]
	a.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if c == nil || c.w == nil || c.n.NeedsFolder() {
		t.Fatalf("context after the folder change %+v", c)
	}
	if _, ok := c.n.FolderArea(dir); !ok {
		t.Fatal("new folder not bound")
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("agent session of the old folder kept: %v", err)
	}
	if s, _, _ := settings.Load(a.path); s.Bindings[0].Dir != dir {
		t.Fatalf("saved dir %q", s.Bindings[0].Dir)
	}
}
