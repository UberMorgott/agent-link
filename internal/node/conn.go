package node

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
	id     string   // node id; empty for a peer older than v0.5
	app    string   // program version the peer announced, if any
	port   int      // peer port the peer announced, if any
	pake   bool     // authenticated by the PAKE (sealed records); false: the legacy handshake
	// presence is what the peer last said of its sessions, by area (guarded by
	// Node.mu); nil until its first presence frame.
	presence map[string]AreaPresence
	c        net.Conn
	w        *wire

	wmu     sync.Mutex
	leaving atomic.Bool // closeAfterTelling ran: frames are no longer sent or handled
	kick    chan struct{}
	done    chan struct{}
	once    sync.Once
}

func newPeerConn(h peerHello, dialer string, w *wire) *peerConn {
	return &peerConn{peer: h.name, dialer: dialer, areas: h.areas, proto: h.proto, caps: h.caps,
		id: h.id, app: h.app, port: h.port, pake: h.pake, c: w.c, w: w,
		kick: make(chan struct{}, 1), done: make(chan struct{})}
}

func (pc *peerConn) has(c string) bool { return slices.Contains(pc.caps, c) }

// attKey names this session's attachment transfers: a session that replaces
// another never loses its transfers when the old one ends.
func (pc *peerConn) attKey() string { return fmt.Sprintf("%s#%p", pc.peer, pc) }

func (pc *peerConn) write(f frame) error {
	pc.wmu.Lock()
	defer pc.wmu.Unlock()
	if pc.leaving.Load() {
		return nil // the session is being closed; the peer is gone
	}
	_ = pc.c.SetWriteDeadline(time.Now().Add(writeTimeout))
	return pc.w.write(f)
}

// lingerTimeout bounds how long a session closed by closeAfterTelling stays
// open for the peer to read the last frame and hang up.
const lingerTimeout = 2 * time.Second

// closeAfterTelling ends the session once what was written has reached the
// peer: it half-closes (a FIN after the data), stops sending and handling
// frames, and the session ends when the peer hangs up or after lingerTimeout.
// Closing at once while input is unread resets the connection, and a reset
// can discard the last frame before the peer reads it.
func (pc *peerConn) closeAfterTelling() {
	tc, ok := pc.c.(interface{ CloseWrite() error }) // *net.TCPConn, or the Hub's wrapper of one
	if !ok {
		pc.close()
		return
	}
	pc.wmu.Lock()
	pc.leaving.Store(true)
	_ = tc.CloseWrite()
	pc.wmu.Unlock()
	_ = pc.c.SetReadDeadline(time.Now().Add(lingerTimeout))
	time.AfterFunc(lingerTimeout, pc.close)
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
	switch {
	case n.left || (n.removedLocked(pc.peer) && !n.rejoinLocked(pc.peer, pc.id)):
		n.mu.Unlock()
		return false
	case old != nil && old.id != "" && pc.id != "" && old.id != pc.id:
		// Two machines use one name: the smaller node id keeps it everywhere.
		if old.id < pc.id {
			n.mu.Unlock()
			return false
		}
	case old != nil && !preferred(n.cfg.Node, pc, old):
		n.mu.Unlock()
		return false
	case old == nil && !n.hub.addSession():
		// A replacement keeps the slot of the session it replaces.
		n.mu.Unlock()
		n.log.Warn("session refused: too many live sessions", "peer", pc.peer)
		return false
	}
	n.conns[pc.peer] = pc
	n.known[pc.peer] = true
	firstPAKE := pc.pake && !n.pakeSeen[pc.peer]
	if firstPAKE {
		n.pakeSeen[pc.peer] = true
	}
	// A removed node stays told until a live record of it comes back
	// (mergeSelfLocked): a session some member opened before it learned of the
	// removal is not a way back in.
	if self := n.members[n.cfg.Node]; !errors.Is(n.problem, ErrRemoved) || self == nil || !self.Removed {
		n.problem = nil
	}
	n.areas[pc.peer] = pc.areas
	areas := make(map[string][]string, len(n.areas))
	maps.Copy(areas, n.areas)
	n.mu.Unlock()
	if old != nil {
		old.close()
	}
	if err := n.store.saveAreas(areas); err != nil {
		n.log.Warn("save areas", "err", err)
	}
	if firstPAKE {
		n.savePAKESeen()
	}
	n.log.Info("peer connected", "peer", pc.peer, "dialer", pc.dialer, "areas", pc.areas,
		"proto", pc.proto, "caps", pc.caps, "app", pc.app, "pake", pc.pake)
	n.changed("peer")
	return true
}

// savePAKESeen writes the names that have had a PAKE session, so a restart
// cannot be used to force the legacy handshake on them.
func (n *Node) savePAKESeen() {
	n.saveMu.Lock()
	defer n.saveMu.Unlock()
	n.mu.Lock()
	names := slices.Sorted(maps.Keys(n.pakeSeen))
	n.mu.Unlock()
	if err := n.store.savePAKESeen(names); err != nil {
		n.log.Warn("save pake-seen names", "err", err)
	}
}

func (n *Node) unregister(pc *peerConn) {
	n.mu.Lock()
	gone := n.conns[pc.peer] == pc
	hadPresence := pc.presence != nil
	if gone {
		n.hub.dropSession()
		delete(n.conns, pc.peer)
		n.offline[pc.peer] = time.Now()
		n.log.Info("peer disconnected", "peer", pc.peer)
	}
	n.mu.Unlock()
	if gone {
		n.touchSeen(pc.peer)
		n.changed("peer")
		if hadPresence {
			n.changed("chats")
		}
	}
}

// runConn serves a registered connection until it closes. dialed is the
// address this node dialed, empty for an inbound connection.
func (n *Node) runConn(ctx context.Context, pc *peerConn, dialed string) {
	stop := context.AfterFunc(ctx, pc.close)
	defer stop()
	n.noteSession(pc, dialed)
	n.sendProject(pc)
	n.wg.Go(func() { n.writeLoop(pc) })
	n.readLoop(pc)
	pc.close()
	n.atts.dropPeer(pc.attKey())
	n.unregister(pc)
}

// readLoop handles the peer's frames. Once the peer has sent a heartbeat it is
// known to send them, and a session silent for heartbeatTimeout is dead: the
// read deadline closes it. A peer that never sends one (an older version) is
// never timed out.
func (n *Node) readLoop(pc *peerConn) {
	pc.w.maxLine = maxFrame
	beats := false
	for {
		data, err := pc.w.next()
		if err != nil {
			var ne net.Error
			switch {
			case errors.As(err, &ne) && ne.Timeout():
				n.log.Warn("peer silent, closing session", "peer", pc.peer, "after", n.heartbeatTimeout)
			case errors.Is(err, errRecord) || errors.Is(err, errExhausted):
				n.log.Warn("closing session", "peer", pc.peer, "err", err)
			}
			return
		}
		if pc.leaving.Load() {
			continue // drained until the peer hangs up, not handled
		}
		// A frame this version cannot read or does not know is skipped, never a
		// reason to drop the session: the peer may be a much newer version.
		// (On a sealed session it has at least passed the record check.)
		f, ok := decodeFrame(data)
		switch {
		case !ok:
			n.log.Debug("unreadable frame skipped", "peer", pc.peer, "len", len(data))
		case f.Type == "msg":
			if !n.receive(pc, f.Msg) {
				return
			}
		case f.Type == "ack":
			if err := n.store.ack(pc.peer, f.ID); err != nil {
				n.log.Warn("ack", "peer", pc.peer, "id", f.ID, "err", err)
			} else {
				n.changed("messages")
			}
		case f.Type == frameHeartbeat:
			beats = true
		case f.Type == frameMembers:
			n.mergeMembers(f.Members)
		case f.Type == framePresence:
			n.receivePresence(pc, f.Presence)
		case f.Type == frameProject:
			n.mergeProjectMeta(f.ProjectMeta)
		case f.Type == frameAtt:
			n.receiveAttachment(pc, f.Att)
		default:
			n.log.Debug("unknown frame type ignored", "peer", pc.peer, "type", f.Type)
		}
		if beats {
			_ = pc.c.SetReadDeadline(time.Now().Add(n.heartbeatTimeout))
		}
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
	n.cleanAttachments(m)
	if strings.HasPrefix(m.ChatID, legacyPrefix) { // a legacy chat closed there: control only
		if !n.receiveLegacyClose(pc.peer, *m) {
			n.log.Warn("rejected legacy chat message", "peer", pc.peer, "id", m.ID)
		}
		return pc.write(frame{Type: "ack", ID: m.ID}) == nil
	}
	if m.ChatID != "" {
		if !n.receiveChat(pc.peer, pc.id, *m) {
			n.log.Warn("rejected chat message", "peer", pc.peer, "id", m.ID, "chat", m.ChatID)
			return pc.write(frame{Type: "ack", ID: m.ID}) == nil // dropped: resending cannot help
		}
		if m.Kind == KindChatOpen || m.Kind == KindChatClose || m.Kind == KindChatMembers || m.Kind == KindReceipt {
			return pc.write(frame{Type: "ack", ID: m.ID}) == nil
		}
	}
	isNew, err := n.store.saveInbound(*m)
	if err != nil {
		n.log.Error("persist inbound", "id", m.ID, "err", err)
		return true // no ACK: the sender retries
	}
	if isNew {
		n.log.Info("message received", "from", m.From, "id", m.ID, "kind", m.Kind, "job_status", m.JobStatus)
		n.changed("messages")
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
	pushed := map[string]bool{} // attachment blobs sent on this session
	var told []AreaPresence     // the presence last sent
	var toldAt time.Time
	for {
		// The presence is looked at on every pass (at least every resendTick),
		// so a session that expires silently is noticed too.
		if pc.has(CapPresence) && time.Since(toldAt) >= presenceGap {
			if p := n.presenceFor(pc.areas); told == nil || !slices.Equal(p, told) {
				if pc.write(frame{Type: framePresence, Presence: p}) != nil {
					pc.close()
					return
				}
				told, toldAt = p, time.Now()
			}
		}
		msgs, err := n.store.pending(pc.peer)
		if err != nil {
			n.log.Error("read outbox", "peer", pc.peer, "err", err)
		}
		live := make(map[string]time.Time, len(msgs))
		for _, m := range msgs {
			if m.ChatID != "" && !pc.has(CapChat) {
				continue // waits for the peer to take part in chats again
			}
			if (m.Kind == KindReceipt && !pc.has(CapReceipts)) || (m.Kind == KindChatMembers && !pc.has(CapChatMembers)) {
				_ = n.store.ack(pc.peer, m.ID) // it cannot read them: drop them
				continue
			}
			if t, ok := sentAt[m.ID]; ok && time.Since(t) < n.resendAfter {
				live[m.ID] = t
				continue
			}
			if err := n.pushAttachments(pc, &m, pushed); err != nil {
				pc.close()
				return
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
