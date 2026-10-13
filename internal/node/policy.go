package node

import "time"

// The delivery policy: every hold time and retry budget of an unread message,
// in one place. Who holds a message:
//
//   - the lease (lease.go, leases.json) is the durable record of one message
//     for one recipient: its owner, state, attempts and failures;
//   - a hold (hold.go) is the runtime exclusion while an owner delivers it:
//     a session's claim (routing.go) or a seat's mark (seats.go), of one type
//     and one rule (hold.holds) over the hold times below.
//
// Budgets per message (the lease): maxLeaseAttempts automatic leases with a
// leaseBackoff between them, then failed (needs_human); maxOwnerFails per
// owner, then it is passed over; maxWaiterWakes waiter wakes per
// waiterWakeKeep. Budgets of other subjects: maxIdleWakes per idle period of
// a session, launchTries per desktop launch, seatRetry per seat's failed turns.

// Hold times.
const (
	// claimTTL is how long a hook's claim holds without an ack: the hook acks
	// right after it delivers, so a claim older than that was never delivered
	// and the message goes back to its route.
	claimTTL = time.Minute
	// inboxWakeGrace: a session still idle this long after a successful inbox
	// wake did not take it (its crossSessionInbound setting may drop posts, or
	// it sits in a dialog); its background waiter wakes it from then on. A
	// wake's hold lasts as long.
	inboxWakeGrace = 2 * time.Minute
	// launchHold bounds a launch claim: the first turn's own timeout, plus the
	// time to end it (runDirect drops or acknowledges it before that).
	launchHold = desktopTurnTimeout + 5*time.Minute
	// queuedOwnerMax bounds how long a session keeps a message whose wake
	// prompt sits in its queue or inbox while it shows no activity (active: its
	// last hook event, not a heartbeat): a session whose waiter hit its wake cap
	// but keeps heartbeating would hold it for as long as it stays registered.
	// Past it the lease fails (queued_stale, needs_human): the prompt was
	// accepted and never proven, so nothing queues it a second time.
	queuedOwnerMax = 30 * time.Minute
)

// Lease budgets (lease.go).
const (
	// maxLeaseAttempts bounds the automatic leases of one message (every via
	// but ViaHook); then it is failed.
	maxLeaseAttempts = 5
	// maxOwnerFails: an owner whose leases of a message failed this often is
	// passed over for it.
	maxOwnerFails   = 2
	leaseBackoffMin = 15 * time.Second
	leaseBackoffMax = 5 * time.Minute
	// runningHold is how long a running lease waits for its ack.
	runningHold = launchHold
	// leaseHoldMax bounds a held lease whose ack keeps failing (holdForAck):
	// then it is failed and the message is released to the hooks.
	leaseHoldMax = time.Hour
)

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

// maxIdleWakes bounds the node's wakes of one idle period: the first, and one
// retry after it lapsed untaken (inboxWakeGrace; its claim lapses with it and
// the messages are unread again; an inbox post is retried with them, a queue
// wake carries only what the queue does not hold). A lapsed wake also frees the session's
// waiter (Claude, InboxWakes); a message goes to one of them only (claims).
// After that the waiter or the session's next event takes them.
const maxIdleWakes = 2

// maxStaleWakes bounds the further wakes of one idle period of a WakeQueue
// session (Codex: no waiter wakes it, and a closed app sends no hook event
// that would end the idle period): once the last wake stayed unproven for
// staleWakeWait, the session is woken again, for messages no wake of it
// carried yet only. An unproven prompt itself is never queued again
// (queued_stale). After maxIdleWakes+maxStaleWakes wakes nothing wakes the
// session in this idle period any more, and the authors of what waits for it
// then hear needs_human, once (Session.WakeCapped).
const maxStaleWakes = 3

// rolloutTurnQuiet: a turn a Codex thread's rollout shows started and not
// ended counts as running only while the rollout was written to within it
// (queueHeld). A thread killed inside a turn writes no end; a turn silent for
// longer (one long tool call) is woken like an idle thread, and its prompt
// waits behind the turn.
const rolloutTurnQuiet = 10 * time.Minute

// capNotedMax bounds the message ids a session remembers as reported at its
// wake cap (Session.CapNoted); an older one may then be reported again.
const capNotedMax = 200

// staleWakeWaits are the waits before the stale wakes of one idle period, each
// counted from the session's last wake: a newer message is tried soon, then
// ever more rarely. They do not wait for the leases of the last wake to fail
// (queuedOwnerMax): a stale wake carries only what no wake carried yet.
var staleWakeWaits = [maxStaleWakes]time.Duration{2 * time.Minute, 15 * time.Minute, time.Hour}

// staleWakeWait is how long a WakeQueue session's last wake of wakes so far
// must be unproven before the next stale wake (staleWakeWaits).
func staleWakeWait(wakes int) time.Duration {
	return staleWakeWaits[min(max(wakes-maxIdleWakes, 0), maxStaleWakes-1)]
}

// launchTries bounds the launches of one desktop launch's messages (a new
// session each; launch.go).
const launchTries = 2

// seatRetry are the waits before the automatic tries of a seat's failed turn.
var seatRetry = []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute}
