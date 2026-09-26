package node

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"

	"github.com/UberMorgott/agent-link/internal/config"
)

// maxFrame bounds one frame: a newline-delimited JSON line of a legacy
// session, or the plaintext of one sealed record.
const maxFrame = 1 << 20

// frame is the single wire type: newline-delimited JSON over TCP, sealed in
// records (wire.go) once a PAKE session is confirmed.
//
// Handshake since v0.6 (D = dialer, A = acceptor), a CPace PAKE (pake.go)
// with sid = dialNonce, followed by key confirmation:
//
//	D -> A  hello{node, areas, proto, caps, node_id, nonce: dialNonce, pake: Ya}
//	A -> D  hello{node, areas, proto, caps, node_id, nonce: acceptNonce, pake: Yb, mac: acceptor tag}
//	D -> A  auth{mac: dialer tag over the transcript}
//	A -> D  ok                (the first sealed record)
//
// The tags are HMACs under keys derived from the CPace ISK, which binds both
// shares and both sides' names and node ids; the dialer's tag and the record
// keys also bind the hash of both raw hello lines. D sends its tag only after
// A's checked out, so neither side ever hands out a value an attacker could
// test code guesses against offline. From the ok on, every frame in both
// directions is a sealed record.
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
	// Project selects the context on the other side: a project id in both
	// hellos of a project session, absent for the legacy network. The hellos
	// are in the transcript, and the project id is in the CPace channel id.
	Project string `json:"project,omitempty"`
	// Presence: this node's sessions per shared area (presence frames, CapPresence).
	Presence []AreaPresence `json:"presence,omitempty"`
	// ProjectMeta: the project's shared name (project frames, CapProjects).
	ProjectMeta *ProjectMeta `json:"project_meta,omitempty"`
	// Att: one chunk of an attachment blob (att frames, CapAttachments).
	Att *attChunk `json:"att,omitempty"`
}

// ProtocolVersion is announced in hello. It only grows; it never gates a
// session, only capabilities do.
const ProtocolVersion = 6

// helloNameTaken is hello.error when the dialer's name belongs to another live member.
const helloNameTaken = "name-taken"

// helloUnknownProject is hello.error when the acceptor has no context for the
// dialer's project (sent by the Hub, without a MAC).
const helloUnknownProject = "unknown-project"

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
	// CapPAKE: authenticates with the CPace handshake and seals every frame
	// after it (wire.go). There is no separate cap for the records, so no
	// peer that runs the PAKE can be talked into a plain session.
	CapPAKE = "pake"
	// CapReceipts: reads KindReceipt messages (read receipts of chat messages).
	CapReceipts = "receipts-v1"
	// CapProjects: a project context (docs/plans/projects-v1.md); announced by
	// project nodes only, so every member of a project has it.
	CapProjects = "projects-v1"
)

// Capabilities is the list sent in hello by a legacy node; a project node adds CapProjects.
var Capabilities = []string{CapCaps, CapHeartbeat, CapActivity, CapJobReattach, CapMembers, CapPAKE, CapChat, CapReceipts, CapPresence, CapChatMembers, CapAttachments}

// caps is the capability list this node announces.
func (n *Node) caps() []string {
	if n.cfg.Project == "" {
		return Capabilities
	}
	return append(slices.Clone(Capabilities), CapProjects)
}

// cpaceCI is the CPace channel identifier of this node's sessions.
func (n *Node) cpaceCI() []byte {
	if n.cfg.Project == "" {
		return []byte(cpaceCI)
	}
	return projectCI(n.cfg.Project)
}

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
}

func helloOf(f frame) peerHello {
	return peerHello{name: f.Node, areas: f.Areas, proto: f.Proto, caps: f.Caps, id: f.NodeID, app: f.App, port: f.Port}
}

// hello is this node's hello frame.
func (n *Node) hello(nonce, mac string) frame {
	return frame{Type: "hello", Node: n.cfg.Node, Areas: n.cfg.Areas, Nonce: nonce, MAC: mac,
		Proto: ProtocolVersion, Caps: n.caps(), NodeID: n.id, App: n.appVersion, Port: n.port(), Project: n.cfg.Project}
}

// decodeFrame parses one frame leniently: unknown fields are ignored, and a
// field of an unexpected JSON type leaves that field empty instead of failing
// the frame. It reports false only when the line is not a JSON object with a type.
func decodeFrame(line []byte) (frame, bool) {
	var f frame
	if err := json.Unmarshal(line, &f); err != nil {
		if _, ok := errors.AsType[*json.UnmarshalTypeError](err); !ok {
			return frame{}, false
		}
		// Unmarshal keeps decoding past a type mismatch; the other fields are set.
	}
	return f, f.Type != ""
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
// accepted; otherwise the announced name is learned. A PAKE session leaves w sealed.
func (n *Node) dialHandshake(w *wire, want string) (peerHello, error) {
	self := n.cfg.Node
	remote := w.c.RemoteAddr()
	dialNonce := randomHex(32)
	sid, _ := hex.DecodeString(dialNonce)
	cp, err := newCPace(n.secret, n.cpaceCI(), sid)
	if err != nil {
		return peerHello{}, err
	}
	hello := n.hello(dialNonce, "")
	hello.PAKE = hex.EncodeToString(cp.share)
	helloLine, err := json.Marshal(hello)
	if err != nil {
		return peerHello{}, err
	}
	if err := w.writeData(helloLine); err != nil {
		return peerHello{}, err
	}
	f, err := w.readFrame("hello")
	if err != nil {
		return peerHello{}, err
	}
	peer := f.Node
	switch {
	case f.Error == helloUnknownProject:
		return peerHello{}, ErrUnknownProject
	case f.Error == helloNameTaken:
		return peerHello{}, ErrNameTaken
	case f.Project != n.cfg.Project:
		return peerHello{}, fmt.Errorf("%w: %q", ErrWrongProject, f.Project)
	case peer == self:
		return peerHello{}, ErrSameName
	case !config.ValidName(peer):
		return peerHello{}, fmt.Errorf("invalid peer name %q", peer)
	case want != "" && peer != want:
		return peerHello{}, fmt.Errorf("%w: %q, want %q", ErrWrongPeer, peer, want)
	case !validNonce(f.Nonce):
		return peerHello{}, errors.New("invalid nonce")
	}
	h := helloOf(f)
	if f.PAKE == "" {
		if n.cfg.Project != "" {
			return peerHello{}, fmt.Errorf("%w: a project session needs the PAKE", ErrLegacyRefused)
		}
		if err := n.legacyAllowed(remote, peer); err != nil {
			return peerHello{}, err
		}
		if err := n.checkMAC(f.MAC, n.mac("accept", self, peer, dialNonce, f.Nonce)); err != nil {
			return peerHello{}, err
		}
		if err := w.write(frame{Type: "auth", MAC: hex.EncodeToString(n.mac("dial", self, peer, dialNonce, f.Nonce))}); err != nil {
			return peerHello{}, err
		}
		if _, err := w.readFrame("ok"); err != nil {
			return peerHello{}, fmt.Errorf("peer rejected session: %w", err)
		}
		n.log.Warn("legacy handshake: peer runs a version before v0.6", "peer", peer, "remote", remote)
		return h, nil
	}
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
	th := transcript(helloLine, w.lastLine)
	if err := w.write(frame{Type: "auth", MAC: hex.EncodeToString(keys.dialerTag(th))}); err != nil {
		return peerHello{}, err
	}
	ck, err := keys.channel(th)
	if err != nil {
		return peerHello{}, err
	}
	if err := w.secure(ck, true); err != nil {
		return peerHello{}, err
	}
	// The ok is the first sealed record: it proves the acceptor saw the same transcript.
	if _, err := w.readFrame("ok"); err != nil {
		return peerHello{}, fmt.Errorf("peer rejected session: %w", err)
	}
	h.pake = true
	return h, nil
}

// acceptHandshake authenticates an inbound connection; a PAKE session leaves
// w sealed. The caller sends the final ok frame once the connection is registered.
func (n *Node) acceptHandshake(w *wire) (peerHello, error) {
	self := n.cfg.Node
	remote := w.c.RemoteAddr()
	f, err := w.readFrame("hello")
	if err != nil {
		return peerHello{}, err
	}
	dialerLine := w.lastLine
	// The Hub routes by project; a node checks again: a project node never
	// takes a legacy or a foreign project's hello, the legacy node never a
	// project's, and a project session is always the PAKE.
	switch {
	case f.Project != n.cfg.Project:
		return peerHello{}, fmt.Errorf("%w: %q", ErrWrongProject, f.Project)
	case n.cfg.Project != "" && f.PAKE == "":
		return peerHello{}, fmt.Errorf("%w: a project session needs the PAKE", ErrLegacyRefused)
	}
	if f.Node == self {
		// Answer with our name so the dialer can tell it reached itself (or a twin name).
		_ = w.write(frame{Type: "hello", Node: self, Project: n.cfg.Project})
		return peerHello{}, ErrSameName
	}
	n.mu.Lock()
	// A member that left a project and joined it again is a new node id.
	rejoin := n.rejoinLocked(f.Node, f.NodeID)
	known, removed := n.known[f.Node] || rejoin, n.removedLocked(f.Node) && !rejoin
	taken := n.takenLocked(f.Node, f.NodeID)
	n.mu.Unlock()
	if !config.ValidName(f.Node) || removed || (!n.open && !known) {
		return peerHello{}, fmt.Errorf("%w %q", ErrUnknownPeer, f.Node)
	}
	if taken {
		_ = w.write(frame{Type: "hello", Node: self, Error: helloNameTaken})
		return peerHello{}, fmt.Errorf("%w: %q", ErrNameTaken, f.Node)
	}
	if !validNonce(f.Nonce) {
		return peerHello{}, errors.New("invalid nonce")
	}
	peer, dialNonce, acceptNonce := f.Node, f.Nonce, randomHex(32)
	h := helloOf(f)
	if f.PAKE == "" {
		// A legacy dialer: its MAC exchange leaks an offline-checkable value.
		if err := n.legacyAllowed(remote, peer); err != nil {
			return peerHello{}, err
		}
		mac := hex.EncodeToString(n.mac("accept", peer, self, dialNonce, acceptNonce))
		if err := w.write(n.hello(acceptNonce, mac)); err != nil {
			return peerHello{}, err
		}
		auth, err := w.readFrame("auth")
		if err != nil {
			return peerHello{}, err
		}
		if err := n.checkMAC(auth.MAC, n.mac("dial", peer, self, dialNonce, acceptNonce)); err != nil {
			return peerHello{}, err
		}
		n.log.Warn("legacy handshake: peer runs a version before v0.6", "peer", peer, "remote", remote)
		return h, nil
	}
	ya, err := hex.DecodeString(f.PAKE)
	if err != nil {
		return peerHello{}, ErrAuth
	}
	sid, _ := hex.DecodeString(dialNonce)
	cp, err := newCPace(n.secret, n.cpaceCI(), sid)
	if err != nil {
		return peerHello{}, err
	}
	keys, err := cp.keys(ya, false, cpaceAD(peer, f.NodeID), cpaceAD(self, n.id))
	if err != nil {
		return peerHello{}, ErrAuth
	}
	reply := n.hello(acceptNonce, hex.EncodeToString(keys.acceptorTag))
	reply.PAKE = hex.EncodeToString(cp.share)
	replyLine, err := json.Marshal(reply)
	if err != nil {
		return peerHello{}, err
	}
	if err := w.writeData(replyLine); err != nil {
		return peerHello{}, err
	}
	auth, err := w.readFrame("auth")
	if err != nil {
		return peerHello{}, err
	}
	th := transcript(dialerLine, replyLine)
	if err := n.checkMAC(auth.MAC, keys.dialerTag(th)); err != nil {
		return peerHello{}, err
	}
	ck, err := keys.channel(th)
	if err != nil {
		return peerHello{}, err
	}
	if err := w.secure(ck, false); err != nil {
		return peerHello{}, err
	}
	h.pake = true
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
