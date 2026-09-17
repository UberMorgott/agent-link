package app

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"strings"

	"github.com/UberMorgott/agent-link/internal/config"
	"github.com/UberMorgott/agent-link/internal/node"
	"github.com/UberMorgott/agent-link/internal/settings"
)

//go:embed web
var webFS embed.FS

// TokenHeader carries the per-run token on every web UI API call.
const TokenHeader = "X-Agentlink-Token"

const maxBody = 1 << 20

func newToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// URL returns the address of a web UI page, e.g. "settings" or "inbox".
func (a *App) URL(page string) string {
	return "http://" + a.APIAddr() + "/ui/" + page
}

// Handler serves the web UI under /ui/ and the node's control API elsewhere.
//
//	GET  /ui/settings, /ui/inbox   pages with the per-run token embedded
//	GET  /ui/api/status            Status
//	GET  /ui/api/settings          settings.Settings
//	POST /ui/api/settings          settings.Settings -> save, restart node
//	GET  /ui/api/inbox             []node.Entry
//	POST /ui/api/send              node.SendRequest -> node.Message
func (a *App) Handler() http.Handler {
	ui := http.NewServeMux()
	ui.HandleFunc("GET /ui/settings", a.page("web/settings.html"))
	ui.HandleFunc("GET /ui/inbox", a.page("web/inbox.html"))
	static, _ := fs.Sub(webFS, "web/static") // constant path inside the embed
	ui.Handle("GET /ui/static/", http.StripPrefix("/ui/static/", http.FileServerFS(static)))
	api := http.NewServeMux()
	api.HandleFunc("GET /ui/api/status", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, a.Status()) })
	api.HandleFunc("GET /ui/api/settings", func(w http.ResponseWriter, _ *http.Request) {
		s := a.Settings()
		s.API, s.HandlerCommand = "", nil
		writeJSON(w, s)
	})
	api.HandleFunc("POST /ui/api/settings", a.saveSettings)
	api.HandleFunc("GET /ui/api/inbox", a.inbox)
	api.HandleFunc("POST /ui/api/send", a.send)
	ui.Handle("/ui/api/", a.requireToken(api))

	root := http.NewServeMux()
	root.Handle("/ui/", ui)
	root.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/settings", http.StatusFound)
	})
	root.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		n := a.node()
		if n == nil {
			http.Error(w, "node is not running", http.StatusServiceUnavailable)
			return
		}
		n.APIHandler().ServeHTTP(w, r)
	})
	return loopbackHost(root)
}

// loopbackHost rejects non-loopback Host headers (DNS rebinding) and forbids
// framing the UI.
func loopbackHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if !config.IsLoopbackHost(host) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

// requireToken accepts only same-origin requests that carry the per-run token.
// A cross-site page cannot read the token, and the custom header forces a CORS
// preflight that this server never approves.
func (a *App) requireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get(TokenHeader)
		sameOrigin := true
		if o := r.Header.Get("Origin"); o != "" && o != "http://"+r.Host {
			sameOrigin = false
		}
		if s := r.Header.Get("Sec-Fetch-Site"); s != "" && s != "same-origin" && s != "none" {
			sameOrigin = false
		}
		if !sameOrigin || subtle.ConstantTimeCompare([]byte(got), []byte(a.token)) != 1 {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *App) page(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		data, err := webFS.ReadFile(name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(strings.ReplaceAll(string(data), "{{TOKEN}}", a.token)))
	}
}

type saveResult struct {
	Saved bool   `json:"saved"`
	Error string `json:"error,omitempty"`
}

func (a *App) saveSettings(w http.ResponseWriter, r *http.Request) {
	var s settings.Settings
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&s); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	err := a.Apply(s)
	switch {
	case err == nil:
		writeJSON(w, saveResult{Saved: true})
	case errors.Is(err, ErrNotStarted):
		writeJSON(w, saveResult{Saved: true, Error: err.Error()})
	default:
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, saveResult{Error: err.Error()})
	}
}

func (a *App) inbox(w http.ResponseWriter, _ *http.Request) {
	n := a.node()
	if n == nil {
		writeJSON(w, []node.Entry{})
		return
	}
	entries, err := n.Recent(200)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if entries == nil {
		entries = []node.Entry{}
	}
	writeJSON(w, entries)
}

func (a *App) send(w http.ResponseWriter, r *http.Request) {
	var req node.SendRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	n := a.node()
	if n == nil {
		http.Error(w, "node is not running; save settings first", http.StatusServiceUnavailable)
		return
	}
	m, err := n.Send(req.To, req.Body, req.ReplyTo)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, m)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
