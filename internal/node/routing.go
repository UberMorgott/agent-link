package node

import (
	"fmt"
	"strings"
	"time"
)

// Routing of unread messages between this node's live sessions. Read state is
// the node's, but a message is delivered to one session only: several
// sessions in one folder all poll Unread, and a reply one of them asked for
// must not land in the others as a task. A message is for:
//
//   - the session assigned to answer it (Ack) or that claimed it (Claim);
//   - else the session that wrote the message it answers, following reply_to
//     back through this node's chat store (chatRecord.Session, recorded by
//     agentlink send inside the session; or a message assigned to a session);
//   - else no session in particular: the first session to Claim it takes it.
//
// Only live sessions count: a message for a session that is gone is anyone's.

// maxRouteHops bounds the walk up a reply_to chain.
const maxRouteHops = 16

// ClaimRequest is the body of POST /claim.
type ClaimRequest struct {
	IDs       []string `json:"ids"`
	SessionID string   `json:"session_id"`
	Folder    string   `json:"folder,omitempty"` // routing hint of the control API
}

// liveIDs is the set of live session ids.
func (r *sessionRegistry) liveIDs(now time.Time) map[string]bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]bool{}
	for id, s := range r.sessions {
		if s.live(now) {
			out[id] = true
		}
	}
	return out
}

// routeOf is the live session unread message id is for; "" when it is for no
// session in particular. rec is its chat record (nil for a plain message).
// The caller holds n.sess.claimMu.
func (n *Node) routeOf(id string, rec *chatRecord, live map[string]bool) string {
	if s := n.sess.claims[id]; live[s] {
		return s
	}
	if rec == nil {
		return ""
	}
	if s := assignedSession(*rec); live[s] {
		return s
	}
	parent := rec.Message.ReplyTo
	for range maxRouteHops {
		if parent == "" {
			break
		}
		p, ok := n.chats.message(parent)
		if !ok {
			break
		}
		if p.Message.From == n.cfg.Node && live[p.Session] {
			return p.Session
		}
		if s := assignedSession(p); live[s] {
			return s
		}
		parent = p.Message.ReplyTo
	}
	return ""
}

// assignedSession is the session a record is assigned to ("session:<id>").
func assignedSession(r chatRecord) string {
	s, _ := strings.CutPrefix(r.Assigned, "session:")
	if s == r.Assigned {
		return ""
	}
	return s
}

// Claim takes unread messages for delivery to one session, atomically: it
// returns the ids granted, those still unread and not for another live
// session (routeOf). A granted message stays that session's while it lives,
// until it is acknowledged; claiming again is granted again.
func (n *Node) Claim(req ClaimRequest) ([]string, error) {
	switch {
	case !validSessionID(req.SessionID):
		return nil, fmt.Errorf("%w: invalid session_id", ErrBadRequest)
	case len(req.IDs) > 1000:
		return nil, fmt.Errorf("%w: at most 1000 ids", ErrBadRequest)
	}
	r := n.sess
	r.claimMu.Lock()
	defer r.claimMu.Unlock()
	live := r.liveIDs(time.Now())
	plain := map[string]bool{}
	for _, p := range n.store.unreadPlain() {
		plain[p.Message.ID] = true
	}
	granted := []string{}
	for _, id := range req.IDs {
		var to string
		if rec, ok := n.chats.message(id); ok {
			if !rec.Unread || !rec.ReadAt.IsZero() || rec.Message.Kind != "" {
				continue
			}
			to = n.routeOf(id, &rec, live)
		} else if plain[id] {
			to = n.routeOf(id, nil, live)
		} else {
			continue
		}
		if to == "" || to == req.SessionID {
			r.claims[id] = req.SessionID
			granted = append(granted, id)
		}
	}
	return granted, nil
}
