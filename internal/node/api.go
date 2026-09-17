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

// apiHandler serves the loopback control API:
//
//	POST /send               SendRequest -> Message
//	GET  /wait?timeout=30s   200 []Message (marked delivered) or 204 on timeout; 0 waits forever
//	GET  /inbox?limit=50     200 []Entry
func (n *Node) apiHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /send", n.handleSend)
	mux.HandleFunc("GET /wait", n.handleWait)
	mux.HandleFunc("GET /inbox", n.handleInbox)
	return localOnly(mux)
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
