package app

import (
	"errors"
	"time"

	"github.com/UberMorgott/agent-link/internal/node"
	"github.com/UberMorgott/agent-link/internal/settings"
)

// Retirement of owned local chats (settings.LocalChat.Owner): when the
// owner's session ends (SessionEnd, or its TTL lapsed) or, for a subagent,
// the subagent leaves its session's live agents (SubagentStop, or
// agentLiveTTL lapsed), the chat's seats are removed after RetireGrace, which
// ends a running turn and its agent process; then the chat leaves (its data
// to .left, as the GC's), unless an unread reply holds it: it stays hidden
// (Retired) until that is read. A session back within the grace (a resumed
// Claude session keeps its id) keeps its chats.
//
// Liveness is watched, not trusted blindly: a chat retires only after its
// owner was seen live in this run of the app (a session outside every
// project folder, registered nowhere, is never taken for gone), and a
// subagent only after it was seen among its session's live agents. The rest
// is left to the idle GC (gcLocalChats).

const (
	// RetireGrace is how long an owner stays gone before its chats retire.
	RetireGrace = 90 * time.Second
	// retireEvery is how often gcLoop checks the owners.
	retireEvery = 15 * time.Second
	// retireTurnWait bounds the wait for a cancelled turn to exit; a turn
	// still running is tried again at the next check.
	retireTurnWait = 10 * time.Second
)

// ownerWatch is what the app saw of a chat's owner (App.owners).
type ownerWatch struct {
	session bool      // its session was seen live
	agent   bool      // it was seen live itself (a subagent: among its session's agents)
	gone    time.Time // when it was first seen gone after that; zero while live
}

// liveAgentsLocked maps each live session of every context to its live
// subagents, now (the sessions' own clock, whatever time a check is for).
func (a *App) liveAgentsLocked() map[string]map[string]bool {
	now := time.Now()
	live := map[string]map[string]bool{}
	add := func(n *node.Node) {
		for _, s := range n.Sessions() {
			m := live[s.SessionID]
			if m == nil {
				m = map[string]bool{}
				live[s.SessionID] = m
			}
			for _, ag := range s.LiveAgents(now) {
				m[ag] = true
			}
		}
	}
	if a.legacy != nil {
		add(a.legacy.n)
	}
	for _, c := range a.projects {
		add(c.n)
	}
	return live
}

// observeOwnerLocked records what live says of the owner o of local chat
// pid and reports whether it has been gone for RetireGrace.
func (a *App) observeOwnerLocked(pid string, o settings.LocalChatOwner, live map[string]map[string]bool, now time.Time) bool {
	if a.owners == nil {
		a.owners = map[string]ownerWatch{}
	}
	w := a.owners[pid]
	agents, sessionLive := live[o.Session]
	selfLive := sessionLive && (o.Agent == "" || agents[o.Agent])
	w.session = w.session || sessionLive
	w.agent = w.agent || selfLive
	gone := (!sessionLive && w.session) || (sessionLive && !selfLive && w.agent)
	switch {
	case !gone:
		w.gone = time.Time{}
	case w.gone.IsZero():
		w.gone = now
	}
	a.owners[pid] = w
	return gone && now.Sub(w.gone) >= RetireGrace
}

func (a *App) forgetOwnerLocked(pid string) { delete(a.owners, pid) }

// ownerGoneLocked reports whether the owner of owned local chat pid is gone
// now (strictly: its session live in no context, or a subagent seen live and
// no longer among its session's agents).
func (a *App) ownerGoneLocked(pid string) bool {
	i := a.bindingIndex(pid)
	if i < 0 {
		return false
	}
	o := chatOwner(a.s.Bindings[i].Chat)
	if o == nil {
		return false
	}
	agents, ok := a.liveAgentsLocked()[o.Session]
	return !ok || (o.Agent != "" && !agents[o.Agent] && a.owners[pid].agent)
}

// retireOwnedChats retires every owned local chat whose owner has been gone
// for RetireGrace (gcLoop, every retireEvery).
func (a *App) retireOwnedChats(now time.Time) {
	a.mu.Lock()
	live := a.liveAgentsLocked()
	var due []string
	for _, b := range a.s.Bindings {
		o := chatOwner(b.Chat)
		if o == nil || !b.Chat.Retired.IsZero() {
			continue
		}
		if a.observeOwnerLocked(b.ID, *o, live, now) {
			due = append(due, b.ID)
		}
	}
	a.mu.Unlock()
	for _, pid := range due {
		a.retireChat(pid)
	}
}

// retireChat closes owned local chat pid: its seats go (their running turns
// are cancelled and waited for, retireTurnWait), then the chat leaves, or,
// with an unread reply, stays Retired until it is read (gcLocalChats).
func (a *App) retireChat(pid string) {
	// Checked again under a.mu, and marked retiring: discuss routes no call
	// to it (a new own chat takes one) while its seats go.
	a.mu.Lock()
	c := a.projects[pid]
	var owner string
	if i := a.bindingIndex(pid); i >= 0 {
		if o := chatOwner(a.s.Bindings[i].Chat); o != nil {
			owner = o.Session
		}
	}
	a.mu.Unlock()
	// The owner's registration (POST /sessions) waits while its seats go: it
	// cannot come back between the check and the removal.
	mu := a.ownerLock(owner)
	mu.Lock()
	a.mu.Lock()
	if owner == "" || !a.ownerGoneLocked(pid) {
		a.mu.Unlock()
		mu.Unlock()
		return
	}
	if a.retiring == nil {
		a.retiring = map[string]bool{}
	}
	a.retiring[pid] = true
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.retiring, pid)
		a.mu.Unlock()
	}()
	if c != nil {
		for _, s := range c.n.Seats() {
			if err := c.n.RemoveSeat(s.ID); err != nil && !errors.Is(err, node.ErrUnknownSeat) {
				a.log.Warn("retire chat: remove seat", "project", pid, "seat", s.ID, "err", err)
			}
		}
	}
	mu.Unlock()
	if c != nil {
		if !c.n.WaitSeatTurns(retireTurnWait) {
			a.log.Warn("retire chat: a turn is still running; tried again later", "project", pid)
			return
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	i := a.bindingIndex(pid)
	if i < 0 || a.s.Bindings[i].Chat == nil || !a.s.Bindings[i].Chat.Retired.IsZero() {
		return
	}
	if !a.ownerGoneLocked(pid) {
		// Back meanwhile (a resumed session): the chat stays; its next ask
		// adds a seat again.
		a.log.Info("retire chat: its owner is back; kept", "project", pid)
		return
	}
	defer func() { go a.events.publish("projects", projectTopic(pid), "status", "dashboard") }()
	if c != nil && localChatState(c, nil).unread {
		lc := *a.s.Bindings[i].Chat
		lc.Retired = time.Now().UTC()
		s := a.s
		s.Bindings = append([]settings.ProjectBinding(nil), s.Bindings...)
		s.Bindings[i].Chat = &lc
		if err := settings.Save(a.path, s); err != nil {
			a.log.Warn("retire chat", "project", pid, "err", err)
			return
		}
		a.s = s
		a.forgetOwnerLocked(pid)
		a.log.Info("owned chat retired; kept hidden for an unread reply", "project", pid)
		return
	}
	if err := a.leaveProjectLocked(pid); err != nil {
		a.log.Warn("retire chat", "project", pid, "err", err)
		return
	}
	a.forgetOwnerLocked(pid)
	a.log.Info("owned chat retired", "project", pid)
}

// liveState is a local chat's LocalChatView.Live, LiveEndedAt, Waiting and
// LastActive (localChatLiveLocked).
type liveState struct {
	live, waiting bool
	endedAt, last time.Time
}

// localChatLiveLocked is the live state of the local chat of binding b
// (b.Chat nil: a folder's project chat). Live: not retired, and a discuss
// caller waits for a reply (App.discussWaiters), a seat's turn runs or is
// queued, or other work runs in it. Its owner's session being open does not
// make it live (that only keeps it from retiring), nor does an unread reply:
// it is for a person (the dashboard's needs_human), and a retired chat stays
// hidden only for it. Not live, endedAt is when it was last seen to stop
// (checkLive), else its last activity.
func (a *App) localChatLiveLocked(b settings.ProjectBinding) liveState {
	st := localChatState(a.projects[b.ID], b.Chat)
	v := liveState{waiting: st.waiting || a.discussWaiters[b.ID] > 0, last: st.last}
	v.live = (v.waiting || st.running) && (b.Chat == nil || b.Chat.Retired.IsZero())
	if !v.live {
		v.endedAt = v.last
		if m, ok := a.chatLive[b.ID]; ok && !m.live && !m.endedAt.IsZero() {
			v.endedAt = m.endedAt
		}
	}
	return v
}
