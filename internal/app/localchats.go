package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/UberMorgott/agent-link/internal/config"
	"github.com/UberMorgott/agent-link/internal/node"
	"github.com/UberMorgott/agent-link/internal/settings"
)

// Local agent chats of discuss (settings.LocalChat). An agent session asks in
// its own chat by default (owned: settings.LocalChat.Owner), one per
// (project, owner, topic), so its thread with the asked agent continues and
// no other session shares it; a person, or an agent passing shared, asks in
// the folder's project chat or a topic's persistent chat of the project. A
// caller may also start a temporary chat. Each such chat is a local binding of
// its own: one chat, its own seats, never on the network and never picked by
// folder routing.

// Chat scopes (LocalChatView.Scope).
const (
	ChatScopeProject             = "project"
	ChatScopeProjectTemporary    = "project_temporary"
	ChatScopeFolderless          = "folderless"
	ChatScopeFolderlessTemporary = "folderless_temporary"
)

const (
	// TempChatIdle is how long a temporary chat stays after its last activity
	// once none of its sessions is live and nothing in it is pending.
	TempChatIdle = 24 * time.Hour
	// EmptyChatGrace is how long a temporary chat with no seat and no message
	// stays after its last use, even while its owner lives: nothing in it
	// is worth keeping.
	EmptyChatGrace = 2 * time.Minute
	// ChatLiveGrace is how long a local chat stays shown after it stopped
	// being live (LocalChatView.LiveEndedAt); the project list is told again
	// when it lapses.
	ChatLiveGrace = 60 * time.Second
)

var errUnknownLocalChat = errors.New("unknown local chat")

// LocalChatView is a local chat as the API shows it (ProjectView, discuss).
type LocalChatView struct {
	Scope   string `json:"scope"`
	Topic   string `json:"topic,omitempty"`
	Project string `json:"project,omitempty"` // the folder's local project; "" outside projects
	Folder  string `json:"folder,omitempty"`  // where its agents work
	// ExpiresAt is, for a temporary chat, the earliest time it is removed: it
	// stays longer while one of its sessions is live or something is pending.
	ExpiresAt time.Time `json:"expires_at,omitzero"`
	// Owner is the agent the chat belongs to (settings.LocalChat.Owner);
	// absent for a shared chat.
	Owner *settings.LocalChatOwner `json:"owner,omitempty"`

	// The state below is set in the project list (localChatViewLocked), not in
	// a discuss answer.

	// Live: the chat is in use (localChatLiveLocked); a retired one never is.
	Live bool `json:"live"`
	// LiveEndedAt is when it stopped being live (absent while live): the page
	// keeps it shown for ChatLiveGrace from then.
	LiveEndedAt time.Time `json:"live_ended_at,omitzero"`
	// Retired: its owner ended and only an unread reply holds it (retire.go);
	// hidden from the sidebar, the dashboard's needs_human shows the reply.
	Retired bool `json:"retired,omitempty"`
	// Waiting: a discuss caller waits for a reply, or an asked agent's turn
	// runs or is queued (SeatView.TurnQueued).
	Waiting bool `json:"waiting,omitempty"`
	// LastActive is the chat's last message or use.
	LastActive time.Time `json:"last_active,omitzero"`
}

// LocalActivityView is whether a folder's local project chat is in use
// (localChatLiveLocked: a caller waits, a turn or job runs) and its last
// message or use (ProjectView.Activity).
type LocalActivityView struct {
	Live        bool      `json:"live"`
	LiveEndedAt time.Time `json:"live_ended_at,omitzero"` // as in LocalChatView
	Waiting     bool      `json:"waiting,omitempty"`
	LastActive  time.Time `json:"last_active,omitzero"`
}

// localChatViewLocked is the view of the local chat of binding b with its
// live state now; nil for a folder's project binding.
func (a *App) localChatViewLocked(b settings.ProjectBinding) *LocalChatView {
	if b.Chat == nil {
		return nil
	}
	v := localChatViewOf(b.Chat)
	st := a.localChatLiveLocked(b)
	v.Live, v.LiveEndedAt, v.Waiting, v.LastActive = st.live, st.endedAt, st.waiting, st.last
	if b.Chat.Temporary {
		v.ExpiresAt = st.last.Add(TempChatIdle)
	}
	return &v
}

// localActivityLocked is the activity of a folder's local project chat.
func (a *App) localActivityLocked(b settings.ProjectBinding) *LocalActivityView {
	st := a.localChatLiveLocked(b)
	return &LocalActivityView{Live: st.live, LiveEndedAt: st.endedAt, Waiting: st.waiting, LastActive: st.last}
}

// discussWaiting counts a discuss caller starting (+1) or ending (-1) its
// wait for a reply in binding pid; the project list shows the change.
func (a *App) discussWaiting(pid string, delta int) {
	a.mu.Lock()
	if a.discussWaiters == nil {
		a.discussWaiters = map[string]int{}
	}
	a.discussWaiters[pid] += delta
	if a.discussWaiters[pid] <= 0 {
		delete(a.discussWaiters, pid)
	}
	a.mu.Unlock()
	a.changed(pid)
	a.liveChanged()
}

// localChatViewOf is lc's view; nil is a folder's project chat.
func localChatViewOf(lc *settings.LocalChat) LocalChatView {
	if lc == nil {
		return LocalChatView{Scope: ChatScopeProject}
	}
	v := LocalChatView{Topic: lc.Topic, Project: lc.Project, Folder: lc.Folder, Owner: chatOwner(lc), Retired: !lc.Retired.IsZero()}
	switch {
	case lc.Temporary && lc.Project != "":
		v.Scope = ChatScopeProjectTemporary
	case lc.Temporary:
		v.Scope = ChatScopeFolderlessTemporary
	case lc.Project != "":
		v.Scope = ChatScopeProject
	default:
		v.Scope = ChatScopeFolderless
	}
	return v

}

// discussContextLocked picks (or makes) the local context of a discuss
// request: the chat it names, else a new temporary chat, else an agent's own
// chat of its topic (discussOwner), else the topic's shared chat, else the
// folder's project chat. created reports that pid is a binding this call
// added (discuss removes it again when the call fails and it stayed empty).
func (a *App) discussContextLocked(ctx context.Context, req discussRequest, dir string) (pid string, created bool, err error) {
	if req.Chat != "" {
		for i, b := range a.s.Bindings {
			if c := a.projects[b.ID]; c != nil && b.ScopeOf() == settings.ProjectScopeLocal && c.n.OwnsChat(req.Chat) {
				if a.retiring[b.ID] {
					return "", false, errUnknownLocalChat // closing: its seats go now
				}
				if b.Chat != nil && !b.Chat.Retired.IsZero() {
					return b.ID, false, a.adoptRetiredLocked(i, discussOwner(req))
				}
				return b.ID, false, a.touchLocalChatLocked(i, req.SessionID)
			}
		}
		return "", false, errUnknownLocalChat
	}
	if req.Seat != "" && req.Topic == "" && !req.Temporary {
		// A seat asks in its own chat, wherever its folder is.
		for _, b := range a.s.Bindings {
			if c := a.projects[b.ID]; c != nil && slices.ContainsFunc(c.n.Seats(), func(s node.SeatView) bool { return s.ID == req.Seat }) {
				return b.ID, false, nil
			}
		}
	}
	pid = a.localProjectOfLocked(dir)
	root := gitRoot(dir)
	folderless := pid == "" && !a.projectFolderLocked(dir) && root == ""
	if pid == "" && !folderless {
		// A new local project binds the whole work tree, whatever subfolder
		// asks first; a project folder outside git binds itself.
		if root == "" {
			root = dir
		}
		if err := settings.CanAddBinding(a.s.Bindings, false); err != nil {
			return "", false, err
		}
		if pid, err = a.addLocalLocked(ctx, root, nil, filepath.Base(root)); err != nil {
			return "", false, err
		}
		created = true
	}
	owner := discussOwner(req)
	if owner == nil && req.Topic == "" && !req.Temporary && !folderless {
		return pid, created, nil
	}
	if !req.Temporary {
		// The routing key: (project, owner, topic) for an agent's own chat,
		// (project, topic) for a shared one.
		best := -1
		for i, b := range a.s.Bindings {
			lc := b.Chat
			if lc == nil || lc.Project != pid || a.projects[b.ID] == nil || !strings.EqualFold(lc.Topic, req.Topic) ||
				!lc.Retired.IsZero() || a.retiring[b.ID] {
				continue
			}
			var mine bool
			if owner != nil {
				o := chatOwner(lc)
				mine = o != nil && sameOwner(*o, *owner)
			} else {
				mine = lc.Topic != "" && lc.Owner == nil
			}
			if mine && (best < 0 || lc.LastUsed.After(a.s.Bindings[best].Chat.LastUsed)) {
				best = i
			}
		}
		if best >= 0 {
			return a.s.Bindings[best].ID, false, a.touchLocalChatLocked(best, req.SessionID)
		}
	}
	if err := settings.CanAddBinding(a.s.Bindings, true); err != nil {
		return pid, created, err
	}
	// A temporary chat of no owner says so, for an older build that takes
	// its first session for one (settings.LocalChat.OwnerOf).
	lc := &settings.LocalChat{Temporary: req.Topic == "" || owner != nil, Topic: req.Topic, Owner: owner, Project: pid, Folder: dir,
		LastUsed: time.Now().UTC(), Unowned: owner == nil && req.Topic == ""}
	if pid != "" {
		lc.Folder = a.bindingDirLocked(pid)
	}
	if req.SessionID != "" {
		lc.Sessions = []string{req.SessionID}
	}
	name := req.Topic
	if name == "" {
		name = "Temporary " + filepath.Base(lc.Folder)
	}
	pid, err = a.addLocalLocked(ctx, "", lc, name)
	return pid, err == nil, err
}

// discussOwner is the agent a discuss request's own chat belongs to: its
// session, or the subagent of it that asks (AgentID: each subagent owns its
// chats and threads; with no agent id the session's own); nil for a person, a
// seat (asking in its own chat) or a request for the project's shared chat.
func discussOwner(req discussRequest) *settings.LocalChatOwner {
	if req.SessionID == "" || req.Seat != "" || req.Shared {
		return nil
	}
	o := &settings.LocalChatOwner{Session: req.SessionID, Provider: req.Source, Agent: req.AgentID}
	if o.Agent != "" {
		o.AgentType = req.AgentType
	}
	return o
}

// sameOwner reports whether a and b are one agent: the same session and
// subagent.
func sameOwner(a, b settings.LocalChatOwner) bool {
	return a.Session == b.Session && a.Agent == b.Agent
}

// addLocalLocked adds a local binding: the project of folder dir, or local
// chat lc (dir ""). name is its shared name when valid.
func (a *App) addLocalLocked(ctx context.Context, dir string, lc *settings.LocalChat, name string) (string, error) {
	pid, err := config.NewProjectID()
	if err != nil {
		return "", err
	}
	secret, err := config.NewProjectSecret()
	if err != nil {
		return "", err
	}
	if name, err = node.NormalizeProjectName(name); err != nil {
		name = "Project " + pid[:8]
	}
	return pid, a.addProjectLocked(ctx, settings.ProjectBinding{ID: pid, Epoch: config.ProjectEpoch, Secret: secret,
		Dir: dir, Scope: settings.ProjectScopeLocal, Chat: lc}, name)
}

// localProjectOfLocked is the deepest local project holding folder dir, or "".
func (a *App) localProjectOfLocked(dir string) string {
	var pid string
	for _, b := range a.s.Bindings {
		if b.ScopeOf() == settings.ProjectScopeLocal && b.Dir != "" && within(b.Dir, dir) &&
			(pid == "" || len(b.Dir) > len(a.bindingDirLocked(pid))) {
			pid = b.ID
		}
	}
	return pid
}

// projectFolderLocked reports whether a project (of any scope) holds dir.
func (a *App) projectFolderLocked(dir string) bool {
	return slices.ContainsFunc(a.s.Bindings, func(b settings.ProjectBinding) bool { return b.Dir != "" && within(b.Dir, dir) })
}

// gitRoot is the top folder of the git work tree holding dir (where its .git
// is), "" outside one: a project folder even before any project binds it.
func gitRoot(dir string) string {
	for d := filepath.Clean(dir); ; {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		up := filepath.Dir(d)
		if up == d {
			return ""
		}
		d = up
	}
}

// maxChatSessions bounds LocalChat.Sessions: the first (the creator) and the
// latest ones are kept.
const maxChatSessions = 16

// touchLocalChatLocked records a new session ("" none) using the local chat
// of binding i; a folder's project binding is left as it is. A known session
// writes nothing: the chat's activity is its messages (localChatState).
func (a *App) touchLocalChatLocked(i int, session string) error {
	if a.s.Bindings[i].Chat == nil || session == "" || slices.Contains(a.s.Bindings[i].Chat.Sessions, session) {
		return nil
	}
	lc := *a.s.Bindings[i].Chat
	lc.LastUsed = time.Now().UTC()
	lc.Sessions = append(slices.Clone(lc.Sessions), session)
	if len(lc.Sessions) > maxChatSessions {
		lc.Sessions = slices.Delete(lc.Sessions, 1, len(lc.Sessions)-maxChatSessions+1)
	}
	s := a.s
	s.Bindings = slices.Clone(s.Bindings)
	s.Bindings[i].Chat = &lc
	if err := settings.Save(a.path, s); err != nil {
		return err
	}
	a.s = s
	return nil
}

// adoptRetiredLocked brings back retired local chat i, continued by id: it
// becomes owner's (nil: an unowned chat, left to the idle GC), and what was
// seen of its old owner is forgotten.
func (a *App) adoptRetiredLocked(i int, owner *settings.LocalChatOwner) error {
	lc := *a.s.Bindings[i].Chat
	lc.Retired, lc.Owner, lc.LastUsed = time.Time{}, owner, time.Now().UTC()
	switch {
	case owner != nil:
		lc.Temporary, lc.Unowned = true, false
		if !slices.Contains(lc.Sessions, owner.Session) {
			lc.Sessions = append(slices.Clone(lc.Sessions), owner.Session)
		}
	case lc.Topic != "":
		lc.Temporary, lc.Unowned = false, false // a person's topic chat is shared
	default:
		lc.Temporary, lc.Unowned = true, true
	}
	s := a.s
	s.Bindings = slices.Clone(s.Bindings)
	s.Bindings[i].Chat = &lc
	if err := settings.Save(a.path, s); err != nil {
		return err
	}
	a.s = s
	a.forgetOwnerLocked(s.Bindings[i].ID)
	return nil
}

// gcLoop retires the chats of ended owners (retireOwnedChats), collects
// idle and empty local chats (gcLocalChats) and tells the project list of
// chats whose live state changed unseen (checkLive), every retireEvery and
// once at start, until ctx ends.
func (a *App) gcLoop(ctx context.Context) {
	t := time.NewTicker(retireEvery)
	defer t.Stop()
	for {
		now := time.Now()
		a.retireOwnedChats(now)
		a.gcLocalChats(now)
		a.checkLive(now)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// gcLocalChats removes, with nothing pending in it (a seat's queue or turn,
// an unread message, a running job, a discuss caller waiting):
//   - every retired chat: it stayed for an unread reply alone (retire.go);
//   - every temporary chat with no seat and no message EmptyChatGrace after
//     its last use, even while its owner lives (a discuss that failed, or
//     one whose seats went);
//   - every temporary chat none of whose sessions is live after TempChatIdle
//     without activity.
//
// Its data goes to .left, as a leave's. An unread reply whose session ended
// (ChatInfo.NeedsHuman) keeps the chat until a person reads it or reassigns
// it: it is never dropped unseen.
func (a *App) gcLocalChats(now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var live map[string]map[string]bool
	var drop []string
	for _, b := range a.s.Bindings {
		lc := b.Chat
		if lc == nil || !lc.Temporary && lc.Retired.IsZero() || a.retiring[b.ID] || a.discussWaiters[b.ID] > 0 {
			continue
		}
		c := a.projects[b.ID]
		st := localChatState(c, lc)
		if st.waiting || st.running || st.unread {
			continue
		}
		switch {
		case !lc.Retired.IsZero():
		case c != nil && st.empty && now.Sub(st.last) >= EmptyChatGrace:
		case now.Sub(st.last) >= TempChatIdle:
			if live == nil {
				live = a.liveAgentsLocked()
			}
			if slices.ContainsFunc(lc.Sessions, func(s string) bool { _, ok := live[s]; return ok }) {
				continue
			}
		default:
			continue
		}
		drop = append(drop, b.ID)
	}
	for _, pid := range drop {
		if err := a.leaveProjectLocked(pid); err != nil {
			a.log.Warn("remove local chat", "project", pid, "err", err)
			continue
		}
		a.forgetOwnerLocked(pid)
		delete(a.chatLive, pid)
		a.log.Info("local chat removed", "project", pid)
		go a.events.publish("projects", projectTopic(pid), "status", "dashboard")
	}
}

// chatState is what a local chat's context shows now (localChatState).
type chatState struct {
	last time.Time // its last message or use
	// waiting: an asked seat's turn runs or is queued.
	waiting bool
	// running: other work runs in it (an active chat, a job).
	running bool
	unread  bool // an unread message
	empty   bool // no seat and no message
}

// localChatState is the state of local chat lc run by c (nil: not running;
// nothing is known of it). An unreadable chat list counts as running and
// unread: never collect or hide what cannot be read. lc is nil for a
// folder's project chat.
func localChatState(c *appContext, lc *settings.LocalChat) chatState {
	var st chatState
	if lc != nil {
		st.last = lc.LastUsed
	}
	if c == nil {
		return st
	}
	seats := c.n.Seats()
	for _, s := range seats {
		st.waiting = st.waiting || seatWaits(s)
	}
	chats, err := c.n.Chats(false, false)
	if err != nil {
		st.running, st.unread = true, true
		return st
	}
	st.empty = len(seats) == 0
	for _, ci := range chats {
		if ci.LastAt.After(st.last) {
			st.last = ci.LastAt
		}
		st.running = st.running || ci.Active
		st.unread = st.unread || ci.Unread > 0
		st.empty = st.empty && ci.Count == 0
	}
	st.running = st.running || c.w != nil && c.w.Busy()
	return st
}

// seatWaits reports whether seat s has work that runs by itself: a turn
// running or queued, or messages a turn will take. Messages whose turns failed
// or that wait past the hop limit (needs_human, paused) wait for a person, so
// they keep no chat live.
func seatWaits(s node.SeatView) bool {
	switch {
	case s.Status == node.SeatRunning || s.TurnQueued:
		return true
	case s.Status == node.SeatNeedsHuman || s.Status == node.SeatPaused:
		return false
	}
	return len(s.Pending) > 0
}

// liveMark is the live state of a local binding last seen by checkLive.
type liveMark struct {
	live    bool
	endedAt time.Time // when it was seen to stop being live
}

// liveChanged asks for a checkLive soon (a node's seats, chats or jobs
// changed); calls while one is pending share it.
func (a *App) liveChanged() {
	if a.liveCheck.CompareAndSwap(false, true) {
		go func() {
			a.liveCheck.Store(false)
			a.checkLive(time.Now())
		}()
	}
}

// checkLive notes the live state of every local binding and tells the
// project list when one changed: a chat stopping to be live records when
// (LiveEndedAt), and the list is told again once its ChatLiveGrace lapsed.
func (a *App) checkLive(now time.Time) {
	a.mu.Lock()
	flipped := a.noteLiveLocked(now)
	a.mu.Unlock()
	if flipped {
		a.events.publish("projects")
	}
}

// noteLiveLocked is checkLive's note of every local binding; it reports
// whether one changed.
func (a *App) noteLiveLocked(now time.Time) bool {
	flipped := false
	seen := map[string]bool{}
	for _, b := range a.s.Bindings {
		if b.ScopeOf() != settings.ProjectScopeLocal {
			continue
		}
		seen[b.ID] = true
		live := a.localChatLiveLocked(b).live
		m, known := a.chatLive[b.ID]
		switch {
		case !known:
		case m.live && !live:
			m.endedAt = now.UTC()
			time.AfterFunc(ChatLiveGrace, func() { a.events.publish("projects") })
			flipped = true
		case !m.live && live:
			m.endedAt = time.Time{}
			flipped = true
		}
		m.live = live
		if a.chatLive == nil {
			a.chatLive = map[string]liveMark{}
		}
		a.chatLive[b.ID] = m
	}
	for pid := range a.chatLive {
		if !seen[pid] {
			delete(a.chatLive, pid)
		}
	}
	return flipped
}

// dropIfEmpty removes local binding pid when nothing is in it (no seat, no
// message, nothing pending, no discuss caller waiting): what a failed discuss
// created. A concurrent discuss that routed into it meanwhile keeps it.
func (a *App) dropIfEmpty(pid string) {
	a.mu.Lock()
	i := a.bindingIndex(pid)
	drop := i >= 0 && a.discussWaiters[pid] == 0 && !a.retiring[pid]
	if drop {
		c := a.projects[pid]
		st := localChatState(c, a.s.Bindings[i].Chat)
		drop = c != nil && st.empty && !st.waiting && !st.running && !st.unread
	}
	if drop {
		if err := a.leaveProjectLocked(pid); err != nil {
			a.log.Warn("remove empty local binding", "project", pid, "err", err)
			drop = false
		} else {
			a.forgetOwnerLocked(pid)
			delete(a.chatLive, pid)
		}
	}
	a.mu.Unlock()
	if drop {
		a.events.publish("projects", projectTopic(pid), "status", "dashboard")
	}
}

// chatOwner is the agent local chat lc belongs to (settings.LocalChat.Owner);
// nil for a shared chat or none.
func chatOwner(lc *settings.LocalChat) *settings.LocalChatOwner {
	if lc == nil {
		return nil
	}
	return lc.Owner
}

// migrateChatOwners makes the owner of every temporary chat of a build
// before owners were stored explicit: the session that made it
// (settings.LocalChat.OwnerOf, which reads Unowned and Sessions[0]). changed
// reports any.
func migrateChatOwners(s settings.Settings) (out settings.Settings, changed bool) {
	for i, b := range s.Bindings {
		if b.Chat == nil || b.Chat.Owner != nil {
			continue
		}
		o := b.Chat.OwnerOf()
		if o == nil {
			continue
		}
		if !changed {
			s.Bindings = slices.Clone(s.Bindings)
		}
		lc := *b.Chat
		lc.Owner, changed = o, true
		s.Bindings[i].Chat = &lc
	}
	return s, changed
}
