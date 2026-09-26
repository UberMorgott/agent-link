package node

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// The launch ladder. A message that asks this node must be seen by an agent
// session. When an area of this node has eligible unread messages and no live
// session at all, the node opens a visible session in the area's folder
// (SessionLauncher), as the project's autonomy allows (SetAutonomy: not off;
// a message that only informs this node only in full mode). Every launch is
// an autonomous turn (autoTake): none while the node is stopped or its
// budgets paused it.
//
// In the agent's desktop app (DirectLauncher, launch mode LaunchDesktop, the
// app installed): the node claims the messages for the launch first
// (launchClaim: no session's hooks take them meanwhile), runs the session's
// first turn headless with the messages themselves as its prompt (startDirect)
// and shows the session in the app as soon as its id is known. Only a turn
// that succeeded proves the session took them: then they are acknowledged as
// that session's (launch_confirmed; an ack that keeps failing keeps them
// claimed by that session and is retried until it succeeds, launch_state.go).
// A turn that
// started and then failed, was interrupted or timed out drops the claim (the
// hooks deliver them again), is launch_failed:<reason> and needs_human: it may
// have acted in part, so nothing opens again for them. Only a launch that
// failed before its turn started falls back to Terminal, once.
//
// No session opens in a folder occupied by one that is not registered
// (folderOccupied, occupancy.go): needs_human instead.
//
// A launch always starts a new session (of the provider of the area's last
// one): nothing proves an older session is closed, and resuming one that is
// still open would give it a second writer. The session runs with the owner's
// full agent permissions (LaunchCommand, ClaudeArgs, CodexTurn): auto-open is
// off unless the owner switches it on per project.
//
// Else in Windows Terminal:
//
//  1. It starts a new session with a
//     prompt that tells it to read the messages; the session's hooks deliver
//     them as always, in that same first turn. Unlike a wake, the prompt does
//     not carry the messages: it goes through wt's command line (launchSafe
//     strips newlines and quotes), and nothing can claim them before the
//     session exists and registers (a new session's id is unknown until
//     then; a launch that never starts would hold them hidden).
//  2. The launch is confirmed when a session of the area registers
//     (SessionStart) within launchConfirm; else it is tried once more as a new
//     session, then given up (launch_failed:timeout).
//  3. One launch per area per launchDebounce; a message a launch was confirmed
//     or failed for is not launched for again (a newer one is).
//
// Eligible: unread, asking this node (in full mode also a message of another
// member that only informs it, within the hop limit), not taken by the worker, not paused by
// the loop guard (those report AttemptNeedsHuman instead), and older than
// launchGrace (the worker and the hooks go first). Eligibility is re-read
// right before every attempt. Every step is reported to the messages' authors
// as attempt events (sendAttempts).

// Launch ladder timings.
const (
	launchGrace    = 10 * time.Second
	launchConfirm  = 90 * time.Second
	launchDebounce = 3 * time.Minute
	launchTries    = 2
)

// Launch failure reasons (launch_failed:<reason>).
var (
	// ErrNoTerminal: no Windows Terminal (wt.exe) to open a session in.
	ErrNoTerminal = errors.New("no_terminal")
	// ErrNoAgent: the agent's program (claude, codex) is not found.
	ErrNoAgent = errors.New("no_agent")
	// ErrTurnTimeout: a desktop launch's first turn ran out of time.
	ErrTurnTimeout = errors.New("timeout")
	// ErrTurnInterrupted: a desktop launch's first turn was interrupted.
	ErrTurnInterrupted = errors.New("interrupted")
	// ErrOpenApp: the first turn succeeded but the desktop app did not open
	// (the messages were taken all the same).
	ErrOpenApp = errors.New("open_app")
)

// LaunchSpec is one session to open: Provider (ProviderClaude or
// ProviderCodex) in Folder, resuming ResumeID when set, starting with Prompt.
type LaunchSpec struct {
	Provider string
	Folder   string
	ResumeID string
	Prompt   string
	// Seat is the seat whose turn this is (seats.go): the turn ends with ctx
	// (StopSeat) instead of outliving it. NoOpen keeps the desktop app closed.
	// Env is added to the agent's environment.
	Seat   string
	NoOpen bool
	Env    []string
}

// SessionLauncher opens a visible agent session.
type SessionLauncher interface {
	Launch(ctx context.Context, spec LaunchSpec) error
}

// DirectLauncher is a SessionLauncher that can also open a session in the
// agent's desktop app (DesktopLauncher): it runs the session's first turn
// itself, with the messages as its prompt.
type DirectLauncher interface {
	SessionLauncher
	// Direct reports whether provider's sessions can open this way.
	Direct(provider string) bool
	// Run runs spec's first turn and shows the session; started gets the
	// session id as soon as it is known. It returns after the turn: nil only
	// for a turn that succeeded (or ErrOpenApp: it succeeded, the app did not
	// open).
	Run(ctx context.Context, spec LaunchSpec, started func(session string)) error
}

// Launch modes of a project (SetLaunchMode).
const (
	// LaunchDesktop opens sessions in the agent's desktop app when it is
	// installed, in Terminal otherwise (the default).
	LaunchDesktop = "desktop"
	// LaunchTerminal opens sessions in Terminal.
	LaunchTerminal = "terminal"
)

// SetLaunchMode sets how sessions open (LaunchDesktop when not
// LaunchTerminal); it may change while the node runs.
func (n *Node) SetLaunchMode(mode string) {
	if mode != LaunchTerminal {
		mode = LaunchDesktop
	}
	n.deliv.mu.Lock()
	n.deliv.mode = mode
	n.deliv.mu.Unlock()
}

// LaunchMode reports how sessions open (LaunchDesktop or LaunchTerminal).
func (n *Node) LaunchMode() string {
	n.deliv.mu.Lock()
	defer n.deliv.mu.Unlock()
	return n.deliv.mode
}

// SetLauncher sets how the node opens a session when none is live, and the
// provider it opens when the area has no last session (ProviderClaude when
// empty). It must be set before Serve or Run; nil opens none.
func (n *Node) SetLauncher(l SessionLauncher, provider string) {
	n.launcher = l
	if provider != ProviderCodex {
		provider = ProviderClaude
	}
	n.deliv.mu.Lock()
	n.deliv.provider = provider
	n.deliv.mu.Unlock()
}

// Full permissions of an opened session: the owner chose that auto-opened
// sessions act with all rights, as the owner's own would (docs/agent-usage.md).
const (
	claudeFullAccess = "bypassPermissions" // --permission-mode
	codexFullAccess  = "--dangerously-bypass-approvals-and-sandbox"
)

// LaunchCommand is the command line (program first) that opens spec in a new
// Windows Terminal window: `wt -w new -d <folder> <agent ...>`, with full
// permissions.
func LaunchCommand(spec LaunchSpec) []string {
	args := []string{"wt.exe", "-w", "new", "-d", spec.Folder}
	prompt := launchSafe(spec.Prompt)
	switch {
	case spec.Provider == ProviderCodex && spec.ResumeID != "":
		args = append(args, "codex", "resume", codexFullAccess, spec.ResumeID)
	case spec.Provider == ProviderCodex:
		args = append(args, "codex", codexFullAccess, "-C", spec.Folder)
	case spec.ResumeID != "":
		args = append(args, "claude", "--permission-mode", claudeFullAccess, "--resume", spec.ResumeID)
	default:
		args = append(args, "claude", "--permission-mode", claudeFullAccess)
	}
	if prompt != "" {
		args = append(args, prompt)
	}
	return args
}

// LaunchEnv is env (os.Environ) without what ties a process to an agent
// session it runs in (CLAUDECODE, CLAUDE_CODE_*, CLAUDE_PID, CODEX_* but
// CODEX_HOME, AGENTLINK_JOB_ID): an opened session is a new top-level one
// (a child session would, for one, keep no transcript to resume).
func LaunchEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		u := strings.ToUpper(name)
		switch {
		case u == "CLAUDECODE", u == "CLAUDE_PID", u == "CLAUDE_AGENT_SDK_VERSION", u == "AGENTLINK_JOB_ID",
			strings.HasPrefix(u, "CLAUDE_CODE_"), strings.HasPrefix(u, "CODEX_") && u != "CODEX_HOME":
			continue
		}
		out = append(out, kv)
	}
	return out
}

// launchSafe makes text safe as one argument through wt's command line: wt
// splits commands at ';', and quotes would end the argument.
func launchSafe(s string) string {
	s = strings.Map(func(r rune) rune {
		switch r {
		case ';', '"', '\r', '\n', '\t', '`', '^', '%', '&', '|', '<', '>':
			return ' '
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// deliveryState is the ladder's memory; spent and acks are durable
// (launch_state.go), the rest starts over after a restart (an event may then
// be reported again).
type deliveryState struct {
	mu       sync.Mutex
	provider string
	mode     string // LaunchDesktop or LaunchTerminal
	// sent: attempt events already reported, by message id and event.
	sent map[string]bool
	// woken: messages a wake was reported for, awaiting their ack.
	woken map[string]bool
	// spent: messages a launch was already confirmed or given up for; not
	// launched for again (a session that took them and ended, or a window the
	// person closed, must not reopen every launchDebounce).
	spent   map[string]spentMark
	acks    []*ackJob                 // pending acks of desktop launches
	pending map[string]*pendingLaunch // by area
	last    map[string]time.Time      // last launch per area
	// statePath: launch_state.json; pruned: last pruneLaunchState.
	statePath string
	pruned    time.Time
	// leasePruned: last prune of the lease book (leaseSweep).
	leasePruned time.Time
}

type pendingLaunch struct {
	at    time.Time
	tries int
	spec  LaunchSpec
	ids   []string
	// direct: a desktop-app launch running its first turn (runDirect ends it).
	direct bool
	// before: the live sessions of the area at the launch (nil: none); only
	// another one confirms it then.
	before map[string]bool
	// cancel ends a direct launch's first turn (SetStopped).
	cancel context.CancelFunc
}

func newDeliveryState() *deliveryState {
	return &deliveryState{provider: ProviderClaude, mode: LaunchDesktop, sent: map[string]bool{}, woken: map[string]bool{}, spent: map[string]spentMark{},
		pending: map[string]*pendingLaunch{}, last: map[string]time.Time{}}
}

// report sends event for the ids not reported with it yet.
func (n *Node) report(ids []string, event string) {
	d := n.deliv
	var fresh []string
	d.mu.Lock()
	for _, id := range ids {
		if k := id + "|" + event; !d.sent[k] {
			d.sent[k] = true
			fresh = append(fresh, id)
		}
	}
	if len(d.sent) > 20000 { // bounded: an old event may then be reported again
		clear(d.sent)
	}
	d.mu.Unlock()
	n.sendAttempts(fresh, event)
}

// noteWoken reports a wake of msgs to their authors; their ack confirms it.
func (n *Node) noteWoken(msgs []UnreadMessage) {
	var ids []string
	d := n.deliv
	d.mu.Lock()
	for _, m := range msgs {
		if !m.OwnHuman && m.ChatID != "" {
			ids = append(ids, m.ID)
			d.woken[m.ID] = true
		}
	}
	d.mu.Unlock()
	n.report(ids, AttemptWakeRequested)
}

// confirmRead reports woken_confirmed for the woken messages among ids, which
// a session just acknowledged.
func (n *Node) confirmRead(ids []string) {
	var woken []string
	d := n.deliv
	d.mu.Lock()
	for _, id := range ids {
		if d.woken[id] {
			delete(d.woken, id)
			woken = append(woken, id)
		}
	}
	d.mu.Unlock()
	n.report(woken, AttemptWokenConfirmed)
}

// launchAreas are the areas a session can be opened for, with their folders.
func (n *Node) launchAreas() map[string]string {
	out := map[string]string{}
	if n.folders.work != "" {
		out[""] = n.folders.work
	}
	for a, dir := range n.folders.projects {
		if dir != "" {
			out[a] = dir
		}
	}
	return out
}

// areaLive reports whether a live session is registered in area.
func (n *Node) areaLive(area string, now time.Time) bool {
	r := n.sess
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.sessions {
		if s.Area == area && s.live(now) {
			return true
		}
	}
	return false
}

// launchable splits area's unread messages (folder dir) into the ones a
// launch is for and the paused ones that need a person.
func (n *Node) launchable(dir string, now time.Time) (eligible, paused []UnreadMessage) {
	page, err := n.unreadFor(dir, "", "", 1000, false)
	if err != nil {
		return nil, nil
	}
	held := n.launchHeld()
	book := n.leases.all()
	full := n.autoMode() == AutonomyFull
	d := n.deliv
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, m := range page.Messages {
		l, leased := book[m.ID]
		leased = leased && (l.State == LeaseLeased || l.State == LeaseRunning || l.Failed)
		// Full autonomy: a chat message of another member that only informs
		// this node counts too, within the hop limit.
		fyi := full && !m.AsksYou && !m.Paused && m.ChatID != "" && m.Kind == "" && m.Direction == "in" && !n.overDepth(m.Message)
		switch {
		case (!m.AsksYou && !fyi) || m.OwnHuman || m.Assigned != "" || held[m.ID] || leased:
		case m.Paused:
			paused = append(paused, m)
		case d.isSpent(m.ID) || now.Sub(m.ReceivedAt) < launchGrace:
		default:
			eligible = append(eligible, m)
		}
	}
	return eligible, paused
}

// launchDue runs one step of the launch ladder for every area.
func (n *Node) launchDue(ctx context.Context, now time.Time) {
	if n.launcher == nil || n.autoMode() == AutonomyOff || !n.autoOK() {
		return
	}
	for area, dir := range n.launchAreas() {
		n.launchArea(ctx, area, dir, now)
	}
}

func (n *Node) launchArea(ctx context.Context, area, dir string, now time.Time) {
	d := n.deliv
	eligible, paused := n.launchable(dir, now)
	n.report(ids(paused), AttemptNeedsHuman)
	d.mu.Lock()
	p := d.pending[area]
	d.mu.Unlock()
	if p != nil && p.direct {
		return // its first turn is running
	}
	live := n.areaLive(area, now)
	if live {
		// Sessions live: only the orphans (every one of them passed over for
		// the message) open a new one.
		eligible = n.orphans(area, eligible, now)
	}
	// A launch is confirmed by a live session of the area that was not live
	// at the launch.
	confirmed := p != nil && live && n.areaHasNew(area, p.before, now)
	if confirmed {
		d.mu.Lock()
		delete(d.pending, area)
		d.mu.Unlock()
		n.spend(p.ids, now, AttemptLaunchConfirmed)
		n.log.Info("opened session confirmed", "area", area, "provider", p.spec.Provider, "tries", p.tries)
		n.report(p.ids, AttemptLaunchConfirmed)
		return
	}
	if p == nil && live && len(eligible) == 0 {
		return
	}
	if p != nil {
		if now.Sub(p.at) < launchConfirm {
			return
		}
		if p.tries >= launchTries || len(eligible) == 0 {
			d.mu.Lock()
			delete(d.pending, area)
			d.mu.Unlock()
			if len(eligible) == 0 {
				n.spend(p.ids, now)
			} else {
				n.spend(p.ids, now, AttemptLaunchFailed+":timeout")
				n.log.Warn("opened session never started", "area", area, "folder", p.spec.Folder, "tries", p.tries)
				n.report(p.ids, AttemptLaunchFailed+":timeout")
			}
			return
		}
		if n.occupiedFor(area, dir, eligible, now) {
			d.mu.Lock()
			delete(d.pending, area)
			d.mu.Unlock()
			return
		}
		spec := p.spec
		spec.ResumeID = "" // the resumed session did not start: a new one
		spec.Prompt = launchPrompt(n, eligible)
		n.startLaunch(ctx, area, spec, eligible, p.tries+1, now)
		return
	}
	if len(eligible) == 0 {
		return
	}
	d.mu.Lock()
	recent, debounced := d.last[area], now.Sub(d.last[area]) < launchDebounce
	provider, mode := d.provider, d.mode
	d.mu.Unlock()
	if debounced {
		n.log.Debug("launch debounced", "area", area, "last", recent)
		return
	}
	if n.occupiedFor(area, dir, eligible, now) {
		return
	}
	// Always a new session: nothing proves the area's last one is closed (a
	// second writer would fork its transcript). Its provider is kept.
	spec := LaunchSpec{Provider: provider, Folder: dir, Prompt: launchPrompt(n, eligible)}
	r := n.sess
	r.mu.Lock()
	ls, ok := r.recent[area]
	r.mu.Unlock()
	if ok && (ls.Provider == ProviderClaude || ls.Provider == ProviderCodex) && inFolder(dir, ls.Folder) {
		spec.Provider = ls.Provider
	}
	if dl, ok := n.launcher.(DirectLauncher); ok && mode != LaunchTerminal && dl.Direct(spec.Provider) {
		n.startDirect(ctx, dl, area, spec, eligible, now)
		return
	}
	n.startLaunch(ctx, area, spec, eligible, 1, now)
}

// startDirect opens spec in the desktop app (dl) for eligible: it claims them
// for the launch (launchClaim), and its first turn's prompt is the claimed
// messages themselves (WakePrompt, as many as fit). A headless turn runs no
// agent-link hooks, so nothing else delivers them to it.
func (n *Node) startDirect(ctx context.Context, dl DirectLauncher, area string, spec LaunchSpec, eligible []UnreadMessage, now time.Time) {
	if !n.autoTake(now) {
		return
	}
	msgs := n.launchClaim(area, eligible[:FitUnread(eligible)])
	if len(msgs) == 0 {
		return
	}
	spec.Prompt = WakePrompt(msgs, len(eligible)-len(msgs), spec.Folder, randomHex(8))
	ctx, cancel := context.WithCancel(ctx)
	p := &pendingLaunch{at: now, tries: 1, spec: spec, ids: ids(msgs), direct: true, cancel: cancel}
	d := n.deliv
	d.mu.Lock()
	d.last[area] = now
	d.pending[area] = p
	d.mu.Unlock()
	n.report(p.ids, AttemptLaunchRequested)
	n.log.Info("opening a session in the desktop app for unread messages", "area", area, "provider", spec.Provider,
		"folder", spec.Folder, "resume", spec.ResumeID, "messages", len(p.ids))
	n.directWG.Go(func() {
		defer cancel()
		n.runDirect(ctx, dl, area, p)
	})
}

// runDirect runs p's first turn and ends p: a turn that succeeded acknowledges
// the messages as the session's (launch_confirmed); any other end drops the
// launch claim, is launch_failed:<reason> and opens Terminal once instead.
func (n *Node) runDirect(ctx context.Context, dl DirectLauncher, area string, p *pendingLaunch) {
	var mu sync.Mutex
	session := ""
	err := dl.Run(ctx, p.spec, func(id string) {
		mu.Lock()
		first := session == ""
		if first {
			session = id
		}
		mu.Unlock()
		if first {
			n.launchSeen(area, p, id)
			// Proof the first turn runs with them: the lease is the session's now.
			if _, err := n.leases.start(launchOwner(area), id, "", p.ids, time.Now()); err != nil {
				n.log.Warn("save leases", "err", err)
			}
			n.changed("leases")
		}
	})
	mu.Lock()
	id := session
	mu.Unlock()
	d := n.deliv
	started := id != ""
	d.mu.Lock()
	if d.pending[area] == p {
		delete(d.pending, area)
	}
	d.last[area] = time.Now() // the ladder waits: the fallback below goes first
	d.mu.Unlock()
	if started && (err == nil || errors.Is(err, ErrOpenApp)) {
		if err != nil {
			n.log.Warn("the opened session's desktop app did not open", "area", area, "session", id, "err", err)
		}
		if aerr := n.ackLaunched(ctx, p.ids, id); aerr != nil {
			// The session took them: they stay claimed by it (no other session
			// or launch gets them again) and the ack is retried until it
			// succeeds (retryLaunchAcks), durably.
			n.holdForAck(area, p.ids, id, time.Now())
			n.log.Warn("acknowledging the opened session's messages failed; retrying later", "area", area, "session", id, "err", aerr)
			return
		}
		n.spend(p.ids, time.Now(), AttemptLaunchConfirmed)
		n.log.Info("opened session's first turn done", "area", area, "provider", p.spec.Provider, "session", id)
		n.report(p.ids, AttemptLaunchConfirmed)
		return
	}
	n.unclaim(launchOwner(area), p.ids) // the hooks, a person, or the fallback deliver them
	reason := "start_error"
	switch {
	case errors.Is(err, ErrNoAgent):
		reason = ErrNoAgent.Error()
	case errors.Is(err, ErrTurnTimeout):
		reason = ErrTurnTimeout.Error()
	case errors.Is(err, ErrTurnInterrupted):
		reason = ErrTurnInterrupted.Error()
	case started:
		reason = "turn_error"
	}
	if started {
		// The turn ran: it may have acted in part. Neither Terminal nor a new
		// launch repeats it; the messages stay unread for the hooks and a
		// person, and are not launched for again.
		if ferr := n.leases.fail(id, p.ids, "launch_"+reason, time.Now()); ferr != nil {
			n.log.Warn("save leases", "err", ferr)
		}
		n.spend(p.ids, time.Now(), AttemptLaunchFailed+":"+reason, AttemptNeedsHuman)
		n.log.Warn("the opened session's first turn failed; not retried", "area", area, "provider", p.spec.Provider,
			"session", id, "reason", reason, "err", err)
		n.report(p.ids, AttemptLaunchFailed+":"+reason)
		n.report(p.ids, AttemptNeedsHuman)
		return
	}
	n.log.Warn("opening a session in the desktop app failed; opening Terminal", "area", area, "provider", p.spec.Provider,
		"reason", reason, "err", err)
	n.report(p.ids, AttemptLaunchFailed+":"+reason)
	n.revokeLeases(launchOwner(area), p.ids, "launch_"+reason, false)
	n.fallbackTerminal(ctx, area, p)
}

// launchAckTries bounds the acks of a desktop launch's messages
// (ackLaunched), launchAckBackoff apart (growing).
const (
	launchAckTries   = 3
	launchAckBackoff = 300 * time.Millisecond
)

// ackLaunched acknowledges ids as session's, retrying a failure a few times.
func (n *Node) ackLaunched(ctx context.Context, ids []string, session string) error {
	req := AckRequest{IDs: ids, SessionID: session}
	ack := n.launchAckFunc()
	var err error
	for i := range launchAckTries {
		if err = ack(req); err == nil {
			return nil
		}
		if i == launchAckTries-1 {
			break
		}
		select {
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		case <-time.After(launchAckBackoff * time.Duration(i+1)):
		}
	}
	return err
}

// occupiedFor reports whether dir is occupied by a session that is not
// registered (folderOccupied); then no session opens for eligible, which need
// that session or a person (needs_human).
func (n *Node) occupiedFor(area, dir string, eligible []UnreadMessage, now time.Time) bool {
	if !n.folderOccupied(dir, now) {
		return false
	}
	n.log.Debug("folder occupied by a session not registered; not opening one", "area", area, "folder", dir)
	n.report(ids(eligible), AttemptNeedsHuman)
	return true
}

// fallbackTerminal opens Terminal once for the messages of the desktop launch
// p that failed before its turn started and are still eligible, unless a
// session is live or the folder is occupied meanwhile; the Terminal ladder
// takes it from there.
func (n *Node) fallbackTerminal(ctx context.Context, area string, p *pendingLaunch) {
	now := time.Now()
	eligible, _ := n.launchable(p.spec.Folder, now.Add(launchGrace))
	if n.areaLive(area, now) {
		eligible = n.orphans(area, eligible, now)
	}
	var msgs []UnreadMessage
	for _, m := range eligible {
		if slices.Contains(p.ids, m.ID) {
			msgs = append(msgs, m)
		}
	}
	if len(msgs) == 0 || n.occupiedFor(area, p.spec.Folder, msgs, now) {
		return
	}
	spec := LaunchSpec{Provider: p.spec.Provider, Folder: p.spec.Folder, Prompt: launchPrompt(n, msgs)}
	n.startLaunch(ctx, area, spec, msgs, 1, now)
}

// launchSeen keeps session, whose id the running first turn of p just named,
// as the area's last one.
func (n *Node) launchSeen(area string, p *pendingLaunch, session string) {
	r := n.sess
	r.mu.Lock()
	r.recent[area] = LastSession{SessionID: session, Provider: p.spec.Provider, Folder: p.spec.Folder, At: time.Now().UTC()}
	if err := writeJSON(r.recentPath, r.recent); err != nil {
		n.log.Warn("save last sessions", "err", err)
	}
	r.mu.Unlock()
	n.log.Info("opened session started", "area", area, "provider", p.spec.Provider, "session", session)
}

// startLaunch opens spec for msgs (try number tries) and records it pending.
func (n *Node) startLaunch(ctx context.Context, area string, spec LaunchSpec, msgs []UnreadMessage, tries int, now time.Time) {
	if !n.autoTake(now) {
		return
	}
	d := n.deliv
	list := ids(msgs)
	d.mu.Lock()
	d.last[area] = now
	d.mu.Unlock()
	n.report(list, AttemptLaunchRequested)
	n.log.Info("opening a session for unread messages", "area", area, "provider", spec.Provider, "folder", spec.Folder,
		"resume", spec.ResumeID, "try", tries, "messages", len(list))
	if err := n.launcher.Launch(ctx, spec); err != nil {
		reason := "start_error"
		switch {
		case errors.Is(err, ErrNoTerminal):
			reason = ErrNoTerminal.Error()
		case errors.Is(err, ErrNoAgent):
			reason = ErrNoAgent.Error()
		}
		// Nothing started: not spent, the ladder tries again after
		// launchDebounce (launch_failed is reported once).
		n.log.Warn("opening a session failed", "area", area, "provider", spec.Provider, "err", err)
		d.mu.Lock()
		delete(d.pending, area)
		d.mu.Unlock()
		n.report(list, AttemptLaunchFailed+":"+reason)
		return
	}
	before := n.areaSessions(area, now)
	d.mu.Lock()
	d.pending[area] = &pendingLaunch{at: now, tries: tries, spec: spec, ids: list, before: before}
	d.mu.Unlock()
}

// areaSessions is the set of live sessions of area (nil: none).
func (n *Node) areaSessions(area string, now time.Time) map[string]bool {
	r := n.sess
	r.mu.Lock()
	defer r.mu.Unlock()
	var out map[string]bool
	for id, s := range r.sessions {
		if s.Area == area && s.live(now) {
			if out == nil {
				out = map[string]bool{}
			}
			out[id] = true
		}
	}
	return out
}

// areaHasNew reports whether a live session of area is not one of before
// (a launch's own, when others were live already).
func (n *Node) areaHasNew(area string, before map[string]bool, now time.Time) bool {
	for id := range n.areaSessions(area, now) {
		if !before[id] {
			return true
		}
	}
	return false
}

// orphans are the msgs no live session of area can take any more, so a new
// session opens for them although sessions are registered: a message whose
// leases failed, when every live session of the area was passed over for it
// (maxOwnerFails) or is idle, cannot be woken and had no hook event for
// AffinityLapse (a registration alone never counts).
func (n *Node) orphans(area string, msgs []UnreadMessage, now time.Time) []UnreadMessage {
	queue := n.canQueue()
	r := n.sess
	type cand struct {
		s        Session
		wakeable bool
	}
	var list []cand
	r.mu.Lock()
	for _, s := range r.sessions {
		if s.Area != area || !s.live(now) {
			continue
		}
		_, inbox := r.inbox[s.SessionID]
		wakeable := (inbox && n.poster != nil) || (s.Wake == WakeQueue && queue) || s.Wake == WakeRewake
		list = append(list, cand{*s, wakeable})
	}
	r.mu.Unlock()
	var out []UnreadMessage
	for _, m := range msgs {
		l, ok := n.leases.get(m.ID)
		if !ok || len(l.Fails) == 0 {
			continue // no lease of it failed: its sessions still get it
		}
		can := slices.ContainsFunc(list, func(c cand) bool {
			recent := now.Sub(c.s.activeAt()) < AffinityLapse
			if !c.s.Idle && recent {
				return true // in a turn: its hooks take it
			}
			return l.Fails[c.s.SessionID] < maxOwnerFails && (c.wakeable || recent)
		})
		if !can {
			out = append(out, m)
		}
	}
	return out
}

// launchPrompt is the first prompt of an opened session: «agent-link:
// непрочитанные сообщения (2) в чате «…» от KPECTIK — прочитай их и действуй».
func launchPrompt(n *Node, msgs []UnreadMessage) string {
	var from []string
	title := ""
	for _, m := range msgs {
		if !slices.Contains(from, m.From) {
			from = append(from, m.From)
		}
		if title == "" && m.ChatID != "" {
			if info, err := n.Chat(m.ChatID); err == nil {
				title = info.Title
			}
		}
	}
	if utf8.RuneCountInString(title) > 60 {
		title = string([]rune(title)[:60]) + "…"
	}
	where := ""
	if title != "" {
		where = fmt.Sprintf(" в чате «%s»", title)
	}
	return launchSafe(fmt.Sprintf("agent-link: непрочитанные сообщения (%d)%s от %s — прочитай их и действуй",
		len(msgs), where, strings.Join(from, ", ")))
}

func ids(msgs []UnreadMessage) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.ID)
	}
	return out
}
