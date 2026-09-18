package node

import (
	"bufio"
	"context"
	"errors"
	"net"
	"sync"
	"time"
)

const writeTimeout = 10 * time.Second

// frameHeartbeat is the liveness frame; older peers ignore unknown frame types.
const frameHeartbeat = "hb"

// peerConn is one authenticated connection to a peer.
type peerConn struct {
	peer   string
	dialer string // node name that opened the TCP connection
	areas  []string
	proto  int      // 0: an older peer that announced no version
	caps   []string // empty for an older peer
	c      net.Conn

	wmu  sync.Mutex
	kick chan struct{}
	done chan struct{}
	once sync.Once
}

func newPeerConn(h peerHello, dialer string, c net.Conn) *peerConn {
	return &peerConn{peer: h.name, dialer: dialer, areas: h.areas, proto: h.proto, caps: h.caps, c: c,
		kick: make(chan struct{}, 1), done: make(chan struct{})}
}

func (pc *peerConn) write(f frame) error {
	pc.wmu.Lock()
	defer pc.wmu.Unlock()
	_ = pc.c.SetWriteDeadline(time.Now().Add(writeTimeout))
	return writeFrame(pc.c, f)
}

func (pc *peerConn) close() {
	pc.once.Do(func() {
		close(pc.done)
		_ = pc.c.Close()
	})
}

func (pc *peerConn) notify() {
	select {
	case pc.kick <- struct{}{}:
	default:
	}
}

// preferred decides which of two live connections to the same peer survives.
// Both sides apply the same rule: keep the connection dialed by the
// lexicographically smaller node; a reconnect from the same dialer replaces the old one.
func preferred(self string, next, old *peerConn) bool {
	if next.dialer == old.dialer {
		return true
	}
	return next.dialer == min(self, next.peer)
}

// register makes pc the live connection for its peer, or reports false when
// an existing connection wins.
func (n *Node) register(pc *peerConn) bool {
	n.mu.Lock()
	old := n.conns[pc.peer]
	if old != nil && !preferred(n.cfg.Node, pc, old) {
		n.mu.Unlock()
		return false
	}
	n.conns[pc.peer] = pc
	n.known[pc.peer] = true
	n.problem = nil
	n.areas[pc.peer] = pc.areas
	areas := make(map[string][]string, len(n.areas))
	for k, v := range n.areas {
		areas[k] = v
	}
	n.mu.Unlock()
	if old != nil {
		old.close()
	}
	if err := n.store.saveAreas(areas); err != nil {
		n.log.Warn("save areas", "err", err)
	}
	n.log.Info("peer connected", "peer", pc.peer, "dialer", pc.dialer, "areas", pc.areas,
		"proto", pc.proto, "caps", pc.caps)
	return true
}

func (n *Node) unregister(pc *peerConn) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.conns[pc.peer] == pc {
		delete(n.conns, pc.peer)
		n.offline[pc.peer] = time.Now()
		n.log.Info("peer disconnected", "peer", pc.peer)
	}
}

// runConn serves a registered connection until it closes.
func (n *Node) runConn(ctx context.Context, pc *peerConn, sc *bufio.Scanner) {
	stop := context.AfterFunc(ctx, pc.close)
	defer stop()
	n.wg.Go(func() { n.writeLoop(pc) })
	n.readLoop(pc, sc)
	pc.close()
	n.unregister(pc)
}

// readLoop handles the peer's frames. Once the peer has sent a heartbeat it is
// known to send them, and a session silent for heartbeatTimeout is dead: the
// read deadline closes it. A peer that never sends one (an older version) is
// never timed out.
func (n *Node) readLoop(pc *peerConn, sc *bufio.Scanner) {
	beats := false
	for sc.Scan() {
		// A frame this version cannot read or does not know is skipped, never a
		// reason to drop the session: the peer may be a much newer version.
		f, ok := decodeFrame(sc.Bytes())
		switch {
		case !ok:
			n.log.Debug("unreadable frame skipped", "peer", pc.peer, "len", len(sc.Bytes()))
		case f.Type == "msg":
			if !n.receive(pc, f.Msg) {
				return
			}
		case f.Type == "ack":
			if err := n.store.ack(pc.peer, f.ID); err != nil {
				n.log.Warn("ack", "peer", pc.peer, "id", f.ID, "err", err)
			}
		case f.Type == frameHeartbeat:
			beats = true
		default:
			n.log.Debug("unknown frame type ignored", "peer", pc.peer, "type", f.Type)
		}
		if beats {
			_ = pc.c.SetReadDeadline(time.Now().Add(n.heartbeatTimeout))
		}
	}
	var ne net.Error
	if errors.As(sc.Err(), &ne) && ne.Timeout() {
		n.log.Warn("peer silent, closing session", "peer", pc.peer, "after", n.heartbeatTimeout)
	}
}

// receive persists an inbound message, runs the inbound hook and ACKs it.
// Duplicates are not stored again but still pass the hook, which is
// idempotent. It returns false when the connection must close.
func (n *Node) receive(pc *peerConn, m *Message) bool {
	if m == nil || !validID(m.ID) || m.From != pc.peer {
		n.log.Warn("rejected message", "peer", pc.peer)
		return true
	}
	isNew, err := n.store.saveInbound(*m)
	if err != nil {
		n.log.Error("persist inbound", "id", m.ID, "err", err)
		return true // no ACK: the sender retries
	}
	if isNew {
		n.log.Info("message received", "from", m.From, "id", m.ID, "kind", m.Kind, "job_status", m.JobStatus)
	}
	if n.onInbound != nil {
		if err := n.onInbound(*m); err != nil {
			n.log.Error("inbound hook", "id", m.ID, "err", err)
			return true // no ACK: the sender retries
		}
	}
	return pc.write(frame{Type: "ack", ID: m.ID}) == nil
}

// writeLoop sends the peer's outbox, resending anything not ACKed within
// resendAfter, and a heartbeat every heartbeatEvery.
func (n *Node) writeLoop(pc *peerConn) {
	ticker := time.NewTicker(n.resendTick)
	defer ticker.Stop()
	beat := time.NewTicker(n.heartbeatEvery)
	defer beat.Stop()
	if pc.write(frame{Type: frameHeartbeat}) != nil {
		pc.close()
		return
	}
	sentAt := map[string]time.Time{}
	for {
		msgs, err := n.store.pending(pc.peer)
		if err != nil {
			n.log.Error("read outbox", "peer", pc.peer, "err", err)
		}
		live := make(map[string]time.Time, len(msgs))
		for _, m := range msgs {
			if t, ok := sentAt[m.ID]; ok && time.Since(t) < n.resendAfter {
				live[m.ID] = t
				continue
			}
			if err := pc.write(frame{Type: "msg", Msg: &m}); err != nil {
				pc.close()
				return
			}
			live[m.ID] = time.Now()
		}
		sentAt = live
		select {
		case <-pc.done:
			return
		case <-pc.kick:
		case <-ticker.C:
		case <-beat.C:
			if pc.write(frame{Type: frameHeartbeat}) != nil {
				pc.close()
				return
			}
		}
	}
}
