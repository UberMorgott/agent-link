package node

import (
	"errors"
	"io/fs"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// Delivery leases (docs/notes/delivery-lease.md). Every hand-off of an
// unread message to one owner (a session, a desktop launch, a seat's turn) is
// a lease with a deadline:
//
//	pending -> leased(owner, deadline) -> running -> acked
//	                  |                     |
//	                  +---- retry <---------+----> failed (needs_human)
//
// leased: the owner was handed the message (a claim, a wake post, a queued
// prompt, a launch). running: proof the owner's turn has it (its ack at the
// woken prompt, a launch's session id, LeaseStart). A lease that loses its
// owner (wake failed, session ended or gone, deadline passed without proof)
// goes back (retry) with a bounded backoff and the owner counts one failure;
// an owner that failed a message maxOwnerFails times is passed over for it
// (routeOf, wakeIdle, orphans). After maxLeaseAttempts automatic leases the
// message is failed: no automatic delivery any more, its author hears
// needs_human, and the hooks still deliver it at a session's next event.
//
// The book is persisted in leases.json: after a restart a message is not
// woken for again beyond its caps, and a lapsed wake's token still proves.
// Lock order: n.sess.claimMu, then leaseBook.mu.

// Lease states.
const (
	LeasePending = "pending"
	LeaseLeased  = "leased"
	LeaseRunning = "running"
	LeaseAcked   = "acked"
	LeaseRetry   = "retry"
	LeaseFailed  = "failed"
)

// Lease vias: how the owner was handed the message.
const (
	ViaHook   = "hook"   // a session's hook claimed it (the session is in a turn)
	ViaInbox  = "inbox"  // the node posted a wake to a Claude session's inbox
	ViaQueue  = "queue"  // the node queued a Codex wake
	ViaWaiter = "waiter" // a Claude session's background waiter woke it
	ViaLaunch = "launch" // a desktop launch's first turn
	ViaSeat   = "seat"   // the node's turn of a seat
)

// Lease limits.
const (
	leaseFile = "leases.json"
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
	// leaseDoneKeep: a record of a message no longer unread is kept this long
	// (the API shows it); leaseKeep bounds every record.
	leaseDoneKeep = 10 * time.Minute
	leaseKeep     = 7 * 24 * time.Hour
)

// Lease is one message's delivery lease for one recipient on this node.
type Lease struct {
	ID    string `json:"id"`             // message id
	Seat  string `json:"seat,omitempty"` // the seat it is pending for; "" for the node's sessions
	State string `json:"state"`
	Owner string `json:"owner,omitempty"`
	Via   string `json:"via,omitempty"`
	Token string `json:"token,omitempty"`
	// Attempts counts automatic leases (every via but ViaHook); WaiterWakes
	// the ViaWaiter ones (maxWaiterWakes), the last at WaiterAt.
	Attempts    int       `json:"attempts,omitempty"`
	WaiterWakes int       `json:"waiter_wakes,omitempty"`
	WaiterAt    time.Time `json:"waiter_at,omitzero"`
	// Fails counts failed leases per owner.
	Fails    map[string]int `json:"fails,omitempty"`
	Deadline time.Time      `json:"deadline,omitzero"`
	NextAt   time.Time      `json:"next_at,omitzero"`
	Reason   string         `json:"reason,omitempty"`
	// Hold: running with no deadline, its ack pending (holdForAck).
	Hold bool      `json:"hold,omitempty"`
	At   time.Time `json:"at"`
}

// LeaseView is the lease state of one unread message (GET /leases).
type LeaseView struct {
	ID       string    `json:"id"`
	ChatID   string    `json:"chat_id,omitempty"`
	Seat     string    `json:"seat,omitempty"`
	State    string    `json:"state"`
	Owner    string    `json:"owner,omitempty"`
	Via      string    `json:"via,omitempty"`
	Attempts int       `json:"attempts"`
	Deadline time.Time `json:"deadline,omitzero"`
	NextAt   time.Time `json:"next_at,omitzero"`
	Reason   string    `json:"reason,omitempty"`
}

// leaseKey is the book key of message id pending for seat ("" for none).
func leaseKey(seat, id string) string {
	if seat == "" {
		return id
	}
	return seatKey(seat, id)
}

// automatic reports whether a lease by via counts as an attempt.
func automatic(via string) bool { return via != ViaHook }

// leaseBackoff is the retry delay after attempts automatic leases.
func leaseBackoff(attempts int) time.Duration {
	d := leaseBackoffMin
	for i := 1; i < attempts && d < leaseBackoffMax; i++ {
		d *= 2
	}
	return min(d, leaseBackoffMax)
}

type leaseBook struct {
	path  string
	mu    sync.Mutex
	m     map[string]*Lease
	dirty bool
}

func newLeaseBook(dir string) *leaseBook {
	b := &leaseBook{m: map[string]*Lease{}}
	if dir != "" {
		b.path = filepath.Join(dir, leaseFile)
	}
	return b
}

// load reads leases.json (missing or unreadable: none).
func (b *leaseBook) load() error {
	if b.path == "" {
		return nil
	}
	var list []*Lease
	if err := readJSON(b.path, &list); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, l := range list {
		if l != nil && l.ID != "" {
			b.m[leaseKey(l.Seat, l.ID)] = l
		}
	}
	return nil
}

// saveLocked writes the book; the caller holds b.mu.
func (b *leaseBook) saveLocked() error {
	b.dirty = false
	if b.path == "" {
		return nil
	}
	list := slices.Collect(maps.Values(b.m))
	slices.SortFunc(list, func(x, y *Lease) int { return strings.Compare(leaseKey(x.Seat, x.ID), leaseKey(y.Seat, y.ID)) })
	if err := writeJSON(b.path, list); err != nil {
		b.dirty = true
		return err
	}
	return nil
}

// touchLocked saves after a change of l: at once for an automatic lease,
// at the next flush for a hook's.
func (b *leaseBook) touchLocked(l *Lease) error {
	if automatic(l.Via) || l.State == LeaseFailed {
		return b.saveLocked()
	}
	b.dirty = true
	return nil
}

// flush saves a book whose last change was not saved yet.
func (b *leaseBook) flush() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.dirty {
		return nil
	}
	return b.saveLocked()
}

// get returns a copy of key's record.
func (b *leaseBook) get(key string) (Lease, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	l, ok := b.m[key]
	if !ok {
		return Lease{}, false
	}
	c := *l
	c.Fails = maps.Clone(l.Fails)
	return c, true
}

// blocked reports whether a lease of key by via must not be taken now: a
// failed message takes no automatic lease, one in its retry backoff no node
// wake (inbox, queue), one leased or running no other owner's.
func (b *leaseBook) blocked(key, owner, via string, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	l, ok := b.m[key]
	if !ok {
		return false
	}
	switch l.State {
	case LeaseFailed:
		return automatic(via)
	case LeaseRetry:
		return (via == ViaInbox || via == ViaQueue) && now.Before(l.NextAt)
	case LeaseLeased, LeaseRunning:
		return l.Owner != owner && automatic(via) && (l.Hold || l.Deadline.IsZero() || now.Before(l.Deadline))
	}
	return false
}

// take leases key (message id, seat) to owner by via until deadline. It is
// idempotent: the same owner and token again refreshes the deadline. An
// automatic lease counts an attempt (and a waiter wake). A lease another
// owner still holds is not taken; one past its deadline is revoked first.
func (b *leaseBook) take(id, seat, owner, via, token string, deadline, now time.Time) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	key := leaseKey(seat, id)
	l := b.m[key]
	if l == nil {
		l = &Lease{ID: id, Seat: seat, State: LeasePending}
		b.m[key] = l
	}
	switch l.State {
	case LeaseAcked:
		return false, nil
	case LeaseLeased, LeaseRunning:
		if l.Owner == owner && l.Token == token && l.Via == via {
			if l.State == LeaseLeased {
				l.Deadline = deadline
			}
			return true, b.touchLocked(l)
		}
		if l.Hold || (!l.Deadline.IsZero() && now.Before(l.Deadline)) {
			if l.Owner != owner {
				return false, nil
			}
		} else {
			b.revokeLocked(l, "deadline", now)
		}
	}
	if l.State == LeaseFailed && automatic(via) {
		return false, nil
	}
	if automatic(via) {
		l.Attempts++
	}
	if via == ViaWaiter {
		l.WaiterWakes++
		l.WaiterAt = now
	}
	l.State, l.Owner, l.Via, l.Token, l.Deadline, l.NextAt, l.Hold, l.At = LeaseLeased, owner, via, token, deadline, time.Time{}, false, now
	return true, b.touchLocked(l)
}

// start moves owner's leased leases of ids to running on proof its turn has
// them (token "" matches any token), and to newOwner when set (a launch's
// session). It returns the message ids started, and running ones again.
func (b *leaseBook) start(owner, newOwner, token string, ids []string, now time.Time) ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	changed := false
	for _, l := range b.m {
		if l.Owner != owner || !slices.Contains(ids, l.ID) || (token != "" && l.Token != token) {
			continue
		}
		switch l.State {
		case LeaseRunning:
			out = append(out, l.ID)
		case LeaseLeased:
			l.State, l.Deadline, l.At = LeaseRunning, now.Add(runningHold), now
			if newOwner != "" {
				l.Owner = newOwner
			}
			out = append(out, l.ID)
			changed = true
		}
	}
	if !changed {
		return out, nil
	}
	return out, b.saveLocked()
}

// hold keeps ids running for session with no deadline, their ack pending
// (holdForAck).
func (b *leaseBook) hold(session string, ids []string, now time.Time) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, id := range ids {
		l := b.m[id]
		if l == nil {
			l = &Lease{ID: id, Via: ViaLaunch}
			b.m[id] = l
		}
		l.State, l.Owner, l.Deadline, l.Hold, l.At = LeaseRunning, session, time.Time{}, true, now
	}
	return b.saveLocked()
}

// ack marks the leases of ids acked: the node's (seat "") and, for seat, the
// seat's. A lease still leased by the acking owner was proven by that ack.
func (b *leaseBook) ack(seat string, ids []string, seatDone map[string]bool, now time.Time) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	save := false
	mark := func(key string) {
		l := b.m[key]
		if l == nil || l.State == LeaseAcked {
			return
		}
		l.State, l.Deadline, l.NextAt, l.Hold, l.At = LeaseAcked, time.Time{}, time.Time{}, false, now
		save = save || automatic(l.Via)
		b.dirty = true
	}
	for _, id := range ids {
		mark(id)
		if seat != "" && seatDone[id] {
			mark(leaseKey(seat, id))
		}
	}
	if !save {
		return nil
	}
	return b.saveLocked()
}

// revokeLocked ends l's lease: back to retry with a backoff and one failure of
// its owner, failed after maxLeaseAttempts; a hook's goes back to pending
// without either. It reports whether l is failed now. The caller holds b.mu.
func (b *leaseBook) revokeLocked(l *Lease, reason string, now time.Time) bool {
	if l.State != LeaseLeased && l.State != LeaseRunning {
		return false
	}
	l.Reason, l.Deadline, l.Hold, l.At = reason, time.Time{}, false, now
	if !automatic(l.Via) {
		l.State = LeasePending
		if l.Attempts > 0 {
			l.State = LeaseRetry
		}
		return false
	}
	if l.Fails == nil {
		l.Fails = map[string]int{}
	}
	l.Fails[l.Owner]++
	if l.Attempts >= maxLeaseAttempts {
		l.State = LeaseFailed
		return true
	}
	l.State, l.NextAt = LeaseRetry, now.Add(leaseBackoff(l.Attempts))
	return false
}

// revoke ends owner's leases of ids (all of owner's when ids is nil) for
// reason. It returns the message ids now failed.
func (b *leaseBook) revoke(owner string, ids []string, reason string, now time.Time) ([]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var failed []string
	changed := false
	for _, l := range b.m {
		if l.Owner != owner || (ids != nil && !slices.Contains(ids, l.ID)) || (l.State != LeaseLeased && l.State != LeaseRunning) {
			continue
		}
		if b.revokeLocked(l, reason, now) {
			failed = append(failed, l.ID)
		}
		changed = true
	}
	if !changed {
		return nil, nil
	}
	return failed, b.saveLocked()
}

// fail ends owner's leases of ids as failed (a turn that started and failed:
// it may have acted in part, so nothing delivers it automatically again).
func (b *leaseBook) fail(owner string, ids []string, reason string, now time.Time) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, l := range b.m {
		if l.Owner == owner && slices.Contains(ids, l.ID) && (l.State == LeaseLeased || l.State == LeaseRunning) {
			if l.Fails == nil {
				l.Fails = map[string]int{}
			}
			l.Fails[l.Owner]++
			l.State, l.Reason, l.Deadline, l.Hold, l.At = LeaseFailed, reason, time.Time{}, false, now
		}
	}
	return b.saveLocked()
}

// fails is how often owner's leases of key failed.
func (b *leaseBook) fails(key, owner string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if l := b.m[key]; l != nil {
		return l.Fails[owner]
	}
	return 0
}

// passedOver reports whether owner failed key maxOwnerFails times.
func (b *leaseBook) passedOver(key, owner string) bool {
	return owner != "" && b.fails(key, owner) >= maxOwnerFails
}

// leaseEnd is one lease the sweep ends: its owner, message id and why.
type leaseEnd struct {
	owner, id, reason string
}

// due lists the leases that must end now: past their deadline, or owned by a
// session that is not live (gone(owner) true).
func (b *leaseBook) due(now time.Time, gone func(owner string) bool) []leaseEnd {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []leaseEnd
	for _, l := range b.m {
		if (l.State != LeaseLeased && l.State != LeaseRunning) || l.Hold {
			continue
		}
		switch {
		case gone(l.Owner):
			out = append(out, leaseEnd{l.Owner, l.ID, "session_gone"})
		case !l.Deadline.IsZero() && !now.Before(l.Deadline):
			out = append(out, leaseEnd{l.Owner, l.ID, "deadline"})
		}
	}
	return out
}

// prune drops the records of messages no longer unread (unread(key) false)
// after leaseDoneKeep, and every record after leaseKeep.
func (b *leaseBook) prune(now time.Time, unread func(key string) bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	changed := false
	for k, l := range b.m {
		if l.Hold && unread(k) {
			continue
		}
		if now.Sub(l.At) > leaseKeep || (!unread(k) && now.Sub(l.At) > leaseDoneKeep) {
			delete(b.m, k)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return b.saveLocked()
}

// all returns copies of every record.
func (b *leaseBook) all() map[string]Lease {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[string]Lease, len(b.m))
	for k, l := range b.m {
		out[k] = *l
	}
	return out
}
