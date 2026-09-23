package node

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
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

// dialTCP connects to addr, bounded by the test's context.
func dialTCP(t *testing.T, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(t.Context(), "tcp", addr)
}

// listenTCP listens on addr, bounded by the test's context.
func listenTCP(t *testing.T, addr string) (net.Listener, error) {
	var lc net.ListenConfig
	return lc.Listen(t.Context(), "tcp", addr)
}

func listen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := listenTCP(t, "127.0.0.1:0")
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
	eventually(t, tn.cfg.Node+" running", tn.running)
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
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+tn.apiLn.Addr().String()+"/wait?timeout="+timeout, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
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
		msgs, _ := c.store.claimUndelivered("")
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
	lnB2, err := listenTCP(t, addr)
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
	entries, err := b.store.recent(0, false)
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

func TestChangeHookRunsAfterDurableMessageOutsideLock(t *testing.T) {
	lnA, lnB := listen(t), listen(t)
	n := newTestNode(t, "a", testSecret, nil, t.TempDir(), lnA, map[string]net.Listener{"b": lnB})
	changed := make(chan string, 1)
	n.SetChangeHook(func(topic string) {
		_ = n.Peers() // deadlocks if the callback runs under Node.mu
		changed <- topic
	})
	if _, err := n.Send("b", "stored", ""); err != nil {
		t.Fatal(err)
	}
	select {
	case topic := <-changed:
		if topic != "messages" {
			t.Fatalf("topic = %q", topic)
		}
	case <-time.After(time.Second):
		t.Fatal("message mutation did not publish")
	}
	entries, err := n.Recent(0)
	if err != nil || len(entries) != 1 || entries[0].Body != "stored" {
		t.Fatalf("callback ran without durable message: entries=%+v err=%v", entries, err)
	}
}

func TestAPIRejectsForeignOrigin(t *testing.T) {
	a, _ := pair(t, testSecret, testSecret)
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+a.apiLn.Addr().String()+"/send",
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

// The hook runs before the ACK: a failing hook withholds it, so the sender
// resends and the hook sees the (already stored) message again.
func TestInboundHookGatesAck(t *testing.T) {
	lnA, lnB := listen(t), listen(t)
	a := newTestNode(t, "a", testSecret, nil, t.TempDir(), lnA, map[string]net.Listener{"b": lnB})
	b := newTestNode(t, "b", testSecret, nil, t.TempDir(), lnB, map[string]net.Listener{"a": lnA})
	var mu sync.Mutex
	calls := 0
	b.SetInboundHook(func(m Message) error {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 1 {
			return errors.New("disk full")
		}
		return nil
	})
	a.start(t)
	b.start(t)
	if _, err := a.Send("b", "hook me", ""); err != nil {
		t.Fatal(err)
	}
	eventually(t, "ack after the hook succeeded", func() bool { p, _ := a.store.pending("b"); return len(p) == 0 })
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Fatalf("hook calls = %d, want 2 (failed, then the resend)", calls)
	}
}

// Status updates are stored and deduplicated, never returned by wait, and
// fold into the sender's inbox entry for the request with the final answer.
func TestStatusUpdatesFoldIntoRequest(t *testing.T) {
	a, b := pair(t, testSecret, testSecret)
	req, err := a.Send("b", "do it", "")
	if err != nil {
		t.Fatal(err)
	}
	if code, msgs := waitHTTP(t, b, "10s"); code != http.StatusOK || len(msgs) != 1 {
		t.Fatalf("request wait = %d %+v", code, msgs)
	}
	latest := func() Entry {
		entries, err := a.Recent(0)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.Kind == KindStatus {
				t.Fatalf("status update listed: %+v", e)
			}
			if e.Direction == "out" && e.ID == req.ID {
				return e
			}
		}
		t.Fatal("request not listed")
		return Entry{}
	}
	for _, st := range []string{JobQueued, JobRunning} {
		m := Message{ID: DerivedID(req.ID, st), To: "a", ReplyTo: req.ID, Kind: KindStatus, JobStatus: st}
		for range 2 { // a resend of the same update is a duplicate
			if _, err := b.SendMessage(m); err != nil {
				t.Fatal(err)
			}
		}
		eventually(t, st+" seen by the sender", func() bool { return latest().JobStatus == st })
	}
	if _, err := b.SendMessage(Message{To: "a", ReplyTo: req.ID, Kind: KindStatus}); err != nil {
		t.Fatal(err) // bodiless status is allowed
	}
	if _, err := b.SendMessage(Message{To: "a", ReplyTo: req.ID}); err == nil {
		t.Fatal("bodiless reply accepted")
	}
	if code, msgs := waitHTTP(t, a, "500ms"); code != http.StatusNoContent {
		t.Fatalf("status update woke wait: %d %+v", code, msgs)
	}
	if _, err := b.SendMessage(Message{To: "a", ReplyTo: req.ID, Body: "done", JobStatus: JobCompleted}); err != nil {
		t.Fatal(err)
	}
	code, msgs := waitHTTP(t, a, "10s")
	if code != http.StatusOK || len(msgs) != 1 || msgs[0].JobStatus != JobCompleted || msgs[0].Body != "done" {
		t.Fatalf("final reply wait = %d %+v", code, msgs)
	}
	if e := latest(); e.JobStatus != JobCompleted || e.Answer != "done" {
		t.Fatalf("request entry = %+v", e)
	}
	count := 0
	a.store.mu.Lock()
	for _, r := range a.store.inbox {
		if r.Message.Kind == KindStatus {
			count++
		}
	}
	a.store.mu.Unlock()
	if count != 3 {
		t.Fatalf("stored status updates = %d, want 3 (duplicates dropped)", count)
	}
}

// newCodeNode builds a node keyed by a pairing code whose only peer is an
// address; the peer's name is learned from the handshake.
func newCodeNode(t *testing.T, name, code string, peerLn, own net.Listener) *testNode {
	t.Helper()
	key, err := config.KeyFromCode(code)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Node: name, Listen: own.Addr().String(), API: "127.0.0.1:0",
		DataDir: t.TempDir(), SecretEnv: "UNUSED",
	}
	if peerLn != nil {
		cfg.Peers = []config.Peer{{Addr: peerLn.Addr().String()}}
	}
	n, err := New(cfg, key, nil)
	if err != nil {
		t.Fatal(err)
	}
	n.backoffMin, n.backoffMax, n.handshakeTimeout = 20*time.Millisecond, 200*time.Millisecond, 3*time.Second
	n.resendAfter, n.resendTick = 300*time.Millisecond, 50*time.Millisecond
	return &testNode{Node: n, peerLn: own}
}

// Two nodes that know only each other's address and the same code (typed in
// different case) pair, learn each other's names and exchange messages.
func TestPairByAddressAndCode(t *testing.T) {
	lnA, lnB := listen(t), listen(t)
	a := newCodeNode(t, "morgott", "k7q2mx", lnB, lnA)
	b := newCodeNode(t, "nikita", "K7Q2MX", lnA, lnB)
	a.start(t)
	b.start(t)
	eventually(t, "names learned", func() bool { return a.Connected("nikita") && b.Connected("morgott") })
	if p := a.Peers(); len(p) != 1 || p[0] != "nikita" {
		t.Fatalf("a peers %v", p)
	}
	sent, err := a.Send("", "hello by code", "") // empty to: the only peer
	if err != nil {
		t.Fatal(err)
	}
	code, msgs := waitHTTP(t, b, "10s")
	if code != http.StatusOK || len(msgs) != 1 || msgs[0].ID != sent.ID || msgs[0].From != "morgott" {
		t.Fatalf("b wait = %d %+v", code, msgs)
	}
	if _, err := b.Send("morgott", "hi back", sent.ID); err != nil {
		t.Fatal(err)
	}
	if code, msgs := waitHTTP(t, a, "10s"); code != http.StatusOK || len(msgs) != 1 || msgs[0].ReplyTo != sent.ID {
		t.Fatalf("a wait = %d %+v", code, msgs)
	}
	if a.Problem() != nil || b.Problem() != nil {
		t.Fatalf("problems after pairing: %v / %v", a.Problem(), b.Problem())
	}
}

// Only one side knows the other's address: the other runs without a peer and
// still accepts the session.
func TestPairOneSidedAddress(t *testing.T) {
	lnA, lnB := listen(t), listen(t)
	a := newCodeNode(t, "morgott", "ABC123", lnB, lnA)
	b := newCodeNode(t, "nikita", "abc123", nil, lnB)
	a.start(t)
	b.start(t)
	eventually(t, "connected", func() bool { return a.Connected("nikita") && b.Connected("morgott") })
	if _, err := b.Send("", "reply without a configured peer", ""); err != nil {
		t.Fatal(err)
	}
	if code, _ := waitHTTP(t, a, "10s"); code != http.StatusOK {
		t.Fatalf("a wait = %d", code)
	}
}

func TestWrongCodeRejected(t *testing.T) {
	lnA, lnB := listen(t), listen(t)
	a := newCodeNode(t, "morgott", "ABC123", lnB, lnA)
	b := newCodeNode(t, "nikita", "ABC124", lnA, lnB)
	a.start(t)
	b.start(t)
	eventually(t, "auth problem reported", func() bool {
		return errors.Is(a.Problem(), ErrAuth) || errors.Is(b.Problem(), ErrAuth)
	})
	time.Sleep(300 * time.Millisecond)
	if a.Connected("nikita") || b.Connected("morgott") || len(a.Peers()) != 0 || len(b.Peers()) != 0 {
		t.Fatal("nodes with different codes connected or learned a name")
	}
	if _, err := a.Send("", "x", ""); !errors.Is(err, ErrUnknownPeer) {
		t.Fatalf("send before any session: %v, want ErrUnknownPeer", err)
	}
}

func TestSameNameReported(t *testing.T) {
	lnA, lnB := listen(t), listen(t)
	a := newCodeNode(t, "morgott", "ABC123", lnB, lnA)
	b := newCodeNode(t, "morgott", "ABC123", nil, lnB)
	a.start(t)
	b.start(t)
	eventually(t, "same-name problem", func() bool { return errors.Is(a.Problem(), ErrSameName) })
}
