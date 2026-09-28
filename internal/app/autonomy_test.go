package app

import (
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/UberMorgott/agent-link/internal/node"
	"github.com/UberMorgott/agent-link/internal/settings"
)

// The emergency stop: saved, applied to every running project and to a
// project started later, kept by a settings save, shown in the status; the
// tray hears of a change.
func TestStopAll(t *testing.T) {
	var mu sync.Mutex
	var heard []bool
	h := projectsHarness(t, "alice", "", func(a *App) {
		a.StopAllChanged = func(on bool) { mu.Lock(); heard = append(heard, on); mu.Unlock() }
	})
	var site ProjectView
	if code, raw := h.api(t, http.MethodPost, "projects", map[string]any{"name": "Сайт", "dir": t.TempDir()}, &site); code != http.StatusOK {
		t.Fatalf("create: %d %s", code, raw)
	}
	var st Status
	if code, raw := h.api(t, http.MethodPost, "autonomy/stop", map[string]any{"on": true}, &st); code != http.StatusOK || !st.StopAll {
		t.Fatalf("stop: %d %s", code, raw)
	}
	if !h.app.projects[site.ID].n.Stopped() || !h.app.Settings().StopAll || !h.app.Status().StopAll {
		t.Fatal("the stop is not applied")
	}
	if s, _, err := settings.Load(h.app.path); err != nil || !s.StopAll {
		t.Fatalf("the stop is not saved: %v", err)
	}
	// A settings save (no stop_all in the form) keeps it; the restarted and a
	// new project's node are stopped too.
	if code, raw := h.do(t, http.MethodPost, "/ui/api/settings", jsonOf(t, map[string]any{"node": "alice", "listen": "127.0.0.1:0", "handler": "none"}), h.tokenHdr()); code != http.StatusOK ||
		strings.Contains(raw, `"error"`) {
		t.Fatalf("save: %d %s", code, raw)
	}
	var other ProjectView
	if code, raw := h.api(t, http.MethodPost, "projects", map[string]any{"name": "Второй", "dir": t.TempDir()}, &other); code != http.StatusOK {
		t.Fatalf("create: %d %s", code, raw)
	}
	if !h.app.Settings().StopAll || !h.app.projects[site.ID].n.Stopped() || !h.app.projects[other.ID].n.Stopped() {
		t.Fatal("the stop was lost by a save or a new project")
	}
	st = Status{}
	if code, raw := h.api(t, http.MethodPost, "autonomy/stop", map[string]any{"on": false}, &st); code != http.StatusOK || st.StopAll ||
		h.app.projects[site.ID].n.Stopped() || h.app.projects[other.ID].n.Stopped() {
		t.Fatalf("start again: %d %s", code, raw)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(heard) != 2 || !heard[0] || heard[1] {
		t.Fatalf("the tray heard %v", heard)
	}
}

// A folder bound to a network and a local project runs two contexts: the
// global stop halts both, and the status and project views report each
// scope's state.
func TestStopAllBothScopes(t *testing.T) {
	h := projectsHarness(t, "alice", "")
	dir := t.TempDir()
	var network ProjectView
	if code, raw := h.api(t, http.MethodPost, "projects", map[string]any{"name": "Shared", "dir": dir}, &network); code != http.StatusOK {
		t.Fatalf("create: %d %s", code, raw)
	}
	local := localProjectForTest(t, h, "Local", dir)
	scopes := func(st Status) map[string]ContextAutonomy {
		out := map[string]ContextAutonomy{}
		for _, c := range st.Autonomy {
			out[c.Scope] = c
		}
		return out
	}
	if got := scopes(h.app.Status()); len(got) != 2 || got[settings.ProjectScopeNetwork].Project != network.ID ||
		got[settings.ProjectScopeLocal].Project != local.ID || got[settings.ProjectScopeLocal].Halted || got[settings.ProjectScopeNetwork].Halted {
		t.Fatalf("status before the stop: %+v", h.app.Status().Autonomy)
	}
	var st Status
	if code, raw := h.api(t, http.MethodPost, "autonomy/stop", map[string]any{"on": true}, &st); code != http.StatusOK || !st.StopAll {
		t.Fatalf("stop: %d %s", code, raw)
	}
	if got := scopes(st); !got[settings.ProjectScopeLocal].Halted || !got[settings.ProjectScopeNetwork].Halted {
		t.Fatalf("stop did not halt both scopes: %+v", st.Autonomy)
	}
	var list []ProjectView
	if code, raw := h.api(t, http.MethodGet, "projects", nil, &list); code != http.StatusOK || len(list) != 2 {
		t.Fatalf("projects: %d %s", code, raw)
	}
	for _, v := range list {
		n := h.app.projects[v.ID].n
		if !n.Stopped() || !n.AutoHeld(node.Message{}) {
			t.Fatalf("project %s still works by itself", v.ID)
		}
		if v.Autonomy == nil || !v.Autonomy.Halted || v.Autonomy.Stopped {
			t.Fatalf("view of %s: %+v", v.ID, v.Autonomy)
		}
	}
	st = Status{}
	if code, raw := h.api(t, http.MethodPost, "autonomy/stop", map[string]any{"on": false}, &st); code != http.StatusOK {
		t.Fatalf("start: %d %s", code, raw)
	}
	if got := scopes(st); got[settings.ProjectScopeLocal].Halted || got[settings.ProjectScopeNetwork].Halted {
		t.Fatalf("still halted: %+v", st.Autonomy)
	}
}

// A project's manual pause persists and combines with the global pause.
// Human messages remain in the shared chat and wait for the asked seat.
func TestProjectAgentPausePersistsAndQueues(t *testing.T) {
	runner := &seatRunner{}
	h := projectsHarness(t, "alice", "", func(a *App) { a.Launcher = runner })
	site := localProjectForTest(t, h, "Site", t.TempDir())
	var other ProjectView
	if code, raw := h.api(t, http.MethodPost, "projects", map[string]any{"name": "Other", "dir": t.TempDir()}, &other); code != http.StatusOK {
		t.Fatalf("create other: %d %s", code, raw)
	}
	path := "projects/" + site.ID + "/autonomy/stop"
	if code, raw := h.api(t, http.MethodPost, path, map[string]any{"on": true}, &site); code != http.StatusOK || site.Autonomy == nil || !site.Autonomy.Stopped {
		t.Fatalf("pause site: %d %s", code, raw)
	}
	if !h.app.projects[site.ID].n.Stopped() || h.app.projects[other.ID].n.Stopped() {
		t.Fatal("project pause affected the wrong node")
	}
	if s, _, err := settings.Load(h.app.path); err != nil || len(s.Bindings) != 2 || !s.Bindings[h.app.bindingIndex(site.ID)].StopAgents {
		t.Fatalf("project pause not persisted: %v", err)
	}
	if code, raw := h.do(t, http.MethodPost, "/ui/api/settings", jsonOf(t, map[string]any{"node": "alice", "listen": "127.0.0.1:0", "handler": "none"}), h.tokenHdr()); code != http.StatusOK || strings.Contains(raw, `"error"`) {
		t.Fatalf("save settings while paused: %d %s", code, raw)
	}
	if !h.app.projects[site.ID].n.Stopped() || h.app.projects[other.ID].n.Stopped() {
		t.Fatal("project pause lost after context restart")
	}
	// Adding a seat while stopped and asking it must preserve the message.
	n := h.app.projects[site.ID].n
	seat, err := n.AddSeat(node.SeatRequest{Provider: node.ProviderCodex})
	if err != nil {
		t.Fatal(err)
	}
	chat, err := n.NewProjectChat(nil)
	if err != nil {
		t.Fatal(err)
	}
	message, err := n.SendRequest(node.SendRequest{ChatID: chat.ID, Body: "Please inspect", AuthorKind: node.AuthorHuman, AskSeats: []string{seat.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if got := n.Seats(); len(got) != 1 || len(got[0].Pending) != 1 || got[0].Pending[0].ID != message.ID {
		t.Fatalf("paused message not queued: %+v", got)
	}
	if got, err := n.ChatMessages(chat.ID, 0, 0, 10); err != nil || !slices.ContainsFunc(got, func(m node.ChatMessage) bool { return m.ID == message.ID }) {
		t.Fatalf("human chat message lost: %+v, %v", got, err)
	}
	var st Status
	if code, raw := h.api(t, http.MethodPost, "autonomy/stop", map[string]any{"on": true}, &st); code != http.StatusOK || !st.StopAll {
		t.Fatalf("global stop: %d %s", code, raw)
	}
	st = Status{}
	if code, raw := h.api(t, http.MethodPost, "autonomy/stop", map[string]any{"on": false}, &st); code != http.StatusOK || st.StopAll {
		t.Fatalf("global resume: %d %s", code, raw)
	}
	if !n.Stopped() || h.app.projects[other.ID].n.Stopped() || len(n.Seats()[0].Pending) != 1 {
		t.Fatal("global resume released the project pause")
	}
	if code, raw := h.api(t, http.MethodPost, path, map[string]any{"on": false}, &site); code != http.StatusOK || site.Autonomy == nil || site.Autonomy.Stopped {
		t.Fatalf("resume site: %d %s", code, raw)
	}
	deadline := time.Now().Add(6 * time.Second) // wakeLoop checks seats every two seconds.
	for len(n.Seats()[0].Pending) != 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := n.Seats(); len(got[0].Pending) != 0 {
		t.Fatalf("queued seat message was not delivered: %+v", got)
	}
	if n.Stopped() {
		t.Fatal("node stayed stopped after project resume")
	}
	h.wantError(t, http.MethodPost, "projects/legacy/autonomy/stop", map[string]any{"on": true}, http.StatusNotFound, "not_found")
}

// A budget pause tells the owner once (Notify), the view shows it, and
// autonomy/resume ends it.
func TestAutonomyPauseNotifiesAndResumes(t *testing.T) {
	type note struct{ title, text string }
	notes := make(chan note, 4)
	h := projectsHarness(t, "alice", "", func(a *App) { a.Notify = func(title, text string) { notes <- note{title, text} } })
	var site ProjectView
	if code, raw := h.api(t, http.MethodPost, "projects", map[string]any{"name": "Сайт", "dir": t.TempDir()}, &site); code != http.StatusOK {
		t.Fatalf("create: %d %s", code, raw)
	}
	if code, raw := h.api(t, http.MethodPost, "projects/"+site.ID+"/binding", map[string]any{"autonomy": "full", "turns_per_hour": 1}, &site); code != http.StatusOK {
		t.Fatalf("full: %d %s", code, raw)
	}
	n := h.app.projects[site.ID].n
	if got := n.Autonomy(); got.Mode != node.AutonomyFull || got.TurnsPerHour != 1 || got.MaxDepth != 0 {
		t.Fatalf("node autonomy %+v", got)
	}
	// The node's pause hook (node.TestAutonomyBudgets drives the budgets).
	h.app.autonomyPaused(site.ID, node.PauseTurns)
	select {
	case got := <-notes:
		if got.title != uiStrings["autonomy.paused.title"] || !strings.Contains(got.text, "«Сайт»") {
			t.Fatalf("notification %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no notification")
	}
	var v ProjectView
	if code, raw := h.api(t, http.MethodPost, "projects/"+site.ID+"/autonomy/resume", nil, &v); code != http.StatusOK || v.Autonomy.Paused || n.AutonomyStatus().Paused {
		t.Fatalf("resume: %d %s", code, raw)
	}
	select {
	case got := <-notes:
		t.Fatalf("told twice: %+v", got)
	default:
	}
	h.wantError(t, http.MethodPost, "projects/legacy/autonomy/resume", nil, http.StatusNotFound, "not_found")
}
