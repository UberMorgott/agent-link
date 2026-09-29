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

// ownerLiveLocked reports whether the owner o of local chat pid counts as
// live: its session is, and a subagent is among its live agents (or was never
// seen there, so its absence says nothing).
func (a *App) ownerLiveLocked(pid string, o settings.LocalChatOwner, live map[string]map[string]bool) bool {
	agents, ok := live[o.Session]
	return ok && (o.Agent == "" || agents[o.Agent] || !a.owners[pid].agent)
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
	o := a.s.Bindings[i].Chat.OwnerOf()
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
		o := b.Chat.OwnerOf()
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
		if o := a.s.Bindings[i].Chat.OwnerOf(); o != nil {
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
	if c != nil && hasUnread(c.n) {
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

// hasUnread reports an unread message in n's chats (an error counts as one:
// nothing is dropped unseen).
func hasUnread(n *node.Node) bool {
	chats, err := n.Chats(false, false)
	if err != nil {
		return true
	}
	for _, ci := range chats {
		if ci.Unread > 0 {
			return true
		}
	}
	return false
}

// localChatLiveLocked is LocalChatView.Live, Waiting and LastActive of the
// local chat of binding b (live: liveAgentsLocked). Live: not retired, and
// its owner (session, and subagent) is live, another session of it is live,
// a discuss caller waits for a reply (App.discussWaiters), a seat's turn runs
// or is queued, or a job runs. A shared chat (a folder's project chat, b.Chat
// nil, or a persistent topic) is live only while something runs in it or one
// of its sessions is live: an idle one is not in use, however long it is kept.
// An unread reply alone does not keep it live: it is for a person (the
// dashboard's needs_human), and a retired chat stays hidden only for it.
func (a *App) localChatLiveLocked(b settings.ProjectBinding, live map[string]map[string]bool) (isLive, waiting bool, last time.Time) {
	lc := b.Chat
	c := a.projects[b.ID]
	last, waiting, running := localChatState(c, lc)
	waiting = waiting || a.discussWaiters[b.ID] > 0
	if lc == nil {
		return waiting || running, waiting, last
	}
	if !lc.Retired.IsZero() {
		return false, waiting, last
	}
	o := lc.OwnerOf()
	if o != nil && a.ownerLiveLocked(b.ID, *o, live) {
		return true, waiting, last
	}
	for _, s := range lc.Sessions {
		if _, ok := live[s]; ok && (o == nil || s != o.Session) {
			return true, waiting, last
		}
	}
	return waiting || running, waiting, last
}
