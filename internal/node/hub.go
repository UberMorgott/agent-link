package node

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"
)

// Hub runs several nodes, one per context (the legacy network and each
// project), on one peer listener and one discovery socket
// (docs/plans/projects-v1.md §5.1). It owns what they share: the listener,
// the beacon socket, the handshake guard and pending-connection limit, a cap
// on live sessions over all contexts and a cap on concurrent outbound dials.
//
// An inbound connection's first line is its hello; the Hub picks the node by
// the hello's project ("" is the legacy node) and hands the connection over
// with that line and whatever was read after it still unread, together with
// the connection's lease on the limits. A hello for a project it does not run
// is answered with hello{error: "unknown-project"}, without a MAC.
type Hub struct {
	ln  net.Listener
	log *slog.Logger

	guard   *authGuard
	pending *connLimit
	dialSem chan struct{}

	handshakeTimeout time.Duration
	discovery        bool
	beacons          beaconSocket

	sessMu      sync.Mutex
	sessions    int // live sessions over all contexts
	maxSessions int

	mu     sync.Mutex
	ctx    context.Context // set by Start
	closed bool            // Wait was called: no more Add
	nodes  map[string]*hubEntry
	wg     sync.WaitGroup
}

// hubEntry is one context running on the Hub.
type hubEntry struct {
	n      *Node
	ln     *chanListener
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	// stopping: Remove runs; the entry routes nothing but keeps its id.
	stopping bool
}

// HubConfig configures a Hub. Log may be nil.
type HubConfig struct {
	// Discovery runs the beacon socket; each node with discovery on
	// (config.Discovery) sends its beacon on it and hears beacons of its tag.
	Discovery bool
	// DiscoveryPort replaces BeaconPort; 0 keeps it.
	DiscoveryPort int
	Log           *slog.Logger
}

// Hub limits.
const (
	// MaxHubProjects caps the project contexts of one Hub (the legacy node aside).
	MaxHubProjects = 32
	// maxHubSessions caps the live sessions over all contexts, both directions.
	maxHubSessions = 256
	// maxHubDials caps the concurrent outbound dial attempts over all contexts.
	maxHubDials = 64
	// scopeBeforeHello is the guard scope of connections that fail before
	// naming a context (no line, an oversize one); no project id or "" is it.
	scopeBeforeHello = "#hello"
)

// Hub errors.
var (
	ErrHubNotRunning    = errors.New("hub is not running")
	ErrContextExists    = errors.New("context already runs on the hub")
	ErrTooManyProjects  = errors.New("too many projects")
	errUnknownProjectID = errors.New("hello for a project this hub does not run")
)

// NewHub makes a Hub serving peers on ln. It does nothing until Start.
func NewHub(ln net.Listener, cfg HubConfig) *Hub {
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	port := cfg.DiscoveryPort
	if port == 0 {
		port = BeaconPort
	}
	return &Hub{
		ln: ln, log: log.With("hub", ln.Addr().String()),
		guard: newAuthGuard(), pending: newConnLimit(pendingTotal, pendingPerSource),
		dialSem:          make(chan struct{}, maxHubDials),
		handshakeTimeout: 10 * time.Second,
		discovery:        cfg.Discovery,
		beacons: beaconSocket{port: port, group: net.ParseIP(BeaconGroup).To4(), every: BeaconEvery,
			ifaces: beaconInterfaces, log: log},
		maxSessions: maxHubSessions,
		nodes:       map[string]*hubEntry{},
	}
}

// Start serves the listener (and the beacon socket) until ctx ends; the
// listener is closed then. Wait waits for everything the Hub started.
func (h *Hub) Start(ctx context.Context) {
	h.mu.Lock()
	h.ctx = ctx
	h.mu.Unlock()
	stop := context.AfterFunc(ctx, func() { _ = h.ln.Close() })
	h.wg.Go(func() {
		defer stop()
		h.acceptLoop(ctx)
	})
	if h.discovery {
		h.wg.Go(func() { h.beacons.run(ctx, &h.wg, h.beaconMsgs, h.heard) })
	}
}

// Wait returns once the Hub's context has ended and every node has stopped.
// Add fails from the moment Wait is called.
func (h *Hub) Wait() {
	h.mu.Lock()
	h.closed = true // no wg.Go after this: Wait may be waiting
	h.mu.Unlock()
	h.wg.Wait()
}

// Add runs n on the Hub as the context of its project (or the legacy one)
// until Remove or the end of the Hub's context. n must not run elsewhere.
func (h *Hub) Add(n *Node) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ctx == nil || h.ctx.Err() != nil || h.closed {
		return ErrHubNotRunning
	}
	pid := n.cfg.Project
	if h.nodes[pid] != nil {
		return fmt.Errorf("%w: %q", ErrContextExists, pid)
	}
	if pid != "" {
		count := 0
		for p := range h.nodes {
			if p != "" {
				count++
			}
		}
		if count >= MaxHubProjects {
			return ErrTooManyProjects
		}
	}
	n.hub = h
	ctx, cancel := context.WithCancel(h.ctx)
	e := &hubEntry{n: n, ln: newChanListener(h.ln.Addr()), ctx: ctx, cancel: cancel, done: make(chan struct{})}
	h.nodes[pid] = e
	h.wg.Go(func() {
		defer close(e.done)
		n.Run(ctx, e.ln)
	})
	return nil
}

// Remove stops the context of pid ("" for the legacy one) and waits for it.
// It reports whether that context ran. Until it has stopped, its id stays
// taken: an Add of the same project fails meanwhile.
func (h *Hub) Remove(pid string) bool {
	h.mu.Lock()
	e := h.nodes[pid]
	if e == nil || e.stopping {
		h.mu.Unlock()
		return false
	}
	e.stopping = true
	h.mu.Unlock()
	e.cancel()
	<-e.done
	h.mu.Lock()
	delete(h.nodes, pid)
	h.mu.Unlock()
	return true
}

// Node returns the running node of pid ("" for the legacy one), or nil.
func (h *Hub) Node(pid string) *Node {
	if e := h.entry(pid); e != nil {
		return e.n
	}
	return nil
}

// entry is the running context of pid, or nil (also while it stops).
func (h *Hub) entry(pid string) *hubEntry {
	h.mu.Lock()
	defer h.mu.Unlock()
	if e := h.nodes[pid]; e != nil && !e.stopping {
		return e
	}
	return nil
}

func (h *Hub) entries() []*hubEntry {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*hubEntry, 0, len(h.nodes))
	for _, e := range h.nodes {
		if !e.stopping {
			out = append(out, e)
		}
	}
	return out
}

func (h *Hub) acceptLoop(ctx context.Context) {
	for {
		c, err := h.ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			h.log.Warn("accept", "err", err)
			continue
		}
		h.wg.Go(func() { h.route(ctx, c) })
	}
}

// route reads c's first line under the Hub's limits and hands c to the node
// of its project.
func (h *Hub) route(ctx context.Context, c net.Conn) {
	var ip net.IP
	if tcp, ok := c.RemoteAddr().(*net.TCPAddr); ok {
		ip = tcp.IP
	}
	if !h.guard.allowIn(ip, scopeBeforeHello) {
		h.log.Debug("inbound connection closed: too many failed handshakes", "remote", c.RemoteAddr())
		_ = c.Close()
		return
	}
	if !h.pending.acquire(ip) {
		h.log.Debug("inbound connection closed: too many unauthenticated connections", "remote", c.RemoteAddr())
		_ = c.Close()
		return
	}
	lease := newInboundLease(h.guard, h.pending, ip)
	lease.scope = scopeBeforeHello
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	_ = c.SetDeadline(time.Now().Add(h.handshakeTimeout))
	br := bufio.NewReader(c)
	line, err := readFirstLine(br, maxHandshakeLine)
	if err != nil {
		h.log.Debug("inbound connection closed before a hello", "remote", c.RemoteAddr(), "err", err)
		lease.fail()
		_ = c.Close()
		return
	}
	var pid string
	if f, ok := decodeFrame(bytes.TrimRight(line, "\r\n")); ok {
		pid = f.Project
	}
	e := h.entry(pid)
	if e == nil {
		if pid != "" {
			if reply, err := json.Marshal(frame{Type: "hello", Error: helloUnknownProject}); err == nil {
				_, _ = c.Write(append(reply, '\n'))
			}
		}
		// Not a guess: a member may still dial a project this side left, or
		// join it before this side did. Only the slot is freed.
		h.log.Debug("inbound hello for no context", "remote", c.RemoteAddr(), "err", errUnknownProjectID)
		lease.release()
		_ = c.Close()
		return
	}
	// From here failures count against this context only (scopedKey).
	if !h.guard.allowIn(ip, pid) {
		h.log.Debug("inbound connection closed: too many failed handshakes", "remote", c.RemoteAddr(), "project", pid)
		lease.release()
		_ = c.Close()
		return
	}
	lease.scope = pid
	tail := bytes.Clone(peekBuffered(br))
	hc := &hubConn{Conn: c, r: io.MultiReader(bytes.NewReader(line), bytes.NewReader(tail), c), lease: lease}
	h.handoff(e, hc)
}

// handoff gives hc to e's node; a stopped node closes it and frees its slot.
func (h *Hub) handoff(e *hubEntry, hc *hubConn) {
	select {
	case e.ln.ch <- hc:
	case <-e.ln.done:
		hc.lease.release()
		_ = hc.Close()
	case <-e.ctx.Done():
		hc.lease.release()
		_ = hc.Close()
	}
}

// peekBuffered is what br has read from the connection beyond the first line.
func peekBuffered(br *bufio.Reader) []byte {
	b, _ := br.Peek(br.Buffered()) // never more than is buffered: no error
	return b
}

// readFirstLine reads one line, its newline included, of at most limit bytes.
// A line without a newline before EOF fails.
func readFirstLine(br *bufio.Reader, limit int) ([]byte, error) {
	var line []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if len(line)+len(chunk) > limit+1 {
			return nil, bufio.ErrTooLong
		}
		line = append(line, chunk...)
		switch {
		case err == nil:
			return line, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		default:
			return nil, err
		}
	}
}

// beaconMsgs are the beacons of every context with discovery on.
func (h *Hub) beaconMsgs() [][]byte {
	var out [][]byte
	for _, e := range h.entries() {
		if e.n.netTag == "" {
			continue
		}
		if msg, err := json.Marshal(e.n.beacon()); err == nil {
			out = append(out, msg)
		}
	}
	return out
}

// heard routes a beacon by its tag to the context of that network.
func (h *Hub) heard(b beacon, ip net.IP) {
	for _, e := range h.entries() {
		if e.n.ownsTag(b.Net) {
			e.n.heard(b, ip)
			return
		}
	}
}

// acquireDial takes an outbound dial slot of the Hub until release; a node
// without a Hub needs none. It fails only when ctx ends first.
func (h *Hub) acquireDial(ctx context.Context) (release func(), ok bool) {
	if h == nil {
		return func() {}, true
	}
	select {
	case h.dialSem <- struct{}{}:
	case <-ctx.Done():
		return nil, false
	}
	var once sync.Once
	return func() { once.Do(func() { <-h.dialSem }) }, true
}

// addSession counts a new live session, or reports false at the cap.
func (h *Hub) addSession() bool {
	if h == nil {
		return true
	}
	h.sessMu.Lock()
	defer h.sessMu.Unlock()
	if h.sessions >= h.maxSessions {
		return false
	}
	h.sessions++
	return true
}

func (h *Hub) dropSession() {
	if h == nil {
		return
	}
	h.sessMu.Lock()
	h.sessions--
	h.sessMu.Unlock()
}

// hubConn is a connection the Hub handed over: reads replay the first line
// and what was buffered after it, then go on to the raw connection.
type hubConn struct {
	net.Conn
	r     io.Reader
	lease *inboundLease
}

func (c *hubConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// CloseWrite half-closes the raw connection (see closeAfterTelling).
func (c *hubConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return errors.ErrUnsupported
}

// leaseOf returns the Hub's lease of a handed-over connection, or nil.
func leaseOf(c net.Conn) *inboundLease {
	if hc, ok := c.(*hubConn); ok {
		return hc.lease
	}
	return nil
}

// chanListener is a node's listener under a Hub: the Hub hands it
// connections; Addr is the real listener's.
type chanListener struct {
	addr net.Addr
	ch   chan net.Conn
	done chan struct{}
	once sync.Once
}

func newChanListener(addr net.Addr) *chanListener {
	return &chanListener{addr: addr, ch: make(chan net.Conn), done: make(chan struct{})}
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *chanListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *chanListener) Addr() net.Addr { return l.addr }
