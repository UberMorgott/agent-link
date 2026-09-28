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

// Local agent chats of discuss (settings.LocalChat). A folder's project chat
// is the persistent default. Beside it a caller names a topic (a persistent
// chat of the project, or of no project outside project folders) or starts a
// temporary chat; a session outside any project folder gets its own temporary
// chat by default. Each such chat is a local binding of its own: one chat,
// its own seats, never on the network and never picked by folder routing.

// Chat scopes (LocalChatView.Scope).
const (
	ChatScopeProject             = "project"
	ChatScopeProjectTemporary    = "project_temporary"
	ChatScopeFolderless          = "folderless"
	ChatScopeFolderlessTemporary = "folderless_temporary"
)

// TempChatIdle is how long a temporary chat stays after its last activity
// once none of its sessions is live and nothing in it is pending.
const TempChatIdle = 24 * time.Hour

// localChatGCEvery is how often temporary chats are collected.
const localChatGCEvery = 10 * time.Minute

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
}

// localChatViewOf is lc's view; nil is a folder's project chat.
func localChatViewOf(lc *settings.LocalChat) LocalChatView {
	if lc == nil {
		return LocalChatView{Scope: ChatScopeProject}
	}
	v := LocalChatView{Topic: lc.Topic, Project: lc.Project, Folder: lc.Folder}
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
	if lc.Temporary {
		v.ExpiresAt = lc.LastUsed.Add(TempChatIdle)
	}
	return v
}

// discussContextLocked picks (or makes) the local context of a discuss
// request: the chat it names, else its topic's or a new temporary chat, else
// the folder's project chat, else, outside any project folder, the session's
// temporary chat.
func (a *App) discussContextLocked(ctx context.Context, req discussRequest, dir string) (string, error) {
	if req.Chat != "" {
		for i, b := range a.s.Bindings {
			if c := a.projects[b.ID]; c != nil && b.ScopeOf() == settings.ProjectScopeLocal && c.n.OwnsChat(req.Chat) {
				return b.ID, a.touchLocalChatLocked(i, req.SessionID)
			}
		}
		return "", errUnknownLocalChat
	}
	if req.Seat != "" && req.Topic == "" && !req.Temporary {
		// A seat asks in its own chat, wherever its folder is.
		for _, b := range a.s.Bindings {
			if c := a.projects[b.ID]; c != nil && slices.ContainsFunc(c.n.Seats(), func(s node.SeatView) bool { return s.ID == req.Seat }) {
				return b.ID, nil
			}
		}
	}
	pid := a.localProjectOfLocked(dir)
	folderless := pid == "" && !a.projectFolderLocked(dir) && !inWorkTree(dir)
	if pid == "" && !folderless {
		if settings.ProjectCount(a.s.Bindings) >= settings.MaxProjects {
			return "", &settings.Problem{Key: "too_many_projects"}
		}
		var err error
		if pid, err = a.addLocalLocked(ctx, dir, nil, filepath.Base(dir)); err != nil {
			return "", err
		}
	}
	if req.Topic == "" && !req.Temporary && !folderless {
		return pid, nil
	}
	if !req.Temporary {
		best := -1
		for i, b := range a.s.Bindings {
			lc := b.Chat
			if lc == nil || lc.Project != pid || a.projects[b.ID] == nil {
				continue
			}
			mine := lc.Topic != "" && strings.EqualFold(lc.Topic, req.Topic) ||
				req.Topic == "" && lc.Temporary && req.SessionID != "" && len(lc.Sessions) > 0 && lc.Sessions[0] == req.SessionID
			if mine && (best < 0 || lc.LastUsed.After(a.s.Bindings[best].Chat.LastUsed)) {
				best = i
			}
		}
		if best >= 0 {
			return a.s.Bindings[best].ID, a.touchLocalChatLocked(best, req.SessionID)
		}
	}
	if len(a.s.Bindings)-settings.ProjectCount(a.s.Bindings) >= settings.MaxLocalChats {
		return "", &settings.Problem{Key: "too_many_projects"}
	}
	lc := &settings.LocalChat{Temporary: req.Topic == "", Topic: req.Topic, Project: pid, Folder: dir, LastUsed: time.Now().UTC()}
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
	return a.addLocalLocked(ctx, "", lc, name)
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

// inWorkTree reports whether dir is inside a git work tree: a project folder
// even before any project binds it.
func inWorkTree(dir string) bool {
	for d := filepath.Clean(dir); ; {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return true
		}
		up := filepath.Dir(d)
		if up == d {
			return false
		}
		d = up
	}
}

// touchLocalChatLocked records a use of the local chat of binding i by
// session ("" none); a folder's project binding is left as it is.
func (a *App) touchLocalChatLocked(i int, session string) error {
	if a.s.Bindings[i].Chat == nil {
		return nil
	}
	lc := *a.s.Bindings[i].Chat
	lc.LastUsed = time.Now().UTC()
	if session != "" && !slices.Contains(lc.Sessions, session) {
		lc.Sessions = append(slices.Clone(lc.Sessions), session)
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

// gcLoop collects idle temporary chats until ctx ends.
func (a *App) gcLoop(ctx context.Context) {
	t := time.NewTicker(localChatGCEvery)
	defer t.Stop()
	for {
		a.gcLocalChats(time.Now())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// gcLocalChats removes every temporary chat none of whose sessions is live,
// with nothing pending (a seat's queue or turn, an unread message, a running
// job) and no activity for TempChatIdle. Its data goes to .left, as a leave's.
// An unread reply whose session ended (ChatInfo.NeedsHuman) keeps the chat
// until a person reads it or reassigns it: it is never dropped unseen.
func (a *App) gcLocalChats(now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	live := map[string]bool{}
	for _, c := range a.projects {
		for _, s := range c.n.Sessions() {
			live[s.SessionID] = true
		}
	}
	if a.legacy != nil {
		for _, s := range a.legacy.n.Sessions() {
			live[s.SessionID] = true
		}
	}
	var drop []string
	for _, b := range a.s.Bindings {
		if b.Chat == nil || !b.Chat.Temporary || slices.ContainsFunc(b.Chat.Sessions, func(s string) bool { return live[s] }) {
			continue
		}
		if last, busy := localChatActivity(a.projects[b.ID], b.Chat); !busy && now.Sub(last) >= TempChatIdle {
			drop = append(drop, b.ID)
		}
	}
	for _, pid := range drop {
		if err := a.leaveProjectLocked(pid); err != nil {
			a.log.Warn("remove temporary chat", "project", pid, "err", err)
			continue
		}
		a.log.Info("temporary chat removed", "project", pid)
		go a.events.publish("projects", projectTopic(pid), "status", "dashboard")
	}
}

// localChatActivity is the last activity of local chat lc run by c (nil: not
// running) and whether something in it is pending.
func localChatActivity(c *appContext, lc *settings.LocalChat) (last time.Time, busy bool) {
	last = lc.LastUsed
	if c == nil {
		return last, false
	}
	chats, err := c.n.Chats(false, false)
	if err != nil {
		return last, true
	}
	for _, ci := range chats {
		if ci.LastAt.After(last) {
			last = ci.LastAt
		}
		busy = busy || ci.Unread > 0 || ci.Active
	}
	for _, s := range c.n.Seats() {
		busy = busy || len(s.Pending) > 0 || s.Status == node.SeatRunning
	}
	return last, busy || (c.w != nil && c.w.Busy())
}
