package node

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// store persists messages as JSON files:
//
//	inbox/<id>.json          inbound record (message + delivered flag)
//	outbox/<peer>/<id>.json  queued until the peer ACKs
//	sent/<peer>/<id>.json    ACKed by the peer
//	dropped/<peer>-<id>/     a project member's queue for a node id that is gone (never sent)
//	areas.json               last areas each peer announced
//	members.json             the membership table, tombstones included
//	pake_seen.json           peer names that authenticated with the PAKE
//	node_id                  this node's random id
type store struct {
	dir string

	mu      sync.Mutex
	inbox   map[string]*inboxRecord
	changed chan struct{} // closed and replaced when a new inbound message arrives
}

type inboxRecord struct {
	Message    Message   `json:"message"`
	Delivered  bool      `json:"delivered"`
	ReceivedAt time.Time `json:"received_at"`
	// Unread and ReadAt track a plain message (no chat) for this node's
	// sessions like chatRecord does; chat messages are tracked there.
	Unread bool      `json:"unread,omitempty"`
	ReadAt time.Time `json:"read_at,omitzero"`
}

func openStore(dir string) (*store, error) {
	for _, sub := range []string{"inbox", "outbox", "sent"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			return nil, err
		}
	}
	s := &store{dir: dir, inbox: map[string]*inboxRecord{}, changed: make(chan struct{})}
	files, err := jsonFiles(filepath.Join(dir, "inbox"))
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		var r inboxRecord
		if err := readJSON(f, &r); err != nil {
			return nil, err
		}
		s.inbox[r.Message.ID] = &r
	}
	return s, nil
}

func (s *store) enqueue(peer string, m Message) error {
	dir := filepath.Join(s.dir, "outbox", peer)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return writeJSON(filepath.Join(dir, m.ID+".json"), m)
}

// dropOutbox moves peer's queued messages to dropped/<peer>-<oldID>/: they
// were for a node that left or re-joined as a new node, so they are kept but
// never sent. A second queue for the same old id gets a time suffix.
func (s *store) dropOutbox(peer, oldID string) error {
	src := filepath.Join(s.dir, "outbox", peer)
	if _, err := os.Stat(src); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	root := filepath.Join(s.dir, "dropped")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	dst := filepath.Join(root, peer+"-"+oldID)
	if _, err := os.Stat(dst); err == nil {
		dst += "-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	err := os.Rename(src, dst)
	for try := 0; err != nil && try < 3; try++ {
		// On Windows a file being read by the write loop blocks the move for a moment.
		time.Sleep(20 * time.Millisecond)
		err = os.Rename(src, dst)
	}
	return err
}

// pending returns the peer's queued messages, oldest first.
func (s *store) pending(peer string) ([]Message, error) {
	return readMessages(filepath.Join(s.dir, "outbox", peer))
}

// ack moves a queued message to sent. Unknown ids are ignored.
func (s *store) ack(peer, id string) error {
	if !validID(id) {
		return nil
	}
	sentDir := filepath.Join(s.dir, "sent", peer)
	if err := os.MkdirAll(sentDir, 0o700); err != nil {
		return err
	}
	err := os.Rename(filepath.Join(s.dir, "outbox", peer, id+".json"), filepath.Join(sentDir, id+".json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// delivery tells where the copy of message id for peer is: "queued" in the
// outbox, "sent" once ACKed, or "" when there is none.
func (s *store) delivery(peer, id string) string {
	for _, box := range []struct{ dir, status string }{{"sent", "sent"}, {"outbox", "queued"}} {
		if _, err := os.Stat(filepath.Join(s.dir, box.dir, peer, id+".json")); err == nil {
			return box.status
		}
	}
	return ""
}

// saveInbound persists m unless its id was already received. It reports
// whether the message is new.
func (s *store) saveInbound(m Message) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.inbox[m.ID]; ok {
		return false, nil
	}
	// Status updates are never handed to wait: they only refine the inbox view.
	r := &inboxRecord{Message: m, Delivered: m.Kind == KindStatus, ReceivedAt: time.Now().UTC(), Unread: m.Kind == "" && m.ChatID == ""}
	if err := writeJSON(s.inboxPath(m.ID), r); err != nil {
		return false, err
	}
	s.inbox[m.ID] = r
	close(s.changed)
	s.changed = make(chan struct{})
	return true, nil
}

// markRead acknowledges plain message id; found is false for an unknown id
// or a chat message (tracked by chatStore).
func (s *store) markRead(id string) (found, wasUnread bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.inbox[id]
	if r == nil || r.Message.ChatID != "" {
		return false, false, nil
	}
	if !r.Unread || !r.ReadAt.IsZero() {
		return true, false, nil
	}
	next := *r
	next.ReadAt = time.Now().UTC()
	if err := writeJSON(s.inboxPath(id), &next); err != nil {
		return true, false, err
	}
	*r = next
	return true, true, nil
}

// unreadPlain returns the plain messages this node's sessions have not acknowledged.
func (s *store) unreadPlain() []inboxRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []inboxRecord
	for _, r := range s.inbox {
		if r.Unread && r.ReadAt.IsZero() {
			out = append(out, *r)
		}
	}
	return out
}

// updates returns a channel that is closed when the next inbound message arrives.
func (s *store) updates() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.changed
}

// claimUndelivered marks all undelivered inbound messages delivered and returns
// them. A non-empty chat claims only that chat's messages and leaves the rest.
func (s *store) claimUndelivered(chat string) ([]Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var recs []*inboxRecord
	for _, r := range s.inbox {
		if !r.Delivered && (chat == "" || r.Message.ChatID == chat) {
			recs = append(recs, r)
		}
	}
	slices.SortFunc(recs, func(a, b *inboxRecord) int { return a.ReceivedAt.Compare(b.ReceivedAt) })
	out := make([]Message, 0, len(recs))
	for _, r := range recs {
		r.Delivered = true
		if err := writeJSON(s.inboxPath(r.Message.ID), r); err != nil {
			r.Delivered = false
			return out, err
		}
		out = append(out, r.Message)
	}
	return out, nil
}

// recent lists inbound, queued and sent messages, newest first. Status updates
// are folded into the outbound request they refer to; a request with a real
// (not failed) reply is answered, whatever failed reply came before or after.
// With chats, chat messages are listed too (the control API's inbox).
func (s *store) recent(limit int, chats bool) ([]Entry, error) {
	var out []Entry
	// progress[peer/request id]: the latest status update, the latest reply
	// and when anything about the request last arrived.
	type progress struct {
		status, reply *Message
		heard         time.Time
	}
	byRequest := map[string]*progress{}
	s.mu.Lock()
	for _, r := range s.inbox {
		m := r.Message
		if m.ChatID != "" && !chats { // chats have their own history (chatStore)
			continue
		}
		if m.ReplyTo != "" {
			key := m.From + "/" + m.ReplyTo
			p := byRequest[key]
			if p == nil {
				p = &progress{}
				byRequest[key] = p
			}
			if r.ReceivedAt.After(p.heard) {
				p.heard = r.ReceivedAt
			}
			slot := &p.reply
			if m.Kind == KindStatus {
				slot = &p.status
			}
			if better(*slot, &r.Message) {
				*slot = &r.Message
			}
		}
		if m.Kind == KindStatus {
			continue
		}
		status := "pending"
		if r.Delivered {
			status = "delivered"
		}
		out = append(out, Entry{Direction: "in", Status: status, Peer: m.From, Message: m})
	}
	s.mu.Unlock()
	// own[peer/request id]: this node's latest status update and whether it
	// replied, for an inbound request its own handler works on.
	type ownProgress struct {
		status  *Message
		replied bool
	}
	own := map[string]*ownProgress{}
	for _, box := range []struct{ dir, status string }{{"outbox", "queued"}, {"sent", "sent"}} {
		peers, err := os.ReadDir(filepath.Join(s.dir, box.dir))
		if err != nil {
			return nil, err
		}
		for _, p := range peers {
			if !p.IsDir() {
				continue
			}
			msgs, err := readMessages(filepath.Join(s.dir, box.dir, p.Name()))
			if err != nil {
				return nil, err
			}
			for _, m := range msgs {
				if m.ChatID != "" && (!chats || m.Kind == KindChatOpen || m.Kind == KindChatClose || m.Kind == KindChatMembers || m.Kind == KindReceipt) {
					continue
				}
				if m.ReplyTo != "" {
					key := p.Name() + "/" + m.ReplyTo
					o := own[key]
					if o == nil {
						o = &ownProgress{}
						own[key] = o
					}
					if m.Kind != KindStatus {
						o.replied = true
					} else if o.status == nil || m.CreatedAt.After(o.status.CreatedAt) {
						o.status = &m
					}
				}
				e := Entry{Direction: "out", Status: box.status, Peer: p.Name(), Message: m}
				if isRequest(m) {
					e.LastHeard = m.CreatedAt
				}
				if pr := byRequest[p.Name()+"/"+m.ID]; pr != nil && isRequest(m) {
					switch {
					case pr.reply != nil:
						e.JobStatus, e.Answer = pr.reply.JobStatus, pr.reply.Body
					case pr.status != nil:
						e.JobStatus, e.Activity = pr.status.JobStatus, pr.status.Activity
					}
					if pr.heard.After(e.LastHeard) {
						e.LastHeard = pr.heard
					}
				}
				if m.Kind == KindStatus {
					continue
				}
				out = append(out, e)
			}
		}
	}
	for i := range out {
		e := &out[i]
		if e.Direction != "in" || !isRequest(e.Message) {
			continue
		}
		if o := own[e.Peer+"/"+e.ID]; o != nil && !o.replied && o.status != nil &&
			(o.status.JobStatus == JobQueued || o.status.JobStatus == JobRunning) {
			e.JobStatus, e.Activity = o.status.JobStatus, o.status.Activity
		}
	}
	slices.SortFunc(out, func(a, b Entry) int { return b.CreatedAt.Compare(a.CreatedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// isRequest is Message.IsRequest that also counts a chat message which is
// not a reply (recent lists chat messages only when asked to).
func isRequest(m Message) bool { return m.ReplyTo == "" && m.Kind == "" }

// better reports whether next replaces cur as a request's reply (or status):
// a failed one never replaces one that did not fail, else the newer wins.
func better(cur, next *Message) bool {
	if cur == nil {
		return true
	}
	if curFailed, nextFailed := cur.JobStatus == JobFailed, next.JobStatus == JobFailed; curFailed != nextFailed {
		return curFailed
	}
	return next.CreatedAt.After(cur.CreatedAt)
}

func (s *store) loadAreas() (map[string][]string, error) {
	areas := map[string][]string{}
	err := readJSON(filepath.Join(s.dir, "areas.json"), &areas)
	if errors.Is(err, fs.ErrNotExist) {
		return areas, nil
	}
	return areas, err
}

func (s *store) saveAreas(areas map[string][]string) error {
	return writeJSON(filepath.Join(s.dir, "areas.json"), areas)
}

// nodeID returns the node's random id from node_id, creating it on first use.
func (s *store) nodeID() (string, error) {
	path := filepath.Join(s.dir, "node_id")
	data, err := os.ReadFile(filepath.Clean(path))
	if err == nil && validID(strings.TrimSpace(string(data))) {
		return strings.TrimSpace(string(data)), nil
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	id := newID()
	return id, os.WriteFile(path, []byte(id+"\n"), 0o600)
}

func (s *store) loadMembers() ([]Member, error) {
	var ms []Member
	err := readJSON(filepath.Join(s.dir, "members.json"), &ms)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return ms, err
}

func (s *store) saveMembers(ms []Member) error {
	return writeJSON(filepath.Join(s.dir, "members.json"), ms)
}

// loadPAKESeen returns the peer names that have had a PAKE session here.
func (s *store) loadPAKESeen() ([]string, error) {
	var names []string
	err := readJSON(filepath.Join(s.dir, "pake_seen.json"), &names)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return names, err
}

func (s *store) savePAKESeen(names []string) error {
	return writeJSON(filepath.Join(s.dir, "pake_seen.json"), names)
}

func (s *store) inboxPath(id string) string { return filepath.Join(s.dir, "inbox", id+".json") }

func readMessages(dir string) ([]Message, error) {
	files, err := jsonFiles(dir)
	if err != nil {
		return nil, err
	}
	msgs := make([]Message, 0, len(files))
	for _, f := range files {
		var m Message
		err := readJSON(f, &m)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			// On Windows a file being moved (ACKed) cannot be opened for a moment.
			time.Sleep(20 * time.Millisecond)
			err = readJSON(f, &m)
		}
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) { // ACKed concurrently
				continue
			}
			return nil, err
		}
		msgs = append(msgs, m)
	}
	slices.SortFunc(msgs, func(a, b Message) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return msgs, nil
}

func jsonFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			files = append(files, filepath.Join(dir, e.Name()))
		}
	}
	return files, nil
}

func readJSON(path string, v any) error {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// writeJSON writes atomically: temp file, fsync, rename over the target.
func writeJSON(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}
