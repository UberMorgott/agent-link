package app

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/UberMorgott/agent-link/internal/config"
	"github.com/UberMorgott/agent-link/internal/node"
	"github.com/UberMorgott/agent-link/internal/settings"
)

// Errors of the member buttons, besides settings and node errors.
var (
	// ErrNotConfigured: members are added after the settings were saved once.
	ErrNotConfigured = errors.New("settings not saved yet")
	// ErrNotRunning: the node is not running (no code yet, or a start failure).
	ErrNotRunning = errors.New("node is not running")
)

// AddMember keeps addr (IP or host, port optional) in the settings and dials
// it now: the member at that address joins the table and every other member
// learns and dials it. It also brings back a member someone removed.
func (a *App) AddMember(addr string) error {
	err := a.addMember(addr)
	if err == nil {
		a.events.publish("settings", "members")
	}
	return err
}

func (a *App) addMember(addr string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.configured {
		return ErrNotConfigured
	}
	norm, err := config.WithDefaultPort(strings.TrimSpace(addr))
	if err != nil {
		return &settings.Problem{Key: "peer_addr"}
	}
	s := a.s.WithPeer(norm)
	if err := settings.Save(a.path, s); err != nil {
		return err
	}
	a.s = s
	if a.n != nil {
		return a.n.AddPeer(norm)
	}
	return nil
}

// RemoveMember removes name from the whole network (a tombstone every member
// applies) and drops its addresses from the settings.
func (a *App) RemoveMember(name string) error {
	err := a.removeMember(name)
	if err == nil {
		a.events.publish("settings", "members")
	}
	return err
}

func (a *App) removeMember(name string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.n == nil {
		return ErrNotRunning
	}
	var addrs []string
	for _, m := range a.n.Members() {
		if m.Name == name {
			addrs = m.Addrs
		}
	}
	if err := a.n.RemoveMember(name); err != nil {
		return err
	}
	s := a.s
	s.Peers = slices.DeleteFunc(slices.Clone(s.Peers), func(p config.Peer) bool {
		return p.Name == name || slices.Contains(addrs, p.Addr)
	})
	if len(s.Peers) == len(a.s.Peers) {
		return nil
	}
	if len(s.Peers) == 0 {
		s.Peers = nil
	}
	if err := settings.Save(a.path, s); err != nil {
		return err
	}
	a.s = s
	return nil
}

// memberAction serves POST /ui/api/members/add {"addr"} and /members/remove {"name"}.
func (a *App) memberAction(do func(req node.MemberRequest) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req node.MemberRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, msg("error.bad_request", nil))
			return
		}
		if err := do(req); err != nil {
			writeError(w, http.StatusBadRequest, memberError(err))
			return
		}
		writeJSON(w, a.Status())
	}
}

func memberError(err error) string {
	var p *settings.Problem
	switch {
	case errors.As(err, &p):
		return msg("error."+p.Key, nil)
	case errors.Is(err, ErrNotConfigured):
		return msg("error.not_configured", nil)
	case errors.Is(err, ErrNotRunning):
		return msg("error.not_running", nil)
	case errors.Is(err, node.ErrSelf):
		return msg("error.remove_self", nil)
	case errors.Is(err, node.ErrUnknownPeer):
		return msg("error.unknown_member", nil)
	default:
		return msg("error.save", nil)
	}
}

// SetProfile sets how this member shows itself in every project: its
// nickname (node.ValidDisplay; its own name or "" for none) and its chat color
// (node.ChatColors, "" the derived one). Both are saved, then put in its
// member record in every running context, which carries them to the other
// members live. The name itself never changes: it stays the member's identity,
// and every earlier nickname keeps resolving to it (settings NicknameAliases).
// A nickname another member of any project has as name or nickname is
// refused (nickname_taken).
func (a *App) SetProfile(nickname, color string) error {
	if color != "" && !node.ValidChatColor(color) {
		return &settings.Problem{Key: "chat_color"}
	}
	a.mu.Lock()
	if nickname == a.s.Node {
		nickname = ""
	}
	if nickname != "" && !node.ValidDisplay(nickname) {
		a.mu.Unlock()
		return &settings.Problem{Key: "nickname"}
	}
	ctxs := a.contextsLocked()
	for _, c := range ctxs {
		if nickname != "" && !strings.EqualFold(nickname, a.s.Nickname) && c.n.NameTaken(nickname) {
			a.mu.Unlock()
			return &settings.Problem{Key: "nickname_taken"}
		}
	}
	s := a.s
	if s.Nickname != nickname && s.Nickname != "" {
		s.NicknameAliases = append(slices.DeleteFunc(slices.Clone(s.NicknameAliases), func(x string) bool { return x == s.Nickname }), s.Nickname)
	}
	s.NicknameAliases = slices.DeleteFunc(s.NicknameAliases, func(x string) bool { return x == nickname })
	if len(s.NicknameAliases) > 8 {
		s.NicknameAliases = s.NicknameAliases[len(s.NicknameAliases)-8:]
	}
	s.Nickname, s.ChatColor = nickname, color
	if a.configured {
		if err := settings.Save(a.path, s); err != nil {
			a.mu.Unlock()
			return err
		}
	}
	a.s = s
	a.mu.Unlock()
	for _, c := range ctxs {
		c.n.SetChatColor(color)
		c.n.SetDisplay(nickname, s.NicknameAliases)
	}
	a.events.publish("status", "projects", "members")
	return nil
}

// contextsLocked lists the running contexts, the legacy network first. The
// caller holds a.mu.
func (a *App) contextsLocked() []*appContext {
	var ctxs []*appContext
	if a.legacy != nil {
		ctxs = append(ctxs, a.legacy)
	}
	for _, c := range a.projects {
		ctxs = append(ctxs, c)
	}
	return ctxs
}

// setProfile serves POST /ui/api/profile {"nickname", "color"} -> Status; a
// bad or taken nickname answers 400 with the sentence to show.
func (a *App) setProfile(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Nickname string `json:"nickname"`
		Color    string `json:"color"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, msg("error.bad_request", nil))
		return
	}
	var p *settings.Problem
	switch err := a.SetProfile(strings.TrimSpace(req.Nickname), req.Color); {
	case errors.As(err, &p) && (p.Key == "nickname" || p.Key == "nickname_taken"):
		writeError(w, http.StatusBadRequest, msg("error."+p.Key, nil))
		return
	case errors.As(err, &p):
		writeError(w, http.StatusBadRequest, msg("error.bad_request", nil))
		return
	case err != nil:
		a.log.Error("save profile", "err", err)
		writeError(w, http.StatusInternalServerError, msg("error.save", nil))
		return
	}
	writeJSON(w, a.Status())
}
