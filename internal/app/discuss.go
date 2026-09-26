package app

import (
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/UberMorgott/agent-link/internal/config"
	"github.com/UberMorgott/agent-link/internal/node"
	"github.com/UberMorgott/agent-link/internal/settings"
)

// discuss posts to this machine's private agent chat for the caller's folder.
// A network project bound to the same folder has a separate chat.
func (a *App) discuss(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Folder    string `json:"folder"`
		Provider  string `json:"provider"`
		Body      string `json:"body"`
		SessionID string `json:"session_id"`
		Source    string `json:"source"`
		Seat      string `json:"seat"`
	}
	if !decode(w, r, &req) {
		return
	}
	dir, err := filepath.Abs(strings.TrimSpace(req.Folder))
	if err != nil || !filepath.IsAbs(req.Folder) || strings.TrimSpace(req.Body) == "" ||
		(req.Provider != node.ProviderClaude && req.Provider != node.ProviderCodex) ||
		(req.Source != "" && req.Source != node.ProviderClaude && req.Source != node.ProviderCodex) ||
		(req.SessionID == "") != (req.Source == "") {
		writeCodedError(w, http.StatusBadRequest, "bad_request")
		return
	}
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		writeCodedError(w, http.StatusBadRequest, "dir")
		return
	}
	a.mu.Lock()
	var pid string
	for _, b := range a.s.Bindings {
		if b.ScopeOf() == settings.ProjectScopeLocal && b.Dir != "" && within(b.Dir, dir) &&
			(pid == "" || len(b.Dir) > len(a.bindingDirLocked(pid))) {
			pid = b.ID
		}
	}
	if pid == "" {
		if len(a.s.Bindings) >= settings.MaxProjects {
			a.mu.Unlock()
			writeCodedError(w, http.StatusBadRequest, "too_many_projects")
			return
		}
		pid, err = config.NewProjectID()
		if err == nil {
			var secret string
			secret, err = config.NewProjectSecret()
			if err == nil {
				name, nameErr := node.NormalizeProjectName(filepath.Base(dir))
				if nameErr != nil {
					name = "Project " + pid[:8]
				}
				err = a.addProjectLocked(r.Context(), settings.ProjectBinding{ID: pid, Epoch: config.ProjectEpoch, Secret: secret,
					Dir: dir, Scope: settings.ProjectScopeLocal}, name)
			}
		}
	}
	c := a.projects[pid]
	a.mu.Unlock()
	if err != nil {
		a.failed(w, "discuss project", err)
		return
	}
	if c == nil {
		writeCodedError(w, http.StatusInternalServerError, "not_found")
		return
	}
	chat, err := c.n.NewProjectChat(nil)
	if err != nil {
		a.failed(w, "discuss chat", err)
		return
	}
	// Serialize the check and addition across concurrent discuss requests.
	a.mu.Lock()
	seat := ""
	for _, s := range c.n.Seats() {
		if s.Provider == req.Provider {
			seat = s.ID
			break
		}
	}
	if seat == "" {
		added, addErr := c.n.AddSeat(node.SeatRequest{Provider: req.Provider}) //nolint:contextcheck // the seat turn outlives the request and runs under the node's own context
		if addErr != nil {
			a.mu.Unlock()
			a.failed(w, "discuss agent", addErr)
			return
		}
		seat = added.ID
	}
	a.mu.Unlock()
	// An external interactive session keeps its folder hook in the network
	// project when one exists. The direct discuss reply serves this call;
	// AgentLink-launched seats use their explicit project selector in hooks.
	message, err := c.n.SendRequest(node.SendRequest{ChatID: chat.ID, Body: req.Body, Folder: dir,
		SessionID: req.SessionID, Seat: req.Seat, AskSeats: []string{seat}, AuthorKind: authorKind(req.SessionID, req.Seat)})
	if err != nil {
		a.failed(w, "discuss message", err)
		return
	}
	a.changed(pid)
	writeJSON(w, struct {
		Project string `json:"project"`
		Chat    string `json:"chat"`
		ID      string `json:"id"`
		Seat    string `json:"seat"`
		Queued  bool   `json:"queued,omitempty"`
	}{pid, chat.ID, message.ID, seat, discussQueued(c.n, seat)})
}

func authorKind(session, seat string) string {
	if session == "" && seat == "" {
		return node.AuthorHuman
	}
	return node.AuthorAgent
}

func (a *App) bindingDirLocked(pid string) string {
	for _, b := range a.s.Bindings {
		if b.ID == pid {
			return b.Dir
		}
	}
	return ""
}

// discussReply waits for the asked seat's direct answer to one discuss message.
// A subscription is installed before reading history, so a reply cannot fall
// into a gap between the history check and the next event.
func (a *App) discussReply(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	pid, chat, id, seat := q.Get("project"), q.Get("chat"), q.Get("id"), q.Get("seat")
	timeout, err := time.ParseDuration(q.Get("timeout"))
	if pid == "" || chat == "" || id == "" || seat == "" || err != nil || timeout <= 0 || timeout > 15*time.Minute {
		http.Error(w, "invalid discuss reply request", http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	c := a.projects[pid]
	a.mu.Unlock()
	if c == nil || !c.n.OwnsChat(chat) {
		http.Error(w, "unknown discuss project or chat", http.StatusNotFound)
		return
	}
	ask, ok, err := discussAsk(c.n, chat, id)
	if err != nil {
		a.failed(w, "discuss history", err)
		return
	}
	if !ok || ask.Kind != "" || !slices.Contains(ask.AskSeats, seat) {
		http.Error(w, "unknown discuss request or seat", http.StatusNotFound)
		return
	}
	sub := a.events.subscribe()
	defer sub.close()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	cursor := ask.Seq
	for {
		// A settings save may restart the project node while this request waits.
		// Read the current node, whose chat history is persisted across restarts.
		a.mu.Lock()
		current := a.projects[pid]
		a.mu.Unlock()
		if current == nil {
			http.Error(w, "discuss project was removed", http.StatusGone)
			return
		}
		c = current
		reply, found, scanErr := discussAnswer(c.n, chat, id, seat, &cursor)
		if scanErr != nil {
			a.failed(w, "discuss reply", scanErr)
			return
		}
		if found {
			writeJSON(w, reply)
			return
		}
		if discussQueued(c.n, seat) {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-timer.C:
			if reply, found, err := discussAnswer(c.n, chat, id, seat, &cursor); err == nil && found {
				writeJSON(w, reply)
				return
			} else if err != nil {
				a.failed(w, "discuss reply", err)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		case <-sub.wake:
			sub.snapshot()
		}
	}
}

func discussQueued(n *node.Node, seat string) bool {
	if n.Stopped() || n.AutonomyStatus().Paused {
		return true
	}
	for _, s := range n.Seats() {
		if s.ID == seat {
			return s.Stopped
		}
	}
	return true // the selected seat was removed while this request was in flight
}

func discussAsk(n *node.Node, chat, id string) (node.ChatMessage, bool, error) {
	var before uint64
	for {
		page, err := n.ChatMessages(chat, before, 0, 1000)
		if err != nil {
			return node.ChatMessage{}, false, err
		}
		for _, m := range page {
			if m.ID == id {
				return m, true, nil
			}
		}
		if len(page) < 1000 || page[0].Seq <= 1 {
			return node.ChatMessage{}, false, nil
		}
		before = page[0].Seq
	}
}

func discussAnswer(n *node.Node, chat, id, seat string, cursor *uint64) (node.Message, bool, error) {
	for {
		page, err := n.ChatMessages(chat, 0, *cursor, 1000)
		if err != nil {
			return node.Message{}, false, err
		}
		for _, m := range page {
			*cursor = m.Seq
			if m.Kind == "" && m.ReplyTo == id && m.Agent != nil && m.Agent.Seat == seat {
				return m.Message, true, nil
			}
		}
		if len(page) < 1000 {
			return node.Message{}, false, nil
		}
	}
}
