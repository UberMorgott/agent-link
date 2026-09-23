package node

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/UberMorgott/agent-link/internal/config"
)

// testProject is a project id and secret with the key derived from them.
type testProject struct {
	id, secret string
	key        []byte
}

func newTestProject(t *testing.T) testProject {
	t.Helper()
	id, err := config.NewProjectID()
	if err != nil {
		t.Fatal(err)
	}
	secret, err := config.NewProjectSecret()
	if err != nil {
		t.Fatal(err)
	}
	key, err := config.ProjectKey(id, config.ProjectEpoch, secret)
	if err != nil {
		t.Fatal(err)
	}
	return testProject{id: id, secret: secret, key: key}
}

// newProjectNode builds a node of project p on ln; peers maps name -> listener.
func newProjectNode(t *testing.T, name string, p testProject, ln net.Listener, peers map[string]net.Listener) *testNode {
	t.Helper()
	return newProjectNodeAt(t, name, p, t.TempDir(), ln, peers)
}

// restartProject stops tn and starts its project node again on its data
// directory and a new peer port.
func restartProject(t *testing.T, tn *testNode, p testProject, peers map[string]net.Listener) *testNode {
	t.Helper()
	tn.stop()
	next := newProjectNodeAt(t, tn.cfg.Node, p, tn.cfg.DataDir, listen(t), peers)
	next.start(t)
	return next
}

func newProjectNodeAt(t *testing.T, name string, p testProject, dir string, ln net.Listener, peers map[string]net.Listener) *testNode {
	t.Helper()
	cfg := config.Config{Node: name, Listen: ln.Addr().String(), API: "127.0.0.1:0", DataDir: dir, SecretEnv: "UNUSED", Project: p.id}
	for peer, pl := range peers {
		cfg.Peers = append(cfg.Peers, config.Peer{Name: peer, Addr: pl.Addr().String()})
	}
	n, err := New(cfg, p.key, nil)
	if err != nil {
		t.Fatal(err)
	}
	n.resendAfter, n.resendTick = 300*time.Millisecond, 50*time.Millisecond
	n.backoffMin, n.backoffMax = 20*time.Millisecond, 200*time.Millisecond
	n.handshakeTimeout = 3 * time.Second
	return &testNode{Node: n, peerLn: ln}
}

func TestProjectNodesConnect(t *testing.T) {
	p := newTestProject(t)
	lnA, lnB := listen(t), listen(t)
	a := newProjectNode(t, "a", p, lnA, map[string]net.Listener{"b": lnB})
	b := newProjectNode(t, "b", p, lnB, nil)
	a.start(t)
	b.start(t)
	eventually(t, "a<->b connected", func() bool { return a.Connected("b") && b.Connected("a") })
	if !a.PeerHas("b", CapProjects) || !b.PeerHas("a", CapProjects) {
		t.Fatal("projects capability not announced")
	}
	a.mu.Lock()
	pc := a.conns["b"]
	a.mu.Unlock()
	if !pc.pake || !pc.w.sealed() {
		t.Fatal("project session not sealed by the PAKE")
	}
}

// The project id is bound into CPace: the same key under another project id
// derives nothing in common.
func TestCPaceChannelBindsProject(t *testing.T) {
	p := newTestProject(t)
	other := newTestProject(t)
	sid := bytes.Repeat([]byte{7}, 32)
	d, err := newCPace(p.key, projectCI(p.id), sid)
	if err != nil {
		t.Fatal(err)
	}
	for name, ci := range map[string][]byte{"other project": projectCI(other.id), "legacy": []byte(cpaceCI)} {
		a, err := newCPace(p.key, ci, sid)
		if err != nil {
			t.Fatal(err)
		}
		kd, err1 := d.keys(a.share, true, cpaceAD("d", "1"), cpaceAD("a", "2"))
		ka, err2 := a.keys(d.share, false, cpaceAD("d", "1"), cpaceAD("a", "2"))
		if err1 != nil || err2 != nil || bytes.Equal(kd.acceptorTag, ka.acceptorTag) {
			t.Fatalf("%s: keys agree (%v, %v)", name, err1, err2)
		}
	}
	a, _ := newCPace(p.key, projectCI(p.id), sid)
	kd, _ := d.keys(a.share, true, cpaceAD("d", "1"), cpaceAD("a", "2"))
	ka, _ := a.keys(d.share, false, cpaceAD("d", "1"), cpaceAD("a", "2"))
	if !bytes.Equal(kd.acceptorTag, ka.acceptorTag) {
		t.Fatal("same project: keys differ")
	}
}

// A dialer that names the project but runs CPace under another project's
// channel id gets a tag that confirms nothing, and no session.
func TestProjectHandshakeUsesProjectCI(t *testing.T) {
	p := newTestProject(t)
	a := newProjectNode(t, "a", p, listen(t), nil)
	a.start(t)
	for _, ci := range [][]byte{projectCI(newTestProject(t).id), []byte(cpaceCI), projectCI(p.id)} {
		c, err := dialTCP(t, a.peerLn.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		nonce := randomHex(32)
		sid, _ := hex.DecodeString(nonce)
		cp, err := newCPace(p.key, ci, sid)
		if err != nil {
			t.Fatal(err)
		}
		hello, _ := json.Marshal(frame{Type: "hello", Node: "eve", Nonce: nonce, NodeID: "id-eve", PAKE: hex.EncodeToString(cp.share), Project: p.id})
		w := newWire(c)
		if err := w.writeData(hello); err != nil {
			t.Fatal(err)
		}
		f, err := w.readFrame("hello")
		if err != nil {
			t.Fatal(err)
		}
		yb, _ := hex.DecodeString(f.PAKE)
		keys, err := cp.keys(yb, true, cpaceAD("eve", "id-eve"), cpaceAD(f.Node, f.NodeID))
		if err != nil {
			t.Fatal(err)
		}
		confirmed := hex.EncodeToString(keys.acceptorTag) == f.MAC
		if want := bytes.Equal(ci, projectCI(p.id)); confirmed != want || f.Project != p.id {
			t.Fatalf("ci %q: confirmed %v, want %v; reply project %q", ci, confirmed, want, f.Project)
		}
		_ = c.Close()
	}
}

// sendHello dials tn, sends f and reports whether any hello came back.
func sendHello(t *testing.T, tn *testNode, f frame) bool {
	t.Helper()
	c, err := dialTCP(t, tn.peerLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	_ = writeFrame(c, f)
	_, err = readFrame(newScanner(c), "hello")
	return err == nil
}

func TestProjectNodeRefusesLegacyAndForeignHellos(t *testing.T) {
	p := newTestProject(t)
	a := newProjectNode(t, "a", p, listen(t), nil)
	a.start(t)
	share := func(key []byte, ci []byte) string {
		cp, err := newCPace(key, ci, make([]byte, 32))
		if err != nil {
			t.Fatal(err)
		}
		return hex.EncodeToString(cp.share)
	}
	other := newTestProject(t)
	cases := map[string]frame{
		"legacy MAC handshake": {Type: "hello", Node: "eve", Nonce: randomHex(32), Project: p.id},
		"no project":           {Type: "hello", Node: "eve", Nonce: randomHex(32), PAKE: share(p.key, []byte(cpaceCI))},
		"foreign project":      {Type: "hello", Node: "eve", Nonce: randomHex(32), PAKE: share(other.key, projectCI(other.id)), Project: other.id},
	}
	for name, f := range cases {
		if sendHello(t, a, f) {
			t.Errorf("%s: project node answered", name)
		}
	}
	// The control: a proper project hello is answered.
	if !sendHello(t, a, frame{Type: "hello", Node: "bob", Nonce: randomHex(32), PAKE: share(p.key, projectCI(p.id)), Project: p.id}) {
		t.Fatal("proper project hello not answered")
	}
}

func TestLegacyNodeRefusesProjectHello(t *testing.T) {
	a := openNode(t, time.Second, 5*time.Second)
	p := newTestProject(t)
	cp, err := newCPace([]byte(testSecret), []byte(cpaceCI), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	if sendHello(t, a, frame{Type: "hello", Node: "eve", Nonce: randomHex(32), PAKE: hex.EncodeToString(cp.share), Project: p.id}) {
		t.Fatal("legacy node answered a project hello")
	}
	if legacyHello(t, a, "bob") != true {
		t.Fatal("control: legacy hello not answered")
	}
}

// A dialer told the project is unknown there reports it as its problem; a
// project node never falls back to the legacy handshake.
func TestProjectDialerProblems(t *testing.T) {
	p := newTestProject(t)
	for name, c := range map[string]struct {
		reply frame
		want  error
	}{
		"unknown project": {frame{Type: "hello", Error: helloUnknownProject}, ErrUnknownProject},
		"other project":   {frame{Type: "hello", Node: "b", Nonce: randomHex(32), PAKE: "00", Project: newTestProject(t).id}, ErrWrongProject},
		"legacy acceptor": {frame{Type: "hello", Node: "b", Nonce: randomHex(32), MAC: "00", Project: p.id}, ErrLegacyRefused},
	} {
		t.Run(name, func(t *testing.T) {
			ln := listen(t)
			t.Cleanup(func() { _ = ln.Close() })
			go func() {
				for {
					conn, err := ln.Accept()
					if err != nil {
						return
					}
					_, _ = bufio.NewReader(conn).ReadBytes('\n')
					_ = writeFrame(conn, c.reply)
					_ = conn.Close()
				}
			}()
			a := newProjectNode(t, "a", p, listen(t), map[string]net.Listener{"b": ln})
			a.start(t)
			eventually(t, name, func() bool { return errors.Is(a.Problem(), c.want) })
		})
	}
}

func TestProjectMetaLWW(t *testing.T) {
	l := ProjectMeta{Lamport: 2, Writer: "bb"}
	for name, c := range map[string]struct {
		r    ProjectMeta
		want bool
	}{
		"higher lamport":          {ProjectMeta{Lamport: 3, Writer: "aa"}, true},
		"lower lamport":           {ProjectMeta{Lamport: 1, Writer: "zz"}, false},
		"tie, higher writer":      {ProjectMeta{Lamport: 2, Writer: "cc"}, true},
		"tie, lower writer":       {ProjectMeta{Lamport: 2, Writer: "aa"}, false},
		"same lamport and writer": {ProjectMeta{Lamport: 2, Writer: "bb"}, false},
	} {
		if got := metaNewer(c.r, l); got != c.want {
			t.Errorf("%s: metaNewer = %v, want %v", name, got, c.want)
		}
	}
	for _, bad := range []string{"", "   ", strings.Repeat("я", 81), "a\nb", "a\x00"} {
		if _, err := NormalizeProjectName(bad); !errors.Is(err, ErrProjectName) {
			t.Errorf("NormalizeProjectName(%q) = %v", bad, err)
		}
	}
	if got, err := NormalizeProjectName("  Проект  "); err != nil || got != "Проект" {
		t.Fatalf("NormalizeProjectName = %q, %v", got, err)
	}
}

// The creator's name reaches a joiner on connect; renames on both sides at
// once converge on the higher (Lamport, writer) everywhere, and survive a restart.
func TestProjectMetaGossip(t *testing.T) {
	p := newTestProject(t)
	lnA, lnB := listen(t), listen(t)
	a := newProjectNode(t, "a", p, lnA, map[string]net.Listener{"b": lnB})
	b := newProjectNode(t, "b", p, lnB, nil)
	if m := b.ProjectMeta(); m.Lamport != 0 || m.Name != "" || m.ID != p.id {
		t.Fatalf("joiner meta %+v", m)
	}
	created, err := a.Rename("Alpha")
	if err != nil || created.Lamport != 1 || created.Writer != a.ID() || created.CreatedAt.IsZero() {
		t.Fatalf("create: %+v, %v", created, err)
	}
	a.start(t)
	b.start(t)
	eventually(t, "b learns the name", func() bool { return b.ProjectMeta().Name == "Alpha" })

	// Concurrent renames at Lamport 2: the larger writer id wins on both.
	a.stop()
	if _, err := a.Rename("From A"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Rename("From B"); err != nil {
		t.Fatal(err)
	}
	want := "From A"
	if b.ID() > a.ID() {
		want = "From B"
	}
	a = restartProject(t, a, p, map[string]net.Listener{"b": lnB})
	eventually(t, "names converge", func() bool {
		return a.ProjectMeta().Name == want && b.ProjectMeta().Name == want
	})
	if a.ProjectMeta() != b.ProjectMeta() {
		t.Fatalf("metas differ: %+v vs %+v", a.ProjectMeta(), b.ProjectMeta())
	}

	// A rename while connected spreads at once.
	if _, err := b.Rename("Gamma"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a learns the rename", func() bool { return a.ProjectMeta().Name == "Gamma" })
	if _, err := a.Rename(" \t"); !errors.Is(err, ErrProjectName) {
		t.Fatalf("blank rename: %v", err)
	}
	legacy := openNode(t, time.Second, 5*time.Second)
	if _, err := legacy.Rename("x"); !errors.Is(err, ErrNotProject) {
		t.Fatalf("legacy rename: %v", err)
	}
}
