package app

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/UberMorgott/agent-link/internal/node"
	"github.com/UberMorgott/agent-link/internal/settings"
)

// discussRequest is the body of POST /discuss.
type discussRequest struct {
	Folder    string `json:"folder"`
	Provider  string `json:"provider"`
	Body      string `json:"body"`
	SessionID string `json:"session_id"`
	Source    string `json:"source"`
	Seat      string `json:"seat"`
	// AgentID and AgentType: the subagent of the session that asks
	// (node.SendRequest.AgentID); its hooks take the reply while it lives.
	AgentID   string `json:"agent_id"`
	AgentType string `json:"agent_type"`
	// Chat continues a local chat by id; Topic names a persistent chat;
	// Temporary starts a new temporary chat. At most one of them.
	Chat      string `json:"chat"`
	Topic     string `json:"topic"`
	Temporary bool   `json:"temporary"`
}

// discuss posts to this machine's private agent chat for the caller: by
// default the folder's project chat (a network project bound to the same
// folder has a separate one), outside any project folder the caller session's
// temporary chat, else the chat the request names (localchats.go).
func (a *App) discuss(w http.ResponseWriter, r *http.Request) {
	var req discussRequest
	if !decode(w, r, &req) {
		return
	}
	req.Topic = strings.TrimSpace(req.Topic)
	dir, err := filepath.Abs(strings.TrimSpace(req.Folder))
	picks := 0
	for _, set := range []bool{req.Chat != "", req.Topic != "", req.Temporary} {
		if set {
			picks++
		}
	}
	if err != nil || !filepath.IsAbs(req.Folder) || strings.TrimSpace(req.Body) == "" ||
		(req.Provider != node.ProviderClaude && req.Provider != node.ProviderCodex) ||
		(req.Source != "" && req.Source != node.ProviderClaude && req.Source != node.ProviderCodex) ||
		(req.SessionID == "") != (req.Source == "") || picks > 1 || !settings.ValidAlias(req.Topic) {
		writeCodedError(w, http.StatusBadRequest, "bad_request")
		return
	}
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		writeCodedError(w, http.StatusBadRequest, "dir")
		return
	}
	a.mu.Lock()
	pid, err := a.discussContextLocked(r.Context(), req, dir)
	c := a.projects[pid]
	var lc *settings.LocalChat
	if i := a.bindingIndex(pid); i >= 0 {
		lc = a.s.Bindings[i].Chat
	}
	a.mu.Unlock()
	if errors.Is(err, errUnknownLocalChat) {
		writeCodedError(w, http.StatusNotFound, "not_found")
		return
	}
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
	seat, added := "", false
	for _, s := range c.n.Seats() {
		if s.Provider == req.Provider {
			seat = s.ID
			break
		}
	}
	if seat == "" {
		// The new seat starts only after the message is queued for it, so its
		// first turn (with the introduction) carries the message.
		view, addErr := c.n.AddSeat(node.SeatRequest{Provider: req.Provider, Defer: true}) //nolint:contextcheck // the seat turn outlives the request and runs under the node's own context
		if addErr != nil {
			a.mu.Unlock()
			a.failed(w, "discuss agent", addErr)
			return
		}
		seat, added = view.ID, true
	}
	a.mu.Unlock()
	// An external interactive session keeps its folder hook in the network
	// project when one exists. The message records the session (its origin):
	// the seat's reply is unread for it alone, and its hooks take it from here
	// also after the direct wait ends (controlUnread). AgentLink-launched
	// seats use their explicit project selector in hooks.
	message, err := c.n.SendRequest(node.SendRequest{ChatID: chat.ID, Body: req.Body, Folder: dir,
		SessionID: req.SessionID, AgentID: req.AgentID, AgentType: req.AgentType, Seat: req.Seat, AskSeats: []string{seat}, AuthorKind: authorKind(req.SessionID, req.Seat)})
	if added {
		if _, startErr := c.n.StartSeat(seat, false); startErr != nil { //nolint:contextcheck // the seat turn outlives the request and runs under the node's own context
			a.log.Warn("start discuss seat", "seat", seat, "err", startErr)
		}
	}
	if err != nil {
		a.failed(w, "discuss message", err)
		return
	}
	a.changed(pid)
	view := localChatViewOf(lc)
	writeJSON(w, struct {
		Project string `json:"project"`
		Chat    string `json:"chat"`
		ID      string `json:"id"`
		Seat    string `json:"seat"`
		Queued  bool   `json:"queued,omitempty"`
		// Scope is the chat's kind (LocalChatView.Scope); Topic and ExpiresAt
		// as in LocalChatView.
		Scope     string    `json:"scope"`
		Topic     string    `json:"topic,omitempty"`
		ExpiresAt time.Time `json:"expires_at,omitzero"`
		// HoldReason: no seat answers it automatically (node.HoldAutoLimit):
		// the caller returns at once instead of waiting for a reply.
		HoldReason string `json:"hold_reason,omitempty"`
	}{pid, chat.ID, message.ID, seat, discussQueued(c.n, seat), view.Scope, view.Topic, view.ExpiresAt, message.HoldReason})
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
	// client_ack=1: the caller acknowledges the reply itself once it decoded
	// it (discussMessage). Without it (callers before v0.6.30, whose MCP
	// servers outlive an update) the reply is read here as they expect.
	clientAck := q.Get("client_ack") == "1"
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
			a.discussReplied(w, c.n, chat, reply, clientAck)
			return
		}
		if discussQueued(c.n, seat) || c.n.AutoHeld(ask.Message) { // held: no turn runs for it
			w.WriteHeader(http.StatusAccepted)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-timer.C:
			if reply, found, err := discussAnswer(c.n, chat, id, seat, &cursor); err == nil && found {
				a.discussReplied(w, c.n, chat, reply, clientAck)
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

// discussReplied answers the waiting discuss call with reply. A caller that
// acknowledges replies itself gets it unread, so a reply it never decoded
// stays for the session's hooks; a legacy caller has it read here, or its
// hooks would deliver it a second time.
func (a *App) discussReplied(w http.ResponseWriter, n *node.Node, chat string, reply node.ChatMessage, clientAck bool) {
	if !clientAck && reply.Unread {
		if _, err := n.Ack(chat, node.AckRequest{IDs: []string{reply.ID}}); err != nil {
			a.log.Warn("read discuss reply", "id", reply.ID, "err", err)
		}
	}
	writeJSON(w, reply.Message)
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

func discussAnswer(n *node.Node, chat, id, seat string, cursor *uint64) (node.ChatMessage, bool, error) {
	for {
		page, err := n.ChatMessages(chat, 0, *cursor, 1000)
		if err != nil {
			return node.ChatMessage{}, false, err
		}
		for _, m := range page {
			*cursor = m.Seq
			if m.Kind == "" && m.ReplyTo == id && m.Agent != nil && m.Agent.Seat == seat {
				return m, true, nil
			}
		}
		if len(page) < 1000 {
			return node.ChatMessage{}, false, nil
		}
	}
}
