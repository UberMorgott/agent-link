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
// Then either side sends msg{msg} and answers with ack{id}.
type frame struct {
	Type  string   `json:"type"`
	Node  string   `json:"node,omitempty"`
	Areas []string `json:"areas,omitempty"`
	Nonce string   `json:"nonce,omitempty"`
	MAC   string   `json:"mac,omitempty"`
	Msg   *Message `json:"msg,omitempty"`
	ID    string   `json:"id,omitempty"`
}

var errAuth = errors.New("authentication failed")

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
		return errAuth
	}
	return nil
}

func validNonce(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 32
}

// dialHandshake authenticates an outbound connection to the expected peer and
// returns the areas it announced.
func (n *Node) dialHandshake(c net.Conn, sc *bufio.Scanner, peer string) ([]string, error) {
	self := n.cfg.Node
	dialNonce := randomHex(32)
	if err := writeFrame(c, frame{Type: "hello", Node: self, Areas: n.cfg.Areas, Nonce: dialNonce}); err != nil {
		return nil, err
	}
	f, err := readFrame(sc, "hello")
	if err != nil {
		return nil, err
	}
	if f.Node != peer || !validNonce(f.Nonce) {
		return nil, fmt.Errorf("unexpected peer %q", f.Node)
	}
	if err := n.checkMAC(f.MAC, n.mac("accept", self, peer, dialNonce, f.Nonce)); err != nil {
		return nil, err
	}
	mac := hex.EncodeToString(n.mac("dial", self, peer, dialNonce, f.Nonce))
	if err := writeFrame(c, frame{Type: "auth", MAC: mac}); err != nil {
		return nil, err
	}
	if _, err := readFrame(sc, "ok"); err != nil {
		return nil, fmt.Errorf("peer rejected session: %w", err)
	}
	return f.Areas, nil
}

// acceptHandshake authenticates an inbound connection. The caller sends the
// final ok frame once the connection is registered.
func (n *Node) acceptHandshake(c net.Conn, sc *bufio.Scanner) (string, []string, error) {
	self := n.cfg.Node
	f, err := readFrame(sc, "hello")
	if err != nil {
		return "", nil, err
	}
	if _, known := n.peers[f.Node]; !known {
		return "", nil, fmt.Errorf("unknown node %q", f.Node)
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
