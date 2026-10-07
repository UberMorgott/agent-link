package app

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/UberMorgott/agent-link/internal/node"
)

// sweepRunner is a seatRunner that sweeps orphan threads: it reports which
// of candidates keep keeps, archiving the rest.
type sweepRunner struct {
	seatRunner
	candidates []string
	before     time.Time
	calls      int
}

func (r *sweepRunner) ArchiveOrphanSessions(_ context.Context, keep func(string) bool, before time.Time) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.before = before
	var out []string
	for _, id := range r.candidates {
		if !keep(id) {
			out = append(out, id)
		}
	}
	return out, nil
}

// The sweep keeps every live seat's thread and archives the rest of the
// seat threads idle for threadSweepIdle; it waits while a binding's context
// does not run.
func TestSweepSeatThreadsKeepsLiveSeats(t *testing.T) {
	runner := &sweepRunner{candidates: []string{"codex-1", "orphan-1"}}
	h := projectsHarness(t, "alice", "", func(a *App) { a.Launcher = runner })
	site := localProjectForTest(t, h, "Сайт", t.TempDir())
	var codex node.SeatView
	if code, raw := h.api(t, http.MethodPost, "projects/"+site.ID+"/seats", map[string]any{"provider": "codex"}, &codex); code != http.StatusOK {
		t.Fatalf("add codex: %d %s", code, raw)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if keep, ok := h.app.seatSessions(); ok && keep["codex-1"] {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the seat's session is not kept")
		}
		time.Sleep(20 * time.Millisecond)
	}
	now := time.Now()
	if got := h.app.sweepSeatThreads(t.Context(), now); len(got) != 1 || got[0] != "orphan-1" {
		t.Fatalf("archived %v", got)
	}
	runner.mu.Lock()
	before := runner.before
	runner.mu.Unlock()
	if !before.Equal(now.Add(-threadSweepIdle)) {
		t.Fatalf("before %v", before)
	}
	// A binding whose context does not run: no sweep.
	h.app.mu.Lock()
	c := h.app.projects[site.ID]
	delete(h.app.projects, site.ID)
	h.app.mu.Unlock()
	got := h.app.sweepSeatThreads(t.Context(), now)
	runner.mu.Lock()
	calls := runner.calls
	runner.mu.Unlock()
	if got != nil || calls != 1 {
		t.Fatalf("swept without a context: %v (%d calls)", got, calls)
	}
	h.app.mu.Lock()
	h.app.projects[site.ID] = c
	h.app.mu.Unlock()
}
