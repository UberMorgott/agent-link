package node

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"slices"
	"time"
)

// The launch ladder's durable part (launch_state.json in the data directory):
//
//   - spent: messages a launch was confirmed or given up for (a desktop turn
//     that started and failed is needs_human), with the attempt events already
//     reported for them. After a restart they are still not launched for, and
//     those events are not reported again. Only the automatic launch is
//     suppressed: the hooks still deliver them to a live session (a person
//     decides). An entry goes once its message is read or after spentKeep.
//   - acks: desktop launches whose first turn succeeded but whose ack kept
//     failing (ackLaunched). The messages stay claimed by the opened session
//     (ackOnly: no other session, hook, wake or launch takes them) and the ack
//     is retried every launchAckRetry until it succeeds (launch_confirmed).
const (
	launchStateFile  = "launch_state.json"
	spentKeep        = 7 * 24 * time.Hour
	launchAckRetry   = 30 * time.Second
	launchPruneEvery = time.Minute
)

// spentMark is one spent message: when, and the events reported for it.
type spentMark struct {
	At     time.Time `json:"at"`
	Events []string  `json:"events,omitempty"`
}

// ackJob is the pending ack of a desktop launch's messages as Session's.
type ackJob struct {
	IDs     []string  `json:"ids"`
	Session string    `json:"session"`
	Area    string    `json:"area"`
	At      time.Time `json:"at"`
	next    time.Time // next try
}

type launchState struct {
	Spent map[string]spentMark `json:"spent"`
	Acks  []*ackJob            `json:"acks,omitempty"`
}

// loadLaunchState reads launch_state.json (missing: nothing) into n.deliv and
// restores the claims of the pending acks.
func (n *Node) loadLaunchState() error {
	d := n.deliv
	d.statePath = filepath.Join(n.cfg.DataDir, launchStateFile)
	var st launchState
	if err := readJSON(d.statePath, &st); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		n.log.Warn("launch state unreadable; starting over", "path", d.statePath, "err", err)
		return nil
	}
	d.mu.Lock()
	for id, m := range st.Spent {
		d.spent[id] = m
		for _, ev := range m.Events {
			d.sent[id+"|"+ev] = true
		}
	}
	d.acks = st.Acks
	d.mu.Unlock()
	r := n.sess
	r.claimMu.Lock()
	for _, j := range st.Acks {
		for _, id := range j.IDs {
			r.claims[id] = sessionClaim{session: j.Session, at: j.At, ackOnly: true}
		}
	}
	r.claimMu.Unlock()
	return nil
}

// saveLaunchStateLocked writes the durable part of d; the caller holds d.mu.
func (n *Node) saveLaunchStateLocked() {
	d := n.deliv
	if d.statePath == "" {
		return
	}
	if err := writeJSON(d.statePath, launchState{Spent: d.spent, Acks: d.acks}); err != nil {
		n.log.Warn("save launch state", "err", err)
	}
}

// spend marks ids spent at now with the events reported for them, durably.
func (n *Node) spend(ids []string, now time.Time, events ...string) {
	if len(ids) == 0 {
		return
	}
	d := n.deliv
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, id := range ids {
		m := d.spent[id]
		if m.At.IsZero() {
			m.At = now
		}
		for _, ev := range events {
			if !slices.Contains(m.Events, ev) {
				m.Events = append(m.Events, ev)
			}
		}
		d.spent[id] = m
	}
	n.saveLaunchStateLocked()
}

// isSpent reports whether id is spent; the caller holds d.mu.
func (d *deliveryState) isSpent(id string) bool {
	_, ok := d.spent[id]
	return ok
}

// holdForAck keeps ids, which session's first turn took, claimed by session
// until they are acknowledged, and queues their ack (retryLaunchAcks).
func (n *Node) holdForAck(area string, ids []string, session string, now time.Time) {
	r := n.sess
	r.claimMu.Lock()
	for _, id := range ids {
		r.claims[id] = sessionClaim{session: session, at: now, ackOnly: true}
	}
	r.claimMu.Unlock()
	d := n.deliv
	d.mu.Lock()
	for _, id := range ids {
		if _, ok := d.spent[id]; !ok {
			d.spent[id] = spentMark{At: now}
		}
	}
	d.acks = append(d.acks, &ackJob{IDs: slices.Clone(ids), Session: session, Area: area, At: now, next: now.Add(launchAckRetry)})
	n.saveLaunchStateLocked()
	d.mu.Unlock()
}

// launchAckFunc is the ack of a desktop launch's messages.
func (n *Node) launchAckFunc() func(AckRequest) error {
	if n.launchAck != nil {
		return n.launchAck
	}
	return func(req AckRequest) error { _, err := n.Ack("", req); return err }
}

// retryLaunchAcks tries the pending acks that are due; one that succeeds is
// launch_confirmed.
func (n *Node) retryLaunchAcks(ctx context.Context, now time.Time) {
	d := n.deliv
	d.mu.Lock()
	var due []*ackJob
	for _, j := range d.acks {
		if !now.Before(j.next) {
			due = append(due, j)
		}
	}
	d.mu.Unlock()
	ack := n.launchAckFunc()
	for _, j := range due {
		if ctx.Err() != nil {
			return
		}
		err := ack(AckRequest{IDs: j.IDs, SessionID: j.Session})
		d.mu.Lock()
		if err != nil {
			j.next = now.Add(launchAckRetry)
			d.mu.Unlock()
			n.log.Warn("acknowledging an opened session's messages failed again", "area", j.Area, "session", j.Session, "err", err)
			continue
		}
		d.acks = slices.DeleteFunc(d.acks, func(o *ackJob) bool { return o == j })
		n.saveLaunchStateLocked()
		d.mu.Unlock()
		n.log.Info("opened session's messages acknowledged", "area", j.Area, "session", j.Session)
		n.report(j.IDs, AttemptLaunchConfirmed)
	}
}

// pruneLaunchState drops the spent messages that are read (or gone) or older
// than spentKeep, at most every launchPruneEvery.
func (n *Node) pruneLaunchState(now time.Time) {
	d := n.deliv
	d.mu.Lock()
	if len(d.spent) == 0 || now.Sub(d.pruned) < launchPruneEvery {
		d.mu.Unlock()
		return
	}
	d.pruned = now
	d.mu.Unlock()
	unread := map[string]bool{}
	for _, p := range n.store.unreadPlain() {
		unread[p.Message.ID] = true
	}
	for _, u := range n.chats.unread() {
		unread[u.rec.Message.ID] = true
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	pending := map[string]bool{}
	for _, j := range d.acks {
		for _, id := range j.IDs {
			pending[id] = true
		}
	}
	changed := false
	for id, m := range d.spent {
		if (!unread[id] && !pending[id]) || now.Sub(m.At) > spentKeep {
			delete(d.spent, id)
			changed = true
		}
	}
	if changed {
		n.saveLaunchStateLocked()
	}
}

// launchMaintain runs the ladder's upkeep: pending acks and pruning.
func (n *Node) launchMaintain(ctx context.Context, now time.Time) {
	n.retryLaunchAcks(ctx, now)
	n.pruneLaunchState(now)
}
