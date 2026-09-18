package node

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"

	"github.com/UberMorgott/agent-link/internal/config"
)

// maxFrame bounds one newline-delimited JSON frame.
const maxFrame = 1 << 20

// frame is the single wire type: newline-delimited JSON over TCP.
//
// Handshake since v0.6 (D = dialer, A = acceptor), a CPace PAKE (pake.go)
// with sid = dialNonce, followed by key confirmation:
//
//	D -> A  hello{node, areas, proto, caps, node_id, nonce: dialNonce, pake: Ya}
//	A -> D  hello{node, areas, proto, caps, node_id, nonce: acceptNonce, pake: Yb, mac: acceptor tag}
//	D -> A  auth{mac: dialer tag}
//	A -> D  ok
//
// The tags are HMACs under keys derived from the CPace ISK, which binds both
// shares and both sides' names and node ids. D sends its tag only after A's
// checked out, so neither side ever hands out a value an attacker could test
// code guesses against offline.
//
// The legacy handshake (before v0.6; a hello without pake):
//
//	D -> A  hello{node, areas, proto, caps, nonce: dialNonce}
//	A -> D  hello{node, areas, proto, caps, nonce: acceptNonce, mac: HMAC("accept", D, A, dialNonce, acceptNonce)}
//	D -> A  auth{mac: HMAC("dial", D, A, dialNonce, acceptNonce)}
//	A -> D  ok
//
// Its MACs are keyed by the session key directly: one recorded (or, for A's,
// merely requested) MAC lets the code be brute-forced offline. It is
// therefore run only with a peer at a private address (legacyAllowed), and
// never with a peer name this node has had a PAKE session with.
//
// Then either side sends msg{msg} and answers with ack{id}, and sends hb (a
// heartbeat, since v0.3) every HeartbeatEvery. Node names are
// announced in hello and bound by both MACs, so a peer's name can be learned
// from the handshake; the shared key alone authenticates it.
//
// Versions interoperate without negotiation failures: since v0.4 hello also
// carries proto (ProtocolVersion) and caps (Capabilities); a peer that sends
// neither is an older version with no optional capabilities. Unknown frame
// types, unknown fields and frames that do not parse are skipped, never an
// error that closes the session. A new feature is only used towards a peer
// that announced its capability (Node.PeerHas).
//
// Since v0.5 hello also carries node_id (a random id kept in the data
// directory), app (the program version) and port (the dialer's peer port),
// and peers with the "members" capability exchange members frames (the
// gossiped membership table, see members.go). An acceptor that refuses a
// dialer because its name is taken by a live member with a smaller node_id
// answers hello{error: "name-taken"} without a MAC.
type frame struct {
	Type    string   `json:"type"`
	Node    string   `json:"node,omitempty"`
	Areas   []string `json:"areas,omitempty"`
	Nonce   string   `json:"nonce,omitempty"`
	MAC     string   `json:"mac,omitempty"`
	Proto   int      `json:"proto,omitempty"`
	Caps    []string `json:"caps,omitempty"`
	NodeID  string   `json:"node_id,omitempty"`
	App     string   `json:"app,omitempty"`
	Port    int      `json:"port,omitempty"`
	Error   string   `json:"error,omitempty"`
	Msg     *Message `json:"msg,omitempty"`
	ID      string   `json:"id,omitempty"`
	Members []Member `json:"members,omitempty"`
	PAKE    string   `json:"pake,omitempty"` // hex CPace share (since v0.6)
}

// ProtocolVersion is announced in hello. It only grows; it never gates a
// session, only capabilities do.
const ProtocolVersion = 6

// helloNameTaken is hello.error when the dialer's name belongs to another live member.
const helloNameTaken = "name-taken"

// Capabilities this node announces. Older peers announce none.
const (
	// CapHeartbeat: sends hb frames.
	CapHeartbeat = "hb"
	// CapActivity: status updates may carry Message.Activity.
	CapActivity = "activity"
	// CapCaps: announces proto and caps in hello.
	CapCaps = "caps"
	// CapJobReattach: a running job survives a restart of the answering app.
	CapJobReattach = "job-reattach"
	// CapMembers: exchanges the membership table (members frames) and dials
	// the members it learns.
	CapMembers = "members"
	// CapPAKE: authenticates with the CPace handshake.
	CapPAKE = "pake"
)

// Capabilities is the list sent in hello.
var Capabilities = []string{CapCaps, CapHeartbeat, CapActivity, CapJobReattach, CapMembers, CapPAKE}

// handshakeFrames are the frame types readFrame knows; any other type is skipped.
var handshakeFrames = map[string]bool{"hello": true, "auth": true, "ok": true}

// peerHello is what a peer announced in its hello.
type peerHello struct {
	name  string
	areas []string
	proto int
	caps  []string
	id    string // empty for a peer older than v0.5
	app   string
	port  int
	// pake: authenticated by the PAKE; false for the legacy handshake.
	pake bool
	// key is the session key the PAKE derived (nil for legacy). Nothing uses
	// it yet: the link itself is not encrypted by agent-link.
	key []byte
}

func helloOf(f frame) peerHello {
	return peerHello{name: f.Node, areas: f.Areas, proto: f.Proto, caps: f.Caps, id: f.NodeID, app: f.App, port: f.Port}
}

// hello is this node's hello frame.
func (n *Node) hello(nonce, mac string) frame {
	return frame{Type: "hello", Node: n.cfg.Node, Areas: n.cfg.Areas, Nonce: nonce, MAC: mac,
		Proto: ProtocolVersion, Caps: Capabilities, NodeID: n.id, App: n.appVersion, Port: n.port()}
}

func newScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxFrame)
	return sc
}

// readFrame returns the next handshake frame, which must be of type want.
// Frames of a type the handshake does not know (a newer peer's extras) and
// frames that do not parse are skipped; the handshake deadline bounds the wait.
func readFrame(sc *bufio.Scanner, want string) (frame, error) {
	for {
		if !sc.Scan() {
			if err := sc.Err(); err != nil {
				return frame{}, err
			}
			return frame{}, io.EOF
		}
		f, ok := decodeFrame(sc.Bytes())
		if !ok || !handshakeFrames[f.Type] {
			continue
		}
		if f.Type != want {
			return frame{}, fmt.Errorf("expected %q frame, got %q", want, f.Type)
		}
		return f, nil
	}
}

// decodeFrame parses one frame leniently: unknown fields are ignored, and a
// field of an unexpected JSON type leaves that field empty instead of failing
// the frame. It reports false only when the line is not a JSON object with a type.
func decodeFrame(line []byte) (frame, bool) {
	var f frame
	if err := json.Unmarshal(line, &f); err != nil {
		var te *json.UnmarshalTypeError
		if !errors.As(err, &te) {
			return frame{}, false
		}
		// Unmarshal keeps decoding past a type mismatch; the other fields are set.
	}
	return f, f.Type != ""
}

func writeFrame(w io.Writer, f frame) error {
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	_, err = w.Write(append(data, '\n'))
	return err
}

func (n *Node) mac(role, dialer, acceptor, dialNonce, acceptNonce string) []byte {
	h := hmac.New(sha256.New, n.secret)
	h.Write([]byte(strings.Join([]string{"agentlink/1", role, dialer, acceptor, dialNonce, acceptNonce}, "\n")))
	return h.Sum(nil)
}

func (n *Node) checkMAC(got string, want []byte) error {
	b, err := hex.DecodeString(got)
	if err != nil || !hmac.Equal(b, want) {
		return ErrAuth
	}
	return nil
}

func validNonce(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 32
}

// dialHandshake authenticates an outbound connection and returns what the peer
// announced (name, areas, protocol version, capabilities). want, when not empty, is the only name
// accepted; otherwise the announced name is learned.
func (n *Node) dialHandshake(c net.Conn, sc *bufio.Scanner, want string) (peerHello, error) {
	self := n.cfg.Node
	dialNonce := randomHex(32)
	sid, _ := hex.DecodeString(dialNonce)
	cp, err := newCPace(n.secret, sid)
	if err != nil {
		return peerHello{}, err
	}
	hello := n.hello(dialNonce, "")
	hello.PAKE = hex.EncodeToString(cp.share)
	if err := writeFrame(c, hello); err != nil {
		return peerHello{}, err
	}
	f, err := readFrame(sc, "hello")
	if err != nil {
		return peerHello{}, err
	}
	peer := f.Node
	switch {
	case f.Error == helloNameTaken:
		return peerHello{}, ErrNameTaken
	case peer == self:
		return peerHello{}, ErrSameName
	case !config.ValidName(peer):
		return peerHello{}, fmt.Errorf("invalid peer name %q", peer)
	case want != "" && peer != want:
		return peerHello{}, fmt.Errorf("%w: %q, want %q", ErrWrongPeer, peer, want)
	case !validNonce(f.Nonce):
		return peerHello{}, errors.New("invalid nonce")
	}
	var mac string
	h := helloOf(f)
	if f.PAKE != "" {
		yb, err := hex.DecodeString(f.PAKE)
		if err != nil {
			return peerHello{}, ErrAuth
		}
		keys, err := cp.keys(yb, true, cpaceAD(self, n.id), cpaceAD(peer, f.NodeID))
		if err != nil {
			return peerHello{}, ErrAuth
		}
		if err := n.checkMAC(f.MAC, keys.acceptorTag); err != nil {
			return peerHello{}, err
		}
		mac, h.pake, h.key = hex.EncodeToString(keys.dialerTag), true, keys.session
	} else {
		if err := n.legacyAllowed(c.RemoteAddr(), peer); err != nil {
			return peerHello{}, err
		}
		if err := n.checkMAC(f.MAC, n.mac("accept", self, peer, dialNonce, f.Nonce)); err != nil {
			return peerHello{}, err
		}
		mac = hex.EncodeToString(n.mac("dial", self, peer, dialNonce, f.Nonce))
	}
	if err := writeFrame(c, frame{Type: "auth", MAC: mac}); err != nil {
		return peerHello{}, err
	}
	if _, err := readFrame(sc, "ok"); err != nil {
		return peerHello{}, fmt.Errorf("peer rejected session: %w", err)
	}
	if !h.pake {
		n.log.Warn("legacy handshake: peer runs a version before v0.6", "peer", peer, "remote", c.RemoteAddr())
	}
	return h, nil
}

// acceptHandshake authenticates an inbound connection. The caller sends the
// final ok frame once the connection is registered.
func (n *Node) acceptHandshake(c net.Conn, sc *bufio.Scanner) (peerHello, error) {
	self := n.cfg.Node
	f, err := readFrame(sc, "hello")
	if err != nil {
		return peerHello{}, err
	}
	if f.Node == self {
		// Answer with our name so the dialer can tell it reached itself (or a twin name).
		_ = writeFrame(c, frame{Type: "hello", Node: self})
		return peerHello{}, ErrSameName
	}
	n.mu.Lock()
	known, removed := n.known[f.Node], n.removedLocked(f.Node)
	taken := n.takenLocked(f.Node, f.NodeID)
	n.mu.Unlock()
	if !config.ValidName(f.Node) || removed || (!n.open && !known) {
		return peerHello{}, fmt.Errorf("%w %q", ErrUnknownPeer, f.Node)
	}
	if taken {
		_ = writeFrame(c, frame{Type: "hello", Node: self, Error: helloNameTaken})
		return peerHello{}, fmt.Errorf("%w: %q", ErrNameTaken, f.Node)
	}
	if !validNonce(f.Nonce) {
		return peerHello{}, errors.New("invalid nonce")
	}
	peer, dialNonce, acceptNonce := f.Node, f.Nonce, randomHex(32)
	h := helloOf(f)
	if f.PAKE != "" {
		ya, err := hex.DecodeString(f.PAKE)
		if err != nil {
			return peerHello{}, ErrAuth
		}
		sid, _ := hex.DecodeString(dialNonce)
		cp, err := newCPace(n.secret, sid)
		if err != nil {
			return peerHello{}, err
		}
		keys, err := cp.keys(ya, false, cpaceAD(peer, f.NodeID), cpaceAD(self, n.id))
		if err != nil {
			return peerHello{}, ErrAuth
		}
		reply := n.hello(acceptNonce, hex.EncodeToString(keys.acceptorTag))
		reply.PAKE = hex.EncodeToString(cp.share)
		if err := writeFrame(c, reply); err != nil {
			return peerHello{}, err
		}
		auth, err := readFrame(sc, "auth")
		if err != nil {
			return peerHello{}, err
		}
		if err := n.checkMAC(auth.MAC, keys.dialerTag); err != nil {
			return peerHello{}, err
		}
		h.pake, h.key = true, keys.session
		return h, nil
	}
	// A legacy dialer: its MAC exchange leaks an offline-checkable value.
	if err := n.legacyAllowed(c.RemoteAddr(), peer); err != nil {
		return peerHello{}, err
	}
	mac := hex.EncodeToString(n.mac("accept", peer, self, dialNonce, acceptNonce))
	if err := writeFrame(c, n.hello(acceptNonce, mac)); err != nil {
		return peerHello{}, err
	}
	auth, err := readFrame(sc, "auth")
	if err != nil {
		return peerHello{}, err
	}
	if err := n.checkMAC(auth.MAC, n.mac("dial", peer, self, dialNonce, acceptNonce)); err != nil {
		return peerHello{}, err
	}
	n.log.Warn("legacy handshake: peer runs a version before v0.6", "peer", peer, "remote", c.RemoteAddr())
	return h, nil
}

// legacyAllowed permits the legacy handshake only with a peer at a private
// address (loopback, RFC 1918, fc00::/7, link-local, or a ZeroTier network of
// this machine), and never with a name that has had a PAKE session here: a
// newer peer does not fall back, so that would be a downgrade.
func (n *Node) legacyAllowed(remote net.Addr, peer string) error {
	tcp, ok := remote.(*net.TCPAddr)
	if !ok || !n.isPrivate(tcp.IP) {
		return fmt.Errorf("%w: %s is not a private address", ErrLegacyRefused, remote)
	}
	n.mu.Lock()
	seen := n.pakeSeen[peer]
	n.mu.Unlock()
	if seen {
		return fmt.Errorf("%w: %q authenticated with the PAKE before", ErrLegacyRefused, peer)
	}
	return nil
}

// privateAddr reports whether ip is private (config.PrivateIP) or in one of
// nets (this machine's ZeroTier networks). It is Node.isPrivate by default.
func privateAddr(ip net.IP, nets []*net.IPNet) bool {
	if config.PrivateIP(ip) {
		return true
	}
	for _, ipn := range nets {
		if ipn.Contains(ip) {
			return true
		}
	}
	return false
}

// zeroTierNets lists the networks of this machine's running ZeroTier adapters.
func zeroTierNets() []*net.IPNet {
	ifs, _ := net.Interfaces()
	var out []*net.IPNet
	for _, ifi := range ifs {
		if ifi.Flags&net.FlagUp == 0 || !strings.Contains(strings.ToLower(ifi.Name), "zerotier") {
			continue
		}
		as, _ := ifi.Addrs()
		for _, a := range as {
			if ipn, ok := a.(*net.IPNet); ok {
				out = append(out, ipn)
			}
		}
	}
	return out
}
