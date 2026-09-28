package app

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/UberMorgott/agent-link/internal/node"
)

// A local project without autonomy of its own follows the network project
// bound to its folder (read through: later changes there apply too); its own
// setting wins; with no network project there it keeps the defaults.
func TestLocalProjectInheritsNetworkAutonomy(t *testing.T) {
	h := projectsHarness(t, "alice", "")
	dir := t.TempDir()
	var network ProjectView
	if code, raw := h.api(t, http.MethodPost, "projects", map[string]any{"name": "Shared", "dir": dir}, &network); code != http.StatusOK {
		t.Fatalf("create: %d %s", code, raw)
	}
	if code, raw := h.api(t, http.MethodPost, "projects/"+network.ID+"/binding", map[string]any{"autonomy": "full", "max_auto_depth": 0}, &network); code != http.StatusOK {
		t.Fatalf("full: %d %s", code, raw)
	}
	local := localProjectForTest(t, h, "Local", dir)
	alone := localProjectForTest(t, h, "Alone", t.TempDir())
	ln := h.app.projects[local.ID].n
	if got := ln.Autonomy(); got.Mode != node.AutonomyFull || got.MaxDepth != 0 {
		t.Fatalf("local autonomy %+v", got)
	}
	if local.Autonomy == nil || local.Autonomy.Mode != node.AutonomyFull || local.Autonomy.MaxAutoDepth != 0 {
		t.Fatalf("local view %+v", local.Autonomy)
	}
	if got := h.app.projects[alone.ID].n.Autonomy(); got.Mode != node.AutonomyOff || got.MaxDepth != node.MaxAutoDepth {
		t.Fatalf("unrelated local autonomy %+v", got)
	}
	// The owner changes the network project: the local one follows.
	if code, raw := h.api(t, http.MethodPost, "projects/"+network.ID+"/binding", map[string]any{"autonomy": "asked", "max_auto_depth": 5}, &network); code != http.StatusOK {
		t.Fatalf("asked: %d %s", code, raw)
	}
	if got := ln.Autonomy(); got.Mode != node.AutonomyAsked || got.MaxDepth != 5 {
		t.Fatalf("local after the change %+v", got)
	}
	// Its own setting wins over the network project's.
	if code, raw := h.api(t, http.MethodPost, "projects/"+local.ID+"/binding", map[string]any{"max_auto_depth": 3}, &local); code != http.StatusOK {
		t.Fatalf("own: %d %s", code, raw)
	}
	if code, raw := h.api(t, http.MethodPost, "projects/"+network.ID+"/binding", map[string]any{"max_auto_depth": 7}, &network); code != http.StatusOK {
		t.Fatalf("network again: %d %s", code, raw)
	}
	if got := ln.Autonomy(); got.Mode != node.AutonomyAsked || got.MaxDepth != 3 {
		t.Fatalf("local with its own %+v", got)
	}
	// The global stop still halts it.
	if err := h.app.SetStopAll(true); err != nil {
		t.Fatal(err)
	}
	if !ln.Stopped() {
		t.Fatal("the global stop does not halt the inheriting project")
	}
}

type discussHeldAPI struct {
	discussResultAPI
	HoldReason string `json:"hold_reason"`
}

// A discuss ask past the hop limit returns held at once, and the reply wait
// does not sit out its timeout.
func TestDiscussReturnsHeldPastHopLimit(t *testing.T) {
	h := projectsHarness(t, "alice", "", func(a *App) { a.Launcher = &seatRunner{} })
	dir := repoDir(t) // a project folder: outside one each discuss gets a temporary chat
	post := func(fields map[string]string) discussHeldAPI {
		t.Helper()
		fields["folder"] = dir
		code, raw := h.do(t, http.MethodPost, "/discuss", jsonOf(t, fields), nil)
		if code != http.StatusOK {
			t.Fatalf("discuss: %d %s", code, raw)
		}
		var got discussHeldAPI
		if err := json.Unmarshal([]byte(raw), &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	codex := post(map[string]string{"provider": node.ProviderCodex, "body": "setup"})
	claude := post(map[string]string{"provider": node.ProviderClaude, "body": "setup"})
	if codex.HoldReason != "" || claude.HoldReason != "" {
		t.Fatalf("a person's ask held: %+v %+v", codex, claude)
	}
	n := h.app.projects[codex.Project].n
	eventuallyApp(t, "seat setup", func() bool {
		for _, s := range n.Seats() {
			if s.SessionID == "" || s.LastTurn.IsZero() || s.Status == node.SeatRunning {
				return false
			}
		}
		return true
	})
	if code, raw := h.api(t, http.MethodPost, "projects/"+codex.Project+"/binding", map[string]any{"autonomy": "asked", "max_auto_depth": 1}, nil); code != http.StatusOK {
		t.Fatalf("limit: %d %s", code, raw)
	}
	// Agents asking each other: past depth 1 the ask is held.
	var held discussHeldAPI
	from, to := claude, codex
	provider := map[string]string{codex.Seat: node.ProviderCodex, claude.Seat: node.ProviderClaude}
	for range 4 {
		got := post(map[string]string{"provider": provider[to.Seat], "source": provider[from.Seat],
			"session_id": provider[from.Seat] + "-1", "seat": from.Seat, "body": "hop"})
		if got.HoldReason != "" {
			held = got
			break
		}
		from, to = to, from
	}
	if held.HoldReason != node.HoldAutoLimit || held.Queued {
		t.Fatalf("no held discuss result: %+v", held)
	}
	path := "/discuss/reply?" + url.Values{"project": {held.Project}, "chat": {held.Chat},
		"id": {held.ID}, "seat": {held.Seat}, "timeout": {"10s"}}.Encode()
	start := time.Now()
	if code, body := h.do(t, http.MethodGet, path, "", nil); code != http.StatusAccepted || body != "" {
		t.Fatalf("held reply wait: %d %s", code, body)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("the held reply wait took %s", d)
	}
}
