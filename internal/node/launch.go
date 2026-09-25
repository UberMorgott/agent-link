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
// (SessionLauncher), while auto-open is on (SetAutoOpen).
//
// In the agent's desktop app (DirectLauncher, launch mode LaunchDesktop, the
// app installed): the node claims the messages for the launch first
// (launchClaim: no session's hooks take them meanwhile), runs the session's
// first turn headless with the messages themselves as its prompt (startDirect)
// and shows the session in the app as soon as its id is known. Only a turn
// that succeeded proves the session took them: then they are acknowledged as
// that session's (launch_confirmed). A turn that failed, was interrupted or
// timed out drops the claim (the hooks deliver them again), is
// launch_failed:<reason> and falls back to Terminal once.
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
// Eligible: unread, asking this node, not taken by the worker, not paused by
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

// SetAutoOpen turns opening sessions on or off (a project's «auto-open»
// setting); it may change while the node runs.
func (n *Node) SetAutoOpen(on bool) { n.autoOpen.Store(on) }

// AutoOpen reports whether the node opens sessions.
func (n *Node) AutoOpen() bool { return n.autoOpen.Load() }

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

// deliveryState is the ladder's memory (never persisted: after a restart the
// ladder starts over, which may report an event again).
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
	spent   map[string]bool
	pending map[string]*pendingLaunch // by area
	last    map[string]time.Time      // last launch per area
}

type pendingLaunch struct {
	at    time.Time
	tries int
	spec  LaunchSpec
	ids   []string
	// direct: a desktop-app launch running its first turn (runDirect ends it).
	direct bool
}

func newDeliveryState() *deliveryState {
	return &deliveryState{provider: ProviderClaude, mode: LaunchDesktop, sent: map[string]bool{}, woken: map[string]bool{}, spent: map[string]bool{},
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
	d := n.deliv
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, m := range page.Messages {
		switch {
		case !m.AsksYou || m.OwnHuman || m.Assigned != "" || held[m.ID]:
		case m.Paused:
			paused = append(paused, m)
		case d.spent[m.ID] || now.Sub(m.ReceivedAt) < launchGrace:
		default:
			eligible = append(eligible, m)
		}
	}
	return eligible, paused
}

// launchDue runs one step of the launch ladder for every area.
func (n *Node) launchDue(ctx context.Context, now time.Time) {
	if n.launcher == nil || !n.AutoOpen() {
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
	if n.areaLive(area, now) {
		if p != nil {
			d.mu.Lock()
			delete(d.pending, area)
			for _, id := range p.ids {
				d.spent[id] = true
			}
			d.mu.Unlock()
			n.log.Info("opened session confirmed", "area", area, "provider", p.spec.Provider, "tries", p.tries)
			n.report(p.ids, AttemptLaunchConfirmed)
		}
		return
	}
	if p != nil {
		if now.Sub(p.at) < launchConfirm {
			return
		}
		if p.tries >= launchTries || len(eligible) == 0 {
			d.mu.Lock()
			delete(d.pending, area)
			for _, id := range p.ids {
				d.spent[id] = true
			}
			d.mu.Unlock()
			if len(eligible) > 0 {
				n.log.Warn("opened session never started", "area", area, "folder", p.spec.Folder, "tries", p.tries)
				n.report(p.ids, AttemptLaunchFailed+":timeout")
			}
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
	msgs := n.launchClaim(area, eligible[:FitUnread(eligible)])
	if len(msgs) == 0 {
		return
	}
	spec.Prompt = WakePrompt(msgs, len(eligible)-len(msgs), spec.Folder, randomHex(8))
	p := &pendingLaunch{at: now, tries: 1, spec: spec, ids: ids(msgs), direct: true}
	d := n.deliv
	d.mu.Lock()
	d.last[area] = now
	d.pending[area] = p
	d.mu.Unlock()
	n.report(p.ids, AttemptLaunchRequested)
	n.log.Info("opening a session in the desktop app for unread messages", "area", area, "provider", spec.Provider,
		"folder", spec.Folder, "resume", spec.ResumeID, "messages", len(p.ids))
	n.directWG.Go(func() { n.runDirect(ctx, dl, area, p) })
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
		}
	})
	mu.Lock()
	id := session
	mu.Unlock()
	d := n.deliv
	ok := id != "" && (err == nil || errors.Is(err, ErrOpenApp))
	d.mu.Lock()
	if d.pending[area] == p {
		delete(d.pending, area)
	}
	if ok {
		for _, m := range p.ids {
			d.spent[m] = true
		}
	}
	d.last[area] = time.Now() // the ladder waits: the fallback below goes first
	d.mu.Unlock()
	if ok {
		if err != nil {
			n.log.Warn("the opened session's desktop app did not open", "area", area, "session", id, "err", err)
		}
		if _, aerr := n.Ack("", AckRequest{IDs: p.ids, SessionID: id}); aerr != nil {
			n.log.Warn("acknowledging the opened session's messages", "session", id, "err", aerr)
		}
		n.log.Info("opened session's first turn done", "area", area, "provider", p.spec.Provider, "session", id)
		n.report(p.ids, AttemptLaunchConfirmed)
		return
	}
	n.unclaim(launchOwner(area), p.ids) // the hooks, or the fallback, deliver them
	reason := "start_error"
	switch {
	case errors.Is(err, ErrNoAgent):
		reason = ErrNoAgent.Error()
	case errors.Is(err, ErrTurnTimeout):
		reason = ErrTurnTimeout.Error()
	case errors.Is(err, ErrTurnInterrupted):
		reason = ErrTurnInterrupted.Error()
	case id != "":
		reason = "turn_error"
	}
	n.log.Warn("opening a session in the desktop app failed; opening Terminal", "area", area, "provider", p.spec.Provider,
		"session", id, "reason", reason, "err", err)
	n.report(p.ids, AttemptLaunchFailed+":"+reason)
	n.fallbackTerminal(ctx, area, p)
}

// fallbackTerminal opens Terminal once for the messages of the failed desktop
// launch p that are still eligible; the Terminal ladder takes it from there.
func (n *Node) fallbackTerminal(ctx context.Context, area string, p *pendingLaunch) {
	now := time.Now()
	eligible, _ := n.launchable(p.spec.Folder, now.Add(launchGrace))
	var msgs []UnreadMessage
	for _, m := range eligible {
		if slices.Contains(p.ids, m.ID) {
			msgs = append(msgs, m)
		}
	}
	if len(msgs) == 0 {
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
		n.log.Warn("opening a session failed", "area", area, "provider", spec.Provider, "err", err)
		d.mu.Lock()
		delete(d.pending, area)
		for _, id := range list {
			d.spent[id] = true
		}
		d.mu.Unlock()
		n.report(list, AttemptLaunchFailed+":"+reason)
		return
	}
	d.mu.Lock()
	d.pending[area] = &pendingLaunch{at: now, tries: tries, spec: spec, ids: list}
	d.mu.Unlock()
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
