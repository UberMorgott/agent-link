package node

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Routing of unread messages between this node's live sessions. Read state is
// the node's, but a message is delivered to one session only: several
// sessions in one folder all poll Unread, and a reply one of them asked for
// must not land in the others as a task. A message is for:
//
//   - the session that claimed it (Claim) and has not acknowledged it within
//     claimTTL, or the session assigned to answer it (Ack); or the session
//     the node woke with it (wakeClaim) for inboxWakeGrace, whose hooks
//     acknowledge it instead of delivering it (UnreadPage.Woken);
//   - else the chat's session (chat affinity): the one behind the chat's
//     newest message a session of this node wrote (chatRecord.Session,
//     recorded by agentlink send inside the session) or was assigned. A
//     session that never took part in a chat does not get its messages while
//     one that did lives. An active session (in a turn) wins over an idle
//     one of its area: while one lives, affinity picks among the active ones
//     only, and a chat whose session is idle is for no session in particular
//     (the active one's hooks take it, no idle session is woken). A session
//     without a hook event for AffinityLapse (LastActive) has lost its
//     chats: an abandoned one its waiter keeps live does not hold them;
//   - else no session in particular: the first session to Claim it takes it.
//
// Only live sessions count: a message for a session that is gone is anyone's.

// claimTTL is how long a claim holds without an ack: the hook acks right
// after it delivers, so a claim older than that was never delivered and the
// message goes back to its route.
const claimTTL = time.Minute

// AffinityLapse: a session with no hook event (LastActive) for this long no
// longer holds its chats (routeOf), though its waiter keeps it live.
const AffinityLapse = 30 * time.Minute

// sessionClaim is one session's claim of an unread message (Claim), or, with
// wake, the node's for the prompt that woke the session with it (wakeClaim).
type sessionClaim struct {
	session string
	at      time.Time
	wake    bool
	// token (a wake only) is the one the wake prompt carries (WakeMarker): the
	// hooks acknowledge the message only at a prompt that carries it.
	token string
	// launch: the node's for the first turn of a desktop launch (launchClaim);
	// session is then launchOwner(area), no registered session.
	launch bool
	// ackOnly: the messages a desktop launch's session took whose ack is
	// pending (holdForAck); held until acknowledged, delivered to nobody.
	ackOnly bool
}

// launchHold bounds a launch claim: the first turn's own timeout, plus the
// time to end it (runDirect drops or acknowledges it before that).
const launchHold = desktopTurnTimeout + 5*time.Minute

// held reports whether the claim still holds for a live session: claimTTL,
// or inboxWakeGrace for a wake (then the session's waiter takes over); a
// launch claim holds for launchHold, no session being live yet.
func (c sessionClaim) held(live map[string]bool) bool {
	if c.ackOnly {
		return true
	}
	if c.launch {
		return time.Since(c.at) < launchHold
	}
	ttl := claimTTL
	if c.wake {
		ttl = inboxWakeGrace
	}
	return live[c.session] && time.Since(c.at) < ttl
}

// launchOwner is the claim owner of a desktop launch in area: never a session
// id (validSessionID refuses a space).
func launchOwner(area string) string { return "launch " + area }

// launchClaim claims msgs for the first turn of a desktop launch in area,
// atomically, like a wake (claimLocked): not the ones a session's hook or
// another launch holds. Until runDirect acknowledges or drops them, no
// session's hooks deliver them (routeOf names the launch). It returns the ones
// granted, in order.
func (n *Node) launchClaim(area string, msgs []UnreadMessage) []UnreadMessage {
	r := n.sess
	r.claimMu.Lock()
	defer r.claimMu.Unlock()
	live := r.liveIDs(time.Now())
	owner := launchOwner(area)
	var want []string
	for _, m := range msgs {
		if c, ok := r.claims[m.ID]; ok && c.held(live) {
			continue // a session's hook, a wake or a launch has it
		}
		want = append(want, m.ID)
	}
	granted := n.claimLocked(owner, want, ViaLaunch, "", live)
	var out []UnreadMessage
	for _, m := range msgs {
		if slices.Contains(granted, m.ID) {
			r.claims[m.ID] = sessionClaim{session: owner, at: time.Now(), launch: true}
			out = append(out, m)
		}
	}
	return out
}

// routedTo maps each of ids to the live session it is for (routeOf), ""
// for no session in particular.
func (n *Node) routedTo(ids []string) map[string]string {
	r := n.sess
	r.claimMu.Lock()
	defer r.claimMu.Unlock()
	live := r.liveIDs(time.Now())
	out := make(map[string]string, len(ids))
	for _, id := range ids {
		if rec, ok := n.chats.message(id); ok {
			out[id] = n.routeOf(id, &rec, live)
		} else {
			out[id] = n.routeOf(id, nil, live)
		}
	}
	return out
}

// launchHeld is the set of messages a desktop launch holds (launchClaim), or
// its session does while their ack is pending (holdForAck).
func (n *Node) launchHeld() map[string]bool {
	r := n.sess
	r.claimMu.Lock()
	defer r.claimMu.Unlock()
	out := map[string]bool{}
	for id, c := range r.claims {
		if (c.launch || c.ackOnly) && c.held(nil) {
			out[id] = true
		}
	}
	return out
}

// ClaimRequest is the body of POST /claim.
type ClaimRequest struct {
	IDs       []string `json:"ids"`
	SessionID string   `json:"session_id"`
	Folder    string   `json:"folder,omitempty"` // routing hint of the control API
	// WakeToken makes it a wake claim (Claude's background waiter wakes the
	// session with them, WakeMarker(WakeToken) in its output): the hooks
	// acknowledge them only at an event that shows the session got that wake
	// (UnreadPage.Woken), else it lapses after inboxWakeGrace.
	WakeToken string `json:"wake_token,omitempty"`
}

// liveIDs is the set of live session ids.
func (r *sessionRegistry) liveIDs(now time.Time) map[string]bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]bool{}
	for id, s := range r.sessions {
		if s.live(now) {
			out[id] = true
		}
	}
	return out
}

// activeAts is when each registered session was last active (activeAt).
func (r *sessionRegistry) activeAts() map[string]time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]time.Time, len(r.sessions))
	for id, s := range r.sessions {
		out[id] = s.activeAt()
	}
	return out
}

// routeOf is the live session unread message id is for; "" when it is for no
// session in particular. rec is its chat record (nil for a plain message).
// The caller holds n.sess.claimMu.
func (n *Node) routeOf(id string, rec *chatRecord, live map[string]bool) string {
	if q := n.leases.queuedOwner(id); q != "" && live[q] {
		// Its wake prompt sits in that session's queue or inbox: it stays that
		// session's until proof, the session's end or its silence for
		// queuedOwnerMax (leaseSweep; no second handler meanwhile).
		return q
	}
	if c, ok := n.sess.claims[id]; ok && c.held(live) {
		return c.session
	}
	if rec == nil {
		return ""
	}
	if s := assignedSession(*rec); live[s] {
		return s
	}
	recent := n.sess.recentlyActive(live, time.Now())
	aff := n.chats.affinity(rec.Message.ChatID, n.cfg.Node, recent)
	if n.leases.passedOver(id, aff) {
		// Affinity is a preference: a session whose leases of the message kept
		// failing does not hold it.
		aff = ""
	}
	if busy := n.sess.busyBeside(aff, recent); busy != nil {
		// The chat's session is idle while another of its area is in a turn:
		// the active one wins (no wake), affinity only picks among them.
		return n.chats.affinity(rec.Message.ChatID, n.cfg.Node, busy)
	}
	return aff
}

// recentlyActive is the subset of live whose sessions had a hook event within
// AffinityLapse.
func (r *sessionRegistry) recentlyActive(live map[string]bool, now time.Time) map[string]bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]bool, len(live))
	for id, ok := range live {
		if s := r.sessions[id]; ok && s != nil && now.Sub(s.activeAt()) < AffinityLapse {
			out[id] = true
		}
	}
	return out
}

// busyBeside is, when session id is idle, the set of the live sessions of its
// area in a turn (not Idle); nil when it is not idle or there are none.
func (r *sessionRegistry) busyBeside(id string, live map[string]bool) map[string]bool {
	if id == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.sessions[id]
	if s == nil || !s.Idle {
		return nil
	}
	var out map[string]bool
	for oid, o := range r.sessions {
		if live[oid] && !o.Idle && o.Area == s.Area {
			if out == nil {
				out = map[string]bool{}
			}
			out[oid] = true
		}
	}
	return out
}

// assignedSession is the session a record is assigned to ("session:<id>").
func assignedSession(r chatRecord) string {
	s, _ := strings.CutPrefix(r.Assigned, "session:")
	if s == r.Assigned {
		return ""
	}
	return s
}

// Claim takes unread messages for delivery to one live session, atomically:
// it returns the ids granted, those still unread and not for another live
// session (routeOf); none to a session that is not registered. A granted message stays that session's while it lives,
// until it is acknowledged; claiming again is granted again, but not what the
// prompt that woke the session holds (wakeClaim).
func (n *Node) Claim(req ClaimRequest) ([]string, error) {
	switch {
	case !validSessionID(req.SessionID):
		return nil, fmt.Errorf("%w: invalid session_id", ErrBadRequest)
	case len(req.IDs) > 1000:
		return nil, fmt.Errorf("%w: at most 1000 ids", ErrBadRequest)
	case req.WakeToken != "" && !validWakeToken(req.WakeToken):
		return nil, fmt.Errorf("%w: invalid wake_token", ErrBadRequest)
	}
	r := n.sess
	now := time.Now()
	r.claimMu.Lock()
	live := r.liveIDs(now)
	if !live[req.SessionID] {
		r.claimMu.Unlock()
		// Only a registered session takes messages: a hook of a run that never
		// registered (a headless claude -p) must not steal them.
		return []string{}, nil
	}
	want, spent, via := req.IDs, []string(nil), ViaHook
	if req.WakeToken != "" {
		want, spent = n.waiterWakesLeft(req.SessionID, req.IDs, now)
		via = ViaWaiter
	}
	granted := n.claimLocked(req.SessionID, want, via, req.WakeToken, live)
	r.claimMu.Unlock()
	n.report(spent, AttemptNeedsHuman)
	return granted, nil
}

// maxWaiterWakes bounds the wakes of one message by Claude waiters (Claim
// with a WakeToken), like maxIdleWakes the node's own: a wake no hook event
// proved lapses and may be tried again, but a session that never shows it
// got one (it is gone, or its transcript lacks the wakes) is not woken for
// that message forever. After that no waiter wakes for it: it stays unread
// for the session's next event, and its author hears it needs a person
// (AttemptNeedsHuman).
const maxWaiterWakes = 2

// waiterWakeKeep: a message's waiter wake count is forgotten this long after
// its last wake.
const waiterWakeKeep = 24 * time.Hour

// waiterSpentMsg reports whether no waiter may wake with the message of key
// any more: it used up maxWaiterWakes (the lease's count, forgotten
// waiterWakeKeep after its last waiter wake) or its lease failed.
func (n *Node) waiterSpentMsg(key string, now time.Time) bool {
	l, ok := n.leases.get(key)
	if !ok {
		return false
	}
	return l.Failed || (l.WaiterWakes >= maxWaiterWakes && now.Sub(l.WaiterAt) <= waiterWakeKeep)
}

// waiterWakesLeft splits ids into those the waiter of session may still wake
// with and those that used up maxWaiterWakes (their lease counts them).
func (n *Node) waiterWakesLeft(session string, ids []string, now time.Time) (left, spent []string) {
	for _, id := range ids {
		if n.waiterSpentMsg(leaseKey(n.seatPendingFor(session, id), id), now) {
			spent = append(spent, id)
		} else {
			left = append(left, id)
		}
	}
	return left, spent
}

// waiterSpent drops from page the messages no waiter may wake with anymore
// (maxWaiterWakes), so a waiter does not poll for them.
func (n *Node) waiterSpent(page *UnreadPage) {
	now := time.Now()
	kept := page.Messages[:0]
	for _, m := range page.Messages {
		if !n.waiterSpentMsg(leaseKey(m.ForSeat, m.ID), now) {
			kept = append(kept, m)
		}
	}
	page.Total -= len(page.Messages) - len(kept)
	page.Messages = kept
}

// validWakeToken accepts a short token of letters, digits, '-' and '_' (it
// goes into WakeMarker).
func validWakeToken(t string) bool { return wakeTokenPattern.MatchString(t) }

var wakeTokenPattern = regexp.MustCompile(`^[0-9A-Za-z_-]{8,64}$`)

// claimLocked grants session the ids of want still unread, not for another
// live session and not held by its own wake claim, as a wake claim with wake
// (carrying token). A wake never takes a message a hook claimed (a normal
// claim that holds, even of the same session): that hook delivers it.
// The caller holds n.sess.claimMu.
func (n *Node) claimLocked(session string, want []string, via, token string, live map[string]bool) []string {
	r := n.sess
	wake := via != ViaHook
	now := time.Now()
	deadline := now.Add(claimTTL)
	switch via {
	case ViaLaunch:
		deadline = now.Add(launchHold)
	case ViaInbox, ViaQueue, ViaWaiter:
		deadline = now.Add(inboxWakeGrace)
	}
	granted := []string{}
	plain := map[string]bool{}
	for _, p := range n.store.unreadPlain() {
		plain[p.Message.ID] = true
	}
	for _, id := range want {
		if seat := n.seatPendingFor(session, id); seat != "" {
			if n.leases.blocked(leaseKey(seat, id), session, via, now) {
				continue
			}
			if _, ok := n.seatClaim(session, id, wake, token); ok {
				if took, err := n.leases.take(id, seat, session, via, token, deadline, now); err != nil {
					n.log.Warn("save leases", "err", err)
				} else if !took {
					n.seatUnclaim(session, []string{id})
					continue
				}
				granted = append(granted, id)
			}
			continue
		}
		if n.seatHas(id) {
			continue // another seat's (seatsForIncoming): not for this session
		}
		if c, ok := r.claims[id]; ok && c.ackOnly {
			continue // taken by a launched session, its ack pending: nobody's to deliver
		}
		var to string
		if rec, ok := n.chats.message(id); ok {
			if !rec.Unread || !rec.ReadAt.IsZero() || rec.Message.Kind != "" {
				continue
			}
			to = n.routeOf(id, &rec, live)
		} else if plain[id] {
			to = n.routeOf(id, nil, live)
		} else {
			continue
		}
		if to != "" && to != session {
			continue
		}
		if _, woke := n.wokeWith(id, session, live); woke {
			continue // the wake prompt has it: not delivered again
		}
		if c, ok := r.claims[id]; wake && ok && !c.wake && c.held(live) {
			continue // a hook is delivering it: waking with it too delivers it twice
		}
		if n.leases.blocked(id, session, via, now) {
			continue // failed, in its backoff, or another owner's lease holds
		}
		if took, err := n.leases.take(id, "", session, via, token, deadline, now); err != nil {
			n.log.Warn("save leases", "err", err)
		} else if !took {
			continue
		}
		r.claims[id] = sessionClaim{session: session, at: now, wake: wake, token: token}
		granted = append(granted, id)
	}
	return granted
}

// wakeClaim claims msgs for the wake prompt of session (claimLocked) under a
// new wake token and returns the ones granted, in order, with the token.
func (n *Node) wakeClaim(session, via string, msgs []UnreadMessage) ([]UnreadMessage, string) {
	r := n.sess
	r.claimMu.Lock()
	defer r.claimMu.Unlock()
	live := r.liveIDs(time.Now())
	if !live[session] {
		return nil, ""
	}
	token := randomHex(8)
	granted := n.claimLocked(session, ids(msgs), via, token, live)
	var out []UnreadMessage
	for _, m := range msgs {
		if slices.Contains(granted, m.ID) {
			out = append(out, m)
		}
	}
	return out, token
}

// unclaim drops session's claims of ids (a wake that failed): the messages go
// back to their route and the hooks deliver them.
func (n *Node) unclaim(session string, ids []string) {
	n.seatUnclaim(session, ids)
	r := n.sess
	r.claimMu.Lock()
	defer r.claimMu.Unlock()
	for _, id := range ids {
		if c, ok := r.claims[id]; ok && c.session == session {
			delete(r.claims, id)
		}
	}
}

// wokeWith reports whether id is held by a wake claim of session, and the
// token of that wake.
func (n *Node) wokeWith(id, session string, live map[string]bool) (string, bool) {
	c, ok := n.sess.claims[id]
	if ok && c.wake && c.session == session && c.held(live) {
		return c.token, true
	}
	return "", false
}
