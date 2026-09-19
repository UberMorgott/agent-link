package node

import (
	"errors"
	"net"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/UberMorgott/agent-link/internal/config"
)

// newMeshNode builds a node that knows only the addresses of seeds (names
// are learned), with fast timers. It is not serving until start.
func newMeshNode(t *testing.T, name string, seeds ...net.Listener) *testNode {
	t.Helper()
	own := listen(t)
	cfg := config.Config{Node: name, Listen: own.Addr().String(), API: "127.0.0.1:0", DataDir: t.TempDir(), SecretEnv: "UNUSED"}
	for _, s := range seeds {
		cfg.Peers = append(cfg.Peers, config.Peer{Addr: s.Addr().String()})
	}
	n, err := New(cfg, []byte(testSecret), nil)
	if err != nil {
		t.Fatal(err)
	}
	n.backoffMin, n.backoffMax, n.handshakeTimeout = 20*time.Millisecond, 200*time.Millisecond, 3*time.Second
	n.resendAfter, n.resendTick, n.meshEvery = 300*time.Millisecond, 50*time.Millisecond, 50*time.Millisecond
	n.SetAppVersion("9.9.9")
	return &testNode{Node: n, peerLn: own}
}

func meshed(nodes ...*testNode) func() bool {
	return func() bool {
		for _, a := range nodes {
			for _, b := range nodes {
				if a != b && !a.Connected(b.cfg.Node) {
					return false
				}
			}
		}
		return true
	}
}

// A knows B, B knows C: A and C learn each other from B and connect. D, added
// by address on A alone, reaches everyone. Removing D on B spreads as a
// tombstone: every session to D ends, nobody dials it or lets it back in.
func TestMembersMeshAddRemove(t *testing.T) {
	c := newMeshNode(t, "c")
	b := newMeshNode(t, "b", c.peerLn)
	a := newMeshNode(t, "a", b.peerLn)
	for _, n := range []*testNode{a, b, c} {
		n.start(t)
	}
	eventually(t, "a, b, c fully meshed", meshed(a, b, c))

	d := newMeshNode(t, "d")
	d.start(t)
	if err := a.AddPeer(d.peerLn.Addr().String()); err != nil {
		t.Fatal(err)
	}
	eventually(t, "d meshed with everyone", meshed(a, b, c, d))
	ms := c.Members()
	if ms[0].Name != "c" || !ms[0].Self || len(ms) != 4 {
		t.Fatalf("c members %+v", ms)
	}
	for _, m := range ms[1:] {
		if !m.Online || m.App != "9.9.9" || len(m.Addrs) == 0 || m.Legacy {
			t.Fatalf("c sees %+v", m)
		}
	}
	if _, err := a.Send("", "to whom?", ""); !errors.Is(err, ErrAmbiguousPeer) {
		t.Fatalf("empty to with 3 peers: %v, want ErrAmbiguousPeer", err)
	}
	if _, err := a.Send("d", "hello d", ""); err != nil {
		t.Fatal(err)
	}

	if err := b.RemoveMember("d"); err != nil {
		t.Fatal(err)
	}
	if err := b.RemoveMember("b"); !errors.Is(err, ErrSelf) {
		t.Fatalf("remove self: %v", err)
	}
	gone := func() bool {
		for _, n := range []*testNode{a, b, c} {
			if n.Connected("d") || slices.Contains(n.Peers(), "d") {
				return false
			}
		}
		return !d.Connected("a") && !d.Connected("b") && !d.Connected("c")
	}
	eventually(t, "d removed everywhere", gone)
	eventually(t, "d told it was removed", func() bool { return errors.Is(d.Problem(), ErrRemoved) })
	time.Sleep(500 * time.Millisecond) // dial loops and d's redials run meanwhile
	if !gone() {
		t.Fatal("d came back")
	}
	if !meshed(a, b, c)() {
		t.Fatal("removing d broke the others' sessions")
	}
	if _, err := a.Send("d", "x", ""); !errors.Is(err, ErrUnknownPeer) {
		t.Fatalf("send to removed member: %v", err)
	}
	stored, err := c.store.loadMembers()
	if err != nil {
		t.Fatal(err)
	}
	if i := slices.IndexFunc(stored, func(m Member) bool { return m.Name == "d" }); i < 0 || !stored[i].Removed {
		t.Fatalf("c stored %+v", stored)
	}

	// Adding d's address by hand again brings it back for everyone.
	if err := c.AddPeer(d.peerLn.Addr().String()); err != nil {
		t.Fatal(err)
	}
	eventually(t, "d back in the mesh", meshed(a, b, c, d))
	eventually(t, "d no longer reports the removal", func() bool { return d.Problem() == nil })
}

// A restarted node keeps its table: it dials a member it learned by gossip.
func TestMembersPersisted(t *testing.T) {
	c := newMeshNode(t, "c")
	b := newMeshNode(t, "b", c.peerLn)
	a := newMeshNode(t, "a", b.peerLn)
	for _, n := range []*testNode{a, b, c} {
		n.start(t)
	}
	eventually(t, "meshed", meshed(a, b, c))
	b.stop()
	a.stop()
	cfg := a.cfg
	cfg.Peers = nil // only members.json knows c now
	own, err := listenTCP(t, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Listen = own.Addr().String()
	n, err := New(cfg, []byte(testSecret), nil)
	if err != nil {
		t.Fatal(err)
	}
	n.backoffMin, n.backoffMax, n.meshEvery = 20*time.Millisecond, 200*time.Millisecond, 50*time.Millisecond
	a2 := &testNode{Node: n, peerLn: own}
	a2.start(t)
	eventually(t, "restarted a dials c from its table", func() bool { return a2.Connected("c") })
}

// A v0.4 peer (no members capability) connects to a meshed node, is never
// sent a members frame and its messages are delivered.
func TestMembersLegacyPeer(t *testing.T) {
	b := newMeshNode(t, "b")
	a := newMeshNode(t, "a", b.peerLn)
	a.start(t)
	b.start(t)
	eventually(t, "a-b", meshed(a, b))
	conn, sc := manualDial(t, a, "old", nil, `,"proto":4,"caps":["caps","hb","activity","job-reattach"]`)
	eventually(t, "old connected", func() bool { return a.Connected("old") })
	if ms := a.Members(); !slices.ContainsFunc(ms, func(m MemberInfo) bool { return m.Name == "old" && m.Online && m.Legacy }) {
		t.Fatalf("a members %+v", ms)
	}
	m := Message{ID: newID(), From: "old", To: "a", Body: "still here", CreatedAt: time.Now().UTC()}
	if err := writeFrame(conn, frame{Type: "msg", Msg: &m}); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for sc.Scan() {
		f, _ := decodeFrame(sc.Bytes())
		if f.Type == frameMembers {
			t.Fatal("members frame sent to a peer without the capability")
		}
		if f.Type == "ack" && f.ID == m.ID {
			break
		}
	}
	if _, err := a.Send("old", "reply", m.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "b learned old from a", func() bool { return slices.Contains(b.Peers(), "old") })
}

// Two machines named x: the one with the smaller node id keeps the name on a
// third node, the other is told its name is taken.
func TestMembersNameCollision(t *testing.T) {
	a := newMeshNode(t, "a")
	x1 := newMeshNode(t, "x", a.peerLn)
	x2 := newMeshNode(t, "x", a.peerLn)
	a.start(t)
	win, lose := x1, x2
	if x2.ID() < x1.ID() {
		win, lose = x2, x1
	}
	lose.start(t)
	eventually(t, "loser connected while alone", func() bool { return a.Connected("x") })
	win.start(t)
	eventually(t, "winner holds the name", func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		pc := a.conns["x"]
		return pc != nil && pc.id == win.ID()
	})
	eventually(t, "loser told", func() bool { return errors.Is(lose.Problem(), ErrNameTaken) })
	time.Sleep(300 * time.Millisecond)
	a.mu.Lock()
	pc := a.conns["x"]
	a.mu.Unlock()
	if pc == nil || pc.id != win.ID() || win.Problem() != nil {
		t.Fatalf("after settling: holder %v, winner problem %v", pc, win.Problem())
	}
}

// A session registered just before a tombstone arrived must not bring the
// member back when it is noted afterwards.
func TestNoteSessionKeepsTombstone(t *testing.T) {
	a := newMeshNode(t, "a")
	id := newID()
	a.mergeMembers([]Member{{Name: "d", ID: id, Addrs: []string{"10.0.0.4:7420"}, Ver: 10}})
	a.mergeMembers([]Member{{Name: "d", ID: id, Ver: 20, Removed: true}})
	a.noteSession(&peerConn{peer: "d", id: id}, "10.0.0.5:7420")
	a.mu.Lock()
	got := *a.members["d"]
	a.mu.Unlock()
	if !got.Removed || got.Ver != 20 {
		t.Fatalf("tombstone after a late session note: %+v", got)
	}
}

func TestMergeLastWriterWins(t *testing.T) {
	a := newMeshNode(t, "a")
	id := newID()
	a.mergeMembers([]Member{{Name: "b", ID: id, Addrs: []string{"10.0.0.2:7420"}, Ver: 10}})
	a.mergeMembers([]Member{{Name: "b", ID: id, Addrs: []string{"10.0.0.3:7420"}, Ver: 5}}) // older: ignored
	a.mergeMembers([]Member{{Name: "b", Addrs: []string{"0.0.0.0:1", "bad", "10.0.0.4:7420"}, Ver: 20, ID: "junk"}})
	a.mu.Lock()
	got := *a.members["b"]
	a.mu.Unlock()
	if got.Ver != 10 || !slices.Equal(got.Addrs, []string{"10.0.0.2:7420"}) {
		t.Fatalf("after older and invalid records: %+v", got)
	}
	a.mergeMembers([]Member{{Name: "b", ID: id, Ver: 30, Removed: true}})
	if slices.Contains(a.Peers(), "b") {
		t.Fatal("tombstone not applied")
	}
	a.mergeMembers([]Member{{Name: "b", ID: id, Addrs: []string{"10.0.0.2:7420"}, Ver: 25}})
	if slices.Contains(a.Peers(), "b") {
		t.Fatal("older alive record resurrected a tombstone")
	}
	if !newer(&Member{Ver: 1, Removed: true}, &Member{Ver: 1}) || newer(&Member{Ver: 1}, &Member{Ver: 1, Removed: true}) {
		t.Fatal("tie not broken towards the tombstone")
	}
}

func TestMergeMemberInfoPublishesOnlyAfterPersistence(t *testing.T) {
	a := newMeshNode(t, "a")
	id := newID()
	a.mergeMembers([]Member{{Name: "b", ID: id, Ver: 10, Seen: 10, App: "1.0"}})
	changes := make(chan string, 2)
	a.SetChangeHook(func(topic string) {
		_ = a.Members() // proves the observer is outside Node.mu
		changes <- topic
	})

	a.mergeMembers([]Member{{Name: "b", ID: id, Ver: 10, Seen: 20, App: "2.0"}})
	select {
	case topic := <-changes:
		if topic != "members" {
			t.Fatalf("topic = %q, want members", topic)
		}
	case <-time.After(time.Second):
		t.Fatal("member info persistence did not publish")
	}
	stored, err := a.store.loadMembers()
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(stored, func(m Member) bool { return m.Name == "b" })
	if i < 0 || stored[i].Seen != 20 || stored[i].App != "2.0" {
		t.Fatalf("member info was not durable before callback: %+v", stored)
	}

	a.mergeMembers([]Member{{Name: "b", ID: id, Ver: 10, Seen: 20, App: "2.0"}})
	select {
	case topic := <-changes:
		t.Fatalf("no-op merge published %q", topic)
	default:
	}

	a.store.dir = filepath.Join(t.TempDir(), "missing")
	a.mergeMembers([]Member{{Name: "b", ID: id, Ver: 10, Seen: 30, App: "3.0"}})
	select {
	case topic := <-changes:
		t.Fatalf("failed persistence published %q", topic)
	default:
	}
}

func TestNoteSessionInfoPublishesOnlyWhenChangedAndPersisted(t *testing.T) {
	a := newMeshNode(t, "a")
	id := newID()
	now := time.Now().Unix()
	a.mergeMembers([]Member{{Name: "b", ID: id, Ver: 10, Seen: now, App: "1.0"}})
	changes := make(chan string, 2)
	a.SetChangeHook(func(topic string) {
		_ = a.Members()
		changes <- topic
	})

	pc := &peerConn{peer: "b", id: id, app: "2.0"}
	a.noteSession(pc, "")
	select {
	case topic := <-changes:
		if topic != "members" {
			t.Fatalf("topic = %q, want members", topic)
		}
	case <-time.After(time.Second):
		t.Fatal("session info persistence did not publish")
	}
	stored, err := a.store.loadMembers()
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(stored, func(m Member) bool { return m.Name == "b" })
	if i < 0 || stored[i].App != "2.0" {
		t.Fatalf("session info was not durable before callback: %+v", stored)
	}

	// Seen has one-second precision. With the current second and same app this
	// path persists nothing new and must not wake the UI.
	a.mu.Lock()
	a.members["b"].Seen = time.Now().Unix()
	a.mu.Unlock()
	a.noteSession(pc, "")
	select {
	case topic := <-changes:
		t.Fatalf("no-op session note published %q", topic)
	default:
	}
}
