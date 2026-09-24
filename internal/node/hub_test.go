package node

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/UberMorgott/agent-link/internal/config"
)

// startHub runs a Hub on a fresh loopback listener until the test ends.
func startHub(t *testing.T) *Hub {
	t.Helper()
	h := NewHub(listen(t), HubConfig{})
	h.handshakeTimeout = 3 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	h.Start(ctx)
	t.Cleanup(func() {
		cancel()
		h.Wait()
	})
	return h
}

// onHub runs tn on h (tn.peerLn must be h's listener) until the test ends. It
// stops tn before tn's data directory goes: that TempDir cleanup was
// registered after startHub's, so it would run first, while tn still writes.
func onHub(t *testing.T, h *Hub, tn *testNode) *testNode {
	t.Helper()
	if err := h.Add(tn.Node); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Remove(tn.cfg.Project) })
	eventually(t, tn.cfg.Node+" running on the hub", tn.running)
	return tn
}

var loopbackIP = net.IPv4(127, 0, 0, 1)

// guardFails is how many failed handshakes the Hub holds against loopback in scope.
func (h *Hub) guardFails(scope string) int {
	h.guard.mu.Lock()
	defer h.guard.mu.Unlock()
	if e := h.guard.m[scopedKey(loopbackIP, scope)]; e != nil {
		return e.fails
	}
	return 0
}

func (h *Hub) pendingCount() int {
	h.pending.mu.Lock()
	defer h.pending.mu.Unlock()
	return h.pending.n
}

// rawProjectSession runs a dialer named name of project p against addr
// through CPace; first writes the hello line (and anything else) to c. It
// returns the sealed wire once the acceptor's ok arrived.
func rawProjectSession(t *testing.T, addr string, p testProject, name string, first func(c net.Conn, hello []byte)) *wire {
	t.Helper()
	c, err := dialTCP(t, addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	nonce := randomHex(32)
	sid, _ := hex.DecodeString(nonce)
	cp, err := newCPace(p.key, projectCI(p.id), sid)
	if err != nil {
		t.Fatal(err)
	}
	id := "id-" + name
	hello, _ := json.Marshal(frame{Type: "hello", Node: name, Nonce: nonce, NodeID: id, PAKE: hex.EncodeToString(cp.share),
		Project: p.id, Caps: []string{CapPAKE, CapProjects}})
	first(c, append(hello, '\n'))
	w := newWire(c)
	f, err := w.readFrame("hello")
	if err != nil {
		t.Fatal(err)
	}
	yb, _ := hex.DecodeString(f.PAKE)
	keys, err := cp.keys(yb, true, cpaceAD(name, id), cpaceAD(f.Node, f.NodeID))
	if err != nil || hex.EncodeToString(keys.acceptorTag) != f.MAC {
		t.Fatalf("acceptor tag: %v", err)
	}
	th := transcript(hello, w.lastLine)
	if err := w.write(frame{Type: "auth", MAC: hex.EncodeToString(keys.dialerTag(th))}); err != nil {
		t.Fatal(err)
	}
	ck, err := keys.channel(th)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.secure(ck, true); err != nil {
		t.Fatal(err)
	}
	if _, err := w.readFrame("ok"); err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Time{})
	return w
}

// helloReply sends line to addr and returns the first frame answered, if any.
func helloReply(t *testing.T, addr string, line []byte) (frame, bool) {
	t.Helper()
	c, err := dialTCP(t, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write(line); err != nil {
		return frame{}, false
	}
	f, err := readFrame(newScanner(c), "hello")
	return f, err == nil
}

func TestHubRoutesContexts(t *testing.T) {
	h := startHub(t)
	p := newTestProject(t)
	legacy := onHub(t, h, newTestNode(t, "a", testSecret, nil, t.TempDir(), h.ln, nil))
	proj := onHub(t, h, newProjectNode(t, "a", p, h.ln, nil))

	b := newTestNode(t, "b", testSecret, nil, t.TempDir(), listen(t), map[string]net.Listener{"a": h.ln})
	c := newProjectNode(t, "c", p, listen(t), map[string]net.Listener{"a": h.ln})
	for _, n := range []*testNode{b, c} {
		n.start(t)
	}
	eventually(t, "legacy a<->b and project a<->c", func() bool {
		return legacy.Connected("b") && b.Connected("a") && proj.Connected("c") && c.Connected("a")
	})
	if legacy.Connected("c") || proj.Connected("b") || !proj.PeerHas("c", CapProjects) {
		t.Fatal("a session reached the wrong context")
	}

	// A pre-projects dialer (golden bytes of a v0.5 hello) still reaches the legacy node.
	golden := []byte(`{"type":"hello","node":"old","areas":["dev"],"nonce":"` + strings.Repeat("ab", 32) + `","proto":4,"caps":["caps","hb"]}` + "\n")
	if f, ok := helloReply(t, h.ln.Addr().String(), golden); !ok || f.MAC == "" || f.Node != "a" || f.Project != "" {
		t.Fatalf("legacy hello through the hub: %+v %v", f, ok)
	}
	// Without its context a project hello is told so, and never reaches the legacy node.
	if !h.Remove(p.id) || h.Node(p.id) != nil {
		t.Fatal("remove")
	}
	line, _ := json.Marshal(frame{Type: "hello", Node: "c", Nonce: randomHex(32), PAKE: "00", Project: p.id})
	if f, ok := helloReply(t, h.ln.Addr().String(), append(line, '\n')); !ok || f.Error != helloUnknownProject || f.MAC != "" || f.Node != "" {
		t.Fatalf("hello for a removed project: %+v %v", f, ok)
	}
	if legacy.Connected("c") {
		t.Fatal("project hello handed to the legacy node")
	}
}

// A dialer of a project the hub does not run is told so and reports it.
func TestHubUnknownProjectReported(t *testing.T) {
	h := startHub(t)
	onHub(t, h, newProjectNode(t, "a", newTestProject(t), h.ln, nil))
	stranger := newProjectNode(t, "d", newTestProject(t), listen(t), map[string]net.Listener{"a": h.ln})
	stranger.start(t)
	eventually(t, "unknown project reported", func() bool { return errors.Is(stranger.Problem(), ErrUnknownProject) })
}

// Without a legacy context a hello without project is closed unanswered.
func TestHubWithoutLegacy(t *testing.T) {
	h := startHub(t)
	onHub(t, h, newProjectNode(t, "a", newTestProject(t), h.ln, nil))
	if f, ok := helloReply(t, h.ln.Addr().String(), []byte(`{"type":"hello","node":"old","nonce":"`+randomHex(32)+`"}`+"\n")); ok {
		t.Fatalf("answered %+v", f)
	}
}

// The hello may come in any number of TCP writes, the next frame may come in
// the same write, and the session is not limited by the first-line cap.
func TestHubHandoffKeepsTheStream(t *testing.T) {
	h := startHub(t)
	p := newTestProject(t)
	a := onHub(t, h, newProjectNode(t, "a", p, h.ln, nil))
	addr := h.ln.Addr().String()

	rawProjectSession(t, addr, p, "split", func(c net.Conn, hello []byte) {
		for i := 0; i < len(hello); i += 7 {
			_, _ = c.Write(hello[i:min(i+7, len(hello))])
			time.Sleep(2 * time.Millisecond)
		}
	})
	eventually(t, "split hello connected", func() bool { return a.Connected("split") })

	w := rawProjectSession(t, addr, p, "joined", func(c net.Conn, hello []byte) {
		// An unknown frame right behind the hello, in the same write: the
		// node must still see it (and skip it) before the auth.
		_, _ = c.Write(append(hello, []byte(`{"type":"future","x":1}`+"\n")...))
	})
	eventually(t, "joined connected", func() bool { return a.Connected("joined") })

	// More than maxFrame in total over the session.
	body := strings.Repeat("x", maxFrame/2)
	var ids []string
	for range 3 {
		m := Message{ID: newID(), From: "joined", To: "a", Body: body, CreatedAt: time.Now().UTC()}
		ids = append(ids, m.ID)
		if err := w.write(frame{Type: "msg", Msg: &m}); err != nil {
			t.Fatal(err)
		}
	}
	_ = w.c.SetReadDeadline(time.Now().Add(10 * time.Second))
	acked := map[string]bool{}
	for len(acked) < len(ids) {
		data, err := w.next()
		if err != nil {
			t.Fatalf("after %d acks: %v", len(acked), err)
		}
		if f, ok := decodeFrame(data); ok && f.Type == "ack" {
			acked[f.ID] = true
		}
	}
}

// Every path an inbound connection can take frees its pending slot exactly
// once, and failures count against the source.
func TestHubLeaseOnEveryPath(t *testing.T) {
	h := startHub(t)
	h.handshakeTimeout = 200 * time.Millisecond
	p := newTestProject(t)
	a := onHub(t, h, newProjectNode(t, "a", p, h.ln, nil))
	addr := h.ln.Addr().String()
	closed := func(c net.Conn) {
		t.Helper()
		_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
		if _, err := c.Read(make([]byte, 512)); err == nil {
			// the unknown-project reply; the close follows
			if _, err := c.Read(make([]byte, 512)); err == nil {
				t.Fatal("connection not closed")
			}
		}
		_ = c.Close()
	}
	// Failures before a hello, and in each context, are counted apart.
	settled := func(what string, before, inProject int) {
		t.Helper()
		eventually(t, what, func() bool {
			return h.pendingCount() == 0 && h.guardFails(scopeBeforeHello) == before && h.guardFails(p.id) == inProject && h.guardFails("") == 0
		})
	}

	c, _ := dialTCP(t, addr) // silent until the deadline
	closed(c)
	settled("timeout", 1, 0)

	c, _ = dialTCP(t, addr)
	_, _ = c.Write(bytes.Repeat([]byte("a"), maxHandshakeLine+10))
	closed(c)
	settled("oversize line", 2, 0)

	c, _ = dialTCP(t, addr)
	line, _ := json.Marshal(frame{Type: "hello", Node: "x", Nonce: randomHex(32), PAKE: "00", Project: newTestProject(t).id})
	_, _ = c.Write(append(line, '\n'))
	closed(c)
	settled("unknown project: not a guess", 2, 0)

	// A wrong key fails the node's handshake, charged through the Hub's lease.
	f, _, w, _ := rawPAKEProject(t, addr, p.id, "eve", newTestProject(t).key)
	if f.MAC == "" {
		t.Fatal("no acceptor tag")
	}
	_ = w.write(frame{Type: "auth", MAC: hex.EncodeToString(make([]byte, 32))})
	closed(w.c)
	settled("auth failure", 2, 1)

	// A stopped node: the handoff frees the slot and closes the connection.
	server, client := net.Pipe()
	if !h.pending.acquire(loopbackIP) {
		t.Fatal("acquire")
	}
	lease := newInboundLease(h.guard, h.pending, loopbackIP)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.handoff(&hubEntry{ln: newChanListener(h.ln.Addr()), ctx: ctx}, &hubConn{Conn: server, r: server, lease: lease})
	if _, err := client.Read(make([]byte, 1)); err == nil {
		t.Fatal("handed to a stopped node, not closed")
	}
	lease.release() // a second release changes nothing
	settled("stopped node", 2, 1)

	// Success forgets the failures.
	rawProjectSession(t, addr, p, "bob", func(c net.Conn, hello []byte) { _, _ = c.Write(hello) })
	eventually(t, "bob connected", func() bool { return a.Connected("bob") })
	settled("success forgets only its context", 2, 0)
}

// rawPAKEProject is rawPAKE for a project hello, keyed by key.
func rawPAKEProject(t *testing.T, addr, pid, name string, key []byte) (frame, *cpace, *wire, []byte) {
	t.Helper()
	c, err := dialTCP(t, addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	nonce := randomHex(32)
	sid, _ := hex.DecodeString(nonce)
	cp, err := newCPace(key, projectCI(pid), sid)
	if err != nil {
		t.Fatal(err)
	}
	hello, _ := json.Marshal(frame{Type: "hello", Node: name, Nonce: nonce, NodeID: "id-" + name, PAKE: hex.EncodeToString(cp.share), Project: pid})
	w := newWire(c)
	if err := w.writeData(hello); err != nil {
		t.Fatal(err)
	}
	f, err := w.readFrame("hello")
	if err != nil {
		t.Fatal(err)
	}
	return f, cp, w, transcript(hello, w.lastLine)
}

func TestHubSessionCap(t *testing.T) {
	h := startHub(t)
	h.maxSessions = 1
	onHub(t, h, newTestNode(t, "a", testSecret, nil, t.TempDir(), h.ln, nil))
	b := newMeshNode(t, "b", h.ln)
	c := newMeshNode(t, "c", h.ln)
	b.start(t)
	c.start(t)
	eventually(t, "one session", func() bool { return b.Connected("a") || c.Connected("a") })
	time.Sleep(500 * time.Millisecond)
	h.sessMu.Lock()
	live := h.sessions
	h.sessMu.Unlock()
	if live != 1 || (b.Connected("a") && c.Connected("a")) {
		t.Fatalf("%d live sessions over a cap of 1", live)
	}
}

func TestHubDialCap(t *testing.T) {
	h := startHub(t)
	for range cap(h.dialSem) {
		h.dialSem <- struct{}{}
	}
	p := newTestProject(t)
	b := newProjectNode(t, "b", p, listen(t), nil)
	b.start(t)
	a := onHub(t, h, newProjectNode(t, "a", p, h.ln, map[string]net.Listener{"b": b.peerLn}))
	time.Sleep(300 * time.Millisecond)
	if a.Connected("b") {
		t.Fatal("dialed without a free slot")
	}
	<-h.dialSem
	eventually(t, "dial after a slot frees", func() bool { return a.Connected("b") && b.Connected("a") })
	eventually(t, "the dial slot freed once the session runs", func() bool { return len(h.dialSem) == cap(h.dialSem)-1 })
}

// Beacons reach the context of their tag; the Hub sends one per context.
func TestHubDiscoveryRouting(t *testing.T) {
	h := startHub(t)
	p := newTestProject(t)
	legacy := newTestNode(t, "a", testSecret, nil, t.TempDir(), h.ln, nil)
	legacy.netTag = NetworkTag([]byte(testSecret))
	onHub(t, h, legacy)
	proj := newProjectNode(t, "a", p, h.ln, nil)
	proj.netTag = config.ProjectTag(p.key)
	onHub(t, h, proj)

	msgs := map[int]string{}
	for _, m := range h.beaconMsgs() {
		var b beacon
		if err := json.Unmarshal(m, &b); err != nil {
			t.Fatal(err)
		}
		msgs[b.V] = b.Net
	}
	if len(msgs) != 2 || msgs[beaconVersion] != legacy.netTag || msgs[projectBeaconVersion] != proj.netTag {
		t.Fatalf("beacons %v", msgs)
	}

	pb := newProjectNode(t, "pb", p, listen(t), nil)
	lb := newMeshNode(t, "lb")
	pb.start(t)
	lb.start(t)
	h.heard(beacon{T: beaconType, V: projectBeaconVersion, Net: proj.netTag, Node: "pb", ID: pb.id, Port: tcpPort(t, pb.peerLn)}, loopbackIP)
	h.heard(beacon{T: beaconType, V: beaconVersion, Net: legacy.netTag, Node: "lb", ID: lb.id, Port: tcpPort(t, lb.peerLn)}, loopbackIP)
	eventually(t, "each context dialed its own beacon", func() bool { return proj.Connected("pb") && legacy.Connected("lb") })
	if proj.Connected("lb") || legacy.Connected("pb") {
		t.Fatal("a beacon reached the wrong context")
	}
}

func TestHubAddLimits(t *testing.T) {
	h := startHub(t)
	p := newTestProject(t)
	onHub(t, h, newProjectNode(t, "a", p, h.ln, nil))
	if err := h.Add(newProjectNode(t, "b", p, h.ln, nil).Node); !errors.Is(err, ErrContextExists) {
		t.Fatalf("second node of one project: %v", err)
	}
	for i := 1; i < MaxHubProjects; i++ {
		if err := h.Add(newProjectNode(t, "a", newTestProject(t), h.ln, nil).Node); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.Add(newProjectNode(t, "a", newTestProject(t), h.ln, nil).Node); !errors.Is(err, ErrTooManyProjects) {
		t.Fatalf("project %d: %v", MaxHubProjects+1, err)
	}
	if err := h.Add(newTestNode(t, "a", testSecret, nil, t.TempDir(), h.ln, nil).Node); err != nil {
		t.Fatalf("legacy beside %d projects: %v", MaxHubProjects, err)
	}
	if h.Remove("nope") {
		t.Fatal("removed an unknown context")
	}
	// A removed project's id is free again once it stopped.
	if !h.Remove(p.id) {
		t.Fatal("remove")
	}
	if err := h.Add(newProjectNode(t, "a", p, h.ln, nil).Node); err != nil {
		t.Fatalf("re-add after remove: %v", err)
	}
}

func TestHubAddAfterWait(t *testing.T) {
	h := NewHub(listen(t), HubConfig{})
	if err := h.Add(newTestNode(t, "a", testSecret, nil, t.TempDir(), h.ln, nil).Node); !errors.Is(err, ErrHubNotRunning) {
		t.Fatalf("add before start: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.Start(ctx)
	cancel()
	h.Wait()
	if err := h.Add(newTestNode(t, "a", testSecret, nil, t.TempDir(), h.ln, nil).Node); !errors.Is(err, ErrHubNotRunning) {
		t.Fatalf("add after wait: %v", err)
	}
}
