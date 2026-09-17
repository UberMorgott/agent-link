// Package node implements an agentlink peer: authenticated TCP sessions to
// known peers, a persistent outbox/inbox, and the loopback control API.
package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/UberMorgott/agent-link/internal/config"
)

// Node is one running agentlink peer.
type Node struct {
	cfg    config.Config
	secret []byte
	store  *store
	log    *slog.Logger
	peers  map[string]string // name -> addr

	mu    sync.Mutex
	conns map[string]*peerConn
	areas map[string][]string // last areas each peer announced

	wg sync.WaitGroup

	resendAfter      time.Duration
	resendTick       time.Duration
	backoffMin       time.Duration
	backoffMax       time.Duration
	handshakeTimeout time.Duration
}

// New opens the node's data directory. log may be nil.
func New(cfg config.Config, secret []byte, log *slog.Logger) (*Node, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if len(secret) < config.MinSecretLen {
		return nil, fmt.Errorf("shared secret must be at least %d bytes", config.MinSecretLen)
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	st, err := openStore(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	n := &Node{
		cfg: cfg, secret: secret, store: st, log: log.With("node", cfg.Node),
		peers: map[string]string{}, conns: map[string]*peerConn{}, areas: map[string][]string{},
		resendAfter: 5 * time.Second, resendTick: time.Second,
		backoffMin: 250 * time.Millisecond, backoffMax: 5 * time.Second,
		handshakeTimeout: 10 * time.Second,
	}
	for _, p := range cfg.Peers {
		n.peers[p.Name] = p.Addr
	}
	known, err := st.loadAreas()
	if err != nil {
		return nil, err
	}
	for peer, areas := range known {
		if _, ok := n.peers[peer]; ok {
			n.areas[peer] = areas
		}
	}
	return n, nil
}

// Serve runs the peer listener, the peer dialers and the control API until ctx
// is cancelled. apiLn must be bound to a loopback address.
func (n *Node) Serve(ctx context.Context, peerLn, apiLn net.Listener) error {
	if tcp, ok := apiLn.Addr().(*net.TCPAddr); !ok || !tcp.IP.IsLoopback() {
		return fmt.Errorf("control API must listen on loopback, got %s", apiLn.Addr())
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	srv := &http.Server{
		Handler:           n.apiHandler(),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	apiErr := make(chan error, 1)
	go func() { apiErr <- srv.Serve(apiLn) }()

	n.wg.Go(func() { n.acceptLoop(ctx, peerLn) })
	for name, addr := range n.peers {
		n.wg.Go(func() { n.dialLoop(ctx, name, addr) })
	}
	n.log.Info("serving", "listen", peerLn.Addr(), "api", apiLn.Addr())

	var err error
	select {
	case <-ctx.Done():
	case err = <-apiErr:
	}
	cancel()
	_ = peerLn.Close()
	_ = srv.Close()
	n.wg.Wait()
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}

// Connected reports whether a live session to peer exists.
func (n *Node) Connected(peer string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.conns[peer] != nil
}

// Send queues a message to a node name or to "area:NAME". Area messages fan
// out to every peer that announced the area.
func (n *Node) Send(to, body, replyTo string) (Message, error) {
	if body == "" {
		return Message{}, errors.New("empty body")
	}
	if replyTo != "" && !validID(replyTo) {
		return Message{}, fmt.Errorf("invalid reply_to %q", replyTo)
	}
	m := Message{ID: newID(), From: n.cfg.Node, To: to, Body: body, ReplyTo: replyTo, CreatedAt: time.Now().UTC()}
	var recipients []string
	if area, ok := strings.CutPrefix(to, AreaPrefix); ok {
		m.Area = area
		n.mu.Lock()
		for peer, areas := range n.areas {
			if slices.Contains(areas, area) {
				recipients = append(recipients, peer)
			}
		}
		n.mu.Unlock()
		if len(recipients) == 0 {
			return Message{}, fmt.Errorf("no known peer subscribed to area %q", area)
		}
		slices.Sort(recipients)
	} else {
		if _, ok := n.peers[to]; !ok {
			return Message{}, fmt.Errorf("unknown node %q", to)
		}
		recipients = []string{to}
	}
	for _, peer := range recipients {
		if err := n.enqueue(peer, m); err != nil {
			return Message{}, err
		}
	}
	return m, nil
}

func (n *Node) enqueue(peer string, m Message) error {
	if err := n.store.enqueue(peer, m); err != nil {
		return err
	}
	n.mu.Lock()
	pc := n.conns[peer]
	n.mu.Unlock()
	if pc != nil {
		pc.notify()
	}
	return nil
}

func (n *Node) acceptLoop(ctx context.Context, ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			n.log.Warn("accept", "err", err)
			continue
		}
		n.wg.Go(func() { n.handleInbound(ctx, c) })
	}
}

func (n *Node) handleInbound(ctx context.Context, c net.Conn) {
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	_ = c.SetDeadline(time.Now().Add(n.handshakeTimeout))
	sc := newScanner(c)
	peer, areas, err := n.acceptHandshake(c, sc)
	if err != nil {
		n.log.Warn("inbound handshake rejected", "remote", c.RemoteAddr(), "err", err)
		_ = c.Close()
		return
	}
	pc := newPeerConn(peer, peer, areas, c)
	if !n.register(pc) {
		_ = c.Close()
		return
	}
	if err := pc.write(frame{Type: "ok"}); err != nil {
		pc.close()
		n.unregister(pc)
		return
	}
	_ = c.SetDeadline(time.Time{})
	n.runConn(ctx, pc, sc)
}

func (n *Node) dialLoop(ctx context.Context, peer, addr string) {
	backoff := n.backoffMin
	for {
		if n.Connected(peer) {
			backoff = n.backoffMin
		} else if n.dialOnce(ctx, peer, addr) {
			backoff = n.backoffMin
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, n.backoffMax)
	}
}

// dialOnce connects, authenticates and serves one session. It reports whether
// a session was established.
func (n *Node) dialOnce(ctx context.Context, peer, addr string) bool {
	d := net.Dialer{Timeout: n.handshakeTimeout}
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		n.log.Debug("dial", "peer", peer, "err", err)
		return false
	}
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	_ = c.SetDeadline(time.Now().Add(n.handshakeTimeout))
	sc := newScanner(c)
	areas, err := n.dialHandshake(c, sc, peer)
	if err != nil {
		n.log.Warn("outbound handshake failed", "peer", peer, "err", err)
		_ = c.Close()
		return false
	}
	_ = c.SetDeadline(time.Time{})
	pc := newPeerConn(peer, n.cfg.Node, areas, c)
	if !n.register(pc) {
		_ = c.Close()
		return false
	}
	n.runConn(ctx, pc, sc)
	return true
}
