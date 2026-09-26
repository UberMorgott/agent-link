package node

import (
	"slices"
	"strings"
	"time"
)

// The node's side of delivery leases (lease.go): the exported API a
// provider's liveness monitor drives (LeaseStart, LeaseRevoke), the view of
// every unread message's lease (Leases, GET /leases), the sweep that ends
// leases that lost their owner, and the restore after a restart.

// leasePruneEvery bounds how often leaseSweep prunes the book.
const leasePruneEvery = time.Minute

// LeaseStart moves owner's leased leases of ids to running: proof the
// owner's turn has them (a Codex app-server turn/started of that thread
// carrying the wake prompt, token its WakeMarker token; "" matches any). It
// returns the ids running now. Never call it on a queue or RPC accept.
func (n *Node) LeaseStart(owner, token string, ids []string) []string {
	out, err := n.leases.start(owner, "", token, ids, time.Now())
	if err != nil {
		n.log.Warn("save leases", "err", err)
	}
	if len(out) > 0 {
		n.changed("leases")
	}
	return out
}

// LeaseRevoke ends owner's leases of ids for reason (its wake failed, its
// session died, its turn never started): they go back to retry, eligible for
// another owner, or failed after maxLeaseAttempts (needs_human). The owner's
// claims of them are dropped, so another session's hooks may take them.
func (n *Node) LeaseRevoke(owner string, ids []string, reason string) {
	if len(ids) == 0 {
		return
	}
	n.revokeLeases(owner, ids, reason, true)
}

// revokeLeases revokes owner's leases of ids (all of owner's for nil),
// dropping its claims of them when drop, and reports the failed ones.
func (n *Node) revokeLeases(owner string, ids []string, reason string, drop bool) {
	failed, err := n.leases.revoke(owner, ids, reason, time.Now())
	if err != nil {
		n.log.Warn("save leases", "err", err)
	}
	if drop {
		n.dropClaims(owner, ids)
	}
	if len(failed) > 0 {
		n.log.Warn("delivery failed after every lease; a person decides", "owner", owner, "messages", len(failed))
		n.report(failed, AttemptNeedsHuman)
	}
	n.changed("leases")
}

// dropClaims drops owner's claims of ids (all of owner's for nil) but the
// ackOnly ones (a launched session's taken messages).
func (n *Node) dropClaims(owner string, ids []string) {
	r := n.sess
	r.claimMu.Lock()
	for id, c := range r.claims {
		if c.session == owner && !c.ackOnly && (ids == nil || slices.Contains(ids, id)) {
			delete(r.claims, id)
		}
	}
	r.claimMu.Unlock()
	n.seatUnclaim(owner, ids)
}

// Leases is the lease state of every unread message of this node, oldest
// first, a message with no lease as pending (and one per seat it is pending
// for).
func (n *Node) Leases() []LeaseView {
	book := n.leases.all()
	var out []LeaseView
	view := func(id, chat, seat string) {
		v := LeaseView{ID: id, ChatID: chat, Seat: seat, State: LeasePending}
		if l, ok := book[leaseKey(seat, id)]; ok {
			v.State, v.Owner, v.Via, v.Attempts, v.Deadline, v.NextAt, v.Reason = l.State, l.Owner, l.Via, l.Attempts, l.Deadline, l.NextAt, l.Reason
		}
		out = append(out, v)
	}
	type item struct {
		at             time.Time
		id, chat, seat string
	}
	var items []item
	for _, u := range n.chats.unread() {
		items = append(items, item{at: u.rec.ReceivedAt, id: u.rec.Message.ID, chat: u.rec.Message.ChatID})
	}
	for _, p := range n.store.unreadPlain() {
		items = append(items, item{at: p.ReceivedAt, id: p.Message.ID})
	}
	st := n.seats
	st.mu.Lock()
	for _, s := range st.seats {
		for _, p := range s.Pending {
			items = append(items, item{at: p.At, id: p.ID, seat: s.ID})
		}
	}
	st.mu.Unlock()
	slices.SortStableFunc(items, func(a, b item) int {
		if c := a.at.Compare(b.at); c != 0 {
			return c
		}
		return strings.Compare(leaseKey(a.seat, a.id), leaseKey(b.seat, b.id))
	})
	for _, it := range items {
		chat := it.chat
		if chat == "" && it.seat != "" {
			if rec, ok := n.chats.message(it.id); ok {
				chat = rec.Message.ChatID
			}
		}
		view(it.id, chat, it.seat)
	}
	if out == nil {
		out = []LeaseView{}
	}
	return out
}

// sessionOwner reports whether owner is a session id (not a launch's or a
// seat's).
func sessionOwner(owner string) bool {
	return owner != "" && !strings.HasPrefix(owner, "launch ") && !strings.HasPrefix(owner, seatOwnerPrefix)
}

// seatOwnerPrefix prefixes a seat's id as the owner of its turn's leases.
const seatOwnerPrefix = "seat:"

// seatOwner is the lease owner of seat's turns.
func seatOwner(seat string) string { return seatOwnerPrefix + seat }

// leaseSweep ends the leases that lost their owner (a session that is not
// live any more, or one silent on a queued wake for queuedOwnerMax) or passed
// their deadline without proof, saves what is not saved and prunes the book.
func (n *Node) leaseSweep(now time.Time) {
	live := n.sess.liveIDs(now)
	active := n.sess.activeAts()
	ends := n.leases.due(now, func(owner string) bool { return sessionOwner(owner) && !live[owner] },
		func(owner string) time.Time { return active[owner] })
	type group struct{ owner, reason string }
	byGroup := map[group][]string{}
	for _, e := range ends {
		g := group{e.owner, e.reason}
		byGroup[g] = append(byGroup[g], e.id)
	}
	for g, list := range byGroup {
		n.log.Info("delivery lease ended", "owner", g.owner, "reason", g.reason, "messages", len(list))
		// A lapsed or stale wake keeps its claim (and token): a late proof
		// acknowledges instead of delivering twice. A gone session's claims go.
		n.revokeLeases(g.owner, list, g.reason, g.reason != "deadline" && g.reason != "queued_stale")
	}
	for _, e := range n.leases.staleHolds(now) {
		n.releaseHold(e.owner, []string{e.id}, now)
	}
	if err := n.leases.flush(); err != nil {
		n.log.Warn("save leases", "err", err)
	}
	n.deliv.mu.Lock()
	due := now.Sub(n.deliv.leasePruned) >= leasePruneEvery
	if due {
		n.deliv.leasePruned = now
	}
	n.deliv.mu.Unlock()
	if !due {
		return
	}
	unread := map[string]bool{}
	for _, u := range n.chats.unread() {
		unread[u.rec.Message.ID] = true
	}
	for _, p := range n.store.unreadPlain() {
		unread[p.Message.ID] = true
	}
	st := n.seats
	st.mu.Lock()
	for _, s := range st.seats {
		for _, p := range s.Pending {
			unread[leaseKey(s.ID, p.ID)] = true
		}
	}
	st.mu.Unlock()
	if err := n.leases.prune(now, func(key string) bool { return unread[key] }); err != nil {
		n.log.Warn("save leases", "err", err)
	}
}

// releaseHold gives up session's held messages ids whose ack kept failing for
// leaseHoldMax (holdForAck): their lease is failed (needs_human), their ack
// job and ackOnly claims go, and the hooks may show them to a person again;
// nothing launches for them (spent, failed).
func (n *Node) releaseHold(session string, ids []string, now time.Time) {
	if err := n.leases.fail(session, ids, "ack_stuck", now); err != nil {
		n.log.Warn("save leases", "err", err)
	}
	d := n.deliv
	d.mu.Lock()
	for _, j := range d.acks {
		if j.Session == session {
			j.IDs = slices.DeleteFunc(j.IDs, func(id string) bool { return slices.Contains(ids, id) })
		}
	}
	d.acks = slices.DeleteFunc(d.acks, func(j *ackJob) bool { return len(j.IDs) == 0 })
	n.saveLaunchStateLocked()
	d.mu.Unlock()
	r := n.sess
	r.claimMu.Lock()
	for _, id := range ids {
		if c, ok := r.claims[id]; ok && c.ackOnly && c.session == session {
			delete(r.claims, id)
		}
	}
	r.claimMu.Unlock()
	n.log.Warn("an opened session's messages were never acknowledged; a person decides", "session", session, "messages", len(ids))
	n.report(ids, AttemptNeedsHuman)
	n.changed("leases")
}

// restoreLeases picks up the book after a restart: a session's wake lease
// gets its claim back (its token still proves; it lapses as it would have);
// a launch's or seat's turn died with the node: one that had started is
// failed (it may have acted), one that had not goes back to retry.
func (n *Node) restoreLeases(now time.Time) {
	book := n.leases.all()
	r := n.sess
	type end struct{ owner, id string }
	var failed, retry []end
	r.claimMu.Lock()
	for _, l := range book {
		if l.State != LeaseLeased && l.State != LeaseRunning {
			continue
		}
		switch {
		case !sessionOwner(l.Owner) && l.State == LeaseRunning && !l.Hold:
			failed = append(failed, end{l.Owner, l.ID})
		case !sessionOwner(l.Owner) && !l.Hold:
			retry = append(retry, end{l.Owner, l.ID})
		case l.Hold || l.Token == "" || l.Via == ViaHook:
		case l.Seat != "":
			st := n.seats
			st.mu.Lock()
			if _, ok := st.marks[seatKey(l.Seat, l.ID)]; !ok {
				st.marks[seatKey(l.Seat, l.ID)] = seatMark{at: l.At, wake: true, token: l.Token}
			}
			st.mu.Unlock()
		default:
			if _, ok := r.claims[l.ID]; !ok {
				r.claims[l.ID] = sessionClaim{session: l.Owner, at: l.At, wake: true, token: l.Token}
			}
		}
	}
	r.claimMu.Unlock()
	var needs []string
	for _, e := range failed {
		if err := n.leases.fail(e.owner, []string{e.id}, "restart", now); err != nil {
			n.log.Warn("save leases", "err", err)
		}
		needs = append(needs, e.id)
	}
	for _, e := range retry {
		f, err := n.leases.revoke(e.owner, []string{e.id}, "restart", now)
		if err != nil {
			n.log.Warn("save leases", "err", err)
		}
		needs = append(needs, f...)
	}
	n.report(needs, AttemptNeedsHuman)
}
