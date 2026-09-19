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
	"path/filepath"
	"strings"

	"github.com/UberMorgott/agent-link/internal/config"
	"github.com/UberMorgott/agent-link/internal/node"
	"github.com/UberMorgott/agent-link/internal/settings"
	"github.com/UberMorgott/agent-link/internal/worker"
)

//go:embed web
var webFS embed.FS

// TokenHeader carries the per-run token on every web UI API call.
const TokenHeader = "X-Agentlink-Token" //nolint:gosec // G101: an HTTP header name; the token itself is random per run (newToken)

const maxBody = 1 << 20

func newToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// URL returns the address of a web UI route, e.g. "dashboard" or "inbox".
func (a *App) URL(page string) string {
	return "http://" + a.APIAddr() + "/ui/" + page
}

// Handler serves the web UI under /ui/ and the node's control API elsewhere.
//
//	GET  /ui/dashboard, /ui/inbox, /ui/participants, /ui/settings  application shell with the per-run token embedded
//	GET  /ui/api/status            Status
//	GET  /ui/api/settings          settings.Settings
//	POST /ui/api/settings          settings.Settings -> save, restart node
//	GET  /ui/api/inbox             []node.Entry
//	GET  /ui/api/threads           []Thread (inbox entries paired by reply_to)
//	GET  /ui/api/dashboard         DashboardSummary
//	GET  /ui/api/participants      []ParticipantView
//	POST /ui/api/send              node.SendRequest -> node.Message
//	POST /ui/api/members/add       {"addr"} -> keep and dial that address -> Status
//	POST /ui/api/members/remove    {"name"} -> remove the member everywhere -> Status
//	POST /ui/api/pick-folder       {"start"} -> native folder dialog -> pickResult
//	POST /ui/api/agent             {"handler","agent_path"} -> agentInfo
//	POST /ui/api/pick-agent        {"start"} -> native file dialog for a program -> pickResult
//	POST /ui/api/find-agent        {"handler"} -> look for the agent again -> agentInfo
//	GET  /ui/api/update            UpdateStatus
//	POST /ui/api/update/check      ask GitHub for the latest release -> UpdateStatus
//	POST /ui/api/update/apply      install the newer release, then restart -> UpdateStatus
//	POST /ui/api/update/auto       {"auto"} -> save the auto-update switch -> UpdateStatus
//	POST /ui/api/quit              exit the app (same path as the tray's Quit)
func (a *App) Handler() http.Handler {
	ui := http.NewServeMux()
	for _, path := range []string{"/ui/dashboard", "/ui/inbox", "/ui/participants", "/ui/settings"} {
		ui.HandleFunc("GET "+path, a.page("web/app.html"))
	}
	static, _ := fs.Sub(webFS, "web/static") // constant path inside the embed
	ui.Handle("GET /ui/static/", http.StripPrefix("/ui/static/", http.FileServerFS(static)))
	api := http.NewServeMux()
	api.HandleFunc("GET /ui/api/status", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, a.Status()) })
	api.HandleFunc("GET /ui/api/settings", func(w http.ResponseWriter, _ *http.Request) {
		s := a.Settings()
		s.Secret, s.HandlerCommand = "", nil
		s.Autostart, _ = a.Autostart() // as Windows has it, also after a change in the tray or Task Manager
		writeJSON(w, s)
	})
	api.HandleFunc("POST /ui/api/settings", a.saveSettings)
	api.HandleFunc("GET /ui/api/inbox", a.inbox)
	api.HandleFunc("GET /ui/api/threads", a.threads)
	api.HandleFunc("GET /ui/api/dashboard", a.dashboard)
	api.HandleFunc("GET /ui/api/participants", a.participants)
	api.HandleFunc("POST /ui/api/send", a.send)
	api.HandleFunc("POST /ui/api/members/add", a.memberAction(func(r node.MemberRequest) error { return a.AddMember(r.Addr) }))
	api.HandleFunc("POST /ui/api/members/remove", a.memberAction(func(r node.MemberRequest) error { return a.RemoveMember(r.Name) }))
	api.HandleFunc("POST /ui/api/pick-folder", a.pickFolder)
	api.HandleFunc("POST /ui/api/agent", a.agentInfo)
	api.HandleFunc("POST /ui/api/pick-agent", a.pickAgent)
	api.HandleFunc("POST /ui/api/find-agent", a.findAgent)
	api.HandleFunc("GET /ui/api/update", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, a.UpdateStatus()) })
	api.HandleFunc("POST /ui/api/update/check", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, a.CheckUpdate(r.Context())) })
	api.HandleFunc("POST /ui/api/update/apply", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, a.InstallUpdate(r.Context())) })
	api.HandleFunc("POST /ui/api/update/auto", a.setAutoUpdate)
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
		http.Redirect(w, r, "/ui/dashboard", http.StatusFound)
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
	// Found says where the agent program was found on save, if it was looked for.
	Found string `json:"found,omitempty"`
}

func (a *App) saveSettings(w http.ResponseWriter, r *http.Request) {
	var s settings.Settings
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&s); err != nil {
		writeError(w, http.StatusBadRequest, msg("error.bad_request", nil))
		return
	}
	f, err := a.Apply(r.Context(), s)
	found := ""
	if f.Path != "" {
		found = foundText(f)
	}
	var p *settings.Problem
	switch {
	case err == nil:
		writeJSON(w, saveResult{Saved: true, Found: found})
	case errors.Is(err, ErrNotStarted):
		writeJSON(w, saveResult{Saved: true, Error: userError(err), Found: found})
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

func (a *App) setAutoUpdate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Auto bool `json:"auto"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, msg("error.bad_request", nil))
		return
	}
	if err := a.SetAutoUpdate(req.Auto); err != nil {
		a.log.Error("save auto-update", "err", err)
		writeError(w, http.StatusInternalServerError, msg("error.save", nil))
		return
	}
	writeJSON(w, a.UpdateStatus())
}

func (a *App) inbox(w http.ResponseWriter, _ *http.Request) {
	entries, err := a.recent(200)
	if err != nil {
		a.log.Error("inbox", "err", err)
		writeError(w, http.StatusInternalServerError, msg("error.internal", nil))
		return
	}
	writeJSON(w, entries)
}

// recent loads local history, treating an unconfigured or stopped node as an
// empty history so the UI can render its initial state without special cases.
func (a *App) recent(limit int) ([]node.Entry, error) {
	n := a.node()
	if n == nil {
		return []node.Entry{}, nil
	}
	entries, err := n.Recent(limit)
	if entries == nil {
		entries = []node.Entry{}
	}
	return entries, err
}

// threads serves the inbox as questions paired with their answers.
func (a *App) threads(w http.ResponseWriter, r *http.Request) {
	peer := r.URL.Query().Get("peer")
	limit := 200
	if peer != "" {
		limit = 0
	}
	entries, err := a.recent(limit)
	if err != nil {
		a.log.Error("threads", "err", err)
		writeError(w, http.StatusInternalServerError, msg("error.internal", nil))
		return
	}
	writeJSON(w, filterThreads(threads(entries), peer, a.Status().Node))
}

func (a *App) dashboard(w http.ResponseWriter, _ *http.Request) {
	entries, err := a.recent(0)
	if err != nil {
		a.log.Error("dashboard", "err", err)
		writeError(w, http.StatusInternalServerError, msg("error.internal", nil))
		return
	}
	writeJSON(w, buildDashboard(a.Status(), entries))
}

func (a *App) participants(w http.ResponseWriter, _ *http.Request) {
	entries, err := a.recent(0)
	if err != nil {
		a.log.Error("participants", "err", err)
		writeError(w, http.StatusInternalServerError, msg("error.internal", nil))
		return
	}
	writeJSON(w, buildParticipants(a.Status(), entries))
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
	if errors.Is(err, node.ErrAmbiguousPeer) {
		writeError(w, http.StatusBadRequest, msg("error.ambiguous_peer", map[string]string{"peers": strings.Join(n.Peers(), ", ")}))
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, sendError(err))
		return
	}
	writeJSON(w, m)
}

// ErrPickCancelled means the user closed a Windows dialog without choosing.
var ErrPickCancelled = errors.New("dialog cancelled")

// ErrPickUnsupported means this platform has no native dialog.
var ErrPickUnsupported = errors.New("native dialog is not supported on this platform")

type pickResult struct {
	Path      string `json:"path,omitempty"`
	Cancelled bool   `json:"cancelled,omitempty"`
	Message   string `json:"message,omitempty"`
}

// pickKeys are the strings of one kind of dialog.
type pickKeys struct{ what, cancelled, unsupported, failed string }

// pick opens a native dialog in this (tray) process: a browser page cannot
// learn absolute paths. One dialog of any kind at a time.
func (a *App) pick(w http.ResponseWriter, r *http.Request, keys pickKeys, open func(start string) (string, error)) {
	var req struct {
		Start string `json:"start"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, msg("error.bad_request", nil))
		return
	}
	if open == nil {
		writeError(w, http.StatusNotImplemented, msg(keys.unsupported, nil))
		return
	}
	if !a.picking.CompareAndSwap(false, true) {
		writeError(w, http.StatusConflict, msg("error.pick_busy", nil))
		return
	}
	defer a.picking.Store(false)
	path, err := open(strings.TrimSpace(req.Start))
	switch {
	case err == nil:
		writeJSON(w, pickResult{Path: path})
	case errors.Is(err, ErrPickCancelled):
		writeJSON(w, pickResult{Cancelled: true, Message: msg(keys.cancelled, nil)})
	case errors.Is(err, ErrPickUnsupported):
		writeError(w, http.StatusNotImplemented, msg(keys.unsupported, nil))
	default:
		a.log.Error("pick "+keys.what, "err", err)
		writeError(w, http.StatusInternalServerError, msg(keys.failed, nil))
	}
}

func (a *App) pickFolder(w http.ResponseWriter, r *http.Request) {
	var open func(string) (string, error)
	if a.PickFolder != nil {
		open = func(start string) (string, error) {
			return a.PickFolder(start, msg("settings.work_dir.pick_title", nil))
		}
	}
	a.pick(w, r, pickKeys{"folder", "settings.work_dir.cancelled", "error.pick_unsupported", "error.pick"}, open)
}

// pickAgent opens the file dialog for the agent program. start is the current
// program path or its folder; the dialog opens in that folder.
func (a *App) pickAgent(w http.ResponseWriter, r *http.Request) {
	var open func(string) (string, error)
	if a.PickFile != nil {
		open = func(start string) (string, error) {
			if start != "" && filepath.Ext(start) != "" {
				start = filepath.Dir(start)
			}
			return a.PickFile(start, msg("settings.agent.pick_title", nil), msg("settings.agent.filter", nil), "*.exe;*.cmd;*.bat")
		}
	}
	a.pick(w, r, pickKeys{"agent", "settings.agent.cancelled", "error.pick_agent_unsupported", "error.pick_agent"}, open)
}

type agentInfo struct {
	Path   string `json:"path,omitempty"`
	Source string `json:"source,omitempty"` // settings.AgentFrom*|AgentMissing; empty for no handler
	Text   string `json:"text"`
}

// agentInfo tells which program the chosen handler would run, for the line
// under «Кто отвечает».
func (a *App) agentInfo(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Handler   string `json:"handler"`
		AgentPath string `json:"agent_path"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, msg("error.bad_request", nil))
		return
	}
	if _, ok := worker.ForHandler(req.Handler); !ok || a.Agents.LookPath == nil {
		writeJSON(w, agentInfo{Text: ""})
		return
	}
	path, source := a.Agents.Resolve(req.Handler, strings.TrimSpace(req.AgentPath))
	info := agentInfo{Path: path, Source: source}
	switch {
	case source == settings.AgentFromPath:
		info.Text = msg("settings.agent.from_path", map[string]string{"path": path})
	case source == settings.AgentFromSetting && a.Agents.Kind(req.Handler, path) != "":
		info.Text = foundText(settings.Found{Path: path, Kind: a.Agents.Kind(req.Handler, path)})
	case source == settings.AgentFromSetting:
		info.Text = msg("settings.agent.from_setting", map[string]string{"path": path})
	case path != "":
		info.Text = msg("settings.agent.gone", map[string]string{"path": path})
	default:
		info.Text = msg("settings.agent.missing", nil)
	}
	writeJSON(w, info)
}

// foundText is «Найден: <install kind> — <path>».
func foundText(f settings.Found) string {
	return msg("settings.agent.found", map[string]string{"kind": msg("agent_kind."+f.Kind, nil), "path": f.Path})
}

// findAgent looks for the handler's agent again («Найти заново») and returns
// what it would save; the page keeps it until «Сохранить». A program on PATH
// needs no agent_path, so Path is then empty.
func (a *App) findAgent(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Handler string `json:"handler"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, msg("error.bad_request", nil))
		return
	}
	if _, ok := worker.ForHandler(req.Handler); !ok || a.Agents.LookPath == nil {
		writeJSON(w, agentInfo{Text: ""})
		return
	}
	f, ok := a.Agents.Discover(req.Handler)
	if !ok {
		writeJSON(w, agentInfo{Source: settings.AgentMissing, Text: msg("settings.agent.not_found", nil)})
		return
	}
	info := agentInfo{Path: f.Path, Source: settings.AgentFromSetting, Text: foundText(f)}
	if f.Kind == settings.KindPath {
		info.Path, info.Source = "", settings.AgentFromPath
	}
	writeJSON(w, info)
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
