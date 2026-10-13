package node

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// Autonomy: how far this node's agents work by themselves in a project (the
// owner's choice per project, settings.ProjectBinding) and the node's
// emergency stop (SetStopped).
//
//   - AutonomyOff: the node opens no session by itself (launch.go). Live
//     sessions still get their messages as before (hooks, wakes).
//   - AutonomyAsked: a message that asks this node opens a session when none
//     is live (the launch ladder).
//   - AutonomyFull: a message that only informs this node (no ask) opens one
//     too, within the hop limit. Nothing counts or bounds its autonomous
//     turns: the owner's stop (Stopped) is the only brake.
//   - MaxDepth is the hop limit of an automatic chain (MaxAutoDepth by
//     default, 0 = none): past it a request waits for a person (Paused).
//   - Stopped: no wake, launch or seat turn, and no automatic lease at all
//     (leaseBook.stop): the active ones are released without counting a
//     failure and running turns end. Messages stay unread; the hooks of a
//     session a person works in still deliver them.

// Autonomy modes.
const (
	AutonomyOff   = "off"
	AutonomyAsked = "asked"
	AutonomyFull  = "full"
)

// autonomyFile held the state of the removed turn and run budgets (a pause,
// its reason, the turns of the last hour). Nothing reads it: a pause left in
// it by an older version is dropped with the file (dropBudgetState).
const autonomyFile = "autonomy.json"

// ErrStopped: the node's agents are stopped (SetStopped).
var ErrStopped = errors.New("agents are stopped")

// Autonomy is a project's autonomy setting.
type Autonomy struct {
	Mode string
	// MaxDepth is the hop limit; 0 means none.
	MaxDepth int
}

// AutonomyStatus is a project's autonomy as the app shows it.
type AutonomyStatus struct {
	Autonomy
	Stopped bool
}

type autonomy struct {
	stopped atomic.Bool
	stopMu  sync.RWMutex // serializes a waiter's inbox claim with a stop transition
	mu      sync.Mutex
	cfg     Autonomy
	path    string
}

func newAutonomy(dir string) *autonomy {
	a := &autonomy{cfg: Autonomy{Mode: AutonomyOff, MaxDepth: MaxAutoDepth}}
	if dir != "" {
		a.path = filepath.Join(dir, autonomyFile)
	}
	return a
}

// dropBudgetState removes the autonomy.json an older version left, whatever
// it holds (a "turns" or "run" pause, counted turns, unreadable content): the
// budgets are gone, so no such pause holds delivery. The owner's stop is not
// kept there (settings: StopAll, ProjectBinding.StopAgents) and stays.
func (a *autonomy) dropBudgetState() error {
	if a.path == "" {
		return nil
	}
	if err := os.Remove(a.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// SetAutonomy sets the project's autonomy; it may change while the node runs.
// An unknown mode is AutonomyOff and a negative MaxDepth MaxAutoDepth.
func (n *Node) SetAutonomy(cfg Autonomy) {
	switch cfg.Mode {
	case AutonomyAsked, AutonomyFull:
	default:
		cfg.Mode = AutonomyOff
	}
	if cfg.MaxDepth < 0 {
		cfg.MaxDepth = MaxAutoDepth
	}
	a := n.auto
	a.mu.Lock()
	a.cfg = cfg
	a.mu.Unlock()
	n.changed("autonomy")
}

// Autonomy reports the project's autonomy setting.
func (n *Node) Autonomy() Autonomy {
	a := n.auto
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg
}

// AutonomyStatus reports the autonomy setting with the stop's state.
func (n *Node) AutonomyStatus() AutonomyStatus {
	a := n.auto
	a.mu.Lock()
	defer a.mu.Unlock()
	return AutonomyStatus{Autonomy: a.cfg, Stopped: a.stopped.Load()}
}

// SetStopped turns the node's emergency stop on or off. On, it releases every
// active automatic lease (no failure counted, claims dropped), ends the
// running seat turns and desktop-launch turns, and lets no wake, launch or
// seat turn start; messages stay unread.
func (n *Node) SetStopped(on bool) {
	n.auto.stopMu.Lock()
	was := n.auto.stopped.Swap(on)
	n.auto.stopMu.Unlock()
	if was == on {
		return
	}
	n.presenceChanged()
	if !on {
		n.leases.resume()
		n.store.signal()
		n.log.Info("agents may work again")
		n.changed("autonomy")
		return
	}
	ends, err := n.leases.stop(time.Now())
	if err != nil {
		n.log.Warn("save leases", "err", err)
	}
	byOwner := map[string][]string{}
	for _, e := range ends {
		byOwner[e.owner] = append(byOwner[e.owner], e.id)
	}
	for owner, list := range byOwner {
		n.dropClaims(owner, list)
	}
	st := n.seats
	st.mu.Lock()
	for _, cancel := range st.run {
		cancel()
	}
	st.mu.Unlock()
	d := n.deliv
	d.mu.Lock()
	for _, p := range d.pending {
		if p.cancel != nil {
			p.cancel()
		}
	}
	d.mu.Unlock()
	n.log.Warn("agents stopped: no wake, launch or seat turn until switched back on", "released", len(ends))
	n.changed("leases")
	n.changed("autonomy")
}

// Stopped reports whether the node's emergency stop is on.
func (n *Node) Stopped() bool { return n.auto.stopped.Load() }

// autoMode reports the autonomy mode.
func (n *Node) autoMode() string {
	a := n.auto
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg.Mode
}

// maxDepth is the hop limit (0: none).
func (n *Node) maxDepth() int {
	a := n.auto
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg.MaxDepth
}

// overDepth reports whether m is past the hop limit.
func (n *Node) overDepth(m Message) bool {
	limit := n.maxDepth()
	return limit > 0 && int(m.AutoDepth) > limit
}

// held reports whether m asks for answers (members or seats) past the hop
// limit (Message.Held with this project's limit): it is kept, and no handler
// runs for it.
func (n *Node) held(m Message) bool { return m.asksAnyone() && n.overDepth(m) }

// AutoHeld reports whether no automatic handler may run for m: the node is
// stopped or m is past the hop limit (the worker leaves it unread).
func (n *Node) AutoHeld(m Message) bool { return n.Stopped() || n.held(m) }
