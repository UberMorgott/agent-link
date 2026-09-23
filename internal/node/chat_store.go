package node

import (
	"errors"
	"fmt"
	"io/fs"
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
//	chats/<chat id>/runs.json            automatic runs on this node: root id -> request id
//	chat_views.json                      ChatView per chat id, this node only
//
// Status updates are not kept: only the latest one per job, in memory.
type chatStore struct {
	dir string

	mu    sync.Mutex
	chats map[string]*chatState
	byMsg map[string]string // message id -> chat id
	views map[string]ChatView
}

type chatState struct {
	chat  Chat
	msgs  []chatRecord   // by Seq
	index map[string]int // message id -> position in msgs
	runs  map[string]string
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
}

var errChatMismatch = errors.New("chat participants or area differ from the stored chat")

func openChatStore(dir string) (*chatStore, error) {
	root := filepath.Join(dir, "chats")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	cs := &chatStore{dir: dir, chats: map[string]*chatState{}, byMsg: map[string]string{}, views: map[string]ChatView{}}
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
		if err := readJSON(filepath.Join(root, e.Name(), "runs.json"), &st.runs); err != nil && !errors.Is(err, fs.ErrNotExist) {
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
		}
		cs.chats[st.chat.ID] = st
	}
	return cs, nil
}

func newChatState() *chatState {
	return &chatState{index: map[string]int{}, runs: map[string]string{}, jobs: map[string]Message{}, done: map[string]bool{}}
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
// participants and area. It reports whether c is new.
func (cs *chatStore) ensure(c Chat) (bool, error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if st := cs.chats[c.ID]; st != nil {
		if !slices.Equal(st.chat.Participants, c.Participants) || st.chat.Area != c.Area {
			return false, errChatMismatch
		}
		return false, nil
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
func (cs *chatStore) add(m Message) (chatRecord, bool, bool, error) {
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
	r := chatRecord{Seq: seq, Message: m, ReceivedAt: time.Now().UTC()}
	if err := writeJSON(filepath.Join(cs.chatDir(m.ChatID), "messages", m.ID+".json"), r); err != nil {
		return chatRecord{}, false, false, err
	}
	st.index[m.ID] = len(st.msgs)
	st.msgs = append(st.msgs, r)
	cs.byMsg[m.ID] = m.ChatID
	st.noteDone(m)
	closed := false
	if m.Kind == KindChatClose && (st.chat.CloseID == "" || m.ID < st.chat.CloseID) {
		c := st.chat
		closed = c.CloseID == ""
		c.CloseID, c.ClosedBy, c.ClosedAt = m.ID, m.From, m.CreatedAt
		if err := writeJSON(filepath.Join(cs.chatDir(m.ChatID), "chat.json"), c); err != nil {
			return r, true, false, err
		}
		st.chat = c
	}
	return r, true, closed, nil
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
	if m.JobStatus == JobCompleted || m.JobStatus == JobFailed {
		// A job that ends without a reply of its own (answered another way).
		st.done[key] = true
		delete(st.jobs, key)
		return true
	}
	if old, ok := st.jobs[key]; ok {
		oldSeq, newSeq := activitySeq(old), activitySeq(m)
		if newSeq < oldSeq || (newSeq == oldSeq && m.CreatedAt.Before(old.CreatedAt)) {
			return false
		}
	}
	st.jobs[key] = m
	return true
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

// claimRun records the automatic run for request id under root. It reports
// false when another request of the same root already ran here; the same
// request is claimed again (a resent duplicate).
func (cs *chatStore) claimRun(chatID, root, id string) (bool, error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	st := cs.chats[chatID]
	if st == nil {
		return false, fmt.Errorf("%w %s", ErrUnknownChat, chatID)
	}
	if got, ok := st.runs[root]; ok {
		return got == id, nil
	}
	st.runs[root] = id
	if err := writeJSON(filepath.Join(cs.chatDir(chatID), "runs.json"), st.runs); err != nil {
		delete(st.runs, root)
		return false, err
	}
	return true, nil
}
