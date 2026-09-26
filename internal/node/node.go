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
	"sync/atomic"
	"time"

	"github.com/UberMorgott/agent-link/internal/config"
)

// Sentinel errors callers can map to their own wording.
var (
	// ErrAuth: the peer does not hold the same pairing code or secret.
	ErrAuth = errors.New("authentication failed")
	// ErrSameName: the peer announced this node's own name (the address is
	// this machine, or both sides picked the same name).
	ErrSameName = errors.New("peer has this node's name")
	// ErrWrongPeer: the peer at the address announced a different name than configured.
	ErrWrongPeer = errors.New("unexpected peer name")
	// ErrUnknownPeer: a message is addressed to a node that never connected.
	ErrUnknownPeer = errors.New("unknown node")
	// ErrNoAreaPeer: no known peer subscribed to the area.
	ErrNoAreaPeer = errors.New("no peer subscribed to the area")
	// ErrEmptyBody: a request or reply without text.
	ErrEmptyBody = errors.New("empty body")
	// ErrAmbiguousPeer: a message without a recipient while several peers are known.
	ErrAmbiguousPeer = errors.New("several peers known, name one")
	// ErrNameTaken: another live member with a smaller node id uses this node's name.
	ErrNameTaken = errors.New("name taken by another member")
	// ErrRemoved: another member removed this node from the network.
	ErrRemoved = errors.New("removed from the network")
	// ErrSelf: the operation names this node itself.
	ErrSelf = errors.New("this node itself")
	// ErrLegacyRefused: the peer offers only the pre-v0.6 handshake, which
	// leaks an offline-checkable MAC, from a public address (or after a PAKE
	// session under its name).
	ErrLegacyRefused = errors.New("legacy handshake refused")
	// ErrWrongProject: the peer's hello names another project (or none) than
	// this node's context.
	ErrWrongProject = errors.New("peer is in another project")
	// ErrUnknownProject: the peer's app has no context for this node's project.
	ErrUnknownProject = errors.New("peer does not know the project")
)

// target is one peer address to dial; name is fixed by config or learned
// from the first handshake (guarded by Node.mu). revive is set for an address
// the user added by hand: its first session brings back a removed member.
type target struct {
	addr    string
	name    string
	revive  bool
	auto    bool // learned from gossip or discovery, not configured by the user
	started bool // its dial loop runs
}

// Node is one running agentlink peer.
type Node struct {
	cfg     config.Config
	secret  []byte
	store   *store
	chats   *chatStore
	atts    *attachStore
	log     *slog.Logger
	targets []*target
	// open accepts any authenticated peer name: some peer has no configured
	// name, or no peer is configured at all (the other side dials in).
	open bool

	mu      sync.Mutex
	known   map[string]bool // configured and learned peer names
	conns   map[string]*peerConn
	areas   map[string][]string  // last areas each peer announced
	problem error                // last handshake failure, nil once a session is up
	offline map[string]time.Time // when each peer's last session ended
	started time.Time            // stands in for offline of peers never connected
	members map[string]*Member   // the gossiped membership table, incl. this node and tombstones
	dialing map[string]bool      // members with a running dial loop
	tried   map[string]time.Time // discovered addresses by when they were last dialed
	meta    ProjectMeta          // a project node's shared meta (project.json)
	left    bool                 // Leave ran: no new sessions
	saveMu  sync.Mutex           // orders members.json and project.json writes

	ensureMu sync.Mutex // serializes EnsureOpenChat
	sess     *sessionRegistry
	seats    *seatStore
	// seatExtra is added to the environment of seat turns (SetSeatEnv).
	seatExtra []string
	folders   folderMap
	// autoAnswer: the worker answers requests no live session takes (presence).
	autoAnswer bool
	// waker wakes idle WakeQueue sessions (wake.go) every wakeEvery (0:
	// wakePoll); nil: none.
	waker     SessionWaker
	wakeEvery time.Duration
	// poster wakes idle Claude sessions through their inbox (inbox.go);
	// launcher opens a visible session when none is live (launch.go), while
	// autoOpen is on. deliv is the delivery ladder's state.
	poster   InboxPoster
	launcher SessionLauncher
	autoOpen atomic.Bool
	deliv    *deliveryState
	// leases: the delivery lease of every message handed to an owner (lease.go).
	leases *leaseBook
	// directWG counts the desktop-app first turns running (runDirect).
	directWG sync.WaitGroup
	// occupied replaces folderOccupied, launchAck the ack of a desktop
	// launch's messages (tests); nil: the real ones.
	occupied  func(dir string, now time.Time) bool
	launchAck atomic.Pointer[func(req AckRequest) error]
	// seatBusy replaces seatOccupied (tests); nil: the real one.
	seatBusy func(s Seat, now time.Time) bool

	selfAddrs []string // this node's own peer addresses, set by Run

	id         string // random node id, kept in the data directory
	appVersion string
	listenPort int
	netTag     string // discovery network id derived from the key; empty when off
	oldNetTag  string // the pre-v0.6 tag, only matched in received beacons

	guard     *authGuard        // failed inbound handshakes per source
	pending   *connLimit        // inbound connections still in the handshake
	pakeSeen  map[string]bool   // peer names that authenticated with the PAKE (guarded by mu)
	isPrivate func(net.IP) bool // addresses allowed the legacy handshake (privateAddr)

	wg sync.WaitGroup
	// runCtx is Run's context while it runs, nil otherwise (guarded by runMu):
	// spawn starts goroutines only then, so none is added once Run waits.
	runMu  sync.Mutex
	runCtx context.Context
	// hub, when set (Hub.Add), owns the listener, the discovery socket and
	// the handshake limits this node shares with the other contexts.
	hub *Hub

	resendAfter      time.Duration
	resendTick       time.Duration
	backoffMin       time.Duration
	backoffMax       time.Duration
	handshakeTimeout time.Duration
	heartbeatEvery   time.Duration
	heartbeatTimeout time.Duration
	noNewsAfter      time.Duration
	meshEvery        time.Duration // how often new members get a dial loop

	// Discovery (cfg.Discovery): the UDP beacon's port, multicast group,
	// interval and the interfaces it is sent on and received from.
	beaconPort   int
	beaconGroup  net.IP
	beaconEvery  time.Duration
	beaconIfaces func() []net.Interface

	onInbound    func(Message) error
	onChange     func(topic string)
	onLocalReply func(requestID string)
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
	id, err := st.nodeID()
	if err != nil {
		return nil, err
	}
	chats, err := openChatStore(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	members, err := st.loadMembers()
	if err != nil {
		return nil, err
	}
	atts, err := openAttachStore(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	n := &Node{
		cfg: cfg, secret: secret, store: st, chats: chats, atts: atts, log: log.With("node", cfg.Node),
		known: map[string]bool{}, conns: map[string]*peerConn{}, areas: map[string][]string{},
		offline: map[string]time.Time{}, started: time.Now(),
		members: map[string]*Member{}, dialing: map[string]bool{}, tried: map[string]time.Time{},
		guard: newAuthGuard(), pending: newConnLimit(pendingTotal, pendingPerSource), pakeSeen: map[string]bool{},
		isPrivate:   func(ip net.IP) bool { return privateAddr(ip, zeroTierNets()) },
		id:          id,
		open:        len(cfg.Peers) == 0,
		resendAfter: 5 * time.Second, resendTick: time.Second,
		backoffMin: 250 * time.Millisecond, backoffMax: 5 * time.Second,
		handshakeTimeout: 10 * time.Second,
		heartbeatEvery:   HeartbeatEvery, heartbeatTimeout: HeartbeatTimeout,
		noNewsAfter: NoNewsAfter, meshEvery: time.Second,
		beaconPort: cfg.DiscoveryPort, beaconGroup: net.ParseIP(BeaconGroup).To4(),
		beaconEvery: BeaconEvery, beaconIfaces: beaconInterfaces,
	}
	seen, err := st.loadPAKESeen()
	if err != nil {
		return nil, err
	}
	for _, name := range seen {
		n.pakeSeen[name] = true
	}
	if n.beaconPort == 0 {
		n.beaconPort = BeaconPort
	}
	switch {
	case !cfg.Discovery:
	case cfg.Project != "":
		n.netTag = config.ProjectTag(secret)
	default:
		n.netTag, n.oldNetTag = NetworkTag(secret), legacyNetworkTag(secret)
	}
	for _, p := range cfg.Peers {
		n.targets = append(n.targets, &target{addr: p.Addr, name: p.Name})
		if p.Name == "" {
			n.open = true
		} else {
			n.known[p.Name] = true
		}
	}
	// areas.json also remembers peers learned from earlier handshakes.
	saved, err := st.loadAreas()
	if err != nil {
		return nil, err
	}
	for peer, areas := range saved {
		if n.open && config.ValidName(peer) && peer != cfg.Node {
			n.known[peer] = true
		}
		if n.known[peer] {
			n.areas[peer] = areas
		}
	}
	// members.json: every member ever learned, removed ones as tombstones.
	for _, m := range members {
		if !config.ValidName(m.Name) {
			continue
		}
		n.members[m.Name] = &m
		switch {
		case m.Name == cfg.Node:
		case m.Removed:
			delete(n.known, m.Name)
		default:
			n.known[m.Name] = true
		}
	}
	if cfg.Project != "" {
		if err := n.loadProjectMeta(); err != nil {
			return nil, err
		}
	}
	if n.sess, err = openSessions(cfg.DataDir); err != nil {
		return nil, err
	}
	if n.seats, err = openSeats(cfg.DataDir); err != nil {
		return nil, err
	}
	n.deliv = newDeliveryState()
	if err := n.loadLaunchState(); err != nil {
		return nil, err
	}
	if err := n.repairChats(); err != nil {
		return nil, err
	}
	n.MigrateProjectChats()
	n.leases = newLeaseBook(cfg.DataDir)
	if err := n.leases.load(); err != nil {
		n.log.Warn("delivery leases unreadable; starting over", "err", err)
	}
	n.restoreLeases(time.Now())
	return n, nil
}

// SetAppVersion sets the program version announced to peers. It must be set
// before Serve or Run.
func (n *Node) SetAppVersion(v string) { n.appVersion = v }

// ID returns this node's random id, kept in its data directory.
func (n *Node) ID() string { return n.id }

// SetInboundHook registers fn to be called for every inbound message after it
// is persisted and before it is ACKed, including resent duplicates (a crash
// may have hit between persisting and fn), so fn must be idempotent by id.
// When fn fails the message is not ACKed and the sender resends it. It must be
// set before Serve or Run, and fn must not block for long.
func (n *Node) SetInboundHook(fn func(Message) error) { n.onInbound = fn }

// SetLocalReplyHook registers fn to be called after this node's user or an
// agent session sent a reply through the control API (SendRequest), with the
// id of the request it answers; a job's own reply to its request is not
// reported. It must be set before Serve or Run, and fn must not block for long.
func (n *Node) SetLocalReplyHook(fn func(requestID string)) { n.onLocalReply = fn }

// SetChangeHook registers a nonblocking observer for local state changes. The
// hook is invoked after persistence and outside Node locks.
func (n *Node) SetChangeHook(fn func(topic string)) { n.onChange = fn }

func (n *Node) changed(topic string) {
	if n.onChange != nil {
		n.onChange(topic)
	}
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
		Handler:           n.APIHandler(),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	apiErr := make(chan error, 1)
	go func() { apiErr <- srv.Serve(apiLn) }()
	n.log.Info("control API", "api", apiLn.Addr())

	peersDone := make(chan struct{})
	go func() {
		defer close(peersDone)
		n.Run(ctx, peerLn)
	}()

	var err error
	select {
	case <-ctx.Done():
	case err = <-apiErr:
	}
	cancel()
	_ = srv.Close()
	<-peersDone
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}

// Run serves peers only (listener and dialers) until ctx is cancelled, then
// closes peerLn. Hosts that serve the control API themselves mount APIHandler.
func (n *Node) Run(ctx context.Context, peerLn net.Listener) {
	addrs, port := listenAddrs(peerLn.Addr())
	n.mu.Lock()
	n.selfAddrs, n.listenPort = addrs, port
	n.mu.Unlock()
	n.refreshSelf()
	n.runMu.Lock()
	n.runCtx = ctx
	n.runMu.Unlock()
	n.wg.Go(func() { n.acceptLoop(ctx, peerLn) })
	n.wg.Go(func() { n.meshLoop(ctx) }) // also dials the configured peers
	n.wg.Go(func() { n.activityLoop(ctx) })
	n.wg.Go(func() { n.attachSweepLoop(ctx) })
	if n.waker != nil || n.poster != nil || n.launcher != nil {
		n.wg.Go(func() { n.wakeLoop(ctx) })
	}
	if n.netTag != "" && n.hub == nil { // under a Hub, its socket carries the beacons
		n.wg.Go(func() { n.discoveryLoop(ctx) })
	}
	n.log.Info("serving peers", "listen", peerLn.Addr(), "addrs", addrs, "discovery", n.netTag != "", "project", n.cfg.Project)
	<-ctx.Done()
	n.runMu.Lock()
	n.runCtx = nil
	n.runMu.Unlock()
	_ = peerLn.Close()
	n.wg.Wait()
	n.mu.Lock()
	for _, t := range n.targets {
		t.started = false // a later Run dials them again
	}
	n.mu.Unlock()
}

// running reports whether Run is serving peers.
func (n *Node) running() bool {
	n.runMu.Lock()
	defer n.runMu.Unlock()
	return n.runCtx != nil
}

// spawn runs fn on the node's wait group while Run runs, reporting whether it
// did; fn gets Run's context. Callers outside Run's own goroutines (the Hub's
// beacon reader) start work this way.
func (n *Node) spawn(fn func(ctx context.Context)) bool {
	n.runMu.Lock()
	defer n.runMu.Unlock()
	ctx := n.runCtx
	if ctx == nil {
		return false
	}
	n.wg.Go(func() { fn(ctx) })
	return true
}

// AddPeer dials addr (host or host:port, default port added) in addition to
// the configured peers, now and in a later Run. The first session through it
// brings back a member someone removed; the address then spreads to every member.
func (n *Node) AddPeer(addr string) error {
	a, err := config.WithDefaultPort(addr)
	if err != nil {
		return err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, t := range n.targets {
		if t.addr == a {
			t.revive = true
			return nil
		}
	}
	n.targets = append(n.targets, &target{addr: a, revive: true})
	return nil
}

// Link liveness and sender-side tracking defaults.
const (
	// HeartbeatEvery is how often a node sends a heartbeat frame on each session.
	HeartbeatEvery = 15 * time.Second
	// HeartbeatTimeout closes a session that has been silent this long, but only
	// once the peer has sent a heartbeat: older peers never do.
	HeartbeatTimeout = 45 * time.Second
	// NoNewsAfter marks an unanswered outbound request (Entry.NoNewsMin) whose
	// peer has sent nothing about it for this long while connected, or has been
	// disconnected this long.
	NoNewsAfter = 5 * time.Minute
)

// Recent lists inbound, queued and sent messages, newest first. Unanswered
// outbound requests the peer has been silent about carry NoNewsMin.
func (n *Node) Recent(limit int) ([]Entry, error) { return n.entries(limit, false) }

// Inbox is Recent with chat messages too (the control API's /inbox): every
// message is in a chat now, so the app's history views list them as well.
func (n *Node) Inbox(limit int) ([]Entry, error) { return n.entries(limit, true) }

func (n *Node) entries(limit int, chats bool) ([]Entry, error) {
	entries, err := n.store.recent(limit, chats)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	for i := range entries {
		e := &entries[i]
		if e.Direction != "out" || !e.IsRequest() || e.Answer != "" ||
			e.JobStatus == JobCompleted || e.JobStatus == JobFailed || e.LastHeard.IsZero() {
			continue
		}
		silent := now.Sub(e.LastHeard)
		offline := n.offlineFor(e.Peer, now)
		// A request queued behind other jobs is news enough while the peer is up.
		stale := offline >= n.noNewsAfter || (offline == 0 && e.JobStatus != JobQueued && silent >= n.noNewsAfter)
		if stale {
			e.NoNewsMin = max(1, int(silent/time.Minute))
		}
	}
	return entries, nil
}

// offlineFor is how long peer has had no session: 0 while connected, and
// since this node started for a peer that never connected.
func (n *Node) offlineFor(peer string, now time.Time) time.Duration {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.conns[peer] != nil {
		return 0
	}
	since, ok := n.offline[peer]
	if !ok {
		since = n.started
	}
	return max(now.Sub(since), time.Nanosecond)
}

// Connected reports whether a live session to peer exists.
func (n *Node) Connected(peer string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.conns[peer] != nil
}

// PeerCaps returns the protocol version and capabilities peer announced on its
// live session; ok is false without one. An older peer announces version 0
// and no capabilities.
func (n *Node) PeerCaps(peer string) (proto int, caps []string, ok bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	pc := n.conns[peer]
	if pc == nil {
		return 0, nil, false
	}
	return pc.proto, slices.Clone(pc.caps), true
}

// PeerHas reports whether peer's live session announced capability c. Gate
// every feature newer than v0.3 on it.
func (n *Node) PeerHas(peer, c string) bool {
	_, caps, ok := n.PeerCaps(peer)
	return ok && slices.Contains(caps, c)
}

// Peers returns the known peer names (configured or learned, not removed),
// connected ones first.
func (n *Node) Peers() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]string, 0, len(n.known))
	for p := range n.known {
		if !n.removedLocked(p) {
			out = append(out, p)
		}
	}
	slices.SortFunc(out, func(a, b string) int {
		ca, cb := n.conns[a] != nil, n.conns[b] != nil
		if ca != cb {
			if ca {
				return -1
			}
			return 1
		}
		return strings.Compare(a, b)
	})
	return out
}

// Problem returns the last handshake failure (ErrAuth, ErrSameName,
// ErrWrongPeer wrapped), or nil once a session is established.
func (n *Node) Problem() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.problem
}

func (n *Node) setProblem(err error) {
	n.mu.Lock()
	n.problem = err
	n.mu.Unlock()
	n.changed("peer")
}

// Send queues a message to a node name or to "area:NAME". Area messages fan
// out to every peer that announced the area. An empty to means the only
// known peer; with several it is ErrAmbiguousPeer, which lists them.
func (n *Node) Send(to, body, replyTo string) (Message, error) {
	return n.SendMessage(Message{To: to, Body: body, ReplyTo: replyTo})
}

// SendMessage queues m like Send. An empty ID gets a random one; From and
// CreatedAt are always set here. A status update needs a ReplyTo and may have
// no body. Re-sending the same ID is safe: the receiver deduplicates by id.
//
// A message with ChatID goes to every other participant of that chat instead
// of To (which must be empty); Responders, RootID, AutoDepth and ActivityInfo
// are kept, and an empty RootID starts a new chain at the message itself.
func (n *Node) SendMessage(m Message) (Message, error) {
	switch {
	case m.ChatID != "" && m.To != "":
		return Message{}, errors.New("a chat message has no to")
	case m.ChatID == "" && (len(m.Responders) > 0 || m.RootID != "" || m.ActivityInfo != nil):
		return Message{}, errors.New("chat fields without chat_id")
	case m.Kind != "" && m.Kind != KindStatus:
		return Message{}, fmt.Errorf("invalid kind %q", m.Kind)
	case m.Kind == KindStatus && m.ReplyTo == "":
		return Message{}, errors.New("status update without reply_to")
	case m.Body == "" && m.Kind != KindStatus:
		return Message{}, ErrEmptyBody
	case m.ReplyTo != "" && !validID(m.ReplyTo):
		return Message{}, fmt.Errorf("invalid reply_to %q", m.ReplyTo)
	case m.ID != "" && !validID(m.ID):
		return Message{}, fmt.Errorf("invalid id %q", m.ID)
	}
	if m.ID == "" {
		m.ID = newID()
	}
	if m.ChatID != "" {
		return n.sendChat(m)
	}
	m.Participants, m.AutoDepth = nil, 0
	m.From, m.Area, m.CreatedAt = n.cfg.Node, "", time.Now().UTC()
	if m.To == "" {
		switch peers := n.Peers(); {
		case len(peers) == 1:
			m.To = peers[0]
		case len(peers) > 1:
			slices.Sort(peers)
			return Message{}, fmt.Errorf("%w: %s", ErrAmbiguousPeer, strings.Join(peers, ", "))
		}
	}
	to := m.To
	var recipients []string
	if area, ok := strings.CutPrefix(to, AreaPrefix); ok {
		m.Area = area
		n.mu.Lock()
		for peer, areas := range n.areas {
			if slices.Contains(areas, area) && n.known[peer] && !n.removedLocked(peer) {
				recipients = append(recipients, peer)
			}
		}
		n.mu.Unlock()
		if len(recipients) == 0 {
			return Message{}, fmt.Errorf("%w %q", ErrNoAreaPeer, area)
		}
		slices.Sort(recipients)
	} else {
		n.mu.Lock()
		ok := n.known[to] && !n.removedLocked(to)
		n.mu.Unlock()
		if !ok {
			return Message{}, fmt.Errorf("%w %q", ErrUnknownPeer, to)
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
	n.changed("messages")
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
	// A connection handed over by the Hub carries the Hub's lease; a node on
	// its own listener takes one from its own guard and limit.
	lease := leaseOf(c)
	if lease == nil {
		var ip net.IP
		if tcp, ok := c.RemoteAddr().(*net.TCPAddr); ok {
			ip = tcp.IP
		}
		if !n.guard.allow(ip) {
			n.log.Debug("inbound connection closed: too many failed handshakes", "remote", c.RemoteAddr())
			_ = c.Close()
			return
		}
		if !n.pending.acquire(ip) {
			n.log.Debug("inbound connection closed: too many unauthenticated connections", "remote", c.RemoteAddr())
			_ = c.Close()
			return
		}
		lease = newInboundLease(n.guard, n.pending, ip)
	}
	_ = c.SetDeadline(time.Now().Add(n.handshakeTimeout))
	w := newWire(c)
	hello, err := n.acceptHandshake(w)
	lease.release()
	if err != nil {
		// A name clash is a state of the network, not a guess.
		if !errors.Is(err, ErrSameName) && !errors.Is(err, ErrNameTaken) {
			if d := lease.fail(); d > 0 {
				n.log.Warn("too many failed handshakes, source blocked", "remote", c.RemoteAddr(), "for", d)
			}
		}
		n.log.Warn("inbound handshake rejected", "remote", c.RemoteAddr(), "err", err)
		_ = c.Close()
		return
	}
	lease.ok()
	pc := newPeerConn(hello, hello.name, w)
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
	n.runConn(ctx, pc, "")
}

func (n *Node) dialLoop(ctx context.Context, t *target) {
	backoff := n.backoffMin
	for {
		n.mu.Lock()
		name, idle := t.name, t.name != "" && n.removedLocked(t.name) && !t.revive
		n.mu.Unlock()
		switch {
		case idle: // a removed member: only a hand-added address brings it back
			backoff = n.backoffMax
		case name != "" && n.Connected(name):
			backoff = n.backoffMin
		case n.dialOnce(ctx, t):
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
func (n *Node) dialOnce(ctx context.Context, t *target) bool {
	// Under a Hub, dial + handshake take one of its outbound slots.
	release, ok := n.hub.acquireDial(ctx)
	if !ok {
		return false
	}
	defer release()
	d := net.Dialer{Timeout: n.handshakeTimeout}
	c, err := d.DialContext(ctx, "tcp", t.addr)
	if err != nil {
		n.log.Debug("dial", "addr", t.addr, "err", err)
		return false
	}
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	_ = c.SetDeadline(time.Now().Add(n.handshakeTimeout))
	w := newWire(c)
	n.mu.Lock()
	want := t.name
	n.mu.Unlock()
	hello, err := n.dialHandshake(w, want)
	if err != nil {
		n.log.Warn("outbound handshake failed", "addr", t.addr, "err", err)
		// Learned and discovered addresses go stale; only the user's own ones are reported.
		if !t.auto && (errors.Is(err, ErrAuth) || errors.Is(err, ErrSameName) || errors.Is(err, ErrWrongPeer) || errors.Is(err, ErrLegacyRefused) ||
			errors.Is(err, ErrWrongProject) || errors.Is(err, ErrUnknownProject)) || errors.Is(err, ErrNameTaken) {
			n.setProblem(err)
		}
		_ = c.Close()
		return false
	}
	n.mu.Lock()
	t.name = hello.name
	revive := t.revive
	n.mu.Unlock()
	if revive {
		n.revive(hello.name, hello.id)
	}
	_ = c.SetDeadline(time.Time{})
	pc := newPeerConn(hello, n.cfg.Node, w)
	if !n.register(pc) {
		_ = c.Close()
		return false
	}
	n.mu.Lock()
	t.revive = false
	n.mu.Unlock()
	release() // the session runs; the dial slot is free again
	n.runConn(ctx, pc, t.addr)
	return true
}
