package node

import (
	"context"
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"strconv"
	"time"

	"golang.org/x/net/ipv4"

	"github.com/UberMorgott/agent-link/internal/config"
)

// LAN discovery (config.Discovery): every BeaconEvery a node sends a beacon
// datagram to UDP BeaconGroup:BeaconPort (multicast, and a directed broadcast
// on each IPv4 network) on every running interface except loopback, ZeroTier
// included, and listens on that port. A node that hears a beacon with its own
// network tag from a member it has no session with dials the sender's IP at
// the announced peer port. The beacon carries no secret: the tag is a slow
// one-way function of the key, and the TCP handshake still authenticates,
// so a recorded or forged beacon at most causes one dial.
//
//	{"t":"agentlink","v":1,"net":"<16 hex>","node":"<name>","id":"<node id>","port":7420}
const (
	BeaconPort  = 7421
	BeaconGroup = "239.255.74.21"
	BeaconEvery = 5 * time.Second

	beaconType    = "agentlink"
	beaconMax     = 512
	beaconRedial  = 15 * time.Second // per address, whatever the beacons say
	netTagInfo    = "agentlink/network-tag/v1"
	netTagRounds  = 100_000
	netTagByteLen = 8
)

type beacon struct {
	T    string `json:"t"`
	V    int    `json:"v"`
	Net  string `json:"net"`
	Node string `json:"node"`
	ID   string `json:"id,omitempty"`
	Port int    `json:"port"`
}

// NetworkTag is the network id in beacons: PBKDF2-SHA256 of the session key,
// so nodes of one network recognise each other without sending the key or
// the pairing code, and guessing a code from a tag costs 100k hashes per try.
func NetworkTag(key []byte) string {
	b, err := pbkdf2.Key(sha256.New, string(key), []byte(netTagInfo), netTagRounds, netTagByteLen)
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

// discoveryLoop sends and receives beacons until ctx ends. Without a usable
// socket it logs once and returns: the node works without discovery.
func (n *Node) discoveryLoop(ctx context.Context) {
	lc := net.ListenConfig{Control: reuseAddr}
	c, err := lc.ListenPacket(ctx, "udp4", ":"+strconv.Itoa(n.beaconPort))
	if err != nil {
		n.log.Warn("discovery off: cannot listen", "port", n.beaconPort, "err", err)
		return
	}
	defer func() { _ = c.Close() }() // also ends readBeacons
	p := ipv4.NewPacketConn(c)
	_ = p.SetMulticastLoopback(true) // other instances on this machine (tests, two users)
	_ = p.SetMulticastTTL(1)
	n.wg.Go(func() { n.readBeacons(ctx, p) })

	group := &net.UDPAddr{IP: n.beaconGroup, Port: n.beaconPort}
	joined := map[string]bool{}
	tick := time.NewTicker(n.beaconEvery)
	defer tick.Stop()
	for {
		msg, err := json.Marshal(beacon{T: beaconType, V: 1, Net: n.netTag, Node: n.cfg.Node, ID: n.id, Port: n.port()})
		if err != nil {
			return
		}
		for _, ifi := range n.beaconIfaces() {
			key := ifi.Name + "/" + strconv.Itoa(ifi.Index)
			if !joined[key] {
				if err := p.JoinGroup(&ifi, group); err != nil {
					n.log.Debug("discovery join", "iface", ifi.Name, "err", err)
				}
				joined[key] = true // retried only when the interface list changes
			}
			if ifi.Flags&net.FlagMulticast != 0 && p.SetMulticastInterface(&ifi) == nil {
				if _, err := p.WriteTo(msg, nil, group); err != nil {
					n.log.Debug("discovery send", "iface", ifi.Name, "err", err)
				}
			}
			if ifi.Flags&net.FlagBroadcast == 0 {
				continue
			}
			for _, ipn := range ipv4Nets(ifi) {
				if b := broadcastAddr(ipn); b != nil {
					_, _ = p.WriteTo(msg, nil, &net.UDPAddr{IP: b, Port: n.beaconPort})
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (n *Node) readBeacons(ctx context.Context, p *ipv4.PacketConn) {
	buf := make([]byte, beaconMax)
	for {
		size, _, src, err := p.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			n.log.Debug("discovery read", "err", err)
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
		n.heard(ctx, b, udp.IP)
	}
}

// heard dials the sender of a beacon of this network, unless it is this node,
// a removed member, already connected, or was dialed at that address lately.
func (n *Node) heard(ctx context.Context, b beacon, ip net.IP) {
	if b.T != beaconType || b.Net != n.netTag || b.ID == n.id || b.Node == n.cfg.Node ||
		!config.ValidName(b.Node) || b.Port <= 0 || b.Port > 65535 || ip == nil {
		return
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
	n.wg.Go(func() { n.dialOnce(ctx, &target{addr: addr, name: b.Node, auto: true}) })
}
