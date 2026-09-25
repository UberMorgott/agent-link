package node

import (
	"context"
	"fmt"
	"time"
)

// Waking idle sessions (WakeQueue). A Codex session has no background hook
// that could wake it (unlike Claude Code's asyncRewake waiter), so the node
// does it: when a live WakeQueue session is idle (its last hook event was a
// Stop that let it stop) and actionable unread messages it may take wait, the
// node queues one short prompt for it in its agent, once per idle period. The
// agent starts a turn with it, and the session's own hooks (UserPromptSubmit)
// claim, deliver and acknowledge the messages as always. A session whose app
// is closed keeps the prompt queued until it is opened again; the node never
// resumes one itself. Without a usable waker (no codex 0.149 or later, or a
// failed wake) such a session gets WakeNextEvent and reads at its next event.

// wakePoll is how often the node looks for idle sessions to wake.
const wakePoll = 2 * time.Second

// SessionWaker wakes an idle agent session (codexqueue.Queue).
type SessionWaker interface {
	// Check looks for the agent's CLI (when due, or always when force); it may
	// run programs.
	Check(ctx context.Context, force bool)
	// Ready reports whether Wake can be tried; it runs nothing.
	Ready() bool
	// Wake queues text as the next prompt of session in the agent home home.
	Wake(ctx context.Context, home, session, text string) error
}

// SetSessionWaker sets the waker of WakeQueue sessions. It must be set before
// Serve or Run; without one such sessions get WakeNextEvent.
func (n *Node) SetSessionWaker(w SessionWaker) { n.waker = w }

// canQueue reports whether a WakeQueue session can be woken now.
func (n *Node) canQueue() bool { return n.waker != nil && n.waker.Ready() }

// wakeLoop wakes idle WakeQueue sessions until ctx is done.
func (n *Node) wakeLoop(ctx context.Context) {
	every := n.wakeEvery
	if every <= 0 {
		every = wakePoll
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		n.waker.Check(ctx, false)
		n.syncQueueWake()
		n.wakeIdle(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// syncQueueWake gives the sessions that asked for WakeQueue that mode while
// the waker is ready and WakeNextEvent while it is not.
func (n *Node) syncQueueWake() {
	want := WakeNextEvent
	if n.canQueue() {
		want = WakeQueue
	}
	r := n.sess
	now := time.Now()
	changed := false
	r.mu.Lock()
	for _, s := range r.sessions {
		if s.Asked == WakeQueue && s.Wake != want && s.live(now) {
			s.Wake, changed = want, true
		}
	}
	if changed {
		_ = r.saveLocked(now)
	}
	r.mu.Unlock()
	if changed {
		n.presenceChanged()
		n.changed("sessions")
	}
}

// wakeIdle queues a wake for every idle WakeQueue session not woken in this
// idle period that has unread messages to take. A failed wake leaves the
// session to its next event (WakeNextEvent) and is logged.
func (n *Node) wakeIdle(ctx context.Context) {
	if !n.canQueue() {
		return
	}
	r := n.sess
	now := time.Now()
	var due []Session
	r.mu.Lock()
	for _, s := range r.sessions {
		if s.Wake == WakeQueue && s.Idle && !s.Woken && s.live(now) {
			due = append(due, *s)
		}
	}
	r.mu.Unlock()
	for _, s := range due {
		page, err := n.unreadFor(s.Folder, s.SessionID, "", 1, true)
		if err != nil || page.Total == 0 {
			continue
		}
		err = n.waker.Wake(ctx, s.CodexHome, s.SessionID, WakeText(page.Total))
		r.mu.Lock()
		cur := r.sessions[s.SessionID]
		if cur != nil {
			if err == nil {
				cur.Woken = true
			} else {
				cur.Wake = WakeNextEvent
			}
			_ = r.saveLocked(time.Now())
		}
		r.mu.Unlock()
		if err != nil {
			n.log.Warn("waking an idle session failed; it reads its messages at its next event", "session", s.SessionID, "provider", s.Provider, "err", err)
			n.presenceChanged()
			n.changed("sessions")
			return
		}
		n.log.Info("idle session woken", "session", s.SessionID, "provider", s.Provider, "unread", page.Total)
	}
}

// WakeText is the prompt queued for an idle session with n unread messages:
// «agent-link: 3 новых сообщения — прочитай их».
func WakeText(n int) string {
	form := "новых сообщений"
	switch n10, n100 := n%10, n%100; {
	case n10 == 1 && n100 != 11:
		form = "новое сообщение"
	case n10 >= 2 && n10 <= 4 && (n100 < 12 || n100 > 14):
		form = "новых сообщения"
	}
	return fmt.Sprintf("agent-link: %d %s — прочитай их", n, form)
}
