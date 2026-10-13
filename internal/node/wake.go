package node

import (
	"context"
	"slices"
	"time"
)

// Waking idle sessions. The node wakes a live session that ended its turn
// (Idle) and has actionable unread messages to take, once per idle period
// plus one retry of a wake it never took (maxIdleWakes). What its own person
// wrote to the others (OwnHuman) is no reason for a wake. A Codex thread gets
// no message twice (its queue keeps what it accepted), and the node does not
// depend on its hooks: the prompt in the thread's rollout acknowledges the
// wake's messages and ends the wake count (observeQueueStart), activity it
// reports ends it too (SessionActivity), and a wake that stays unproven lets
// newer messages wake it later, bounded (maxStaleWakes, then needs_human).
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
//
// One recipient per message, by priority: a session in a turn (not Idle)
// gets it through its hooks, so no idle session of its area is woken for a
// message for no session in particular; a message routed to an idle session
// (its claim, assignment or chat affinity, routeOf; affinity never names an
// idle session while one of its area is in a turn) wakes that one; else the
// area's wakeable idle session active last (LastActive) is woken, and only it.
// The same loop runs the launch ladder (launch.go).

// wakePoll is how often the node looks for idle sessions to wake.
const wakePoll = 2 * time.Second

// wakeDormant is how often a dormant local chat's node runs the same loop:
// one with no live session, no seat with a turn or pending messages, no open
// lease, no launch in progress and no message a launch is for (dormant). Any
// change of the node (Node.changed) runs the loop at once and it polls every
// wakePoll again until it is dormant once more.
const wakeDormant = 60 * time.Second

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

// ImageWaker is a SessionWaker that can also hand the agent image files
// with the prompt (`codex queue --image`).
type ImageWaker interface {
	WakeImages(ctx context.Context, home, session, text string, images []string) error
}

// SetSessionWaker sets the waker of WakeQueue sessions. It must be set before
// Serve or Run; without one such sessions get WakeNextEvent.
func (n *Node) SetSessionWaker(w SessionWaker) { n.waker = w }

// canQueue reports whether a WakeQueue session can be woken now.
func (n *Node) canQueue() bool { return n.waker != nil && n.waker.Ready() }

// wakeLoop wakes idle sessions and runs the launch ladder until ctx is done.
func (n *Node) wakeLoop(ctx context.Context) {
	every, dormantEvery := n.wakeEvery, n.dormantEvery
	if every <= 0 {
		every = wakePoll
	}
	if dormantEvery <= 0 {
		dormantEvery = wakeDormant
	}
	t := time.NewTimer(every)
	defer t.Stop()
	for {
		if n.waker != nil {
			n.waker.Check(ctx, false)
			n.syncQueueWake()
			n.keepPinned(time.Now())
		}
		n.leaseSweep(time.Now())
		n.sess.noteAwake(time.Now())
		if now := time.Now(); n.orphanDue.Swap(false) || now.Sub(n.orphanSwept) >= orphanSweepEvery {
			n.orphanSwept = now
			n.orphanSweep(now)
		}
		// Stopped (SetStopped) or paused by the budgets (autonomy.go): no
		// wake, seat turn or launch starts; messages wait unread, and
		// launchDue tells their authors why (held:<reason>).
		if n.autoOK() {
			n.wakeIdle(ctx)
		}
		n.seatsDue(ctx, time.Now())
		n.launchMaintain(ctx, time.Now())
		n.launchDue(ctx, time.Now())
		// The pass's own changes do not run it again: drop their kicks, then
		// decide. A change after this point kicks a dormant loop at once, and
		// dormant sees every change before it.
		select {
		case <-n.wakeKick:
		default:
		}
		wait, kick := every, (<-chan struct{})(nil)
		if n.dormant(time.Now()) {
			wait, kick = dormantEvery, n.wakeKick
		}
		n.wakeTicks.Add(1)
		t.Reset(wait)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-kick:
		}
	}
}

// kickWake runs a dormant wake loop at once (Node.changed).
func (n *Node) kickWake() {
	select {
	case n.wakeKick <- struct{}{}:
	default:
	}
}

// dormant reports whether the wake loop of a local chat has nothing to do
// until something changes (wakeDormant). Network projects never are.
func (n *Node) dormant(now time.Time) bool {
	if !n.cfg.LocalOnly || len(n.sess.liveIDs(now)) > 0 {
		return false
	}
	st := n.seats
	st.mu.Lock()
	busy := len(st.run) > 0 || len(st.queued) > 0 || slices.ContainsFunc(st.seats, func(s *Seat) bool { return len(s.Pending) > 0 })
	st.mu.Unlock()
	if busy {
		return false
	}
	for _, l := range n.leases.all() {
		if l.State != LeaseAcked && l.State != LeaseFailed {
			return false
		}
	}
	d := n.deliv
	d.mu.Lock()
	busy = len(d.pending) > 0 || len(d.acks) > 0
	d.mu.Unlock()
	if busy {
		return false
	}
	// A message a launch is for, or will be once its launchGrace passed.
	for _, dir := range n.launchAreas() {
		if eligible, _ := n.launchable(dir, now.Add(launchGrace)); len(eligible) > 0 {
			return false
		}
	}
	return true
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
	var capped []Session // every wake of their idle period is spent (noteWakeCap)
	// active: areas with a live session in a turn (its hooks deliver);
	// cands: per area, the wakeable idle sessions, among which pickIdle picks
	// the one a message for no session in particular wakes.
	active := map[string]bool{}
	cands := map[string][]Session{}
	r.mu.Lock()
	for _, s := range r.sessions {
		if s.live(now) && !s.Idle {
			active[s.Area] = true
		}
	}
	for _, s := range r.sessions {
		if !s.Idle || !s.live(now) {
			continue
		}
		if !s.wakeDue(now) {
			if queue && !s.WakeCapped && s.wakeSpent(now) {
				capped = append(capped, *s)
			}
			continue
		}
		if a, ok := r.inbox[s.SessionID]; ok && n.poster != nil {
			list = append(list, due{s: *s, inbox: a})
		} else if s.Wake == WakeQueue && queue {
			list = append(list, due{s: *s})
		} else {
			continue
		}
		cands[s.Area] = append(cands[s.Area], *s)
	}
	r.mu.Unlock()
	for _, s := range capped {
		n.noteWakeCap(s, !active[s.Area] && len(cands[s.Area]) == 0)
	}
	for _, d := range list {
		s := d.s
		page, err := n.unreadFor(s.Folder, s.SessionID, "", hookBatchIDs, true, false, AgentFilter{Skip: s.liveAgents(time.Now())})
		if err != nil || len(page.Messages) == 0 {
			continue
		}
		byInbox := d.inbox.socket != ""
		via := ViaQueue
		if byInbox {
			via = ViaInbox
		}
		turn := time.Now()
		// One recipient per message: the session it is routed to (its claim,
		// assignment or chat), else, while no session of the area is in a turn,
		// the area's idle session pickIdle picks for it. A message no wake may
		// claim now (failed, in its backoff, another owner's) is left out before
		// the prompt is fitted, so it never keeps the newer ones out of every
		// wake; so is one the session's turn has from an earlier wake, one an
		// earlier wake put into its queue (handedTo: a queue keeps its prompts,
		// so no message is queued twice; an inbox post may be dropped and is
		// retried), and what this node's own person wrote to the others
		// (OwnHuman: information for the session's next event, no reason to
		// start a turn, and no wake of the idle period is spent on it).
		mine := n.routedTo(ids(page.Messages))
		take := page.Messages[:0:0]
		skipped := 0
		for _, m := range page.Messages {
			to := mine[m.ID]
			if m.ForSeat == "" && to != s.SessionID && (to != "" || active[s.Area] || n.pickIdle(m.ID, cands[s.Area]) != s.SessionID) {
				continue
			}
			if m.OwnHuman || (m.ForSeat == "" && (n.leases.blocked(m.ID, s.SessionID, via, turn) || n.leases.handedTo(m.ID, s.SessionID, !byInbox))) {
				skipped++
				continue
			}
			take = append(take, m)
		}
		if len(take) == 0 {
			continue
		}
		// A message in its backoff (or failed, or another owner's) is claimed
		// by no wake: no turn is taken, and no budget state written, for it.
		fit := take[:FitUnread(take)]
		if !slices.ContainsFunc(fit, func(m UnreadMessage) bool {
			return m.ForSeat != "" || !n.leases.blocked(m.ID, s.SessionID, via, turn)
		}) {
			continue
		}
		if !n.autoTake(turn) {
			return // stopped, or a budget just paused autonomous delivery
		}
		msgs, token := n.wakeClaim(s.SessionID, via, fit)
		if len(msgs) == 0 {
			n.autoRefund(turn) // nothing to wake with: no turn spent
			continue
		}
		text := WakePrompt(msgs, skipped+len(take)-len(msgs)+page.Total-len(page.Messages), s.Folder, token)
		images := wakeImages(msgs)
		iw, withImages := n.waker.(ImageWaker)
		switch {
		case byInbox:
			err = n.poster.Post(ctx, d.inbox.socket, d.inbox.token, text)
		case withImages && len(images) > 0:
			err = iw.WakeImages(ctx, s.CodexHome, s.SessionID, text, images)
		default:
			err = n.waker.Wake(ctx, s.CodexHome, s.SessionID, text)
		}
		if err != nil {
			// Its hooks, another session or a launch deliver them instead.
			n.revokeLeases(s.SessionID, ids(msgs), "wake_failed", true)
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
		if !byInbox {
			// After the wake is counted: the proof ends that count (wakeProven).
			go n.observeQueueStart(ctx, s, token, ids(msgs))
		}
	}
}

// pickIdle is the session of cands a wake with message id goes to: the one
// whose leases of it failed least, then the one active last (LastActive, not
// its registration); none passed over for it (maxOwnerFails). "" when every
// one is.
func (n *Node) pickIdle(id string, cands []Session) string {
	var best *Session
	bestFails := 0
	for i := range cands {
		c := &cands[i]
		f := n.leases.fails(id, c.SessionID)
		if f >= maxOwnerFails {
			continue
		}
		if best == nil || f < bestFails || (f == bestFails && (c.activeAt().After(best.activeAt()) ||
			(c.activeAt().Equal(best.activeAt()) && c.SessionID < best.SessionID))) {
			best, bestFails = c, f
		}
	}
	if best == nil {
		return ""
	}
	return best.SessionID
}

func (n *Node) observeQueueStart(ctx context.Context, s Session, token string, ids []string) {
	home := s.CodexHome
	if home == "" {
		home = agentHome("CODEX_HOME", ".codex")
	}
	path := codexRollout(home, s.SessionID)
	if path == "" {
		n.log.Warn("queued Codex turn has no rollout to observe", "session", s.SessionID)
		return // missing proof is not evidence that the queued prompt was dropped
	}
	tick := time.NewTicker(wakePoll)
	defer tick.Stop()
	for {
		if rolloutContains(path, WakeMarker(token)) {
			// The thread's rollout has the prompt: its turn started with the
			// messages in it. That is the proof the session's hook acknowledges
			// them on (UnreadPage.Woken), and the node acknowledges them itself:
			// a thread whose hooks do not run (not loaded, stale) would leave
			// them unread and their lease to lapse though it read them.
			if got := n.LeaseStart(s.SessionID, token, ids); len(got) > 0 {
				if _, err := n.Ack("", AckRequest{IDs: got, SessionID: s.SessionID}); err != nil {
					n.log.Warn("acknowledge a proven queued wake", "session", s.SessionID, "err", err)
				}
			}
			n.wakeProven(s.SessionID)
			return
		}
		select {
		case <-tick.C:
			if !n.queueLeaseActive(s.SessionID, token, ids) {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// wakeProven ends the wake count of session's idle period on proof that it
// took a wake (its rollout shows the queued turn): it is in a turn, whether
// or not a hook event says so. A session whose hooks do not fire stays Idle
// for the node; without this it would stay woken, its wakes spent, for as
// long as it is registered, and nothing would wake it for a later message.
// The messages of the proven wake are its turn's (leaseBook.handedTo): no
// later wake carries them.
func (n *Node) wakeProven(session string) {
	r := n.sess
	now := time.Now()
	r.mu.Lock()
	s := r.sessions[session]
	ok := s != nil && s.Woken
	if ok {
		s.Woken, s.Wakes, s.WokeAt, s.WakeCapped = false, 0, time.Time{}, false
		if err := r.saveLocked(now); err != nil {
			n.log.Warn("save sessions", "err", err)
		}
	}
	r.mu.Unlock()
	if ok {
		n.changed("sessions")
	}
}

func (n *Node) queueLeaseActive(owner, token string, ids []string) bool {
	for _, id := range ids {
		l, ok := n.leases.get(id)
		if ok && l.Owner == owner && l.Token == token && (l.State == LeaseLeased || l.State == LeaseRunning) {
			return true
		}
	}
	return false
}

// wakeDue reports whether the node may wake the idle session now: not woken
// in this idle period yet, or its last wake lapsed untaken (a session that
// took it left the idle period) and a retry is left.
//
// A WakeQueue session has no waiter and, with its app closed, no hook event
// that would end the idle period: past maxIdleWakes it is woken again once
// its last wake stayed unproven for staleWakeWait (as every queue wake, for
// messages no wake of it carried yet), maxStaleWakes times.
func (s Session) wakeDue(now time.Time) bool {
	switch {
	case !s.Woken:
		return true
	case s.Wakes < maxIdleWakes:
		return now.Sub(s.WokeAt) >= inboxWakeGrace
	case s.Wake == WakeQueue && s.Wakes < maxIdleWakes+maxStaleWakes:
		return now.Sub(s.WokeAt) >= staleWakeWait(s.Wakes)
	}
	return false
}

// wakeSpent reports whether a WakeQueue session used up every wake of its
// idle period and the last one stayed unproven for queuedOwnerMax.
func (s Session) wakeSpent(now time.Time) bool {
	return s.Woken && s.Wake == WakeQueue && s.Wakes >= maxIdleWakes+maxStaleWakes && now.Sub(s.WokeAt) >= queuedOwnerMax
}

// noteWakeCap tells the authors of the messages that wait for session s,
// whose wakes of this idle period are spent (wakeSpent), that a person is
// needed: once per idle period (Session.WakeCapped, saved, so a restart does
// not repeat it). The messages stay unread for the session's next event.
//
// alone: no session of its area is in a turn or can be woken, so a message
// for no session in particular waits for it too.
func (n *Node) noteWakeCap(s Session, alone bool) {
	page, err := n.unreadFor(s.Folder, s.SessionID, "", hookBatchIDs, true, false, AgentFilter{Skip: s.liveAgents(time.Now())})
	if err != nil || len(page.Messages) == 0 {
		return
	}
	mine := n.routedTo(ids(page.Messages))
	now := time.Now()
	var list []string
	for _, m := range page.Messages {
		to := mine[m.ID]
		if m.ForSeat == "" && !m.OwnHuman && (to == s.SessionID || (to == "" && alone)) &&
			!n.leases.blocked(m.ID, s.SessionID, ViaQueue, now) && !n.leases.handedTo(m.ID, s.SessionID, true) {
			list = append(list, m.ID)
		}
	}
	if len(list) == 0 {
		return
	}
	r := n.sess
	r.mu.Lock()
	cur := r.sessions[s.SessionID]
	first := cur != nil && cur.Woken && !cur.WakeCapped
	if first {
		cur.WakeCapped = true
		if err := r.saveLocked(now); err != nil {
			n.log.Warn("save sessions", "err", err)
		}
	}
	r.mu.Unlock()
	if !first {
		return
	}
	n.log.Warn("an idle session took none of its wakes; a person decides", "session", s.SessionID, "provider", s.Provider, "wakes", s.Wakes, "messages", len(list))
	n.report(list, AttemptNeedsHuman)
	n.changed("sessions")
}

// hookBatchIDs bounds the unread messages one wake looks at.
const hookBatchIDs = 50

// wakeImages are the image attachments of msgs, by absolute path: a waker that
// can (ImageWaker, `codex queue --image`) hands them to the agent with the
// prompt, which lists every attachment's path (FormatUnread).
func wakeImages(msgs []UnreadMessage) []string {
	var images []string
	for _, m := range msgs {
		for _, a := range m.Attachments {
			if !a.Failed && a.Path != "" && a.IsImage() {
				images = append(images, a.Path)
			}
		}
	}
	return images
}
