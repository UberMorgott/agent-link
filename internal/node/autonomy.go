package node

import (
	"errors"
	"io/fs"
	"path/filepath"
	"slices"
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
//     too, within the hop limit. Its budgets bound the node's autonomous
//     turns (a wake, a launch, a seat's turn; autoTake): at most TurnsPerHour
//     in any hour and at most MaxRun of continuous autonomous work (turns less
//     than autoQuietGap apart). An exhausted budget pauses autonomous delivery
//     (messages stay unread), the pause hook tells the owner once, and
//     ResumeAutonomy goes on. The budget state is kept in autonomy.json, so a
//     restart neither resumes a pause nor forgets it was told.
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

// Budget defaults of AutonomyFull.
const (
	DefaultTurnsPerHour = 30
	DefaultMaxRun       = 4 * time.Hour
	// autoQuietGap: autonomous turns further apart than this start a new run.
	autoQuietGap = 15 * time.Minute
	autonomyFile = "autonomy.json"
)

// Pause reasons (AutonomyStatus.Reason).
const (
	PauseTurns = "turns" // TurnsPerHour reached
	PauseRun   = "run"   // MaxRun of continuous work reached
)

// ErrStopped: the node's agents are stopped (SetStopped).
var ErrStopped = errors.New("agents are stopped")

// Autonomy is a project's autonomy setting.
type Autonomy struct {
	Mode string
	// MaxDepth is the hop limit; 0 means none.
	MaxDepth     int
	TurnsPerHour int
	MaxRun       time.Duration
}

// autonomyState is the budget state of AutonomyFull (autonomy.json).
type autonomyState struct {
	Paused   bool        `json:"paused,omitempty"`
	Reason   string      `json:"reason,omitempty"`
	PausedAt time.Time   `json:"paused_at,omitzero"`
	Turns    []time.Time `json:"turns,omitempty"` // autonomous turns of the last hour
	RunStart time.Time   `json:"run_start,omitzero"`
	LastTurn time.Time   `json:"last_turn,omitzero"`
}

// AutonomyStatus is a project's autonomy as the app shows it.
type AutonomyStatus struct {
	Autonomy
	Stopped  bool
	Paused   bool
	Reason   string
	PausedAt time.Time
	// TurnsLastHour counts the autonomous turns of the last hour, Run is how
	// long the current run of autonomous work lasts (0: none), in full mode.
	TurnsLastHour int
	Run           time.Duration
}

type autonomy struct {
	stopped atomic.Bool
	mu      sync.Mutex
	cfg     Autonomy
	st      autonomyState
	path    string
	onPause func(reason string)
}

func newAutonomy(dir string) *autonomy {
	a := &autonomy{cfg: Autonomy{Mode: AutonomyOff, MaxDepth: MaxAutoDepth, TurnsPerHour: DefaultTurnsPerHour, MaxRun: DefaultMaxRun}}
	if dir != "" {
		a.path = filepath.Join(dir, autonomyFile)
	}
	return a
}

// load reads autonomy.json (missing: none).
func (a *autonomy) load() error {
	if a.path == "" {
		return nil
	}
	var st autonomyState
	if err := readJSON(a.path, &st); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	a.mu.Lock()
	a.st = st
	a.mu.Unlock()
	return nil
}

// saveLocked writes the budget state; the caller holds a.mu.
func (a *autonomy) saveLocked() error {
	if a.path == "" {
		return nil
	}
	return writeJSON(a.path, a.st)
}

// SetAutonomy sets the project's autonomy; it may change while the node runs.
// An unknown mode is AutonomyOff, a negative MaxDepth MaxAutoDepth, and an
// unset budget its default.
func (n *Node) SetAutonomy(cfg Autonomy) {
	switch cfg.Mode {
	case AutonomyAsked, AutonomyFull:
	default:
		cfg.Mode = AutonomyOff
	}
	if cfg.MaxDepth < 0 {
		cfg.MaxDepth = MaxAutoDepth
	}
	if cfg.TurnsPerHour <= 0 {
		cfg.TurnsPerHour = DefaultTurnsPerHour
	}
	if cfg.MaxRun <= 0 {
		cfg.MaxRun = DefaultMaxRun
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

// AutonomyStatus reports the autonomy setting with its budget state.
func (n *Node) AutonomyStatus() AutonomyStatus {
	now := time.Now()
	a := n.auto
	a.mu.Lock()
	defer a.mu.Unlock()
	s := AutonomyStatus{Autonomy: a.cfg, Stopped: a.stopped.Load(), Paused: a.st.Paused, Reason: a.st.Reason, PausedAt: a.st.PausedAt}
	if a.cfg.Mode == AutonomyFull {
		s.TurnsLastHour = len(recentTurns(a.st.Turns, now))
		if !a.st.RunStart.IsZero() && now.Sub(a.st.LastTurn) < autoQuietGap {
			s.Run = now.Sub(a.st.RunStart)
		}
	}
	return s
}

// SetAutonomyPauseHook sets what is told of a pause of autonomous delivery
// (once per pause). It must be set before Serve or Run.
func (n *Node) SetAutonomyPauseHook(f func(reason string)) { n.auto.onPause = f }

// ResumeAutonomy ends a pause and starts the budgets over.
func (n *Node) ResumeAutonomy() {
	a := n.auto
	a.mu.Lock()
	a.st = autonomyState{}
	err := a.saveLocked()
	a.mu.Unlock()
	if err != nil {
		n.log.Warn("save autonomy", "err", err)
	}
	n.log.Info("autonomous delivery resumed")
	n.changed("autonomy")
}

// SetStopped turns the node's emergency stop on or off. On, it releases every
// active automatic lease (no failure counted, claims dropped), ends the
// running seat turns and desktop-launch turns, and lets no wake, launch or
// seat turn start; messages stay unread.
func (n *Node) SetStopped(on bool) {
	if n.auto.stopped.Swap(on) == on {
		return
	}
	if !on {
		n.leases.resume()
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

// autoOK reports whether autonomous delivery may run now: not stopped, not
// paused by a budget.
func (n *Node) autoOK() bool {
	if n.Stopped() {
		return false
	}
	a := n.auto
	a.mu.Lock()
	defer a.mu.Unlock()
	return !a.st.Paused
}

// autoMode reports the autonomy mode.
func (n *Node) autoMode() string {
	a := n.auto
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg.Mode
}

// autoTake reports whether one autonomous turn may start now and, in full
// mode, counts it against the budgets; an exhausted budget pauses autonomous
// delivery (and tells the pause hook once) instead.
func (n *Node) autoTake(now time.Time) bool {
	if n.Stopped() {
		return false
	}
	a := n.auto
	a.mu.Lock()
	if a.st.Paused {
		a.mu.Unlock()
		return false
	}
	if a.cfg.Mode != AutonomyFull {
		a.mu.Unlock()
		return true
	}
	a.st.Turns = recentTurns(a.st.Turns, now)
	if a.st.LastTurn.IsZero() || now.Sub(a.st.LastTurn) >= autoQuietGap {
		a.st.RunStart = now
	}
	reason := ""
	switch {
	case len(a.st.Turns) >= a.cfg.TurnsPerHour:
		reason = PauseTurns
	case now.Sub(a.st.RunStart) >= a.cfg.MaxRun:
		reason = PauseRun
	}
	if reason != "" {
		a.st.Paused, a.st.Reason, a.st.PausedAt = true, reason, now
	} else {
		a.st.Turns = append(a.st.Turns, now)
		a.st.LastTurn = now
	}
	err := a.saveLocked()
	hook := a.onPause
	a.mu.Unlock()
	if err != nil {
		n.log.Warn("save autonomy", "err", err)
	}
	if reason == "" {
		return true
	}
	n.log.Warn("autonomous delivery paused: budget reached", "reason", reason)
	n.changed("autonomy")
	if hook != nil {
		hook(reason)
	}
	return false
}

// recentTurns keeps the turns of the hour before now.
func recentTurns(turns []time.Time, now time.Time) []time.Time {
	return slices.DeleteFunc(slices.Clone(turns), func(t time.Time) bool { return now.Sub(t) >= time.Hour })
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

// held reports whether m asks for answers past the hop limit (Message.Held
// with this project's limit): it is kept, and no handler runs for it.
func (n *Node) held(m Message) bool { return len(m.Responders) > 0 && n.overDepth(m) }

// AutoHeld reports whether no automatic handler may run for m: the node is
// stopped or m is past the hop limit (the worker leaves it unread).
func (n *Node) AutoHeld(m Message) bool { return n.Stopped() || n.held(m) }
