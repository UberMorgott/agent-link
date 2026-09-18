package node

import (
	"bufio"
	"encoding/hex"
	"net"
	"testing"
	"time"

	"github.com/UberMorgott/agent-link/internal/config"
)

// manualDial authenticates to a as name by hand, writing raw lines, so a test
// can play an older or a much newer peer. extras are written before each
// handshake frame (unknown frame types a newer peer might send); helloExtra is
// spliced into the hello object (e.g. `,"proto":9,"caps":["x"]`, or "" for a
// legacy hello with neither).
func manualDial(t *testing.T, a *testNode, name string, extras []string, helloExtra string) (net.Conn, *bufio.Scanner) {
	t.Helper()
	cfg := config.Config{Node: name, Listen: "127.0.0.1:0", API: "127.0.0.1:0", DataDir: t.TempDir(), SecretEnv: "UNUSED"}
	b, err := New(cfg, []byte(testSecret), nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.Dial("tcp", a.peerLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	line := func(s string) {
		t.Helper()
		if _, err := c.Write([]byte(s + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range extras {
		line(e)
	}
	nonce := randomHex(32)
	line(`{"type":"hello","node":"` + name + `","nonce":"` + nonce + `"` + helloExtra + `}`)
	sc := newScanner(c)
	hello, err := readFrame(sc, "hello")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range extras {
		line(e)
	}
	mac := hex.EncodeToString(b.mac("dial", name, hello.Node, nonce, hello.Nonce))
	line(`{"type":"auth","mac":"` + mac + `","future":[1,2,3]}`)
	if _, err := readFrame(sc, "ok"); err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Time{})
	return c, sc
}

// awaitAck reads frames from the node until it ACKs id.
func awaitAck(t *testing.T, c net.Conn, sc *bufio.Scanner, id string) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	defer func() { _ = c.SetReadDeadline(time.Time{}) }()
	for sc.Scan() {
		if f, ok := decodeFrame(sc.Bytes()); ok && f.Type == "ack" && f.ID == id {
			return
		}
	}
	t.Fatalf("no ack for %s: %v", id, sc.Err())
}

func TestCapabilitiesExchanged(t *testing.T) {
	a, b := pair(t, testSecret, testSecret)
	eventually(t, "a<->b connected", func() bool { return a.Connected("b") && b.Connected("a") })
	proto, caps, ok := a.PeerCaps("b")
	if !ok || proto != ProtocolVersion || len(caps) != len(Capabilities) {
		t.Fatalf("a sees b as proto %d caps %v ok %v", proto, caps, ok)
	}
	if !b.PeerHas("a", CapJobReattach) || b.PeerHas("a", "no-such-cap") {
		t.Fatal("PeerHas answers wrong")
	}
}

// A v0.3 peer sends no proto and no caps: it connects, has no capabilities,
// and its messages are delivered.
func TestLegacyPeerWithoutCaps(t *testing.T) {
	a := openNode(t, time.Second, 5*time.Second)
	c, sc := manualDial(t, a, "b", nil, "")
	eventually(t, "b connected", func() bool { return a.Connected("b") })
	proto, caps, ok := a.PeerCaps("b")
	if !ok || proto != 0 || len(caps) != 0 || a.PeerHas("b", CapHeartbeat) {
		t.Fatalf("legacy peer seen as proto %d caps %v", proto, caps)
	}
	m := Message{ID: newID(), From: "b", To: "a", Body: "from the past", CreatedAt: time.Now().UTC()}
	if err := writeFrame(c, frame{Type: "msg", Msg: &m}); err != nil {
		t.Fatal(err)
	}
	awaitAck(t, c, sc, m.ID)
}

// A much newer peer sends unknown frame types (during the handshake and
// after), unknown fields, fields of an unexpected type and even garbage lines:
// all are skipped, the session stays up and its message is delivered.
func TestFuturePeerUnknownFramesAndFields(t *testing.T) {
	a := openNode(t, time.Second, 5*time.Second)
	extras := []string{`{"type":"x-negotiate","offer":{"zstd":true}}`}
	c, sc := manualDial(t, a, "b", extras, `,"proto":99,"caps":["caps","quantum"],"pubkey":"abc","areas2":[1]`)
	eventually(t, "b connected", func() bool { return a.Connected("b") })
	if proto, _, _ := a.PeerCaps("b"); proto != 99 || !a.PeerHas("b", "quantum") {
		t.Fatalf("future caps not stored: proto %d", proto)
	}
	for _, l := range []string{
		`{"type":"x-stream","chunk":"AAAA","seq":1}`,
		`{"type":"ack","id":12345}`, // wrong JSON type for a known field
		`not json at all`,
		`[1,2,3]`,
		`{"no_type":true}`,
	} {
		if _, err := c.Write([]byte(l + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	id := newID()
	msg := `{"type":"msg","ttl":3,"msg":{"id":"` + id + `","from":"b","to":"a","body":"hi from v9",` +
		`"created_at":"2030-01-01T00:00:00Z","priority":"high","attachments":[{"n":1}]}}`
	if _, err := c.Write([]byte(msg + "\n")); err != nil {
		t.Fatal(err)
	}
	awaitAck(t, c, sc, id)
	if !a.Connected("b") {
		t.Fatal("session dropped")
	}
	entries, err := a.store.recent(0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		found = found || (e.ID == id && e.Body == "hi from v9")
	}
	if !found {
		t.Fatal("message with unknown fields not stored")
	}
}
