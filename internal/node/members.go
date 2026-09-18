package node

import (
	"context"
	"errors"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/UberMorgott/agent-link/internal/config"
)

// Membership: every node keeps a table of all members of its network (same
// key), itself included, and exchanges it with each peer that announced
// CapMembers, on connect and whenever a merge changed it. Records are
// last-writer-wins per member by Ver (unix nanoseconds of the change; ties
// broken deterministically). A removal is a tombstone record that spreads the
// same way. Every node dials every live member it learns (full mesh, at most
// maxMembers), so an address added on one node reaches all of them.
//
// Name collisions: two machines with one name are told apart by their random
// node ids; the smaller id keeps the name on every node (register,
// acceptHandshake, mergeSelfLocked) and the other gets ErrNameTaken.
const (
	maxMembers   = 64
	maxAddrs     = 8
	frameMembers = "members"
)

// Member is one record of the membership table as gossiped and stored.
type Member struct {
	Name    string   `json:"name"`
	ID      string   `json:"id,omitempty"` // node id; empty for a peer older than v0.5
	Addrs   []string `json:"addrs,omitempty"`
	Ver     int64    `json:"ver"`
	Removed bool     `json:"removed,omitempty"`
	// Seen (unix seconds) and App are informational: the newest Seen wins and
	// a change of them alone is not gossiped on its own.
	Seen int64  `json:"seen,omitempty"`
	App  string `json:"app,omitempty"`
}

// MemberInfo is one member as listed by Members and GET /members.
type MemberInfo struct {
	Name   string    `json:"name"`
	Self   bool      `json:"self,omitempty"`
	Online bool      `json:"online"`
	Addrs  []string  `json:"addrs,omitempty"`
	Seen   time.Time `json:"seen,omitzero"`
	App    string    `json:"app,omitempty"`
	Proto  int       `json:"proto,omitempty"`
	// Legacy: connected, but an older version without membership exchange.
	Legacy bool `json:"legacy,omitempty"`
}

// newer reports whether record r replaces l.
func newer(r, l *Member) bool {
	switch {
	case r.Ver != l.Ver:
		return r.Ver > l.Ver
	case r.Removed != l.Removed:
		return r.Removed
	case r.ID != l.ID:
		return r.ID < l.ID
	default:
		return strings.Join(r.Addrs, " ") > strings.Join(l.Addrs, " ")
	}
}

// nextVer is a version newer than l's.
func nextVer(l *Member) int64 {
	v := time.Now().UnixNano()
	if l != nil && v <= l.Ver {
		v = l.Ver + 1
	}
	return v
}

// mergeAddrs returns the valid host:port addresses of front, then back,
// without duplicates, at most maxAddrs.
func mergeAddrs(front, back []string) []string {
	var out []string
	for _, a := range slices.Concat(front, back) {
		host, port, err := net.SplitHostPort(a)
		if err != nil || host == "" || slices.Contains(out, a) {
			continue
		}
		if p, err := strconv.Atoi(port); err != nil || p <= 0 || p > 65535 {
			continue
		}
		if ip := net.ParseIP(host); ip != nil && (ip.IsUnspecified() || ip.IsMulticast()) {
			continue
		}
		out = append(out, a)
		if len(out) == maxAddrs {
			break
		}
	}
	return out
}

func containsAll(have, want []string) bool {
	for _, w := range want {
		if !slices.Contains(have, w) {
			return false
		}
	}
	return true
}

// listenAddrs returns the addresses peers can dial this node at and its peer
// port: the listener's own address, or for a listener on all interfaces
// every IPv4 address of a running interface (ZeroTier first, no loopback or
// link-local).
func listenAddrs(addr net.Addr) ([]string, int) {
	tcp, ok := addr.(*net.TCPAddr)
	if !ok {
		return nil, 0
	}
	port := strconv.Itoa(tcp.Port)
	if !tcp.IP.IsUnspecified() {
		return []string{net.JoinHostPort(tcp.IP.String(), port)}, tcp.Port
	}
	ifs, _ := net.Interfaces()
	slices.SortStableFunc(ifs, func(a, b net.Interface) int {
		za, zb := strings.Contains(strings.ToLower(a.Name), "zerotier"), strings.Contains(strings.ToLower(b.Name), "zerotier")
		switch {
		case za == zb:
			return 0
		case za:
			return -1
		default:
			return 1
		}
	})
	var out []string
	for _, ifi := range ifs {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		as, _ := ifi.Addrs()
		for _, a := range as {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.To4() == nil || ipn.IP.IsLoopback() || ipn.IP.IsLinkLocalUnicast() {
				continue
			}
			out = append(out, net.JoinHostPort(ipn.IP.String(), port))
		}
	}
	return mergeAddrs(out, nil), tcp.Port
}

func (n *Node) port() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.listenPort
}

// removedLocked reports a tombstone for name (never for this node).
func (n *Node) removedLocked(name string) bool {
	m := n.members[name]
	return m != nil && m.Removed && name != n.cfg.Node
}

// takenLocked reports that a live session holds name with a node id smaller
// than id: that machine keeps the name.
func (n *Node) takenLocked(name, id string) bool {
	pc := n.conns[name]
	return pc != nil && pc.id != "" && id != "" && pc.id != id && pc.id < id
}

// refreshSelf puts this node's current id, addresses and version in its own record.
func (n *Node) refreshSelf() {
	n.mu.Lock()
	changed := n.assertSelfLocked(0)
	n.mu.Unlock()
	if changed {
		n.membersChanged()
	}
}

// assertSelfLocked makes this node's own record current, with a version
// above above. A tombstone with this node's id stays: someone removed it.
func (n *Node) assertSelfLocked(above int64) bool {
	self := n.cfg.Node
	l := n.members[self]
	if l != nil && l.ID == n.id {
		if l.Removed {
			return false
		}
		if above < l.Ver && containsAll(l.Addrs, n.selfAddrs) && l.App == n.appVersion {
			return false
		}
	}
	r := &Member{Name: self, ID: n.id, Addrs: mergeAddrs(n.selfAddrs, nil), App: n.appVersion, Seen: time.Now().Unix()}
	if l != nil && l.ID == n.id {
		r.Addrs = mergeAddrs(n.selfAddrs, l.Addrs)
	}
	r.Ver = max(nextVer(l), above+1)
	n.members[self] = r
	if errors.Is(n.problem, ErrNameTaken) || errors.Is(n.problem, ErrRemoved) {
		n.problem = nil
	}
	return true
}

// mergeSelfLocked applies a newer record about this node's own name.
func (n *Node) mergeSelfLocked(r *Member) bool {
	switch {
	case r.Removed && (r.ID == "" || r.ID == n.id):
		n.members[n.cfg.Node] = r
		n.problem = ErrRemoved
		n.log.Warn("this node was removed from the network by another member")
		return true
	case r.Removed || r.ID == "" || n.id < r.ID:
		// An old tombstone or record, or a machine that lost the name to this one.
		return n.assertSelfLocked(r.Ver)
	case r.ID == n.id:
		n.members[n.cfg.Node] = r
		n.assertSelfLocked(0) // keep every own address in it
		return true
	default:
		n.members[n.cfg.Node] = r
		n.problem = ErrNameTaken
		n.log.Warn("another member with a smaller node id uses this name", "name", r.Name)
		return true
	}
}

func (n *Node) aliveLocked() int {
	c := 0
	for _, m := range n.members {
		if !m.Removed {
			c++
		}
	}
	return c
}

// mergeMembers applies records a peer sent. Changes are stored and passed on.
func (n *Node) mergeMembers(recs []Member) {
	changed, seen := false, false
	var drop []*peerConn
	n.mu.Lock()
	for _, r := range recs {
		if !config.ValidName(r.Name) || r.Ver <= 0 || (r.ID != "" && !validID(r.ID)) {
			continue
		}
		r.Addrs = mergeAddrs(r.Addrs, nil)
		l := n.members[r.Name]
		if l != nil && r.Seen > l.Seen {
			l.Seen, seen = r.Seen, true
			if r.App != "" {
				l.App = r.App
			}
		}
		switch {
		case l != nil && !newer(&r, l):
			continue
		case l == nil && !r.Removed && n.aliveLocked() >= maxMembers:
			continue
		case l != nil:
			r.Seen = max(r.Seen, l.Seen)
			if r.App == "" {
				r.App = l.App
			}
		}
		if r.Name == n.cfg.Node {
			changed = n.mergeSelfLocked(&r) || changed
			continue
		}
		n.members[r.Name] = &r
		changed = true
		if r.Removed {
			delete(n.known, r.Name)
			if pc := n.conns[r.Name]; pc != nil {
				drop = append(drop, pc)
			}
		} else {
			n.known[r.Name] = true
		}
	}
	n.mu.Unlock()
	for _, pc := range drop {
		pc.close()
	}
	switch {
	case changed:
		n.membersChanged()
	case seen:
		n.persistMembers()
	}
}

// noteSession records a new session in pc's member record: its node id, the
// address this node dialed (or, inbound, the peer's address with the port it
// announced), when it was seen and its version. Then it sends the table.
func (n *Node) noteSession(pc *peerConn, dialed string) {
	var addrs []string
	switch {
	case dialed != "":
		addrs = []string{dialed}
	case pc.port > 0:
		if ra, ok := pc.c.RemoteAddr().(*net.TCPAddr); ok {
			addrs = []string{net.JoinHostPort(ra.IP.String(), strconv.Itoa(pc.port))}
		}
	}
	addrs = mergeAddrs(addrs, nil)
	now := time.Now().Unix()
	n.mu.Lock()
	l := n.members[pc.peer]
	changed := false
	switch {
	case l == nil || l.Removed:
		n.members[pc.peer] = &Member{Name: pc.peer, ID: pc.id, Addrs: addrs, Ver: nextVer(l), Seen: now, App: pc.app}
		changed = true
	case (pc.id != "" && pc.id != l.ID) || !containsAll(l.Addrs, addrs):
		r := &Member{Name: pc.peer, ID: l.ID, Addrs: mergeAddrs(addrs, l.Addrs), Ver: nextVer(l), Seen: now, App: l.App}
		if pc.id != "" {
			r.ID = pc.id
		}
		if pc.app != "" {
			r.App = pc.app
		}
		n.members[pc.peer] = r
		changed = true
	default:
		l.Seen = now
		if pc.app != "" {
			l.App = pc.app
		}
	}
	n.mu.Unlock()
	if changed {
		n.membersChanged()
		return
	}
	n.persistMembers()
	n.sendMembers(pc)
}

// touchSeen marks peer as seen now (its session just ended).
func (n *Node) touchSeen(peer string) {
	n.mu.Lock()
	m := n.members[peer]
	if m != nil {
		m.Seen = time.Now().Unix()
	}
	n.mu.Unlock()
	if m != nil {
		n.persistMembers()
	}
}

// revive brings back a removed member because the user added its address by hand.
func (n *Node) revive(name, id string) {
	n.mu.Lock()
	l := n.members[name]
	ok := l != nil && l.Removed && name != n.cfg.Node
	if ok {
		n.members[name] = &Member{Name: name, ID: id, Addrs: l.Addrs, Ver: nextVer(l), Seen: time.Now().Unix(), App: l.App}
		n.known[name] = true
	}
	n.mu.Unlock()
	if ok {
		n.log.Info("member added back", "peer", name)
		n.membersChanged()
	}
}

// RemoveMember removes name from the network: a tombstone that every member
// applies (ends its session, stops dialing it, refuses it) and stores. Only
// adding its address by hand (AddPeer) brings it back.
func (n *Node) RemoveMember(name string) error {
	if name == n.cfg.Node {
		return ErrSelf
	}
	n.mu.Lock()
	l := n.members[name]
	if (l == nil || l.Removed) && !n.known[name] {
		n.mu.Unlock()
		return ErrUnknownPeer
	}
	r := &Member{Name: name, Removed: true, Ver: nextVer(l)}
	if l != nil {
		r.ID, r.Addrs, r.Seen, r.App = l.ID, l.Addrs, l.Seen, l.App
	}
	n.members[name] = r
	delete(n.known, name)
	pc := n.conns[name]
	n.mu.Unlock()
	n.log.Info("member removed", "peer", name)
	if pc != nil {
		// It learns why before the session ends; the others learn from the gossip.
		if pc.has(CapMembers) {
			_ = pc.write(frame{Type: frameMembers, Members: n.snapshot()})
		}
		pc.close()
	}
	n.membersChanged()
	return nil
}

// Members lists this node first, then every member that is not removed,
// online ones first.
func (n *Node) Members() []MemberInfo {
	n.mu.Lock()
	defer n.mu.Unlock()
	self := MemberInfo{Name: n.cfg.Node, Self: true, Online: true, Addrs: slices.Clone(n.selfAddrs), App: n.appVersion, Proto: ProtocolVersion}
	var out []MemberInfo
	names := map[string]bool{}
	for name, m := range n.members {
		if !m.Removed {
			names[name] = true
		}
	}
	for name := range n.known {
		if !n.removedLocked(name) {
			names[name] = true
		}
	}
	delete(names, n.cfg.Node)
	for name := range names {
		info := MemberInfo{Name: name}
		if m := n.members[name]; m != nil {
			info.Addrs, info.App = slices.Clone(m.Addrs), m.App
			if m.Seen > 0 {
				info.Seen = time.Unix(m.Seen, 0).UTC()
			}
		}
		if pc := n.conns[name]; pc != nil {
			info.Online, info.Proto, info.Legacy = true, pc.proto, !pc.has(CapMembers)
			if pc.app != "" {
				info.App = pc.app
			}
		}
		out = append(out, info)
	}
	slices.SortFunc(out, func(a, b MemberInfo) int {
		if a.Online != b.Online {
			if a.Online {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
	return append([]MemberInfo{self}, out...)
}

func (n *Node) snapshot() []Member {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.snapshotLocked()
}

func (n *Node) snapshotLocked() []Member {
	out := make([]Member, 0, len(n.members))
	for _, m := range n.members {
		c := *m
		c.Addrs = slices.Clone(m.Addrs)
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b Member) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// membersChanged stores the table and sends it to every peer that exchanges it.
func (n *Node) membersChanged() {
	n.persistMembers()
	n.mu.Lock()
	snap := n.snapshotLocked()
	var to []*peerConn
	for _, pc := range n.conns {
		if pc.has(CapMembers) {
			to = append(to, pc)
		}
	}
	n.mu.Unlock()
	for _, pc := range to {
		n.wg.Go(func() {
			if pc.write(frame{Type: frameMembers, Members: snap}) != nil {
				pc.close()
			}
		})
	}
}

// sendMembers sends the table to one peer, if it exchanges it.
func (n *Node) sendMembers(pc *peerConn) {
	if !pc.has(CapMembers) {
		return
	}
	if pc.write(frame{Type: frameMembers, Members: n.snapshot()}) != nil {
		pc.close()
	}
}

func (n *Node) persistMembers() {
	n.saveMu.Lock()
	defer n.saveMu.Unlock()
	if err := n.store.saveMembers(n.snapshot()); err != nil {
		n.log.Warn("save members", "err", err)
	}
}

// meshLoop starts a dial loop for every live member with an address and for
// every address added by AddPeer.
func (n *Node) meshLoop(ctx context.Context) {
	tick := time.NewTicker(n.meshEvery)
	defer tick.Stop()
	for {
		n.mu.Lock()
		for name, m := range n.members {
			if name == n.cfg.Node || m.Removed || len(m.Addrs) == 0 || n.dialing[name] {
				continue
			}
			n.dialing[name] = true
			n.wg.Go(func() { n.memberDialLoop(ctx, name) })
		}
		for _, t := range n.targets {
			if !t.started {
				t.started = true
				n.wg.Go(func() { n.dialLoop(ctx, t) })
			}
		}
		n.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// memberDialLoop keeps a session to a member while it is in the table,
// trying each of its addresses (configured targets dial theirs themselves).
func (n *Node) memberDialLoop(ctx context.Context, name string) {
	defer func() {
		n.mu.Lock()
		delete(n.dialing, name)
		n.mu.Unlock()
	}()
	backoff := n.backoffMin
	for {
		n.mu.Lock()
		var addrs []string
		if m := n.members[name]; m != nil && !m.Removed {
			for _, a := range m.Addrs {
				if !n.hasTargetLocked(a) {
					addrs = append(addrs, a)
				}
			}
		}
		n.mu.Unlock()
		if len(addrs) == 0 {
			return
		}
		for _, a := range addrs {
			if ctx.Err() != nil || n.Connected(name) {
				backoff = n.backoffMin
				break
			}
			if n.dialOnce(ctx, &target{addr: a, name: name, auto: true}) {
				backoff = n.backoffMin
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, n.backoffMax)
	}
}

func (n *Node) hasTargetLocked(addr string) bool {
	for _, t := range n.targets {
		if t.addr == addr {
			return true
		}
	}
	return false
}
