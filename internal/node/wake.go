package node

import (
	"context"
	"slices"
	"time"
)

// Waking idle sessions. The node wakes a live session that ended its turn
// (Idle) and has actionable unread messages to take, once per idle period
// plus one retry of a wake it never took (maxIdleWakes). What its own person
// wrote to the others (OwnHuman) is no reason for a wake. A Codex thread's
// queue keeps what it accepted and runs it whenever it gets to it, so its
// prompt is a pointer, no message (pointer.go): the turn fetches what waits
// for the session then, no message has two pointers in the queue at once
// (leaseBook.handedTo), and no thread has two pointers its rollout has not
// shown (pointerPending): a wake that is due meanwhile leases its messages to
// the session under the pointer already queued, unless that one is lost
// (unproven for queuedOwnerMax, or not run by a turn the thread took since:
// pointer.go), when a new one is queued. The node does not depend on
// the thread's hooks: the prompt in its rollout ends the wake count and starts
// the fetch's grace (observeQueueStart), its fetch or activity it reports ends
// the count too (Deliver, SessionActivity), and a wake that stays unproven
// lets newer messages wake it later, bounded (maxStaleWakes, then
// needs_human).
// Nor does it take such a thread for idle on its hooks' word alone: while its
// rollout shows a turn running, or a queued prompt of the node about to run,
// it queues nothing (queueHeld, rollout_turn.go), and the messages that came
// meanwhile go into one wake after the turn; their authors hear why they wait
// (held:busy_turn, noteBusyTurn).
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
// A Claude session's prompt is the messages themselves (WakePrompt, as the
// hooks format them), as many as fit FormatBudget. The node claims them for
// the session as a wake claim first (wakeClaim): its hooks then do not deliver
// them again but acknowledge them at that prompt (UnreadPage.Woken); a failed
// wake drops the claim, and one the session never took lapses after
// inboxWakeGrace, and the hooks deliver them as always. A pointer's messages
// are claimed the same way and leased to the session, but held back from
// nothing: its hooks deliver them at their next event, its fetch otherwise.
// Each woken message's author hears of it (AttemptWakeRequested, then
// AttemptWokenConfirmed at the ack).
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
	if n.waker != nil {
		n.resumeQueueObservers(ctx)
	}
	for {
		if n.waker != nil {
			n.waker.Check(ctx, false)
			n.syncQueueWake()
			n.keepPinned(time.Now())
		}
		n.leaseSweep(time.Now())
		if n.waker != nil {
			n.observeLateStarts(ctx, time.Now())
		}
		n.sess.noteAwake(time.Now())
		if now := time.Now(); n.orphanDue.Swap(false) || now.Sub(n.orphanSwept) >= orphanSweepEvery {
			n.orphanSwept = now
			n.orphanSweep(now)
		}
		// Stopped (SetStopped, autonomy.go): no
		// wake, seat turn or launch starts; messages wait unread, and
		// launchDue tells their authors why (held:<reason>).
		if !n.Stopped() {
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
			if queue && s.wakeSpent(now) {
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
			key := leaseKey(m.ForSeat, m.ID) // a seat's message has its own lease
			if m.OwnHuman || n.leases.blocked(key, s.SessionID, via, turn) || n.leases.handedTo(key, s.SessionID, !byInbox) {
				skipped++
				continue
			}
			take = append(take, m)
		}
		if len(take) == 0 {
			continue
		}
		// A thread its rollout shows in a turn, or about to run a prompt of the
		// node still in its queue, gets no prompt now: what waits for it goes
		// into one wake after the turn.
		// The authors hear of the running turn (held:busy_turn).
		if !byInbox {
			if held, busy := n.queueHeld(s, turn); held {
				if busy {
					n.noteBusyTurn(s, take)
				}
				continue
			}
		}
		// A message in its backoff (or failed, or another owner's) is claimed
		// by no wake. An inbox prompt carries what fits it; a pointer carries
		// nothing, so it is for every one of them.
		fit := take
		if byInbox {
			fit = take[:FitUnread(take)]
		}
		if !slices.ContainsFunc(fit, func(m UnreadMessage) bool {
			return !n.leases.blocked(leaseKey(m.ForSeat, m.ID), s.SessionID, via, turn)
		}) {
			continue
		}
		if n.Stopped() {
			return
		}
		// One pointer at a time: while one is queued for the thread and its
		// rollout has not shown it, nothing more goes into that queue. That
		// pointer's turn fetches everything that waits then, so these messages
		// are leased to the session under its token (pointerPending); the
		// wake's schedule, count and bounds are those of a wake queued anew.
		// A pointer queued before a turn that just ended may run next: nothing
		// for the thread until that is settled (pointerLostWait); one it did
		// not run by then is lost, and the wake queues a new one.
		pending := ""
		if !byInbox {
			var settling bool
			if pending, settling = n.pointerPending(s, turn); settling {
				continue
			}
		}
		msgs, token := n.wakeClaim(s.SessionID, via, fit, pending)
		if len(msgs) == 0 {
			continue
		}
		switch {
		case byInbox:
			text := WakePrompt(msgs, skipped+len(take)-len(msgs)+page.Total-len(page.Messages), s.Folder, token)
			err = n.poster.Post(ctx, d.inbox.socket, d.inbox.token, text)
		case pending != "":
			// Covered by the pointer in its queue: no prompt.
		default:
			// No message, no image of one: the turn fetches them (pointer.go).
			err = n.waker.Wake(ctx, s.CodexHome, s.SessionID, PointerPrompt(s.SessionID, token, cliExecutable()))
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
		n.log.Info("idle session woken", "session", s.SessionID, "provider", s.Provider, "inbox", byInbox, "queued_pointer", pending != "", "messages", len(msgs), "unread", page.Total)
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

// noteBusyTurn tells the authors of msgs, which wait for session s behind a
// turn its rollout shows running (queueHeld), why nothing wakes it for them:
// held:busy_turn, once per message (Session.HeldNoted, saved, so a restart
// does not repeat it). Their wake follows the turn's end.
func (n *Node) noteBusyTurn(s Session, msgs []UnreadMessage) {
	r := n.sess
	now := time.Now()
	var fresh []string
	r.mu.Lock()
	if cur := r.sessions[s.SessionID]; cur != nil {
		for _, m := range msgs {
			if m.ForSeat == "" && !slices.Contains(cur.HeldNoted, m.ID) {
				fresh = append(fresh, m.ID)
			}
		}
		if len(fresh) > 0 {
			cur.HeldNoted = append(cur.HeldNoted, fresh...)
			if over := len(cur.HeldNoted) - capNotedMax; over > 0 {
				cur.HeldNoted = slices.Clone(cur.HeldNoted[over:])
			}
			if err := r.saveLocked(now); err != nil {
				n.log.Warn("save sessions", "err", err)
			}
		}
	}
	r.mu.Unlock()
	if len(fresh) == 0 {
		return
	}
	n.log.Info("messages wait behind a running turn of an idle session", "session", s.SessionID, "provider", s.Provider, "messages", len(fresh))
	n.report(fresh, AttemptHeld+":"+HoldBusyTurn)
	n.changed("sessions")
}

// observeQueueStart watches the rollout of session s for the prompt of the
// queue wake token (one observer per wake, Node.observed). Once it is there
// the wake count ends (wakeProven), and a pointer's messages ids stay the
// session's for pointerFetchGrace (pointerShown): the prompt proves the
// pointer was shown, the fetch delivers them. A prompt of a node before
// pointers (its token is none, pointerToken) carried the messages: it starts
// and acknowledges them. It ends when no lease of the wake is open any more,
// unless one failed unproven (queued_stale) while the thread is in a turn: the
// prompt waits behind that turn, and its late start is still proven
// (observeLateStarts runs it again for a turn that begins later).
func (n *Node) observeQueueStart(ctx context.Context, s Session, token string, ids []string) {
	key := s.SessionID + "\x00" + token
	if _, dup := n.observed.LoadOrStore(key, struct{}{}); dup {
		return
	}
	defer n.observed.Delete(key)
	path := codexRollout(codexHomeOf(s), s.SessionID)
	if path == "" {
		n.log.Warn("queued Codex turn has no rollout to observe", "session", s.SessionID)
		return // missing proof is not evidence that the queued prompt was dropped
	}
	tick := time.NewTicker(wakePoll)
	defer tick.Stop()
	for {
		if pointerToken(token) {
			// A pointer is for what came after it was queued too (wakeIdle).
			if cur := n.leases.withToken(s.SessionID, token); len(cur) > 0 {
				ids = cur
			}
		}
		if rolloutContains(path, WakeMarker(token)) {
			if pointerToken(token) {
				// The thread's rollout has the pointer: its turn started and is to
				// fetch. Nothing is read by that alone.
				n.pointerShown(s.SessionID, token)
			} else if got := n.LeaseStart(s.SessionID, token, ids); len(got) > 0 {
				// A prompt queued before pointers: its turn started with the
				// messages in it. That is the proof the session's hook acknowledges
				// them on (UnreadPage.Woken), and the node acknowledges them itself:
				// a thread whose hooks do not run (not loaded, stale) would leave
				// them unread and their lease to lapse though it read them.
				if _, err := n.Ack("", AckRequest{IDs: got, SessionID: s.SessionID}); err != nil {
					n.log.Warn("acknowledge a proven queued wake", "session", s.SessionID, "err", err)
				}
			}
			n.wakeProven(s.SessionID)
			return
		}
		select {
		case <-tick.C:
			if !n.queueLeaseActive(s.SessionID, token, ids) && !n.queueLateStart(s, token, ids, time.Now()) {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// resumeQueueObservers watches the rollouts of the queue wakes still unproven
// in the book again (leased, ViaQueue; one observer per wake token): the
// observer of a wake ends with the node, and without one a prompt the thread
// runs after a restart would prove and acknowledge nothing where the thread's
// hooks do not fire.
func (n *Node) resumeQueueObservers(ctx context.Context) {
	type wake struct{ owner, token string }
	byWake := map[wake][]string{}
	for _, l := range n.leases.all() {
		if l.State == LeaseLeased && l.Via == ViaQueue && l.Token != "" && sessionOwner(l.Owner) {
			w := wake{l.Owner, l.Token}
			byWake[w] = append(byWake[w], l.ID)
		}
	}
	if len(byWake) == 0 {
		return
	}
	r := n.sess
	for w, list := range byWake {
		s := Session{SessionID: w.owner}
		r.mu.Lock()
		if cur := r.sessions[w.owner]; cur != nil {
			s = *cur
		}
		r.mu.Unlock()
		go n.observeQueueStart(ctx, s, w.token, list)
	}
	n.log.Info("queued wakes observed again", "wakes", len(byWake))
}

// queueLateStart reports whether the prompt of the queue wake token, whose
// leases of ids failed unproven (queued_stale), may still start: the thread
// of s is in a turn by its rollout.
func (n *Node) queueLateStart(s Session, token string, ids []string, now time.Time) bool {
	stale := false
	for _, id := range ids {
		l, ok := n.leases.get(id)
		if ok && l.Owner == s.SessionID && l.Token == token && l.State == LeaseFailed && l.Reason == "queued_stale" {
			stale = true
			break
		}
	}
	if !stale {
		return false
	}
	_, running := n.turnRunning(s, now)
	return running
}

// observeLateStarts watches the rollouts of the queue wakes that failed
// unproven (queued_stale) again while their thread is in a turn by its
// rollout: the prompt is still in the thread's queue and runs after that
// turn, or is the turn, and nothing else would prove and acknowledge it where
// the thread's hooks do not fire.
func (n *Node) observeLateStarts(ctx context.Context, now time.Time) {
	type wake struct{ owner, token string }
	byWake := map[wake][]string{}
	for _, l := range n.leases.all() {
		if l.State == LeaseFailed && l.Reason == "queued_stale" && l.Via == ViaQueue && l.Token != "" && sessionOwner(l.Owner) {
			w := wake{l.Owner, l.Token}
			byWake[w] = append(byWake[w], l.ID)
		}
	}
	running := map[string]bool{} // by owner: one rollout read each
	r := n.sess
	for w, list := range byWake {
		if _, on := n.observed.Load(w.owner + "\x00" + w.token); on {
			continue
		}
		r.mu.Lock()
		cur := r.sessions[w.owner]
		var s Session
		if cur != nil {
			s = *cur
		}
		r.mu.Unlock()
		if cur == nil || !s.live(now) {
			continue
		}
		on, seen := running[w.owner]
		if !seen {
			_, on = n.turnRunning(s, now)
			running[w.owner] = on
		}
		if on {
			go n.observeQueueStart(ctx, s, w.token, list)
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
		s.Woken, s.Wakes, s.WokeAt, s.WakeCapped, s.CapNoted = false, 0, time.Time{}, false, nil
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
// needed: once per message (Session.CapNoted, saved with WakeCapped, so a
// restart does not repeat it), also for one that comes after the first
// notice, as nothing wakes the session for it either. The messages stay
// unread for the session's next event.
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
	var fresh []string
	if cur != nil && cur.Woken {
		for _, id := range list {
			if !slices.Contains(cur.CapNoted, id) {
				fresh = append(fresh, id)
			}
		}
	}
	if len(fresh) > 0 {
		cur.WakeCapped = true
		cur.CapNoted = append(cur.CapNoted, fresh...)
		if over := len(cur.CapNoted) - capNotedMax; over > 0 {
			cur.CapNoted = slices.Clone(cur.CapNoted[over:])
		}
		if err := r.saveLocked(now); err != nil {
			n.log.Warn("save sessions", "err", err)
		}
	}
	r.mu.Unlock()
	if len(fresh) == 0 {
		return
	}
	n.log.Warn("an idle session took none of its wakes; a person decides", "session", s.SessionID, "provider", s.Provider, "wakes", s.Wakes, "messages", len(fresh))
	n.report(fresh, AttemptNeedsHuman)
	n.changed("sessions")
}

// hookBatchIDs bounds the unread messages one wake looks at.
const hookBatchIDs = 50
