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
//	GET  /ui/api/threads           []Thread (inbox entries paired by reply_to)
//	POST /ui/api/send              node.SendRequest -> node.Message
//	POST /ui/api/pick-folder       {"start"} -> native folder dialog -> pickResult
//	POST /ui/api/quit              exit the app (same path as the tray's Quit)
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
		s.Secret, s.HandlerCommand = "", nil
		writeJSON(w, s)
	})
	api.HandleFunc("POST /ui/api/settings", a.saveSettings)
	api.HandleFunc("GET /ui/api/inbox", a.inbox)
	api.HandleFunc("GET /ui/api/threads", a.threads)
	api.HandleFunc("POST /ui/api/send", a.send)
	api.HandleFunc("POST /ui/api/pick-folder", a.pickFolder)
	api.HandleFunc("POST /ui/api/quit", func(w http.ResponseWriter, _ *http.Request) {
		if a.QuitFunc == nil {
			writeError(w, http.StatusNotImplemented, msg("error.internal", nil))
			return
		}
		writeJSON(w, map[string]bool{"quitting": true})
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		a.Quit()
	})
	ui.Handle("/ui/api/", a.requireToken(api))

	root := http.NewServeMux()
	root.Handle("/ui/", ui)
	root.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/settings", http.StatusFound)
	})
	root.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		n := a.node()
		if n == nil {
			http.Error(w, "node is not running: open the settings page and save a pairing code", http.StatusServiceUnavailable)
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
		if err != nil { // embedded at build time
			http.Error(w, msg("error.internal", nil), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		page := strings.ReplaceAll(string(data), "{{TOKEN}}", a.token)
		page = strings.ReplaceAll(page, "{{STRINGS}}", stringsAttr())
		_, _ = w.Write([]byte(page))
	}
}

type saveResult struct {
	Saved bool   `json:"saved"`
	Error string `json:"error,omitempty"`
}

func (a *App) saveSettings(w http.ResponseWriter, r *http.Request) {
	var s settings.Settings
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&s); err != nil {
		writeError(w, http.StatusBadRequest, msg("error.bad_request", nil))
		return
	}
	err := a.Apply(s)
	var p *settings.Problem
	switch {
	case err == nil:
		writeJSON(w, saveResult{Saved: true})
	case errors.Is(err, ErrNotStarted):
		writeJSON(w, saveResult{Saved: true, Error: userError(err)})
	case errors.As(err, &p):
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(saveResult{Error: userError(err)})
	default:
		a.log.Error("save settings", "err", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(saveResult{Error: msg("error.save", nil)})
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
		a.log.Error("inbox", "err", err)
		writeError(w, http.StatusInternalServerError, msg("error.internal", nil))
		return
	}
	if entries == nil {
		entries = []node.Entry{}
	}
	writeJSON(w, entries)
}

// threads serves the inbox as questions paired with their answers.
func (a *App) threads(w http.ResponseWriter, _ *http.Request) {
	n := a.node()
	if n == nil {
		writeJSON(w, []Thread{})
		return
	}
	entries, err := n.Recent(200)
	if err != nil {
		a.log.Error("threads", "err", err)
		writeError(w, http.StatusInternalServerError, msg("error.internal", nil))
		return
	}
	writeJSON(w, threads(entries))
}

func (a *App) send(w http.ResponseWriter, r *http.Request) {
	var req node.SendRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, msg("error.bad_request", nil))
		return
	}
	n := a.node()
	if n == nil {
		writeError(w, http.StatusServiceUnavailable, msg("error.not_running", nil))
		return
	}
	m, err := n.Send(strings.TrimSpace(req.To), req.Body, req.ReplyTo)
	if err != nil {
		writeError(w, http.StatusBadRequest, sendError(err))
		return
	}
	writeJSON(w, m)
}

// ErrPickCancelled means the user closed the folder dialog without choosing.
var ErrPickCancelled = errors.New("folder dialog cancelled")

// ErrPickUnsupported means this platform has no native folder dialog.
var ErrPickUnsupported = errors.New("folder dialog is not supported on this platform")

type pickResult struct {
	Path      string `json:"path,omitempty"`
	Cancelled bool   `json:"cancelled,omitempty"`
	Message   string `json:"message,omitempty"`
}

// pickFolder opens the native folder dialog in this (tray) process: a browser
// page cannot learn absolute paths. One dialog at a time.
func (a *App) pickFolder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Start string `json:"start"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, msg("error.bad_request", nil))
		return
	}
	if a.PickFolder == nil {
		writeError(w, http.StatusNotImplemented, msg("error.pick_unsupported", nil))
		return
	}
	if !a.picking.CompareAndSwap(false, true) {
		writeError(w, http.StatusConflict, msg("error.pick_busy", nil))
		return
	}
	defer a.picking.Store(false)
	path, err := a.PickFolder(strings.TrimSpace(req.Start), msg("settings.work_dir.pick_title", nil))
	switch {
	case err == nil:
		writeJSON(w, pickResult{Path: path})
	case errors.Is(err, ErrPickCancelled):
		writeJSON(w, pickResult{Cancelled: true, Message: msg("settings.work_dir.cancelled", nil)})
	case errors.Is(err, ErrPickUnsupported):
		writeError(w, http.StatusNotImplemented, msg("error.pick_unsupported", nil))
	default:
		a.log.Error("pick folder", "err", err)
		writeError(w, http.StatusInternalServerError, msg("error.pick", nil))
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// writeError answers with {"error": text}; text is already one sentence for the user.
func writeError(w http.ResponseWriter, code int, text string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": text})
}

// userError turns a settings or start failure into one sentence of advice.
func userError(err error) string {
	var p *settings.Problem
	var op *net.OpError
	switch {
	case errors.As(err, &p):
		return msg("error."+p.Key, nil)
	case errors.As(err, &op) && op.Op == "listen":
		addr := ""
		if op.Addr != nil {
			addr = op.Addr.String()
		}
		return msg("error.listen", map[string]string{"addr": addr})
	default:
		return msg("error.start", nil)
	}
}

func sendError(err error) string {
	switch {
	case errors.Is(err, node.ErrEmptyBody):
		return msg("error.empty_body", nil)
	case errors.Is(err, node.ErrUnknownPeer):
		return msg("error.unknown_peer", nil)
	case errors.Is(err, node.ErrNoAreaPeer):
		return msg("error.no_area_peer", nil)
	default:
		return msg("error.send", nil)
	}
}
