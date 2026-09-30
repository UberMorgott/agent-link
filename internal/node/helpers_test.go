package node

import (
	"io"
	"net"
	"time"
)

// Helpers only the tests use.

// add stores m like put, not unread for this node's sessions.
func (cs *chatStore) add(m Message) (chatRecord, bool, bool, error) { return cs.put(m, false) }

// fail records a failed handshake from ip and returns how long it is now blocked.
func (g *authGuard) fail(ip net.IP) time.Duration { return g.failIn(ip, "") }

// success forgets ip's failures.
func (g *authGuard) success(ip net.IP) { g.successIn(ip, "") }

// ReadClaudeStream reads the events of `claude -p --output-format
// stream-json` until the result: started gets the session id at the first
// event naming it. It returns the session id and the turn's error.
func ReadClaudeStream(r io.Reader, started func(string)) (string, error) {
	return readClaudeStream(r, started, nil, nil, nil)
}

// presenceFor is this node's presence for a peer with areas peerAreas.
func (n *Node) presenceFor(peerAreas []string) []AreaPresence {
	return n.presenceForCaps(peerAreas, false)
}

func (w *wire) sealed() bool { return w.out != nil }
