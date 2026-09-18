package node

import (
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/UberMorgott/agent-link/internal/config"
)

// SendRequest is the body of POST /send.
type SendRequest struct {
	To      string `json:"to"`
	Body    string `json:"body"`
	ReplyTo string `json:"reply_to,omitempty"`
}

// APIHandler serves the loopback control API:
//
//	POST /send               SendRequest -> Message
//	GET  /wait?timeout=30s   200 []Message (marked delivered) or 204 on timeout; 0 waits forever.
//	                         Requests and replies only: status updates never wake it.
//	GET  /inbox?limit=50     200 []Entry
//	GET  /members            200 []MemberInfo (this node first)
//	POST /members            {"addr"} -> dial that address too (AddPeer)
//	POST /members/remove     {"name"} -> remove the member from the network
func (n *Node) APIHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /send", n.handleSend)
	mux.HandleFunc("GET /wait", n.handleWait)
	mux.HandleFunc("GET /inbox", n.handleInbox)
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
	m, err := n.Send(req.To, req.Body, req.ReplyTo)
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
		msgs, err := n.store.claimUndelivered()
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

func writeJSONResponse(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
