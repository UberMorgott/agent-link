package node

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// store persists messages as JSON files:
//
//	inbox/<id>.json          inbound record (message + delivered flag)
//	outbox/<peer>/<id>.json  queued until the peer ACKs
//	sent/<peer>/<id>.json    ACKed by the peer
//	areas.json               last areas each peer announced
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

// saveInbound persists m unless its id was already received. It reports
// whether the message is new.
func (s *store) saveInbound(m Message) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.inbox[m.ID]; ok {
		return false, nil
	}
	// Status updates are never handed to wait: they only refine the inbox view.
	r := &inboxRecord{Message: m, Delivered: m.Kind == KindStatus, ReceivedAt: time.Now().UTC()}
	if err := writeJSON(s.inboxPath(m.ID), r); err != nil {
		return false, err
	}
	s.inbox[m.ID] = r
	close(s.changed)
	s.changed = make(chan struct{})
	return true, nil
}

// updates returns a channel that is closed when the next inbound message arrives.
func (s *store) updates() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.changed
}

// claimUndelivered marks all undelivered inbound messages delivered and returns them.
func (s *store) claimUndelivered() ([]Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var recs []*inboxRecord
	for _, r := range s.inbox {
		if !r.Delivered {
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
// are folded into the outbound request they refer to.
func (s *store) recent(limit int) ([]Entry, error) {
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
			if *slot == nil || m.CreatedAt.After((*slot).CreatedAt) {
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
				e := Entry{Direction: "out", Status: box.status, Peer: p.Name(), Message: m}
				if m.IsRequest() {
					e.LastHeard = m.CreatedAt
				}
				if pr := byRequest[p.Name()+"/"+m.ID]; pr != nil && m.IsRequest() {
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
	slices.SortFunc(out, func(a, b Entry) int { return b.CreatedAt.Compare(a.CreatedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
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

func (s *store) inboxPath(id string) string { return filepath.Join(s.dir, "inbox", id+".json") }

func readMessages(dir string) ([]Message, error) {
	files, err := jsonFiles(dir)
	if err != nil {
		return nil, err
	}
	msgs := make([]Message, 0, len(files))
	for _, f := range files {
		var m Message
		if err := readJSON(f, &m); err != nil {
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
	data, err := os.ReadFile(path)
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
