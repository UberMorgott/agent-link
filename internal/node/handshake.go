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
// Handshake (D = dialer, A = acceptor):
//
//	D -> A  hello{node, areas, proto, caps, nonce: dialNonce}
//	A -> D  hello{node, areas, proto, caps, nonce: acceptNonce, mac: HMAC("accept", D, A, dialNonce, acceptNonce)}
//	D -> A  auth{mac: HMAC("dial", D, A, dialNonce, acceptNonce)}
//	A -> D  ok
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
type frame struct {
	Type  string   `json:"type"`
	Node  string   `json:"node,omitempty"`
	Areas []string `json:"areas,omitempty"`
	Nonce string   `json:"nonce,omitempty"`
	MAC   string   `json:"mac,omitempty"`
	Proto int      `json:"proto,omitempty"`
	Caps  []string `json:"caps,omitempty"`
	Msg   *Message `json:"msg,omitempty"`
	ID    string   `json:"id,omitempty"`
}

// ProtocolVersion is announced in hello. It only grows; it never gates a
// session, only capabilities do.
const ProtocolVersion = 4

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
)

// Capabilities is the list sent in hello.
var Capabilities = []string{CapCaps, CapHeartbeat, CapActivity, CapJobReattach}

// handshakeFrames are the frame types readFrame knows; any other type is skipped.
var handshakeFrames = map[string]bool{"hello": true, "auth": true, "ok": true}

// peerHello is what a peer announced in its hello.
type peerHello struct {
	name  string
	areas []string
	proto int
	caps  []string
}

func helloOf(f frame) peerHello {
	return peerHello{name: f.Node, areas: f.Areas, proto: f.Proto, caps: f.Caps}
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
	if err := writeFrame(c, frame{Type: "hello", Node: self, Areas: n.cfg.Areas, Nonce: dialNonce, Proto: ProtocolVersion, Caps: Capabilities}); err != nil {
		return peerHello{}, err
	}
	f, err := readFrame(sc, "hello")
	if err != nil {
		return peerHello{}, err
	}
	peer := f.Node
	switch {
	case peer == self:
		return peerHello{}, ErrSameName
	case !config.ValidName(peer):
		return peerHello{}, fmt.Errorf("invalid peer name %q", peer)
	case want != "" && peer != want:
		return peerHello{}, fmt.Errorf("%w: %q, want %q", ErrWrongPeer, peer, want)
	case !validNonce(f.Nonce):
		return peerHello{}, errors.New("invalid nonce")
	}
	if err := n.checkMAC(f.MAC, n.mac("accept", self, peer, dialNonce, f.Nonce)); err != nil {
		return peerHello{}, err
	}
	mac := hex.EncodeToString(n.mac("dial", self, peer, dialNonce, f.Nonce))
	if err := writeFrame(c, frame{Type: "auth", MAC: mac}); err != nil {
		return peerHello{}, err
	}
	if _, err := readFrame(sc, "ok"); err != nil {
		return peerHello{}, fmt.Errorf("peer rejected session: %w", err)
	}
	return helloOf(f), nil
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
	known := n.known[f.Node]
	n.mu.Unlock()
	if !config.ValidName(f.Node) || (!n.open && !known) {
		return peerHello{}, fmt.Errorf("%w %q", ErrUnknownPeer, f.Node)
	}
	if !validNonce(f.Nonce) {
		return peerHello{}, errors.New("invalid nonce")
	}
	peer, dialNonce, acceptNonce := f.Node, f.Nonce, randomHex(32)
	mac := hex.EncodeToString(n.mac("accept", peer, self, dialNonce, acceptNonce))
	if err := writeFrame(c, frame{Type: "hello", Node: self, Areas: n.cfg.Areas, Nonce: acceptNonce, MAC: mac, Proto: ProtocolVersion, Caps: Capabilities}); err != nil {
		return peerHello{}, err
	}
	auth, err := readFrame(sc, "auth")
	if err != nil {
		return peerHello{}, err
	}
	if err := n.checkMAC(auth.MAC, n.mac("dial", peer, self, dialNonce, acceptNonce)); err != nil {
		return peerHello{}, err
	}
	return helloOf(f), nil
}
