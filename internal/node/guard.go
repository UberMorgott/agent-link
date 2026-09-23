package node

import (
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// authGuard limits online guessing of the code: after guardFree failed inbound
// handshakes from one source, further connections from it are closed unread
// for a time that doubles with each failure, up to guardMax. A source is an
// IPv4 address or an IPv6 /64. A success clears it; a source quiet for
// guardForget starts over. At most guardCap sources are tracked: when full,
// forgotten ones are dropped, then the one quiet longest.
type authGuard struct {
	mu  sync.Mutex
	m   map[string]*guardEntry
	now func() time.Time
}

type guardEntry struct {
	fails int
	last  time.Time // last failure
	until time.Time // closed unread until then
}

const (
	guardFree   = 5
	guardBase   = time.Second
	guardMax    = 5 * time.Minute
	guardForget = 30 * time.Minute
	guardCap    = 4096
)

func newAuthGuard() *authGuard {
	return &authGuard{m: map[string]*guardEntry{}, now: time.Now}
}

func guardKey(ip net.IP) string {
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	if len(ip) == net.IPv6len {
		return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
	}
	return ""
}

// scopedKey is the guard entry of ip within scope: a Hub keeps the failures
// of each context (and those before any hello) apart, so succeeding in one
// project never clears guesses against another, and failing in one never
// blocks the others. A node on its own uses the scope "".
func scopedKey(ip net.IP, scope string) string {
	if scope == "" {
		return guardKey(ip)
	}
	return guardKey(ip) + "|" + scope
}

// allow reports whether a connection from ip may try a handshake now.
func (g *authGuard) allow(ip net.IP) bool { return g.allowIn(ip, "") }

func (g *authGuard) allowIn(ip net.IP, scope string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	e := g.m[scopedKey(ip, scope)]
	return e == nil || !g.now().Before(e.until)
}

// fail records a failed handshake from ip and returns how long it is now blocked.
func (g *authGuard) fail(ip net.IP) time.Duration { return g.failIn(ip, "") }

func (g *authGuard) failIn(ip net.IP, scope string) time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	now, k := g.now(), scopedKey(ip, scope)
	e := g.m[k]
	if e != nil && now.Sub(e.last) > guardForget {
		e = nil
	}
	if e == nil {
		g.makeRoomLocked(now)
		e = &guardEntry{}
		g.m[k] = e
	}
	e.fails++
	e.last = now
	if e.fails <= guardFree {
		return 0
	}
	d := guardMax
	if shift := e.fails - guardFree - 1; shift < 20 {
		d = min(guardBase<<shift, guardMax)
	}
	e.until = now.Add(d)
	return d
}

// success forgets ip's failures.
func (g *authGuard) success(ip net.IP) { g.successIn(ip, "") }

func (g *authGuard) successIn(ip net.IP, scope string) {
	g.mu.Lock()
	delete(g.m, scopedKey(ip, scope))
	g.mu.Unlock()
}

func (g *authGuard) makeRoomLocked(now time.Time) {
	if len(g.m) < guardCap {
		return
	}
	for k, e := range g.m {
		if now.Sub(e.last) > guardForget && !now.Before(e.until) {
			delete(g.m, k)
		}
	}
	for len(g.m) >= guardCap {
		var oldest string
		var at time.Time
		for k, e := range g.m {
			if oldest == "" || e.last.Before(at) {
				oldest, at = k, e.last
			}
		}
		delete(g.m, oldest)
	}
}

// connLimit caps the inbound connections that have not authenticated yet: at
// most total in all and perSource from one source (guardKey), so a flood of
// idle or slow connections cannot pile up goroutines and buffers. Each one is
// also bounded by the handshake deadline.
type connLimit struct {
	mu               sync.Mutex
	total, perSource int
	n                int
	by               map[string]int
}

const (
	pendingTotal     = 64
	pendingPerSource = 8
)

func newConnLimit(total, perSource int) *connLimit {
	return &connLimit{total: total, perSource: perSource, by: map[string]int{}}
}

// acquire reports whether a connection from ip may start a handshake; if so,
// release must follow once the handshake ends.
func (l *connLimit) acquire(ip net.IP) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	k := guardKey(ip)
	if l.n >= l.total || l.by[k] >= l.perSource {
		return false
	}
	l.n++
	l.by[k]++
	return true
}

// inboundLease is one inbound connection's hold on a connLimit slot and its
// pending verdict for the authGuard. Whoever owns the connection (the Hub
// while it reads the first line, then the node) ends it: release frees the
// slot, fail and ok also record the handshake's outcome. Each acts once.
type inboundLease struct {
	guard   *authGuard
	pending *connLimit
	ip      net.IP
	// scope is the guard scope the verdict counts in (scopedKey); set by
	// its owner before the handshake.
	scope    string
	released atomic.Bool
	judged   atomic.Bool
}

func newInboundLease(g *authGuard, l *connLimit, ip net.IP) *inboundLease {
	return &inboundLease{guard: g, pending: l, ip: ip}
}

func (l *inboundLease) release() {
	if l.released.CompareAndSwap(false, true) {
		l.pending.release(l.ip)
	}
}

// fail releases and records a failed handshake; it returns how long the
// source is now blocked.
func (l *inboundLease) fail() time.Duration {
	l.release()
	if l.judged.CompareAndSwap(false, true) {
		return l.guard.failIn(l.ip, l.scope)
	}
	return 0
}

// ok releases and forgets the source's failures.
func (l *inboundLease) ok() {
	l.release()
	if l.judged.CompareAndSwap(false, true) {
		l.guard.successIn(l.ip, l.scope)
	}
}

func (l *connLimit) release(ip net.IP) {
	l.mu.Lock()
	defer l.mu.Unlock()
	k := guardKey(ip)
	l.n--
	if l.by[k]--; l.by[k] <= 0 {
		delete(l.by, k)
	}
}
