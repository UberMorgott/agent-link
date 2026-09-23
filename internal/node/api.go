package node

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/UberMorgott/agent-link/internal/config"
)

// SendRequest is the body of POST /send. With ChatID the message goes to that
// chat (To stays empty): Ask names the participants asked to answer (none:
// the message only informs) and Parent the request whose job sends it
// (AGENTLINK_JOB_ID), which continues that request's automatic chain.
type SendRequest struct {
	To      string   `json:"to"`
	Body    string   `json:"body"`
	ReplyTo string   `json:"reply_to,omitempty"`
	ChatID  string   `json:"chat_id,omitempty"`
	Ask     []string `json:"ask,omitempty"`
	Parent  string   `json:"parent,omitempty"`
}

// SendRequest sends req like POST /send: to a chat with ChatID, else like Send.
func (n *Node) SendRequest(req SendRequest) (Message, error) {
	if req.ChatID != "" {
		if req.To != "" {
			return Message{}, errors.New("give either to or chat_id")
		}
		return n.SendChat(ChatSend{ChatID: req.ChatID, Body: req.Body, ReplyTo: req.ReplyTo, Ask: req.Ask, Parent: req.Parent})
	}
	if len(req.Ask) > 0 {
		return Message{}, errors.New("ask needs chat_id")
	}
	return n.Send(req.To, req.Body, req.ReplyTo)
}

// CreateChatRequest is the body of POST /chats.
type CreateChatRequest struct {
	Participants []string `json:"participants"` // the other members; this node is added
	Area         string   `json:"area,omitempty"`
}

// ArchiveRequest is the body of POST /chats/{id}/archive.
type ArchiveRequest struct {
	Archived bool `json:"archived"`
}

// APIHandler serves the loopback control API:
//
//	POST /send               SendRequest -> Message
//	GET  /wait?timeout=30s   200 []Message (marked delivered) or 204 on timeout; 0 waits forever.
//	                         Requests and replies only: status updates never wake it.
//	     &chat=ID            only that chat's messages; the others stay for a later wait
//	POST /chats              CreateChatRequest -> ChatInfo
//	GET  /chats?archive=1    200 []ChatInfo: the archive (default the main list); &legacy=1 adds pre-chat history
//	GET  /chats/{id}         200 ChatInfo
//	GET  /chats/{id}/messages?before=SEQ&after=SEQ&limit=50   200 []ChatMessage in Seq order
//	POST /chats/{id}/close   -> ChatInfo
//	POST /chats/{id}/archive ArchiveRequest -> ChatInfo
//	GET  /inbox?limit=50     200 []Entry
//	GET  /members            200 []MemberInfo (this node first)
//	POST /members            {"addr"} -> dial that address too (AddPeer)
//	POST /members/remove     {"name"} -> remove the member from the network
func (n *Node) APIHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /send", n.handleSend)
	mux.HandleFunc("GET /wait", n.handleWait)
	mux.HandleFunc("GET /inbox", n.handleInbox)
	n.ChatRoutes(mux, "", func(w http.ResponseWriter, code int, err error) { http.Error(w, err.Error(), code) })
	mux.HandleFunc("GET /members", func(w http.ResponseWriter, _ *http.Request) { writeJSONResponse(w, n.Members()) })
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
	entries, err := n.store.recent(limit)
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
// for the control API and the web UI alike. fail answers an error; its code
// is 404 for an unknown chat, 409 for a closed one or closing a legacy one, else 400.
func (n *Node) ChatRoutes(mux *http.ServeMux, prefix string, fail func(w http.ResponseWriter, code int, err error)) {
	failed := func(w http.ResponseWriter, err error) {
		code := http.StatusBadRequest
		switch {
		case errors.Is(err, ErrUnknownChat):
			code = http.StatusNotFound
		case errors.Is(err, ErrChatClosed), errors.Is(err, ErrLegacyChat):
			code = http.StatusConflict
		}
		fail(w, code, err)
	}
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
	mux.HandleFunc("POST "+prefix+"/chats/{id}/close", func(w http.ResponseWriter, r *http.Request) {
		info, err := n.CloseChat(r.PathValue("id"))
		reply(w, info, err)
	})
	mux.HandleFunc("POST "+prefix+"/chats/{id}/archive", func(w http.ResponseWriter, r *http.Request) {
		var req ArchiveRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxFrame)).Decode(&req); err != nil {
			failed(w, fmt.Errorf("%w: %w", ErrBadRequest, err))
			return
		}
		info, err := n.ArchiveChat(r.PathValue("id"), req.Archived)
		reply(w, info, err)
	})
}

func writeJSONResponse(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
