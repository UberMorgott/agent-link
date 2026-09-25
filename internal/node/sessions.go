package node

import (
	"context"
	"errors"
	"fmt"
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
	// Primary: the oldest live session of its area, the one that answers.
	Primary bool `json:"primary"`
	// Asked is the wake mode the session asked for; Wake is what the node
	// gives it (WakeQueue only while it can queue, else WakeNextEvent).
	Asked     string `json:"asked_wake,omitempty"`
	Idle      bool   `json:"idle,omitempty"`
	CodexHome string `json:"codex_home,omitempty"`
	// Woken: the node queued a wake in this idle period; Wakes of them (at
	// most maxIdleWakes), the last at WokeAt. A wake the session never took
	// is retried once its claim lapses (wakeIdle).
	Woken  bool      `json:"woken,omitempty"`
	Wakes  int       `json:"wakes,omitempty"`
	WokeAt time.Time `json:"woke_at,omitzero"`
	// Inbox: the node holds a usable inbox of the session (InboxSocket) and
	// wakes it there when idle. Not persisted: the address lives in memory.
	Inbox bool `json:"inbox,omitempty"`
}

func (s Session) live(now time.Time) bool {
	return now.Sub(s.LastSeen) < time.Duration(s.TTLSec)*time.Second
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
	claims  map[string]sessionClaim

	// inbox is each live Claude session's inbox address (memory only, never
	// persisted or sent); recent is the last session seen per area, kept in
	// last_sessions.json, which a launch resumes (launch.go).
	inbox      map[string]inboxAddr
	recent     map[string]LastSession
	recentPath string
}

// LastSession is the latest agent session registered for an area: a launch
// with no live session there resumes it.
type LastSession struct {
	SessionID string    `json:"session_id"`
	Provider  string    `json:"provider"`
	Folder    string    `json:"folder"`
	At        time.Time `json:"at"`
}

func openSessions(dir string) (*sessionRegistry, error) {
	r := &sessionRegistry{path: filepath.Join(dir, "sessions.json"), sessions: map[string]*Session{}, last: map[string]ActivityState{},
		on: map[string]map[string]string{}, claims: map[string]sessionClaim{}, inbox: map[string]inboxAddr{},
		recent: map[string]LastSession{}, recentPath: filepath.Join(dir, "last_sessions.json")}
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
	return r, nil
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
	now := time.Now().UTC()
	r := n.sess
	r.mu.Lock()
	s := r.sessions[req.SessionID]
	fresh := s == nil || !s.live(now) || s.Wake != req.Wake || s.Area != area
	if s == nil || !s.live(now) {
		if len(r.sessions) >= maxSessions {
			_ = r.saveLocked(now) // drops the dead ones
		}
		if len(r.sessions) >= maxSessions {
			r.mu.Unlock()
			return Session{}, fmt.Errorf("%w: too many sessions", ErrBadRequest)
		}
		s = &Session{SessionID: req.SessionID, RegisteredAt: now}
		r.sessions[req.SessionID] = s
	}
	s.Provider, s.Folder, s.Area, s.Wake, s.TTLSec, s.LastSeen = req.Provider, filepath.Clean(req.Folder), area, req.Wake, ttl, now
	// A new idle period (or none) may be woken again.
	s.Woken = s.Woken && s.Idle && req.Idle
	if !s.Woken {
		s.Wakes, s.WokeAt = 0, time.Time{}
	}
	s.Asked, s.Idle, s.CodexHome = asked, req.Idle, req.CodexHome
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
	err := r.saveLocked(time.Now())
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w %s", ErrUnknownSession, id)
	}
	n.presenceChanged()
	n.changed("sessions")
	return err
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
	if len(req.Text) > 500 || len(req.Type) > 32 || len(req.ID) > 128 || len(req.Label) > 128 ||
		(req.AgentID != "" && !validSessionID(req.AgentID)) {
		return Message{}, fmt.Errorf("%w: activity too long", ErrBadRequest)
	}
	r := n.sess
	now := time.Now().UTC()
	r.mu.Lock()
	s := r.sessions[req.SessionID]
	if s == nil || !s.live(now) {
		r.mu.Unlock()
		return Message{}, fmt.Errorf("%w %s", ErrUnknownSession, req.SessionID)
	}
	s.LastSeen = now
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
	if req.AgentID == "" {
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
	if req.AgentID == "" {
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

// activityLoop runs refreshActivity until ctx is done.
func (n *Node) activityLoop(ctx context.Context) {
	t := time.NewTicker(ActivityRefresh / 4)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
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
