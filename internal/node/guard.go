package node

import (
	"net"
	"sync"
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

// allow reports whether a connection from ip may try a handshake now.
func (g *authGuard) allow(ip net.IP) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	e := g.m[guardKey(ip)]
	return e == nil || !g.now().Before(e.until)
}

// fail records a failed handshake from ip and returns how long it is now blocked.
func (g *authGuard) fail(ip net.IP) time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	now, k := g.now(), guardKey(ip)
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
func (g *authGuard) success(ip net.IP) {
	g.mu.Lock()
	delete(g.m, guardKey(ip))
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
