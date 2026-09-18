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
