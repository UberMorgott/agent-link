package node

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Chat history retention (config.HistoryDays, HistoryKeep; the app sets
// them from settings). On the slow GC tick (attachSweepLoop: at start and
// every attSweepEvery) a chat drops its messages that are
//
//   - older than HistoryDays (by when this node stored them), and
//   - not among its newest HistoryKeep, and
//   - not waiting for anyone: unread, pending for a seat, held or leased for
//     delivery, needing a person (a failed lease, an orphaned reply, a
//     request a held status waits on), or this node's own message still
//     queued for a peer.
//
// A message becomes busy again only while it is unread, which the chat store
// checks under its lock as it drops a record. A reply to a dropped message
// routes like a new message (its asker's session is not known any more).
// The inbox dedup record of a dropped message goes too: a peer resends only
// what this node never ACKed, and it ACKs on storing.
//
// The inbox records and sent copies of what the chat store no longer has
// (a dropped message, a status update, a control or plain message) go by
// age alone, an unread plain message excepted. The blobs a dropped message
// named go with the next attachment sweep (OrphanAge). HistoryDays 0 keeps
// everything.

// pruneHistory applies the retention above at now.
func (n *Node) pruneHistory(now time.Time) {
	days := n.cfg.HistoryDays
	if days <= 0 {
		return
	}
	cutoff := now.Add(-time.Duration(days) * 24 * time.Hour)
	busy := n.historyBusy()
	removed, err := n.chats.prune(cutoff, n.cfg.HistoryKeep, func(chat Chat, r chatRecord) bool {
		if busy[r.Message.ID] || (r.Unread && r.ReadAt.IsZero()) || r.Orphaned != "" {
			return true
		}
		if r.Message.From != n.cfg.Node {
			return false
		}
		return slices.ContainsFunc(chat.Participants, func(p string) bool { return n.store.queued(p, r.Message.ID) })
	})
	if err != nil {
		n.log.Warn("prune chat history", "err", err)
	}
	gone, err := n.store.pruneOld(cutoff, func(id string) bool {
		_, ok := n.chats.message(id)
		return ok || busy[id]
	})
	if err != nil {
		n.log.Warn("prune inbox and sent copies", "err", err)
	}
	if len(removed) > 0 || gone > 0 {
		n.log.Info("old history pruned", "messages", len(removed), "copies", gone, "days", days, "keep", n.cfg.HistoryKeep)
		n.changed("messages")
	}
}

// historyBusy is the set of message ids delivery still needs: pending for a
// seat, held by a claim or a seat's mark, or with a lease record that is
// leased, running, held or failed.
func (n *Node) historyBusy() map[string]bool {
	busy := map[string]bool{}
	st := n.seats
	st.mu.Lock()
	for _, s := range st.seats {
		for _, p := range s.Pending {
			busy[p.ID] = true
		}
	}
	for k := range st.marks {
		busy[k[strings.LastIndexByte(k, '/')+1:]] = true
	}
	st.mu.Unlock()
	r := n.sess
	r.claimMu.Lock()
	for id := range r.claims {
		busy[id] = true
	}
	r.claimMu.Unlock()
	for _, l := range n.leases.all() {
		if l.State == LeaseLeased || l.State == LeaseRunning || l.Hold || l.Failed {
			busy[l.ID] = true
		}
	}
	return busy
}

// prune drops from every chat the records older than cutoff (ReceivedAt)
// that are not among its newest keep, not a request a held status waits on
// (JobHeld: a person decides) and not kept(chat, record), with their files.
// It returns the ids dropped.
func (cs *chatStore) prune(cutoff time.Time, keep int, kept func(Chat, chatRecord) bool) ([]string, error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	var removed []string
	var dropped []Message // the replies dropped
	var errs []error
	for _, st := range cs.chats {
		old := len(st.msgs) - max(keep, 1) // the newest stays: it numbers the next (put)
		if old <= 0 {
			continue
		}
		held := map[string]bool{}
		for _, j := range st.jobs {
			if j.JobStatus == JobHeld {
				held[j.ReplyTo] = true
			}
		}
		drop := map[string]bool{}
		for _, r := range st.msgs[:old] {
			if !r.ReceivedAt.Before(cutoff) || held[r.Message.ID] || kept(st.chat, r) {
				continue
			}
			err := os.Remove(filepath.Join(cs.chatDir(st.chat.ID), "messages", r.Message.ID+".json"))
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
				continue
			}
			drop[r.Message.ID] = true
		}
		if len(drop) == 0 {
			continue
		}
		var replies []Message // dropped replies: their index entries are rebuilt
		st.msgs = slices.DeleteFunc(st.msgs, func(r chatRecord) bool {
			if drop[r.Message.ID] && r.Message.Kind == "" && r.Message.ReplyTo != "" {
				replies = append(replies, r.Message)
			}
			return drop[r.Message.ID]
		})
		clear(st.index)
		for i, r := range st.msgs {
			st.index[r.Message.ID] = i
		}
		for id := range drop {
			delete(cs.byMsg, id)
			delete(cs.replies, id)
			removed = append(removed, id)
		}
		dropped = append(dropped, replies...)
	}
	cs.forgetRepliesLocked(dropped)
	return removed, errors.Join(errs...)
}

// forgetRepliesLocked drops the dropped replies from the reply index as a
// reopen would rebuild it: an author's entry for what it answered goes
// unless another stored reply of theirs answers it too (then the earliest
// counts). The caller holds cs.mu.
func (cs *chatStore) forgetRepliesLocked(dropped []Message) {
	if len(dropped) == 0 {
		return
	}
	pairs := map[[2]string]bool{}
	for _, m := range dropped {
		pairs[[2]string{m.ReplyTo, m.From}] = true
		if by := cs.replies[m.ReplyTo]; by != nil {
			delete(by, m.From)
		}
	}
	for _, st := range cs.chats {
		for _, r := range st.msgs {
			if pairs[[2]string{r.Message.ReplyTo, r.Message.From}] {
				cs.noteReplyLocked(r.Message)
			}
		}
	}
	for _, m := range dropped {
		if len(cs.replies[m.ReplyTo]) == 0 {
			delete(cs.replies, m.ReplyTo)
		}
	}
}

// pruneOld drops the inbox records received and the sent copies written
// before cutoff whose message is not kept(id), an unread plain message
// excepted. kept is called under s.mu (the chat store's lock may be taken
// inside it, as full does). It returns how many it dropped.
func (s *store) pruneOld(cutoff time.Time, kept func(id string) bool) (int, error) {
	var errs []error
	n := 0
	// Under s.mu: a record and its file go together (saveInbound, markRead).
	s.mu.Lock()
	for id, r := range s.inbox {
		if !r.ReceivedAt.Before(cutoff) || (r.Unread && r.ReadAt.IsZero()) || kept(id) {
			continue
		}
		if err := os.Remove(s.inboxPath(id)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
			continue
		}
		delete(s.inbox, id)
		n++
	}
	s.mu.Unlock()
	peers, err := os.ReadDir(filepath.Join(s.dir, "sent"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		errs = append(errs, err)
	}
	for _, p := range peers {
		if !p.IsDir() {
			continue
		}
		dir := filepath.Join(s.dir, "sent", p.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, f := range files {
			id, ok := strings.CutSuffix(f.Name(), ".json")
			if !ok || f.IsDir() {
				continue
			}
			info, err := f.Info()
			if err != nil || !info.ModTime().Before(cutoff) || kept(id) {
				continue
			}
			if err := os.Remove(filepath.Join(dir, f.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
				continue
			}
			n++
		}
	}
	return n, errors.Join(errs...)
}
