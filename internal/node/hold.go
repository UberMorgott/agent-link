package node

import "time"

// holdKind is how an owner holds an unread message while it delivers it.
type holdKind uint8

const (
	// holdHook: a session's hook claimed it (Claim) and acks it right after it
	// delivers; it holds for claimTTL while the session lives.
	holdHook holdKind = iota
	// holdWake: the node or a waiter woke the session with it (wakeClaim); the
	// hooks acknowledge it at the prompt carrying token. It holds for
	// inboxWakeGrace while the session lives.
	holdWake
	// holdLaunch: a desktop launch's first turn has it (launchClaim); it holds
	// for launchHold, no session being live yet.
	holdLaunch
	// holdAck: a launched session took it and its ack is pending
	// (holdForAck): it holds until released, delivered to nobody.
	holdAck
	// holdTurn: the node's turn of a seat has it (claimTurn); it holds until
	// the turn ends.
	holdTurn
)

// hold is the runtime exclusion of one unread message while its owner
// delivers it: a session's claim (sessionRegistry.claims, by message id) or a
// seat's mark (seatStore.marks, by seatKey). Its durable record is the
// message's lease (lease.go); the hold times are in policy.go.
type hold struct {
	// owner is the session (or launchOwner) of a claim; "" on a seat's mark,
	// whose key names the seat.
	owner string
	kind  holdKind
	// token (a wake only) is the one the wake prompt carries (WakeMarker): the
	// hooks acknowledge the message only at a prompt that carries it.
	token string
	at    time.Time
}

// holds reports whether h still holds at now; live is whether its owner
// session is live (a seat's mark does not depend on it: pass true).
func (h hold) holds(now time.Time, live bool) bool {
	switch h.kind {
	case holdAck, holdTurn:
		return true
	case holdLaunch:
		return now.Sub(h.at) < launchHold
	case holdWake:
		return live && now.Sub(h.at) < inboxWakeGrace
	case holdHook:
	}
	return live && now.Sub(h.at) < claimTTL
}

// held reports whether session claim h still holds now, live the set of live
// sessions.
func (h hold) held(live map[string]bool) bool { return h.holds(time.Now(), live[h.owner]) }

// wake reports whether h is a wake's hold.
func (h hold) wake() bool { return h.kind == holdWake }
