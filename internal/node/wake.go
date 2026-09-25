package node

import (
	"context"
	"time"
)

// Waking idle sessions. The node wakes a live session that ended its turn
// (Idle) and has actionable unread messages to take, once per idle period
// plus one retry of a wake it never took (maxIdleWakes):
//
//   - Codex (WakeQueue): it queues one prompt in the session's agent
//     (`codex queue`, SessionWaker); the agent starts a turn with it. A session
//     whose app is closed keeps the prompt queued until it is opened again.
//     Without a usable waker (no codex 0.149 or later, or a failed wake) such
//     a session gets WakeNextEvent and reads at its next event.
//   - Claude Code: it posts the prompt to the session's inbox (inbox.go,
//     InboxPoster). A failed post drops the inbox, and the session's
//     background waiter (asyncRewake, which defers to the node while it holds
//     the inbox) wakes it instead.
//
// The prompt is the messages themselves (WakePrompt, as the hooks format
// them), as many as fit FormatBudget. The node claims them for the session as
// a wake claim first (wakeClaim): its hooks then do not deliver them again but
// acknowledge them at that prompt (UnreadPage.Woken); a failed wake drops the
// claim, and one the session never took lapses after inboxWakeGrace, and the
// hooks deliver them as always. Each woken message's author hears of it
// (AttemptWakeRequested, then AttemptWokenConfirmed at the ack).
// The same loop runs the launch ladder (launch.go).

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

// wakeLoop wakes idle sessions and runs the launch ladder until ctx is done.
func (n *Node) wakeLoop(ctx context.Context) {
	every := n.wakeEvery
	if every <= 0 {
		every = wakePoll
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if n.waker != nil {
			n.waker.Check(ctx, false)
			n.syncQueueWake()
		}
		n.wakeIdle(ctx)
		n.launchDue(ctx, time.Now())
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

// wakeIdle wakes every idle session not woken in this idle period that has
// actionable unread messages to take: WakeQueue ones through the waker,
// Claude ones through their inbox. What it takes is re-read right before each
// wake.
func (n *Node) wakeIdle(ctx context.Context) {
	queue := n.canQueue()
	if !queue && n.poster == nil {
		return
	}
	r := n.sess
	now := time.Now()
	type due struct {
		s     Session
		inbox inboxAddr
	}
	var list []due
	r.mu.Lock()
	for _, s := range r.sessions {
		if !s.Idle || !s.live(now) || !s.wakeDue(now) {
			continue
		}
		if a, ok := r.inbox[s.SessionID]; ok && n.poster != nil {
			list = append(list, due{s: *s, inbox: a})
		} else if s.Wake == WakeQueue && queue {
			list = append(list, due{s: *s})
		}
	}
	r.mu.Unlock()
	for _, d := range list {
		s := d.s
		page, err := n.unreadFor(s.Folder, s.SessionID, "", hookBatchIDs, true)
		if err != nil || len(page.Messages) == 0 {
			continue
		}
		msgs, token := n.wakeClaim(s.SessionID, page.Messages[:FitUnread(page.Messages)])
		if len(msgs) == 0 {
			continue
		}
		text := WakePrompt(msgs, page.Total-len(msgs), s.Folder, token)
		byInbox := d.inbox.socket != ""
		if byInbox {
			err = n.poster.Post(ctx, d.inbox.socket, d.inbox.token, text)
		} else {
			err = n.waker.Wake(ctx, s.CodexHome, s.SessionID, text)
		}
		if err != nil {
			n.unclaim(s.SessionID, ids(msgs)) // its hooks deliver them instead
		}
		r.mu.Lock()
		if cur := r.sessions[s.SessionID]; cur != nil {
			switch {
			case err == nil:
				cur.Woken, cur.WokeAt = true, time.Now()
				cur.Wakes++
				if a, ok := r.inbox[s.SessionID]; ok && byInbox {
					a.woke = time.Now()
					r.inbox[s.SessionID] = a
				}
			case byInbox:
				delete(r.inbox, s.SessionID) // its waiter wakes it from now on
			default:
				cur.Wake = WakeNextEvent
			}
			_ = r.saveLocked(time.Now())
		}
		r.mu.Unlock()
		if err != nil {
			n.log.Warn("waking an idle session failed; it reads its messages at its next event", "session", s.SessionID, "provider", s.Provider, "inbox", byInbox, "err", err)
			n.presenceChanged()
			n.changed("sessions")
			continue
		}
		n.log.Info("idle session woken", "session", s.SessionID, "provider", s.Provider, "inbox", byInbox, "messages", len(msgs), "unread", page.Total)
		n.noteWoken(msgs)
	}
}

// maxIdleWakes bounds the node's wakes of one idle period: the first, and one
// retry after it lapsed untaken (inboxWakeGrace; its claim lapses with it and
// the messages are unread again). A lapsed wake also frees the session's
// waiter (Claude, InboxWakes); a message goes to one of them only (claims).
// After that the waiter or the session's next event takes them.
const maxIdleWakes = 2

// wakeDue reports whether the node may wake the idle session now: not woken
// in this idle period yet, or its last wake lapsed untaken (a session that
// took it left the idle period) and a retry is left.
func (s Session) wakeDue(now time.Time) bool {
	if !s.Woken {
		return true
	}
	return s.Wakes < maxIdleWakes && now.Sub(s.WokeAt) >= inboxWakeGrace
}

// hookBatchIDs bounds the unread messages one wake looks at.
const hookBatchIDs = 50
