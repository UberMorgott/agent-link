package node

import (
	"net"
	"testing"
	"time"

	"github.com/UberMorgott/agent-link/internal/config"
)

// rawPeer authenticates a bare (sealed) TCP session to a as node name and returns it.
// Nothing runs on it: the test decides which frames the "peer" sends. What a
// writes is drained so its writes never block.
func rawPeer(t *testing.T, a *testNode, name string) *wire {
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
	w := newWire(c)
	if _, err := b.dialHandshake(w, ""); err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Time{})
	go func() {
		for {
			if _, err := w.next(); err != nil {
				return
			}
		}
	}()
	return w
}

// openNode starts a node with no configured peer (it accepts any
// authenticated name) and fast heartbeats.
func openNode(t *testing.T, every, timeout time.Duration) *testNode {
	t.Helper()
	a := newTestNode(t, "a", testSecret, nil, t.TempDir(), listen(t), nil)
	a.heartbeatEvery, a.heartbeatTimeout = every, timeout
	a.start(t)
	return a
}

// A peer that sent one heartbeat and then went silent (a dead link, a frozen
// machine) is dropped after the heartbeat timeout.
func TestSilentPeerIsDroppedAfterHeartbeat(t *testing.T) {
	a := openNode(t, 50*time.Millisecond, 300*time.Millisecond)
	w := rawPeer(t, a, "b")
	eventually(t, "b connected", func() bool { return a.Connected("b") })
	if err := w.write(frame{Type: frameHeartbeat}); err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	eventually(t, "b dropped", func() bool { return !a.Connected("b") })
	if d := time.Since(began); d < 250*time.Millisecond || d > 3*time.Second {
		t.Fatalf("dropped after %s, want about 300ms", d)
	}
}

// An older peer never sends heartbeats and must never be timed out.
func TestLegacyPeerWithoutHeartbeatStaysConnected(t *testing.T) {
	a := openNode(t, 50*time.Millisecond, 200*time.Millisecond)
	rawPeer(t, a, "b")
	eventually(t, "b connected", func() bool { return a.Connected("b") })
	time.Sleep(time.Second)
	if !a.Connected("b") {
		t.Fatal("a peer without heartbeats was disconnected")
	}
}

// Two current nodes keep an idle session alive with heartbeats alone.
func TestHeartbeatsKeepIdleSessionAlive(t *testing.T) {
	lnA, lnB := listen(t), listen(t)
	a := newTestNode(t, "a", testSecret, nil, t.TempDir(), lnA, map[string]net.Listener{"b": lnB})
	b := newTestNode(t, "b", testSecret, nil, t.TempDir(), lnB, map[string]net.Listener{"a": lnA})
	for _, n := range []*testNode{a, b} {
		n.heartbeatEvery, n.heartbeatTimeout = 50*time.Millisecond, 300*time.Millisecond
		n.start(t)
	}
	eventually(t, "connected", func() bool { return a.Connected("b") && b.Connected("a") })
	a.mu.Lock()
	pc := a.conns["b"]
	a.mu.Unlock()
	time.Sleep(time.Second)
	a.mu.Lock()
	same := a.conns["b"] == pc
	a.mu.Unlock()
	if !same {
		t.Fatal("an idle session with heartbeats was replaced")
	}
}

func noNews(t *testing.T, n *testNode, id string) Entry {
	t.Helper()
	entries, err := n.Recent(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.ID == id {
			return e
		}
	}
	t.Fatalf("request %s not listed", id)
	return Entry{}
}

// A request to a peer that stays disconnected is marked once NoNewsAfter passed.
func TestNoNewsWhilePeerOffline(t *testing.T) {
	a := newTestNode(t, "a", testSecret, nil, t.TempDir(), listen(t), map[string]net.Listener{"b": listen(t)})
	a.noNewsAfter = 150 * time.Millisecond
	a.start(t)
	m, err := a.Send("b", "anyone there?", "")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "no news mark", func() bool { return noNews(t, a, m.ID).NoNewsMin == 1 })
}

// While connected: silence about a delivered request is marked, a queued
// status is news enough, and a later running status clears the mark until
// the peer goes quiet again.
func TestNoNewsWhileConnected(t *testing.T) {
	a := openNode(t, time.Second, 5*time.Second)
	a.noNewsAfter = 200 * time.Millisecond
	w := rawPeer(t, a, "b")
	eventually(t, "b connected", func() bool { return a.Connected("b") })
	m, err := a.Send("b", "question", "")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "silent request marked", func() bool { return noNews(t, a, m.ID).NoNewsMin > 0 })

	status := func(label, job, activity string) {
		t.Helper()
		st := Message{
			ID: DerivedID(m.ID, label), From: "b", To: "a", ReplyTo: m.ID, Kind: KindStatus,
			JobStatus: job, Activity: activity, CreatedAt: time.Now().UTC(),
		}
		if err := w.write(frame{Type: "msg", Msg: &st}); err != nil {
			t.Fatal(err)
		}
	}
	status("queued", JobQueued, "")
	eventually(t, "queued clears the mark", func() bool {
		e := noNews(t, a, m.ID)
		return e.JobStatus == JobQueued && e.NoNewsMin == 0
	})
	time.Sleep(300 * time.Millisecond)
	if e := noNews(t, a, m.ID); e.NoNewsMin != 0 {
		t.Fatalf("queued request marked: %+v", e)
	}
	status("running-1", JobRunning, "Read docs/index.md")
	eventually(t, "activity listed", func() bool {
		e := noNews(t, a, m.ID)
		return e.JobStatus == JobRunning && e.Activity == "Read docs/index.md" && e.NoNewsMin == 0
	})
	eventually(t, "quiet running request marked", func() bool { return noNews(t, a, m.ID).NoNewsMin > 0 })
}
