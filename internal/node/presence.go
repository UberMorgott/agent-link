package node

import (
	"slices"
	"time"
)

// Presence tells a peer, for every area this node shares with it, whether a
// live agent session is attached here (and how it wakes) and whether the
// worker answers when none is: the sender's chat can then say what happens
// to a delivered, unread message. It is a presence frame, sent to peers with
// CapPresence when the session starts and whenever the state changes (at
// most once per presenceGap); older peers never get one, and would skip it.
// It is not stored: a peer's presence ends with its session.

const (
	// CapPresence: sends and reads presence frames.
	CapPresence = "presence-v1"
	// framePresence carries Frame.Presence.
	framePresence = "presence"
	// presenceGap is the least time between two presence frames on a session.
	presenceGap = 500 * time.Millisecond
	// maxPresenceAreas bounds a received presence frame.
	maxPresenceAreas = 256
)

// AreaPresence is the session state of a node for one area ("" is the
// working folder: direct messages and chats without a project there).
type AreaPresence struct {
	Area string `json:"area"`
	// Session is the wake mode of the live session that reads the area's
	// messages (WakeRewake when any does, else WakeNextEvent); empty: none.
	Session string `json:"session,omitempty"`
	// AutoAnswer: the worker answers requests while no session is live.
	AutoAnswer bool `json:"auto_answer,omitempty"`
}

// SetAutoAnswer tells the node whether its worker answers requests no live
// session takes, which is part of its presence. It must be set before Serve or Run.
func (n *Node) SetAutoAnswer(on bool) { n.autoAnswer = on }

// presenceFor is this node's presence for a peer with areas peerAreas: the
// working folder's, then every area both have, sorted.
func (n *Node) presenceFor(peerAreas []string) []AreaPresence {
	areas := []string{""}
	for _, a := range n.cfg.Areas {
		if a != "" && slices.Contains(peerAreas, a) && !slices.Contains(areas, a) {
			areas = append(areas, a)
		}
	}
	slices.Sort(areas[1:])
	r := n.sess
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]AreaPresence, 0, len(areas))
	for _, a := range areas {
		p := AreaPresence{Area: a, AutoAnswer: n.autoAnswer}
		want := n.localArea(a)
		for _, s := range r.sessions {
			if s.Area != want || !s.live(now) {
				continue
			}
			if s.Wake == WakeRewake {
				p.Session = WakeRewake
				break
			}
			p.Session = WakeNextEvent
		}
		out = append(out, p)
	}
	return out
}

// receivePresence keeps what a peer's presence frame says, for its session.
func (n *Node) receivePresence(pc *peerConn, list []AreaPresence) {
	if len(list) > maxPresenceAreas {
		return
	}
	got := make(map[string]AreaPresence, len(list))
	for _, p := range list {
		switch p.Session {
		case "", WakeRewake, WakeNextEvent:
		default:
			continue
		}
		if len(p.Area) <= 128 {
			got[p.Area] = p
		}
	}
	n.mu.Lock()
	same := pc.presence != nil && len(pc.presence) == len(got)
	for a, p := range got {
		same = same && pc.presence[a] == p
	}
	pc.presence = got
	n.mu.Unlock()
	if !same {
		n.changed("chats")
	}
}

// PeerPresence returns what peer last said of its session for area; ok is
// false while it is disconnected or has said nothing (an older version).
func (n *Node) PeerPresence(peer, area string) (AreaPresence, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	pc := n.conns[peer]
	if pc == nil || pc.presence == nil {
		return AreaPresence{}, false
	}
	p, ok := pc.presence[area]
	return p, ok
}

// presenceChanged makes every session's writer look at the presence again now.
func (n *Node) presenceChanged() {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, pc := range n.conns {
		pc.notify()
	}
}
