package node

import (
	"fmt"
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
//     (the active one's hooks take it, no idle session is woken);
//   - else no session in particular: the first session to Claim it takes it.
//
// Only live sessions count: a message for a session that is gone is anyone's.

// claimTTL is how long a claim holds without an ack: the hook acks right
// after it delivers, so a claim older than that was never delivered and the
// message goes back to its route.
const claimTTL = time.Minute

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
	granted := n.claimLocked(owner, want, true, "", live)
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

// routeOf is the live session unread message id is for; "" when it is for no
// session in particular. rec is its chat record (nil for a plain message).
// The caller holds n.sess.claimMu.
func (n *Node) routeOf(id string, rec *chatRecord, live map[string]bool) string {
	if c, ok := n.sess.claims[id]; ok && c.held(live) {
		return c.session
	}
	if rec == nil {
		return ""
	}
	if s := assignedSession(*rec); live[s] {
		return s
	}
	aff := n.chats.affinity(rec.Message.ChatID, n.cfg.Node, live)
	if busy := n.sess.busyBeside(aff, live); busy != nil {
		// The chat's session is idle while another of its area is in a turn:
		// the active one wins (no wake), affinity only picks among them.
		return n.chats.affinity(rec.Message.ChatID, n.cfg.Node, busy)
	}
	return aff
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
	}
	r := n.sess
	r.claimMu.Lock()
	defer r.claimMu.Unlock()
	live := r.liveIDs(time.Now())
	if !live[req.SessionID] {
		// Only a registered session takes messages: a hook of a run that never
		// registered (a headless claude -p) must not steal them.
		return []string{}, nil
	}
	return n.claimLocked(req.SessionID, req.IDs, false, "", live), nil
}

// claimLocked grants session the ids of want still unread, not for another
// live session and not held by its own wake claim, as a wake claim with wake
// (carrying token). A wake never takes a message a hook claimed (a normal
// claim that holds, even of the same session): that hook delivers it.
// The caller holds n.sess.claimMu.
func (n *Node) claimLocked(session string, want []string, wake bool, token string, live map[string]bool) []string {
	r := n.sess
	granted := []string{}
	plain := map[string]bool{}
	for _, p := range n.store.unreadPlain() {
		plain[p.Message.ID] = true
	}
	for _, id := range want {
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
		r.claims[id] = sessionClaim{session: session, at: time.Now(), wake: wake, token: token}
		granted = append(granted, id)
	}
	return granted
}

// wakeClaim claims msgs for the wake prompt of session (claimLocked) under a
// new wake token and returns the ones granted, in order, with the token.
func (n *Node) wakeClaim(session string, msgs []UnreadMessage) ([]UnreadMessage, string) {
	r := n.sess
	r.claimMu.Lock()
	defer r.claimMu.Unlock()
	live := r.liveIDs(time.Now())
	if !live[session] {
		return nil, ""
	}
	token := randomHex(8)
	granted := n.claimLocked(session, ids(msgs), true, token, live)
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
