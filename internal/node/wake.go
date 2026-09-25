package node

import (
	"context"
	"fmt"
	"time"
)

// Waking idle sessions. The node wakes a live session that ended its turn
// (Idle) and has actionable unread messages to take, once per idle period:
//
//   - Codex (WakeQueue): it queues one short prompt in the session's agent
//     (`codex queue`, SessionWaker); the agent starts a turn with it. A session
//     whose app is closed keeps the prompt queued until it is opened again.
//     Without a usable waker (no codex 0.149 or later, or a failed wake) such
//     a session gets WakeNextEvent and reads at its next event.
//   - Claude Code: it posts the prompt to the session's inbox (inbox.go,
//     InboxPoster). A failed post drops the inbox, and the session's
//     background waiter (asyncRewake, which defers to the node while it holds
//     the inbox) wakes it instead.
//
// Either way the session's own hooks (UserPromptSubmit) claim, deliver and
// acknowledge the messages as always, and each woken message's author hears
// of it (AttemptWakeRequested, then AttemptWokenConfirmed at the ack).
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
		if !s.Idle || s.Woken || !s.live(now) {
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
		if err != nil || page.Total == 0 {
			continue
		}
		byInbox := d.inbox.socket != ""
		if byInbox {
			err = n.poster.Post(ctx, d.inbox.socket, d.inbox.token, WakeText(page.Total))
		} else {
			err = n.waker.Wake(ctx, s.CodexHome, s.SessionID, WakeText(page.Total))
		}
		r.mu.Lock()
		if cur := r.sessions[s.SessionID]; cur != nil {
			switch {
			case err == nil:
				cur.Woken = true
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
		n.log.Info("idle session woken", "session", s.SessionID, "provider", s.Provider, "inbox", byInbox, "unread", page.Total)
		n.noteWoken(page.Messages)
	}
}

// hookBatchIDs bounds the messages one wake reports attempts for.
const hookBatchIDs = 50

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
