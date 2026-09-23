package node

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

// chatStore persists chats as JSON files under the data directory:
//
//	chats/<chat id>/chat.json            Chat
//	chats/<chat id>/messages/<id>.json   chatRecord: a message with its local sequence number
//	chats/<chat id>/held.json            held statuses (JobHeld): author/request id -> status
//	chat_views.json                      ChatView per chat id, this node only
//
// Status updates are not kept: only the latest one per job, in memory, except
// held ones, which never repeat.
type chatStore struct {
	dir string

	mu    sync.Mutex
	chats map[string]*chatState
	byMsg map[string]string // message id -> chat id
	views map[string]ChatView
	// gens is the open generation of each conversation key of keyed chats:
	// the highest generation seen, plus one when that one is closed. It is
	// rebuilt from the chats on open.
	gens map[string]uint32
	// replies: message id -> participants that replied to it (reply_to), from
	// any chat, with the time of their first reply.
	replies map[string]map[string]time.Time
}

type chatState struct {
	chat  Chat
	msgs  []chatRecord   // by Seq
	index map[string]int // message id -> position in msgs
	// jobs is the latest status update per job, keyed by author and request
	// id; a job's final reply removes it (done).
	jobs map[string]Message
	done map[string]bool
}

// chatRecord is one chat message as stored on this node. Seq numbers the
// messages of a chat in the order this node stored them.
type chatRecord struct {
	Seq        uint64    `json:"seq"`
	Message    Message   `json:"message"`
	ReceivedAt time.Time `json:"received_at"`
	// Unread: this node's sessions have not acknowledged the message yet (set
	// for new messages of other nodes, and for this node's own messages a
	// person wrote); ReadAt is when one did. Records stored before unread
	// tracking have neither and count as read.
	Unread bool      `json:"unread,omitempty"`
	ReadAt time.Time `json:"read_at,omitzero"`
	// Assigned is who on this node answers a request that asks it: "worker" or
	// "session:<id>", set once (ClaimRun, Ack).
	Assigned string `json:"assigned,omitempty"`
	// Receipts: on this node's own messages, the latest receipt per recipient.
	Receipts map[string]Receipt `json:"receipts,omitempty"`
}

var errChatMismatch = errors.New("chat participants or area differ from the stored chat")

func openChatStore(dir string) (*chatStore, error) {
	root := filepath.Join(dir, "chats")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	cs := &chatStore{dir: dir, chats: map[string]*chatState{}, byMsg: map[string]string{}, views: map[string]ChatView{},
		gens: map[string]uint32{}, replies: map[string]map[string]time.Time{}}
	if err := readJSON(filepath.Join(dir, "chat_views.json"), &cs.views); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() || !validID(e.Name()) {
			continue
		}
		st := newChatState()
		if err := readJSON(filepath.Join(root, e.Name(), "chat.json"), &st.chat); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, err
		}
		if err := readJSON(filepath.Join(root, e.Name(), "held.json"), &st.jobs); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		files, err := jsonFiles(filepath.Join(root, e.Name(), "messages"))
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			var r chatRecord
			if err := readJSON(f, &r); err != nil {
				return nil, err
			}
			st.msgs = append(st.msgs, r)
		}
		slices.SortFunc(st.msgs, func(a, b chatRecord) int { return compareSeq(a.Seq, b.Seq) })
		for i, r := range st.msgs {
			st.index[r.Message.ID] = i
			cs.byMsg[r.Message.ID] = st.chat.ID
			st.noteDone(r.Message)
			st.clearHeld(r.Message)
			cs.noteReplyLocked(r.Message)
		}
		cs.chats[st.chat.ID] = st
		cs.noteGenLocked(st.chat)
	}
	return cs, nil
}

// noteGenLocked advances the open generation of c's key: to c's generation,
// or past it when c is closed. The caller holds cs.mu.
func (cs *chatStore) noteGenLocked(c Chat) {
	if !c.Keyed() {
		return
	}
	g := c.Gen
	if c.Closed() {
		g++
	}
	k := c.key()
	cs.gens[k] = max(cs.gens[k], g)
}

// gen returns the open generation of a conversation key.
func (cs *chatStore) gen(key string) uint32 {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.gens[key]
}

// noteReplyLocked indexes m as a reply of its author. The caller holds cs.mu.
func (cs *chatStore) noteReplyLocked(m Message) {
	if m.Kind != "" || m.ReplyTo == "" {
		return
	}
	by := cs.replies[m.ReplyTo]
	if by == nil {
		by = map[string]time.Time{}
		cs.replies[m.ReplyTo] = by
	}
	if at, ok := by[m.From]; !ok || m.CreatedAt.Before(at) {
		by[m.From] = m.CreatedAt
	}
}

// repliedBy returns who replied to message id, with when.
func (cs *chatStore) repliedBy(id string) map[string]time.Time {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return maps.Clone(cs.replies[id])
}

func newChatState() *chatState {
	return &chatState{index: map[string]int{}, jobs: map[string]Message{}, done: map[string]bool{}}
}

func compareSeq(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func (cs *chatStore) chatDir(id string) string { return filepath.Join(cs.dir, "chats", id) }

// ensure stores c unless its chat is known. A known chat must have the same
// participants and area (and in a project the same pinned ids and mode). It reports whether c is new.
func (cs *chatStore) ensure(c Chat) (bool, error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if st := cs.chats[c.ID]; st != nil {
		// An older peer sends no generation (0): the id already names it.
		if !slices.Equal(st.chat.Participants, c.Participants) || st.chat.Area != c.Area || (c.Gen != 0 && st.chat.Gen != c.Gen) ||
			st.chat.Project != c.Project || st.chat.Mode != c.Mode || !slices.Equal(st.chat.ParticipantIDs, c.ParticipantIDs) {
			return false, errChatMismatch
		}
		return false, nil
	}
	if c.Gen != 0 && !c.Keyed() {
		return false, errChatMismatch // a generation belongs to a keyed chat only
	}
	c.CloseID, c.ClosedBy, c.ClosedAt = "", "", time.Time{}
	if err := os.MkdirAll(filepath.Join(cs.chatDir(c.ID), "messages"), 0o700); err != nil {
		return false, err
	}
	if err := writeJSON(filepath.Join(cs.chatDir(c.ID), "chat.json"), c); err != nil {
		return false, err
	}
	st := newChatState()
	st.chat = c
	cs.chats[c.ID] = st
	cs.noteGenLocked(c)
	return true, nil
}

func (cs *chatStore) get(id string) (Chat, bool) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	st := cs.chats[id]
	if st == nil {
		return Chat{}, false
	}
	return st.chat, true
}

// add stores m, a message or a control message of a known chat, unless its id
// is stored already. A close message closes the chat; of concurrent closes the
// smallest message id wins everywhere. It returns the stored record (the
// earlier one for a duplicate), whether m is new and whether it closed the chat.
func (cs *chatStore) add(m Message) (chatRecord, bool, bool, error) { return cs.put(m, false) }

// put is add that stores a new message as unread for this node's sessions.
func (cs *chatStore) put(m Message, unread bool) (chatRecord, bool, bool, error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	st := cs.chats[m.ChatID]
	if st == nil {
		return chatRecord{}, false, false, fmt.Errorf("%w %s", ErrUnknownChat, m.ChatID)
	}
	if i, ok := st.index[m.ID]; ok {
		return st.msgs[i], false, false, nil
	}
	var seq uint64 = 1
	if len(st.msgs) > 0 {
		seq = st.msgs[len(st.msgs)-1].Seq + 1
	}
	r := chatRecord{Seq: seq, Message: m, ReceivedAt: time.Now().UTC(), Unread: unread && m.Kind == ""}
	if err := writeJSON(filepath.Join(cs.chatDir(m.ChatID), "messages", m.ID+".json"), r); err != nil {
		return chatRecord{}, false, false, err
	}
	st.index[m.ID] = len(st.msgs)
	st.msgs = append(st.msgs, r)
	cs.byMsg[m.ID] = m.ChatID
	st.noteDone(m)
	cs.noteReplyLocked(m)
	if st.clearHeld(m) {
		cs.saveHeldLocked(st)
	}
	closed := false
	if m.Kind == KindChatClose && (st.chat.CloseID == "" || m.ID < st.chat.CloseID) {
		c := st.chat
		closed = c.CloseID == ""
		c.CloseID, c.ClosedBy, c.ClosedAt = m.ID, m.From, m.CreatedAt
		if err := writeJSON(filepath.Join(cs.chatDir(m.ChatID), "chat.json"), c); err != nil {
			return r, true, false, err
		}
		st.chat = c
		cs.noteGenLocked(c)
	}
	return r, true, closed, nil
}

// updateLocked rewrites the record of message id after fn changed it; fn
// reports whether it did. The caller holds cs.mu.
func (cs *chatStore) updateLocked(id string, fn func(r *chatRecord) bool) (chatRecord, bool, error) {
	st := cs.chats[cs.byMsg[id]]
	if st == nil {
		return chatRecord{}, false, nil
	}
	i := st.index[id]
	r := st.msgs[i]
	r.Receipts = maps.Clone(r.Receipts)
	if !fn(&r) {
		return st.msgs[i], false, nil
	}
	if err := writeJSON(filepath.Join(cs.chatDir(st.chat.ID), "messages", id+".json"), r); err != nil {
		return st.msgs[i], false, err
	}
	st.msgs[i] = r
	return r, true, nil
}

// markRead acknowledges message id for this node: it is read from now on, and
// a request that asks this node and has no one assigned yet goes to owner.
// It returns the record and whether it was unread.
func (cs *chatStore) markRead(id, owner, self string) (chatRecord, bool, error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	wasUnread := false
	r, _, err := cs.updateLocked(id, func(r *chatRecord) bool {
		changed := false
		if r.Unread && r.ReadAt.IsZero() {
			r.ReadAt, wasUnread, changed = time.Now().UTC(), true, true
		}
		if owner != "" && r.Assigned == "" && r.Message.Asks(self) {
			r.Assigned, changed = owner, true
		}
		return changed
	})
	return r, wasUnread, err
}

// claim assigns request id to owner unless someone is assigned already or a
// session read it before (a read request is the session's to answer). It
// reports whether owner holds it now, and whether it was unread.
func (cs *chatStore) claim(id, owner string) (ok, wasUnread bool, err error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	r, _, err := cs.updateLocked(id, func(r *chatRecord) bool {
		if r.Assigned != "" || (r.Unread && !r.ReadAt.IsZero()) {
			return false
		}
		r.Assigned, wasUnread = owner, r.Unread && r.ReadAt.IsZero()
		if wasUnread {
			r.ReadAt = time.Now().UTC()
		}
		return true
	})
	return err == nil && r.Assigned == owner, wasUnread, err
}

// applyReceipt records peer's receipt on this node's message id, unless an
// equal or later state is known. It reports whether anything changed.
func (cs *chatStore) applyReceipt(peer string, rc Receipt) (bool, error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	_, changed, err := cs.updateLocked(rc.ID, func(r *chatRecord) bool {
		if r.Message.Kind != "" || !slices.Contains(cs.chats[r.Message.ChatID].chat.Participants, peer) {
			return false
		}
		if old, ok := r.Receipts[peer]; ok && stateRank(old.State) >= stateRank(rc.State) {
			return false
		}
		if r.Receipts == nil {
			r.Receipts = map[string]Receipt{}
		}
		r.Receipts[peer] = rc
		return true
	})
	return changed, err
}

// unread returns the records this node's sessions have not acknowledged, with
// their chats, oldest first.
func (cs *chatStore) unread() []unreadRecord {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	var out []unreadRecord
	for _, st := range cs.chats {
		for _, r := range st.msgs {
			if r.Unread && r.ReadAt.IsZero() {
				out = append(out, unreadRecord{chat: st.chat, rec: r})
			}
		}
	}
	return out
}

type unreadRecord struct {
	chat Chat
	rec  chatRecord
}

// noteDone records a final reply: the job it ends shows no activity anymore.
func (st *chatState) noteDone(m Message) {
	if m.Kind == "" && m.ReplyTo != "" && (m.JobStatus == JobCompleted || m.JobStatus == JobFailed) {
		key := m.From + "/" + m.ReplyTo
		st.done[key] = true
		delete(st.jobs, key)
	}
}

// noteStatus keeps m as the latest status of its job unless the job already
// ended or m is older than the kept one. It reports whether m was kept.
func (cs *chatStore) noteStatus(m Message) bool {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	st := cs.chats[m.ChatID]
	if st == nil {
		return false
	}
	key := m.From + "/" + m.ReplyTo
	if st.done[key] {
		return false
	}
	old, had := st.jobs[key]
	wasHeld := had && old.JobStatus == JobHeld
	if m.JobStatus == JobCompleted || m.JobStatus == JobFailed {
		// A job that ends without a reply of its own (answered another way).
		st.done[key] = true
		delete(st.jobs, key)
		if wasHeld {
			cs.saveHeldLocked(st)
		}
		return true
	}
	if had && m.JobStatus != JobHeld && !wasHeld {
		oldSeq, newSeq := activitySeq(old), activitySeq(m)
		if newSeq < oldSeq || (newSeq == oldSeq && m.CreatedAt.Before(old.CreatedAt)) {
			return false
		}
	}
	if had && (wasHeld || m.JobStatus == JobHeld) && m.CreatedAt.Before(old.CreatedAt) {
		return false
	}
	st.jobs[key] = m
	if wasHeld || m.JobStatus == JobHeld {
		cs.saveHeldLocked(st)
	}
	return true
}

// clearHeld drops the held requests of m's author that m answers: the one it
// replies to, or all of them when a person wrote m (no JobStatus). Only a
// message written after the held status counts; both come from one node.
func (st *chatState) clearHeld(m Message) bool {
	if m.Kind != "" {
		return false
	}
	changed := false
	for key, j := range st.jobs {
		if j.JobStatus == JobHeld && j.From == m.From && (j.ReplyTo == m.ReplyTo || m.JobStatus == "") &&
			!m.CreatedAt.Before(j.CreatedAt) {
			delete(st.jobs, key)
			changed = true
		}
	}
	return changed
}

// saveHeldLocked persists st's held statuses: unlike queued and running ones
// they are never refreshed, so a restart must not lose them. A failed write
// only loses them on the next restart. The caller holds cs.mu.
func (cs *chatStore) saveHeldLocked(st *chatState) {
	held := map[string]Message{}
	for key, m := range st.jobs {
		if m.JobStatus == JobHeld {
			held[key] = m
		}
	}
	_ = writeJSON(filepath.Join(cs.chatDir(st.chat.ID), "held.json"), held)
}

func activitySeq(m Message) uint64 {
	if m.ActivityInfo == nil {
		return 0
	}
	return m.ActivityInfo.Seq
}

// message returns a stored message by id, from any chat.
func (cs *chatStore) message(id string) (chatRecord, bool) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	st := cs.chats[cs.byMsg[id]]
	if st == nil {
		return chatRecord{}, false
	}
	return st.msgs[st.index[id]], true
}

// chatSnapshot is a copy of one chat's state.
type chatSnapshot struct {
	chat Chat
	msgs []chatRecord
	jobs []Message
}

func (cs *chatStore) snapshot(id string) (chatSnapshot, bool) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	st := cs.chats[id]
	if st == nil {
		return chatSnapshot{}, false
	}
	return st.snapshotLocked(), true
}

func (st *chatState) snapshotLocked() chatSnapshot {
	s := chatSnapshot{chat: st.chat, msgs: slices.Clone(st.msgs)}
	for _, m := range st.jobs {
		s.jobs = append(s.jobs, m)
	}
	slices.SortFunc(s.jobs, func(a, b Message) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return s
}

func (cs *chatStore) all() []chatSnapshot {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	out := make([]chatSnapshot, 0, len(cs.chats))
	for _, st := range cs.chats {
		out = append(out, st.snapshotLocked())
	}
	return out
}

func (cs *chatStore) view(id string) (ChatView, bool) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	v, ok := cs.views[id]
	return v, ok
}

func (cs *chatStore) setView(id string, v ChatView) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	old, had := cs.views[id]
	cs.views[id] = v
	if err := writeJSON(filepath.Join(cs.dir, "chat_views.json"), cs.views); err != nil {
		if had {
			cs.views[id] = old
		} else {
			delete(cs.views, id)
		}
		return err
	}
	return nil
}
