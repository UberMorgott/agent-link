package app

import (
	"context"
	"errors"
	"time"

	"github.com/UberMorgott/agent-link/internal/node"
)

// The sweep of orphan Codex threads. A seat's Codex thread is archived when
// its seat leaves (node.RemoveSeat, node.Leave); threads of seats closed
// before that, or left behind when a seat's thread could not be resumed and a
// new one took its place, stay in the Codex app's sidebar. A while after
// start and then every threadSweepEvery, the app archives the threads
// agent-link opened for seats (node.SeatIntroPrefix) that no seat of any of
// its running contexts resumes and that were idle for threadSweepIdle. It
// runs only while every binding's context runs (a seat of one that does not
// would look orphaned); a thread archived anyway is unarchived when its seat
// resumes it (node.CodexTurn).

const (
	threadSweepDelay = 2 * time.Minute
	threadSweepEvery = 6 * time.Hour
	threadSweepIdle  = time.Hour
)

// orphanArchiver archives the seats' threads no seat keeps
// (node.DesktopLauncher).
type orphanArchiver interface {
	ArchiveOrphanSessions(ctx context.Context, keep func(id string) bool, before time.Time) ([]string, error)
}

// threadSweepLoop sweeps orphan seat threads threadSweepDelay after start and
// every threadSweepEvery, until ctx ends.
func (a *App) threadSweepLoop(ctx context.Context) {
	if _, ok := a.Launcher.(orphanArchiver); !ok {
		return
	}
	t := time.NewTimer(threadSweepDelay)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		a.sweepSeatThreads(ctx, time.Now())
		t.Reset(threadSweepEvery)
	}
}

// sweepSeatThreads archives the orphan seat threads idle since before now
// minus threadSweepIdle; it returns the archived ones.
func (a *App) sweepSeatThreads(ctx context.Context, now time.Time) []string {
	oa, ok := a.Launcher.(orphanArchiver)
	if !ok {
		return nil
	}
	keep, ok := a.seatSessions()
	if !ok {
		a.log.Info("orphan seat threads: not swept while a context does not run")
		return nil
	}
	archived, err := oa.ArchiveOrphanSessions(ctx, func(id string) bool { return keep[id] }, now.Add(-threadSweepIdle))
	switch {
	case errors.Is(err, node.ErrNoAgent):
	case err != nil:
		a.log.Warn("archive orphan seat threads", "archived", len(archived), "err", err)
	case len(archived) > 0:
		a.log.Info("archived orphan seat threads", "threads", len(archived))
	}
	return archived
}

// seatSessions are the sessions of the seats of every running context; false
// unless the app is configured and every binding's context runs.
func (a *App) seatSessions() (map[string]bool, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.configured {
		return nil, false
	}
	for _, b := range a.s.Bindings {
		if a.projects[b.ID] == nil {
			return nil, false
		}
	}
	keep := map[string]bool{}
	add := func(c *appContext) {
		for _, id := range c.n.SeatSessions() {
			keep[id] = true
		}
	}
	if a.legacy != nil {
		add(a.legacy)
	}
	for _, c := range a.projects {
		add(c)
	}
	return keep, true
}
