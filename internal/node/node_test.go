package node

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/UberMorgott/agent-link/internal/config"
)

const testSecret = "test-secret-0123456789abcdef"

type testNode struct {
	*Node
	peerLn net.Listener
	apiLn  net.Listener
	cancel context.CancelFunc
	done   chan struct{}
}

func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

// newTestNode builds a node on a fresh listener. peers maps name -> listener.
// The node is not serving until start is called.
func newTestNode(t *testing.T, name, secret string, areas []string, dataDir string, peerLn net.Listener, peers map[string]net.Listener) *testNode {
	t.Helper()
	cfg := config.Config{
		Node: name, Listen: peerLn.Addr().String(), API: "127.0.0.1:0",
		DataDir: dataDir, SecretEnv: "UNUSED", Areas: areas,
	}
	for p, ln := range peers {
		cfg.Peers = append(cfg.Peers, config.Peer{Name: p, Addr: ln.Addr().String()})
	}
	n, err := New(cfg, []byte(secret), nil)
	if err != nil {
		t.Fatal(err)
	}
	n.resendAfter = 300 * time.Millisecond
	n.resendTick = 50 * time.Millisecond
	n.backoffMin = 20 * time.Millisecond
	n.backoffMax = 200 * time.Millisecond
	n.handshakeTimeout = 3 * time.Second
	return &testNode{Node: n, peerLn: peerLn}
}

func (tn *testNode) start(t *testing.T) {
	t.Helper()
	tn.apiLn = listen(t)
	ctx, cancel := context.WithCancel(context.Background())
	tn.cancel, tn.done = cancel, make(chan struct{})
	go func() {
		defer close(tn.done)
		if err := tn.Serve(ctx, tn.peerLn, tn.apiLn); err != nil {
			t.Errorf("serve %s: %v", tn.cfg.Node, err)
		}
	}()
	t.Cleanup(tn.stop)
}

func (tn *testNode) stop() {
	if tn.cancel == nil {
		return
	}
	tn.cancel()
	<-tn.done
	tn.cancel = nil
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitHTTP calls GET /wait on the node's control API.
func waitHTTP(t *testing.T, tn *testNode, timeout string) (int, []Message) {
	t.Helper()
	resp, err := http.Get("http://" + tn.apiLn.Addr().String() + "/wait?timeout=" + timeout)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var msgs []Message
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&msgs); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode, msgs
}

// pair starts two connected nodes a and b.
func pair(t *testing.T, secretA, secretB string) (*testNode, *testNode) {
	t.Helper()
	lnA, lnB := listen(t), listen(t)
	a := newTestNode(t, "a", secretA, nil, t.TempDir(), lnA, map[string]net.Listener{"b": lnB})
	b := newTestNode(t, "b", secretB, []string{"dev"}, t.TempDir(), lnB, map[string]net.Listener{"a": lnA})
	a.start(t)
	b.start(t)
	return a, b
}

func TestHandshakeOK(t *testing.T) {
	a, b := pair(t, testSecret, testSecret)
	eventually(t, "a<->b connected", func() bool { return a.Connected("b") && b.Connected("a") })
	a.mu.Lock()
	areas := a.areas["b"]
	a.mu.Unlock()
	if !slices.Equal(areas, []string{"dev"}) {
		t.Fatalf("a learned areas %v for b, want [dev]", areas)
	}
}

func TestBadSecretRejected(t *testing.T) {
	a, b := pair(t, testSecret, "another-secret-0123456789")
	time.Sleep(time.Second)
	if a.Connected("b") || b.Connected("a") {
		t.Fatal("nodes with different secrets connected")
	}
}

func TestUnknownNodeRejected(t *testing.T) {
	lnA, lnB := listen(t), listen(t)
	a := newTestNode(t, "a", testSecret, nil, t.TempDir(), lnA, map[string]net.Listener{"b": lnB})
	// b does not list "a" as a peer.
	b := newTestNode(t, "b", testSecret, nil, t.TempDir(), lnB, map[string]net.Listener{"c": listen(t)})
	a.start(t)
	b.start(t)
	time.Sleep(time.Second)
	if a.Connected("b") || b.Connected("a") {
		t.Fatal("unknown node was accepted")
	}
}

func TestDirectSendWait(t *testing.T) {
	a, b := pair(t, testSecret, testSecret)
	if code, _ := waitHTTP(t, b, "50ms"); code != http.StatusNoContent {
		t.Fatalf("empty wait returned %d, want 204", code)
	}
	sent, err := a.Send("b", "hello b", "")
	if err != nil {
		t.Fatal(err)
	}
	code, msgs := waitHTTP(t, b, "10s")
	if code != http.StatusOK || len(msgs) != 1 || msgs[0].ID != sent.ID || msgs[0].Body != "hello b" || msgs[0].From != "a" {
		t.Fatalf("wait = %d %+v", code, msgs)
	}
	reply, err := b.Send("a", "hi a", sent.ID)
	if err != nil {
		t.Fatal(err)
	}
	code, msgs = waitHTTP(t, a, "10s")
	if code != http.StatusOK || len(msgs) != 1 || msgs[0].ID != reply.ID || msgs[0].ReplyTo != sent.ID {
		t.Fatalf("reply wait = %d %+v", code, msgs)
	}
	eventually(t, "outboxes drained", func() bool {
		pa, _ := a.store.pending("b")
		pb, _ := b.store.pending("a")
		return len(pa) == 0 && len(pb) == 0
	})
	if code, _ := waitHTTP(t, b, "50ms"); code != http.StatusNoContent {
		t.Fatalf("delivered message returned again: %d", code)
	}
}

func TestAreaFanout(t *testing.T) {
	lnA, lnB, lnC := listen(t), listen(t), listen(t)
	a := newTestNode(t, "a", testSecret, nil, t.TempDir(), lnA, map[string]net.Listener{"b": lnB, "c": lnC})
	b := newTestNode(t, "b", testSecret, []string{"dev"}, t.TempDir(), lnB, map[string]net.Listener{"a": lnA})
	c := newTestNode(t, "c", testSecret, []string{"dev", "ops"}, t.TempDir(), lnC, map[string]net.Listener{"a": lnA})
	for _, n := range []*testNode{a, b, c} {
		n.start(t)
	}
	eventually(t, "a connected to b and c", func() bool { return a.Connected("b") && a.Connected("c") })

	dev, err := a.Send("area:dev", "to dev", "")
	if err != nil {
		t.Fatal(err)
	}
	ops, err := a.Send("area:ops", "to ops", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Send("area:none", "nobody", ""); err == nil {
		t.Fatal("send to an area without subscribers succeeded")
	}
	_, gotB := waitHTTP(t, b, "10s")
	if len(gotB) != 1 || gotB[0].ID != dev.ID || gotB[0].Area != "dev" {
		t.Fatalf("b got %+v", gotB)
	}
	var gotC []string
	eventually(t, "c received dev and ops", func() bool {
		msgs, _ := c.store.claimUndelivered()
		for _, m := range msgs {
			gotC = append(gotC, m.ID)
		}
		return len(gotC) >= 2
	})
	slices.Sort(gotC)
	want := []string{dev.ID, ops.ID}
	slices.Sort(want)
	if !slices.Equal(gotC, want) {
		t.Fatalf("c got %v, want %v", gotC, want)
	}
	if code, _ := waitHTTP(t, b, "300ms"); code != http.StatusNoContent {
		t.Fatal("b received an ops message")
	}
}

func TestOfflineThenReconnect(t *testing.T) {
	lnA, lnB := listen(t), listen(t)
	dirB := t.TempDir()
	a := newTestNode(t, "a", testSecret, nil, t.TempDir(), lnA, map[string]net.Listener{"b": lnB})
	a.start(t)

	sent, err := a.Send("b", "while you were out", "")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if pending, _ := a.store.pending("b"); len(pending) != 1 {
		t.Fatalf("pending for offline b = %d, want 1", len(pending))
	}

	b := newTestNode(t, "b", testSecret, nil, dirB, lnB, map[string]net.Listener{"a": lnA})
	b.start(t)
	code, msgs := waitHTTP(t, b, "15s")
	if code != http.StatusOK || len(msgs) != 1 || msgs[0].ID != sent.ID {
		t.Fatalf("wait after reconnect = %d %+v", code, msgs)
	}

	// Restart b on the same port and data dir: the delivered message must not come back,
	// and a new message still arrives.
	addr := lnB.Addr().String()
	b.stop()
	eventually(t, "a sees b gone", func() bool { return !a.Connected("b") })
	lnB2, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	b2 := newTestNode(t, "b", testSecret, nil, dirB, lnB2, map[string]net.Listener{"a": lnA})
	b2.start(t)
	next, err := a.Send("b", "after restart", "")
	if err != nil {
		t.Fatal(err)
	}
	code, msgs = waitHTTP(t, b2, "15s")
	if code != http.StatusOK || len(msgs) != 1 || msgs[0].ID != next.ID {
		t.Fatalf("wait after restart = %d %+v", code, msgs)
	}
}

func TestDedupeOnResend(t *testing.T) {
	a, b := pair(t, testSecret, testSecret)
	sent, err := a.Send("b", "once", "")
	if err != nil {
		t.Fatal(err)
	}
	if code, msgs := waitHTTP(t, b, "10s"); code != http.StatusOK || len(msgs) != 1 {
		t.Fatalf("first delivery = %d %+v", code, msgs)
	}
	drained := func() bool { p, _ := a.store.pending("b"); return len(p) == 0 }
	eventually(t, "ack", drained)

	// Simulate a crash before the ACK was recorded: the same message is queued again.
	if err := a.enqueue("b", sent); err != nil {
		t.Fatal(err)
	}
	eventually(t, "resend acked", drained)
	if code, msgs := waitHTTP(t, b, "300ms"); code != http.StatusNoContent {
		t.Fatalf("duplicate delivered: %d %+v", code, msgs)
	}
	entries, err := b.store.recent(0)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, e := range entries {
		if e.Direction == "in" && e.ID == sent.ID {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("inbox holds %d copies, want 1", count)
	}
}

func TestAPIRejectsForeignOrigin(t *testing.T) {
	a, _ := pair(t, testSecret, testSecret)
	req, _ := http.NewRequest(http.MethodPost, "http://"+a.apiLn.Addr().String()+"/send",
		strings.NewReader(`{"to":"b","body":"x"}`))
	req.Header.Set("Origin", "http://evil.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status %d, want 403", resp.StatusCode)
	}
}

func TestInboundHookFiresOncePerNewMessage(t *testing.T) {
	lnA, lnB := listen(t), listen(t)
	a := newTestNode(t, "a", testSecret, nil, t.TempDir(), lnA, map[string]net.Listener{"b": lnB})
	b := newTestNode(t, "b", testSecret, nil, t.TempDir(), lnB, map[string]net.Listener{"a": lnA})
	got := make(chan Message, 10)
	b.SetInboundHook(func(m Message) { got <- m })
	a.start(t)
	b.start(t)
	sent, err := a.Send("b", "hook me", "")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-got:
		if m.ID != sent.ID || m.Body != "hook me" {
			t.Fatalf("hook got %+v", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hook not called")
	}
	select {
	case m := <-got:
		t.Fatalf("hook called twice: %+v", m)
	case <-time.After(700 * time.Millisecond): // longer than resendAfter
	}
}
