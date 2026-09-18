package node

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuf collects what a proxy forwards.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.b.Bytes())
}

// recordingProxy forwards every connection to target and records both directions.
func recordingProxy(t *testing.T, target string) (net.Listener, *syncBuf) {
	t.Helper()
	ln := listen(t)
	t.Cleanup(func() { _ = ln.Close() })
	rec := &syncBuf{}
	ctx := t.Context()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			var d net.Dialer
			up, err := d.DialContext(ctx, "tcp", target)
			if err != nil {
				_ = c.Close()
				continue
			}
			pipe := func(dst, src net.Conn) {
				_, _ = io.Copy(dst, io.TeeReader(src, rec))
				_ = dst.Close()
				_ = src.Close()
			}
			go pipe(up, c)
			go pipe(c, up)
		}
	}()
	return ln, rec
}

// A PAKE session carries messages both ways, and nothing after the hellos is
// readable on the wire: not the body, not even the frame types.
func TestSealedRoundTrip(t *testing.T) {
	lnA, lnB := listen(t), listen(t)
	proxy, rec := recordingProxy(t, lnB.Addr().String())
	a := newTestNode(t, "a", testSecret, nil, t.TempDir(), lnA, map[string]net.Listener{"b": proxy})
	b := newTestNode(t, "b", testSecret, nil, t.TempDir(), lnB, nil)
	for _, n := range []*testNode{a, b} {
		n.isPrivate = func(net.IP) bool { return false }
		n.start(t)
	}
	eventually(t, "a<->b connected", func() bool { return a.Connected("b") && b.Connected("a") })
	q, err := a.Send("b", "top secret question 4711", "")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "b got it", func() bool { return hasEntry(t, b, q.ID) })
	r, err := b.Send("a", "top secret answer 0815", q.ID)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "a got the answer", func() bool { return hasEntry(t, a, r.ID) })
	wire := string(rec.bytes())
	for _, s := range []string{"top secret", `"type":"msg"`, `"type":"ack"`, `"type":"ok"`, `"type":"hb"`, q.ID} {
		if strings.Contains(wire, s) {
			t.Errorf("%q readable on the wire", s)
		}
	}
	if !strings.Contains(wire, `"type":"hello"`) {
		t.Fatal("proxy recorded nothing")
	}
}

func hasEntry(t *testing.T, tn *testNode, id string) bool {
	t.Helper()
	entries, err := tn.store.recent(0)
	if err != nil {
		t.Fatal(err)
	}
	return slices.ContainsFunc(entries, func(e Entry) bool { return e.ID == id && e.Direction == "in" })
}

// sealedPeer runs the whole PAKE with tn as name and returns the sealed wire;
// nothing reads from it.
func sealedPeer(t *testing.T, tn *testNode, name string) *wire {
	t.Helper()
	f, cp, w, th := rawPAKE(t, tn, name, testSecret)
	share, err := hex.DecodeString(f.PAKE)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := cp.keys(share, true, cpaceAD(name, "id-"+name), cpaceAD(f.Node, f.NodeID))
	if err != nil {
		t.Fatal(err)
	}
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
	_ = w.c.SetDeadline(time.Time{})
	eventually(t, name+" connected", func() bool { return tn.Connected(name) })
	return w
}

func sealFrame(t *testing.T, w *wire, f frame) []byte {
	t.Helper()
	data, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := w.out.seal(data)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// closedByPeer reports whether the node closed the connection: reads end in
// an error other than a timeout.
func closedByPeer(w *wire) bool {
	_ = w.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		_, err := w.r.ReadByte()
		if err != nil {
			var ne net.Error
			return !errors.As(err, &ne) || !ne.Timeout()
		}
	}
}

// A record altered anywhere (length, ciphertext, tag) closes the session and
// delivers nothing.
func TestTamperedRecordClosesSession(t *testing.T) {
	for _, at := range []string{"length", "body", "tag"} {
		t.Run(at, func(t *testing.T) {
			a := openNode(t, time.Second, 5*time.Second)
			w := sealedPeer(t, a, "bob")
			m := Message{ID: newID(), From: "bob", To: "a", Body: "rm -rf please", CreatedAt: time.Now().UTC()}
			rec := sealFrame(t, w, frame{Type: "msg", Msg: &m})
			switch at {
			case "length":
				rec[0] ^= 0x80
			case "body":
				rec[recordHeader+5] ^= 0x20
			case "tag":
				rec[len(rec)-1] ^= 1
			}
			if _, err := w.c.Write(rec); err != nil {
				t.Fatal(err)
			}
			if !closedByPeer(w) {
				t.Fatal("connection not closed")
			}
			eventually(t, "bob dropped", func() bool { return !a.Connected("bob") })
			if hasEntry(t, a, m.ID) {
				t.Fatal("tampered message stored")
			}
		})
	}
}

// A replayed or reordered record fails its counter and closes the session.
func TestReplayedRecordRejected(t *testing.T) {
	a := openNode(t, time.Second, 5*time.Second)
	w := sealedPeer(t, a, "bob")
	m := Message{ID: newID(), From: "bob", To: "a", Body: "once", CreatedAt: time.Now().UTC()}
	rec := sealFrame(t, w, frame{Type: "msg", Msg: &m})
	if _, err := w.c.Write(rec); err != nil {
		t.Fatal(err)
	}
	eventually(t, "message stored", func() bool { return hasEntry(t, a, m.ID) })
	if _, err := w.c.Write(rec); err != nil {
		t.Fatal(err)
	}
	if !closedByPeer(w) {
		t.Fatal("replay did not close the connection")
	}
	eventually(t, "bob dropped", func() bool { return !a.Connected("bob") })

	w = sealedPeer(t, a, "carol")
	first := sealFrame(t, w, frame{Type: frameHeartbeat})
	second := sealFrame(t, w, frame{Type: frameHeartbeat})
	if _, err := w.c.Write(append(second, first...)); err != nil {
		t.Fatal(err)
	}
	if !closedByPeer(w) {
		t.Fatal("reordered records did not close the connection")
	}
}

// Records past the counter limit are refused rather than reusing a nonce.
func TestRecordCounterExhausted(t *testing.T) {
	s, err := newSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	s.seq = maxRecords - 1
	if _, err := s.seal([]byte("{}")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.seal([]byte("{}")); !errors.Is(err, errExhausted) {
		t.Fatalf("seal past the limit: %v", err)
	}
	if _, err := s.seal(make([]byte, maxFrame+1)); err == nil {
		t.Fatal("oversized frame sealed")
	}
	r, err := newSealer(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	huge := []byte{0xff, 0xff, 0xff, 0xff}
	if _, err := r.open(bytes.NewReader(huge)); !errors.Is(err, errRecord) {
		t.Fatalf("oversized record: %v", err)
	}
}

// The names that ran the PAKE survive a restart: the legacy handshake stays
// refused for them.
func TestPAKESeenSurvivesRestart(t *testing.T) {
	a, b := pair(t, testSecret, testSecret)
	eventually(t, "a<->b connected", func() bool { return a.Connected("b") && b.Connected("a") })
	b.stop()
	a.stop()
	cfg := a.cfg
	cfg.Peers = nil
	n, err := New(cfg, []byte(testSecret), nil)
	if err != nil {
		t.Fatal(err)
	}
	loopback := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)}
	if err := n.legacyAllowed(loopback, "b"); !errors.Is(err, ErrLegacyRefused) {
		t.Fatalf("legacy handshake as b after restart: %v", err)
	}
	if err := n.legacyAllowed(loopback, "old"); err != nil {
		t.Fatalf("legacy handshake as another name: %v", err)
	}
}

// Unauthenticated connections are capped per source and in all; a
// connection over the cap is closed at once, and the handshake deadline
// frees the slots of idle ones.
func TestUnauthenticatedConnectionCap(t *testing.T) {
	a := newTestNode(t, "a", testSecret, nil, t.TempDir(), listen(t), nil)
	a.pending = newConnLimit(10, 2)
	a.handshakeTimeout = 700 * time.Millisecond
	a.start(t)
	dial := func() net.Conn {
		c, err := dialTCP(t, a.peerLn.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	idle := []net.Conn{dial(), dial()}
	eventually(t, "two pending", func() bool {
		a.pending.mu.Lock()
		defer a.pending.mu.Unlock()
		return a.pending.n == 2
	})
	over := dial()
	began := time.Now()
	_ = over.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := over.Read(make([]byte, 1)); err == nil {
		t.Fatal("connection over the cap answered")
	}
	if d := time.Since(began); d > 500*time.Millisecond {
		t.Fatalf("over-cap connection closed after %s, want at once", d)
	}
	for _, c := range idle { // the deadline closes the idle ones
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, err := c.Read(make([]byte, 1)); err == nil {
			t.Fatal("idle connection answered")
		}
	}
	eventually(t, "slots freed", func() bool {
		a.pending.mu.Lock()
		defer a.pending.mu.Unlock()
		return a.pending.n == 0
	})
	if !legacyHello(t, a, "old") {
		t.Fatal("handshake refused after the slots were freed")
	}
}

func TestConnLimit(t *testing.T) {
	l := newConnLimit(3, 2)
	ip1, ip2, ip3 := net.ParseIP("203.0.113.1"), net.ParseIP("203.0.113.2"), net.ParseIP("203.0.113.3")
	if a, b, c := l.acquire(ip1), l.acquire(ip1), l.acquire(ip1); !a || !b || c {
		t.Fatal("per-source cap")
	}
	if !l.acquire(ip2) || l.acquire(ip3) {
		t.Fatal("global cap")
	}
	l.release(ip1)
	if first, second := l.acquire(ip3), l.acquire(ip3); !first || second {
		t.Fatal("release")
	}
	l.release(ip1)
	l.release(ip2)
	l.release(ip3)
	if l.n != 0 || len(l.by) != 0 {
		t.Fatalf("left %d, %v", l.n, l.by)
	}
}
