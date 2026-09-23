package node

import (
	"context"
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
	"golang.org/x/net/ipv4"

	"github.com/UberMorgott/agent-link/internal/config"
)

// LAN discovery (config.Discovery): every BeaconEvery a node sends a beacon
// datagram to UDP BeaconGroup:BeaconPort (multicast, and a directed broadcast
// on each IPv4 network) on every running interface except loopback, ZeroTier
// included, and listens on that port. A node that hears a beacon with its own
// network tag from a member it has no session with dials the sender's IP at
// the announced peer port. The beacon carries no secret: the tag is a
// memory-hard one-way function of the key (NetworkTag), and the TCP handshake
// still authenticates, so a recorded or forged beacon at most causes one dial.
// Still, a heard tag lets its hearer test code guesses offline at Argon2id
// cost; only a strong code makes that hopeless.
//
//	{"t":"agentlink","v":2,"net":"<16 hex>","node":"<name>","id":"<node id>","port":7420}
//
// A project context sends v3 beacons whose net is config.ProjectTag of the
// project key (128 random bits behind it, so no slow function is needed);
// older nodes ignore the unknown tag.
//
// Before v0.6 beacons were v1 with a PBKDF2 tag (legacyNetworkTag). A node
// still recognises those, so it dials older members it hears, but never
// sends one.
const (
	BeaconPort  = 7421
	BeaconGroup = "239.255.74.21"
	BeaconEvery = 5 * time.Second

	beaconType    = "agentlink"
	beaconMax     = 512
	beaconRedial  = 15 * time.Second // per address, whatever the beacons say
	beaconVersion = 2
	// projectBeaconVersion: a project context's beacon, net = config.ProjectTag.
	projectBeaconVersion = 3
	netTagByteLen        = 8
	// NetworkTag: Argon2id, RFC 9106's second recommended setting (64 MiB,
	// 3 passes, 4 lanes), salted with a fixed domain string.
	netTagSalt    = "agentlink/network-tag/v2"
	netTagTime    = 3
	netTagMemory  = 64 * 1024 // KiB
	netTagThreads = 4
	// legacyNetworkTag (v1 beacons).
	oldNetTagInfo   = "agentlink/network-tag/v1"
	oldNetTagRounds = 100_000
)

type beacon struct {
	T    string `json:"t"`
	V    int    `json:"v"`
	Net  string `json:"net"`
	Node string `json:"node"`
	ID   string `json:"id,omitempty"`
	Port int    `json:"port"`
}

// netTags caches NetworkTag by a hash of the key: a node restarts on every
// settings save, and 64 MiB of Argon2id is worth doing once per key.
var netTags sync.Map

// NetworkTag is the network id in beacons: Argon2id of the session key, so
// nodes of one network recognise each other without sending the key or the
// pairing code, and every code guess against a heard tag costs 64 MiB and
// three passes over it.
func NetworkTag(key []byte) string {
	id := sha256.Sum256(key)
	if v, ok := netTags.Load(id); ok {
		if tag, ok := v.(string); ok {
			return tag
		}
	}
	tag := hex.EncodeToString(argon2.IDKey(key, []byte(netTagSalt), netTagTime, netTagMemory, netTagThreads, netTagByteLen))
	netTags.Store(id, tag)
	return tag
}

// legacyNetworkTag is the v1 beacon tag, PBKDF2-SHA256 of the key; it is
// only compared with received beacons.
func legacyNetworkTag(key []byte) string {
	b, err := pbkdf2.Key(sha256.New, string(key), []byte(oldNetTagInfo), oldNetTagRounds, netTagByteLen)
	if err != nil { // only for parameters outside FIPS limits
		return ""
	}
	return hex.EncodeToString(b)
}

// beaconInterfaces are the running, non-loopback interfaces with IPv4 that
// can multicast or broadcast.
func beaconInterfaces() []net.Interface {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []net.Interface
	for _, ifi := range ifs {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 || ifi.Flags&(net.FlagMulticast|net.FlagBroadcast) == 0 {
			continue
		}
		if len(ipv4Nets(ifi)) > 0 {
			out = append(out, ifi)
		}
	}
	return out
}

func ipv4Nets(ifi net.Interface) []*net.IPNet {
	as, _ := ifi.Addrs()
	var out []*net.IPNet
	for _, a := range as {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLinkLocalUnicast() {
			out = append(out, ipn)
		}
	}
	return out
}

// broadcastAddr is the directed broadcast address of an IPv4 network, or nil
// for a /31 or /32.
func broadcastAddr(ipn *net.IPNet) net.IP {
	ip, mask := ipn.IP.To4(), ipn.Mask
	if len(mask) == net.IPv6len {
		mask = mask[12:]
	}
	if ones, bits := mask.Size(); bits != 32 || ones > 30 {
		return nil
	}
	out := make(net.IP, 4)
	for i := range out {
		out[i] = ip[i] | ^mask[i]
	}
	return out
}

// beaconSocket is where beacons are sent and heard: the port, multicast
// group, interval and interfaces (a Node's own, or its Hub's).
type beaconSocket struct {
	port   int
	group  net.IP
	every  time.Duration
	ifaces func() []net.Interface
	log    *slog.Logger
}

// run sends msgs() every interval and hands every heard beacon to heard,
// until ctx ends. Without a usable socket it logs once and returns: the
// node works without discovery. Its reader runs on wg.
func (s beaconSocket) run(ctx context.Context, wg *sync.WaitGroup, msgs func() [][]byte, heard func(beacon, net.IP)) {
	lc := net.ListenConfig{Control: reuseAddr}
	c, err := lc.ListenPacket(ctx, "udp4", ":"+strconv.Itoa(s.port))
	if err != nil {
		s.log.Warn("discovery off: cannot listen", "port", s.port, "err", err)
		return
	}
	defer func() { _ = c.Close() }() // also ends the reader
	p := ipv4.NewPacketConn(c)
	_ = p.SetMulticastLoopback(true) // other instances on this machine (tests, two users)
	_ = p.SetMulticastTTL(1)
	wg.Go(func() { s.read(ctx, p, heard) })

	group := &net.UDPAddr{IP: s.group, Port: s.port}
	joined := map[string]bool{}
	tick := time.NewTicker(s.every)
	defer tick.Stop()
	for {
		out := msgs()
		for _, ifi := range s.ifaces() {
			key := ifi.Name + "/" + strconv.Itoa(ifi.Index)
			if !joined[key] {
				if err := p.JoinGroup(&ifi, group); err != nil {
					s.log.Debug("discovery join", "iface", ifi.Name, "err", err)
				}
				joined[key] = true // retried only when the interface list changes
			}
			for _, msg := range out {
				s.send(p, ifi, group, msg)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// send writes one beacon on ifi: multicast, and a directed broadcast on each
// of its IPv4 networks.
func (s beaconSocket) send(p *ipv4.PacketConn, ifi net.Interface, group *net.UDPAddr, msg []byte) {
	if ifi.Flags&net.FlagMulticast != 0 && p.SetMulticastInterface(&ifi) == nil {
		if _, err := p.WriteTo(msg, nil, group); err != nil {
			s.log.Debug("discovery send", "iface", ifi.Name, "err", err)
		}
	}
	if ifi.Flags&net.FlagBroadcast == 0 {
		return
	}
	for _, ipn := range ipv4Nets(ifi) {
		if b := broadcastAddr(ipn); b != nil {
			_, _ = p.WriteTo(msg, nil, &net.UDPAddr{IP: b, Port: s.port})
		}
	}
}

func (s beaconSocket) read(ctx context.Context, p *ipv4.PacketConn, heard func(beacon, net.IP)) {
	buf := make([]byte, beaconMax)
	for {
		size, _, src, err := p.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.log.Debug("discovery read", "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}
		udp, ok := src.(*net.UDPAddr)
		if !ok {
			continue
		}
		var b beacon
		if json.Unmarshal(buf[:size], &b) != nil {
			continue
		}
		heard(b, udp.IP)
	}
}

// discoveryLoop runs the node's own beacon socket (a node without a Hub).
func (n *Node) discoveryLoop(ctx context.Context) {
	s := beaconSocket{port: n.beaconPort, group: n.beaconGroup, every: n.beaconEvery, ifaces: n.beaconIfaces, log: n.log}
	s.run(ctx, &n.wg, func() [][]byte {
		msg, err := json.Marshal(n.beacon())
		if err != nil {
			return nil
		}
		return [][]byte{msg}
	}, n.heard)
}

// beacon is this node's beacon: v2 with the Argon2id tag for the legacy
// network, v3 with the project tag (config.ProjectTag) for a project.
func (n *Node) beacon() beacon {
	v := beaconVersion
	if n.cfg.Project != "" {
		v = projectBeaconVersion
	}
	return beacon{T: beaconType, V: v, Net: n.netTag, Node: n.cfg.Node, ID: n.id, Port: n.port()}
}

// ownsTag reports whether a beacon's net is this node's network.
func (n *Node) ownsTag(tag string) bool {
	return n.netTag != "" && (tag == n.netTag || (n.oldNetTag != "" && tag == n.oldNetTag))
}

// heard dials the sender of a beacon of this network, unless it is this node,
// a removed member, already connected, or was dialed at that address lately.
func (n *Node) heard(b beacon, ip net.IP) {
	if b.T != beaconType || !n.ownsTag(b.Net) || b.ID == n.id || b.Node == n.cfg.Node ||
		!config.ValidName(b.Node) || b.Port <= 0 || b.Port > 65535 || ip == nil {
		return
	}
	if !n.running() {
		return // nothing could dial it; a later beacon will do
	}
	addr := net.JoinHostPort(ip.String(), strconv.Itoa(b.Port))
	now := time.Now()
	n.mu.Lock()
	skip := n.removedLocked(b.Node) || n.conns[b.Node] != nil || now.Sub(n.tried[addr]) < beaconRedial ||
		(!n.open && !n.known[b.Node]) || (n.members[b.Node] == nil && n.aliveLocked() >= maxMembers)
	if !skip {
		n.tried[addr] = now
		for a, at := range n.tried {
			if now.Sub(at) > time.Minute {
				delete(n.tried, a)
			}
		}
	}
	n.mu.Unlock()
	if skip {
		return
	}
	n.log.Info("discovered", "peer", b.Node, "addr", addr)
	n.spawn(func(ctx context.Context) { n.dialOnce(ctx, &target{addr: addr, name: b.Node, auto: true}) })
}
