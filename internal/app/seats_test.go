package app

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/UberMorgott/agent-link/internal/node"
)

// seatRunner is a DirectLauncher whose turns succeed at once, naming a new
// session "<provider>-1" and keeping a resumed one.
type seatRunner struct {
	mu    sync.Mutex
	specs []node.LaunchSpec
}

func (*seatRunner) Launch(context.Context, node.LaunchSpec) error { return nil }
func (*seatRunner) Direct(string) bool                            { return true }
func (r *seatRunner) Run(_ context.Context, spec node.LaunchSpec, started func(string)) error {
	r.mu.Lock()
	r.specs = append(r.specs, spec)
	r.mu.Unlock()
	started(cmpOr(spec.ResumeID, spec.Provider+"-1"))
	return nil
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// The seats API: add Claude and Codex to a project, ask Codex from the
// composer, stop, start and remove; errors are coded.
func TestProjectSeatsAPI(t *testing.T) {
	runner := &seatRunner{}
	h := projectsHarness(t, "alice", "", func(a *App) { a.Launcher = runner })
	var site ProjectView
	if code, raw := h.api(t, http.MethodPost, "projects", map[string]any{"name": "Сайт", "dir": t.TempDir()}, &site); code != http.StatusOK {
		t.Fatalf("create: %d %s", code, raw)
	}
	base := "projects/" + site.ID + "/seats"
	var claude, codex node.SeatView
	if code, raw := h.api(t, http.MethodPost, base, map[string]any{"provider": "claude"}, &claude); code != http.StatusOK || claude.Label != "Claude" {
		t.Fatalf("add claude: %d %s", code, raw)
	}
	if code, raw := h.api(t, http.MethodPost, base, map[string]any{"provider": "codex"}, &codex); code != http.StatusOK || codex.Label != "Codex" {
		t.Fatalf("add codex: %d %s", code, raw)
	}
	h.wantError(t, http.MethodPost, base, map[string]any{"provider": "gpt"}, http.StatusBadRequest, "bad_request")
	h.wantError(t, http.MethodPost, base+"/seat-none/stop", nil, http.StatusNotFound, "not_found")
	var seats []node.SeatView
	deadline := time.Now().Add(10 * time.Second)
	for {
		h.api(t, http.MethodGet, base, nil, &seats)
		if len(seats) == 2 && seats[0].SessionID == "claude-1" && seats[1].SessionID == "codex-1" && seats[1].Status == node.SeatOffline {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("seats %+v", seats)
		}
		time.Sleep(20 * time.Millisecond)
	}
	var chat ChatInfoView
	if code, raw := h.api(t, http.MethodPost, "projects/"+site.ID+"/chats", map[string]any{"participants": []string{}}, &chat); code != http.StatusOK {
		t.Fatalf("chat: %d %s", code, raw)
	}
	var m node.Message
	if code, raw := h.api(t, http.MethodPost, "projects/"+site.ID+"/send", map[string]any{"chat_id": chat.ID, "body": "hi", "ask_seats": []string{codex.ID}}, &m); code != http.StatusOK ||
		len(m.AskSeats) != 1 || m.AskSeats[0] != codex.ID {
		t.Fatalf("send: %d %s", code, raw)
	}
	var v node.SeatView
	if code, raw := h.api(t, http.MethodPost, base+"/"+codex.ID+"/stop", nil, &v); code != http.StatusOK || v.Status != node.SeatStopped {
		t.Fatalf("stop: %d %s", code, raw)
	}
	if code, raw := h.api(t, http.MethodPost, base+"/"+codex.ID+"/start", map[string]any{}, &v); code != http.StatusOK || v.Status == node.SeatStopped {
		t.Fatalf("start: %d %s", code, raw)
	}
	if code, raw := h.api(t, http.MethodPost, base+"/"+claude.ID+"/remove", nil, &seats); code != http.StatusOK || len(seats) != 1 || strings.Contains(raw, claude.ID) {
		t.Fatalf("remove: %d %s", code, raw)
	}
}
