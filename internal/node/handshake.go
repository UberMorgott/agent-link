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
//	D -> A  hello{node, areas, nonce: dialNonce}
//	A -> D  hello{node, areas, nonce: acceptNonce, mac: HMAC("accept", D, A, dialNonce, acceptNonce)}
//	D -> A  auth{mac: HMAC("dial", D, A, dialNonce, acceptNonce)}
//	A -> D  ok
//
// Then either side sends msg{msg} and answers with ack{id}, and sends hb (a
// heartbeat, since v0.3) every HeartbeatEvery; unknown types are ignored. Node names are
// announced in hello and bound by both MACs, so a peer's name can be learned
// from the handshake; the shared key alone authenticates it.
type frame struct {
	Type  string   `json:"type"`
	Node  string   `json:"node,omitempty"`
	Areas []string `json:"areas,omitempty"`
	Nonce string   `json:"nonce,omitempty"`
	MAC   string   `json:"mac,omitempty"`
	Msg   *Message `json:"msg,omitempty"`
	ID    string   `json:"id,omitempty"`
}

func newScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxFrame)
	return sc
}

func readFrame(sc *bufio.Scanner, want string) (frame, error) {
	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return frame{}, err
		}
		return frame{}, io.EOF
	}
	var f frame
	if err := json.Unmarshal(sc.Bytes(), &f); err != nil {
		return frame{}, err
	}
	if f.Type != want {
		return frame{}, fmt.Errorf("expected %q frame, got %q", want, f.Type)
	}
	return f, nil
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

// dialHandshake authenticates an outbound connection and returns the peer's
// name and the areas it announced. want, when not empty, is the only name
// accepted; otherwise the announced name is learned.
func (n *Node) dialHandshake(c net.Conn, sc *bufio.Scanner, want string) (string, []string, error) {
	self := n.cfg.Node
	dialNonce := randomHex(32)
	if err := writeFrame(c, frame{Type: "hello", Node: self, Areas: n.cfg.Areas, Nonce: dialNonce}); err != nil {
		return "", nil, err
	}
	f, err := readFrame(sc, "hello")
	if err != nil {
		return "", nil, err
	}
	peer := f.Node
	switch {
	case peer == self:
		return "", nil, ErrSameName
	case !config.ValidName(peer):
		return "", nil, fmt.Errorf("invalid peer name %q", peer)
	case want != "" && peer != want:
		return "", nil, fmt.Errorf("%w: %q, want %q", ErrWrongPeer, peer, want)
	case !validNonce(f.Nonce):
		return "", nil, errors.New("invalid nonce")
	}
	if err := n.checkMAC(f.MAC, n.mac("accept", self, peer, dialNonce, f.Nonce)); err != nil {
		return "", nil, err
	}
	mac := hex.EncodeToString(n.mac("dial", self, peer, dialNonce, f.Nonce))
	if err := writeFrame(c, frame{Type: "auth", MAC: mac}); err != nil {
		return "", nil, err
	}
	if _, err := readFrame(sc, "ok"); err != nil {
		return "", nil, fmt.Errorf("peer rejected session: %w", err)
	}
	return peer, f.Areas, nil
}

// acceptHandshake authenticates an inbound connection. The caller sends the
// final ok frame once the connection is registered.
func (n *Node) acceptHandshake(c net.Conn, sc *bufio.Scanner) (string, []string, error) {
	self := n.cfg.Node
	f, err := readFrame(sc, "hello")
	if err != nil {
		return "", nil, err
	}
	if f.Node == self {
		// Answer with our name so the dialer can tell it reached itself (or a twin name).
		_ = writeFrame(c, frame{Type: "hello", Node: self})
		return "", nil, ErrSameName
	}
	n.mu.Lock()
	known := n.known[f.Node]
	n.mu.Unlock()
	if !config.ValidName(f.Node) || (!n.open && !known) {
		return "", nil, fmt.Errorf("%w %q", ErrUnknownPeer, f.Node)
	}
	if !validNonce(f.Nonce) {
		return "", nil, errors.New("invalid nonce")
	}
	peer, dialNonce, acceptNonce := f.Node, f.Nonce, randomHex(32)
	mac := hex.EncodeToString(n.mac("accept", peer, self, dialNonce, acceptNonce))
	if err := writeFrame(c, frame{Type: "hello", Node: self, Areas: n.cfg.Areas, Nonce: acceptNonce, MAC: mac}); err != nil {
		return "", nil, err
	}
	auth, err := readFrame(sc, "auth")
	if err != nil {
		return "", nil, err
	}
	if err := n.checkMAC(auth.MAC, n.mac("dial", peer, self, dialNonce, acceptNonce)); err != nil {
		return "", nil, err
	}
	return peer, f.Areas, nil
}
