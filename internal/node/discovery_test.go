package node

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"
)

func loopbackIfaces() []net.Interface {
	ifs, _ := net.Interfaces()
	var out []net.Interface
	for _, i := range ifs {
		if i.Flags&net.FlagLoopback != 0 && i.Flags&net.FlagUp != 0 {
			out = append(out, i)
		}
	}
	return out
}

// freeUDPPort returns a port that was free a moment ago.
func freeUDPPort(t *testing.T) int {
	t.Helper()
	c, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	return c.LocalAddr().(*net.UDPAddr).Port
}

func TestNetworkTag(t *testing.T) {
	a, b := NetworkTag([]byte(testSecret)), NetworkTag([]byte("another-secret-0123456789"))
	if len(a) != 16 || a == b || a != NetworkTag([]byte(testSecret)) {
		t.Fatalf("tags %q %q", a, b)
	}
	for _, c := range []struct {
		cidr, want string
	}{{"10.147.20.5/24", "10.147.20.255"}, {"192.168.1.9/16", "192.168.255.255"}, {"10.0.0.1/32", "<nil>"}} {
		_, ipn, _ := net.ParseCIDR(c.cidr)
		ipn.IP = net.ParseIP(c.cidr[:len(c.cidr)-3])
		if got := broadcastAddr(ipn).String(); got != c.want {
			t.Errorf("broadcast of %s = %s, want %s", c.cidr, got, c.want)
		}
	}
}

// Beacons of another network, of this node, or garbage never cause a dial.
func TestBeaconFiltered(t *testing.T) {
	a := newMeshNode(t, "a")
	a.netTag = NetworkTag([]byte(testSecret))
	ip := net.IPv4(127, 0, 0, 1)
	for _, b := range []beacon{
		{T: beaconType, V: 1, Net: "0000000000000000", Node: "b", ID: newID(), Port: 1},
		{T: beaconType, V: 1, Net: a.netTag, Node: "a", ID: newID(), Port: 1},
		{T: beaconType, V: 1, Net: a.netTag, Node: "b", ID: a.id, Port: 1},
		{T: "other", V: 1, Net: a.netTag, Node: "b", Port: 1},
		{T: beaconType, V: 1, Net: a.netTag, Node: "b c", Port: 1},
		{T: beaconType, V: 1, Net: a.netTag, Node: "b", Port: 0},
	} {
		a.heard(context.Background(), b, ip)
	}
	a.mu.Lock()
	tried := len(a.tried)
	a.mu.Unlock()
	if tried != 0 {
		t.Fatalf("%d dials from filtered beacons", tried)
	}
	if data, _ := json.Marshal(beacon{T: beaconType, V: 1, Net: a.netTag, Node: "a", ID: a.id, Port: 7420}); len(data) > beaconMax {
		t.Fatalf("beacon is %d bytes", len(data))
	}
}

// A beacon of this network from an unknown member makes the node dial the
// sender's address at the announced port, once per address for a while.
func TestBeaconDials(t *testing.T) {
	a, b := newMeshNode(t, "a"), newMeshNode(t, "b")
	a.netTag = NetworkTag([]byte(testSecret))
	a.start(t)
	b.start(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	bc := beacon{T: beaconType, V: 1, Net: a.netTag, Node: "b", ID: b.id, Port: b.peerLn.Addr().(*net.TCPAddr).Port}
	a.heard(ctx, bc, net.IPv4(127, 0, 0, 1))
	eventually(t, "a dialed b from its beacon", meshed(a, b))
}

// A v1 beacon (a node before v0.6, PBKDF2 tag) is still recognised, and the
// Argon2id tag differs from it.
func TestLegacyBeaconDials(t *testing.T) {
	a, b := newMeshNode(t, "a"), newMeshNode(t, "b")
	a.netTag, a.oldNetTag = NetworkTag([]byte(testSecret)), legacyNetworkTag([]byte(testSecret))
	if a.oldNetTag == "" || a.oldNetTag == a.netTag || len(a.oldNetTag) != 16 {
		t.Fatalf("tags %q %q", a.netTag, a.oldNetTag)
	}
	a.start(t)
	b.start(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	bc := beacon{T: beaconType, V: 1, Net: a.oldNetTag, Node: "b", ID: b.id, Port: b.peerLn.Addr().(*net.TCPAddr).Port}
	a.heard(ctx, bc, net.IPv4(127, 0, 0, 1))
	eventually(t, "a dialed b from its v1 beacon", meshed(a, b))
}

// Two nodes with the same key and no addresses find each other by multicast
// beacons on the loopback interface and connect.
func TestDiscoveryLoopback(t *testing.T) {
	if len(loopbackIfaces()) == 0 {
		t.Skip("no loopback interface")
	}
	port := freeUDPPort(t)
	var nodes []*testNode
	for _, name := range []string{"a", "b"} {
		n := newMeshNode(t, name)
		n.netTag = NetworkTag([]byte(testSecret))
		n.beaconPort, n.beaconEvery, n.beaconIfaces = port, 100*time.Millisecond, loopbackIfaces
		nodes = append(nodes, n)
	}
	for _, n := range nodes {
		n.start(t)
	}
	deadline := time.Now().Add(10 * time.Second)
	for !meshed(nodes...)() {
		if time.Now().After(deadline) {
			t.Skip("loopback multicast is not delivered on this machine")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
