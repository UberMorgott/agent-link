package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Live agent sessions (Claude Code, Codex) register on the node through their
// hooks: POST /sessions registers or heartbeats one, DELETE /sessions/{id}
// ends it. A session is bound to the area of its folder (FolderArea); the
// oldest live session of an area is its primary one. While an area has a live
// session the worker leaves its requests to it (ClaimRun). A session that
// sends no heartbeat within its TTL is gone. The registry is kept in
// sessions.json, so a restart of the app does not hand a session's requests
// to the worker.

// Wake modes of a session: how it hears of new messages.
const (
	// WakeRewake: the session's hook waits in the background and wakes the
	// idle session when a message arrives (Claude Code asyncRewake).
	WakeRewake = "rewake"
	// WakeNextEvent: the session sees new messages at its next hook event.
	WakeNextEvent = "next-event"
	// WakeQueue: the node wakes the idle session by queueing a message for it
	// in its agent (Codex: `codex queue`, see wake.go); its hooks deliver.
	WakeQueue = "queue"
)

// Session registry limits.
const (
	// SessionTTL is how long a session stays live without a heartbeat.
	SessionTTL = 15 * time.Minute
	// MaxSessionTTL is the longest TTL a registration may ask for.
	MaxSessionTTL = 24 * time.Hour
	maxSessions   = 64
)

// ErrUnknownSession: no live session with that id is registered.
var ErrUnknownSession = errors.New("unknown session")

// SessionRequest is the body of POST /sessions.
type SessionRequest struct {
	SessionID string `json:"session_id"`
	Provider  string `json:"provider"` // "claude", "codex", ...
	Folder    string `json:"folder"`   // absolute path of the session's working folder
	Wake      string `json:"wake,omitempty"`
	// TTLSec overrides SessionTTL for this session (at most MaxSessionTTL).
	TTLSec int `json:"ttl_sec,omitempty"`
	// Idle: the session ended its turn and waits for its person (WakeQueue
	// wakes only an idle session).
	Idle bool `json:"idle,omitempty"`
	// CodexHome is the session's CODEX_HOME ("": Codex's default), where
	// WakeQueue queues its message.
	CodexHome string `json:"codex_home,omitempty"`
	// InboxSocket and InboxToken are a Claude Code session's cross-session
	// inbox (CLAUDE_CODE_MESSAGING_SOCKET, CLAUDE_CODE_MESSAGING_TOKEN): the
	// node wakes the idle session by posting a prompt there (inbox.go). They
	// are kept in memory only, expire with the session and never leave the node.
	InboxSocket string `json:"inbox_socket,omitempty"`
	InboxToken  string `json:"inbox_token,omitempty"`
	// Heartbeat: a background keep-alive (Claude's waiter), not an event of
	// the session: it keeps the session live (LastSeen), not active
	// (LastActive).
	Heartbeat bool `json:"heartbeat,omitempty"`
	// Agents are the session's live subagents (Claude Code agent_id) as its
	// hooks saw them: a reply for one of them is its hooks' to deliver, no
	// wake of the session takes it (claimLocked). Each registration replaces
	// them.
	Agents []string `json:"agents,omitempty"`
	// Native: Claude Code's in-process hooks module registers the session
	// (plugins/agent-link, hooks/register.ts) and is its only way in: it
	// delivers, wakes and acknowledges by itself. From then on, while the
	// session lives, its shell hooks and their waiter stand down: a
	// registration without Native only keeps it live, and their unread
	// queries and claims are answered with nothing (sessionNative). Only while
	// the module is heard: one silent for nativeSeenTTL (refused at a reload,
	// disabled, crashed) gives the session back to its shell hooks.
	Native bool `json:"native,omitempty"`
}

// Session is one registered live session.
type Session struct {
	SessionID    string    `json:"session_id"`
	Provider     string    `json:"provider"`
	Folder       string    `json:"folder"`
	Area         string    `json:"area"` // "" is the working folder (direct messages)
	Wake         string    `json:"wake"`
	TTLSec       int       `json:"ttl_sec"`
	RegisteredAt time.Time `json:"registered_at"`
	LastSeen     time.Time `json:"last_seen"`
	// LastActive is the session's last hook event (a registration that is no
	// Heartbeat, or activity): what it did last, not only that it lives.
	LastActive time.Time `json:"last_active,omitzero"`
	// Primary: the oldest live session of its area, the one that answers.
	Primary bool `json:"primary"`
	// Asked is the wake mode the session asked for; Wake is what the node
	// gives it (WakeQueue only while it can queue, else WakeNextEvent).
	Asked string `json:"asked_wake,omitempty"`
	Idle  bool   `json:"idle,omitempty"`
	// IdleAt is the last hook event that said the session is idle (a
	// registration that is no Heartbeat): a turn its rollout shows started
	// after it is one the hooks did not report (queueHeld).
	IdleAt    time.Time `json:"idle_at,omitzero"`
	CodexHome string    `json:"codex_home,omitempty"`
	// Woken: the node queued a wake in this idle period; Wakes of them (at
	// most maxIdleWakes), the last at WokeAt. A wake the session never took
	// is retried once its claim lapses (wakeIdle); a WakeQueue session is
	// woken for newer messages after that too (maxStaleWakes). WakeCapped:
	// every wake of the idle period is spent and the authors of what waits
	// for the session heard needs_human, once per message (CapNoted: their
	// ids, at most capNotedMax).
	Woken      bool      `json:"woken,omitempty"`
	Wakes      int       `json:"wakes,omitempty"`
	WokeAt     time.Time `json:"woke_at,omitzero"`
	WakeCapped bool      `json:"wake_capped,omitempty"`
	CapNoted   []string  `json:"cap_noted,omitempty"`
	// Inbox: the node holds a usable inbox of the session (InboxSocket) and
	// wakes it there when idle. Not persisted: the address lives in memory.
	Inbox bool `json:"inbox,omitempty"`
	// Agents: the live subagents of the last registration that was a hook
	// event (SessionRequest.Agents), at AgentsAt; they lapse agentLiveTTL
	// after it (a missed SubagentStop does not hold their replies forever).
	Agents   []string  `json:"agents,omitempty"`
	AgentsAt time.Time `json:"agents_at,omitzero"`
	// Doing is what the main agent does in its turn (a busy AgentStatus
	// state, its hooks' tool kind: SessionDoing) since DoingAt; Subs the
	// subagents a Codex session reports. A turn's start or end resets them.
	Doing   string    `json:"doing,omitempty"`
	DoingAt time.Time `json:"doing_at,omitzero"`
	Subs    int       `json:"subs,omitempty"`
	// Native: its in-process module delivers to it (SessionRequest.Native),
	// last heard at NativeSeen (a registration or heartbeat of the module: the
	// shell hooks' never count).
	Native     bool      `json:"native,omitempty"`
	NativeSeen time.Time `json:"native_seen,omitzero"`
}

// maxSessionAgents bounds SessionRequest.Agents.
const maxSessionAgents = 64

// agentLiveTTL: a session's reported subagents count as live this long after
// the last hook event that reported them (the hooks' own liveAgentTTL).
const agentLiveTTL = 30 * time.Minute

// liveAgents are the session's subagents still live at now (Agents).
func (s Session) liveAgents(now time.Time) []string {
	if now.Sub(s.AgentsAt) >= agentLiveTTL {
		return nil
	}
	return s.Agents
}

// LiveAgents are the session's subagents still live at now (Agents, until
// agentLiveTTL after the hook event that reported them).
func (s Session) LiveAgents(now time.Time) []string { return s.liveAgents(now) }

func (s Session) live(now time.Time) bool {
	return now.Sub(s.LastSeen) < time.Duration(s.TTLSec)*time.Second
}

// activeAt is when the session was last active (LastActive; LastSeen for a
// record saved before it existed).
func (s Session) activeAt() time.Time {
	if s.LastActive.IsZero() {
		return s.LastSeen
	}
	return s.LastActive
}

type sessionRegistry struct {
	path string

	mu       sync.Mutex
	sessions map[string]*Session
	// last is the latest activity each session reported, for elapsed times.
	last map[string]ActivityState
	// on is, per session, the chats (chat -> request) its latest running main
	// activity went to: refreshActivity re-sends it there while the session
	// stays busy.
	on map[string]map[string]string

	// claimMu serializes Claim and the session filter of Unread; claims maps
	// an unread message id to the session that took it for delivery (Claim).
	// Taken before mu and the chat store's lock, never inside them.
	claimMu sync.Mutex
	claims  map[string]hold

	// inbox is each live Claude session's inbox address (memory only, never
	// persisted or sent); recent is the last session seen per area, kept in
	// last_sessions.json, which a launch resumes (launch.go).
	inbox      map[string]inboxAddr
	recent     map[string]LastSession
	recentPath string
	pins       map[string]PinnedSession // area -> preferred live Codex session
	pinsPath   string

	// gone are the sessions saveLocked dropped as expired (TTL, no
	// SessionEnd) whose orphaned replies expireSessions has not reported yet.
	gone []string

	// left are the sessions no longer registered here (memory only): ended by
	// their SessionEnd hook, or dropped as expired with the LastSeen they had.
	// awake is since when the node runs without a gap (its start, or the end of
	// a sleep: noteAwake), passAt its last wake loop pass. A session that only
	// expired counts as gone for good (absence.goneFor) after sessionGoneAfter of that
	// time, not before: one heartbeat missed over a sleep or a restart is not
	// an end.
	left   map[string]leftSession
	awake  time.Time
	passAt time.Time
	// elsewhere are the sessions the last reconciliation found live though not
	// registered here (ReclaimReplies; another context of the process holds
	// them): the orphan sweep leaves them until the next one (OrphanReplies).
	elsewhere map[string]bool
}

// leftSession is a session that left the registry: ended (EndSession), else
// last seen at seen; at is when it left.
type leftSession struct {
	ended bool
	seen  time.Time
	at    time.Time
}

// Gone for good (absence.goneFor).
const (
	// sessionGoneAfter: a session that expired without SessionEnd is gone for
	// good once it went unseen this long while the node was awake.
	sessionGoneAfter = time.Hour
	// sleepGap: a wake loop pass this long after the one before means the
	// machine slept (the loop's longest wait is wakeDormant).
	sleepGap = 5 * time.Minute
)

// noteAwake records a wake loop pass at now: after a gap (the machine slept)
// the sessions' time unseen starts over, they had no chance to report.
func (r *sessionRegistry) noteAwake(now time.Time) {
	now = now.Round(0) // wall clock: the monotonic one may stop in a sleep
	r.mu.Lock()
	defer r.mu.Unlock()
	if now.Sub(r.passAt) >= sleepGap {
		r.awake = now
	}
	r.passAt = now
	maps.DeleteFunc(r.left, func(_ string, l leftSession) bool { return now.Sub(l.at) > strandedMax })
}

// absence is what the registry knows of session sid, which is not live: ended
// (EndSession), else when it, or a node awake to hear of it, was last seen.
type absence struct {
	ended bool
	seen  time.Time
}

func (r *sessionRegistry) absence(sid string) absence {
	r.mu.Lock()
	defer r.mu.Unlock()
	l := r.left[sid]
	a := absence{ended: l.ended, seen: latest(r.awake, l.seen)}
	if s := r.sessions[sid]; s != nil {
		a.seen = latest(a.seen, s.LastSeen)
	}
	return a
}

// goneFor reports whether the session is gone for good at now as far as a
// message it read at readAt goes: it ended, or nothing was seen of it for
// sessionGoneAfter while the node was awake.
func (a absence) goneFor(readAt, now time.Time) bool {
	return a.ended || now.Sub(latest(a.seen, readAt)) >= sessionGoneAfter
}

// latest is the latest of ts.
func latest(ts ...time.Time) time.Time {
	var out time.Time
	for _, t := range ts {
		if t.After(out) {
			out = t
		}
	}
	return out
}

// LastSession is the latest agent session registered for an area: a launch
// with no live session there resumes it.
type LastSession struct {
	SessionID string    `json:"session_id"`
	Provider  string    `json:"provider"`
	Folder    string    `json:"folder"`
	At        time.Time `json:"at"`
}

// PinnedSession is the live Codex thread preferred for untargeted messages in
// its local project area.
type PinnedSession struct {
	SessionID string `json:"session_id"`
	Provider  string `json:"provider"`
	Folder    string `json:"folder"`
	Area      string `json:"area"`
	CodexHome string `json:"codex_home,omitempty"`
}

// PinSessionRequest is the body of POST /session-pin.
type PinSessionRequest struct {
	SessionID string `json:"session_id"`
}

func openSessions(dir string) (*sessionRegistry, error) {
	r := &sessionRegistry{path: filepath.Join(dir, "sessions.json"), sessions: map[string]*Session{}, last: map[string]ActivityState{},
		on: map[string]map[string]string{}, claims: map[string]hold{}, inbox: map[string]inboxAddr{},
		recent: map[string]LastSession{}, recentPath: filepath.Join(dir, "last_sessions.json"),
		pins: map[string]PinnedSession{}, pinsPath: filepath.Join(dir, "pinned_sessions.json"),
		left: map[string]leftSession{}, elsewhere: map[string]bool{}}
	r.awake = time.Now().Round(0)
	r.passAt = r.awake
	var list []Session
	if err := readJSON(r.path, &list); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, s := range list {
		s.Inbox = false // its address was in memory only
		r.sessions[s.SessionID] = &s
	}
	if err := readJSON(r.recentPath, &r.recent); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if r.recent == nil {
		r.recent = map[string]LastSession{}
	}
	if err := readJSON(r.pinsPath, &r.pins); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if r.pins == nil {
		r.pins = map[string]PinnedSession{}
	}
	return r, nil
}

// PinSession prefers one live Codex thread for its local project area.
func (n *Node) PinSession(req PinSessionRequest) (PinnedSession, error) {
	s, ok := n.SessionLive(req.SessionID)
	if !ok {
		return PinnedSession{}, fmt.Errorf("%w %s", ErrUnknownSession, req.SessionID)
	}
	if s.Provider != ProviderCodex {
		return PinnedSession{}, fmt.Errorf("%w: only a Codex session can be pinned", ErrBadRequest)
	}
	p := PinnedSession{SessionID: s.SessionID, Provider: s.Provider, Folder: s.Folder, Area: s.Area, CodexHome: s.CodexHome}
	r := n.sess
	r.mu.Lock()
	old, hadOld := r.pins[s.Area]
	r.pins[s.Area] = p
	err := writeJSON(r.pinsPath, r.pins)
	if err != nil {
		if hadOld {
			r.pins[s.Area] = old
		} else {
			delete(r.pins, s.Area)
		}
	}
	r.mu.Unlock()
	if err != nil {
		return PinnedSession{}, err
	}
	n.changed("sessions")
	return p, nil
}

// UnpinSession removes req.SessionID as a preferred recipient.
func (n *Node) UnpinSession(req PinSessionRequest) (PinnedSession, error) {
	r := n.sess
	r.mu.Lock()
	for area, p := range r.pins {
		if p.SessionID != req.SessionID {
			continue
		}
		delete(r.pins, area)
		if err := writeJSON(r.pinsPath, r.pins); err != nil {
			r.pins[area] = p
			r.mu.Unlock()
			return PinnedSession{}, err
		}
		r.mu.Unlock()
		n.changed("sessions")
		return p, nil
	}
	r.mu.Unlock()
	return PinnedSession{}, fmt.Errorf("%w %s", ErrUnknownSession, req.SessionID)
}

// PinnedSession returns area's preferred recipient.
func (n *Node) PinnedSession(area string) (PinnedSession, bool) { return n.sess.pinned(area) }

func (r *sessionRegistry) pinned(area string) (PinnedSession, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.pins[area]
	return p, ok
}

// pinKeepEvery: how often keepPinned refreshes (and saves) a pinned thread.
const pinKeepEvery = time.Minute

// keepPinned keeps each area's pinned Codex thread registered while it is idle
// and the node can wake it (WakeQueue): Codex sends no heartbeat, so an idle
// UI thread would expire after its TTL though `codex queue` still wakes it. It
// is a heartbeat (LastSeen), no activity. It is kept through the stale wakes
// of its idle period (maxStaleWakes; together their waits outlast its TTL)
// until every wake is spent and the last one stale (wakeSpent: wakeIdle
// reports the cap then). A thread in a turn, or one whose wakes are spent, is
// left to its TTL: then the launch ladder takes its messages.
func (n *Node) keepPinned(now time.Time) {
	if !n.canQueue() {
		return
	}
	r := n.sess
	r.mu.Lock()
	defer r.mu.Unlock()
	changed := false
	for _, p := range r.pins {
		s := r.sessions[p.SessionID]
		if s == nil || !s.live(now) || !s.Idle || s.Wake != WakeQueue || now.Sub(s.LastSeen) < pinKeepEvery || s.wakeSpent(now) {
			continue
		}
		s.LastSeen, changed = now, true
	}
	if changed {
		if err := r.saveLocked(now); err != nil {
			n.log.Warn("save sessions", "err", err)
		}
	}
}

// saveLocked writes the live sessions. The caller holds r.mu.
func (r *sessionRegistry) saveLocked(now time.Time) error {
	list := []Session{}
	for id, s := range r.sessions {
		if !s.live(now) {
			delete(r.sessions, id)
			delete(r.last, id)
			delete(r.on, id)
			delete(r.inbox, id)
			r.gone = append(r.gone, id)
			r.left[id] = leftSession{seen: s.LastSeen, at: now}
			continue
		}
		c := *s
		c.Inbox = false
		list = append(list, c)
	}
	slices.SortFunc(list, func(a, b Session) int { return strings.Compare(a.SessionID, b.SessionID) })
	return writeJSON(r.path, list)
}

func validSessionID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, c := range id {
		ok := c <= unicode.MaxASCII && (unicode.IsLetter(c) || unicode.IsDigit(c) || strings.ContainsRune("-_.:", c))
		if !ok {
			return false
		}
	}
	return true
}

// RegisterSession registers a live session or refreshes it (a heartbeat: the
// same session_id again). Its folder must be one of this node's
// (ErrFolderUnbound); a project node without a folder takes none (ErrNeedsFolder).
func (n *Node) RegisterSession(req SessionRequest) (Session, error) {
	switch {
	case !validSessionID(req.SessionID):
		return Session{}, fmt.Errorf("%w: invalid session_id", ErrBadRequest)
	case req.Provider == "" || len(req.Provider) > 32 || !validSessionID(req.Provider):
		return Session{}, fmt.Errorf("%w: invalid provider", ErrBadRequest)
	case !filepath.IsAbs(req.Folder):
		return Session{}, fmt.Errorf("%w: folder must be an absolute path", ErrBadRequest)
	case req.TTLSec < 0 || time.Duration(req.TTLSec)*time.Second > MaxSessionTTL:
		return Session{}, fmt.Errorf("%w: ttl_sec out of range", ErrBadRequest)
	case len(req.CodexHome) > 1024 || (req.CodexHome != "" && !filepath.IsAbs(req.CodexHome)):
		return Session{}, fmt.Errorf("%w: codex_home must be an absolute path", ErrBadRequest)
	case len(req.Agents) > maxSessionAgents || slices.ContainsFunc(req.Agents, func(a string) bool { return !validSessionID(a) }):
		return Session{}, fmt.Errorf("%w: invalid agents", ErrBadRequest)
	}
	// An inbox the node cannot use is ignored, not refused: the session still
	// registers and its waiter wakes it.
	if !validInboxSocket(req.InboxSocket) || !validInboxToken(req.InboxToken) || req.Provider != ProviderClaude {
		req.InboxSocket, req.InboxToken = "", ""
	}
	switch req.Wake {
	case "":
		req.Wake = WakeNextEvent
	case WakeRewake, WakeNextEvent, WakeQueue:
	default:
		return Session{}, fmt.Errorf("%w: wake must be %q, %q or %q", ErrBadRequest, WakeRewake, WakeNextEvent, WakeQueue)
	}
	asked := req.Wake
	if asked == WakeQueue && !n.canQueue() {
		req.Wake = WakeNextEvent
	}
	area, ok := n.FolderArea(req.Folder)
	switch {
	case n.NeedsFolder():
		return Session{}, ErrNeedsFolder
	case !ok:
		return Session{}, fmt.Errorf("%w: %s", ErrFolderUnbound, req.Folder)
	}
	ttl := req.TTLSec
	if ttl == 0 {
		ttl = int(SessionTTL / time.Second)
	}
	if !req.Native {
		// A module that fell silent has the session no more (sessionNative
		// gives it back): this registration is then the hooks' own again.
		n.sessionNative(req.SessionID)
	}
	now := time.Now().UTC()
	r := n.sess
	r.mu.Lock()
	s := r.sessions[req.SessionID]
	delete(r.left, req.SessionID)
	delete(r.elsewhere, req.SessionID)
	if s != nil && s.live(now) && s.Native && !req.Native {
		// The session's module has it: its shell hooks stand down. Their
		// registration keeps it live and changes nothing (no inbox, no idle
		// state, no subagents of theirs).
		s.LastSeen = now
		err := r.saveLocked(now)
		out := r.withPrimaryLocked(*s, now)
		r.mu.Unlock()
		return out, err
	}
	if req.Native {
		// The module wakes the session itself: the node posts to no inbox.
		req.InboxSocket, req.InboxToken = "", ""
		delete(r.inbox, req.SessionID)
	}
	fresh := s == nil || !s.live(now) || s.Wake != req.Wake || s.Area != area
	back := s == nil || !s.live(now)
	if back {
		if len(r.sessions) >= maxSessions {
			_ = r.saveLocked(now) // drops the dead ones
		}
		if len(r.sessions) >= maxSessions {
			r.mu.Unlock()
			return Session{}, fmt.Errorf("%w: too many sessions", ErrBadRequest)
		}
		s = &Session{SessionID: req.SessionID, RegisteredAt: now}
		r.sessions[req.SessionID] = s
		// Back before the sweep: not gone any more.
		r.gone = slices.DeleteFunc(r.gone, func(id string) bool { return id == req.SessionID })
	}
	seen := s.LastSeen // before this registration
	s.Provider, s.Folder, s.Area, s.Wake, s.TTLSec, s.LastSeen = req.Provider, filepath.Clean(req.Folder), area, req.Wake, ttl, now
	switch {
	case !req.Heartbeat:
		s.LastActive = now
	case s.LastActive.IsZero():
		// A heartbeat is no activity: a record from before LastActive keeps
		// when it was last seen (a new session: now, its registration).
		s.LastActive = seen
		if s.LastActive.IsZero() {
			s.LastActive = now
		}
	}
	// A new idle period (or none) may be woken again.
	s.Woken = s.Woken && s.Idle && req.Idle
	if !s.Woken {
		s.Wakes, s.WokeAt, s.WakeCapped, s.CapNoted = 0, time.Time{}, false, nil
	}
	// A turn begins or ends (or the session is new): what it did is over.
	if back || s.Idle != req.Idle {
		s.Doing, s.DoingAt = "", now
		if req.Idle {
			s.Subs = 0
		}
	}
	s.Asked, s.Idle, s.CodexHome, s.Native = asked, req.Idle, req.CodexHome, req.Native
	if req.Idle && !req.Heartbeat {
		s.IdleAt = now
	}
	if req.Native {
		s.NativeSeen = now
	}
	if !req.Heartbeat {
		// A keep-alive (the idle session's waiter) saw no subagent event: it
		// neither changes nor refreshes them.
		s.Agents, s.AgentsAt = slices.Clone(req.Agents), now
	}
	if req.InboxSocket != "" {
		a := inboxAddr{socket: req.InboxSocket, token: req.InboxToken}
		if old, ok := r.inbox[req.SessionID]; ok && old.socket == a.socket && old.token == a.token {
			a.woke = old.woke // a heartbeat keeps the time of the last wake
		}
		r.inbox[req.SessionID] = a
	}
	if req.Provider == ProviderClaude || req.Provider == ProviderCodex {
		ls := LastSession{SessionID: req.SessionID, Provider: req.Provider, Folder: s.Folder, At: now}
		if old, ok := r.recent[area]; !ok || old.SessionID != ls.SessionID || old.Folder != ls.Folder || now.Sub(old.At) > time.Hour {
			r.recent[area] = ls
			if err := writeJSON(r.recentPath, r.recent); err != nil {
				n.log.Warn("save last sessions", "err", err)
			}
		}
	}
	err := r.saveLocked(now)
	out := r.withPrimaryLocked(*s, now)
	r.mu.Unlock()
	if err != nil {
		return Session{}, err
	}
	if back {
		n.sessionChanged(req.SessionID, true)
	}
	if fresh {
		n.presenceChanged()
	}
	n.changed("sessions")
	return out, nil
}

// EndSession removes a session (its SessionEnd hook).
func (n *Node) EndSession(id string) error {
	r := n.sess
	r.mu.Lock()
	_, ok := r.sessions[id]
	delete(r.sessions, id)
	delete(r.last, id)
	delete(r.on, id)
	delete(r.inbox, id)
	now := time.Now()
	r.left[id] = leftSession{ended: true, at: now}
	err := r.saveLocked(now)
	r.mu.Unlock()
	n.clearPinnedSession(id)
	// Its leases go back at once: another session or a launch takes them.
	n.revokeLeases(id, nil, "session_end", true)
	if !ok {
		return fmt.Errorf("%w %s", ErrUnknownSession, id)
	}
	n.sessionChanged(id, false)
	n.presenceChanged()
	n.changed("sessions")
	return err
}

func (n *Node) clearPinnedSession(id string) {
	r := n.sess
	r.mu.Lock()
	removed := map[string]PinnedSession{}
	for area, p := range r.pins {
		if p.SessionID == id {
			removed[area] = p
			delete(r.pins, area)
		}
	}
	if len(removed) > 0 {
		if err := writeJSON(r.pinsPath, r.pins); err != nil {
			maps.Copy(r.pins, removed)
			n.log.Warn("clear pinned session", "session", id, "error", err)
			removed = nil
		}
	}
	r.mu.Unlock()
	if len(removed) > 0 {
		n.changed("sessions")
	}
}

// sessionChanged hands a session that ended or expired (live false), or
// registered anew (live true), to the session hook, else handles its
// orphaned replies here (OrphanReplies, ReclaimReplies).
func (n *Node) sessionChanged(sid string, live bool) { n.sessionChangedAt(sid, live, time.Now()) }

// sessionChangedAt is sessionChanged as of now.
func (n *Node) sessionChangedAt(sid string, live bool, now time.Time) {
	switch {
	case n.onSession != nil:
		n.onSession(sid, live)
	case live:
		n.ReclaimReplies(sid)
	default:
		n.orphanReplies(sid, now)
	}
}

// SessionLive returns live session sid registered here.
func (n *Node) SessionLive(sid string) (Session, bool) {
	r := n.sess
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.sessions[sid]; s != nil && s.live(now) {
		return *s, true
	}
	return Session{}, false
}

// ReclaimReplies gives session sid, live again (resumed), back its orphaned
// replies: they are its own, no longer waiting for a person. Live though not
// registered here (another context holds it), the orphan sweep leaves it
// until its next reconciliation.
func (n *Node) ReclaimReplies(sid string) {
	r := n.sess
	r.claimMu.Lock()
	ids, err := n.chats.clearOrphaned(sid)
	r.mu.Lock()
	if s := r.sessions[sid]; s == nil || !s.live(time.Now()) {
		r.elsewhere[sid] = true
	}
	r.mu.Unlock()
	r.claimMu.Unlock()
	if err != nil {
		n.log.Warn("clear orphaned replies", "session", sid, "err", err)
	}
	if len(ids) > 0 {
		n.changed("messages")
	}
}

// expireSessions handles the sessions that went without SessionEnd: their TTL
// passed with no heartbeat (saveLocked drops them, also after a restart).
// Their orphaned replies are reported like at SessionEnd (OrphanReplies);
// their leases end in leaseSweep.
func (n *Node) expireSessions(now time.Time) {
	r := n.sess
	r.mu.Lock()
	for _, s := range r.sessions {
		if !s.live(now) {
			if err := r.saveLocked(now); err != nil {
				n.log.Warn("save sessions", "err", err)
			}
			break
		}
	}
	// One registered again since it was dropped is not gone.
	gone := slices.DeleteFunc(r.gone, func(id string) bool { s := r.sessions[id]; return s != nil && s.live(now) })
	r.gone = nil
	r.mu.Unlock()
	for _, id := range gone {
		n.log.Info("session expired without SessionEnd", "session", id)
		n.sessionChanged(id, false)
	}
	if len(gone) > 0 {
		n.presenceChanged()
		n.changed("sessions")
	}
}

// OrphanReplies hands back the requests session sid read and never answered,
// once it is gone for good (reopenStranded, goneFor: ended, or unseen for
// sessionGoneAfter; one that only expired may come back and go on with them),
// and marks the unread replies that wait for session sid, which
// ended or expired, as needing a person (chatRecord.Orphaned: the app, the
// unread API and the chat list show it) and tells their authors
// (AttemptNeedsHuman; a peer's, this node's seats have none to tell). No
// other session takes them by itself (routeOf): the owner reads them or hands
// them to a live session (Reassign). Nothing happens while sid is live here.
// It runs under claimMu, like Reassign: a reply handed over meanwhile is not
// marked, and only the replies it marks are reported.
func (n *Node) OrphanReplies(sid string) { n.orphanReplies(sid, time.Now()) }

// orphanReplies is OrphanReplies as of now.
func (n *Node) orphanReplies(sid string, now time.Time) {
	r := n.sess
	r.claimMu.Lock()
	r.mu.Lock()
	delete(r.elsewhere, sid) // reconciled as live nowhere
	r.mu.Unlock()
	if r.liveIDs(now)[sid] {
		r.claimMu.Unlock()
		return
	}
	var ids []string
	for _, u := range n.chats.unread() {
		if u.rec.Message.Kind == "" && n.replyTarget(u.rec) == sid {
			ids = append(ids, u.rec.Message.ID)
		}
	}
	marked, err := n.chats.markOrphaned(ids, sid)
	// A request it read and took but never answered goes back to the node's
	// sessions (a seat's session answers in its turns, never here).
	var reopened []string
	var rerr error
	if n.seatOfSession(sid) == "" {
		away := r.absence(sid)
		gone := func(readAt time.Time) bool { return away.goneFor(readAt, now) }
		if reopened, rerr = n.chats.reopenStranded(sid, n.cfg.Node, now.Add(-strandedMax), gone); len(reopened) > 0 {
			rerr = errors.Join(rerr, n.leases.reopen(reopened, "assignee_gone", now))
		}
	}
	r.claimMu.Unlock()
	if err = errors.Join(err, rerr); err != nil {
		n.log.Warn("mark orphaned messages", "session", sid, "err", err)
	}
	if len(reopened) > 0 {
		n.log.Warn("requests read by a session that is gone were never answered; unread again", "session", sid, "messages", len(reopened))
	}
	if len(marked) > 0 {
		n.log.Warn("replies wait for a session that is gone; a person decides", "session", sid, "messages", len(marked))
		n.report(marked, AttemptNeedsHuman)
	}
	if len(marked)+len(reopened) > 0 {
		n.changed("messages")
	}
}

// The orphan sweep's pace and reach.
const (
	// orphanSweepEvery is how often the wake loop looks for messages that wait
	// for a session that is gone (orphanSweep); a reply that arrives asks for
	// one at once (Node.orphanDue).
	orphanSweepEvery = 30 * time.Second
	// strandedMax: a request read longer ago than this is not handed back
	// (its session's end is old news; nothing replays the history).
	strandedMax = 24 * time.Hour
	// strandedGrace: the sweep leaves a request read this recently alone (its
	// reader may register a moment later); a session's end hands it back at
	// once, one that only expired after sessionGoneAfter.
	strandedGrace = time.Minute
)

// orphanSweep finds what waits for a session that is not live here, whenever
// that came about: an unread reply whose asker is gone (it arrived after the
// session ended, when OrphanReplies had nothing to mark) and a request a
// session read and took but never answered, once that session is gone for
// good (goneFor). Each such session goes through sessionChanged as ended,
// which decides by its liveness in every context (OrphanReplies, or
// ReclaimReplies when it lives elsewhere: that one the sweep then leaves
// until its next reconciliation, sessionRegistry.elsewhere).
func (n *Node) orphanSweep(now time.Time) {
	r := n.sess
	live := r.liveIDs(now)
	r.mu.Lock()
	elsewhere := maps.Clone(r.elsewhere)
	r.mu.Unlock()
	var gone []string
	note := func(sid string) {
		if sid != "" && !live[sid] && !elsewhere[sid] && !slices.Contains(gone, sid) {
			gone = append(gone, sid)
		}
	}
	for _, u := range n.chats.unread() {
		if u.rec.Message.Kind == "" && u.rec.Orphaned == "" {
			note(n.replyTarget(u.rec))
		}
	}
	for sid, readAt := range n.chats.stranded(n.cfg.Node, now.Add(-strandedMax), now.Add(-strandedGrace)) {
		if n.seatOfSession(sid) == "" && r.absence(sid).goneFor(readAt, now) {
			note(sid)
		}
	}
	for _, sid := range gone {
		n.sessionChangedAt(sid, false, now)
	}
}

// replyTarget is the session unread message rec waits for, live or not: the
// one it was reassigned to, else the one assigned to answer it, else the one
// that wrote the message it replies to; "" for none in particular.
func (n *Node) replyTarget(rec chatRecord) string {
	if rec.ReassignedTo != "" {
		return rec.ReassignedTo
	}
	if s := assignedSession(rec); s != "" {
		return s
	}
	return n.originSession(rec.Message)
}

// ErrNotOrphaned: Reassign without Force of a message that does not need a
// person (its session lives): it would take a live session's reply away.
var ErrNotOrphaned = errors.New("message does not need a person (its session is live); pass force to move it anyway")

// ReassignRequest is the body of POST /reassign: hand unread message ID to
// live session SessionID. Without Force only a message that needs a person
// (ChatMessage.NeedsHuman) moves.
type ReassignRequest struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	Force     bool   `json:"force,omitempty"`
}

// Reassign is ReassignTo for req.SessionID, a live session of this node.
func (n *Node) Reassign(req ReassignRequest) (ChatMessage, error) {
	if !validSessionID(req.SessionID) {
		return ChatMessage{}, fmt.Errorf("%w: invalid session_id", ErrBadRequest)
	}
	s, ok := n.SessionLive(req.SessionID)
	if !ok {
		return ChatMessage{}, fmt.Errorf("%w %s", ErrUnknownSession, req.SessionID)
	}
	// Checked again under claimMu: a session that ends or expires after that
	// finds the reply routed to it (OrphanReplies runs under claimMu too).
	return n.reassignTo(req.ID, s, req.Force, true)
}

// ReassignTo hands unread chat message id (a reply orphaned by its session's
// end, ChatMessage.NeedsHuman; any unread one with force) to live session
// target, which the caller found live here or in another context of this
// process. Its folder must read the message's area (unreadFor), else the
// message would never reach it (ErrBadRequest). The message then waits for
// that session alone (routeOf, like a reply for its asker), no other claim
// holds it, its lease starts over, and the session's hooks or a wake deliver
// it; its Claim and Ack go by (project, id, session) as for any message.
// Only the owner or an agent asking explicitly moves a reply: nothing
// reroutes one by itself. The caller keeps target's liveness from changing
// unseen until it returns (the app's session lock), or reconciles after.
func (n *Node) ReassignTo(id string, target Session, force bool) (ChatMessage, error) {
	return n.reassignTo(id, target, force, false)
}

// reassignTo is ReassignTo; local checks under claimMu that target is still
// a live session of this node.
func (n *Node) reassignTo(id string, target Session, force, local bool) (ChatMessage, error) {
	switch {
	case !validID(id):
		return ChatMessage{}, fmt.Errorf("%w: invalid id", ErrBadRequest)
	case !validSessionID(target.SessionID):
		return ChatMessage{}, fmt.Errorf("%w: invalid session_id", ErrBadRequest)
	}
	rec, ok := n.chats.message(id)
	if !ok {
		return ChatMessage{}, fmt.Errorf("%w %s", ErrUnknownMessage, id)
	}
	c, _ := n.chats.get(rec.Message.ChatID)
	if area, ok := n.FolderArea(target.Folder); !ok || area != n.localArea(c.Area) {
		return ChatMessage{}, fmt.Errorf("%w: session %s works in %s, which does not read the chat of message %s",
			ErrBadRequest, target.SessionID, target.Folder, id)
	}
	r := n.sess
	r.claimMu.Lock()
	if local && !r.liveIDs(time.Now())[target.SessionID] {
		r.claimMu.Unlock()
		return ChatMessage{}, fmt.Errorf("%w %s", ErrUnknownSession, target.SessionID)
	}
	rec, err := n.chats.reassign(id, target.SessionID, force)
	if err == nil {
		delete(r.claims, id) // the old route's claim or wake no longer holds it
	}
	r.claimMu.Unlock()
	if err != nil {
		return ChatMessage{}, err
	}
	// The owner decided: the message's lease (failed, or passed over for the
	// gone session) starts over for its new session.
	if err := n.leases.forget(id); err != nil {
		n.log.Warn("save leases", "err", err)
	}
	n.log.Info("message reassigned", "id", id, "session", target.SessionID, "force", force)
	n.changed("messages")
	n.changed("leases")
	return n.chatMessage(c, rec), nil
}

// wroteInChat reports whether session sid wrote one of this node's messages
// in chat id (chatRecord.Session): the origin of the replies there.
func (n *Node) wroteInChat(id, sid string) bool {
	snap, ok := n.chats.snapshot(id)
	if !ok || !validSessionID(sid) {
		return false
	}
	return slices.ContainsFunc(snap.msgs, func(r chatRecord) bool { return r.Message.From == n.cfg.Node && r.Session == sid })
}

// Sessions lists the live sessions, oldest first, primary ones marked.
func (n *Node) Sessions() []Session {
	r := n.sess
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []Session{}
	for _, s := range r.sessions {
		if s.live(now) {
			out = append(out, r.withPrimaryLocked(*s, now))
		}
	}
	slices.SortFunc(out, func(a, b Session) int {
		if c := a.RegisteredAt.Compare(b.RegisteredAt); c != 0 {
			return c
		}
		return strings.Compare(a.SessionID, b.SessionID)
	})
	return out
}

// withPrimaryLocked marks s primary when no other live session of its area is
// older. The caller holds r.mu.
func (r *sessionRegistry) withPrimaryLocked(s Session, now time.Time) Session {
	s.Primary = true
	_, s.Inbox = r.inbox[s.SessionID]
	for _, o := range r.sessions {
		if o.SessionID != s.SessionID && o.Area == s.Area && o.live(now) &&
			(o.RegisteredAt.Before(s.RegisteredAt) || (o.RegisteredAt.Equal(s.RegisteredAt) && o.SessionID < s.SessionID)) {
			s.Primary = false
			break
		}
	}
	return s
}

// LiveSession reports whether a live session is registered for the session
// area of a message of area (localArea).
func (n *Node) LiveSession(area string) bool {
	want := n.localArea(area)
	r := n.sess
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.sessions {
		if s.Area == want && s.live(now) {
			return true
		}
	}
	return false
}

// ActivityRequest is the body of POST /chats/{id}/activity: what a live
// session is doing for a request of the chat, shown to every participant like
// the worker's activity.
type ActivityRequest struct {
	SessionID string `json:"session_id"`
	// ReplyTo is the request worked on; empty means the chat's newest message
	// from another node.
	ReplyTo string `json:"reply_to,omitempty"`
	ID      string `json:"id,omitempty"`   // ties the start and the end of one operation
	Type    string `json:"type,omitempty"` // "thinking", "edit", "command", "read", "search", "tool"
	Text    string `json:"text,omitempty"`
	// Phase is PhaseRunning (default), PhaseDone (the operation ended, the
	// session still works) or PhaseIdle (the session finished its turn: the
	// activity ends).
	Phase   string `json:"phase,omitempty"`
	AgentID string `json:"agent_id,omitempty"`
	Label   string `json:"label,omitempty"`
	// Tool and Input (a tool call's name and input), or Event (ActThinking,
	// ...), stand for Type and Text when Text is empty: the node words the
	// activity itself (ToolActivity, EventActivity), paths relative to the
	// session's folder. The input is read for a path and a program name only.
	Tool  string          `json:"tool,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	Event string          `json:"event,omitempty"`
}

// PhaseIdle ends a live session's activity on a request (ActivityRequest).
const PhaseIdle = "idle"

// SessionActivity posts a live session's activity to chat id as a status
// update of the request it works on (JobRunning with ActivityInfo; PhaseIdle
// sends JobCompleted, which ends it). It refreshes the session like a
// heartbeat.
func (n *Node) SessionActivity(chatID string, req ActivityRequest) (Message, error) {
	c, ok := n.chats.get(chatID)
	if !ok {
		return Message{}, fmt.Errorf("%w %s", ErrUnknownChat, chatID)
	}
	switch req.Phase {
	case "":
		req.Phase = PhaseRunning
	case PhaseRunning, PhaseDone, PhaseIdle:
	default:
		return Message{}, fmt.Errorf("%w: invalid phase %q", ErrBadRequest, req.Phase)
	}
	if req.Text == "" && (req.Tool != "" || req.Event != "") {
		if len(req.Tool) > 128 {
			return Message{}, fmt.Errorf("%w: activity too long", ErrBadRequest)
		}
		if req.Event != "" {
			typ, text, ok := EventActivity(req.Event)
			if !ok {
				return Message{}, fmt.Errorf("%w: invalid event %q", ErrBadRequest, req.Event)
			}
			req.Type, req.Text = typ, text
		} else {
			folder := ""
			if s, ok := n.SessionLive(req.SessionID); ok {
				folder = s.Folder
			}
			req.Type, req.Text = ToolActivity(folder, req.Tool, req.Input)
		}
	}
	if len(req.Text) > 500 || len(req.Type) > 32 || len(req.ID) > 128 || len(req.Label) > 128 ||
		(req.AgentID != "" && !validSessionID(req.AgentID)) {
		return Message{}, fmt.Errorf("%w: activity too long", ErrBadRequest)
	}
	r := n.sess
	now := time.Now().UTC()
	r.mu.Lock()
	// A session registered in another context of this process reports on the
	// replies to its own messages here (routeOf): it asked in this chat. It is
	// not tracked here (no registration to refresh, no activity to re-send).
	s := r.sessions[req.SessionID]
	tracked := s != nil && s.live(now)
	if !tracked {
		r.mu.Unlock()
		if !n.wroteInChat(c.ID, req.SessionID) {
			return Message{}, fmt.Errorf("%w %s", ErrUnknownSession, req.SessionID)
		}
		r.mu.Lock()
	} else {
		s.LastSeen, s.LastActive = now, now
		if s.Woken && req.AgentID == "" && req.Phase != PhaseIdle {
			// It works on a request, so it took a turn since the node's last
			// wake though no hook event said so (hooks that do not fire leave it
			// Idle and woken for good): the wakes of that idle period are over,
			// and what came since may wake it again. The end of an activity
			// (PhaseIdle) is no work: it may be that of a turn before the wake.
			s.Woken, s.Wakes, s.WokeAt, s.WakeCapped, s.CapNoted = false, 0, time.Time{}, false, nil
			if err := r.saveLocked(now); err != nil {
				n.log.Warn("save sessions", "err", err)
			}
		}
	}
	prev, had := r.last[req.SessionID]
	a := ActivityState{ID: req.ID, Type: req.Type, Text: req.Text, Phase: req.Phase, StartedAt: now, Seq: uint64(now.UnixNano()),
		Session: ShortSession(req.SessionID), Role: "main"}
	if req.AgentID != "" {
		a.Session = a.Session + ":" + req.AgentID // older peers key jobs by Session alone
		a.AgentID, a.ParentSession, a.Role, a.Label = req.AgentID, req.SessionID, "subagent", req.Label
		had = false
	}
	if had && prev.ID == a.ID && prev.Type == a.Type && (prev.Text == a.Text || a.Phase == PhaseDone) {
		a.StartedAt = prev.StartedAt
	}
	if req.AgentID == "" && tracked {
		r.last[req.SessionID] = a
	}
	r.mu.Unlock()
	if req.ReplyTo == "" {
		if snap, ok := n.chats.snapshot(c.ID); ok {
			for _, rec := range slices.Backward(snap.msgs) {
				if m := rec.Message; m.Kind == "" && m.From != n.cfg.Node {
					req.ReplyTo = m.ID
					break
				}
			}
		}
	}
	if !validID(req.ReplyTo) {
		return Message{}, fmt.Errorf("%w: no request to report on (reply_to)", ErrBadRequest)
	}
	if req.AgentID == "" && a.Phase == PhaseIdle {
		if snap, ok := n.chats.snapshot(c.ID); ok {
			for _, child := range snap.jobs {
				x := child.ActivityInfo
				if child.From == n.cfg.Node && child.JobStatus == JobRunning && x != nil && x.ParentSession == req.SessionID && x.AgentID != "" {
					if _, err := n.SessionActivity(chatID, ActivityRequest{SessionID: req.SessionID, ReplyTo: child.ReplyTo,
						AgentID: x.AgentID, Label: x.Label, Phase: PhaseIdle}); err != nil {
						n.log.Warn("end subagent activity", "session", req.SessionID, "agent", x.AgentID, "err", err)
					}
				}
			}
		}
	}
	if req.AgentID == "" && tracked {
		r.mu.Lock()
		on := r.on[req.SessionID]
		switch {
		case a.Phase == PhaseIdle:
			delete(r.last, req.SessionID)
			delete(on, c.ID)
		case on == nil:
			r.on[req.SessionID] = map[string]string{c.ID: req.ReplyTo}
		default:
			on[c.ID] = req.ReplyTo
		}
		r.mu.Unlock()
	}
	return n.SendMessage(sessionStatus(n.cfg.Node, c.ID, req.SessionID, req.ReplyTo, a))
}

// sessionStatus is the status message of activity a of session sid on request
// replyTo of chat chatID.
func sessionStatus(node, chatID, sid, replyTo string, a ActivityState) Message {
	idLabel := node + "/session/" + sid + "/"
	if a.AgentID != "" {
		idLabel += a.AgentID + "/"
	}
	m := Message{ID: DerivedID(replyTo, idLabel+strconv.FormatUint(a.Seq, 10)),
		ChatID: chatID, ReplyTo: replyTo, Kind: KindStatus, JobStatus: JobRunning, Activity: a.Text}
	if a.Phase == PhaseIdle {
		// The end names the session too: it ends that session's line only.
		m.JobStatus, m.Activity = JobCompleted, ""
		m.ActivityInfo = &ActivityState{Phase: PhaseIdle, Seq: a.Seq, Session: a.Session, AgentID: a.AgentID, ParentSession: a.ParentSession, Role: a.Role, Label: a.Label}
	} else {
		m.ActivityInfo = &a
	}
	return m
}

// ActivityRefresh: a busy live session's latest activity is sent again this
// long after it was last reported, so a long tool call (no hook event for its
// whole run) keeps its line on every participant (ActivityExpire).
const ActivityRefresh = 2 * time.Minute

// refreshActivity re-sends the latest activity of every live, busy session
// not heard from for ActivityRefresh, with a new Seq and the same text and
// start. It does not count as the session's heartbeat: a session that stops
// its heartbeats ends (TTL) and its line with it.
func (n *Node) refreshActivity(now time.Time) {
	r := n.sess
	var out []Message
	r.mu.Lock()
	for sid, on := range r.on {
		s, a, ok := r.sessions[sid], r.last[sid], false
		if s != nil && s.live(now) && !s.Idle && a.Phase != PhaseIdle && a.Seq != 0 {
			ok = true
		}
		if !ok || len(on) == 0 {
			delete(r.on, sid)
			continue
		}
		if a.Seq > uint64(now.Add(-ActivityRefresh).UnixNano()) { // reported within ActivityRefresh
			continue
		}
		a.Seq = uint64(now.UnixNano())
		r.last[sid] = a
		for chat, replyTo := range on {
			out = append(out, sessionStatus(n.cfg.Node, chat, sid, replyTo, a))
		}
	}
	r.mu.Unlock()
	for _, m := range out {
		if _, err := n.SendMessage(m); err != nil {
			n.log.Debug("refreshing a session's activity failed", "chat", m.ChatID, "err", err)
		}
	}
}

// activityLoop runs refreshActivity and expireSessions until ctx is done.
func (n *Node) activityLoop(ctx context.Context) {
	t := time.NewTicker(ActivityRefresh / 4)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			n.expireSessions(now)
			n.refreshActivity(now)
		}
	}
}

// ShortSession is the short name of a session id its activity carries: its
// first 8 characters.
func ShortSession(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
