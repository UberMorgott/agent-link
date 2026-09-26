package app

import (
	"net/http"
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
