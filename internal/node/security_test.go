package node

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"slices"
	"testing"
	"time"

	"github.com/gtank/ristretto255"
)

// publicPair starts two nodes that see each other as public addresses: only
// the PAKE may connect them.
func publicPair(t *testing.T) (*testNode, *testNode) {
	t.Helper()
	lnA, lnB := listen(t), listen(t)
	a := newTestNode(t, "a", testSecret, nil, t.TempDir(), lnA, map[string]net.Listener{"b": lnB})
	b := newTestNode(t, "b", testSecret, nil, t.TempDir(), lnB, map[string]net.Listener{"a": lnA})
	for _, n := range []*testNode{a, b} {
		n.isPrivate = func(net.IP) bool { return false }
		n.start(t)
	}
	return a, b
}

func TestPAKESessionFromPublicAddress(t *testing.T) {
	a, b := publicPair(t)
	eventually(t, "a<->b connected", func() bool { return a.Connected("b") && b.Connected("a") })
	if !a.PeerHas("b", CapPAKE) {
		t.Fatal("pake capability not announced")
	}
	for _, m := range a.Members() {
		if m.OldAuth {
			t.Fatalf("PAKE session listed as old auth: %+v", m)
		}
	}
	a.mu.Lock()
	pc := a.conns["b"]
	a.mu.Unlock()
	if !pc.pake || !pc.w.sealed() {
		t.Fatal("session not marked as PAKE or not sealed")
	}
}

// rawPAKE plays a dialer named name that runs CPace with key against tn and
// returns the acceptor's hello, the dialer's CPace state, the connection and
// the handshake transcript.
func rawPAKE(t *testing.T, tn *testNode, name, key string) (frame, *cpace, *wire, []byte) {
	t.Helper()
	c, err := dialTCP(t, tn.peerLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	nonce := randomHex(32)
	sid, _ := hex.DecodeString(nonce)
	cp, err := newCPace([]byte(key), sid)
	if err != nil {
		t.Fatal(err)
	}
	hello, _ := json.Marshal(frame{Type: "hello", Node: name, Nonce: nonce, NodeID: "id-" + name, PAKE: hex.EncodeToString(cp.share)})
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

// A dialer with a wrong code gets the acceptor's share and tag, but they
// confirm no candidate code offline, not even the right one: with its own
// scalar the attacker cannot recompute what the acceptor derived.
func TestWrongCodeGivesNothingReusable(t *testing.T) {
	a := openNode(t, time.Second, 5*time.Second)
	wrong := "another-secret-0123456789"
	f, cp, w, _ := rawPAKE(t, a, "eve", wrong)
	yb, err := hex.DecodeString(f.PAKE)
	if err != nil || len(yb) != 32 || f.MAC == "" {
		t.Fatalf("acceptor reply %+v", f)
	}
	tag, _ := hex.DecodeString(f.MAC)
	for _, guess := range []string{wrong, testSecret} {
		g, err := newCPaceScalar([]byte(guess), cp.sid, cp.y)
		if err != nil {
			t.Fatal(err)
		}
		keys, err := g.keys(yb, true, cpaceAD("eve", "id-eve"), cpaceAD(f.Node, f.NodeID))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(keys.acceptorTag, tag) {
			t.Fatalf("guess %q confirmed offline", guess)
		}
	}
	// Its auth fails and the session never comes up.
	_ = w.write(frame{Type: "auth", MAC: hex.EncodeToString(make([]byte, 32))})
	if _, err := w.readFrame("ok"); err == nil {
		t.Fatal("wrong code got ok")
	}
	if a.Connected("eve") {
		t.Fatal("wrong code connected")
	}

	// The same procedure with the right code does confirm: the check above is meaningful.
	f, cp, w, th := rawPAKE(t, a, "bob", testSecret)
	yb, _ = hex.DecodeString(f.PAKE)
	keys, err := cp.keys(yb, true, cpaceAD("bob", "id-bob"), cpaceAD(f.Node, f.NodeID))
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(keys.acceptorTag) != f.MAC {
		t.Fatal("right code not confirmed")
	}
	_ = w.write(frame{Type: "auth", MAC: hex.EncodeToString(keys.dialerTag(th))})
	ck, err := keys.channel(th)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.secure(ck, true); err != nil {
		t.Fatal(err)
	}
	if _, err := w.readFrame("ok"); err != nil {
		t.Fatalf("right code: %v", err)
	}
	eventually(t, "bob connected", func() bool { return a.Connected("bob") })
}

// An identity or garbage share is refused before any tag is sent.
func TestInvalidShareGetsNoTag(t *testing.T) {
	a := openNode(t, time.Second, 5*time.Second)
	for _, share := range []string{hex.EncodeToString(ristretto255.NewIdentityElement().Bytes()), "zz", hex.EncodeToString(bytes.Repeat([]byte{0xff}, 32))} {
		c, err := dialTCP(t, a.peerLn.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		_ = writeFrame(c, frame{Type: "hello", Node: "eve", Nonce: randomHex(32), PAKE: share})
		if f, err := readFrame(newScanner(c), "hello"); err == nil {
			t.Fatalf("share %s answered with %+v", share, f)
		}
		_ = c.Close()
	}
}

// legacyHello sends a pre-v0.6 hello as name and reports whether the node
// answered with a MAC.
func legacyHello(t *testing.T, tn *testNode, name string) bool {
	t.Helper()
	c, err := dialTCP(t, tn.peerLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	_ = writeFrame(c, frame{Type: "hello", Node: name, Nonce: randomHex(32)})
	f, err := readFrame(newScanner(c), "hello")
	return err == nil && f.MAC != ""
}

func TestLegacyOnlyFromPrivateAddresses(t *testing.T) {
	a := openNode(t, time.Second, 5*time.Second)
	if !legacyHello(t, a, "old") {
		t.Fatal("legacy hello from loopback not answered")
	}
	pub := newTestNode(t, "a", testSecret, nil, t.TempDir(), listen(t), nil)
	pub.isPrivate = func(net.IP) bool { return false }
	pub.start(t)
	if legacyHello(t, pub, "old") {
		t.Fatal("legacy hello from a public address got a MAC")
	}
}

func TestPrivateAddr(t *testing.T) {
	_, zt, _ := net.ParseCIDR("100.64.0.0/10")
	for s, want := range map[string]bool{
		"127.0.0.1": true, "::1": true, "10.1.2.3": true, "172.16.0.1": true, "192.168.1.1": true,
		"169.254.3.4": true, "fe80::1": true, "fd00::1": true, "::ffff:192.168.1.1": true,
		"100.64.1.2": true, // a ZeroTier network of this machine
		"8.8.8.8":    false, "203.0.113.9": false, "2001:db8::1": false, "172.32.0.1": false, "100.128.0.1": false,
	} {
		if got := privateAddr(net.ParseIP(s), []*net.IPNet{zt}); got != want {
			t.Errorf("privateAddr(%s) = %v, want %v", s, got, want)
		}
	}
}

// A name that authenticated with the PAKE never falls back to the legacy handshake.
func TestNoDowngradeAfterPAKE(t *testing.T) {
	a, b := pair(t, testSecret, testSecret)
	eventually(t, "a<->b connected", func() bool { return a.Connected("b") && b.Connected("a") })
	if legacyHello(t, a, "b") {
		t.Fatal("legacy hello under a PAKE peer's name got a MAC")
	}
}

// fakeLegacyAcceptor answers one dial per connection like a pre-v0.6 node named name.
func fakeLegacyAcceptor(t *testing.T, ln net.Listener, name string) {
	t.Helper()
	signer := manualNode(t, name)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				sc := newScanner(c)
				h, err := readFrame(sc, "hello")
				if err != nil {
					return
				}
				nonce := randomHex(32)
				mac := hex.EncodeToString(signer.mac("accept", h.Node, name, h.Nonce, nonce))
				if writeFrame(c, frame{Type: "hello", Node: name, Nonce: nonce, MAC: mac}) != nil {
					return
				}
				auth, err := readFrame(sc, "auth")
				if err != nil || signer.checkMAC(auth.MAC, signer.mac("dial", h.Node, name, h.Nonce, nonce)) != nil {
					return
				}
				_ = writeFrame(c, frame{Type: "ok"})
				for sc.Scan() { // hold the session
				}
			}()
		}
	}()
}

func manualNode(t *testing.T, name string) *Node {
	t.Helper()
	n := newTestNode(t, name, testSecret, nil, t.TempDir(), listen(t), nil)
	return n.Node
}

// A new node dials an old one: on loopback it connects (listed as old auth);
// at a public address it refuses and reports ErrLegacyRefused.
func TestDialLegacyAcceptor(t *testing.T) {
	lnOld, lnA := listen(t), listen(t)
	fakeLegacyAcceptor(t, lnOld, "old")
	a := newTestNode(t, "a", testSecret, nil, t.TempDir(), lnA, map[string]net.Listener{"old": lnOld})
	a.start(t)
	eventually(t, "a->old connected", func() bool { return a.Connected("old") })
	if !slices.ContainsFunc(a.Members(), func(m MemberInfo) bool { return m.Name == "old" && m.OldAuth }) {
		t.Fatal("legacy member not listed as old auth")
	}

	lnOld2, lnB := listen(t), listen(t)
	fakeLegacyAcceptor(t, lnOld2, "old")
	b := newTestNode(t, "b", testSecret, nil, t.TempDir(), lnB, map[string]net.Listener{"old": lnOld2})
	b.isPrivate = func(net.IP) bool { return false }
	b.start(t)
	eventually(t, "legacy refused", func() bool { return errors.Is(b.Problem(), ErrLegacyRefused) })
	if b.Connected("old") {
		t.Fatal("legacy peer at a public address connected")
	}
}

// Failed handshakes from one source get it closed unread for a while.
func TestFailedHandshakesBlockSource(t *testing.T) {
	a := openNode(t, time.Second, 5*time.Second)
	for i := range guardFree + 1 {
		f, _, w, _ := rawPAKE(t, a, "eve", "another-secret-0123456789")
		if f.PAKE == "" {
			t.Fatalf("attempt %d not answered", i)
		}
		_ = w.write(frame{Type: "auth", MAC: hex.EncodeToString(make([]byte, 32))})
		_ = w.c.Close()
	}
	eventually(t, "source blocked", func() bool { return !a.guard.allow(net.ParseIP("127.0.0.1")) })
	c, err := dialTCP(t, a.peerLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	_ = writeFrame(c, frame{Type: "hello", Node: "eve", Nonce: randomHex(32), PAKE: hex.EncodeToString(make([]byte, 32))})
	if _, err := readFrame(newScanner(c), "hello"); err == nil {
		t.Fatal("blocked source answered")
	}
}

func TestAuthGuard(t *testing.T) {
	g := newAuthGuard()
	now := time.Unix(1000, 0)
	g.now = func() time.Time { return now }
	ip := net.ParseIP("203.0.113.7")
	for range guardFree {
		if d := g.fail(ip); d != 0 || !g.allow(ip) {
			t.Fatal("blocked within the free failures")
		}
	}
	if d := g.fail(ip); d != guardBase || g.allow(ip) {
		t.Fatalf("first block %v", d)
	}
	now = now.Add(guardBase)
	if !g.allow(ip) {
		t.Fatal("still blocked after the backoff")
	}
	if d := g.fail(ip); d != 2*guardBase {
		t.Fatalf("second block %v", d)
	}
	for range 30 {
		g.fail(ip)
	}
	if d := g.fail(ip); d != guardMax {
		t.Fatalf("block not capped: %v", d)
	}
	if g.allow(net.ParseIP("203.0.113.8")) != true {
		t.Fatal("another address blocked")
	}
	g.success(ip)
	if !g.allow(ip) {
		t.Fatal("success did not clear")
	}
	// One IPv6 /64 is one source.
	for range guardFree + 1 {
		g.fail(net.ParseIP("2001:db8:1:2::" + "1"))
	}
	if g.allow(net.ParseIP("2001:db8:1:2:ffff::9")) || !g.allow(net.ParseIP("2001:db8:1:3::1")) {
		t.Fatal("IPv6 sources not grouped by /64")
	}
	// Forgotten after guardForget.
	now = now.Add(guardForget + guardMax + time.Second)
	if d := g.fail(ip); d != 0 {
		t.Fatalf("old failures not forgotten: %v", d)
	}
	// Memory stays bounded.
	for i := range guardCap + 100 {
		g.fail(net.IPv4(10, byte(i>>16), byte(i>>8), byte(i)))
	}
	if len(g.m) > guardCap {
		t.Fatalf("%d sources tracked", len(g.m))
	}
}
