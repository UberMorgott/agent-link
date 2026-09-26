package node

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/UberMorgott/agent-link/internal/config"
)

// SendRequest is the body of POST /send. With ChatID the message goes to that
// chat (To stays empty): Ask names the participants asked to answer (none:
// the message only informs) and Parent the request whose job sends it
// (AGENTLINK_JOB_ID), which continues that request's automatic chain.
//
// Area picks the project of the conversation for To (default: the area of
// Folder, the sender's working folder, when it is a project folder here).
// AuthorKind is who writes (Author*); the control API always sends
// AuthorAgent, the app's composer AuthorHuman.
type SendRequest struct {
	To         string   `json:"to"`
	Body       string   `json:"body"`
	ReplyTo    string   `json:"reply_to,omitempty"`
	ChatID     string   `json:"chat_id,omitempty"`
	Ask        []string `json:"ask,omitempty"`
	Parent     string   `json:"parent,omitempty"`
	Area       string   `json:"area,omitempty"`
	Folder     string   `json:"folder,omitempty"`
	AuthorKind string   `json:"author_kind,omitempty"`
	// SessionID is the live session of this node that sends (agentlink send
	// inside it): replies to the message are delivered to it (routeOf).
	SessionID string `json:"session_id,omitempty"`
	// Attachments are files uploaded before (POST /attachments): id and name.
	Attachments []Attachment `json:"attachments,omitempty"`
	// Files are local files to attach (agentlink send --attach): absolute, or
	// relative to Folder; only inside this node's folders or the temp folder.
	Files []string `json:"files,omitempty"`
	// AskSeats are this node's seats asked to answer (ids or labels, "all"),
	// in a chat only; Seat is the sending seat (a seat's turn run by the node,
	// AGENTLINK_SEAT), else the seat of SessionID (seats.go).
	AskSeats []string `json:"ask_seats,omitempty"`
	Seat     string   `json:"seat,omitempty"`
}

// SendRequest sends req like POST /send: every path goes to the one open chat
// of its conversation (EnsureOpenChat), a plain message only for a member
// connected without chat support or a reply to a plain message:
//
//   - ChatID: that chat's conversation (a closed or v0.5 chat continues in its
//     open generation);
//   - ReplyTo a chat message: the conversation of that message;
//   - To a member (empty: the only one): the chat of this node and it in Area,
//     asking it unless Ask says otherwise;
//   - To "area:NAME": the chat of this node and every member of the area, in
//     that area, asking them all unless Ask says otherwise.
//
// A reply (ReplyTo) that is not from the job answering that very request
// (Parent) is reported to the local reply hook: the request is answered here.
func (n *Node) SendRequest(req SendRequest) (Message, error) {
	agent, err := n.senderAgent(req)
	if err != nil {
		return Message{}, err
	}
	sender := ""
	if agent != nil {
		sender = agent.Seat
	}
	asked, err := n.resolveSeats(req.AskSeats, sender)
	if err != nil {
		return Message{}, err
	}
	if req.AuthorKind == AuthorHuman && req.ReplyTo != "" {
		// A person answering a seat's message asks that seat.
		if r, ok := n.chats.message(req.ReplyTo); ok && r.Message.From == n.cfg.Node && r.Message.Agent != nil && r.Message.Agent.Seat != "" {
			if extra, err := n.resolveSeats([]string{r.Message.Agent.Seat}, ""); err == nil {
				asked = slices.Compact(slices.Sorted(slices.Values(append(asked, extra...))))
			}
		}
	}
	atts, err := n.resolveAttachments(req)
	if err != nil {
		return Message{}, err
	}
	if len(atts) > 0 {
		req.Body = withFallback(req.Body, atts)
	}
	m, err := n.sendRequest(req, atts, agent, asked)
	var qerr error
	if err == nil && m.ChatID != "" {
		if qerr = n.deliverToSeats(m, sender); qerr != nil {
			qerr = fmt.Errorf("message %s sent, but the seats' queue was not saved (retried): %w", m.ID, qerr)
		}
	}
	if err == nil && m.ChatID != "" && validSessionID(req.SessionID) {
		if serr := n.chats.setSession(m.ID, req.SessionID); serr != nil {
			n.log.Warn("record sending session", "id", m.ID, "err", serr)
		}
	}
	if err == nil && req.ReplyTo != "" && req.Parent != req.ReplyTo && n.onLocalReply != nil {
		n.onLocalReply(req.ReplyTo)
	}
	if err == nil {
		err = qerr
	}
	return m, err
}

func (n *Node) sendRequest(req SendRequest, atts []Attachment, agent *AgentRef, asked []string) (Message, error) {
	cs := ChatSend{ChatID: req.ChatID, Body: req.Body, ReplyTo: req.ReplyTo, Ask: req.Ask, Parent: req.Parent, AuthorKind: req.AuthorKind, Attachments: atts,
		Agent: agent, AskSeats: asked}
	if req.ChatID != "" {
		if req.To != "" {
			return Message{}, errors.New("give either to or chat_id")
		}
		return n.SendChat(cs)
	}
	if req.ReplyTo != "" {
		if r, ok := n.chats.message(req.ReplyTo); ok && r.Message.Kind == "" {
			cs.ChatID = r.Message.ChatID
			return n.SendChat(cs)
		}
	}
	area := strings.TrimSpace(req.Area)
	if area == "" && req.Folder != "" {
		area, _ = n.FolderArea(req.Folder)
		area = n.localArea(area)
	}
	to := req.To
	if to == "" {
		switch peers := n.Peers(); {
		case len(peers) == 1:
			to = peers[0]
		case len(peers) > 1:
			slices.Sort(peers)
			return Message{}, fmt.Errorf("%w: %s", ErrAmbiguousPeer, strings.Join(peers, ", "))
		}
	}
	var recipients []string
	if a, ok := strings.CutPrefix(to, AreaPrefix); ok {
		area = a
		n.mu.Lock()
		for peer, areas := range n.areas {
			if slices.Contains(areas, a) && n.known[peer] && !n.removedLocked(peer) {
				recipients = append(recipients, peer)
			}
		}
		n.mu.Unlock()
	} else if to != "" {
		recipients = []string{to}
	}
	plain := len(recipients) == 0 || req.ReplyTo != "" // unknown ones fail in Send
	for _, p := range recipients {
		plain = plain || (n.Connected(p) && !n.PeerHas(p, CapChat))
	}
	if plain {
		if len(req.Ask) > 0 || len(asked) > 0 {
			return Message{}, errors.New("ask needs a chat")
		}
		if len(atts) > 0 {
			return Message{}, fmt.Errorf("%w: attachments need a chat", ErrAttachment)
		}
		return n.Send(to, req.Body, req.ReplyTo)
	}
	parts, err := n.normalizeParticipants(recipients)
	if err != nil {
		return Message{}, err
	}
	c, err := n.EnsureOpenChat(parts, area)
	if err != nil {
		return Message{}, err
	}
	cs.ChatID = c.ID
	if len(cs.Ask) == 0 && len(asked) == 0 {
		cs.Ask = recipients
	}
	return n.SendChat(cs)
}

// CreateChatRequest is the body of POST /chats.
type CreateChatRequest struct {
	Participants []string `json:"participants"` // the other members; this node is added
	Area         string   `json:"area,omitempty"`
}

// ArchiveRequest is the body of POST /chats/archive: ChatID names the chat to
// archive, "" the project's active one.
type ArchiveRequest struct {
	ChatID string `json:"chat_id,omitempty"`
}

// APIHandler serves the loopback control API (agents and the CLI; see
// docs/agent-usage.md). It has no close: only people close chats, in the app.
//
//	POST /send               SendRequest -> Message, written by an agent (AuthorAgent)
//	POST /attachments        raw body ?name=, or multipart -> Attachment (for SendRequest.Attachments)
//	GET  /attachments/{id}   the file (?name=, &download=1)
//	GET  /wait?timeout=30s   200 []Message (marked delivered) or 204 on timeout; 0 waits forever.
//	                         Requests and replies only: status updates never wake it.
//	     &chat=ID            only that chat's messages; the others stay for a later wait
//	POST /chats              CreateChatRequest -> ChatInfo (EnsureOpenChat; in a project its one active chat)
//	POST /chats/archive      ArchiveRequest (optional) -> ChatInfo: the project's fresh active chat (ArchiveChat)
//	GET  /chats?archive=1    200 []ChatInfo: the archive (default the main list); &legacy=1 adds pre-chat history
//	GET  /chats/{id}         200 ChatInfo
//	GET  /chats/{id}/messages?before=SEQ&after=SEQ&limit=50   200 []ChatMessage in Seq order
//	POST /chats/{id}/ack     AckRequest -> []AckResult (read; a read receipt to the author)
//	POST /chats/{id}/activity ActivityRequest -> Message (a live session's status update)
//	POST /ack                AckRequest -> []AckResult, chat and plain messages
//	GET  /unread?folder=PATH&after=CURSOR&limit=50   200 UnreadPage
//	     &session=ID         only what that session may take (UnreadFor)
//	     &waiter=1           a Claude waiter: empty while the node wakes that session (InboxWakes)
//	POST /claim              ClaimRequest -> []string (ids granted to the session for delivery)
//	POST /sessions           SessionRequest -> Session (register or heartbeat)
//	GET  /sessions           200 []Session (live ones)
//	DELETE /sessions/{id}    204
//	GET  /inbox?limit=50     200 []Entry, chat messages included (with chat_id)
//	GET  /members            200 []MemberInfo (this node first)
//	GET  /seats              200 []SeatView: this node's local agents (seats.go)
//	POST /members            {"addr"} -> dial that address too (AddPeer)
//	POST /members/remove     {"name"} -> remove the member from the network
func (n *Node) APIHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /send", n.handleSend)
	mux.HandleFunc("GET /wait", n.handleWait)
	mux.HandleFunc("GET /inbox", n.handleInbox)
	mux.HandleFunc("POST /chats/archive", func(w http.ResponseWriter, r *http.Request) {
		var req ArchiveRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxFrame)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		info, err := n.ArchiveChat(strings.TrimSpace(req.ChatID))
		if err != nil {
			http.Error(w, err.Error(), errorCode(err))
			return
		}
		writeJSONResponse(w, info)
	})
	n.ChatRoutes(mux, "", false, func(w http.ResponseWriter, code int, err error) { http.Error(w, err.Error(), code) })
	n.sessionRoutes(mux)
	n.AttachmentRoutes(mux, "", func(w http.ResponseWriter, code int, err error) { http.Error(w, err.Error(), code) })
	mux.HandleFunc("GET /members", func(w http.ResponseWriter, _ *http.Request) { writeJSONResponse(w, n.Members()) })
	mux.HandleFunc("GET /seats", func(w http.ResponseWriter, _ *http.Request) { writeJSONResponse(w, n.Seats()) })
	mux.HandleFunc("POST /members", n.handleAddMember)
	mux.HandleFunc("POST /members/remove", n.handleRemoveMember)
	return localOnly(mux)
}

// MemberRequest is the body of POST /members (Addr) and POST /members/remove (Name).
type MemberRequest struct {
	Addr string `json:"addr,omitempty"`
	Name string `json:"name,omitempty"`
}

func (n *Node) handleAddMember(w http.ResponseWriter, r *http.Request) {
	var req MemberRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxFrame)).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := n.AddPeer(req.Addr); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSONResponse(w, n.Members())
}

func (n *Node) handleRemoveMember(w http.ResponseWriter, r *http.Request) {
	var req MemberRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxFrame)).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := n.RemoveMember(req.Name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSONResponse(w, n.Members())
}

// localOnly rejects browser-originated requests and DNS-rebinding hosts.
func localOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if r.Header.Get("Origin") != "" || !config.IsLoopbackHost(host) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (n *Node) handleSend(w http.ResponseWriter, r *http.Request) {
	var req SendRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxFrame)).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req.AuthorKind = AuthorAgent // people write in the app
	m, err := n.SendRequest(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSONResponse(w, m)
}

func (n *Node) handleWait(w http.ResponseWriter, r *http.Request) {
	var timeout <-chan time.Time
	if s := r.URL.Query().Get("timeout"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil || d < 0 {
			http.Error(w, "invalid timeout", http.StatusBadRequest)
			return
		}
		if d > 0 {
			t := time.NewTimer(d)
			defer t.Stop()
			timeout = t.C
		}
	}
	for {
		changed := n.store.updates()
		msgs, err := n.store.claimUndelivered(r.URL.Query().Get("chat"))
		msgs = slices.DeleteFunc(msgs, n.handledHere)
		if len(msgs) > 0 {
			writeJSONResponse(w, msgs)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		select {
		case <-changed:
		case <-timeout:
			w.WriteHeader(http.StatusNoContent)
			return
		case <-r.Context().Done():
			return
		}
	}
}

// handledHere reports whether chat message m no longer needs a waiter: the
// worker or a session on this node took it, or a session already read it.
// Such a message is consumed without being returned, so a `wait` after the
// worker answered a request never hands that stale request to an agent.
func (n *Node) handledHere(m Message) bool {
	if m.ChatID == "" {
		return false
	}
	r, ok := n.chats.message(m.ID)
	return ok && (r.Assigned != "" || (r.Unread && !r.ReadAt.IsZero()))
}

func (n *Node) handleInbox(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if s := r.URL.Query().Get("limit"); s != "" {
		v, err := strconv.Atoi(s)
		if err != nil || v < 0 {
			http.Error(w, "invalid limit", http.StatusBadRequest)
			return
		}
		limit = v
	}
	entries, err := n.store.recent(limit, true)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if entries == nil {
		entries = []Entry{}
	}
	writeJSONResponse(w, entries)
}

// ErrBadRequest wraps a chat API request that could not be read.
var ErrBadRequest = errors.New("bad request")

// ChatRoutes mounts the chat endpoints under prefix (e.g. "/ui/api") on mux,
// for the control API and the web UI alike; withClose adds POST
// /chats/{id}/close, which only the app's people use. fail answers an error;
// its code is 404 for an unknown chat, 409 for a closed one or closing a
// legacy one, else 400.
func (n *Node) ChatRoutes(mux *http.ServeMux, prefix string, withClose bool, fail func(w http.ResponseWriter, code int, err error)) {
	failed := func(w http.ResponseWriter, err error) { fail(w, errorCode(err), err) }
	reply := func(w http.ResponseWriter, v any, err error) {
		if err != nil {
			failed(w, err)
			return
		}
		writeJSONResponse(w, v)
	}
	mux.HandleFunc("POST "+prefix+"/chats", func(w http.ResponseWriter, r *http.Request) {
		var req CreateChatRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxFrame)).Decode(&req); err != nil {
			failed(w, fmt.Errorf("%w: %w", ErrBadRequest, err))
			return
		}
		info, err := n.CreateChat(req.Participants, strings.TrimSpace(req.Area))
		reply(w, info, err)
	})
	mux.HandleFunc("GET "+prefix+"/chats", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		chats, err := n.Chats(q.Get("archive") == "1", q.Get("legacy") == "1")
		reply(w, chats, err)
	})
	mux.HandleFunc("GET "+prefix+"/chats/{id}", func(w http.ResponseWriter, r *http.Request) {
		info, err := n.Chat(r.PathValue("id"))
		reply(w, info, err)
	})
	mux.HandleFunc("GET "+prefix+"/chats/{id}/messages", func(w http.ResponseWriter, r *http.Request) {
		var nums [3]uint64
		for i, k := range []string{"before", "after", "limit"} {
			if s := r.URL.Query().Get(k); s != "" {
				v, err := strconv.ParseUint(s, 10, 32)
				if err != nil {
					failed(w, fmt.Errorf("%w: invalid %s", ErrBadRequest, k))
					return
				}
				nums[i] = v
			}
		}
		msgs, err := n.ChatMessages(r.PathValue("id"), nums[0], nums[1], int(min(nums[2], 1000)))
		reply(w, msgs, err)
	})
	if withClose {
		mux.HandleFunc("POST "+prefix+"/chats/{id}/close", func(w http.ResponseWriter, r *http.Request) {
			info, err := n.CloseChat(r.PathValue("id"))
			reply(w, info, err)
		})
	}
}

// errorCode is the HTTP status of a control API error.
func errorCode(err error) int {
	switch {
	case errors.Is(err, ErrUnknownChat), errors.Is(err, ErrUnknownSession):
		return http.StatusNotFound
	case errors.Is(err, ErrChatClosed), errors.Is(err, ErrLegacyChat):
		return http.StatusConflict
	}
	return http.StatusBadRequest
}

// sessionRoutes mounts the endpoints of live sessions on the control API:
// read state (ack, unread), the session registry and session activity.
func (n *Node) sessionRoutes(mux *http.ServeMux) {
	reply := func(w http.ResponseWriter, v any, err error) {
		if err != nil {
			http.Error(w, err.Error(), errorCode(err))
			return
		}
		writeJSONResponse(w, v)
	}
	decode := func(w http.ResponseWriter, r *http.Request, v any) bool {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxFrame)).Decode(v); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return false
		}
		return true
	}
	ack := func(w http.ResponseWriter, r *http.Request, chat string) {
		var req AckRequest
		if decode(w, r, &req) {
			res, err := n.Ack(chat, req)
			reply(w, res, err)
		}
	}
	mux.HandleFunc("POST /chats/{id}/ack", func(w http.ResponseWriter, r *http.Request) { ack(w, r, r.PathValue("id")) })
	mux.HandleFunc("POST /ack", func(w http.ResponseWriter, r *http.Request) { ack(w, r, "") })
	mux.HandleFunc("POST /chats/{id}/activity", func(w http.ResponseWriter, r *http.Request) {
		var req ActivityRequest
		if decode(w, r, &req) {
			m, err := n.SessionActivity(r.PathValue("id"), req)
			reply(w, m, err)
		}
	})
	mux.HandleFunc("GET /unread", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		limit := 50
		if s := q.Get("limit"); s != "" {
			v, err := strconv.Atoi(s)
			if err != nil || v < 1 || v > 1000 {
				http.Error(w, "invalid limit", http.StatusBadRequest)
				return
			}
			limit = v
		}
		// A Claude session's background waiter asks with waiter=1: while the
		// node wakes that session through its inbox, the waiter has nothing to do.
		if q.Get("waiter") == "1" && n.InboxWakes(q.Get("session")) {
			writeJSONResponse(w, UnreadPage{Messages: []UnreadMessage{}})
			return
		}
		waiter := q.Get("waiter") == "1"
		if waiter {
			limit = max(limit, hookBatchIDs) // what it may not wake with is dropped below
		}
		page, err := n.unreadFor(q.Get("folder"), q.Get("session"), q.Get("after"), limit, q.Get("actionable") == "1")
		if err == nil && waiter {
			n.waiterSpent(&page)
		}
		reply(w, page, err)
	})
	mux.HandleFunc("POST /claim", func(w http.ResponseWriter, r *http.Request) {
		var req ClaimRequest
		if decode(w, r, &req) {
			ids, err := n.Claim(req)
			reply(w, ids, err)
		}
	})
	mux.HandleFunc("POST /sessions", func(w http.ResponseWriter, r *http.Request) {
		var req SessionRequest
		if decode(w, r, &req) {
			s, err := n.RegisterSession(req)
			reply(w, s, err)
		}
	})
	mux.HandleFunc("GET /sessions", func(w http.ResponseWriter, _ *http.Request) { writeJSONResponse(w, n.Sessions()) })
	mux.HandleFunc("DELETE /sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := n.EndSession(r.PathValue("id")); err != nil {
			http.Error(w, err.Error(), errorCode(err))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func writeJSONResponse(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
